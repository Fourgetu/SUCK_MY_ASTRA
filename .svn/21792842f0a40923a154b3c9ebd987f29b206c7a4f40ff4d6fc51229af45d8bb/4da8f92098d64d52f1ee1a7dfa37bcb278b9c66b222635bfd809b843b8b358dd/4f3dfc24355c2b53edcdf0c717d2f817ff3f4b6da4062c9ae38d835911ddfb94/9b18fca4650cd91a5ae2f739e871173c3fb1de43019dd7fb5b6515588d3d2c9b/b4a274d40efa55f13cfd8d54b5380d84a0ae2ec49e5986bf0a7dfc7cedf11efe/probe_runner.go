package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	probeRouteAuthFiles    = "/v0/management/auth-files"
	probeRouteAuthDownload = "/v0/management/auth-files/download"
)

var (
	probeUpstreamURL = "https://chatgpt.com/backend-api/codex/responses"
	probeUserAgent   = "codex-tui/0.154.0 (Ubuntu 24.04; x86_64) OVH (codex-tui; 0.154.0)"
)

const (
	probeMaxLines = 40

	probeFireTimeout = 60 * time.Second
	probeMgmtTimeout = 30 * time.Second

	probeMaxAccountsInFlight = 4

	probeMaxBodyBytes = 1 << 10

	probeMgmtMaxBodyBytes = 4 << 20
)

var (
	probeRenewInterval  = 20 * time.Second
	probeRenewThreshold = 120 * time.Second

	probeExitCooldown = 55 * time.Minute

	probeExitPause = 2 * time.Second

	probeRotatingAttempts = 10
	probeRotatingCooldown = 10 * time.Minute

	probeAccountBackoff = 10 * time.Minute
)

