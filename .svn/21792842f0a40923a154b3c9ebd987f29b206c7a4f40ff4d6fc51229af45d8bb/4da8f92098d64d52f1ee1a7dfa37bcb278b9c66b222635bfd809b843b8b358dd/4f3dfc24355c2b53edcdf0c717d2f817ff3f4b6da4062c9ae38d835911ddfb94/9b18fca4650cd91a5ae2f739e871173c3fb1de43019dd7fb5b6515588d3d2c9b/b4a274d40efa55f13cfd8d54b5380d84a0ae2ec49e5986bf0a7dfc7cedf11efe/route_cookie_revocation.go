package main

import (
	"net/http"
	"strings"
	"time"
)

func routeCookieDeletion(headers http.Header, now time.Time) bool {
	for key, lines := range headers {
		if !strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		for _, line := range lines {
			name, value, deadline := parseSetCookieLine(line, now)
			if name != "" && routeCookieWanted(name) &&
				(value == "" || (!deadline.IsZero() && !deadline.After(now))) {
				return true
			}
		}
	}
	return false
}

func (s *pluginState) revokeRouteCookieLocked(key string, now time.Time) {
	if key == "" || s.cookies[key] == nil {
		return
	}
	delete(s.cookies, key)
	s.cookiesDirty = true
	s.flushRouteCookiesLocked(now)
}

func rememberUnchangedRoute(requestID, pairKey string) {
	if requestID == "" || pairKey == "" {
		return
	}
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	if entry, exists := pendingAuth.byID[requestID]; exists {
		entry.pairKey = pairKey
		pendingAuth.byID[requestID] = entry
	}
}
