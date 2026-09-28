package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed cloud_mint_ui.html
var dashboardHTML []byte

const (
	routeStatus       = "/codex-turn-state/status"
	routeBucketsClear = "/codex-turn-state/buckets/clear"

	routeSelftest = "/codex-turn-state/selftest"

	routeDashboard = "/dashboard"

	routeStatusResource = "/status"

	routeConfig = "/codex-turn-state/config"

	routeOpsDryRun   = "/ops/dry-run"
	routeOpsRole     = "/ops/role"
	routeOpsClear    = "/ops/clear"
	routeOpsSelftest = "/ops/selftest"

	routeOpsScope = "/ops/scope"

	routeOpsProbeStart  = "/ops/probe/start"
	routeOpsProbeCancel = "/ops/probe/cancel"

	routeOpsProxyCheck = "/ops/proxy-check"

	routeOpsChoices = "/ops/choices"
)

func managementRegister(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(raw) > 0 {

		_ = json.Unmarshal(raw, &req)
	}

	setCloudPluginID(req.ResourceBasePath)
	response := pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: routeStatus},
			{Method: http.MethodGet, Path: routeCloudDashboardStatus},
			{Method: http.MethodPost, Path: routeBucketsClear},
			{Method: http.MethodPost, Path: routeSelftest},

			{Method: http.MethodGet, Path: routeConfig},
		},
		Resources: []pluginapi.ResourceRoute{
			{

				Path:        routeDashboard,
				Menu:        "云端打票",
				Description: "云端打票：真实任务、脱敏流水与打票设置",
			},
			{

				Path:        routeStatusResource,
				Description: "只读状态（无需鉴权），供看板拉取",
			},

			{Path: routeOpsDryRun, Description: "翻转 dry_run（无需鉴权，需 confirm=1）"},
			{Path: routeOpsRole, Description: "切换 role（无需鉴权，需 confirm=1）"},
			{Path: routeOpsClear, Description: "清空桶（无需鉴权，需 confirm=1）"},
			{Path: routeOpsSelftest, Description: "连通性自检（无需鉴权，需 confirm=1，烧额度）"},

			{Path: routeOpsScope, Description: "保存探测范围（无需鉴权，需 confirm=1）"},

			{Path: routeOpsProbeStart, Description: "启动探测运行（无需鉴权，需 confirm=1，烧额度）"},
			{Path: routeOpsProbeCancel, Description: "取消探测运行（无需鉴权，需 confirm=1）"},

			{Path: routeOpsProxyCheck, Description: "批量测代理到 OpenAI 的连通性（无需鉴权，需 confirm=1，不烧额度）"},

			{Path: routeOpsChoices, Description: "可选账号/模型清单（无需鉴权，只读）"},
		},
	}
	for i := range response.Routes {
		response.Routes[i].Path = cloudRegisteredPath(response.Routes[i].Path)
	}
	return okEnvelope(response)
}

func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return okEnvelope(managementError(http.StatusBadRequest, "could not decode the management request"))
	}

	path := cloudCanonicalPath(strings.TrimRight(strings.TrimSpace(req.Path), "/"))
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch {
	case hasRouteSuffix(path, routeCloudDashboardStatus):
		if isResourcePath(path) {
			return okEnvelope(managementError(http.StatusForbidden, "cloud status requires management authentication"))
		}
		if method != http.MethodGet {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "cloud status is GET only"))
		}
		return okEnvelope(handleCloudDashboardStatus())
	case hasRouteSuffix(path, routeStatus):
		if method != http.MethodGet && method != "" {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "status is a GET route"))
		}
		return okEnvelope(handleStatus())
	case hasRouteSuffix(path, routeConfig):
		if method != http.MethodGet && method != "" {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "config is a GET route"))
		}

		if isResourcePath(path) {
			return okEnvelope(managementError(http.StatusNotFound, "no such codex-turn-state route: "+req.Path))
		}
		return okEnvelope(handleConfig())
	case hasRouteSuffix(path, routeBucketsClear):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "buckets/clear is a POST route"))
		}
		return okEnvelope(handleBucketsClear(req.Body))
	case hasRouteSuffix(path, routeSelftest):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "selftest is a POST route"))
		}
		return okEnvelope(handleSelftest(req.Body))
	case hasRouteSuffix(path, routeOpsChoices):

		if method != http.MethodGet && method != "" {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "choices is a GET route"))
		}
		return okEnvelope(handleChoicesResource())
	case hasRouteSuffix(path, routeOpsDryRun),
		hasRouteSuffix(path, routeOpsRole),
		hasRouteSuffix(path, routeOpsClear),
		hasRouteSuffix(path, routeOpsSelftest),
		hasRouteSuffix(path, routeOpsScope),
		hasRouteSuffix(path, routeOpsProbeStart),
		hasRouteSuffix(path, routeOpsProbeCancel),
		hasRouteSuffix(path, routeOpsProxyCheck):

		return okEnvelope(handleOpsResource(path, method, req.Query))
	case isDashboardPath(path):
		return okEnvelope(handleDashboard())
	default:
		return okEnvelope(managementError(http.StatusNotFound, "no such codex-turn-state route: "+req.Path))
	}
}

