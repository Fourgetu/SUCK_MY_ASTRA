package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	testHeader  = "X-Codex-Turn-State"
	testAuthKey = "selected_auth_id"
)

var testNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

const testTTL = time.Hour

func wallClock() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func storeDirLine(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("store_dir: %q\n", t.TempDir())
}

var tokenByteLen = map[int]int{292: 217, 312: 233, 780: 585}

func fakeToken(n int, issued time.Time) string {
	return fakeTokenSeed(n, issued, 0x5a)
}

func fakeTokenSeed(n int, issued time.Time, seed byte) string {
	size, ok := tokenByteLen[n]
	if !ok {
		panic(fmt.Sprintf("fakeTokenSeed: no decoded byte length known for %d base64 chars", n))
	}
	raw := make([]byte, size)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for i := 9; i < size; i++ {
		raw[i] = seed + byte(i)
	}

	out := base64.URLEncoding.EncodeToString(raw)
	if len(out) != n {
		panic(fmt.Sprintf("fakeTokenSeed: encoded to %d chars, want %d", len(out), n))
	}
	return out
}

func configureYAML(t *testing.T, cfgYAML string) error {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"config_yaml":    []byte(cfgYAML),
		"schema_version": 6,
	})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	t.Cleanup(resetPluginConfig)
	return configure(raw)
}

func mustConfigure(t *testing.T, cfgYAML string) {
	t.Helper()
	if err := configureYAML(t, cfgYAML); err != nil {
		t.Fatalf("configure rejected a valid config: %v", err)
	}
}

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buffer bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	fn()
	return buffer.String()
}

func resetPluginConfig() {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = defaultConfig()
	state.configErrors = nil
}

func businessConfig(dir string, dryRun bool) string {
	return fmt.Sprintf(`role: business
store_dir: %q
template_length: 292
replace_length: 312
ttl_seconds: 3600
dry_run: %t
log_decisions: false
`, dir, dryRun)
}

func interceptAfter(t *testing.T, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal intercept request: %v", err)
	}
	out, err := handleMethod(pluginabi.MethodRequestInterceptAfter, raw)
	if err != nil {
		t.Fatalf("handleMethod(request.intercept_after): %v", err)
	}

	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("plugin returned an error envelope: %+v", env.Error)
	}
	var resp pluginapi.RequestInterceptResponse
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
			t.Fatalf("decode intercept response: %v", errUnmarshal)
		}
	}
	return resp
}

func request(authID, model, headerValue string) pluginapi.RequestInterceptRequest {
	req := pluginapi.RequestInterceptRequest{
		Model:    model,
		Metadata: map[string]any{testAuthKey: authID},
		Headers:  http.Header{},
	}
	if headerValue != "" {
		req.Headers.Set(testHeader, headerValue)
	}
	return req
}

func outgoingHeader(resp pluginapi.RequestInterceptResponse) string {
	if resp.Headers == nil {
		return ""
	}
	for key, values := range resp.Headers {
		if !equalFoldASCII(key, testHeader) {
			continue
		}
		for _, value := range values {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func TestFakeTokenMatchesObservedLengths(t *testing.T) {
	for _, n := range []int{292, 312} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			token := fakeToken(n, testNow)
			if len(token) != n {
				t.Fatalf("fakeToken produced %d chars, want %d", len(token), n)
			}
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
			if err != nil || len(raw) < 9 || raw[0] != 0x80 {
				t.Fatal("synthetic token is not in Fernet shape; the fixture no longer matches the real format")
			}
			issued := time.Unix(int64(binary.BigEndian.Uint64(raw[1:9])), 0)
			if !issued.Equal(testNow) {
				t.Fatalf("embedded timestamp = %s, want %s", issued.UTC(), testNow)
			}
		})
	}
}

func TestFakeTokenSeedProducesDistinctValues(t *testing.T) {
	a := fakeTokenSeed(292, testNow, 0x11)
	b := fakeTokenSeed(292, testNow, 0x22)
	if a == b {
		t.Fatal("two seeds produced the same token; the isolation tests would be vacuous")
	}
	if len(a) != 292 || len(b) != 292 {
		t.Fatalf("lengths = %d/%d, want 292/292", len(a), len(b))
	}
}

func businessHarvestConfig(dir string) string {
	return fmt.Sprintf(`role: business
store_dir: %q
template_length: 292
replace_length: 312
ttl_seconds: 3600
dry_run: false
log_decisions: false
`, dir)
}

func TestBusinessRoleDeclaresResponseHooks(t *testing.T) {
	mustConfigure(t, businessHarvestConfig(t.TempDir()))

	caps := pluginRegistration().Capabilities
	if !caps.ResponseInterceptor {
		t.Error("business role does not advertise the response interceptor, so it is never handed a response to harvest")
	}
	if !caps.StreamChunkInterceptor {
		t.Error("business role does not advertise the stream-chunk interceptor, which is the path real Codex SSE traffic takes")
	}
	if !caps.RequestInterceptor {
		t.Error("business role stopped advertising the request interceptor, so it cannot substitute at all")
	}
}

func TestBusinessRoleHarvestsFromLiveResponse(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessHarvestConfig(dir))
	resetHarvestState(t)

	chunk := pluginapi.StreamChunkInterceptRequest{
		Model:      "gpt-5.5",
		ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: setCookieHeaders(fakeToken(292, wallClock().Add(-time.Minute)),
			"__cflb=cf-live", "__oailb=lb-live"),
		Metadata: map[string]any{testAuthKey: "codex-alpha.json"},
	}
	raw, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		t.Fatalf("marshal chunk: %v", errMarshal)
	}
	if _, errHook := interceptStreamChunk(raw); errHook != nil {
		t.Fatalf("interceptStreamChunk: %v", errHook)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	key := cookieEntryKey(map[string]string{"__cflb": "cf-live", "__oailb": "lb-live"})
	if _, ok := state.cookies[key]; !ok {
		t.Fatalf("the live response's pair was not pooled; pool holds %d entries", len(state.cookies))
	}
}

