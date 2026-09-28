package main

import (
	"encoding"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var statusResponsePublicFields = []string{
	"accounts_error",
	"accounts_source",
	"buckets",
	"buckets.auth_id",
	"buckets.enabled",
	"buckets.len",
	"buckets.model",

	"buckets.observed",
	"buckets.observed.injected_limited",
	"buckets.observed.injected_normal",
	"buckets.observed.injected_other",
	"buckets.observed.injected_silent",
	"buckets.observed.last_at",
	"buckets.observed.last_kind",
	"buckets.observed.last_len",
	"buckets.observed.last_natural_at",
	"buckets.observed.last_natural_kind",

	"buckets.observed.last_signed_at",
	"buckets.observed.last_signed_kind",
	"buckets.observed.last_signed_wrote",
	"buckets.observed.last_wrote",
	"buckets.observed.natural_limited",
	"buckets.observed.natural_normal",
	"buckets.observed.natural_other",
	"buckets.observed.recent_24h",
	"buckets.observed.recent_24h.injected_limited",
	"buckets.observed.recent_24h.injected_normal",
	"buckets.observed.recent_24h.injected_other",
	"buckets.observed.recent_24h.injected_silent",
	"buckets.observed.recent_24h.natural_limited",
	"buckets.observed.recent_24h.natural_normal",
	"buckets.observed.recent_24h.natural_other",
	"buckets.ready",

	"buckets.route_cookies_seconds_left",
	"config_errors",
	"counters",
	"counters.harvest",
	"counters.pass",
	"counters.skip",
	"counters.steer",
	"counters_since",
	"dry_run",
	"generated_at",
	"models",

	"observation_feed",
	"observation_feed.at",
	"observation_feed.auth_id",
	"observation_feed.kind",
	"observation_feed.len",
	"observation_feed.model",

	"observation_feed.served",
	"observation_feed.wrote",
	"observations_since",
	"probe_accounts",
	"probe_proxies",
	"probe_proxies_rotating",
	"probe_proxy_count",
	"probe_proxy_rotating_count",
	"probe_run",
	"probe_run.current",
	"probe_run.done",
	"probe_run.error",
	"probe_run.finished_at",
	"probe_run.lines",
	"probe_run.running",
	"probe_run.started_at",
	"probe_run.total",
	"replace_length",
	"role",
	"store_dir",
	"store_error",
	"targets_ready",
	"targets_total",
	"template_length",
	"ttl_seconds",
}

var choicesResponsePublicFields = []string{
	"accounts",
	"accounts.disabled",
	"accounts.label",
	"accounts.name",
	"accounts.selected",
	"error",
	"models",
	"models.name",
	"models.selected",
}

var proxyCheckResponsePublicFields = []string{
	"blocked",
	"checked",
	"dead",
	"direct",
	"distinct_ips",
	"mismatches",
	"ms",
	"note",
	"ok",
	"other",
	"results",
	"results.colo",
	"results.country",
	"results.detail",
	"results.exit_ip",
	"results.index",
	"results.mismatch",
	"results.ms",
	"results.pool",
	"results.proxy",
	"results.rotated",
	"results.status_code",
	"results.verdict",
	"static_checked",
	"timed_out",
}

func TestAnonymouslyReadableShapesArePinned(t *testing.T) {
	for _, doc := range []struct {
		name	string
		typ	reflect.Type
		want	[]string
		route	string
	}{
		{"statusResponse", reflect.TypeOf(statusResponse{}), statusResponsePublicFields, "/status"},
		{"choicesResponse", reflect.TypeOf(choicesResponse{}), choicesResponsePublicFields, "/ops/choices"},
		{"proxyCheckResponse", reflect.TypeOf(proxyCheckResponse{}), proxyCheckResponsePublicFields, "/ops/proxy-check"},
	} {
		t.Run(doc.name, func(t *testing.T) {
			got := jsonFieldPaths(t, doc.typ, "")
			assertSetEqual(t, doc.name+" fields", got, doc.want,
				"this document is served on "+doc.route+", which needs no credential; "+
					"if the new field can carry a secret it does not belong on this struct at all")
		})
	}
}

type walkerProbeInner struct {
	Alpha	string	`json:"alpha"`
	Omit	string	`json:"-"`
	hidden	string	//nolint:unused // present so the walk is seen to skip it
}

type walkerProbeEmbedded struct {
	Promoted string `json:"promoted"`
}

type walkerProbeOuter struct {
	walkerProbeEmbedded
	Top		int			`json:"top"`
	Nested		walkerProbeInner	`json:"nested"`
	List		[]walkerProbeInner	`json:"list"`
	Pointer		*walkerProbeInner	`json:"pointer"`
	Names		[]string		`json:"names"`
	Untagged	bool
}

func TestJSONFieldPathsDescends(t *testing.T) {
	got := jsonFieldPaths(t, reflect.TypeOf(walkerProbeOuter{}), "")
	want := []string{
		"Untagged",
		"list",
		"list.alpha",
		"names",
		"nested",
		"nested.alpha",
		"pointer",
		"pointer.alpha",
		"promoted",
		"top",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the walk does not descend as the pins above assume.\n got: %v\nwant: %v", got, want)
	}
	for _, path := range got {
		if strings.Contains(path, "Omit") || strings.Contains(path, "hidden") {
			t.Errorf("walk emitted %q; json:\"-\" and unexported fields are never serialised", path)
		}
	}

	raw, err := json.Marshal(walkerProbeOuter{})
	if err != nil {
		t.Fatalf("marshalling the probe: %v", err)
	}
	var emitted map[string]json.RawMessage
	if err := json.Unmarshal(raw, &emitted); err != nil {
		t.Fatalf("unmarshalling the probe: %v", err)
	}
	var actual []string
	for key := range emitted {
		actual = append(actual, key)
	}
	var walked []string
	for _, path := range got {
		if !strings.Contains(path, ".") {
			walked = append(walked, path)
		}
	}
	sort.Strings(actual)
	sort.Strings(walked)
	if !reflect.DeepEqual(walked, actual) {
		t.Errorf("the walk and encoding/json disagree about the top-level keys; every pin in this file "+
			"is a claim about what ships, so the walk has to match the marshaller.\n walked: %v\nmarshalled: %v",
			walked, actual)
	}
}

func jsonFieldPaths(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()
	var out []string
	collectJSONFields(t, typ, prefix, &out)
	sort.Strings(out)
	return out
}

func collectJSONFields(t *testing.T, typ reflect.Type, prefix string, out *[]string) {
	t.Helper()

	typ = derefType(typ)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("collectJSONFields called on %s, which is not a struct", typ)
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]

		if field.Anonymous && name == "" {

			embedded := derefType(field.Type)
			if embedded.Kind() != reflect.Struct {
				t.Fatalf("%s embeds the non-struct %s; encoding/json's rules there are subtle "+
					"(an unexported one is dropped outright) and nothing in this plugin does it, "+
					"so this walk refuses to guess", typ, field.Type)
			}
			rejectOpaque(t, embedded, typ.String()+"."+field.Name)
			collectJSONFields(t, embedded, prefix, out)
			continue
		}

		if field.PkgPath != "" {
			continue
		}
		if name == "" {
			name = field.Name
		}

		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		*out = append(*out, path)

		descendJSONField(t, field.Type, path, out, typ.String()+"."+field.Name)
	}
}

