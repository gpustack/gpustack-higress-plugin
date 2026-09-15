package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

func parse(t *testing.T, raw string) Config {
	t.Helper()
	var c Config
	if err := parseConfig(gjson.Parse(raw), &c); err != nil {
		t.Fatalf("parseConfig(%s): %v", raw, err)
	}
	return c
}

// An empty, missing or malformed sessionKeys must error out rather than
// silently do nothing: Higress's defaultConfig also applies to routes absent
// from matchRules, and a config that does nothing is very hard to tell apart
// from one that is simply wrong.
func TestSessionKeysValidation(t *testing.T) {
	bad := []struct{ name, raw string }{
		{"missing field", `{}`},
		{"not an array", `{"sessionKeys":{"header":"x"}}`},
		{"empty array", `{"sessionKeys":[]}`},
		{"empty object entry", `{"sessionKeys":[{}]}`},
		{"whitespace only", `{"sessionKeys":[{"header":"  "}]}`},
		// Setting both in one link is ambiguous: each link is an independent
		// hop, and guessing wrong would make the actual order disagree with
		// what the config says -- and order is the entire meaning of this
		// field.
		{"both sources in one link", `{"sessionKeys":[{"header":"x","bodyKey":"y"}]}`},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := parseConfig(gjson.Parse(tt.raw), &c); err == nil {
				t.Errorf("expected an error for %s", tt.raw)
			}
		})
	}
}

// Order is priority and must be preserved exactly -- it is the entire meaning
// of this field.
func TestSessionKeysPreservesOrder(t *testing.T) {
	keys, err := parseSessionKeys(gjson.Parse(`{"sessionKeys":[
		{"header":"session_id"},
		{"header":"x-client-request-id"},
		{"bodyKey":"prompt_cache_key"},
		{"bodyKey":"metadata.user_id"}
	]}`))
	if err != nil {
		t.Fatalf("parseSessionKeys: %v", err)
	}
	want := []keySource{
		{sourceHeader, "session_id"},
		{sourceHeader, "x-client-request-id"},
		{sourceBody, "prompt_cache_key"},
		{sourceBody, "metadata.user_id"},
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d entries, want %d", len(keys), len(want))
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("position %d: got %+v, want %+v", i, keys[i], want[i])
		}
	}
}

// Header names are normalised to lower case: Envoy's headers are all lower
// case, and a config that writes Session-Id and silently fails to match is
// very hard to diagnose.
func TestHeaderNamesAreLowercased(t *testing.T) {
	keys, err := parseSessionKeys(gjson.Parse(`{"sessionKeys":[{"header":"X-Client-Request-Id"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].name != "x-client-request-id" {
		t.Errorf("got %q", keys[0].name)
	}
	// Body keys are gjson paths and are case-sensitive, so they **must not** be
	// folded.
	bk, _ := parseSessionKeys(gjson.Parse(`{"sessionKeys":[{"bodyKey":"metadata.userID"}]}`))
	if bk[0].name != "metadata.userID" {
		t.Errorf("bodyKey got case-folded: %q", bk[0].name)
	}
}

// A header-only chain must never stop iteration to buffer the body -- that is
// the direct benefit of the two highest-priority sources both being headers.
func TestHasBodySource(t *testing.T) {
	onlyHeaders := parse(t, `{"sessionKeys":[{"header":"session_id"},{"header":"x-client-request-id"}]}`)
	if onlyHeaders.hasBodySource() {
		t.Error("a header-only chain must never buffer the body")
	}
	mixed := parse(t, `{"sessionKeys":[{"header":"session_id"},{"bodyKey":"prompt_cache_key"}]}`)
	if !mixed.hasBodySource() {
		t.Error("mixed chain should report a body source")
	}
}

// By default body sources are only looked up on the Responses API and
// Anthropic messages -- the two endpoints known to carry a session key.
// **chat/completions is excluded by default**: it is the hottest path, there is
// currently no known standard session key on it, and buffering a body for it
// is a pure loss.
func TestBodyLookupDefaultSuffixes(t *testing.T) {
	c := parse(t, `{"sessionKeys":[{"bodyKey":"prompt_cache_key"}]}`)

	tests := []struct {
		path string
		want bool
	}{
		{"/v1/responses", true},
		{"/v1-openai/responses", true},
		{"/v1/messages", true},
		{"/v1/chat/completions", false},
		{"/v1/embeddings", false},
		{"/v1/audio/transcriptions", false},
	}
	for _, tt := range tests {
		if got := matchPathSuffix(tt.path, c.enableOnPathSuffix); got != tt.want {
			t.Errorf("matchPathSuffix(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestPathSuffixOverride(t *testing.T) {
	c := parse(t, `{"sessionKeys":[{"bodyKey":"sid"}],"enableOnPathSuffix":["/chat/completions"]}`)
	if matchPathSuffix("/v1/responses", c.enableOnPathSuffix) {
		t.Error("override should have replaced the default, not appended to it")
	}
	if !matchPathSuffix("/v1/chat/completions", c.enableOnPathSuffix) {
		t.Error("configured suffix did not match")
	}
	// "*" matches everything, consistent with the other plugins in this repo.
	c2 := parse(t, `{"sessionKeys":[{"bodyKey":"sid"}],"enableOnPathSuffix":["*"]}`)
	if !matchPathSuffix("/anything", c2.enableOnPathSuffix) {
		t.Error(`"*" should match everything`)
	}
}

func TestPathSuffixMustBeArray(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"sessionKeys":[{"bodyKey":"sid"}],"enableOnPathSuffix":"/responses"}`), &c); err == nil {
		t.Error("expected an error for a non-array enableOnPathSuffix")
	}
}