func TestInBandHarvestAttributesByRequestID(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessHarvestConfig(dir))
	resetHarvestState(t)

	const (
		requestID = "req-correlate-1"
		auth      = "codex-alpha.json"
		model     = "gpt-5.5"
	)

	req := request(auth, model, "")
	req.RequestID = requestID
	interceptAfter(t, req)

	chunk := pluginapi.StreamChunkInterceptRequest{
		RequestID:       requestID,
		Model:           model,
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: harvestResponseHeaders(fakeToken(292, wallClock().Add(-time.Minute))),
		Metadata:        nil,
	}
	raw, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		t.Fatalf("marshal chunk: %v", errMarshal)
	}
	if _, errHook := interceptStreamChunk(raw); errHook != nil {
		t.Fatalf("interceptStreamChunk: %v", errHook)
	}

	cells, _, _ := observationsSnapshot()
	found := false
	for _, cell := range cells {
		if cell.AuthID == auth && cell.Model == model {
			found = true
		}
	}
	if !found {
		t.Fatalf("the response was not attributed back to %s; tally holds %d bucket(s)", auth, len(cells))
	}
}

func TestInBandHarvestForgetsAfterUse(t *testing.T) {
	rememberRequestAuth("req-once", "codex-alpha.json")
	got, steered, _ := recallRequestRecord("req-once")
	if got != "codex-alpha.json" {
		t.Fatalf("first recall = %q, want the recorded account", got)
	}
	if steered {
		t.Error("first recall reports the request was steered; markRequestSteered was never called")
	}
	if got, _, _ = recallRequestRecord("req-once"); got != "" {
		t.Fatalf("second recall = %q, want empty: the entry must be consumed", got)
	}
}

func TestRequestWriteFlagSurvivesRecall(t *testing.T) {
	rememberRequestAuth("req-wrote", "codex-alpha.json")
	markRequestSteered("req-wrote", "pair-key-1")
	got, steered, pairKey := recallRequestRecord("req-wrote")
	if got != "codex-alpha.json" || !steered || pairKey != "pair-key-1" {
		t.Fatalf("recall = (%q, %v, %q), want the account, steered and the pair key", got, steered, pairKey)
	}

	markRequestSteered("req-never-seen", "pair-key-2")
	if got, _, _ := recallRequestRecord("req-never-seen"); got != "" {
		t.Errorf("marking an unknown request created an entry with account %q", got)
	}
}

func TestConfigureRoleValidation(t *testing.T) {
	t.Run("missing role defaults to business", func(t *testing.T) {
		mustConfigure(t, "template_length: 292\nreplace_length: 312\n")
		state.mu.Lock()
		got := state.config.Role
		state.mu.Unlock()
		if got != roleBusiness {
			t.Fatalf("Role = %q, want %q: an absent role must fail safe to the non-writing side, "+
				"and DEPLOY.md step 6 installs the .so before step 7 sets the role", got, roleBusiness)
		}
	})

	t.Run("invalid role is rejected", func(t *testing.T) {
		if err := configureYAML(t, "role: probesque\n"); err == nil {
			t.Fatal("configure accepted an invalid role")
		}
	})

	t.Run("probe and business are both accepted", func(t *testing.T) {
		for _, role := range []string{roleProbe, roleBusiness} {
			if err := configureYAML(t, "role: "+role+"\n"+storeDirLine(t)); err != nil {
				t.Fatalf("configure rejected role %q: %v", role, err)
			}
		}
	})

	t.Run("probe without a store_dir is rejected", func(t *testing.T) {

		if err := configureYAML(t, "role: probe\n"); err == nil {
			t.Fatal("configure accepted a probe with no store_dir")
		}
	})
}

func TestIsProbe(t *testing.T) {
	probe := defaultConfig()
	probe.Role = roleProbe
	if !probe.isProbe() {
		t.Fatal("role probe did not report isProbe")
	}
	business := defaultConfig()
	business.Role = roleBusiness
	if business.isProbe() {
		t.Fatal("role business reported isProbe")
	}
}

func TestProbeBaseURLDefaultsToLoopback(t *testing.T) {
	if got := defaultConfig().ProbeBaseURL; got != defaultProbeBaseURL {
		t.Fatalf("defaultConfig().ProbeBaseURL = %q, want %q", got, defaultProbeBaseURL)
	}

	for name, cfgYAML := range map[string]string{
		"absent":           "role: business\n",
		"explicitly empty": "role: business\nprobe_base_url: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			mustConfigure(t, cfgYAML)
			state.mu.Lock()
			got := state.config.ProbeBaseURL
			state.mu.Unlock()
			if got != defaultProbeBaseURL {
				t.Fatalf("probe_base_url = %q, want the default %q", got, defaultProbeBaseURL)
			}
		})
	}

	t.Run("configured value wins", func(t *testing.T) {
		mustConfigure(t, "role: business\nprobe_base_url: http://127.0.0.1:9317\n")
		state.mu.Lock()
		got := state.config.ProbeBaseURL
		state.mu.Unlock()
		if got != "http://127.0.0.1:9317" {
			t.Fatalf("probe_base_url = %q, want the configured value", got)
		}
	})
}