func descendJSONField(t *testing.T, typ reflect.Type, path string, out *[]string, where string) {
	t.Helper()

	typ = derefType(typ)
	rejectOpaque(t, typ, where)

	switch typ.Kind() {
	case reflect.Struct:
		collectJSONFields(t, typ, path, out)
	case reflect.Slice, reflect.Array:

		descendJSONField(t, typ.Elem(), path, out, where+" element")
	}
}

func rejectOpaque(t *testing.T, typ reflect.Type, where string) {
	t.Helper()

	var (
		jsonMarshaler	= reflect.TypeOf((*json.Marshaler)(nil)).Elem()
		textMarshaler	= reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	)
	for _, iface := range []reflect.Type{jsonMarshaler, textMarshaler} {
		if typ.Implements(iface) || reflect.PointerTo(typ).Implements(iface) {
			t.Fatalf("%s is a %s with its own %s; its JSON keys are not readable off the type. "+
				"If it serialises to a single scalar (time.Time does, as an RFC3339 string), say so here "+
				"and allow it; if it serialises to an object, its fields need pinning of their own.",
				where, typ, iface.Name())
		}
	}

	switch typ.Kind() {
	case reflect.Map, reflect.Interface:
		t.Fatalf("%s is a %s, whose keys cannot be pinned by walking the type; "+
			"an open-ended container on an anonymously readable document needs its own assertion", where, typ.Kind())
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("%s is a %s (json.RawMessage or []byte); it serialises as whatever it happens to hold, "+
				"which is not something this walk can pin", where, typ)
		}
	}
}

