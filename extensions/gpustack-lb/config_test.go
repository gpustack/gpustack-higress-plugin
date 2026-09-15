package main

import (
	"bytes"
	"encoding/json"
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

func TestParseConfigDefaults(t *testing.T) {
	c := parse(t, `{}`)

	if c.unhealthyThreshold != defaultUnhealthyThreshold {
		t.Errorf("unhealthyThreshold = %d", c.unhealthyThreshold)
	}
	if c.cooldownMs != defaultCooldownMs {
		t.Errorf("cooldownMs = %d", c.cooldownMs)
	}
	if c.rejectStatus != defaultRejectStatus {
		t.Errorf("rejectStatus = %d", c.rejectStatus)
	}
	if c.modelKey != "model" {
		t.Errorf("modelKey = %q", c.modelKey)
	}
	// Keep model-mapper's upstream default: after the merge this plugin also
	// plays the drop-in role on non-LB routes, and changing the default would
	// break that property. Checked: no plugin in this repo or upstream higress
	// **reads** this header, so writing one extra header is harmless.
	if c.modelToHeader != "x-higress-llm-model-final" {
		t.Errorf("modelToHeader = %q, want upstream default", c.modelToHeader)
	}
	// mode defaults to context, which is the fail-safe choice.
	if c.mode != modeContext {
		t.Errorf("mode = %q, want %q", c.mode, modeContext)
	}
	if len(c.enableOnPathSuffix) == 0 {
		t.Error("enableOnPathSuffix should fall back to the built-in list")
	}
}

// The finisher is the only writer of health state, so all three windows belong
// to it: threshold, cooldown and recovery.
func TestParseConfigOwnsAllHealthWindows(t *testing.T) {
	c := parse(t, `{"health":{"unhealthyThreshold":5,"cooldownMs":30000,"rampMs":5000},"selectors":["x"]}`)
	if c.unhealthyThreshold != 5 {
		t.Errorf("unhealthyThreshold = %d", c.unhealthyThreshold)
	}
	if c.cooldownMs != 30000 {
		t.Errorf("cooldownMs = %d", c.cooldownMs)
	}
	if c.rampMs != 5000 {
		t.Errorf("rampMs = %d", c.rampMs)
	}
}

// rampMs defaults to cooldownMs -- making the penalty window the same length
// as the cooldown is a deliberate default.
func TestRampDefaultsToCooldown(t *testing.T) {
	if c := parse(t, `{"health":{"cooldownMs":30000}}`); c.rampMs != 30000 {
		t.Errorf("rampMs = %d, want 30000", c.rampMs)
	}
}

// Both end instants are stamped into the state by this plugin at ejection
// time, so the reader never needs to know the window lengths.
func TestHealthStateCarriesBothDeadlines(t *testing.T) {
	var st healthState
	if err := json.Unmarshal([]byte(`{"f":2,"e":1000,"r":3000}`), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.EjectedUntil != 1000 || st.RampUntil != 3000 || st.Fails != 2 {
		t.Errorf("got %+v", st)
	}
}

func TestParseConfigRejectsBadMaxBodyBytes(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"maxBodyBytes":0}`), &c); err == nil {
		t.Error("expected error for maxBodyBytes = 0")
	}
	if err := parseConfig(gjson.Parse(`{"enableOnPathSuffix":"nope"}`), &c); err == nil {
		t.Error("expected error for non-array enableOnPathSuffix")
	}
}

// The reject body must not be empty: Envoy's local-reply path treats an empty
// body as a degenerate response, queuing it without ever flushing (the client
// hangs until its own timeout).
func TestRejectBodyIsNeverEmptyAndIsJSON(t *testing.T) {
	for _, msg := range []string{"", "boom", "  "} {
		body := buildRejectBody(msg, 503)
		if len(body) == 0 {
			t.Fatalf("empty reject body for %q", msg)
		}
		if !json.Valid(body) {
			t.Fatalf("reject body for %q is not valid JSON: %s", msg, body)
		}
	}
}

// A message that is already a JSON object passes through verbatim, so callers
// can supply their own error envelope.
func TestRejectBodyPassesThroughJSONObject(t *testing.T) {
	raw := `{"error":{"message":"custom"}}`
	if got := string(buildRejectBody(raw, 503)); got != raw {
		t.Errorf("got %s, want %s", got, raw)
	}
}

func TestPruneStartsDropsLeakedEntries(t *testing.T) {
	const now = 1_000_000
	const maxAge = 1000

	got := pruneStarts([]int64{now - 5000, now - 500, now - 999, now - 1000}, now, maxAge)
	// Both now-5000 and now-1000 are too old (the test is now-ts < maxAge,
	// strictly less than).
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 survivors", got)
	}
	for _, ts := range got {
		if now-ts >= maxAge {
			t.Fatalf("kept a leaked entry: %d", ts)
		}
	}
}

// probation is published by the context role along with the candidate set; the
// finisher only tightens the threshold based on it. This pins down that the
// wire struct actually carries the flag.
func TestProbationFlagRoundTrips(t *testing.T) {
	raw := `{"candidates":[
		{"cluster":"a","kind":"instance","inflight":0,"penalty":3.5,"probation":true},
		{"cluster":"b","kind":"instance","inflight":2,"penalty":0}
	]}`
	var set CandidateSet
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !set.Candidates[0].Probation {
		t.Error("candidate a should be in probation")
	}
	if set.Candidates[1].Probation {
		t.Error("candidate b should not be in probation")
	}
}

// response flags come back as a little-endian integer, not decimal text.
func TestLeUint(t *testing.T) {
	// 16793600 = NoClusterFound(bit 24) | DownstreamConnectionTermination(bit 14)
	if got := leUint([]byte{0x00, 0x40, 0x00, 0x01}); got != 16793600 {
		t.Errorf("got %d, want 16793600", got)
	}
	if got := leUint(nil); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// Which flags count as a health failure is somewhere people change on
// intuition and get wrong, so pin it down.
func TestConnectivityFailureMask(t *testing.T) {
	included := map[string]uint64{
		"UH NoHealthyUpstream":             flagNoHealthyUpstream,
		"UF UpstreamConnectionFailure":     flagUpstreamConnectionFailure,
		"UC UpstreamConnectionTermination": flagUpstreamConnectionTermination,
		"NC NoClusterFound":                flagNoClusterFound,
	}
	for name, bit := range included {
		if connectivityFailureMask&bit == 0 {
			t.Errorf("%s should count as a connectivity failure", name)
		}
	}

	// UT (UpstreamRequestTimeout, bit 2) is deliberately excluded: the AI
	// route's own timeout is 0s, so that flag most likely comes from a
	// cluster-level timeout, and counting it would eject instances during
	// perfectly normal long generations.
	const flagUpstreamRequestTimeout = 1 << 2
	if connectivityFailureMask&flagUpstreamRequestTimeout != 0 {
		t.Error("UT must NOT count as a connectivity failure")
	}
	// DownstreamConnectionTermination (bit 14) is the client disconnecting and
	// says nothing about instance health.
	const flagDownstreamConnectionTermination = 1 << 14
	if connectivityFailureMask&flagDownstreamConnectionTermination != 0 {
		t.Error("DC must NOT count as a connectivity failure")
	}
}

// mode accepts exactly two values; everything else (including empty) falls
// back to context -- defaulting to finisher would put a plugin on every route
// trying to write a cluster header.
func TestModeFallsBackToContext(t *testing.T) {
	tests := []struct {
		raw       string
		want      string
		wantKnown bool
	}{
		{"", modeContext, true},
		{"context", modeContext, true},
		{"finisher", modeFinisher, true},
		{"nonsense", modeContext, false}, // unrecognised also falls back, and must be reportable
	}
	for _, tt := range tests {
		got, known := normalizeMode(tt.raw)
		if got != tt.want || known != tt.wantKnown {
			t.Errorf("normalizeMode(%q) = (%q,%v), want (%q,%v)",
				tt.raw, got, known, tt.want, tt.wantKnown)
		}
	}
	if got := parse(t, `{"mode":"finisher"}`).mode; got != modeFinisher {
		t.Errorf("mode = %q, want finisher", got)
	}
}

// modelMappers is grouped by target, and within a group uses model-mapper's
// key syntax.
func TestResolveCandidateModel(t *testing.T) {
	c := parse(t, `{"candidates":[{"cluster":"a","targetId":"1"}],"modelMappers":{
		"1": {"gpt-4o":"qwen3-32b", "gpt-*":"qwen3-8b", "*":"fallback"},
		"2": {"claude-*":"gpt-4o"}
	}}`)

	tests := []struct{ target, client, want string }{
		{"1", "gpt-4o", "qwen3-32b"},  // exact wins
		{"1", "gpt-5", "qwen3-8b"},    // prefix
		{"1", "whatever", "fallback"}, // "*" catch-all
		{"2", "claude-3", "gpt-4o"},   // a different group, independent
		{"2", "gpt-4o", ""},           // no match in the group and no "*" -> no rewrite
		{"3", "gpt-4o", ""},           // no mapping group for this target -> no rewrite
		{"", "gpt-4o", ""},            // candidate has no targetId -> no rewrite
	}
	for _, tt := range tests {
		if got := resolveCandidateModel(c, tt.target, tt.client); got != tt.want {
			t.Errorf("resolveCandidateModel(%q,%q) = %q, want %q", tt.target, tt.client, got, tt.want)
		}
	}
}

// "First matching prefix" is not "longest match": lexicographically
// '*'(0x2A) < 'c', so legacy-* sorts before legacy-coder-* and hits first.
// This is upstream higress's existing behaviour.
func TestPrefixIsFirstMatchNotLongest(t *testing.T) {
	c := parse(t, `{"candidates":[{"cluster":"a","targetId":"1"}],"modelMappers":{
		"1": {"legacy-*":"short", "legacy-coder-*":"long"}
	}}`)
	if got := resolveCandidateModel(c, "1", "legacy-coder-7b"); got != "short" {
		t.Errorf("got %q, want short -- longest match is not this implementation's semantic", got)
	}
}

// -- health.failOpen: the global config location, and inheritance.

// failOpen has to be configurable in defaultConfig -- the one without any
// candidates. It used to be buried inside parseLB, which returns early when
// there are no candidates, so the global config location was never parsed and
// setting it there did nothing at all.
func TestFailOpenParsedWithoutCandidates(t *testing.T) {
	if got := parse(t, `{"mode":"context","health":{"failOpen":false}}`).shouldFailOpen(); got {
		t.Error("failOpen:false in a candidate-less config must be honoured")
	}
	if got := parse(t, `{"mode":"context"}`).shouldFailOpen(); !got {
		t.Error("failOpen should default to true")
	}
	// It applies with candidates too (regression guard for the old path).
	if got := parse(t, `{"candidates":[{"cluster":"a"}],"health":{"failOpen":false}}`).shouldFailOpen(); got {
		t.Error("failOpen:false alongside candidates must be honoured")
	}
}

// "Unset" and "explicitly false" must be distinguishable, or the inheritance
// logic has no way to decide whether to override.
func TestFailOpenPresenceIsDistinguishable(t *testing.T) {
	if p := parse(t, `{}`).failOpen; p != nil {
		t.Errorf("unset failOpen should stay nil, got %v", *p)
	}
	if p := parse(t, `{"health":{"failOpen":true}}`).failOpen; p == nil || !*p {
		t.Errorf("explicit true should survive as a real true, got %v", p)
	}
}

func TestOverrideInheritsDeploymentKnobs(t *testing.T) {
	global := parse(t, `{"mode":"finisher","health":{"failOpen":false}}`)

	tests := []struct {
		name     string
		rule     string
		wantMode string
		wantOpen bool
	}{
		{"rule silent, inherits", `{"candidates":[{"cluster":"a"}]}`, modeFinisher, false},
		{"rule overrides explicitly", `{"candidates":[{"cluster":"a"}],"health":{"failOpen":true}}`, modeFinisher, true},
		{"rule can override mode", `{"mode":"context","candidates":[{"cluster":"a"}]}`, modeContext, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := parseOverrideConfig(gjson.Parse(tt.rule), global, &c); err != nil {
				t.Fatalf("parseOverrideConfig: %v", err)
			}
			if c.mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", c.mode, tt.wantMode)
			}
			if got := c.shouldFailOpen(); got != tt.wantOpen {
				t.Errorf("shouldFailOpen = %v, want %v", got, tt.wantOpen)
			}
		})
	}
}

// Topology **must not** be inherited. candidates in defaultConfig would turn
// every rule into an LB route, and modelMapping would silently rewrite model
// names on all of them. Both are per-route facts, and inheriting them is
// dangerous rather than convenient.
func TestOverrideDoesNotInheritTopology(t *testing.T) {
	global := parse(t, `{
		"mode":"context",
		"candidates":[{"cluster":"outbound|80||global.static"}],
		"modelMappers":{"9":{"*":"leaked"}},
		"modelMapping":{"*":"leaked-legacy"}
	}`)

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"modelKey":"model"}`), global, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if len(c.candidates) != 0 {
		t.Errorf("candidates leaked from global: %+v", c.candidates)
	}
	if c.lbMode {
		t.Error("rule became an LB route purely by inheriting global candidates")
	}
	if len(c.modelMappers) != 0 {
		t.Errorf("modelMappers leaked from global: %+v", c.modelMappers)
	}
	if got := resolveModel(c, "anything"); got != "anything" {
		t.Errorf("modelMapping leaked from global: resolveModel = %q", got)
	}
}

