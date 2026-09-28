package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, errTemp := os.CreateTemp(dir, ".tmp-*")
	if errTemp != nil {
		return errTemp
	}
	name := tmp.Name()

	defer func() { _ = os.Remove(name) }()

	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return errWrite
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return errSync
	}
	if errClose := tmp.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(name, path)
}

func bucketKey(authID, model string) string {
	return authID + "\x00" + model
}

func legacyRouteCookieEntries(dir string) []routeCookieEntry {
	authDirs, errRead := os.ReadDir(dir)
	if errRead != nil {
		return nil
	}
	var out []routeCookieEntry
	for _, authDir := range authDirs {
		if !authDir.IsDir() {
			continue
		}
		files, errAuth := os.ReadDir(filepath.Join(dir, authDir.Name()))
		if errAuth != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			data, errFile := os.ReadFile(filepath.Join(dir, authDir.Name(), file.Name()))
			if errFile != nil {
				continue
			}
			var legacy struct {
				RouteCookies		map[string]string	`json:"route_cookies"`
				RouteCookiesAt		string			`json:"route_cookies_at"`
				RouteCookiesExpire	string			`json:"route_cookies_expire"`
			}
			if errUnmarshal := json.Unmarshal(data, &legacy); errUnmarshal != nil {
				continue
			}
			if len(legacy.RouteCookies) == 0 || legacy.RouteCookiesAt == "" {
				continue
			}
			out = append(out, routeCookieEntry{
				Pairs:		legacy.RouteCookies,
				Gateway:	gatewayLabel(legacy.RouteCookies),
				SeenAt:		legacy.RouteCookiesAt,
				ExpireAt:	legacy.RouteCookiesExpire,
			})
		}
	}
	return out
}

func bucketRelPath(authID, model string) (string, error) {
	safe := func(s string) bool {
		if s == "" || len(s) > 256 {
			return false
		}
		return strings.IndexAny(s, `/\`+"\x00") < 0 && !strings.Contains(s, "..")
	}
	if !safe(authID) {
		return "", fmt.Errorf("unsafe auth_id %q", authID)
	}
	if !safe(model) {
		return "", fmt.Errorf("unsafe model %q", model)
	}
	return authID + "/" + model + ".json", nil
}