func derefType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

var keylessResourcePaths = []string{
	"/dashboard",
	"/ops/choices",
	"/ops/clear",
	"/ops/dry-run",
	"/ops/probe/cancel",
	"/ops/probe/start",
	"/ops/proxy-check",
	"/ops/role",
	"/ops/scope",
	"/ops/selftest",
	"/status",
}

var authenticatedRoutes = []string{
	"GET /codex-turn-state/cloud-status",
	"GET /codex-turn-state/config",
	"GET /codex-turn-state/status",
	"POST /codex-turn-state/buckets/clear",
	"POST /codex-turn-state/selftest",
}

func TestKeylessSurfaceIsPinned(t *testing.T) {
	dir := t.TempDir()
	mustConfigure(t, probeRoleConfig(dir))

	reg := driveManagementRegister(t)

	var gotResources []string
	for _, res := range reg.Resources {
		gotResources = append(gotResources, res.Path)
	}
	assertSetEqual(t, "keyless resource paths", gotResources, keylessResourcePaths,
		"every path here is served without a credential; adding one is a deliberate act")

	var gotRoutes []string
	for _, route := range reg.Routes {
		gotRoutes = append(gotRoutes, route.Method+" "+route.Path)
	}
	assertSetEqual(t, "authenticated management routes", gotRoutes, authenticatedRoutes,
		"moving one of these to the resource list would publish it")
}

var configFieldNames = []string{
	"role",
	"store_dir",
	"template_length",
	"replace_length",
	"ttl_seconds",
	"dry_run",
	"log_decisions",
	"models",
	"probe_accounts",
	"probe_proxies",
	"probe_proxies_rotating",
	"probe_management_key",
	"probe_base_url",
	"cloud_mint",
}

func TestDeclaredConfigFieldsArePinned(t *testing.T) {
	fields := pluginRegistration().Metadata.ConfigFields

	var got []string
	for _, field := range fields {
		got = append(got, field.Name)
	}
	if !reflect.DeepEqual(got, configFieldNames) {
		t.Errorf("declared config fields changed.\n got: %v\nwant: %v\n"+
			"Order matters here because the host renders them in it.", got, configFieldNames)
	}

	for _, field := range fields {
		if strings.TrimSpace(field.Description) == "" {
			t.Errorf("config field %q is declared without a description", field.Name)
		}
	}
}

func assertSetEqual(t *testing.T, what string, got, want []string, why string) {
	t.Helper()

	if len(got) == 0 {
		t.Fatalf("%s: nothing was declared, so this assertion would pass vacuously", what)
	}

	seen := make(map[string]bool, len(want))
	for _, entry := range want {
		seen[entry] = true
	}
	for _, entry := range got {
		if !seen[entry] {
			t.Errorf("%s gained %q -- %s", what, entry, why)
		}
		delete(seen, entry)
	}
	var missing []string
	for entry := range seen {
		missing = append(missing, entry)
	}
	sort.Strings(missing)
	for _, entry := range missing {
		t.Errorf("%s lost %q; update the literal if the removal is intended", what, entry)
	}
}