func hasRouteSuffix(path, suffix string) bool {
	return strings.EqualFold(path, suffix) || strings.HasSuffix(strings.ToLower(path), strings.ToLower(suffix))
}

func isDashboardPath(path string) bool {
	return isResourcePath(path)
}

func isResourcePath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/resource/plugins/")
}

func handleDashboard() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type": []string{"text/html; charset=utf-8"},

			"Cache-Control": []string{"no-store"},

			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: []byte(strings.Replace(string(dashboardHTML), `name="cpa-plugin-id" content="codex-turn-state"`, `name="cpa-plugin-id" content="`+currentCloudPluginID()+`"`, 1)),
	}
}

func handleOpsResource(path, method string, q url.Values) pluginapi.ManagementResponse {
	if method != http.MethodGet && method != "" {
		return managementError(http.StatusMethodNotAllowed, "keyless action routes are GET-only")
	}
	if strings.TrimSpace(q.Get("confirm")) != "1" {
		return managementError(http.StatusBadRequest,
			"this action changes state or spends quota; it requires confirm=1 so it cannot fire from a bare navigation or a prefetch")
	}
	switch {
	case hasRouteSuffix(path, routeOpsDryRun):
		return handleDryRunResource(q)
	case hasRouteSuffix(path, routeOpsRole):
		return handleRoleResource(q)
	case hasRouteSuffix(path, routeOpsClear):
		return clearBuckets(clearRequestFromQuery(q))
	case hasRouteSuffix(path, routeOpsSelftest):
		return runSelftest(selftestRequestFromQuery(q))
	case hasRouteSuffix(path, routeOpsScope):
		return handleScopeSave(q)
	case hasRouteSuffix(path, routeOpsProbeStart):
		return handleProbeStartResource()
	case hasRouteSuffix(path, routeOpsProbeCancel):
		return handleProbeCancelResource()
	case hasRouteSuffix(path, routeOpsProxyCheck):
		return runProxyCheck()
	default:
		return managementError(http.StatusNotFound, "no such keyless action route")
	}
}

func handleProbeStartResource() pluginapi.ManagementResponse {
	if errStart := probeRunStart(); errStart != nil {
		status := http.StatusBadRequest
		if probeRunSnapshot().Running {
			status = http.StatusConflict
		}
		return managementError(status, errStart.Error())
	}

	return jsonResponse(http.StatusOK, probeRunSnapshot())
}

type probeCancelResponse struct {
	Cancelled bool          `json:"cancelled"`
	ProbeRun  probeRunState `json:"probe_run"`
}

func handleProbeCancelResource() pluginapi.ManagementResponse {
	cancelled := probeRunCancel()
	return jsonResponse(http.StatusOK, probeCancelResponse{
		Cancelled: cancelled,
		ProbeRun:  probeRunSnapshot(),
	})
}

var knownCodexModels = []string{"gpt-5.5", "gpt-5.6-sol", "gpt-6-astra"}

var choicesFetchTimeout = 5 * time.Second

type choiceAccount struct {
	Name  string `json:"name"`
	Label string `json:"label"`

	Disabled bool `json:"disabled"`

	Selected bool `json:"selected"`
}

type choiceModel struct {
	Name     string `json:"name"`
	Selected bool   `json:"selected"`
}

type choicesResponse struct {
	Accounts []choiceAccount `json:"accounts"`
	Models   []choiceModel   `json:"models"`
	Error    string          `json:"error"`
}

func handleChoicesResource() pluginapi.ManagementResponse {
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	out := choicesResponse{

		Accounts: []choiceAccount{},
		Models:   modelChoices(cfg.Models),
	}

	accounts, errAccounts := choiceAccounts(cfg)
	if errAccounts != nil {
		out.Error = errAccounts.Error()

		log.Printf(logPrefix+"choices: credential list unavailable: %s", out.Error)
		return jsonResponse(http.StatusOK, out)
	}
	out.Accounts = accounts
	return jsonResponse(http.StatusOK, out)
}

