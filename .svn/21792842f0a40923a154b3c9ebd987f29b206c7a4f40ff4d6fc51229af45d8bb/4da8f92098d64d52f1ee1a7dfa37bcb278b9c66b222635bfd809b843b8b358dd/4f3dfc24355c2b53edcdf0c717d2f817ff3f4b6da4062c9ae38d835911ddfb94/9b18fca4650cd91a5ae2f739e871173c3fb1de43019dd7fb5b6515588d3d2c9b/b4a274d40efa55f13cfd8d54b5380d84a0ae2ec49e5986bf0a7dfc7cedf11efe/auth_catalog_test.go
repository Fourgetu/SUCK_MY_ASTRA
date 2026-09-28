package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func withAuthCatalog(t *testing.T, files []pluginapi.HostAuthFileEntry, err error) {
	t.Helper()
	authCatalogLister = func() ([]pluginapi.HostAuthFileEntry, error) {
		return files, err
	}
	resetAuthCache()
	t.Cleanup(func() {
		authCatalogLister = listAuthCatalog
		resetAuthCache()
	})
}

func TestEntryIsCodexProviderPrecedence(t *testing.T) {
	cases := []struct {
		name string
		file pluginapi.HostAuthFileEntry
		want bool
	}{

		{"provider codex, plain name", pluginapi.HostAuthFileEntry{Name: "work.json", Provider: "codex"}, true},
		{"provider codex, case-insensitive", pluginapi.HostAuthFileEntry{Name: "work.json", Provider: "Codex"}, true},
		{"provider gemini, codex name", pluginapi.HostAuthFileEntry{Name: "codex-evil.json", Provider: "gemini"}, false},
		{"provider unknown, codex name", pluginapi.HostAuthFileEntry{Name: "codex-x.json", Provider: "unknown"}, false},

		{"type codex, provider empty", pluginapi.HostAuthFileEntry{Name: "work.json", Type: "codex"}, true},
		{"type gemini, codex name", pluginapi.HostAuthFileEntry{Name: "codex-evil.json", Type: "gemini"}, false},

		{"codex name, nothing reported", pluginapi.HostAuthFileEntry{Name: "codex-a.json"}, true},
		{"codex id, nothing reported", pluginapi.HostAuthFileEntry{ID: "codex-b.json"}, true},
		{"plain name, nothing reported", pluginapi.HostAuthFileEntry{Name: "work.json"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryIsCodex(tc.file); got != tc.want {
				t.Fatalf("entryIsCodex(%+v) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}

func TestSteerUsesProviderNotFilename(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "work-account.json", Name: "work-account.json", Provider: "codex"},
	}, nil)

	resp := interceptAfter(t, request("work-account.json", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); !strings.Contains(cookie, "__cflb=cf") {
		t.Fatalf("a provider-verified Codex account under a plain filename was not steered: %q", cookie)
	}
}

func TestSteerRefusesCodexNamedForeignCredential(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "codex-evil.json", Name: "codex-evil.json", Provider: "gemini"},
	}, nil)

	resp := interceptAfter(t, request("codex-evil.json", "gemini-3", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); strings.Contains(cookie, "cf") {
		t.Fatalf("a codex-named foreign credential carried the pool pair: %q", cookie)
	}
}

func TestSteerRefusesAuthAbsentFromCatalog(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "codex-a.json", Name: "codex-a.json", Provider: "codex"},
	}, nil)

	resp := interceptAfter(t, request("codex-ghost.json", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); strings.Contains(cookie, "cf") {
		t.Fatalf("an auth the catalog does not list carried the pool pair: %q", cookie)
	}
}

func TestSteerFallsBackToFilenameWhenCatalogDown(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, nil, errors.New("host API unavailable"))

	resp := interceptAfter(t, request("codex-a.json", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); !strings.Contains(cookie, "__cflb=cf") {
		t.Fatalf("the filename fallback did not steer a codex-named account: %q", cookie)
	}
	resp2 := interceptAfter(t, request("gemini-someone.json", "gemini-3", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp2.Headers.Get("Cookie"); strings.Contains(cookie, "cf") {
		t.Fatalf("the filename fallback steered a foreign-named account: %q", cookie)
	}
}

func TestSteerMatchesBySelectedAuthIndex(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "original.json", Name: "original.json", AuthIndex: "idx-9", Provider: "codex"},
		{ID: "codex-decoy.json", Name: "codex-decoy.json", AuthIndex: "idx-4", Provider: "gemini"},
	}, nil)

	req := request("renamed.json", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77))
	req.Metadata[selectedAuthIndexMetadataKey] = "idx-9"
	resp := interceptAfter(t, req)
	if cookie := resp.Headers.Get("Cookie"); !strings.Contains(cookie, "__cflb=cf") {
		t.Fatalf("an index-verified Codex account was not steered: %q", cookie)
	}

	req2 := request("original.json", "gemini-3", fakeTokenSeed(312, wallClock(), 0x77))
	req2.Metadata[selectedAuthIndexMetadataKey] = "idx-4"
	resp2 := interceptAfter(t, req2)
	if cookie := resp2.Headers.Get("Cookie"); strings.Contains(cookie, "cf") {
		t.Fatalf("the index pointed at a foreign provider yet the pair was attached: %q", cookie)
	}
}

func TestSteerAttributesByIndexAlone(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "codex-a.json", Name: "codex-a.json", AuthIndex: "idx-9", Provider: "codex"},
	}, nil)

	req := pluginapi.RequestInterceptRequest{
		Model:    "gpt-5.6-sol",
		Metadata: map[string]any{selectedAuthIndexMetadataKey: "idx-9"},
		Headers:  http.Header{},
	}
	resp := interceptAfter(t, req)
	if cookie := resp.Headers.Get("Cookie"); !strings.Contains(cookie, "__cflb=cf") {
		t.Fatalf("an index-only Codex request was not steered: %q", cookie)
	}
}

func TestSoleAccountInferenceVerifiedByCatalog(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")
	withAuthList(t, enabledAccounts("work.json"), nil)
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "work.json", Name: "work.json", Provider: "codex"},
	}, nil)

	resp := interceptAfter(t, request("", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); !strings.Contains(cookie, "__cflb=cf") {
		t.Fatalf("the sole Codex account under a plain filename was not steered: %q", cookie)
	}
}

func TestSoleAccountInferenceRefusedByCatalog(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, businessConfig(dir, false))
	seedPoolEntry(t, map[string]string{"__cflb": "cf"}, time.Now(), "")

	withAuthList(t, enabledAccounts("codex-suspect.json"), nil)
	withAuthCatalog(t, []pluginapi.HostAuthFileEntry{
		{ID: "codex-suspect.json", Name: "codex-suspect.json", Provider: "gemini"},
	}, nil)

	resp := interceptAfter(t, request("", "gpt-5.6-sol", fakeTokenSeed(312, wallClock(), 0x77)))
	if cookie := resp.Headers.Get("Cookie"); strings.Contains(cookie, "cf") {
		t.Fatalf("an inferred account the catalog calls foreign carried the pair: %q", cookie)
	}
}
