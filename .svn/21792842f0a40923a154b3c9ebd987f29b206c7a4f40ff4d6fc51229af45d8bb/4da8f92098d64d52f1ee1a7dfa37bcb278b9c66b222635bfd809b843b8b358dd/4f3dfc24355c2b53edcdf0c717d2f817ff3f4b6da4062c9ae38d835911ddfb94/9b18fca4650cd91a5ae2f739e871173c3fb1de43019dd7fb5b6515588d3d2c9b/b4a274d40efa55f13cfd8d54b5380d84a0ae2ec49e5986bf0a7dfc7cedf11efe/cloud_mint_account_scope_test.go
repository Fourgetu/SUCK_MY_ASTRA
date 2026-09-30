package main

import "testing"

// TestCloudMintAccountScope 锁定账号白名单语义：
// 名单为空表示不限制（向后兼容），名单非空时只有名单内账号可以走云端打票。
func TestCloudMintAccountScope(t *testing.T) {
	open := defaultCloudMintConfig()
	if !open.accountInScope("codex-anything.json") {
		t.Fatal("empty accounts list must not restrict cloud mint")
	}
	if !open.accountInScope("") {
		t.Fatal("empty accounts list must allow an empty auth id")
	}

	scoped := defaultCloudMintConfig()
	scoped.Accounts = []string{"codex-keeper.json", "  codex-second.json  "}
	for _, allowed := range []string{"codex-keeper.json", "codex-second.json", "CODEX-KEEPER.JSON", " codex-keeper.json "} {
		if !scoped.accountInScope(allowed) {
			t.Fatalf("listed account %q must be allowed", allowed)
		}
	}
	for _, blocked := range []string{"codex-other.json", "", "codex-keeper.json.bak", "keeper.json"} {
		if scoped.accountInScope(blocked) {
			t.Fatalf("unlisted account %q must be passed through without cloud mint", blocked)
		}
	}
}