func choiceAccounts(cfg pluginConfig) ([]choiceAccount, error) {

	if strings.TrimSpace(cfg.ProbeManagementKey) == "" {
		return nil, fmt.Errorf("probe_management_key is not set, so CPA's credential list cannot be read; " +
			"set it in config.yaml and reload, or keep listing probe_accounts by hand")
	}

	ctx, cancel := context.WithTimeout(context.Background(), choicesFetchTimeout)
	defer cancel()

	files, errList := newProbeClient(cfg).listCodexAuths(ctx)
	if errList != nil {

		return nil, fmt.Errorf("could not read CPA's credential list: %s", probeRedact(errList.Error()))
	}

	selected := make(map[string]bool, len(cfg.ProbeAccounts))
	for _, name := range cfg.ProbeAccounts {
		selected[strings.TrimSpace(name)] = true
	}

	out := make([]choiceAccount, 0, len(files))
	for _, file := range files {
		out = append(out, choiceAccount{
			Name:     file.Name,
			Label:    maskAuthLabel(file.Name),
			Disabled: file.Disabled,
			Selected: selected[file.Name],
		})
	}
	return out, nil
}

func modelChoices(configured []string) []choiceModel {
	selected := make(map[string]bool, len(configured))
	union := make(map[string]bool, len(configured)+len(knownCodexModels))
	for _, raw := range configured {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		selected[name] = true
		union[name] = true
	}
	for _, name := range knownCodexModels {
		union[name] = true
	}

	names := make([]string, 0, len(union))
	for name := range union {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]choiceModel, 0, len(names))
	for _, name := range names {
		out = append(out, choiceModel{Name: name, Selected: selected[name]})
	}
	return out
}

func maskAuthLabel(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {

		return ""
	}

	body := trimmed
	if lower := strings.ToLower(body); strings.HasSuffix(lower, ".json") {
		body = body[:len(body)-len(".json")]
	}
	if lower := strings.ToLower(body); strings.HasPrefix(lower, "codex-") {
		body = body[len("codex-"):]
	}

	kept := make([]string, 0, 4)
	for _, part := range strings.Split(body, "-") {
		part = strings.TrimSpace(part)

		if part == "" || strings.Contains(part, "@") {
			continue
		}
		kept = append(kept, part)
	}

	switch len(kept) {
	case 0:

		return "…"
	case 1:

		return kept[0]
	default:

		return kept[0] + "…" + kept[len(kept)-1]
	}
}

func clearRequestFromQuery(q url.Values) clearRequest {
	return clearRequest{
		AuthID: strings.TrimSpace(q.Get("auth_id")),
		Model:  strings.TrimSpace(q.Get("model")),
		All:    queryTrue(q.Get("all")),
	}
}

func selftestRequestFromQuery(q url.Values) selftestRequest {
	return selftestRequest{
		Model:  strings.TrimSpace(q.Get("model")),
		AuthID: strings.TrimSpace(q.Get("auth_id")),
	}
}

func handleDryRunResource(q url.Values) pluginapi.ManagementResponse {
	value, ok := parseBoolParam(q.Get("value"))
	if !ok {
		return managementError(http.StatusBadRequest, `"value" must be one of on/off/true/false/1/0`)
	}
	state.mu.Lock()
	cfg := state.config
	cfg.DryRun = value
	swapConfigLocked(cfg)
	dir := cfg.StoreDir
	role := cfg.Role
	state.mu.Unlock()

	persisted := true
	warning := ""
	if err := writeRuntimeOverride(dir, role, value); err != nil {

		persisted = false
		warning = "restart will revert: " + err.Error()
		log.Printf(logPrefix+"dry_run set to %t but persisting the override failed: %v", value, err)
	} else {
		log.Printf(logPrefix+"dry_run set to %t via dashboard (keyless)", value)
	}
	return jsonResponse(http.StatusOK, map[string]any{"dry_run": value, "persisted": persisted, "warning": warning})
}