// When the global config fails to parse, rule_matcher **swallows the error and
// leaves globalConfig zero-valued**, so inheritance receives a zero Config.
// failOpen is a pointer for exactly this moment: the zero value's nil is
// inherited as nil and read as the default true, rather than silently flipping
// to fail-closed.
func TestOverrideWithZeroGlobalStaysFailOpen(t *testing.T) {
	var zeroGlobal Config // simulates m.globalConfig after a global parse failure

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), zeroGlobal, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if !c.shouldFailOpen() {
		t.Error("a zero-valued global must not flip rules to fail-closed")
	}
	if c.mode != modeContext {
		t.Errorf("mode = %q, want the parsed default %q (not global's empty string)", c.mode, modeContext)
	}
}

// -- Inheritance of deployment-level config.

// Besides mode and failOpen, the health windows, the reject body and the body
// parameters are **deployment-level** too. Without inheritance, a CR that
// configures them in defaultConfig and then adds any matchRule would silently
// run the matched routes on the built-in defaults.
func TestOverrideInheritsAllDeploymentKnobs(t *testing.T) {
	global := parse(t, `{
		"mode":"finisher",
		"health":{"unhealthyThreshold":9,"cooldownMs":60000,"rampMs":30000,"failOpen":false},
		"maxInflightAgeMs":123456,
		"reject":{"status":429,"message":"slow down"},
		"modelKey":"the_model",
		"modelToHeader":"x-custom",
		"maxBodyBytes":4096,
		"enableOnPathSuffix":["/only-here"]
	}`)

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), global, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"mode", c.mode, modeFinisher},
		{"unhealthyThreshold", c.unhealthyThreshold, int64(9)},
		{"cooldownMs", c.cooldownMs, int64(60000)},
		{"rampMs", c.rampMs, int64(30000)},
		{"maxInflightAgeMs", c.maxInflightAgeMs, int64(123456)},
		{"rejectStatus", c.rejectStatus, int64(429)},
		{"modelKey", c.modelKey, "the_model"},
		{"modelToHeader", c.modelToHeader, "x-custom"},
		{"maxBodyBytes", c.maxBodyBytes, uint32(4096)},
		{"failOpen", c.shouldFailOpen(), false},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if len(c.enableOnPathSuffix) != 1 || c.enableOnPathSuffix[0] != "/only-here" {
		t.Errorf("enableOnPathSuffix not inherited: %v", c.enableOnPathSuffix)
	}
	if !bytes.Contains(c.rejectBody, []byte("slow down")) {
		t.Errorf("reject.message not inherited: %s", c.rejectBody)
	}
}

