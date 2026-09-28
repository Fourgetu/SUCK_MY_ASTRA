package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func routeCookieWanted(name string) bool {
	switch strings.ToLower(strings.TrimLeft(name, "_")) {
	case "cflb", "oailb":
		return true
	}
	return false
}

type routeCookieSet struct {
	pairs		map[string]string
	seenAt		time.Time
	expireAt	time.Time
}

func routeCookiesFromResponseHeaders(headers http.Header, now time.Time) routeCookieSet {
	set := routeCookieSet{seenAt: now}
	for key, values := range headers {
		if !strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		for _, line := range values {
			name, value, deadline := parseSetCookieLine(line, now)
			if name == "" || !routeCookieWanted(name) {
				continue
			}
			if !deadline.IsZero() && !deadline.After(now) {

				continue
			}
			if set.pairs == nil {
				set.pairs = map[string]string{}
			}
			set.pairs[name] = value
			if !deadline.IsZero() && (set.expireAt.IsZero() || deadline.Before(set.expireAt)) {
				set.expireAt = deadline
			}
		}
	}
	return set
}

func parseSetCookieLine(line string, now time.Time) (name, value string, deadline time.Time) {
	segments := strings.Split(line, ";")
	first := strings.TrimSpace(segments[0])
	eq := strings.Index(first, "=")
	if eq <= 0 {
		return "", "", time.Time{}
	}
	name = strings.TrimSpace(first[:eq])
	value = strings.TrimSpace(first[eq+1:])
	if !cookieNameSafe(name) || !cookieValueSafe(value) {
		return "", "", time.Time{}
	}
	for _, attr := range segments[1:] {
		attr = strings.TrimSpace(attr)
		switch {
		case len(attr) > 8 && strings.EqualFold(attr[:8], "max-age="):
			if parsed, err := strconv.ParseInt(strings.TrimSpace(attr[8:]), 10, 64); err == nil {
				d := cookieMaxAgeDeadline(now, parsed)
				if deadline.IsZero() || d.Before(deadline) {
					deadline = d
				}
			}
		case len(attr) > 8 && strings.EqualFold(attr[:8], "expires="):

			if t, err := http.ParseTime(strings.TrimSpace(attr[8:])); err == nil {
				if deadline.IsZero() || t.Before(deadline) {
					deadline = t
				}
			}
		}
	}
	if t := jwtExpiresAt(value); !t.IsZero() {
		deadline = t
	}
	return name, value, deadline
}

func cookieMaxAgeDeadline(now time.Time, seconds int64) time.Time {
	const maximum = time.Duration(1<<63 - 1)
	limit := int64(maximum / time.Second)
	if seconds > limit {
		return now.Add(maximum)
	}
	if seconds < -limit {
		return now.Add(-maximum)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func jwtExpiresAt(value string) time.Time {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}
	}
	return time.Unix(*claims.Exp, 0).UTC()
}

func cookieNameSafe(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return strings.IndexAny(name, "=; \t\r\n,") < 0
}

func cookieValueSafe(value string) bool {
	if len(value) > 4096 {
		return false
	}
	return strings.IndexAny(value, "; \t\r\n,") < 0
}

func (s routeCookieSet) usable(now time.Time, ttl time.Duration) bool {
	if len(s.pairs) == 0 || s.seenAt.IsZero() || s.seenAt.After(now) {
		return false
	}
	if !now.Before(s.seenAt.Add(ttl)) {
		return false
	}
	if !s.expireAt.IsZero() && !now.Before(s.expireAt) {
		return false
	}
	return true
}

func (s routeCookieSet) header() string {
	return cookieHeaderValue(s.pairs)
}

func cookieHeaderValue(pairs map[string]string) string {
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+pairs[name])
	}
	return strings.Join(parts, "; ")
}

func mergeRouteCookies(existing string, pairs map[string]string) string {
	if len(pairs) == 0 {
		return existing
	}
	type pair struct{ name, value string }
	var pairs2 []pair
	positions := map[string]int{}
	for _, segment := range strings.Split(existing, ";") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		name, value := segment, ""
		if eq := strings.Index(segment, "="); eq >= 0 {
			name, value = strings.TrimSpace(segment[:eq]), segment[eq+1:]
		}
		if _, seen := positions[name]; seen {
			continue
		}
		positions[name] = len(pairs2)
		pairs2 = append(pairs2, pair{name, value})
	}
	for name, value := range pairs {
		if at, seen := positions[name]; seen {
			pairs2[at].value = value
			continue
		}
		positions[name] = len(pairs2)
		pairs2 = append(pairs2, pair{name, value})
	}
	var b strings.Builder
	for i, p := range pairs2 {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(p.name)
		b.WriteString("=")
		b.WriteString(p.value)
	}
	return b.String()
}