func handleRoleResource(q url.Values) pluginapi.ManagementResponse {
	role := strings.ToLower(strings.TrimSpace(q.Get("value")))
	if role != roleProbe && role != roleBusiness {
		return managementError(http.StatusBadRequest,
			fmt.Sprintf(`"value" must be %q or %q`, roleProbe, roleBusiness))
	}
	state.mu.Lock()
	cfg := state.config
	if role == roleProbe && strings.TrimSpace(cfg.StoreDir) == "" {
		state.mu.Unlock()
		return managementError(http.StatusConflict, "role probe requires store_dir, which is not configured")
	}
	cfg.Role = role
	swapConfigLocked(cfg)
	dir := cfg.StoreDir
	dryRun := cfg.DryRun
	state.mu.Unlock()

	persisted := true
	warning := ""
	if err := writeRuntimeOverride(dir, role, dryRun); err != nil {
		persisted = false
		warning = "restart will revert: " + err.Error()
		log.Printf(logPrefix+"role set to %s but persisting the override failed: %v", role, err)
	} else {
		log.Printf(logPrefix+"role set to %s via dashboard (keyless)", role)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"role":      role,
		"persisted": persisted,
		"warning":   warning,
		"note":      "若切换后发现钩子没被重新协商（probe 采不到 / business 不替换），重启一次 CPA。",
	})
}

func queryTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func parseBoolParam(v string) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true, true
	case "0", "false", "off", "no":
		return false, true
	}
	return false, false
}

type statusBucket struct {
	AuthID string `json:"auth_id"`
	Model  string `json:"model"`
	Ready  bool   `json:"ready"`

	Len int `json:"len"`

	Enabled bool `json:"enabled"`

	RouteCookiesSecondsLeft int64 `json:"route_cookies_seconds_left,omitempty"`

	Observed *observationSummary `json:"observed,omitempty"`
}

type statusResponse struct {
	Role           string         `json:"role"`
	DryRun         bool           `json:"dry_run"`
	TTLSeconds     int            `json:"ttl_seconds"`
	TemplateLength int            `json:"template_length"`
	ReplaceLength  int            `json:"replace_length"`
	StoreDir       string         `json:"store_dir"`
	Models         []string       `json:"models"`
	Buckets        []statusBucket `json:"buckets"`
	TargetsTotal   int            `json:"targets_total"`
	TargetsReady   int            `json:"targets_ready"`

	AccountsSource string           `json:"accounts_source"`
	AccountsError  string           `json:"accounts_error,omitempty"`
	Counters       decisionCounters `json:"counters"`
	CountersSince  string           `json:"counters_since"`
	GeneratedAt    string           `json:"generated_at"`
	StoreError     string           `json:"store_error,omitempty"`

	ProbeAccounts   []string `json:"probe_accounts"`
	ProbeProxyCount int      `json:"probe_proxy_count"`

	ProbeProxies []string `json:"probe_proxies"`

	ProbeProxyRotatingCount int      `json:"probe_proxy_rotating_count"`
	ProbeProxiesRotating    []string `json:"probe_proxies_rotating"`

	ConfigErrors []string `json:"config_errors,omitempty"`

	ProbeRun probeRunState `json:"probe_run"`

	ObservationsSince string `json:"observations_since,omitempty"`

	ObservationFeed []observationEvent `json:"observation_feed"`
}