// rampMs defaults to cooldownMs, so it has to be inherited together with
// cooldownMs -- otherwise, when the global sets cooldownMs and the rule sets no
// rampMs, the rule would get the value derived from **its own** cooldownMs and
// the two would disagree.
func TestRampMsFollowsInheritedCooldown(t *testing.T) {
	global := parse(t, `{"health":{"cooldownMs":60000}}`)
	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{}`), global, &c); err != nil {
		t.Fatal(err)
	}
	if c.cooldownMs != 60000 || c.rampMs != 60000 {
		t.Errorf("cooldownMs=%d rampMs=%d, want both 60000", c.cooldownMs, c.rampMs)
	}
}

// Topology is never inherited: candidates in defaultConfig would turn every
// rule into an LB route.
func TestOverrideStillDoesNotInheritTopology(t *testing.T) {
	global := parse(t, `{
		"candidates":[{"cluster":"outbound|80||global.static"}],
		"modelMappers":{"9":{"*":"leaked"}},
		"modelMapping":{"*":"leaked-legacy"}
	}`)
	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"modelKey":"model"}`), global, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.candidates) != 0 || c.lbMode {
		t.Errorf("candidates leaked: %+v", c.candidates)
	}
	if len(c.modelMappers) != 0 {
		t.Errorf("modelMappers leaked: %+v", c.modelMappers)
	}
	if got := resolveModel(c, "anything"); got != "anything" {
		t.Errorf("modelMapping leaked: %q", got)
	}
}

