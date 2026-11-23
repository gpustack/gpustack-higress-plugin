// GPUStack-local tests (no upstream counterpart): the request-priority
// header that ai-proxy translates per backend (gpustack/gpustack#6739).

package main

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// priorityConfigJSON has one key and one ref carrying a priority, one of each
// without, and one key whose priority is out of range.
const priorityConfigJSON = `{
  "local_auth": {
    "enabled": true,
    "keys": {
      "with":    {"digest": "sha256$aa$bb", "user_id": 7, "priority": 20},
      "without": {"digest": "sha256$cc$dd", "user_id": 9},
      "null":    {"digest": "sha256$ee$ff", "user_id": 10, "priority": null},
      "junk":    {"digest": "sha256$11$22", "user_id": 11, "priority": "20"},
      "huge":    {"digest": "sha256$33$44", "user_id": 12, "priority": 2147483648},
      "negative": {"digest": "sha256$55$66", "user_id": 13, "priority": -1},
      "frac":     {"digest": "sha256$66$77", "user_id": 15, "priority": 1.9},
      "negfrac":  {"digest": "sha256$77$99", "user_id": 16, "priority": -0.5},
      "expired": {"digest": "sha256$77$88", "user_id": 14, "priority": 5, "exp": 1000}
    },
    "refs": {
      "58": {"priority": 5},
      "77": {},
      "88": {"priority": null}
    }
  },
  "route_match_regexes": ["^ns/ai-route-route-"]
}`

func priorityConfig(t *testing.T) PluginConfig {
	t.Helper()
	var config PluginConfig
	if err := parseGlobalConfig(gjson.Parse(priorityConfigJSON), &config); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	return config
}

func int64p(v int64) *int64 { return &v }

