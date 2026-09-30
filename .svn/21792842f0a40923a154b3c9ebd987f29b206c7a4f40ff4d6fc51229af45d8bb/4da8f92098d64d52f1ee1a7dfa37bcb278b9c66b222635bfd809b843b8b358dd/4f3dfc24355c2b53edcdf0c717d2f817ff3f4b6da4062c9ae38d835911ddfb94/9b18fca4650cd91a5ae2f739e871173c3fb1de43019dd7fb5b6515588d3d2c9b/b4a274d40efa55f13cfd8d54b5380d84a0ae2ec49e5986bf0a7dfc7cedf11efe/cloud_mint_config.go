package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type cloudMintConfig struct {
	Enabled      bool   `yaml:"enabled"`
	URL          string `yaml:"url"`
	// URLs 是多个云函数地址（可选）：某个地址铸不出满血票时短期跳过它、换下一个地址。
	// 填了 urls 就以 urls 为准，url 仅作兜底。
	URLs []string `yaml:"urls"`
	ProxyURL     string `yaml:"proxy_url"`
	ProxyEnv     string `yaml:"proxy_env"`
	KeyEnv       string `yaml:"key_env"`
	Transport    string `yaml:"transport"`
	Gateway      string `yaml:"gateway"`
	TicketLength int    `yaml:"ticket_length"`
	TTLSeconds   int    `yaml:"ttl_seconds"`
	WaitMS       int    `yaml:"wait_ms"`
	TimeoutMS    int    `yaml:"timeout_ms"`

	MintModel string `yaml:"mint_model"`

	FailClosed bool `yaml:"fail_closed"`

	// Accounts 限定哪些账号可以走云端打票（按账号文件名/ID，忽略大小写）；
	// 留空表示不限制。名单外的账号直接放行，不会向云函数发送凭据。
	Accounts []string `yaml:"accounts"`
}

func defaultCloudMintConfig() cloudMintConfig {

	return cloudMintConfig{KeyEnv: "CPA_RELAY_KEY", Transport: "sse", Gateway: "any",
		TicketLength: 780, TTLSeconds: 240, WaitMS: 2000, TimeoutMS: 90000, FailClosed: true,
		MintModel: "gpt-6-astra"}
}

func (c cloudMintConfig) mintModel() string {
	if m := strings.TrimSpace(c.MintModel); m != "" {
		return m
	}
	return "gpt-6-astra"
}

// accountInScope 报告该账号是否允许走云端打票。名单为空表示不限制（向后兼容）；
// 名单非空时，不在名单里的账号一律放行，不把凭据发往云函数。
func (c cloudMintConfig) accountInScope(authID string) bool {
	if len(c.Accounts) == 0 {
		return true
	}
	id := normaliseAccountID(authID)
	if id == "" {
		return false
	}
	for _, account := range c.Accounts {
		if normaliseAccountID(account) == id {
			return true
		}
	}
	return false
}

// equal 逐字段比较云打票配置。Accounts 是切片，结构体因此不能再直接用 != 比较，
// main.go 里检测配置变化时必须走这里。
func (c cloudMintConfig) equal(other cloudMintConfig) bool {
	if c.Enabled != other.Enabled ||
		c.URL != other.URL ||
		c.ProxyURL != other.ProxyURL ||
		c.ProxyEnv != other.ProxyEnv ||
		c.KeyEnv != other.KeyEnv ||
		c.Transport != other.Transport ||
		c.Gateway != other.Gateway ||
		c.TicketLength != other.TicketLength ||
		c.TTLSeconds != other.TTLSeconds ||
		c.WaitMS != other.WaitMS ||
		c.TimeoutMS != other.TimeoutMS ||
		c.MintModel != other.MintModel ||
		c.FailClosed != other.FailClosed ||
		!sameStringList(c.Accounts, other.Accounts) ||
		!sameStringList(c.URLs, other.URLs) {
		return false
	}
	return true
}

var cloudGatewayPattern = regexp.MustCompile(`^unified-[0-9]+$`)
var cloudNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,96}$`)

// cloudAccountIDPattern 匹配 CPA 的账号标识/文件名，例如 codex-name@example.com.json。
// 不能复用 cloudNamePattern：账号名里带 @ 和 +，那套模式只允许 [A-Za-z0-9_.-]。
var cloudAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9@._+:-]{1,160}$`)

// normaliseAccountID 统一账号标识：去掉目录前缀、结尾的 .json 与大小写差异。
// 面板里显示的账号名与宿主上报的 ID 可能一个带扩展名、一个不带，两边都按这套折叠。
func normaliseAccountID(raw string) string {
	id := strings.TrimSpace(raw)
	if index := strings.LastIndexAny(id, "/\\"); index >= 0 {
		id = id[index+1:]
	}
	// 先折叠大小写再去扩展名：.JSON 与 .json 要当成同一个账号。
	id = strings.ToLower(id)
	id = strings.TrimSuffix(id, ".json")
	return strings.TrimSpace(id)
}