// A zero-valued global (what rule_matcher leaves after a global parse failure)
// must not zero out any field.
func TestZeroGlobalKeepsParsedDefaults(t *testing.T) {
	var zeroGlobal Config
	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{}`), zeroGlobal, &c); err != nil {
		t.Fatal(err)
	}
	if c.unhealthyThreshold != defaultUnhealthyThreshold ||
		c.cooldownMs != defaultCooldownMs ||
		c.rampMs != defaultCooldownMs ||
		c.maxInflightAgeMs != defaultMaxInflightAgeMs ||
		c.rejectStatus != defaultRejectStatus ||
		c.modelKey != "model" ||
		c.maxBodyBytes != DefaultMaxBodyBytes ||
		!c.shouldFailOpen() {
		t.Errorf("a zero global wiped parsed defaults: %+v", c)
	}
	if len(c.enableOnPathSuffix) == 0 {
		t.Error("enableOnPathSuffix got wiped")
	}
}

// reject.status is converted to uint32, and 4294967496 truncates to exactly
// 200 -- a rejection that should have been fail-closed turns into a success
// response. The range has to be enforced.
//
// The decision goes through a pure function: parseConfig calls LogWarnf on the
// out-of-range branch, and host ABI calls panic outside a wasm host.
func TestRejectStatusRange(t *testing.T) {
	tests := []struct {
		raw    int64
		want   int64
		wantOK bool
	}{
		{0, defaultRejectStatus, true}, // unset, not out of range
		{429, 429, true},
		{503, 503, true},
		{4294967496, defaultRejectStatus, false}, // truncates to 200 as uint32
		{700, defaultRejectStatus, false},
		{-1, defaultRejectStatus, false},
		{99, defaultRejectStatus, false},
	}
	for _, tt := range tests {
		got, ok := normalizeRejectStatus(tt.raw)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("normalizeRejectStatus(%d) = (%d,%v), want (%d,%v)",
				tt.raw, got, ok, tt.want, tt.wantOK)
		}
	}
	// End to end: a valid value must land in the config.
	if got := parse(t, `{"reject":{"status":429}}`).rejectStatus; got != 429 {
		t.Errorf("rejectStatus = %d, want 429", got)
	}
}

// Parsing must be idempotent: with rule-level isolation enabled, rule_matcher
// re-runs the parse on the same &rule.config, and appending without clearing
// would duplicate candidates and double the weighted intervals.
func TestParseIsIdempotentOnReusedConfig(t *testing.T) {
	var c Config
	raw := `{"candidates":[{"cluster":"a","weight":1},{"cluster":"b","weight":1}],
	         "enableOnPathSuffix":["/x"]}`
	for i := 0; i < 3; i++ {
		if err := parseConfig(gjson.Parse(raw), &c); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if len(c.candidates) != 2 {
			t.Fatalf("round %d: %d candidates, want 2", i, len(c.candidates))
		}
		if len(c.enableOnPathSuffix) != 1 {
			t.Fatalf("round %d: %d suffixes, want 1", i, len(c.enableOnPathSuffix))
		}
	}
}

// rule_matcher retries a failed rule parse against a backup JSON using the
// **same** &rule.config, so a second parse starts on top of whatever the first
// one managed to write. Every field that is only written when its key is
// present must therefore be cleared first -- these are the ones that change
// routing.
func TestReparseDropsStaleFields(t *testing.T) {
	var c Config

	lb := `{"mode":"finisher",
	        "candidates":[{"cluster":"a"}],
	        "modelMappers":{"1":{"*":"m"}},
	        "modelMapping":{"*":"fallback-model"},
	        "health":{"failOpen":false}}`
	if err := parseConfig(gjson.Parse(lb), &c); err != nil {
		t.Fatal(err)
	}
	if !c.lbMode || c.defaultModel != "fallback-model" || c.shouldFailOpen() {
		t.Fatalf("precondition failed: %+v", c)
	}

	// The backup config mentions none of those keys. Each one left behind would
	// be a live misbehaviour: a stale defaultModel silently rewrites every
	// request, stale candidates turn a plain route into an LB route, and a
	// stale failOpen flips it to fail-closed.
	if err := parseConfig(gjson.Parse(`{}`), &c); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"mode", c.mode, modeContext},
		{"lbMode", c.lbMode, false},
		{"candidates", len(c.candidates), 0},
		{"modelMappers", len(c.modelMappers), 0},
		{"defaultModel", c.defaultModel, ""},
		{"failOpen", c.shouldFailOpen(), defaultFailOpen},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v (stale value survived the reparse)", ch.name, ch.got, ch.want)
		}
	}
}

// The reject body embeds the status code, so a rule that overrides only
// reject.status must not inherit the global body -- the response would go out
// with one status while its JSON reported another.
func TestRejectBodyRebuiltWhenOnlyStatusOverridden(t *testing.T) {
	global := parse(t, `{"reject":{"status":503,"message":"global message"}}`)

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"reject":{"status":429}}`), global, &c); err != nil {
		t.Fatal(err)
	}
	if c.rejectStatus != 429 {
		t.Fatalf("rejectStatus = %d, want 429", c.rejectStatus)
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    int64  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(c.rejectBody, &body); err != nil {
		t.Fatalf("reject body is not valid JSON: %s", c.rejectBody)
	}
	if body.Error.Code != 429 {
		t.Errorf("body code = %d, want 429 to match the status actually sent", body.Error.Code)
	}
	// The message was not overridden, so it still has to be the inherited one.
	if body.Error.Message != "global message" {
		t.Errorf("message = %q, want the inherited global message", body.Error.Message)
	}
}

