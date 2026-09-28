package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func businessBlockConfig(dir string) string {
	return fmt.Sprintf(`role: business
store_dir: %q
template_length: 292
replace_length: 312
ttl_seconds: 3600
dry_run: false
log_decisions: false
block_degraded: true
`, dir)
}

func streamChunkResp(t *testing.T, chunk pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	t.Helper()
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	out, errHook := interceptStreamChunk(raw)
	if errHook != nil {
		t.Fatalf("interceptStreamChunk: %v", errHook)
	}
	var env struct {
		OK	bool		`json:"ok"`
		Result	json.RawMessage	`json:"result"`
	}
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("decode envelope: %v ok=%v", errUnmarshal, env.OK)
	}
	var resp pluginapi.StreamChunkInterceptResponse
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
			t.Fatalf("decode chunk response: %v", errUnmarshal)
		}
	}
	return resp
}

func resetBlockedStreams() {
	blockedStreams.mu.Lock()
	blockedStreams.ids = make(map[string]time.Time)
	blockedStreams.mu.Unlock()
}

func TestBlockDegradedStreamWithheld(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessBlockConfig(dir))
	resetHarvestState(t)
	resetObservations(t, "")
	resetModelScans()
	resetBlockedStreams()

	const requestID = "req-block-1"
	req := request("codex-alpha.json", "gpt-6-astra", "")
	req.RequestID = requestID
	interceptAfter(t, req)
	streamChunk(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:		requestID,
		Model:			"gpt-6-astra",
		ChunkIndex:		pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders:	harvestResponseHeaders(fakeToken(780, wallClock())),
	})

	resp := streamChunkResp(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:	requestID,
		Model:		"gpt-6-astra",
		ChunkIndex:	0,
		Body: []byte("event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"r1","model":"gpt-5.6-luna","status":"in_progress"}}` + "\n\n"),
	})
	if resp.DropChunk {
		t.Fatal("the verdict chunk was dropped outright; it must carry the error event")
	}
	body := string(resp.Body)
	for _, want := range []string{"response.failed", "degraded_model_blocked", "gpt-5.6-luna", "gpt-6-astra"} {
		if !strings.Contains(body, want) {
			t.Fatalf("synthetic event missing %q: %s", want, body)
		}
	}

	for i := 1; i <= 3; i++ {
		resp := streamChunkResp(t, pluginapi.StreamChunkInterceptRequest{
			RequestID:	requestID,
			Model:		"gpt-6-astra",
			ChunkIndex:	i,
			Body:		[]byte(`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"),
		})
		if !resp.DropChunk || len(resp.Body) > 0 {
			t.Fatalf("chunk %d not withheld: %+v", i, resp)
		}
	}

	cell := observedBucket(t, "codex-alpha.json", "gpt-6-astra")
	if cell.NaturalLimited != 1 {
		t.Fatalf("NaturalLimited = %d, want 1", cell.NaturalLimited)
	}
}

func TestBlockDegradedOffDeliversNormally(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessHarvestConfig(dir))
	resetHarvestState(t)
	resetObservations(t, "")
	resetModelScans()
	resetBlockedStreams()

	const requestID = "req-noblock"
	req := request("codex-alpha.json", "gpt-6-astra", "")
	req.RequestID = requestID
	interceptAfter(t, req)
	streamChunk(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:		requestID,
		Model:			"gpt-6-astra",
		ChunkIndex:		pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders:	harvestResponseHeaders(fakeToken(780, wallClock())),
	})
	resp := streamChunkResp(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:	requestID,
		Model:		"gpt-6-astra",
		ChunkIndex:	0,
		Body:		[]byte(`data: {"type":"response.created","response":{"model":"gpt-5.6-luna"}}` + "\n\n"),
	})
	if resp.DropChunk || len(resp.Body) > 0 {
		t.Fatalf("gate off must not touch the chunk: %+v", resp)
	}
	resp = streamChunkResp(t, pluginapi.StreamChunkInterceptRequest{
		RequestID:	requestID,
		Model:		"gpt-6-astra",
		ChunkIndex:	1,
		Body:		[]byte(`data: {"delta":"x"}` + "\n\n"),
	})
	if resp.DropChunk {
		t.Fatal("gate off must not drop subsequent chunks")
	}
}

func TestBlockDegradedNonStreamBodyReplaced(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessBlockConfig(dir))
	resetHarvestState(t)
	resetObservations(t, "")
	resetModelScans()
	resetBlockedStreams()

	const requestID = "req-block-nonstream"
	req := request("codex-alpha.json", "gpt-6-astra", "")
	req.RequestID = requestID
	interceptAfter(t, req)

	raw, err := json.Marshal(pluginapi.ResponseInterceptRequest{
		RequestID:		requestID,
		Model:			"gpt-6-astra",
		ResponseHeaders:	harvestResponseHeaders(fakeToken(780, wallClock())),
		Body:			[]byte(`{"id":"r1","model": "gpt-5.6-luna","status":"completed"}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, errHook := interceptResponse(raw)
	if errHook != nil {
		t.Fatalf("interceptResponse: %v", errHook)
	}
	var env struct {
		OK	bool		`json:"ok"`
		Result	json.RawMessage	`json:"result"`
	}
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var resp pluginapi.ResponseInterceptResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if !strings.Contains(string(resp.Body), "degraded_model_blocked") {
		t.Fatalf("degraded body not replaced: %s", resp.Body)
	}
}

func TestClientTurnStateUsable(t *testing.T) {
	cfg := defaultCloudMintConfig()
	fresh := cloudTestTicket(time.Now().Truncate(time.Second))
	if !clientTurnStateUsable(fresh, cfg) {
		t.Fatal("fresh ticket judged unusable")
	}
	stale := cloudTestTicket(time.Now().Add(-300 * time.Second))
	if clientTurnStateUsable(stale, cfg) {
		t.Fatal("300s-old ticket inside a 240s window judged usable")
	}
	if clientTurnStateUsable("not-a-fernet", cfg) {
		t.Fatal("unparseable state judged usable")
	}
}

func TestCloudMintStaleClientTicketRewritten(t *testing.T) {
	resetCloudRequestLogTest(t)
	now := time.Now().Truncate(time.Second)
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		json.NewEncoder(w).Encode(cloudTestResult(now, "gpt-6-sol"))
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.CloudMint = defaultCloudMintConfig()
	cfg.CloudMint.Enabled = true
	cfg.CloudMint.URL = server.URL
	t.Setenv(cfg.CloudMint.KeyEnv, "relay-secret")
	resetCloudMintService()
	t.Cleanup(resetCloudMintService)

	old := cloudCredentialResolver
	t.Cleanup(func() { cloudCredentialResolver = old })
	cloudCredentialResolver = func(pluginapi.RequestInterceptRequest) (cloudMintCredentials, error) {
		return cloudMintCredentials{AuthID: "auth", AccessToken: "access-secret", AccountID: "account"}, nil
	}

	stale := make([]byte, 585)
	stale[0] = 0x80
	binary.BigEndian.PutUint64(stale[1:9], uint64(now.Add(-300*time.Second).Unix()))
	req := pluginapi.RequestInterceptRequest{RequestID: "stale-ride", Model: "gpt-6-sol",
		Headers:	http.Header{turnStateHeader: []string{base64.RawURLEncoding.EncodeToString(stale)}}}
	out := interceptCloudMint(req, cfg)
	if out.Terminate {
		t.Fatalf("stale ticket should mint, not 503: %+v", out)
	}
	if called.Load() == 0 {
		t.Fatal("stale ticket passed through without minting")
	}
	if out.Headers.Get(turnStateHeader) == "" {
		t.Fatal("fresh ticket not injected")
	}
}

func TestCloudMintFailOpenPassesThrough(t *testing.T) {
	resetCloudRequestLogTest(t)
	cfg := defaultConfig()
	cfg.CloudMint = defaultCloudMintConfig()
	cfg.CloudMint.Enabled = true
	cfg.CloudMint.FailClosed = false
	cfg.CloudMint.URL = "http://127.0.0.1:1/"
	t.Setenv(cfg.CloudMint.KeyEnv, "relay-secret")
	resetCloudMintService()
	t.Cleanup(resetCloudMintService)

	old := cloudCredentialResolver
	t.Cleanup(func() { cloudCredentialResolver = old })
	cloudCredentialResolver = func(pluginapi.RequestInterceptRequest) (cloudMintCredentials, error) {
		return cloudMintCredentials{AuthID: "auth", AccessToken: "access-secret"}, nil
	}

	out := interceptCloudMint(pluginapi.RequestInterceptRequest{RequestID: "failopen",
		Model:	"gpt-6-sol", Headers: http.Header{turnStateHeader: []string{"garbage-state"}}}, cfg)
	if out.Terminate {
		t.Fatal("fail_closed=false still terminated")
	}
	if out.Headers.Get(turnStateHeader) != "" {
		t.Fatal("no ticket should be injected on failure")
	}
	cleared := false
	for _, h := range out.ClearHeaders {
		if h == turnStateHeader {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the stale carried state was not stripped on fail-open")
	}
}

func TestCloudMintUnavailableCarriesReason(t *testing.T) {
	out := cloudMintUnavailable("mint_pending")
	if !out.Terminate || out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("not a 503: %+v", out)
	}
	if out.ResponseHeaders.Get("X-Mint-Reason") != "mint_pending" {
		t.Fatalf("missing reason header: %+v", out.ResponseHeaders)
	}
	if !strings.Contains(string(out.ResponseBody), "mint_pending") {
		t.Fatalf("reason not in body: %s", out.ResponseBody)
	}
	for _, err := range []error{
		errors.New("cloud mint pending; retry later"),
		errors.New("cloud mint busy; retry later"),
		errors.New("cloud mint stopped"),
		errors.New("invalid route cookie pair"),
		errors.New("anything else"),
	} {
		if reason := mintUnavailableReason(err); reason == "" {
			t.Fatalf("no reason for %v", err)
		}
	}
}