// endpoints 返回可用的云函数地址：填了 urls 就用 urls，否则用 url；去空白、去重、保序。
func (c cloudMintConfig) endpoints() []string {
	raw := make([]string, 0, len(c.URLs)+1)
	for _, item := range c.URLs {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			raw = append(raw, trimmed)
		}
	}
	if len(raw) == 0 {
		if trimmed := strings.TrimSpace(c.URL); trimmed != "" {
			raw = append(raw, trimmed)
		}
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// validateEndpoint 校验单个云函数地址：必须 HTTPS（回环地址可 HTTP），且不带凭据/query/fragment。
func (c cloudMintConfig) validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("cloud_mint url must be an HTTPS URL without credentials/query/fragment")
	}
	ip := net.ParseIP(u.Hostname())
	local := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("cloud_mint url requires HTTPS (HTTP only on loopback)")
	}
	return nil
}

// sameStringList 逐项比较字符串切片：结构体要逐字段比较，切片不能直接用 !=。
func sameStringList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (c cloudMintConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	endpoints := c.endpoints()
	if len(endpoints) == 0 {
		return errors.New("cloud_mint needs cloud_mint.url or cloud_mint.urls")
	}
	for _, endpoint := range endpoints {
		if err := c.validateEndpoint(endpoint); err != nil {
			return err
		}
	}
	if err := c.validateProxy(); err != nil {
		return err
	}
	if !cloudNamePattern.MatchString(c.KeyEnv) || (c.Gateway != "any" && !cloudGatewayPattern.MatchString(c.Gateway)) {
		return errors.New("invalid cloud_mint key_env or gateway")
	}
	if !cloudNamePattern.MatchString(c.mintModel()) {
		return errors.New("invalid cloud_mint mint_model")
	}
	if c.Transport != "sse" && c.Transport != "websocket" {
		return errors.New("cloud_mint.transport must be sse or websocket")
	}
	if c.TicketLength < 1 || c.TicketLength > 4096 || c.TTLSeconds < 1 || c.TTLSeconds > 3600 {
		return errors.New("invalid cloud_mint ticket_length or ttl_seconds")
	}
	if c.WaitMS < 1 || c.WaitMS > 10000 || c.TimeoutMS < c.WaitMS || c.TimeoutMS > 180000 {
		return errors.New("invalid cloud_mint wait_ms or timeout_ms")
	}
	for _, account := range c.Accounts {
		if !cloudAccountIDPattern.MatchString(strings.TrimSpace(account)) {
			return errors.New("invalid cloud_mint accounts entry")
		}
	}
	return nil
}

type cloudMintCredentials struct{ AuthID, AccessToken, AccountID string }

var cloudHostCall = hostCallJSON
var cloudCredentialResolver = resolveCloudCredentials
var errCloudNotCodex = errors.New("not a selected Codex credential")

func resolveCloudCredentials(req pluginapi.RequestInterceptRequest) (cloudMintCredentials, error) {
	id := metadataString(req.Metadata, selectedAuthMetadataKey)
	if id == "" {
		return cloudMintCredentials{}, errCloudNotCodex
	}
	index := metadataString(req.Metadata, selectedAuthIndexMetadataKey)
	if index == "" {
		var list struct {
			Files []pluginapi.HostAuthFileEntry `json:"files"`
		}
		if cloudHostCall("host.auth.list", map[string]any{}, &list) != nil {
			return cloudMintCredentials{}, errors.New("credential lookup unavailable")
		}
		for _, file := range list.Files {
			if file.ID == id || file.Name == id {
				index = file.AuthIndex
				break
			}
		}
	}
	if index == "" {
		return cloudMintCredentials{}, errors.New("selected credential index unavailable")
	}
	var runtime pluginapi.HostAuthGetRuntimeResponse
	lookup := pluginapi.HostAuthGetRequest{AuthIndex: index}
	if cloudHostCall("host.auth.get_runtime", lookup, &runtime) != nil {
		return cloudMintCredentials{}, errors.New("credential runtime unavailable")
	}
	if runtime.Auth.ID != id && runtime.Auth.Name != id {
		return cloudMintCredentials{}, errors.New("selected credential mismatch")
	}
	provider := strings.ToLower(strings.TrimSpace(runtime.Auth.Provider))
	kind := strings.ToLower(strings.TrimSpace(runtime.Auth.Type))

	if (provider != "" && provider != "codex") || (provider == "" && kind != "codex") {
		return cloudMintCredentials{}, errCloudNotCodex
	}

	if runtime.Auth.Disabled || strings.EqualFold(strings.TrimSpace(runtime.Auth.Status), "disabled") {
		return cloudMintCredentials{}, errors.New("selected credential disabled")
	}
	return readCloudCredentialFile(lookup, runtime.Auth, id)
}

func readCloudCredentialFile(lookup pluginapi.HostAuthGetRequest, runtime pluginapi.HostAuthFileEntry, id string) (cloudMintCredentials, error) {
	var file pluginapi.HostAuthGetResponse
	if cloudHostCall("host.auth.get", lookup, &file) != nil {
		return cloudMintCredentials{}, errors.New("selected credential file unavailable")
	}
	if file.AuthIndex != lookup.AuthIndex || (file.Name != runtime.Name && file.Name != id) {
		return cloudMintCredentials{}, errors.New("credential file mismatch")
	}
	var token struct {
		Type        string `json:"type"`
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if json.Unmarshal(file.JSON, &token) != nil || token.Type != "codex" || strings.TrimSpace(token.AccessToken) == "" {
		return cloudMintCredentials{}, errors.New("invalid Codex credential")
	}
	return cloudMintCredentials{AuthID: id, AccessToken: token.AccessToken, AccountID: token.AccountID}, nil
}