// The mirror case: neither field overridden, so the global body is reused
// verbatim and still agrees with the inherited status.
func TestRejectBodyInheritedWhenNeitherOverridden(t *testing.T) {
	global := parse(t, `{"reject":{"status":429,"message":"slow down"}}`)

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{}`), global, &c); err != nil {
		t.Fatal(err)
	}
	if c.rejectStatus != 429 {
		t.Errorf("rejectStatus = %d, want 429", c.rejectStatus)
	}
	if !bytes.Contains(c.rejectBody, []byte("slow down")) {
		t.Errorf("message not inherited: %s", c.rejectBody)
	}
	if !bytes.Contains(c.rejectBody, []byte("429")) {
		t.Errorf("body code should still be 429: %s", c.rejectBody)
	}
}

// kind exists to keep passive health ejection away from a provider's shared DNS
// cluster. Folding every unrecognised value into "instance" would turn it back
// on for a typo, silently, so an unknown kind is an error.
func TestUnknownCandidateKindIsRejected(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","kind":"provder"}]}`), &c); err == nil {
		t.Error("a misspelled kind must be rejected, not silently treated as an instance")
	}
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a"}]}`,
		`{"candidates":[{"cluster":"a","kind":"instance"}]}`,
		`{"candidates":[{"cluster":"a","kind":"provider"}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err != nil {
			t.Errorf("%s should parse: %v", raw, err)
		}
	}
	if got := parse(t, `{"candidates":[{"cluster":"a"}]}`).candidates[0].Kind; got != KindInstance {
		t.Errorf("an absent kind should default to %q, got %q", KindInstance, got)
	}
}

// selectWeighted skips non-positive weights when summing, so a negative weight
// would leave the candidate unselectable while still forcing the whole set into
// weighted mode -- indistinguishable from "the canary gets no traffic for some
// reason". weight: 0 stays legal; that is a canary dialled down to 0%.
func TestNegativeCandidateWeightIsRejected(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","weight":-1}]}`), &c); err == nil {
		t.Error("a negative weight must be rejected")
	}
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","weight":0}]}`), &c); err != nil {
		t.Errorf("weight 0 is meaningful and must stay legal: %v", err)
	}
}

