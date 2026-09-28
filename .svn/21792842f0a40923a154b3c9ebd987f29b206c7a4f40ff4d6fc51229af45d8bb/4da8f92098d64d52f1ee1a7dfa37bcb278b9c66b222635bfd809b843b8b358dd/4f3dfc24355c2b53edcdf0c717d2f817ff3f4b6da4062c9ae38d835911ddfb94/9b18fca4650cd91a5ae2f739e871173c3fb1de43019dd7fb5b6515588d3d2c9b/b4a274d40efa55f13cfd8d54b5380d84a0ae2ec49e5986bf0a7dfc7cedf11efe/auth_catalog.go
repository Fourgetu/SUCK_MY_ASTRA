package main

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type codexAuth struct {
	AuthID	string
	Enabled	bool
}

const authListCacheTTL = 2 * time.Second

var (
	authListMu	sync.Mutex
	authListCache	[]codexAuth
	authListErr	error
	authListFetched	time.Time
)

var codexAuthLister = listCodexAuths

var authCatalogLister = listAuthCatalog

func cachedCodexAuths() ([]codexAuth, error) {
	authListMu.Lock()
	defer authListMu.Unlock()
	if !authListFetched.IsZero() && time.Since(authListFetched) < authListCacheTTL {
		return authListCache, authListErr
	}
	authListCache, authListErr = codexAuthLister()
	authListFetched = time.Now()
	return authListCache, authListErr
}

var (
	authCatalogMu		sync.Mutex
	authCatalogCache	[]pluginapi.HostAuthFileEntry
	authCatalogErr		error
	authCatalogFetched	time.Time
)

func cachedAuthCatalog() ([]pluginapi.HostAuthFileEntry, error) {
	authCatalogMu.Lock()
	defer authCatalogMu.Unlock()
	if !authCatalogFetched.IsZero() && time.Since(authCatalogFetched) < authListCacheTTL {
		return authCatalogCache, authCatalogErr
	}
	authCatalogCache, authCatalogErr = authCatalogLister()
	authCatalogFetched = time.Now()
	return authCatalogCache, authCatalogErr
}

func resetAuthCache() {
	authListMu.Lock()
	authListCache = nil
	authListErr = nil
	authListFetched = time.Time{}
	authListMu.Unlock()
	authCatalogMu.Lock()
	authCatalogCache = nil
	authCatalogErr = nil
	authCatalogFetched = time.Time{}
	authCatalogMu.Unlock()
}

func listAuthCatalog() ([]pluginapi.HostAuthFileEntry, error) {
	var listed struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errCall := hostCallJSON("host.auth.list", map[string]any{}, &listed); errCall != nil {
		return nil, errCall
	}
	return listed.Files, nil
}

func listCodexAuths() ([]codexAuth, error) {
	files, errList := listAuthCatalog()
	if errList != nil {
		return nil, errList
	}
	var out []codexAuth
	for _, file := range files {
		if !isCodexAuth(file) {
			continue
		}
		name := strings.TrimSpace(file.Name)
		if name == "" {
			continue
		}

		out = append(out, codexAuth{AuthID: name, Enabled: !file.Disabled && !file.Unavailable})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out, nil
}

func isCodexAuth(file pluginapi.HostAuthFileEntry) bool {
	if strings.EqualFold(strings.TrimSpace(file.Provider), "codex") ||
		strings.EqualFold(strings.TrimSpace(file.Type), "codex") {
		return true
	}

	name := strings.ToLower(strings.TrimSpace(file.Name))
	return strings.HasPrefix(name, "codex-") && strings.HasSuffix(name, ".json")
}

func soleEnabledCodexAuth() (string, int, error) {
	accounts, errList := cachedCodexAuths()
	if errList != nil {
		return "", 0, errList
	}
	name := ""
	count := 0
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		count++
		name = account.AuthID
	}
	if count != 1 {
		return "", count, nil
	}
	return name, 1, nil
}

func selectedAuthIsCodex(authID, authIndex string) (codex, resolved bool) {
	files, errList := cachedAuthCatalog()
	if errList != nil {
		return false, false
	}
	for i := range files {
		if authIndex != "" && files[i].AuthIndex == authIndex {
			return entryIsCodex(files[i]), true
		}
	}
	for i := range files {
		if authID != "" && (files[i].ID == authID || files[i].Name == authID) {
			return entryIsCodex(files[i]), true
		}
	}
	return false, true
}

func entryIsCodex(file pluginapi.HostAuthFileEntry) bool {
	if provider := strings.ToLower(strings.TrimSpace(file.Provider)); provider != "" {
		return provider == "codex"
	}
	if kind := strings.ToLower(strings.TrimSpace(file.Type)); kind != "" {
		return kind == "codex"
	}
	return looksCodexAuthID(file.Name) || looksCodexAuthID(file.ID)
}