// Only string-typed session keys are accepted. gjson's String() renders
// numbers and objects into plausible-looking strings too, which would let a
// field that merely happens to share the name be taken as a session key.
func TestSessionKeyFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"normal", `{"previous_response_id":"resp_68f0"}`, "resp_68f0"},
		{"field missing", `{"model":"qwen"}`, ""},
		{"empty string", `{"previous_response_id":""}`, ""},
		{"surrounding whitespace", `{"previous_response_id":"  resp_1  "}`, "resp_1"},
		{"null", `{"previous_response_id":null}`, ""},
		{"number", `{"previous_response_id":12345}`, ""},
		{"object", `{"previous_response_id":{"id":"x"}}`, ""},
		{"invalid JSON", `not json at all`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionKeyFromBody([]byte(tt.body), "previous_response_id"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// bodyKey is a gjson path, not just a top-level field name -- Anthropic's only
// candidate key, metadata.user_id, is nested, and documenting it too narrowly
// would make people think it cannot be read at all.
func TestBodyKeyAcceptsNestedPaths(t *testing.T) {
	tests := []struct{ name, body, key, want string }{
		{"OpenAI cache routing key", `{"model":"gpt","prompt_cache_key":"sess_xyz"}`, "prompt_cache_key", "sess_xyz"},
		{"Anthropic nested user key", `{"metadata":{"user_id":"u_abc"},"messages":[]}`, "metadata.user_id", "u_abc"},
		{"array index", `{"a":[{"b":"v"}]}`, "a.0.b", "v"},
		{"path absent", `{"metadata":{}}`, "metadata.user_id", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionKeyFromBody([]byte(tt.body), tt.key); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A weighted candidate set must exit early: candidates carrying a weight mean
// this route has a business split, and the finisher ignores every scoring
// opinion (design §4.2).
func TestIsWeighted(t *testing.T) {
	zero, ten := int64(0), int64(10)
	tests := []struct {
		name string
		in   []candidate
		want bool
	}{
		{"none weighted", []candidate{{Cluster: "a"}, {Cluster: "b"}}, false},
		{"all weighted", []candidate{{Cluster: "a", Weight: &ten}}, true},
		// weight: 0 is a valid value (a canary dialled to 0%) and must not be
		// treated as unset.
		{"zero weight", []candidate{{Cluster: "a", Weight: &zero}}, true},
		{"mixed", []candidate{{Cluster: "a"}, {Cluster: "b", Weight: &ten}}, true},
		{"empty set", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWeighted(tt.in); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// weight defaults to 1; <= 0 and invalid values all fall back to 1 (zero is
// not a meaningful value -- to opt out, do not install the CR, or use
// defaultConfigDisable).
func TestWeightDefaultsAndGuards(t *testing.T) {
	tests := []struct {
		raw  string
		want float64
	}{
		{`{"sessionKeys":[{"header":"x"}]}`, 1},
		{`{"sessionKeys":[{"header":"x"}],"weight":3}`, 3},
		{`{"sessionKeys":[{"header":"x"}],"weight":0}`, 1},
		{`{"sessionKeys":[{"header":"x"}],"weight":-2}`, 1},
	}
	for _, tt := range tests {
		if got := parse(t, tt.raw).weight; got != tt.want {
			t.Errorf("%s → weight = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// Deployment-level knobs have to be inheritable from defaultConfig. Without
// parseOverrideConfig, weight and enableOnPathSuffix set globally would be
// silently reset to the defaults by every matchRule.
func TestOverrideInheritsDeploymentKnobs(t *testing.T) {
	global := parse(t, `{"sessionKeys":[{"header":"g"}],"weight":4,"enableOnPathSuffix":["/custom"]}`)

	var c Config
	rule := `{"sessionKeys":[{"header":"x-session-id"}]}`
	if err := parseOverrideConfig(gjson.Parse(rule), global, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if c.weight != 4 {
		t.Errorf("weight = %v, want 4 (inherited)", c.weight)
	}
	if !matchPathSuffix("/v1/custom", c.enableOnPathSuffix) {
		t.Errorf("enableOnPathSuffix not inherited: %v", c.enableOnPathSuffix)
	}
	// sessionKeys is not inherited -- which key to read is a per-route fact.
	if len(c.sessionKeys) != 1 || c.sessionKeys[0].name != "x-session-id" {
		t.Errorf("sessionKeys should come from the rule, got %+v", c.sessionKeys)
	}
	// An explicit setting on the rule wins over inheritance.
	var c2 Config
	if err := parseOverrideConfig(gjson.Parse(`{"sessionKeys":[{"header":"x"}],"weight":9}`), global, &c2); err != nil {
		t.Fatal(err)
	}
	if c2.weight != 9 {
		t.Errorf("explicit rule weight = %v, want 9", c2.weight)
	}
}

// A zero-valued global (what rule_matcher leaves behind when the global config
// fails to parse) must not zero out the rule's deployment-level fields.
func TestZeroGlobalDoesNotWipeDefaults(t *testing.T) {
	var zeroGlobal Config
	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"sessionKeys":[{"header":"x"}]}`), zeroGlobal, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if c.weight != defaultWeight {
		t.Errorf("weight = %v, want the default %v", c.weight, defaultWeight)
	}
	if !matchPathSuffix("/v1/responses", c.enableOnPathSuffix) {
		t.Errorf("path suffixes got wiped: %v", c.enableOnPathSuffix)
	}
}

// The default path suffixes must not be a shared package-level slice -- one
// append would contaminate other configs.
func TestDefaultSuffixesAreNotShared(t *testing.T) {
	a := parse(t, `{"sessionKeys":[{"bodyKey":"k"}]}`)
	b := parse(t, `{"sessionKeys":[{"bodyKey":"k"}]}`)
	a.enableOnPathSuffix = append(a.enableOnPathSuffix, "/polluted")
	if matchPathSuffix("/polluted", b.enableOnPathSuffix) {
		t.Error("two configs share the same backing array")
	}
}

// This plugin stops iteration to buffer a body source at 780, before the
// finisher raises the decoder buffer limit at 700. Without its own limit,
// Envoy buffers under the route default (1 MiB stock, as low as 32 KiB in some
// Higress deployments) and a large /v1/messages body gets a 413 -- meaning
// installing this plugin would change which requests succeed. The default must
// therefore match gpustack-lb's maxBodyBytes.
func TestMaxBodyBytesDefaultsToTheFinishersLimit(t *testing.T) {
	c := parse(t, `{"sessionKeys":[{"bodyKey":"prompt_cache_key"}]}`)
	if c.maxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("maxBodyBytes = %d, want %d (gpustack-lb's DefaultMaxBodyBytes)", c.maxBodyBytes, defaultMaxBodyBytes)
	}
	if defaultMaxBodyBytes != 100*1024*1024 {
		t.Errorf("default drifted from gpustack-lb's 100 MiB: %d", defaultMaxBodyBytes)
	}
}

func TestMaxBodyBytesParseAndInherit(t *testing.T) {
	keys := `"sessionKeys":[{"bodyKey":"k"}]`

	if got := parse(t, `{`+keys+`,"maxBodyBytes":4096}`).maxBodyBytes; got != 4096 {
		t.Errorf("maxBodyBytes = %d, want 4096", got)
	}
	var c Config
	if err := parseConfig(gjson.Parse(`{`+keys+`,"maxBodyBytes":0}`), &c); err == nil {
		t.Error("maxBodyBytes 0 must be rejected")
	}

	// Deployment-level, so a matchRule that does not mention it inherits.
	global := parse(t, `{`+keys+`,"maxBodyBytes":8192}`)
	var rule Config
	if err := parseOverrideConfig(gjson.Parse(`{`+keys+`}`), global, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.maxBodyBytes != 8192 {
		t.Errorf("maxBodyBytes = %d, want 8192 inherited", rule.maxBodyBytes)
	}
}

// A prefix match also accepts application/jsonx, which would make this plugin
// stop iteration and buffer a body it cannot parse -- and now also raise the
// decoder buffer limit to do it. It has to agree with gpustack-lb's
// baseMediaType, or one plugin buffers for requests the other passes through.
func TestIsJSONMediaType(t *testing.T) {
	yes := []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"  application/json  ",
	}
	no := []string{
		"application/jsonx",
		"application/json-seq",
		"multipart/form-data; boundary=xyz",
		"text/plain",
		"",
		"not a media type",
	}
	for _, ct := range yes {
		if !isJSONMediaType(ct) {
			t.Errorf("isJSONMediaType(%q) = false, want true", ct)
		}
	}
	for _, ct := range no {
		if isJSONMediaType(ct) {
			t.Errorf("isJSONMediaType(%q) = true, want false", ct)
		}
	}
}