// Weights that sum past int64 are rejected at config load. selectWeighted has a
// silent backstop for the same case, but a pure function cannot log, so this is
// the only place that can tell the operator what is wrong.
func TestOverflowingCandidateWeightsAreRejected(t *testing.T) {
	var c Config
	raw := `{"candidates":[
	          {"cluster":"a","weight":9223372036854775807},
	          {"cluster":"b","weight":9223372036854775807}]}`
	if err := parseConfig(gjson.Parse(raw), &c); err == nil {
		t.Error("weights summing beyond int64 must be rejected at parse time")
	}
}

// Several candidates on one cluster with different targetIds is gpustack's
// several-model-names-on-one-route case and must stay legal. Two entries
// sharing *both* fields are genuinely indistinguishable downstream, so those
// are rejected.
func TestDuplicateCandidateIdentityIsRejected(t *testing.T) {
	var c Config

	same := `{"candidates":[{"cluster":"X","targetId":"2"},{"cluster":"X","targetId":"3"}]}`
	if err := parseConfig(gjson.Parse(same), &c); err != nil {
		t.Errorf("one cluster with two targets must stay legal: %v", err)
	}
	if len(c.candidates) != 2 {
		t.Errorf("got %d candidates, want 2", len(c.candidates))
	}

	dup := `{"candidates":[{"cluster":"X","targetId":"2"},{"cluster":"X","targetId":"2"}]}`
	if err := parseConfig(gjson.Parse(dup), &c); err == nil {
		t.Error("two candidates with the same cluster and targetId must be rejected")
	}
}

