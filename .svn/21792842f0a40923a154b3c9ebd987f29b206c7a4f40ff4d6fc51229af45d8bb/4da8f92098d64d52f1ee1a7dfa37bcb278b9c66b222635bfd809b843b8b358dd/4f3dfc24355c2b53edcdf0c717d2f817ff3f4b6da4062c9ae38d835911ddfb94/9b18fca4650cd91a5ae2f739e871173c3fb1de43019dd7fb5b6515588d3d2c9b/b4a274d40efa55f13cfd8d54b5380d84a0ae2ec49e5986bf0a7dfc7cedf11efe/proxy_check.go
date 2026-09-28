package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var proxyCheckTraceURL = "https://chatgpt.com/cdn-cgi/trace"

const (
	proxyCheckRequestTimeout = 8 * time.Second

	proxyCheckBudget = 45 * time.Second

	proxyCheckParallel = 6

	proxyCheckMaxBody = 4 << 10

	proxyCheckFallbackModel = "gpt-5.5"
)

const (
	proxyPoolStatic   = "static"
	proxyPoolRotating = "rotating"

	proxyVerdictOK          = "ok"
	proxyVerdictBlocked     = "blocked"
	proxyVerdictRateLimited = "ratelimited"
	proxyVerdictUnexpected  = "unexpected"
	proxyVerdictDead        = "dead"
)