func handleStatus() pluginapi.ManagementResponse {
	now := time.Now()

	state.mu.Lock()
	cfg := state.config
	counts := state.counts
	countsAt := state.countsAt
	configErrors := append([]string(nil), state.configErrors...)
	state.mu.Unlock()

	out := statusResponse{
		Role:           cfg.Role,
		DryRun:         cfg.DryRun,
		TTLSeconds:     cfg.TTLSeconds,
		TemplateLength: cfg.TemplateLength,
		ReplaceLength:  cfg.ReplaceLength,
		StoreDir:       cfg.StoreDir,
		Models:         append([]string(nil), cfg.Models...),
		Counters:       counts,
		CountersSince:  countsAt.UTC().Format(time.RFC3339),
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		Buckets:        []statusBucket{},
		ProbeAccounts:  append([]string(nil), cfg.ProbeAccounts...),

		ProbeProxyCount: len(cfg.ProbeProxies),
		ProbeProxies:    append([]string(nil), cfg.ProbeProxies...),

		ProbeProxyRotatingCount: len(cfg.ProbeProxiesRotating),
		ProbeProxiesRotating:    append([]string(nil), cfg.ProbeProxiesRotating...),
		ConfigErrors:            configErrors,

		ProbeRun: probeRunSnapshot(),
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	if out.ProbeAccounts == nil {
		out.ProbeAccounts = []string{}
	}

	if out.ProbeProxies == nil {
		out.ProbeProxies = []string{}
	}
	if out.ProbeProxiesRotating == nil {
		out.ProbeProxiesRotating = []string{}
	}

	ttl := cfg.ttl()

	state.mu.Lock()
	poolLeft := state.poolSecondsLeftLocked(now, ttl)
	state.mu.Unlock()

	observed, feed, since := observationsSnapshot()
	out.ObservationsSince = since
	out.ObservationFeed = feed
	if out.ObservationFeed == nil {
		out.ObservationFeed = []observationEvent{}
	}
	byKey := make(map[string]observationSummary, len(observed))
	for _, cell := range observed {
		byKey[bucketKey(cell.AuthID, cell.Model)] = cell.summary(now)
	}

	accounts, accountsSource, errAccounts := statusAccounts(observed)
	out.AccountsSource = accountsSource
	if errAccounts != nil {

		out.AccountsError = errAccounts.Error()
	}

	if len(cfg.ProbeAccounts) > 0 {
		known := make(map[string]bool, len(accounts))
		for _, account := range accounts {
			known[account.AuthID] = account.Enabled
		}
		scoped := make([]codexAuth, 0, len(cfg.ProbeAccounts))
		for _, name := range cfg.ProbeAccounts {
			enabled, seen := known[name]

			scoped = append(scoped, codexAuth{AuthID: name, Enabled: seen && enabled})
		}
		accounts = scoped
	}

	models := out.Models
	if len(models) == 0 {

		modelSeen := make(map[string]bool)
		for _, cell := range observed {
			modelSeen[cell.Model] = true
		}
		for model := range modelSeen {
			models = append(models, model)
		}
		sort.Strings(models)
	}

	enabledByAuth := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		enabledByAuth[account.AuthID] = account.Enabled
	}

	cellFor := func(auth, model string) statusBucket {
		cell := statusBucket{AuthID: auth, Model: model}
		if summary, ok := byKey[bucketKey(auth, model)]; ok {
			copied := summary
			cell.Observed = &copied

			cell.Ready = summary.LastSignedKind != "" && summary.LastSignedKind != observationLimited
			cell.Len = summary.LastLen
		}
		cell.RouteCookiesSecondsLeft = poolLeft
		return cell
	}

	targetsTotal := 0
	targetsReady := 0
	seen := make(map[string]bool, len(accounts)*len(models))
	for _, account := range accounts {
		for _, model := range models {
			cell := cellFor(account.AuthID, model)
			cell.Enabled = account.Enabled
			out.Buckets = append(out.Buckets, cell)
			seen[bucketKey(account.AuthID, model)] = true
			targetsTotal++
			if cell.Ready {
				targetsReady++
			}
		}
	}

	for _, cell := range observed {
		key := bucketKey(cell.AuthID, cell.Model)
		if seen[key] {
			continue
		}
		seen[key] = true
		row := cellFor(cell.AuthID, cell.Model)
		enabled, known := enabledByAuth[cell.AuthID]
		row.Enabled = !known || enabled
		out.Buckets = append(out.Buckets, row)
	}

	sort.Slice(out.Buckets, func(i, j int) bool {
		if out.Buckets[i].AuthID != out.Buckets[j].AuthID {
			return out.Buckets[i].AuthID < out.Buckets[j].AuthID
		}
		return out.Buckets[i].Model < out.Buckets[j].Model
	})

	out.TargetsTotal = targetsTotal
	out.TargetsReady = targetsReady

	return jsonResponse(http.StatusOK, out)
}

type configResponse struct {
	Role           string   `json:"role"`
	StoreDir       string   `json:"store_dir"`
	Models         []string `json:"models"`
	ProbeAccounts  []string `json:"probe_accounts"`
	ProbeProxies   []string `json:"probe_proxies"`
	DryRun         bool     `json:"dry_run"`
	TTLSeconds     int      `json:"ttl_seconds"`
	TemplateLength int      `json:"template_length"`
	ReplaceLength  int      `json:"replace_length"`
	ConfigErrors   []string `json:"config_errors,omitempty"`
}