// Whether `candidates` is present decides whether the route does LB at all, so
// a malformed value must not read as "absent" -- that would drop the route into
// legacy model-mapper handling with LB silently off.
func TestMalformedCandidatesIsRejected(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"candidates":{}}`,
		`{"candidates":"outbound|80||x"}`,
		`{"candidates":5}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s should be rejected, not silently treated as no-LB", raw)
		}
	}

	// Absent, an explicit null, and an empty array all legitimately mean "no
	// LB on this route". null in particular must not error: gjson reports
	// Exists() for a null literal.
	for _, raw := range []string{`{}`, `{"candidates":null}`, `{"candidates":[]}`} {
		if err := parseConfig(gjson.Parse(raw), &c); err != nil {
			t.Errorf("%s should parse as no-LB: %v", raw, err)
		}
		if c.lbMode {
			t.Errorf("%s should leave lbMode false", raw)
		}
	}
}

// 0 means unlimited; a negative value is a typo. The filter tests maxRun > 0,
// so -1 would silently lift the cap on the very candidate being capped.
func TestNegativeMaxRunningRequestsIsRejected(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","maxRunningRequests":-1}]}`), &c); err == nil {
		t.Error("a negative maxRunningRequests must be rejected")
	}
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","maxRunningRequests":0}]}`), &c); err != nil {
		t.Errorf("0 means unlimited and must stay legal: %v", err)
	}
}

// sort.Slice is not stable and weighted intervals are order-sensitive, so
// candidates sharing a cluster need a deterministic tiebreak -- otherwise a
// reparse can move a given request id onto a different model target.
func TestCandidateOrderIsDeterministicWithinACluster(t *testing.T) {
	// Declared in an order that does not match the sorted one, so a comparator
	// that ignores targetId has something to get wrong.
	raw := `{"candidates":[
	          {"cluster":"X","targetId":"9"},
	          {"cluster":"A","targetId":"1"},
	          {"cluster":"X","targetId":"3"},
	          {"cluster":"X","targetId":"1"}]}`

	var want []string
	for round := 0; round < 20; round++ {
		var c Config
		if err := parseConfig(gjson.Parse(raw), &c); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, cand := range c.candidates {
			got = append(got, cand.Cluster+"/"+cand.TargetID)
		}
		if round == 0 {
			want = got
			expect := []string{"A/1", "X/1", "X/3", "X/9"}
			for i := range expect {
				if got[i] != expect[i] {
					t.Fatalf("order = %v, want %v", got, expect)
				}
			}
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("round %d: order changed from %v to %v", round, want, got)
			}
		}
	}
}

// A reject path answering 2xx or 3xx is not a rejection. 200 hands the caller a
// success carrying an error envelope -- the very outcome the overflow guard
// exists to prevent, just reached through configuration instead of truncation
// -- and 204 is defined to carry no body, so Envoy drops the non-empty body
// §7.3 requires in order to flush at all.
func TestRejectStatusMustBeAnErrorCode(t *testing.T) {
	for _, raw := range []int64{200, 201, 204, 301, 302, 399} {
		if got, ok := normalizeRejectStatus(raw); ok || got != defaultRejectStatus {
			t.Errorf("normalizeRejectStatus(%d) = (%d,%v), want the default and not-ok", raw, got, ok)
		}
	}
	for _, raw := range []int64{400, 429, 503, 599} {
		if got, ok := normalizeRejectStatus(raw); !ok || got != raw {
			t.Errorf("normalizeRejectStatus(%d) = (%d,%v), want it accepted", raw, got, ok)
		}
	}
	// No end-to-end case here: parseConfig LogWarnf's an out-of-range status,
	// and a host ABI call panics outside a wasm host. That is the whole reason
	// normalizeRejectStatus is a separate pure function.
}

// 0 means unlimited, so maxRunningRequests cannot lean on a positivity check
// the way maxBodyBytes does: gjson renders any non-number as 0, which would
// silently lift the cap.
func TestNonNumericMaxRunningRequestsIsRejected(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","maxRunningRequests":"oops"}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":"8"}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":true}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":{}}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected, not read as unlimited", raw)
		}
	}
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","maxRunningRequests":8}]}`), &c); err != nil {
		t.Errorf("a plain number must parse: %v", err)
	}
	if c.candidates[0].maxRunningRequests != 8 {
		t.Errorf("maxRunningRequests = %d, want 8", c.candidates[0].maxRunningRequests)
	}
}