type proxyCheckResult struct {
	Index int    `json:"index"`
	Pool  string `json:"pool"`
	Proxy string `json:"proxy"`

	Rotated    bool   `json:"rotated"`
	Mismatch   string `json:"mismatch,omitempty"`
	Verdict    string `json:"verdict"`
	StatusCode int    `json:"status_code,omitempty"`
	MS         int64  `json:"ms"`
	ExitIP     string `json:"exit_ip,omitempty"`
	Country    string `json:"country,omitempty"`
	Colo       string `json:"colo,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type proxyCheckResponse struct {
	Checked    int `json:"checked"`
	OK         int `json:"ok"`
	Blocked    int `json:"blocked"`
	Dead       int `json:"dead"`
	Other      int `json:"other"`
	Mismatches int `json:"mismatches"`

	StaticChecked int                `json:"static_checked"`
	DistinctIPs   int                `json:"distinct_ips"`
	MS            int64              `json:"ms"`
	TimedOut      bool               `json:"timed_out,omitempty"`
	Direct        bool               `json:"direct,omitempty"`
	Note          string             `json:"note,omitempty"`
	Results       []proxyCheckResult `json:"results"`
}

func runProxyCheck() pluginapi.ManagementResponse {
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	model := proxyCheckFallbackModel
	if len(cfg.Models) > 0 {
		model = cfg.Models[0]
	}

	type target struct {
		pool  string
		index int
		url   string
	}
	var targets []target
	for i, raw := range cfg.ProbeProxies {
		targets = append(targets, target{pool: proxyPoolStatic, index: i + 1, url: raw})
	}
	for i, raw := range cfg.ProbeProxiesRotating {
		targets = append(targets, target{pool: proxyPoolRotating, index: i + 1, url: raw})
	}

	out := proxyCheckResponse{Results: []proxyCheckResult{}}
	if len(targets) == 0 {

		targets = append(targets, target{pool: proxyPoolStatic, index: 1, url: ""})
		out.Direct = true
		out.Note = "两个代理池都是空的 —— 探测会走本机直连，所以这里测的就是直连出口。"
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyCheckBudget)
	defer cancel()

	pool := newProbeClientPool()
	defer pool.closeIdle()

	results := make([]proxyCheckResult, len(targets))
	gate := make(chan struct{}, proxyCheckParallel)
	var wg sync.WaitGroup
	started := time.Now()
	for slot, tgt := range targets {
		wg.Add(1)

		go func(slot int, t target) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			results[slot] = proxyCheckOne(ctx, pool, t.pool, t.index, t.url, model)
		}(slot, tgt)
	}
	wg.Wait()

	out.MS = time.Since(started).Milliseconds()
	out.Results = results
	out.Checked = len(results)
	out.TimedOut = ctx.Err() != nil

	seen := map[string]bool{}
	for _, row := range results {
		switch row.Verdict {
		case proxyVerdictOK:
			out.OK++
		case proxyVerdictDead:
			out.Dead++
		case proxyVerdictBlocked, proxyVerdictRateLimited:
			out.Blocked++
		default:
			out.Other++
		}
		if row.Mismatch != "" {
			out.Mismatches++
		}
		if row.Pool == proxyPoolStatic {
			out.StaticChecked++
			if row.ExitIP != "" {
				seen[row.ExitIP] = true
			}
		}
	}
	out.DistinctIPs = len(seen)

	log.Printf(logPrefix+"proxy check: %d entr(ies) -> %d ok, %d refused, %d unreachable, %d other; %d misdeclared; %d distinct static address(es) in %dms",
		out.Checked, out.OK, out.Blocked, out.Dead, out.Other, out.Mismatches, out.DistinctIPs, out.MS)

	return jsonResponse(http.StatusOK, out)
}

func proxyCheckOne(ctx context.Context, pool *probeClientPool, poolName string, index int, raw, model string) proxyCheckResult {
	out := proxyCheckResult{Index: index, Pool: poolName, Proxy: probeShowProxy(raw), Verdict: proxyVerdictDead}

	client, errClient := pool.get(raw)
	if errClient != nil {

		out.Detail = "这条代理地址本身有问题：" + probeRedact(errClient.Error())
		return out
	}

	if trace := proxyCheckTrace(ctx, client); trace != nil {
		out.ExitIP, out.Country, out.Colo = trace["ip"], trace["loc"], trace["colo"]
		if second := proxyCheckTrace(ctx, client); second != nil && second["ip"] != "" && out.ExitIP != "" {
			out.Rotated = second["ip"] != out.ExitIP
		}
	}

	if out.Rotated && poolName == proxyPoolStatic && raw != "" {
		out.Mismatch = "这条在静态池里，但两次采样给了不同地址 —— 它其实是轮换的，应该移到轮换池。"
	}

	started := time.Now()
	status, errReach := proxyCheckReach(ctx, client, model)
	out.MS = time.Since(started).Milliseconds()
	if errReach != nil {
		out.Detail = "连不上：" + probeRedact(errReach.Error())
		return out
	}

	out.StatusCode = status
	switch status {
	case http.StatusUnauthorized:
		out.Verdict = proxyVerdictOK
		out.Detail = "通。401 正是不带凭据时该有的答复，说明请求确实走到了 OpenAI。"
	case http.StatusForbidden:
		out.Verdict = proxyVerdictBlocked
		out.Detail = "连得上，但上游拒绝（403）—— 多半是这个出口地址被挡了。"
	case http.StatusTooManyRequests:
		out.Verdict = proxyVerdictRateLimited
		out.Detail = "连得上，但被限速（429）—— 这个出口短期内请求太多。"
	default:
		out.Verdict = proxyVerdictUnexpected
		out.Detail = fmt.Sprintf("连得上，上游回了 %d（不带凭据时预期是 401）。", status)
	}
	return out
}

func proxyCheckReach(ctx context.Context, client *http.Client, model string) (int, error) {
	payload := map[string]any{
		"model":  model,
		"stream": true,
		"store":  false,
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "ping",
			}},
		}},
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return 0, errMarshal
	}

	callCtx, cancel := context.WithTimeout(ctx, proxyCheckRequestTimeout)
	defer cancel()
	request, errNew := http.NewRequestWithContext(callCtx, http.MethodPost, probeUpstreamURL, bytes.NewReader(raw))
	if errNew != nil {
		return 0, errNew
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Originator", "codex-tui")
	request.Header.Set("Session-Id", probeUUID())
	request.Header.Set("User-Agent", probeUserAgent)

	response, errDo := client.Do(request)
	if errDo != nil {
		return 0, errDo
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, proxyCheckMaxBody))
	return response.StatusCode, nil
}

func proxyCheckTrace(ctx context.Context, client *http.Client) map[string]string {
	callCtx, cancel := context.WithTimeout(ctx, proxyCheckRequestTimeout)
	defer cancel()
	request, errNew := http.NewRequestWithContext(callCtx, http.MethodGet, proxyCheckTraceURL, nil)
	if errNew != nil {
		return nil
	}
	request.Header.Set("User-Agent", probeUserAgent)

	response, errDo := client.Do(request)
	if errDo != nil {
		return nil
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, proxyCheckMaxBody))
		return nil
	}

	out := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, proxyCheckMaxBody))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		switch key {
		case "ip", "loc", "colo":
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
