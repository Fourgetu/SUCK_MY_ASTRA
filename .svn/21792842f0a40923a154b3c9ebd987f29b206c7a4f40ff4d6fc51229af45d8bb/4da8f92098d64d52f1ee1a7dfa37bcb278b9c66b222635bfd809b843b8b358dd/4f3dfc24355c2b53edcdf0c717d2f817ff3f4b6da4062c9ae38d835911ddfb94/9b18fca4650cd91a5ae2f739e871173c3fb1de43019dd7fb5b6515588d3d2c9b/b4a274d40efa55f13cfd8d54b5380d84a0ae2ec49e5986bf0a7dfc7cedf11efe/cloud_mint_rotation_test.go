package main

import (
	"testing"
	"time"
)

func resetCloudEndpointRotation() {
	cloudEndpointRotation.mu.Lock()
	cloudEndpointRotation.next = 0
	cloudEndpointRotation.cooldown = make(map[string]time.Time)
	cloudEndpointRotation.mu.Unlock()
}

// 填了 urls 就以 urls 为准（去空白、去重、保序）；没填 urls 才退回 url。
func TestCloudMintEndpointsPrefersURLsAndFallsBack(t *testing.T) {
	cfg := defaultCloudMintConfig()
	cfg.URL = "https://one.example/"
	if got := cfg.endpoints(); !sameStringList(got, []string{"https://one.example/"}) {
		t.Fatalf("url fallback = %v", got)
	}

	cfg.URLs = []string{" https://two.example/ ", "", "https://one.example/", "https://two.example/"}
	if got := cfg.endpoints(); !sameStringList(got, []string{"https://two.example/", "https://one.example/"}) {
		t.Fatalf("urls precedence/dedup = %v", got)
	}

	cfg.URLs = nil
	cfg.URL = "   "
	if got := cfg.endpoints(); len(got) != 0 {
		t.Fatalf("blank config must have no endpoints, got %v", got)
	}
}

// 失败的地址进入冷却并被跳过，下一次挑到后面的地址；冷却过期后可以再用。
func TestCloudMintEndpointRotationSkipsCoolingEndpoint(t *testing.T) {
	resetCloudEndpointRotation()
	now := time.Now()
	endpoints := []string{"https://a.example/", "https://b.example/", "https://c.example/"}

	if got := pickCloudEndpoint(endpoints, now); got != endpoints[0] {
		t.Fatalf("first pick = %q", got)
	}
	markCloudEndpointFailed(endpoints, endpoints[0], now, "cloud mint rejected")
	if got := pickCloudEndpoint(endpoints, now); got != endpoints[1] {
		t.Fatalf("pick after failure = %q, want %q", got, endpoints[1])
	}
	markCloudEndpointOK(endpoints[1])
	if got := pickCloudEndpoint(endpoints, now); got != endpoints[1] {
		t.Fatalf("successful endpoint must stay selected, got %q", got)
	}

	markCloudEndpointFailed(endpoints, endpoints[1], now, "")
	markCloudEndpointFailed(endpoints, endpoints[2], now, "")
	if got := pickCloudEndpoint(endpoints, now); got == "" {
		t.Fatal("all endpoints cooling must still yield one")
	}

	later := now.Add(cloudEndpointCooldown + time.Second)
	if got := pickCloudEndpoint(endpoints, later); got == "" {
		t.Fatal("expired cooldown must be usable again")
	}

	if got := pickCloudEndpoint(nil, now); got != "" {
		t.Fatalf("no endpoints must yield empty string, got %q", got)
	}
}

// urls 里每个地址都要过校验，一个非法就整体拒绝；enabled 但一个地址都没有也拒绝。
func TestCloudMintValidateChecksEveryEndpoint(t *testing.T) {
	cfg := defaultCloudMintConfig()
	cfg.Enabled = true
	cfg.URL = ""

	cfg.URLs = []string{"https://ok.example/", "http://plain.example/"}
	if err := cfg.validate(); err == nil {
		t.Fatal("plain HTTP endpoint must be rejected")
	}

	cfg.URLs = []string{"https://ok.example/", "https://also.example/"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid endpoints rejected: %v", err)
	}

	cfg.URLs = nil
	if err := cfg.validate(); err == nil {
		t.Fatal("enabled config without any endpoint must be rejected")
	}
}

// 日志里只出现主机名，不铺整条地址。
func TestCloudEndpointLabelStripsSchemeAndPath(t *testing.T) {
	if got := cloudEndpointLabel("https://abc123.execute-api.ap-southeast-1.amazonaws.com/"); got != "abc123.execute-api.ap-southeast-1.amazonaws.com" {
		t.Fatalf("label = %q", got)
	}
	if got := cloudEndpointLabel("  "); got != "未配置地址" {
		t.Fatalf("blank label = %q", got)
	}
}