func handleConfig() pluginapi.ManagementResponse {
	state.mu.Lock()
	cfg := state.config
	configErrors := append([]string(nil), state.configErrors...)
	state.mu.Unlock()

	out := configResponse{
		Role:           cfg.Role,
		StoreDir:       cfg.StoreDir,
		Models:         append([]string(nil), cfg.Models...),
		ProbeAccounts:  append([]string(nil), cfg.ProbeAccounts...),
		ProbeProxies:   append([]string(nil), cfg.ProbeProxies...),
		DryRun:         cfg.DryRun,
		TTLSeconds:     cfg.TTLSeconds,
		TemplateLength: cfg.TemplateLength,
		ReplaceLength:  cfg.ReplaceLength,
		ConfigErrors:   configErrors,
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	if out.ProbeAccounts == nil {
		out.ProbeAccounts = []string{}
	}
	if out.ProbeProxies == nil {
		out.ProbeProxies = []string{}
	}
	return jsonResponse(http.StatusOK, out)
}

type scopeSaveResponse struct {
	Saved              bool     `json:"saved"`
	Fields             []string `json:"fields"`
	ProbeAccounts      []string `json:"probe_accounts"`
	Models             []string `json:"models"`
	ProbeProxyCount    int      `json:"probe_proxy_count"`
	ProbeProxiesMasked []string `json:"probe_proxies_masked"`
	RotatingCount      int      `json:"probe_proxy_rotating_count"`
	RotatingMasked     []string `json:"probe_proxies_rotating_masked"`
	TargetsTotal       int      `json:"targets_total"`
	ConfigErrors       []string `json:"config_errors,omitempty"`
	Note               string   `json:"note"`
}

func handleScopeSave(q url.Values) pluginapi.ManagementResponse {
	requested := map[string]bool{}
	for _, field := range strings.Split(q.Get("fields"), ",") {
		if name := strings.ToLower(strings.TrimSpace(field)); name != "" {
			requested[name] = true
		}
	}
	if len(requested) == 0 {
		return managementError(http.StatusBadRequest,
			`"fields" is required: name which lists to replace, e.g. fields=accounts,models,proxies,rotating. `+
				`Without it an empty query would be indistinguishable from "clear everything".`)
	}
	for name := range requested {
		switch name {
		case "accounts", "models", "proxies", "rotating":
		default:
			return managementError(http.StatusBadRequest,
				"unknown field "+name+"; expected accounts, models, proxies or rotating")
		}
	}

	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	if strings.TrimSpace(cfg.StoreDir) == "" {
		return managementError(http.StatusConflict,
			"store_dir is not configured, so there is nowhere to save the probe scope")
	}

	accounts, models, proxies := cfg.ProbeAccounts, cfg.Models, cfg.ProbeProxies
	rotating := cfg.ProbeProxiesRotating
	if requested["accounts"] {
		accounts = q["account"]
	}
	if requested["models"] {
		models = q["model"]
	}
	if requested["proxies"] {
		proxies = q["proxy"]
	}
	if requested["rotating"] {
		rotating = q["rotating_proxy"]
	}

	accounts, models, proxies, rotating, problems := normaliseProbeScope(accounts, models, proxies, rotating)

	scope := probeScope{
		Accounts:  accounts,
		Models:    models,
		Proxies:   proxies,
		Rotating:  rotating,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if errWrite := writeProbeScope(cfg.StoreDir, scope); errWrite != nil {
		return managementError(http.StatusInternalServerError,
			"could not save the probe scope: "+errWrite.Error())
	}

	state.mu.Lock()
	state.config.ProbeAccounts = accounts
	state.config.Models = models
	state.config.ProbeProxies = proxies
	state.config.ProbeProxiesRotating = rotating
	state.configErrors = problems
	state.mu.Unlock()

	log.Printf(logPrefix+"probe scope saved: accounts=%d models=%d proxies=%d rotating=%d (fields=%s)",
		len(accounts), len(models), len(proxies), len(rotating), q.Get("fields"))
	for _, problem := range problems {
		log.Printf(logPrefix+"config error (probe scope, not fatal): %s", problem)
	}

	out := scopeSaveResponse{
		Saved:              true,
		Fields:             sortedKeys(requested),
		ProbeAccounts:      accounts,
		Models:             models,
		ProbeProxyCount:    len(proxies),
		ProbeProxiesMasked: maskProxyURLs(proxies),
		RotatingCount:      len(rotating),
		RotatingMasked:     maskProxyURLs(rotating),
		TargetsTotal:       len(accounts) * len(models),
		ConfigErrors:       problems,
		Note: "已保存到插件自己的 scope 文件，立即生效，覆盖 config.yaml 里的同名项。" +
			"采集由看板上的「探测」启动，续期循环每 20 秒重读一次范围。",
	}
	if out.ProbeAccounts == nil {
		out.ProbeAccounts = []string{}
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	return jsonResponse(http.StatusOK, out)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func statusAccounts(observed []bucketObservation) ([]codexAuth, string, error) {
	accounts, errList := listCodexAuths()
	if errList == nil {
		return accounts, "host", nil
	}

	seen := make(map[string]bool)
	var fallback []codexAuth
	for _, cell := range observed {
		if seen[cell.AuthID] {
			continue
		}
		seen[cell.AuthID] = true

		fallback = append(fallback, codexAuth{AuthID: cell.AuthID, Enabled: true})
	}
	sort.Slice(fallback, func(i, j int) bool { return fallback[i].AuthID < fallback[j].AuthID })
	return fallback, "store", errList
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

type clearRequest struct {
	AuthID string `json:"auth_id"`
	Model  string `json:"model"`
	All    bool   `json:"all"`
}

type clearedBucket struct {
	AuthID string `json:"auth_id"`
	Model  string `json:"model"`
}

type clearResponse struct {
	Cleared int             `json:"cleared"`
	Buckets []clearedBucket `json:"buckets"`
}

func handleBucketsClear(body []byte) pluginapi.ManagementResponse {
	var req clearRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return managementError(http.StatusBadRequest, "could not decode the request body as JSON")
		}
	}
	return clearBuckets(req)
}

func clearBuckets(req clearRequest) pluginapi.ManagementResponse {
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	namesBucket := strings.TrimSpace(req.AuthID) != "" || strings.TrimSpace(req.Model) != ""

	var targets []clearedBucket
	switch {
	case req.All && namesBucket:

		return managementError(http.StatusBadRequest,
			`"all" cannot be combined with "auth_id" or "model" -- send one or the other`)
	case req.All:
	case strings.TrimSpace(req.AuthID) != "" && strings.TrimSpace(req.Model) != "":

		if _, errPath := bucketRelPath(req.AuthID, req.Model); errPath != nil {
			return managementError(http.StatusBadRequest, errPath.Error())
		}
		targets = append(targets, clearedBucket{AuthID: req.AuthID, Model: req.Model})
	default:
		return managementError(http.StatusBadRequest, `give either {"all":true} or both "auth_id" and "model"`)
	}

	if req.All {

		now := time.Now()
		state.mu.Lock()
		state.cookies = make(map[string]*routeCookieEntry)
		state.cookiesDirty = true
		state.mu.Unlock()
		if strings.TrimSpace(cfg.StoreDir) != "" {
			if err := writeRouteCookiePool(cfg.StoreDir, state.cookies, now, cfg.ttl()); err != nil {
				log.Printf(logPrefix+"pool rewrite after clear failed: %v", err)
			}
		}
		clearAllObservations()
		log.Printf(logPrefix + "cleared route-cookie pool and all observations")
		return jsonResponse(http.StatusOK, clearResponse{Cleared: 1, Buckets: []clearedBucket{}})
	}

	cleared := 0
	var done []clearedBucket
	for _, target := range targets {
		if deleteObservation(target.AuthID, target.Model) {
			cleared++
			done = append(done, target)
		}
	}
	if cleared > 0 {
		log.Printf(logPrefix+"cleared %d bucket observation(s)", cleared)
	}
	if done == nil {
		done = []clearedBucket{}
	}
	return jsonResponse(http.StatusOK, clearResponse{Cleared: cleared, Buckets: done})
}

type selftestRequest struct {
	Model  string `json:"model"`
	AuthID string `json:"auth_id"`
}

type selftestResponse struct {
	Reached    bool   `json:"reached"`
	StatusCode int    `json:"status_code"`
	Model      string `json:"model"`
	AuthID     string `json:"auth_id"`

	Targeted  bool `json:"targeted"`
	Harvested bool `json:"harvested"`

	UpstreamErrorCode string `json:"upstream_error_code"`
	UpstreamErrorType string `json:"upstream_error_type"`
	Note              string `json:"note"`
	Error             string `json:"error,omitempty"`
}

type upstreamErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const (
	selftestNote = "连通性自检不会落盘：host.model.execute 会跳过本插件的响应拦截器。采集请用看板上的「探测」。"

	selftestNoteUntargeted = selftestNote +
		" 本次未指定 auth_id，由调度器选号；上游响应不含账号标识，因此无法得知实际使用的是哪个号。要定点检查请传 auth_id。"

	selftestNoteNoStatus = " 上游返回了错误但宿主未透传 HTTP 状态码，故 status_code 为 0；请看 upstream_error_code 和 error 原文。"
)

func handleSelftest(body []byte) pluginapi.ManagementResponse {
	var req selftestRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return managementError(http.StatusBadRequest, "could not decode the request body as JSON")
		}
	}
	return runSelftest(req)
}

func runSelftest(req selftestRequest) pluginapi.ManagementResponse {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return managementError(http.StatusBadRequest, `"model" is required`)
	}

	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	if len(cfg.Models) > 0 && !containsFold(cfg.Models, model) {
		return managementError(http.StatusBadRequest,
			fmt.Sprintf("model %q is not in the configured models list", model))
	}

	authID := strings.TrimSpace(req.AuthID)
	if authID != "" {
		if _, errAuth := bucketRelPath(authID, model); errAuth != nil {
			return managementError(http.StatusBadRequest, "unsafe auth_id or model: "+errAuth.Error())
		}
	}

	if !hostAPIAvailable() {
		log.Printf(logPrefix + "selftest could not run: no host callback table")
		return managementError(http.StatusServiceUnavailable,
			"this plugin holds no host callback table, so it could not issue any request. "+
				"The self-test did not run; this says nothing about the upstream. "+
				"The plugin was loaded without a host API, which is a loader problem.")
	}

	out := selftestResponse{
		Model:     model,
		AuthID:    authID,
		Targeted:  authID != "",
		Harvested: false,
		Note:      selftestNote,
	}
	if !out.Targeted {
		out.Note = selftestNoteUntargeted
	}

	payload := map[string]any{
		"model": model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "ping"}},
		}},
		"store": false,
	}
	rawBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, errMarshal.Error())
	}

	exec := pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-responses",
		ExitProtocol:  "openai-responses",
		Model:         model,
		Stream:        false,
		Body:          rawBody,
		Headers:       http.Header{"Content-Type": []string{"application/json"}},

		AuthID: authID,
	}

	var execResp pluginapi.HostModelExecutionResponse
	errCall := hostCallJSON("host.model.execute", exec, &execResp)
	if errCall == nil {
		out.Reached = true
		out.StatusCode = execResp.StatusCode
		log.Printf(logPrefix+"selftest reached upstream model=%s auth=%s targeted=%t status=%d",
			orDash(model), orDash(authID), out.Targeted, out.StatusCode)
		return jsonResponse(http.StatusOK, out)
	}

	out.Error = errCall.Error()
	upstream, okUpstream := upstreamErrorFrom(out.Error)
	status, okStatus := statusFromExecutionError(out.Error)
	switch {
	case okUpstream:
		out.Reached = true
		out.UpstreamErrorCode = strings.TrimSpace(upstream.Error.Code)
		out.UpstreamErrorType = strings.TrimSpace(upstream.Error.Type)
		if okStatus {
			out.StatusCode = status
		} else {
			out.Note += selftestNoteNoStatus
		}
	case okStatus:
		out.Reached = true
		out.StatusCode = status
	}
	log.Printf(logPrefix+"selftest model=%s auth=%s targeted=%t reached=%t status=%d upstream_code=%s",
		orDash(model), orDash(authID), out.Targeted, out.Reached, out.StatusCode, orDash(out.UpstreamErrorCode))
	return jsonResponse(http.StatusOK, out)
}

