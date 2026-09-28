package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const opsProxyCheckPath = mgmtResourcePath + "ops/proxy-check"

type fakeCheckUpstream struct {
	mu		sync.Mutex
	status		int
	authSeen	[]string
	hdrSeen		[]http.Header
	server		*httptest.Server
}

func newFakeCheckUpstream(t *testing.T, status int) *fakeCheckUpstream {
	t.Helper()
	up := &fakeCheckUpstream{status: status}
	up.server = httptest.NewServer(up)
	t.Cleanup(up.server.Close)
	return up
}

func (u *fakeCheckUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.authSeen = append(u.authSeen, r.Header.Get("Authorization"))
	u.hdrSeen = append(u.hdrSeen, r.Header.Clone())
	status := u.status
	u.mu.Unlock()
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{}`))
}

func (u *fakeCheckUpstream) auths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.authSeen...)
}

type fakeTrace struct {
	mu	sync.Mutex
	ip	string
	loc	string
	colo	string
	status	int
	calls	int
	server	*httptest.Server
}

func newFakeTrace(t *testing.T) *fakeTrace {
	t.Helper()
	tr := &fakeTrace{ip: "203.0.113.7", loc: "GB", colo: "LHR", status: http.StatusOK}
	tr.server = httptest.NewServer(tr)
	t.Cleanup(tr.server.Close)
	return tr
}

func (f *fakeTrace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	ip, loc, colo, status := f.ip, f.loc, f.colo, f.status
	f.mu.Unlock()
	w.WriteHeader(status)

	_, _ = w.Write([]byte("fl=1a2b3c\nh=chatgpt.com\nip=" + ip +
		"\nts=1750000000\nvisit_scheme=https\ncolo=" + colo +
		"\nloc=" + loc + "\ntls=TLSv1.3\n"))
}

func setTraceURL(t *testing.T, rawURL string) {
	t.Helper()
	previous := proxyCheckTraceURL
	proxyCheckTraceURL = rawURL
	t.Cleanup(func() { proxyCheckTraceURL = previous })
}

func checkConfig(t *testing.T, dir string, proxies ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("role: business\nstore_dir: " + jsonQuote(dir) +
		"\ndry_run: true\nlog_decisions: false\n" +
		"models:\n  - gpt-5.5\n")
	if len(proxies) > 0 {
		b.WriteString("probe_proxies:\n")
		for _, p := range proxies {
			b.WriteString("  - " + jsonQuote(p) + "\n")
		}
	}
	return b.String()
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func decodeProxyCheck(t *testing.T, body []byte) proxyCheckResponse {
	t.Helper()
	var out proxyCheckResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode proxy-check response: %v\nbody: %s", err, body)
	}
	return out
}

func TestProxyCheckRequiresConfirm(t *testing.T) {

	mustConfigure(t, checkConfig(t, t.TempDir()))

	resp := driveResource(t, opsProxyCheckPath, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the proxy check ran without confirm=1; a prefetch could dial the whole pool")
	}
}

func TestProxyCheckVerdictsFollowTheUpstreamStatus(t *testing.T) {

	tests := []struct {
		name	string
		status	int
		verdict	string
	}{
		{"401 means the exit reached OpenAI", http.StatusUnauthorized, proxyVerdictOK},
		{"403 means the exit is refused", http.StatusForbidden, proxyVerdictBlocked},
		{"429 means the exit is rate limited", http.StatusTooManyRequests, proxyVerdictRateLimited},
		{"anything else is flagged, not assumed fine", http.StatusInternalServerError, proxyVerdictUnexpected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newFakeCheckUpstream(t, tc.status)
			trace := newFakeTrace(t)
			setUpstream(t, upstream.server.URL)
			setTraceURL(t, trace.server.URL)

			pool := newProbeClientPool()
			defer pool.closeIdle()
			got := proxyCheckOne(t.Context(), pool, proxyPoolStatic, 1, "", "gpt-5.5")

			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %q, want %q (status %d, detail %q)",
					got.Verdict, tc.verdict, tc.status, got.Detail)
			}
			if got.StatusCode != tc.status {
				t.Fatalf("status_code = %d, want %d", got.StatusCode, tc.status)
			}

			if got.ExitIP != "203.0.113.7" || got.Country != "GB" || got.Colo != "LHR" {
				t.Fatalf("trace fields not reported: ip=%q loc=%q colo=%q",
					got.ExitIP, got.Country, got.Colo)
			}
		})
	}
}

func TestProxyCheckSendsNoCredential(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	trace := newFakeTrace(t)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, trace.server.URL)

	pool := newProbeClientPool()
	defer pool.closeIdle()
	proxyCheckOne(t.Context(), pool, proxyPoolStatic, 1, "", "gpt-5.5")

	auths := upstream.auths()
	if len(auths) == 0 {
		t.Fatal("the upstream was never called")
	}
	for i, value := range auths {
		if value != "" {
			t.Fatalf("call %d carried an Authorization header (%q); this check must spend no quota", i, value)
		}
	}
}

func TestProxyCheckSeparatesUnreachableFromRefused(t *testing.T) {

	trace := newFakeTrace(t)
	setTraceURL(t, trace.server.URL)

	setUpstream(t, "http://exit.invalid:9/responses")

	pool := newProbeClientPool()
	defer pool.closeIdle()
	got := proxyCheckOne(t.Context(), pool, proxyPoolStatic, 1, "", "gpt-5.5")

	if got.Verdict != proxyVerdictDead {
		t.Fatalf("verdict = %q, want %q for an exit that cannot be reached", got.Verdict, proxyVerdictDead)
	}
	if got.StatusCode != 0 {
		t.Fatalf("status_code = %d, want 0 when no response was ever received", got.StatusCode)
	}
}

func TestProxyCheckSurvivesATraceOutage(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, "http://trace.invalid:9/cdn-cgi/trace")

	pool := newProbeClientPool()
	defer pool.closeIdle()
	got := proxyCheckOne(t.Context(), pool, proxyPoolStatic, 1, "", "gpt-5.5")

	if got.Verdict != proxyVerdictOK {
		t.Fatalf("verdict = %q, want %q; a trace outage must not change the verdict", got.Verdict, proxyVerdictOK)
	}
	if got.ExitIP != "" {
		t.Fatalf("exit_ip = %q, want empty when the trace failed", got.ExitIP)
	}
}

func TestProxyCheckEmptyPoolChecksTheDirectExit(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	trace := newFakeTrace(t)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, trace.server.URL)
	mustConfigure(t, checkConfig(t, t.TempDir()))

	resp := driveResource(t, opsProxyCheckPath, confirmed(nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeProxyCheck(t, resp.Body)
	if !got.Direct {
		t.Fatal("direct = false; an empty pool means the box's own egress and should say so")
	}
	if got.Checked != 1 || got.OK != 1 {
		t.Fatalf("checked=%d ok=%d, want 1 and 1", got.Checked, got.OK)
	}
	if got.Note == "" {
		t.Fatal("no note explaining that the pool is empty")
	}
}

func TestProxyCheckCountsDistinctExitAddresses(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	trace := newFakeTrace(t)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, trace.server.URL)

	var hitsA, hitsB atomic.Int64
	proxyA := newFakeProxy(t, &hitsA)
	proxyB := newFakeProxy(t, &hitsB)
	mustConfigure(t, checkConfig(t, t.TempDir(), proxyA, proxyB))

	resp := driveResource(t, opsProxyCheckPath, confirmed(nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeProxyCheck(t, resp.Body)
	if got.Checked != 2 {
		t.Fatalf("checked = %d, want 2", got.Checked)
	}

	if got.DistinctIPs != 1 {
		t.Fatalf("distinct_ips = %d, want 1 -- two pool entries sharing one address must be visible",
			got.DistinctIPs)
	}

	if len(got.Results) != 2 || got.Results[0].Index != 1 || got.Results[1].Index != 2 {
		t.Fatalf("results are not indexed 1..n: %+v", got.Results)
	}
}

func TestProxyCheckNeverLeaksAProxyPassword(t *testing.T) {

	trace := newFakeTrace(t)
	setTraceURL(t, trace.server.URL)
	setUpstream(t, "http://exit.invalid:9/responses")
	mustConfigure(t, checkConfig(t, t.TempDir(), testProxyWithPW))

	var resp mgmtResponse
	logged := captureLog(t, func() {
		resp = driveResource(t, opsProxyCheckPath, confirmed(nil))
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(string(resp.Body), testProxySecret) {
		t.Fatalf("the proxy password reached the keyless response body:\n%s", resp.Body)
	}
	if strings.Contains(logged, testProxySecret) {
		t.Fatalf("the proxy password reached the log:\n%s", logged)
	}

	got := decodeProxyCheck(t, resp.Body)
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}
	if got.Results[0].Verdict != proxyVerdictDead {
		t.Fatalf("verdict = %q, want %q for an unresolvable exit", got.Results[0].Verdict, proxyVerdictDead)
	}

	if !strings.Contains(got.Results[0].Proxy, "***@exit.invalid:1080") {
		t.Fatalf("proxy = %q, want the userinfo replaced wholesale", got.Results[0].Proxy)
	}
}

func TestProxyCheckIsRegisteredKeylessWithoutAMenu(t *testing.T) {

	reg := driveManagementRegister(t)
	var found bool
	for _, route := range reg.Resources {
		if route.Path == routeOpsProxyCheck {
			found = true
			if route.Menu != "" {
				t.Fatalf("the proxy-check route declares Menu %q; it is data the page fetches, not a page to navigate to", route.Menu)
			}
		}
	}
	if !found {
		t.Fatalf("%s is not registered as a resource route", routeOpsProxyCheck)
	}
	for _, route := range reg.Routes {
		if strings.Contains(route.Path, "proxy-check") {
			t.Fatal("the proxy-check route is also declared as a management route; one home only")
		}
	}
}

type rotatingTrace struct {
	mu	sync.Mutex
	n	int
	server	*httptest.Server
}

func newRotatingTrace(t *testing.T) *rotatingTrace {
	t.Helper()
	tr := &rotatingTrace{}
	tr.server = httptest.NewServer(tr)
	t.Cleanup(tr.server.Close)
	return tr
}

func (f *rotatingTrace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf("ip=203.0.113.%d\nloc=GB\ncolo=LHR\n", n)))
}

func TestProxyCheckFlagsARotatingEntryDeclaredStatic(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, newRotatingTrace(t).server.URL)

	var hits atomic.Int64
	exit := newFakeProxy(t, &hits)
	mustConfigure(t, checkConfig(t, t.TempDir(), exit))

	resp := driveResource(t, opsProxyCheckPath, confirmed(nil))
	got := decodeProxyCheck(t, resp.Body)
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}
	row := got.Results[0]
	if !row.Rotated {
		t.Fatal("two samples returned different addresses but rotated is false")
	}
	if row.Mismatch == "" {
		t.Fatal("a rotating entry sitting in the static pool was not flagged")
	}
	if got.Mismatches != 1 {
		t.Fatalf("mismatches = %d, want 1", got.Mismatches)
	}
}

func TestProxyCheckDoesNotFlagASteadyRotatingEntry(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	trace := newFakeTrace(t)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, trace.server.URL)

	var hits atomic.Int64
	exit := newFakeProxy(t, &hits)
	mustConfigure(t, checkConfig(t, t.TempDir())+
		"probe_proxies_rotating:\n  - "+jsonQuote(exit)+"\n")

	resp := driveResource(t, opsProxyCheckPath, confirmed(nil))
	got := decodeProxyCheck(t, resp.Body)
	if len(got.Results) != 1 || got.Results[0].Pool != proxyPoolRotating {
		t.Fatalf("the rotating pool was not checked: %+v", got.Results)
	}
	if got.Results[0].Mismatch != "" || got.Mismatches != 0 {
		t.Fatalf("a rotating entry that happened to repeat an address was flagged: %q", got.Results[0].Mismatch)
	}
}

func TestProxyCheckCountsDistinctAddressesForTheStaticPoolOnly(t *testing.T) {

	upstream := newFakeCheckUpstream(t, http.StatusUnauthorized)
	setUpstream(t, upstream.server.URL)
	setTraceURL(t, newRotatingTrace(t).server.URL)

	var hits atomic.Int64
	exit := newFakeProxy(t, &hits)
	mustConfigure(t, checkConfig(t, t.TempDir())+
		"probe_proxies_rotating:\n  - "+jsonQuote(exit)+"\n")

	resp := driveResource(t, opsProxyCheckPath, confirmed(nil))
	got := decodeProxyCheck(t, resp.Body)
	if got.StaticChecked != 0 {
		t.Fatalf("static_checked = %d, want 0 -- only a rotating entry was configured", got.StaticChecked)
	}
	if got.DistinctIPs != 0 {
		t.Fatalf("distinct_ips = %d, want 0: rotating addresses must not be counted", got.DistinctIPs)
	}
}