type probeRunState struct {
	Running    bool     `json:"running"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Done       int      `json:"done"`
	Total      int      `json:"total"`
	Current    string   `json:"current,omitempty"`
	Lines      []string `json:"lines,omitempty"`
	Error      string   `json:"error,omitempty"`
}

var probeRunner struct {
	mu     sync.Mutex
	run    probeRunState
	cancel context.CancelFunc
}

type probeTarget struct {
	account string
	model   string
}

type probeCredential struct {
	name        string
	accessToken string
	accountID   string
	proxyURL    string
	expiresAt   time.Time
}

func probeRunStart() error {
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	accounts := append([]string(nil), cfg.ProbeAccounts...)
	models := append([]string(nil), cfg.Models...)
	proxies := append([]string(nil), cfg.ProbeProxies...)
	rotating := append([]string(nil), cfg.ProbeProxiesRotating...)

	switch {
	case len(accounts) == 0:

		return fmt.Errorf("probe_accounts is empty, so there is nothing to probe; refusing to widen an empty selection to every credential")
	case len(models) == 0:
		return fmt.Errorf("models is empty, so there is no payload to mint with")
	case strings.TrimSpace(cfg.ProbeManagementKey) == "":
		return fmt.Errorf("probe_management_key is not set; it is the Bearer for GET %s and %s, the two read-only calls that fetch the account list and each credential's token", probeRouteAuthFiles, probeRouteAuthDownload)
	case strings.TrimSpace(cfg.StoreDir) == "":
		return fmt.Errorf("store_dir is empty, so a minted pair has nowhere to be pooled for the business role to read")
	}

	probeRunner.mu.Lock()
	defer probeRunner.mu.Unlock()
	if probeRunner.run.Running {
		return fmt.Errorf("a probe is already running; stop it before starting another")
	}

	ctx, cancel := context.WithCancel(context.Background())
	probeRunner.cancel = cancel
	probeRunner.run = probeRunState{
		Running:   true,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	go probeSweep(ctx, cfg, accounts, models, proxies, rotating)
	return nil
}

func probeRunCancel() bool {
	probeRunner.mu.Lock()
	defer probeRunner.mu.Unlock()
	if !probeRunner.run.Running || probeRunner.cancel == nil {
		return false
	}
	probeRunner.cancel()
	probeRunner.run.Lines = probeAppendLine(probeRunner.run.Lines, "stop requested; the probe will finish the current harvest and exit")
	return true
}

func probeRunSnapshot() probeRunState {
	probeRunner.mu.Lock()
	defer probeRunner.mu.Unlock()
	out := probeRunner.run
	out.Lines = append([]string(nil), probeRunner.run.Lines...)
	return out
}

func probeRunUpdate(mutate func(run *probeRunState)) {
	probeRunner.mu.Lock()
	defer probeRunner.mu.Unlock()
	mutate(&probeRunner.run)
}

func probeRunLog(format string, args ...any) {
	line := probeRedact(fmt.Sprintf(format, args...))
	probeRunUpdate(func(run *probeRunState) {
		run.Lines = probeAppendLine(run.Lines, line)
	})
	log.Printf("%sprobe %s", logPrefix, line)
}

func probeRunFail(errRun error) {
	if errRun == nil {
		return
	}
	message := probeRedact(errRun.Error())
	probeRunUpdate(func(run *probeRunState) {
		if run.Error == "" {
			run.Error = message
		}
		run.Lines = probeAppendLine(run.Lines, "error: "+message)
	})
	log.Printf("%sprobe error: %s", logPrefix, message)
}

func probeRunFinish() {
	probeRunner.mu.Lock()
	defer probeRunner.mu.Unlock()
	probeRunner.run.Running = false
	probeRunner.run.Current = ""
	probeRunner.run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if probeRunner.cancel != nil {
		probeRunner.cancel()
		probeRunner.cancel = nil
	}
}

func probeAppendLine(lines []string, line string) []string {
	lines = append(lines, time.Now().UTC().Format("15:04:05")+" "+line)
	if len(lines) > probeMaxLines {

		lines = append([]string(nil), lines[len(lines)-probeMaxLines:]...)
	}
	return lines
}

func probeSweep(ctx context.Context, cfg pluginConfig, accounts, models, proxies, rotating []string) {

	defer probeRunFinish()

	defer func() {
		if recovered := recover(); recovered != nil {
			probeRunFail(fmt.Errorf("probe panicked: %v", recovered))
		}
	}()

	pool := newProbeClientPool()
	defer pool.closeIdle()
	client := newProbeClient(cfg)
	defer client.http.CloseIdleConnections()

	auths, errList := client.listCodexAuths(ctx)
	if errList != nil {
		probeRunFail(fmt.Errorf("could not list Codex credentials: %w", errList))
		return
	}

	known := make(map[string]bool, len(auths))
	for _, auth := range auths {
		known[auth.Name] = true
	}
	for _, account := range accounts {
		if !known[account] {
			probeRunFail(fmt.Errorf("selected account %q is not among CPA's Codex credentials; fix the selection rather than probing something CPA cannot serve", account))
			return
		}
	}

	now := time.Now()
	creds := probeDownloadCreds(ctx, client, accounts, now)
	if len(creds) == 0 {
		probeRunFail(fmt.Errorf("no usable credentials: every selected account's token was unreadable or already expired"))
		return
	}

	idxOf := probeAccountIndex(accounts)
	targets := probePendingTargets(cfg, accounts, models)
	probeRunUpdate(func(run *probeRunState) { run.Total = len(targets) })
	probeRunLog("offline harvest: %d account(s) to mint a pair each, %d static exit(s) + %d rotating entr(ies)", len(targets), len(proxies), len(rotating))
	if len(targets) > 0 {
		if cooling := probeFireBatch(ctx, cfg, pool, creds, targets, idxOf, proxies, rotating, true); cooling > 0 {

			probeRunLog("%d target(s) skipped: every exit already tried within the %s cooldown", cooling, probeExitCooldown)
		}
	} else {
		probeRunLog("no accounts in scope to mint with")
	}

	if ctx.Err() != nil {
		probeRunLog("stopped on request")
		return
	}

	probeRunLog("initial fill done; renewal active — the pool re-mints automatically within %s of the best entry's expiry", probeRenewThreshold)
	probeRunUpdate(func(run *probeRunState) { run.Current = "renewal active" })
	probeRenewLoop(ctx, pool)
}

func probeAccountIndex(accounts []string) map[string]int {
	idx := make(map[string]int, len(accounts))
	for i, name := range accounts {
		idx[name] = i
	}
	return idx
}

func probeFireBatch(ctx context.Context, cfg pluginConfig, pool *probeClientPool, creds map[string]probeCredential, targets []probeTarget, idxOf map[string]int, proxies, rotating []string, countDone bool) int {

	byAccount := make(map[string][]probeTarget, len(creds))
	var order []string
	for _, target := range targets {
		if _, seen := byAccount[target.account]; !seen {
			order = append(order, target.account)
		}
		byAccount[target.account] = append(byAccount[target.account], target)
	}

	var cooling atomic.Int64
	sem := make(chan struct{}, probeMaxAccountsInFlight)
	var wg sync.WaitGroup
	for _, account := range order {
		cred, ok := creds[account]
		if !ok {

			if countDone {
				missing := len(byAccount[account])
				probeRunUpdate(func(run *probeRunState) { run.Done += missing })
			}
			continue
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(list []probeTarget, cred probeCredential, accountIdx int) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, target := range list {
				if ctx.Err() != nil {
					return
				}
				if !probeHarvestBucket(ctx, cfg, pool, cred, target.model, proxies, rotating, accountIdx) {
					cooling.Add(1)
				}
				if countDone {
					probeRunUpdate(func(run *probeRunState) { run.Done++ })
				}
			}
		}(byAccount[account], cred, idxOf[account])
	}
	wg.Wait()
	return int(cooling.Load())
}

func probeHarvestBucket(ctx context.Context, cfg pluginConfig, pool *probeClientPool, cred probeCredential, model string, proxies, rotating []string, accountIdx int) bool {
	key := bucketKey(cred.name, model)
	if !probeClaim(key) {

		return false
	}
	defer probeRelease(key)

	short := maskAuthLabel(cred.name)
	if !probeAccountReady(cred.name, time.Now()) {

		return false
	}

	staticExits := probeExits(proxies, accountIdx)
	if len(proxies) == 0 && len(rotating) > 0 {
		staticExits = nil
	}

	fired := false
	for _, exit := range staticExits {
		if ctx.Err() != nil {
			return fired
		}

		if fired && !probeSleep(ctx, probeExitPause) {
			return fired
		}
		now := time.Now()
		if !probeCooldownReady(exit, cred.name, model, now) {

			continue
		}
		client, errClient := pool.get(exit)
		if errClient != nil {
			probeRunLog("%s %s: exit %s unusable, trying next: %s", short, model, probeShowProxy(exit), probeRedact(errClient.Error()))
			continue
		}

		probeCooldownMark(exit, cred.name, model, now)
		fired = true

		res, errFire := probeFireUpstream(ctx, client, cred, model)
		if errFire != nil {

			probeRunLog("%s %s: exit %s failed at transport, trying next: %s", short, model, probeShowProxy(exit), probeRedact(errFire.Error()))
			continue
		}
		switch probeConsume(cfg, cred.name, short, model, res, exit) {
		case probeOutcomeStored:

			probeCooldownSet(exit, cred.name, model, time.Now().Add(probeSuccessRest(cfg)))
			return true
		case probeOutcomeAccountLimited:

			probeAccountSetBackoff(cred.name, time.Now())
			return fired
		}

	}

	if len(rotating) > 0 {
		stored, rotFired := probeHarvestRotating(ctx, cfg, pool, cred, short, model, rotating, accountIdx)
		fired = fired || rotFired
		if stored {
			return true
		}
	}
	return fired
}

func probeHarvestRotating(ctx context.Context, cfg pluginConfig, pool *probeClientPool, cred probeCredential, short, model string, rotating []string, accountIdx int) (stored, fired bool) {
	now := time.Now()
	if !probeCooldownReady(probeRotatingExit, cred.name, model, now) {
		return false, false
	}
	probeCooldownSet(probeRotatingExit, cred.name, model, now.Add(probeRotatingCooldown))

	for attempt := 0; attempt < probeRotatingAttempts; attempt++ {
		if ctx.Err() != nil {
			return false, fired
		}
		if fired && !probeSleep(ctx, probeExitPause) {
			return false, fired
		}
		exit := rotating[(accountIdx+attempt)%len(rotating)]
		client, errClient := pool.get(exit)
		if errClient != nil {
			probeRunLog("%s %s: rotating exit %s unusable, trying next: %s", short, model, probeShowProxy(exit), probeRedact(errClient.Error()))
			continue
		}
		fired = true

		res, errFire := probeFireUpstream(ctx, client, cred, model)
		if errFire != nil {
			probeRunLog("%s %s: rotating exit %s failed at transport, trying next: %s", short, model, probeShowProxy(exit), probeRedact(errFire.Error()))
			continue
		}
		switch probeConsume(cfg, cred.name, short, model, res, exit) {
		case probeOutcomeStored:

			probeCooldownSet(probeRotatingExit, cred.name, model, time.Now().Add(probeSuccessRest(cfg)))
			return true, fired
		case probeOutcomeAccountLimited:

			probeAccountSetBackoff(cred.name, time.Now())
			return false, fired
		}

	}
	if fired {
		probeRunLog("%s %s: rotating pool gave %d address(es), none of them a %d; resting this bucket for %s",
			short, model, probeRotatingAttempts, cfg.TemplateLength, probeRotatingCooldown)
	}
	return false, fired
}

func probeConsume(cfg pluginConfig, name, short, model string, res probeFireResult, exit string) probeOutcome {
	if len(res.cookies.pairs) > 0 {
		state.mu.Lock()
		state.noteRouteCookiesLocked(res.cookies, exit)
		state.mu.Unlock()
	}
	switch res.status {
	case http.StatusTooManyRequests:

		probeRunLog("%s %s: http=429 — upstream is rate limiting this credential, not this exit; stopping the walk and resting the account for %s",
			short, model, probeAccountBackoff)
		return probeOutcomeAccountLimited
	case http.StatusUnauthorized, http.StatusForbidden:

		probeRunLog("%s %s: http=%d — the credential was refused, no exit can change that; resting the account for %s",
			short, model, res.status, probeAccountBackoff)
		return probeOutcomeAccountLimited
	}
	if res.status != http.StatusOK {
		probeRunLog("%s %s: http=%d via %s, no pair", short, model, res.status, probeShowProxy(exit))
		return probeOutcomeTryNext
	}
	stateLen := len(res.stateValue)
	if stateLen == cfg.ReplaceLength {

		probeRunLog("%s %s: http=200 via %s answered degraded (len=%d)%s — this exit's IP is throttled, trying next",
			short, model, probeShowProxy(exit), stateLen, pooledSuffix(res.cookies.pairs))
		return probeOutcomeTryNext
	}
	if len(res.cookies.pairs) == 0 {

		probeRunLog("%s %s: http=200 via %s but no __cflb/__oailb was set", short, model, probeShowProxy(exit))
		return probeOutcomeTryNext
	}
	switch stateLen {
	case cfg.TemplateLength, 0:
		probeRunLog("%s %s: pooled a %s pair via %s", short, model, orDash(gatewayLabel(res.cookies.pairs)), probeShowProxy(exit))
	default:
		probeRunLog("%s %s: pooled a %s pair via %s (turn-state len=%d)", short, model, orDash(gatewayLabel(res.cookies.pairs)), probeShowProxy(exit), stateLen)
	}
	return probeOutcomeStored
}

func pooledSuffix(pairs map[string]string) string {
	if len(pairs) == 0 {
		return ""
	}
	return fmt.Sprintf(" (pair still pooled: %s)", orDash(gatewayLabel(pairs)))
}

func probeSuccessRest(cfg pluginConfig) time.Duration {
	rest := cfg.ttl() - probeRenewThreshold
	if rest < probeExitPause {
		return probeExitPause
	}
	if rest > probeExitCooldown {
		return probeExitCooldown
	}
	return rest
}

func probeDownloadCreds(ctx context.Context, client *probeClient, accounts []string, now time.Time) map[string]probeCredential {
	creds := make(map[string]probeCredential, len(accounts))
	for _, name := range accounts {
		if ctx.Err() != nil {
			return creds
		}
		short := maskAuthLabel(name)
		blob, errDownload := client.downloadAuth(ctx, name)
		if errDownload != nil {
			probeRunLog("%s: could not read credential: %s", short, probeRedact(errDownload.Error()))
			continue
		}
		cred, errParse := probeParseCredential(name, blob)
		if errParse != nil {
			probeRunLog("%s: credential unusable: %s", short, probeRedact(errParse.Error()))
			continue
		}

		if !cred.expiresAt.IsZero() && !cred.expiresAt.After(now) {
			probeRunLog("%s: access token expired; skipping (not refreshed here — CPA refreshes it, next cycle harvests)", short)
			continue
		}
		creds[name] = cred
	}
	return creds
}

func probeRenewLoop(ctx context.Context, pool *probeClientPool) {
	ticker := time.NewTicker(probeRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return
		}

		state.mu.Lock()
		cfg := state.config
		state.mu.Unlock()
		accounts := append([]string(nil), cfg.ProbeAccounts...)
		models := append([]string(nil), cfg.Models...)
		proxies := append([]string(nil), cfg.ProbeProxies...)
		rotating := append([]string(nil), cfg.ProbeProxiesRotating...)
		if len(accounts) == 0 || len(models) == 0 {
			probeRunUpdate(func(run *probeRunState) { run.Current = "scope is empty; nothing to keep fresh" })
			continue
		}

		now := time.Now()
		left, poolLive := probePoolSecondsLeft(cfg, now)
		if poolLive && left >= probeRenewThreshold {
			probeRunUpdate(func(run *probeRunState) {
				run.Current = fmt.Sprintf("pool fresh (%s left on best pair); next check in %s", left.Round(time.Second), probeRenewInterval)
			})
			continue
		}
		idxOf := probeAccountIndex(accounts)
		var due []probeTarget
		involved := make(map[string]bool)
		for _, account := range accounts {

			if !probeAccountReady(account, now) {
				continue
			}

			model := models[0]

			if !probeBucketHasEligibleExit(proxies, rotating, idxOf[account], account, model, now) {
				continue
			}
			due = append(due, probeTarget{account: account, model: model})
			involved[account] = true
		}
		if len(due) == 0 {
			probeRunUpdate(func(run *probeRunState) {
				run.Current = fmt.Sprintf("pool needs pairs but every exit is cooling or every account resting; next check in %s", probeRenewInterval)
			})
			continue
		}

		probeRunUpdate(func(run *probeRunState) {
			run.Current = fmt.Sprintf("minting fresh pair(s), best entry has %s left", left.Round(time.Second))
		})

		var accountList []string
		for _, account := range accounts {
			if involved[account] {
				accountList = append(accountList, account)
			}
		}
		client := newProbeClient(cfg)
		creds := probeDownloadCreds(ctx, client, accountList, now)
		probeFireBatch(ctx, cfg, pool, creds, due, idxOf, proxies, rotating, false)
		client.http.CloseIdleConnections()
	}
}

func probePoolSecondsLeft(cfg pluginConfig, now time.Time) (time.Duration, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	var best int64
	for _, e := range state.cookies {
		if l := entrySecondsLeft(*e, now, cfg.ttl()); l > best {
			best = l
		}
	}
	return time.Duration(best) * time.Second, best > 0
}

func probePendingTargets(cfg pluginConfig, accounts, models []string) []probeTarget {
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	out := make([]probeTarget, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, probeTarget{account: account, model: model})
	}
	return out
}

func probeExits(proxies []string, accountIdx int) []string {
	if len(proxies) == 0 {
		return []string{""}
	}
	n := len(proxies)
	out := make([]string, 0, n)
	for k := 0; k < n; k++ {
		out = append(out, proxies[(accountIdx+k)%n])
	}
	return out
}

var probeActive = struct {
	mu  sync.Mutex
	set map[string]bool
}{set: map[string]bool{}}

func probeClaim(key string) bool {
	probeActive.mu.Lock()
	defer probeActive.mu.Unlock()
	if probeActive.set[key] {
		return false
	}
	probeActive.set[key] = true
	return true
}

func probeRelease(key string) {
	probeActive.mu.Lock()
	defer probeActive.mu.Unlock()
	delete(probeActive.set, key)
}

var probeCooldown = struct {
	mu    sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

const probeRotatingExit = "\x00rotating"

func probeCooldownKey(exit, account, model string) string {
	return exit + "\x00" + account + "\x00" + model
}

func probeCooldownReady(exit, account, model string, now time.Time) bool {
	probeCooldown.mu.Lock()
	defer probeCooldown.mu.Unlock()
	until, seen := probeCooldown.until[probeCooldownKey(exit, account, model)]
	return !seen || !now.Before(until)
}

func probeCooldownMark(exit, account, model string, now time.Time) {
	probeCooldownSet(exit, account, model, now.Add(probeExitCooldown))
}

func probeCooldownSet(exit, account, model string, until time.Time) {
	probeCooldown.mu.Lock()
	defer probeCooldown.mu.Unlock()
	probeCooldown.until[probeCooldownKey(exit, account, model)] = until
}

var probeAccountRest = struct {
	mu    sync.Mutex
	until map[string]time.Time
}{until: make(map[string]time.Time)}

func probeAccountReady(account string, now time.Time) bool {
	probeAccountRest.mu.Lock()
	defer probeAccountRest.mu.Unlock()
	until, seen := probeAccountRest.until[account]
	return !seen || now.After(until)
}

func probeAccountSetBackoff(account string, now time.Time) {
	probeAccountRest.mu.Lock()
	defer probeAccountRest.mu.Unlock()
	probeAccountRest.until[account] = now.Add(probeAccountBackoff)
}

type probeOutcome int

const (
	probeOutcomeStored probeOutcome = iota

	probeOutcomeTryNext

	probeOutcomeAccountLimited
)

func probeBucketHasEligibleExit(proxies, rotating []string, accountIdx int, account, model string, now time.Time) bool {

	if len(rotating) > 0 && probeCooldownReady(probeRotatingExit, account, model, now) {
		return true
	}

	if len(proxies) == 0 && len(rotating) > 0 {
		return false
	}
	for _, exit := range probeExits(proxies, accountIdx) {
		if probeCooldownReady(exit, account, model, now) {
			return true
		}
	}
	return false
}

func probeSleep(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type probeFireResult struct {
	status     int
	stateValue string
	cookies    routeCookieSet
}

func probeFireUpstream(ctx context.Context, client *http.Client, cred probeCredential, model string) (probeFireResult, error) {
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
		"reasoning":           map[string]any{"effort": "low"},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return probeFireResult{}, errMarshal
	}

	callCtx, cancel := context.WithTimeout(ctx, probeFireTimeout)
	defer cancel()
	request, errNew := http.NewRequestWithContext(callCtx, http.MethodPost, probeUpstreamURL, bytes.NewReader(raw))
	if errNew != nil {
		return probeFireResult{}, errNew
	}
	request.Header.Set("Authorization", "Bearer "+cred.accessToken)
	if cred.accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", cred.accountID)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Originator", "codex-tui")
	request.Header.Set("Session-Id", probeUUID())
	request.Header.Set("User-Agent", probeUserAgent)

	response, errDo := client.Do(request)
	if errDo != nil {
		return probeFireResult{}, errDo
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, probeMaxBodyBytes))
	return probeFireResult{
		status:     response.StatusCode,
		stateValue: response.Header.Get(turnStateHeader),
		cookies:    routeCookiesFromResponseHeaders(response.Header, time.Now()),
	}, nil
}

func probeUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func probeParseCredential(name string, blob map[string]any) (probeCredential, error) {
	token := strings.TrimSpace(stringField(blob, "access_token"))
	if token == "" {
		return probeCredential{}, fmt.Errorf("no access_token in credential file")
	}
	claims := probeJWTClaims(token)
	cred := probeCredential{
		name:        name,
		accessToken: token,
		accountID:   probeAccountID(claims, blob),
		proxyURL:    strings.TrimSpace(stringField(blob, "proxy_url")),
	}
	if exp, ok := probeTokenExpiry(claims); ok {
		cred.expiresAt = exp
	}
	return cred, nil
}

func probeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	data, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(data, &claims); errUnmarshal != nil {
		return nil
	}
	return claims
}

func probeAccountID(claims, blob map[string]any) string {
	if claims != nil {
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if id, ok := auth["chatgpt_account_id"].(string); ok && strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	return strings.TrimSpace(stringField(blob, "account_id"))
}

func probeTokenExpiry(claims map[string]any) (time.Time, bool) {
	if claims == nil {
		return time.Time{}, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

func stringField(blob map[string]any, key string) string {
	if value, ok := blob[key].(string); ok {
		return value
	}
	return ""
}

type probeAuthFile struct {
	Name      string          `json:"name"`
	AuthIndex json.RawMessage `json:"auth_index,omitempty"`
	Disabled  bool            `json:"disabled"`
	Provider  string          `json:"provider"`
	Type      string          `json:"type"`
}

type probeHTTPResult struct {
	status int
	body   []byte
}

type probeClient struct {
	baseURL string
	mgmtKey string
	http    *http.Client
}

func newProbeClient(cfg pluginConfig) *probeClient {

	base := strings.TrimRight(strings.TrimSpace(cfg.ProbeBaseURL), "/")
	if base == "" {
		base = defaultProbeBaseURL
	}
	return &probeClient{
		baseURL: base,
		mgmtKey: strings.TrimSpace(cfg.ProbeManagementKey),

		http: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

func (c *probeClient) call(ctx context.Context, method, path, token string, payload any, timeout time.Duration) (probeHTTPResult, error) {
	var body io.Reader
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return probeHTTPResult{}, errMarshal
		}
		body = bytes.NewReader(raw)
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, errNew := http.NewRequestWithContext(callCtx, method, c.baseURL+path, body)
	if errNew != nil {
		return probeHTTPResult{}, errNew
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, errDo := c.http.Do(request)
	if errDo != nil {
		return probeHTTPResult{}, errDo
	}
	defer func() { _ = response.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(response.Body, probeMgmtMaxBodyBytes+1))
	if errRead != nil {
		return probeHTTPResult{status: response.StatusCode}, errRead
	}
	if len(raw) > probeMgmtMaxBodyBytes {
		return probeHTTPResult{status: response.StatusCode},
			fmt.Errorf("%s %s response exceeds %d bytes", method, path, probeMgmtMaxBodyBytes)
	}
	return probeHTTPResult{status: response.StatusCode, body: raw}, nil
}

func probeExplainStatus(result probeHTTPResult, what string) error {
	switch result.status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: 401 unauthorized -- probe_management_key is wrong or not accepted; this is an auth failure, not a bad path", what)
	case http.StatusForbidden:
		return fmt.Errorf("%s: 403 forbidden -- the key was accepted but lacks access", what)
	case http.StatusNotFound:
		return fmt.Errorf("%s: 404 not found -- this CPA build does not serve that path (a bad key would have answered 401)", what)
	}
	return fmt.Errorf("%s: HTTP %d %s", what, result.status, probeTruncate(probeRedact(string(result.body)), 200))
}

func (c *probeClient) listCodexAuths(ctx context.Context) ([]probeAuthFile, error) {
	result, errCall := c.call(ctx, http.MethodGet, probeRouteAuthFiles, c.mgmtKey, nil, probeMgmtTimeout)
	if errCall != nil {
		return nil, errCall
	}
	if result.status != http.StatusOK {
		return nil, probeExplainStatus(result, "GET "+probeRouteAuthFiles)
	}
	var doc struct {
		Files []probeAuthFile `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(result.body, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("GET %s returned a body that is not the expected {\"files\":[...]} document: %w", probeRouteAuthFiles, errUnmarshal)
	}
	out := make([]probeAuthFile, 0, len(doc.Files))
	for _, file := range doc.Files {
		name := strings.TrimSpace(file.Name)
		if name == "" || strings.Contains(name, ".bak") {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		kind := strings.ToLower(strings.TrimSpace(file.Type))
		lower := strings.ToLower(name)
		if provider != "codex" && kind != "codex" &&
			!(strings.HasPrefix(lower, "codex-") && strings.HasSuffix(lower, ".json")) {
			continue
		}
		file.Name = name
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *probeClient) downloadAuth(ctx context.Context, name string) (map[string]any, error) {
	path := probeRouteAuthDownload + "?name=" + url.QueryEscape(name)
	result, errCall := c.call(ctx, http.MethodGet, path, c.mgmtKey, nil, probeMgmtTimeout)
	if errCall != nil {
		return nil, errCall
	}
	if result.status != http.StatusOK {
		return nil, probeExplainStatus(result, "GET "+probeRouteAuthDownload)
	}
	var blob map[string]any
	if errUnmarshal := json.Unmarshal(result.body, &blob); errUnmarshal != nil {
		return nil, fmt.Errorf("GET %s returned a body that is not a JSON object: %w", probeRouteAuthDownload, errUnmarshal)
	}
	return blob, nil
}

type probeClientPool struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func newProbeClientPool() *probeClientPool {
	return &probeClientPool{clients: map[string]*http.Client{}}
}

func (p *probeClientPool) get(proxyURL string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client, ok := p.clients[proxyURL]; ok {
		return client, nil
	}
	transport := &http.Transport{Proxy: nil}
	if proxyURL != "" {
		parsed, errParse := url.Parse(proxyURL)
		if errParse != nil {
			return nil, fmt.Errorf("exit is not a valid URL: %w", errParse)
		}

		if strings.EqualFold(parsed.Scheme, "socks5h") {
			parsed.Scheme = "socks5"
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{Transport: transport}
	p.clients[proxyURL] = client
	return client, nil
}

func (p *probeClientPool) closeIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, client := range p.clients {
		client.CloseIdleConnections()
	}
}

var (
	probeURLAuthRE = regexp.MustCompile(`(?i)\b([a-z0-9+.\-]+://)[^/\s@]+@`)

	probeTokenRE = regexp.MustCompile(`gAAAAA[A-Za-z0-9_\-=]{16,}`)

	probeBearerRE = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._\-]{16,}`)
)

func probeRedact(text string) string {
	text = probeTokenRE.ReplaceAllString(text, "<turn-state redacted>")
	text = probeBearerRE.ReplaceAllString(text, "${1}<redacted>")
	return probeURLAuthRE.ReplaceAllString(text, "${1}***@")
}

func probeShowProxy(raw string) string {
	if masked := maskProxyURL(raw); masked != "" {
		return masked
	}
	return "(direct)"
}

func probeTruncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + fmt.Sprintf(" ...[+%d chars]", len(text)-limit)
}