var routeCookieGatewayRe = regexp.MustCompile(`(?i)(unified[-_.]?\d+|gateway[-_.][a-z0-9\-]+)`)

func gatewayLabel(pairs map[string]string) string {
	for _, name := range []string{"__oailb", "__cflb"} {
		if m := routeCookieGatewayRe.FindString(pairs[name]); m != "" {
			return m
		}
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(cookieHeaderValue(pairs)))
	return fmt.Sprintf("lb-%08x", h.Sum32())
}

type routeCookieEntry struct {
	Pairs	map[string]string	`json:"pairs"`

	Gateway	string	`json:"gateway,omitempty"`

	Via	string	`json:"via,omitempty"`

	SeenAt	string	`json:"seen_at"`

	ExpireAt	string	`json:"expire_at,omitempty"`

	GoodAt	string	`json:"good_at,omitempty"`

	BadAt	string	`json:"bad_at,omitempty"`
}

const routeCookiePoolFile = "route-cookies.json"

const routeCookiePoolVersion = 1

type routeCookiePoolDoc struct {
	Version		int			`json:"version"`
	UpdatedAt	string			`json:"updated_at"`
	Entries		[]routeCookieEntry	`json:"entries"`
}

func cookieEntryKey(pairs map[string]string) string {
	return cookieHeaderValue(pairs)
}

func entrySeen(e routeCookieEntry) time.Time {
	t, _ := time.Parse(time.RFC3339, e.SeenAt)
	return t
}

func entryExpiry(e routeCookieEntry) time.Time {
	t, _ := time.Parse(time.RFC3339, e.ExpireAt)
	return t
}

func entryUsable(e routeCookieEntry, now time.Time, ttl time.Duration) bool {
	set := routeCookieSet{pairs: e.Pairs, seenAt: entrySeen(e), expireAt: entryExpiry(e)}
	return set.usable(now, ttl)
}

func entryBad(e routeCookieEntry, now time.Time, penalty time.Duration) bool {
	bad, err := time.Parse(time.RFC3339, e.BadAt)
	if err != nil {
		return false
	}
	return now.Sub(bad) < penalty
}

func entryScore(e routeCookieEntry, now time.Time) time.Time {
	if good, err := time.Parse(time.RFC3339, e.GoodAt); err == nil && good.After(entrySeen(e)) {
		return good
	}
	return entrySeen(e)
}

var routeCookieBadPenalty = 90 * time.Second

var routeCookieFlushInterval = 15 * time.Second

func loadRouteCookiePool(dir string) map[string]*routeCookieEntry {
	out := make(map[string]*routeCookieEntry)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return out
	}
	data, errRead := os.ReadFile(filepath.Join(dir, routeCookiePoolFile))
	if errRead == nil {
		var doc routeCookiePoolDoc
		if errUnmarshal := json.Unmarshal(data, &doc); errUnmarshal != nil {
			log.Printf(logPrefix+"%s unreadable, pool starts empty: %v", routeCookiePoolFile, errUnmarshal)
		} else if doc.Version != routeCookiePoolVersion {
			log.Printf(logPrefix+"%s is version %d, want %d; pool starts empty", routeCookiePoolFile, doc.Version, routeCookiePoolVersion)
		} else {
			for i := range doc.Entries {
				e := doc.Entries[i]
				if len(e.Pairs) == 0 {
					continue
				}
				out[cookieEntryKey(e.Pairs)] = &e
			}
		}
	} else if !os.IsNotExist(errRead) {
		log.Printf(logPrefix+"could not read %s: %v", routeCookiePoolFile, errRead)
	}

	for _, e := range legacyRouteCookieEntries(dir) {
		key := cookieEntryKey(e.Pairs)
		if cur, ok := out[key]; !ok || entrySeen(e).After(entrySeen(*cur)) {
			out[key] = &e
		}
	}
	return out
}