// The published value is copied verbatim when it is a non-negative int32;
// everything else -- absent, null, a string, out of range -- is "not
// configured", never an error and never a clipped value.
func TestParsePriority(t *testing.T) {
	config := priorityConfig(t)

	cases := []struct {
		accessKey string
		want      *int64
	}{
		{"with", int64p(20)},
		{"without", nil},
		{"null", nil},
		{"junk", nil},
		{"huge", nil},
		{"negative", nil},
		// gjson's Number includes fractions and Int() truncates them; the
		// literal itself must be an integer.
		{"frac", nil},
		{"negfrac", nil},
	}
	for _, c := range cases {
		got := config.LocalAuth.Keys[c.accessKey].Priority
		switch {
		case c.want == nil && got != nil:
			t.Errorf("keys[%s].priority = %d, want unset", c.accessKey, *got)
		case c.want != nil && got == nil:
			t.Errorf("keys[%s].priority unset, want %d", c.accessKey, *c.want)
		case c.want != nil && *got != *c.want:
			t.Errorf("keys[%s].priority = %d, want %d", c.accessKey, *got, *c.want)
		}
	}

	refs := config.LocalAuth.Refs
	if v := refs["58"].Priority; v == nil || *v != 5 {
		t.Errorf("refs[58].priority = %v, want 5", v)
	}
	if v := refs["77"].Priority; v != nil {
		t.Errorf("refs[77].priority = %d, want unset", *v)
	}
	if v := refs["88"].Priority; v != nil {
		t.Errorf("refs[88].priority = %d, want unset", *v)
	}

	// The full range is inclusive at the top.
	var edge PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{"local_auth":{"keys":{"edge":{"digest":"x","priority":2147483647}}}}`), &edge); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if v := edge.LocalAuth.Keys["edge"].Priority; v == nil || *v != requestPriorityMax {
		t.Errorf("priority 2^31-1 = %v, want accepted", v)
	}
}

// The lookup answers for both identity forms, and only for a live key that
// carries a value.
func TestLocalAuthPriority(t *testing.T) {
	config := priorityConfig(t)
	now := markerNow()

	cases := []struct {
		name string
		id   identity
		want int64
		ok   bool
	}{
		{"key with priority", identity{State: identityResolved, AccessKey: "with"}, 20, true},
		{"ref with priority", identity{State: identityResolved, Ref: "58"}, 5, true},
		{"key without priority", identity{State: identityResolved, AccessKey: "without"}, 0, false},
		{"ref without priority", identity{State: identityResolved, Ref: "77"}, 0, false},
		{"ref with null priority", identity{State: identityResolved, Ref: "88"}, 0, false},
		{"unknown key", identity{State: identityResolved, AccessKey: "nope"}, 0, false},
		{"unknown ref", identity{State: identityResolved, Ref: "nope"}, 0, false},
		{"anonymous", identity{State: identityResolved, Anonymous: true}, 0, false},
		{"unresolved", identity{State: identityUnresolved}, 0, false},
		{"expired key", identity{State: identityResolved, AccessKey: "expired"}, 0, false},
	}
	for _, c := range cases {
		got, ok := config.LocalAuth.priority(c.id, now)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: priority = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}

	// An expired entry's priority is honoured one second before expiry and
	// not at it.
	id := identity{State: identityResolved, AccessKey: "expired"}
	if _, ok := config.LocalAuth.priority(id, time.Unix(999, 0)); !ok {
		t.Error("a key one second before expiry lost its priority")
	}
	if _, ok := config.LocalAuth.priority(id, time.Unix(1000, 0)); ok {
		t.Error("a key at its expiry kept its priority")
	}
}

// The header value is the decimal priority, and only a resolved, named caller
// with a configured key gets one.
func TestRequestPriority(t *testing.T) {
	config := priorityConfig(t)
	now := markerNow()

	cases := []struct {
		name string
		id   identity
		want string
		ok   bool
	}{
		{"key", identity{State: identityResolved, Source: sourceKeys, AccessKey: "with"}, "20", true},
		{"ref", identity{State: identityResolved, Source: sourceMarker, Ref: "58"}, "5", true},
		{"unconfigured", identity{State: identityResolved, AccessKey: "without"}, "", false},
		{"anonymous", identity{State: identityResolved, Anonymous: true}, "", false},
		{"unresolved", identity{State: identityUnresolved}, "", false},
	}
	for _, c := range cases {
		got, ok := requestPriority(config, c.id, now)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: requestPriority = (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}

	// With local authentication off there is no table to answer from.
	off := priorityConfig(t)
	off.LocalAuth.Enabled = false
	if _, ok := requestPriority(off, identity{State: identityResolved, AccessKey: "with"}, now); ok {
		t.Error("a priority was published with local_auth disabled")
	}
}

// A client must not be able to hand the authorization call a priority of its
// own choosing, whatever the allowed_headers matcher admits.
func TestBuildAuthzRequestHeadersDropsClientSuppliedPriority(t *testing.T) {
	config := testConfig(t)
	config.Authz.AllowedHeaders = mustHeaderMatcher(t, `[{"prefix":"x-"}]`)

	got := buildAuthzRequestHeaders(config, testRequestInfo, [][2]string{
		{headerRequestPriority, "2147483647"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}, identity{State: identityUnresolved}, testConn)

	if v := got.Get(headerRequestPriority); v != "" {
		t.Fatalf("a client-supplied priority reached the authorization call: %q", v)
	}
}

// The server's own priority header travels to the upstream request without
// being declared, like the marker: when the identity was the server's to name
// there is no table entry to read a value from, so this is the only source.
func TestRelayResponseHeaderCarriesTheServerPriority(t *testing.T) {
	config := testConfig(t)
	config.Authz.AllowedUpstreamHeaders = nil

	if !relayResponseHeader(config, "X-GpuStack-Request-Priority") {
		t.Error("the server's priority header was not relayed")
	}
	// Nothing else is let through by the unconditional rule.
	if relayResponseHeader(config, "x-something-else") {
		t.Error("an unrelated response header was relayed")
	}

	// The value is held to the table's contract: it travels undeclared, so a
	// malformed one is dropped here instead of reaching ai-proxy.
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"20", true}, {"2147483647", true},
		{"abc", false}, {"1.5", false}, {"-0.5", false}, {"-1", false}, {"2147483648", false},
	} {
		if got := relayUpstreamHeader(config, "X-GpuStack-Request-Priority", c.value); got != c.want {
			t.Errorf("relay priority %q = %v, want %v", c.value, got, c.want)
		}
	}
	// A declared header is not subject to that rule.
	m, err := buildHeaderMatcher([]gjson.Result{gjson.Parse(`{"exact":"x-declared"}`)})
	if err != nil {
		t.Fatalf("buildHeaderMatcher: %v", err)
	}
	config.Authz.AllowedUpstreamHeaders = m
	if !relayUpstreamHeader(config, "x-declared", "anything") {
		t.Error("a declared header was not relayed")
	}
}