// gjson's Bool() coerces a non-boolean to false, and for failOpen false is a
// meaningful setting rather than a default -- so a typo would flip the route
// from fail-open to fail-closed, which is precisely the distinction the pointer
// on failOpen exists to preserve.
func TestFailOpenMustBeAStrictBoolean(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"health":{"failOpen":"oops"}}`,
		`{"health":{"failOpen":"false"}}`,
		`{"health":{"failOpen":0}}`,
		`{"health":{"failOpen":[]}}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected, not read as an explicit false", raw)
		}
	}

	// A null literal reads as unset, not as false -- gjson reports Exists()
	// for it, so without the check it would look like a deliberate disable.
	if err := parseConfig(gjson.Parse(`{"health":{"failOpen":null}}`), &c); err != nil {
		t.Fatalf("null should be treated as unset: %v", err)
	}
	if c.failOpen != nil {
		t.Errorf("null should leave failOpen unset, got %v", *c.failOpen)
	}
	if !c.shouldFailOpen() {
		t.Error("unset must read as the default, true")
	}

	// Real booleans still work.
	if err := parseConfig(gjson.Parse(`{"health":{"failOpen":false}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.shouldFailOpen() {
		t.Error("an explicit false must be honoured")
	}
}

// Same coercion trap as maxRunningRequests: 0 is a meaningful weight (a canary
// at 0%), so a malformed value cannot fall back to it. `weight: "oops"` would
// mark the candidate explicitly weighted-zero -- unreachable -- and drag the
// whole set into weighted mode, discarding every scoring opinion.
func TestNonNumericCandidateWeightIsRejected(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","weight":"oops"}]}`,
		`{"candidates":[{"cluster":"a","weight":"10"}]}`,
		`{"candidates":[{"cluster":"a","weight":true}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
	// null means "no weight on this candidate", i.e. the scored path.
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","weight":null}]}`), &c); err != nil {
		t.Fatalf("null weight should mean unweighted: %v", err)
	}
	if c.candidates[0].Weight != nil {
		t.Error("null weight must leave the candidate unweighted")
	}
}

// A mis-typed modelMappers, or a mis-typed group inside it, must not be skipped
// silently: the candidates it covers would stop rewriting their model name and
// reach an instance that does not serve it.
func TestMalformedModelMappersIsRejected(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a"}],"modelMappers":"oops"}`,
		`{"candidates":[{"cluster":"a"}],"modelMappers":[]}`,
		`{"candidates":[{"cluster":"a"}],"modelMappers":{"2":"oops"}}`,
		`{"candidates":[{"cluster":"a"}],"modelMappers":{"2":["gpt"]}}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
	ok := `{"candidates":[{"cluster":"a","targetId":"2"}],"modelMappers":{"2":{"gpt":"qwen"}}}`
	if err := parseConfig(gjson.Parse(ok), &c); err != nil {
		t.Errorf("a well-formed modelMappers must parse: %v", err)
	}
	if got := resolveCandidateModel(c, "2", "gpt"); got != "qwen" {
		t.Errorf("resolveCandidateModel = %q, want qwen", got)
	}
}

// gjson's Int() truncates toward zero rather than failing, and that is the
// dangerous direction here: 0 is meaningful for both of these fields, so a
// fractional value below 1 lands on "explicitly weighted zero" (candidate
// unreachable, whole set forced into weighted mode) or on "unlimited".
//
// Truncation also slips past a plain negative check -- -0.5 becomes 0, which is
// not < 0 -- so it has to be caught before the conversion.
func TestFractionalNumbersAreRejected(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","weight":1.5}]}`,
		`{"candidates":[{"cluster":"a","weight":0.5}]}`,
		`{"candidates":[{"cluster":"a","weight":-0.5}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":2.5}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":0.9}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected rather than truncated", raw)
		}
	}

	// An integral value is fine however the YAML rendered it.
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","weight":2.0}]}`,
		`{"candidates":[{"cluster":"a","weight":1e3}]}`,
		`{"candidates":[{"cluster":"a","maxRunningRequests":8.0}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err != nil {
			t.Errorf("%s should parse: %v", raw, err)
		}
	}
	if c.candidates[0].maxRunningRequests != 8 {
		t.Errorf("maxRunningRequests = %d, want 8", c.candidates[0].maxRunningRequests)
	}

	// null still means unset on both.
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a","maxRunningRequests":null}]}`), &c); err != nil {
		t.Errorf("null maxRunningRequests should mean unset: %v", err)
	}
	if c.candidates[0].maxRunningRequests != 0 {
		t.Errorf("null should leave maxRunningRequests at 0 (unlimited), got %d", c.candidates[0].maxRunningRequests)
	}
}