func writeRouteCookiePool(dir string, pool map[string]*routeCookieEntry, now time.Time, ttl time.Duration) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("store_dir is empty")
	}
	doc := routeCookiePoolDoc{Version: routeCookiePoolVersion, UpdatedAt: now.UTC().Format(time.RFC3339)}
	for _, e := range pool {
		if entryUsable(*e, now, ttl) {
			doc.Entries = append(doc.Entries, *e)
		}
	}
	sort.Slice(doc.Entries, func(i, j int) bool { return doc.Entries[i].Gateway < doc.Entries[j].Gateway })
	data, errMarshal := json.MarshalIndent(doc, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	return atomicWrite(filepath.Join(dir, routeCookiePoolFile), append(data, '\n'))
}

func entrySecondsLeft(e routeCookieEntry, now time.Time, ttl time.Duration) int64 {
	if !entryUsable(e, now, ttl) {
		return 0
	}
	deadline := entrySeen(e).Add(ttl)
	if expire := entryExpiry(e); !expire.IsZero() && expire.Before(deadline) {
		deadline = expire
	}
	return int64(deadline.Sub(now).Seconds())
}

func bestRouteCookie(pool map[string]*routeCookieEntry, now time.Time, ttl time.Duration) (routeCookieEntry, bool) {
	var best, fallback *routeCookieEntry
	var bestScore, fallbackScore time.Time
	for _, e := range pool {
		if !entryUsable(*e, now, ttl) {
			continue
		}
		score := entryScore(*e, now)
		if entryBad(*e, now, routeCookieBadPenalty) {
			if fallback == nil || score.After(fallbackScore) {
				fallback, fallbackScore = e, score
			}
			continue
		}
		if best == nil || score.After(bestScore) {
			best, bestScore = e, score
		}
	}
	if best != nil {
		return *best, true
	}

	if fallback != nil {
		return *fallback, true
	}
	return routeCookieEntry{}, false
}

func (s *pluginState) noteRouteCookiesLocked(set routeCookieSet, via string) {
	if len(set.pairs) == 0 || set.seenAt.IsZero() {
		return
	}
	key := cookieEntryKey(set.pairs)
	e := s.cookies[key]
	if e == nil {
		e = &routeCookieEntry{
			Pairs:		set.pairs,
			Gateway:	gatewayLabel(set.pairs),
		}
		s.cookies[key] = e
	}
	if set.seenAt.After(entrySeen(*e)) {
		e.SeenAt = set.seenAt.UTC().Format(time.RFC3339)
	}
	if !set.expireAt.IsZero() {
		e.ExpireAt = set.expireAt.UTC().Format(time.RFC3339)
	}
	if via != "" {

		e.Via = via
	}
	s.cookiesDirty = true
	s.flushRouteCookiesLocked(time.Now())
}

func (s *pluginState) flushRouteCookiesLocked(now time.Time) {
	if !s.cookiesDirty || s.config.StoreDir == "" {
		return
	}
	if !s.cookiesFlushed.IsZero() && now.Sub(s.cookiesFlushed) < routeCookieFlushInterval {
		return
	}
	if err := writeRouteCookiePool(s.config.StoreDir, s.cookies, now, s.config.ttl()); err != nil {
		log.Printf(logPrefix+"pool flush failed: %v", err)
		return
	}
	s.cookiesDirty = false
	s.cookiesFlushed = now
}

func (s *pluginState) bestRouteCookieLocked(now time.Time, ttl time.Duration) (routeCookieSet, string, bool) {
	e, ok := bestRouteCookie(s.cookies, now, ttl)
	if !ok {
		return routeCookieSet{}, "", false
	}
	return routeCookieSet{pairs: e.Pairs, seenAt: entrySeen(e), expireAt: entryExpiry(e)},
		cookieEntryKey(e.Pairs), true
}

func (s *pluginState) markRouteCookieOutcomeLocked(key, kind string, now time.Time) {
	e := s.cookies[key]
	if e == nil {
		return
	}
	switch kind {
	case observationLimited:
		e.BadAt = now.UTC().Format(time.RFC3339)
	case observationSilent:
		return
	default:
		e.GoodAt = now.UTC().Format(time.RFC3339)
	}
	s.cookiesDirty = true
	s.flushRouteCookiesLocked(now)
}

func (s *pluginState) poolSecondsLeftLocked(now time.Time, ttl time.Duration) int64 {
	var left int64
	for _, e := range s.cookies {
		if l := entrySecondsLeft(*e, now, ttl); l > left {
			left = l
		}
	}
	return left
}
