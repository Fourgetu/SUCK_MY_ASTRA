package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func jwtWithExp(exp int64) string {
	payload := fmt.Sprintf(`{"exp":%d}`, exp)
	return "x." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".y"
}

func TestCookieMaxAgeOverflowCannotReadAsDeletion(t *testing.T) {

	set := routeCookiesFromResponseHeaders(setCookieHeaders("",
		"__cflb=a; Max-Age=9223372036854775807"), testNow)
	if _, ok := set.pairs["__cflb"]; !ok {
		t.Fatal("a huge Max-Age overflowed into a deletion and the pair was dropped")
	}
}

func TestJwtExpZeroReadsAsExpired(t *testing.T) {

	if got := jwtExpiresAt(jwtWithExp(0)); got.IsZero() {
		t.Fatal("exp=0 was read as an absent claim; the credential expired at the epoch")
	}
	live := testNow.Unix() + 60
	if got := jwtExpiresAt(jwtWithExp(live)); !got.Equal(time.Unix(live, 0).UTC()) {
		t.Fatalf("the exp claim was not honoured: %s", got)
	}
}

func TestRouteCookieDeletionDetectsEmptyAndLapsed(t *testing.T) {
	past := testNow.Add(-time.Minute).UTC().Format(http.TimeFormat)
	for _, line := range []string{
		"__cflb=",
		"__cflb=; Max-Age=0",
		"__oailb=gone; Expires=" + past,
	} {
		if !routeCookieDeletion(setCookieHeaders("", line), testNow) {
			t.Fatalf("deletion not detected: %q", line)
		}
	}
	if routeCookieDeletion(setCookieHeaders("", "__cflb=live; Max-Age=600"), testNow) {
		t.Fatal("a live issuance was read as a deletion")
	}
	if routeCookieDeletion(setCookieHeaders("", "oai-did=; Max-Age=0"), testNow) {
		t.Fatal("a non-replayed cookie's deletion was attributed to the pool")
	}
}

func TestHarvestDeletionEvictsTheCarriedPair(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	resetHarvestState(t)
	pairs := map[string]string{"__cflb": "cf-dead", "__oailb": "lb-dead"}
	seedPoolEntry(t, pairs, time.Now(), "")
	key := cookieEntryKey(pairs)

	rememberRequestAuth("req-del", "codex-alpha.json")
	markRequestSteered("req-del", key)

	past := testNow.Add(-time.Minute).UTC().Format(http.TimeFormat)
	headers := setCookieHeaders("", "__cflb=; Expires="+past, "__oailb=; Max-Age=0")
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()
	harvestFromResponse(cfg, headers, map[string]any{}, "gpt-5.5", "req-del")

	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.cookies[key]; ok {
		t.Fatal("the upstream's deletion left the dead pair in the pool")
	}
}

func TestHarvestDeletionLeavesOtherPairsAlone(t *testing.T) {

	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	resetHarvestState(t)
	carried := map[string]string{"__cflb": "cf-a", "__oailb": "lb-a"}
	other := map[string]string{"__cflb": "cf-b", "__oailb": "lb-b"}
	seedPoolEntry(t, carried, time.Now(), "")
	seedPoolEntry(t, other, time.Now(), "")

	rememberRequestAuth("req-del2", "codex-alpha.json")
	markRequestSteered("req-del2", cookieEntryKey(carried))

	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()
	harvestFromResponse(cfg, setCookieHeaders("", "__cflb=; Max-Age=0"), map[string]any{}, "gpt-5.5", "req-del2")

	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.cookies[cookieEntryKey(carried)]; ok {
		t.Fatal("the carried pair survived its own deletion")
	}
	if _, ok := state.cookies[cookieEntryKey(other)]; !ok {
		t.Fatal("an unrelated pair was evicted by another's deletion")
	}
}

func TestRememberUnchangedRouteRecordsKeyWithoutSteer(t *testing.T) {

	rememberRequestAuth("req-unchanged", "codex-alpha.json")
	rememberUnchangedRoute("req-unchanged", "key-1")
	_, steered, pairKey := recallRequestRecord("req-unchanged")
	if steered {
		t.Fatal("an untouched request was counted as steered")
	}
	if pairKey != "key-1" {
		t.Fatalf("pairKey = %q, want key-1", pairKey)
	}
}
