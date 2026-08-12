package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

// globalConfigJSON is the shape the reconciler is expected to write into
// defaultConfig: shared state and the endpoint, but deliberately no `enabled`.
const globalConfigJSON = `{
  "local_auth": {
    "enabled": true,
    "keys": {
      "3192253c1f4a9b7e": {"exp": 1790000000, "digest": "sha256$aa$bb", "user_id": 7},
      "b7d2e91045ac6f38": {"exp": null, "digest": "sha256$cc$dd", "user_id": 9}
    },
    "refs": {"58": {"exp": null}, "77": {"exp": 1790000000}}
  },
  "authz": {
    "endpoint": {"path": "/token-auth", "request_method": "GET", "service_name": "gpustack.static", "service_port": 80},
    "endpoint_mode": "forward_auth",
    "timeout": 30000,
    "authorization_request": {
      "allowed_headers": [{"exact": "x-higress-llm-model"}],
      "headers_to_add": {"X-GPUStack-Auth-Token": "derived-token"}
    },
    "authorization_response": {
      "allowed_upstream_headers": [{"exact": "X-Mse-Consumer"}, {"exact": "cookie"}]
    }
  },
  "route_match_regexes": ["^ns/ai-route-route-"],
  "status_on_error": 403,
  "auth_cache": {"header": "x-gpustack-auth-cache"}
}`

func mustGlobal(t *testing.T) PluginConfig {
	t.Helper()
	var config PluginConfig
	if err := parseGlobalConfig(gjson.Parse(globalConfigJSON), &config); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	return config
}

// The whole safety story rests on the route gate: nothing else stops the global
// config from applying to every route on a shared gateway.
func TestRouteGate(t *testing.T) {
	config := mustGlobal(t)
	if config.AccessPolicy != "" {
		t.Fatal("defaultConfig must not carry access_policy; only a dedicated rule may declare a route public")
	}

	cases := []struct {
		routeName string
		want      bool
	}{
		{"ns/ai-route-route-42.internal", true},
		{"ns/ai-route-route-42.fallback.internal", true},
		{"ns/some-other-tenant-route", false},
		{"ai-route-route-42.internal", false}, // unanchored namespace prefix
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.routeName, func(t *testing.T) {
			if got := matchesRoute(config.RouteMatchRegexes, tc.routeName); got != tc.want {
				t.Errorf("matchesRoute(%q) = %v, want %v", tc.routeName, got, tc.want)
			}
		})
	}
}

// An unconfigured gate must leave the plugin inert, never wide open.
func TestEmptyRouteGateMatchesNothing(t *testing.T) {
	var config PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{}`), &config); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if matchesRoute(config.RouteMatchRegexes, "ns/ai-route-route-42.internal") {
		t.Fatal("an absent route_match_regexes matched a route")
	}
}

func TestGlobalConfigParsesEveryBlock(t *testing.T) {
	config := mustGlobal(t)

	if !config.LocalAuth.Enabled {
		t.Error("local_auth.enabled not parsed")
	}
	if len(config.LocalAuth.Keys) != 2 || len(config.LocalAuth.Refs) != 2 {
		t.Fatalf("keys=%d refs=%d, want 2 and 2", len(config.LocalAuth.Keys), len(config.LocalAuth.Refs))
	}
	if got := config.LocalAuth.Keys["3192253c1f4a9b7e"]; got.UserID != 7 || got.Exp == nil || *got.Exp != 1790000000 {
		t.Errorf("keys entry = %+v", got)
	}
	if got := config.LocalAuth.Keys["b7d2e91045ac6f38"]; got.Exp != nil {
		t.Error("an explicit null exp must mean never expires, not epoch zero")
	}
	if config.Authz.Path != "/token-auth" || config.Authz.RequestMethod != "GET" {
		t.Errorf("endpoint = %q %q", config.Authz.RequestMethod, config.Authz.Path)
	}
	if config.Authz.TimeoutMillis != 30000 {
		t.Errorf("timeout = %d", config.Authz.TimeoutMillis)
	}
	if !config.Authz.ready() {
		t.Error("a fully specified endpoint must be ready to dispatch")
	}
	if config.StatusOnError != 403 {
		t.Errorf("status_on_error = %d", config.StatusOnError)
	}
	if config.AuthCache.Header != "x-gpustack-auth-cache" {
		t.Errorf("auth_cache.header = %q", config.AuthCache.Header)
	}
	if config.FailureModeAllow {
		t.Error("failure_mode_allow must default to false: GPUStack fails closed today")
	}
}

func TestOverrideInheritsGlobal(t *testing.T) {
	global := mustGlobal(t)

	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if len(rule.RouteMatchRegexes) != 1 {
		t.Fatal("a rule must inherit the route gate")
	}
	if rule.AccessPolicy != "" {
		t.Error("the generic rule must not inherit or invent a public policy")
	}
	if len(rule.LocalAuth.Keys) != 2 || rule.Authz.Path != "/token-auth" || rule.AuthCache.Header == "" {
		t.Error("a rule must inherit the shared state it did not redeclare")
	}
}

func TestOverrideCanDeclarePublic(t *testing.T) {
	global := mustGlobal(t)

	var rule PluginConfig
	err := parseOverrideRuleConfig(
		gjson.Parse(`{"access_policy": "PUBLIC"}`), global, &rule)
	if err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if rule.AccessPolicy != "public" {
		t.Errorf("access_policy = %q, want the case-folded %q", rule.AccessPolicy, "public")
	}
}

// Every rule is parsed against the same global value. If a rule's parse can
// reach into the global key table, one route's config corrupts every other
// route's notion of which keys are live -- the worst failure this plugin has.
func TestOverrideDoesNotMutateGlobalKeyTable(t *testing.T) {
	global := mustGlobal(t)

	var first PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{
		"local_auth": {"keys": {"cafebabecafebabe": {"digest": "sha256$ee$ff", "user_id": 1}}}
	}`), global, &first); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if len(first.LocalAuth.Keys) != 1 {
		t.Fatalf("a redeclared key table must replace, not merge: got %d entries", len(first.LocalAuth.Keys))
	}
	if len(global.LocalAuth.Keys) != 2 {
		t.Fatalf("global key table was mutated through the shared map header: got %d entries", len(global.LocalAuth.Keys))
	}
	if _, ok := global.LocalAuth.Keys["cafebabecafebabe"]; ok {
		t.Error("the rule's key leaked into the global table")
	}

	// A second rule that redeclares nothing must still see the pristine global.
	var second PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{}`), global, &second); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if len(second.LocalAuth.Keys) != 2 {
		t.Errorf("a later rule saw a corrupted global table: %d entries", len(second.LocalAuth.Keys))
	}
}

// gjson reports an explicit null as existing, so a naive Exists() check would
// take the branch and quietly replace an inherited table with an empty one --
// which reads as "every key revoked".
func TestNullLocalAuthTablesDoNotWipeInheritedOnes(t *testing.T) {
	global := mustGlobal(t)

	var rule PluginConfig
	if err := parseOverrideRuleConfig(
		gjson.Parse(`{"local_auth": {"keys": null, "refs": null}}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if len(rule.LocalAuth.Keys) != 2 || len(rule.LocalAuth.Refs) != 2 {
		t.Errorf("a null table emptied the inherited one: keys=%d refs=%d",
			len(rule.LocalAuth.Keys), len(rule.LocalAuth.Refs))
	}
}

func TestConfigRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			name: "key without a digest",
			json: `{"local_auth": {"keys": {"3192253c1f4a9b7e": {"user_id": 7}}}}`,
		},
		{
			name: "key with an empty access key",
			json: `{"local_auth": {"keys": {"": {"digest": "sha256$aa$bb"}}}}`,
		},
		{
			name: "endpoint without a service name",
			json: `{"authz": {"endpoint": {"path": "/token-auth"}}}`,
		},
		{
			name: "endpoint without a path",
			json: `{"authz": {"endpoint": {"service_name": "gpustack.static"}}}`,
		},
		{
			name: "unsupported endpoint mode",
			json: `{"authz": {"endpoint_mode": "envoy"}}`,
		},
		{
			name: "uncompilable route gate",
			json: `{"route_match_regexes": ["([a-z"]}`,
		},
		{
			name: "uncompilable regex matcher",
			json: `{"authz": {"authorization_request": {"allowed_headers": [{"regex": "([a-z"}]}}}`,
		},
		{
			name: "matcher entry with no known pattern",
			json: `{"authz": {"authorization_request": {"allowed_headers": [{"equals": "cookie"}]}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config PluginConfig
			if err := parseGlobalConfig(gjson.Parse(tc.json), &config); err == nil {
				t.Error("expected a parse error")
			}
		})
	}
}

func TestStatusOnErrorDefaults(t *testing.T) {
	var config PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{}`), &config); err != nil {
		t.Fatalf("an empty global block is legitimate: %v", err)
	}
	if config.StatusOnError != defaultStatusOnError {
		t.Errorf("status_on_error = %d, want %d", config.StatusOnError, defaultStatusOnError)
	}
	if config.Authz.ready() {
		t.Error("a config with no endpoint must not claim to be ready to dispatch")
	}
}

func TestUpstreamRequestHeadersToRemove(t *testing.T) {
	var config PluginConfig
	err := parseGlobalConfig(gjson.Parse(
		`{"upstream_request": {"headers_to_remove": ["Cookie", "  X-Debug  ", ""]}}`), &config)
	if err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	got := config.UpstreamRequest.HeadersToRemove
	// Lower-cased because Envoy hands header names down lower-cased; blanks
	// dropped so a stray comma in the CR cannot turn into a removal of "".
	want := []string{"cookie", "x-debug"}
	if len(got) != len(want) {
		t.Fatalf("headers_to_remove = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("headers_to_remove[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A rule narrowing the list must not reach into the slice it inherited, for the
// same reason the key tables are rebuilt rather than mutated.
func TestUpstreamRequestOverrideDoesNotMutateGlobal(t *testing.T) {
	var global PluginConfig
	if err := parseGlobalConfig(gjson.Parse(
		`{"upstream_request": {"headers_to_remove": ["cookie", "x-debug"]}}`), &global); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}

	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(
		`{"enabled": true, "upstream_request": {"headers_to_remove": ["cookie"]}}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if len(rule.UpstreamRequest.HeadersToRemove) != 1 {
		t.Errorf("rule list = %v, want it replaced not merged", rule.UpstreamRequest.HeadersToRemove)
	}
	if len(global.UpstreamRequest.HeadersToRemove) != 2 {
		t.Errorf("global list was mutated to %v", global.UpstreamRequest.HeadersToRemove)
	}

	var inheritor PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"enabled": true}`), global, &inheritor); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if len(inheritor.UpstreamRequest.HeadersToRemove) != 2 {
		t.Errorf("a later rule inherited a corrupted list: %v", inheritor.UpstreamRequest.HeadersToRemove)
	}
}