func upstreamErrorFrom(message string) (upstreamErrorBody, bool) {
	idx := strings.Index(message, "{")
	if idx < 0 {
		return upstreamErrorBody{}, false
	}
	var body upstreamErrorBody
	if errDecode := json.NewDecoder(strings.NewReader(message[idx:])).Decode(&body); errDecode != nil {
		return upstreamErrorBody{}, false
	}
	if body.Error.Type == "" && body.Error.Code == "" && body.Error.Message == "" {
		return upstreamErrorBody{}, false
	}
	return body, true
}

func statusFromExecutionError(message string) (int, bool) {
	const marker = "failed with status "
	idx := strings.LastIndex(message, marker)
	if idx < 0 {
		return 0, false
	}
	digits := strings.TrimSpace(message[idx+len(marker):])
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	status := 0
	for _, char := range digits[:end] {
		status = status*10 + int(char-'0')
	}
	if status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}

func hostCallJSON(method string, payload any, out any) error {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return errMarshal
	}
	response, errCall := hostCall(method, raw)
	if errCall != nil {
		return errCall
	}
	if len(response) == 0 {
		return fmt.Errorf("host call %s returned nothing", method)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(response, &env); errUnmarshal != nil {
		return fmt.Errorf("decode %s response: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("host call %s failed", method)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, "could not encode the response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

func managementError(status int, message string) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(map[string]string{"error": message})
	if errMarshal != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}
