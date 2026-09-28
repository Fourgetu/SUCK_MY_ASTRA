package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const cloudResponseLimit = 256 << 10

type cloudMintTicket struct {
	TurnState   string    `json:"turn_state"`
	TicketLen   int       `json:"ticket_len"`
	ServedModel string    `json:"served_model"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type cloudMintResult struct {
	Transport  string                     `json:"transport"`
	Gateway    string                     `json:"gateway"`
	Cookies    map[string]string          `json:"cookies"`
	ExpiresAt  time.Time                  `json:"expires_at"`
	Tickets    map[string]cloudMintTicket `json:"tickets"`
	AttemptLog []cloudAttemptLog          `json:"attempt_log"`
	Error      struct {
		AttemptLog []cloudAttemptLog `json:"attempt_log"`
	} `json:"error"`
}

type cloudMintEntry struct {
	Ticket              string
	Cookies             map[string]string
	Gateway, Model      string
	IssuedAt, ExpiresAt time.Time
}

func requestCloudMint(ctx context.Context, work cloudMintWork) (cloudMintEntry, error) {
	cfg, creds, model, key := work.cfg, work.creds, work.model, work.key
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, nil)
	if err != nil {
		return cloudMintEntry{}, errors.New("invalid cloud endpoint")
	}
	req.Header.Set("X-Relay-Key", key)
	req.Header.Set("X-Relay-Mint", cfg.Gateway)

	req.Header.Set("X-Mint-Gateway", cfg.Gateway)
	req.Header.Set("X-Mint-Model", model)
	req.Header.Set("X-Mint-Transport", cfg.Transport)
	req.Header.Set("X-Mint-Len", strconv.Itoa(cfg.TicketLength))
	req.Header.Set("X-Mint-TTL", strconv.Itoa(cfg.TTLSeconds))
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", creds.AccountID)
	}
	if work.seedCookie != "" {
		req.Header.Set("Cookie", work.seedCookie)
	}
	transport, err := newCloudMintTransport(work.proxyURL)
	if err != nil {
		return cloudMintEntry{}, err
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return cloudMintEntry{}, errors.New("cloud mint unavailable or timed out")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, cloudResponseLimit+1))
	if err != nil || len(raw) > cloudResponseLimit {
		return cloudMintEntry{}, errors.New("cloud mint response invalid or too large")
	}
	var result cloudMintResult
	if json.Unmarshal(raw, &result) != nil {
		return cloudMintEntry{}, errors.New("cloud mint response is not JSON")
	}
	logCloudAttempts(result.AttemptLog)
	logCloudAttempts(result.Error.AttemptLog)
	if res.StatusCode != http.StatusOK {
		return cloudMintEntry{}, errors.New("cloud mint rejected")
	}
	return validateCloudMint(result, cfg, model, time.Now())
}

func cloudIssuedAt(ticket string) time.Time {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(ticket, "="))
	if err != nil || len(raw) < 9 || raw[0] != 0x80 {
		return time.Time{}
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds > 1<<40 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

func validateCloudMint(r cloudMintResult, cfg cloudMintConfig, model string, now time.Time) (cloudMintEntry, error) {
	t, ok := r.Tickets[model]
	if !ok || r.Transport != cfg.Transport || t.ServedModel != model || len(t.TurnState) != cfg.TicketLength || t.TicketLen != len(t.TurnState) {
		return cloudMintEntry{}, errors.New("cloud ticket model, transport or length mismatch")
	}
	issued := cloudIssuedAt(t.TurnState)
	if issued.IsZero() || issued.After(now.Add(30*time.Second)) {
		return cloudMintEntry{}, errors.New("invalid ticket issue time")
	}

	if cfg.Gateway != "any" && (r.Gateway != cfg.Gateway || cloudCookieGateway(r.Cookies) != cfg.Gateway) {
		return cloudMintEntry{}, errors.New("cloud target gateway mismatch")
	}
	gateway := r.Gateway
	if gateway == "" {
		gateway = cloudCookieGateway(r.Cookies)
	}
	cookies := map[string]string{}
	for _, name := range []string{"__cflb", "__oailb"} {
		value := r.Cookies[name]
		if value == "" || !cookieValueSafe(value) {
			return cloudMintEntry{}, errors.New("cloud cookie pair missing or invalid")
		}
		cookies[name] = value
	}
	expiry := issued.Add(time.Duration(cfg.TTLSeconds) * time.Second)
	for _, deadline := range []time.Time{t.ExpiresAt, r.ExpiresAt, jwtExpiresAt(cookies["__oailb"])} {
		if !deadline.After(now) {
			return cloudMintEntry{}, errors.New("cloud ticket or pair expired")
		}
		if deadline.Before(expiry) {
			expiry = deadline
		}
	}
	if !expiry.After(now.Add(time.Second)) {
		return cloudMintEntry{}, errors.New("cloud ticket lifetime too short")
	}
	return cloudMintEntry{Ticket: t.TurnState, Cookies: cookies, Gateway: gateway, Model: model, IssuedAt: issued, ExpiresAt: expiry}, nil
}

var cloudUnifiedPattern = regexp.MustCompile(`(?i)unified[-_.]?(\d+)`)

func cloudCookieGateway(cookies map[string]string) string {
	parts := strings.Split(cookies["__oailb"], ".")
	if len(parts) > 1 {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err == nil {
			if match := cloudUnifiedPattern.FindStringSubmatch(string(raw)); len(match) > 1 {
				return "unified-" + match[1]
			}
		}
	}
	label := gatewayLabel(cookies)
	if cloudGatewayPattern.MatchString(label) {
		return label
	}
	return ""
}
