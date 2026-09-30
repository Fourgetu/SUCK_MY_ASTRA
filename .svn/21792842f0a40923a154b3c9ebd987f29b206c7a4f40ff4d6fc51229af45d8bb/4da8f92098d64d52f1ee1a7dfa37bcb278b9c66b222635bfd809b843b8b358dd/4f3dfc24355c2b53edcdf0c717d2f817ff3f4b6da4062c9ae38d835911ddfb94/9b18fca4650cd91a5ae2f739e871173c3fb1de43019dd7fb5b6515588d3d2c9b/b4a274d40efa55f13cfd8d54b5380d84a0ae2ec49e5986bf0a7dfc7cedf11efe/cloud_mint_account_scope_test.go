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

// TestCloudMintConfigEqual 锁定 equal 语义：Accounts 是切片，结构体不能再直接用 != 比较，
// main.go 检测配置变化走的是这个方法。
func TestCloudMintConfigEqual(t *testing.T) {
	base := defaultCloudMintConfig()
	if !base.equal(defaultCloudMintConfig()) {
		t.Fatal("identical configs must compare equal")
	}
	withAccounts := defaultCloudMintConfig()
	withAccounts.Accounts = []string{"codex-keeper.json"}
	if base.equal(withAccounts) {
		t.Fatal("a changed account allowlist must count as a config change")
	}
	sameAccounts := defaultCloudMintConfig()
	sameAccounts.Accounts = []string{"codex-keeper.json"}
	if !withAccounts.equal(sameAccounts) {
		t.Fatal("equal allowlists must compare equal")
	}
	sameAccounts.Accounts = []string{"codex-keeper.json", "codex-second.json"}
	if withAccounts.equal(sameAccounts) {
		t.Fatal("a longer allowlist must count as a config change")
	}
	otherGateway := defaultCloudMintConfig()
	otherGateway.Gateway = "unified-88"
	if base.equal(otherGateway) {
		t.Fatal("a changed gateway must count as a config change")
	}
}
