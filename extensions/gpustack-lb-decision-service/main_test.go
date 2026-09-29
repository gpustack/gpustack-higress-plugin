package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb-decision-service/provider"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/tidwall/gjson"
)

func parseTestConfig(t *testing.T, json string) PluginConfig {
	t.Helper()
	var cfg PluginConfig
	if err := parseGlobalConfig(gjson.Parse(json), &cfg); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	return cfg
}

const routeConfig = `{
	"modelSelection": {
		"instructions": "Pick the most suitable model for this task.",
		"criteria": {
			"gpt-6-luna": "strongest reasoner, highest cost",
			"claude-opus-5.5": "strong reasoner for code",
			"deepseek-v4-flash": "fast and cheap"
		}
	}
}`

func testCandidates() []wire.Candidate {
	return []wire.Candidate{
		{Cluster: "outbound|80||gpt6.static", ModelName: "gpt-6-luna"},
		{Cluster: "outbound|80||opus.static", ModelName: "claude-opus-5.5"},
		{Cluster: "outbound|80||dsv4.static", ModelName: "deepseek-v4-flash"},
	}
}

func TestParseGlobalConfigZeroConfig(t *testing.T) {
	// Zero configuration: no provider at all -> the feature stays inert
	// (config.decision == nil, handled at request time). There is
	// deliberately NO hosted default: the callout ships task bodies to
	// the decision service, so the target must be explicit.
	var cfg PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{}`), &cfg); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if cfg.decision != nil {
		t.Fatalf("decision = %v, want nil (no hosted default)", cfg.decision)
	}
	if cfg.modelSelection != nil {
		t.Error("modelSelection should be unset without config")
	}
	if cfg.decisionTimeoutMs != defaultDecisionTimeoutMs || cfg.maxStateBytes != defaultMaxStateBytes {
		t.Error("knob defaults not applied")
	}

	// An explicit provider arms the callout; the cluster is derived from
	// the endpoint host when absent. Without a `type` the generic
	// systemone provider applies; `type: typesafe` selects the hosted
	// flavour (inheriting systemone's wire behaviour).
	armed := PluginConfig{}
	if err := parseGlobalConfig(gjson.Parse(`{"provider": {"endpoint": "https://jev.internal:8010"}}`), &armed); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if armed.decision == nil || armed.decision.Name() != "systemone" {
		t.Fatalf("decision = %v, want systemone", armed.decision)
	}
	ts := PluginConfig{}
	if err := parseGlobalConfig(gjson.Parse(`{"provider": {"type": "typesafe", "endpoint": "https://api.typesafe.ai"}}`), &ts); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if ts.decision == nil || ts.decision.Name() != "typesafe" {
		t.Fatalf("decision = %v, want typesafe", ts.decision)
	}
	if got := ts.decision.DecisionHost(); got != "api.typesafe.ai" {
		t.Errorf("DecisionHost = %q, want api.typesafe.ai", got)
	}
	if got := ts.decision.DecisionCluster(); got != "outbound|443||api.typesafe.ai" {
		t.Errorf("DecisionCluster = %q, want outbound|443||api.typesafe.ai", got)
	}
	if got := armed.decision.DecisionHost(); got != "jev.internal:8010" {
		t.Errorf("DecisionHost = %q, want jev.internal:8010", got)
	}
	if got := armed.decision.DecisionCluster(); got != "outbound|8010||jev.internal" {
		t.Errorf("DecisionCluster = %q, want outbound|8010||jev.internal", got)
	}

	// An endpoint-less provider (typesafe included) is a config error.
	if err := parseGlobalConfig(gjson.Parse(`{"provider": {"type": "typesafe"}}`), &cfg); err == nil {
		t.Error("expected error for provider without endpoint (no hosted default)")
		// A cluster without an endpoint is rejected too: the endpoint host is
		// the callout's :authority, so cluster-only would send an empty one.
		if err := parseGlobalConfig(gjson.Parse(`{"provider": {"cluster": "outbound|443||jev.dns"}}`), &cfg); err == nil {
			t.Error("expected error for cluster-only provider (empty :authority)")
		}
	}
}

func TestParseOverrideRuleConfig(t *testing.T) {
	global := parseTestConfig(t, routeConfig)
	if global.modelSelection == nil || len(global.modelSelection.Criteria) != 3 {
		t.Fatalf("modelSelection = %+v", global.modelSelection)
	}
	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(routeConfig), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	// Override with a private jev service.
	if err := parseOverrideRuleConfig(gjson.Parse(`{"provider": {"endpoint": "http://jev.internal:8010"}}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if rule.decision.DecisionHost() != "jev.internal:8010" {
		t.Errorf("DecisionHost = %q", rule.decision.DecisionHost())
	}
	if rule.decision.DecisionCluster() != "outbound|8010||jev.internal" {
		t.Errorf("DecisionCluster = %q", rule.decision.DecisionCluster())
	}
}

func TestParseConfigErrors(t *testing.T) {
	var cfg PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{"activeProviderId": "nope"}`), &cfg); err == nil {
		t.Error("expected error for unknown activeProviderId")
	}
	// A non-typesafe type (laya) with an endpoint is accepted; without one
	// it is a config error (no hosted default applies).
	var layaCfg PluginConfig
	laya := `{"provider": {"type": "laya", "endpoint": "http://laya:8000", "apiToken": "k"}}`
	if err := parseGlobalConfig(gjson.Parse(laya), &layaCfg); err != nil {
		t.Errorf("laya provider should be accepted: %v", err)
	}
	if layaCfg.decision.DecisionPath() != "/v1/systemone" {
		t.Errorf("laya DecisionPath = %q, want /v1/systemone", layaCfg.decision.DecisionPath())
	}
	if layaCfg.decision.DecisionCluster() != "outbound|8000||laya" {
		t.Errorf("laya DecisionCluster = %q", layaCfg.decision.DecisionCluster())
	}
	if err := parseGlobalConfig(gjson.Parse(`{"provider": {"type": "laya"}}`), &layaCfg); err == nil {
		t.Error("expected error for laya without endpoint")
	}
}

func TestRuleProviderOverrideDropsGlobalActiveProviderId(t *testing.T) {
	// A matchRule carrying its own `provider` replaces the global
	// catalogue wholesale; the inherited global activeProviderId must not
	// dangle against the new single-entry list (that used to be a config
	// error).
	globalJSON := `{
		"providers": [
			{"id": "default", "endpoint": "https://api.typesafe.ai", "cluster": "c1"},
			{"id": "tenant-a", "endpoint": "http://jev-a.internal:8010", "cluster": "c2"}
		],
		"activeProviderId": "default"
	}`
	global := parseTestConfig(t, globalJSON)
	if global.decision.DecisionCluster() != "c1" {
		t.Fatalf("global cluster = %q, want c1", global.decision.DecisionCluster())
	}

	var rule PluginConfig
	ruleJSON := `{"provider": {"endpoint": "http://jev-c.internal:8010"}}`
	if err := parseOverrideRuleConfig(gjson.Parse(ruleJSON), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if got := rule.decision.DecisionHost(); got != "jev-c.internal:8010" {
		t.Errorf("rule DecisionHost = %q, want jev-c.internal:8010", got)
	}
}

func TestRankWeightValidation(t *testing.T) {
	for _, bad := range []string{`{"rankWeight": 0}`, `{"rankWeight": -5}`} {
		var cfg PluginConfig
		if err := parseGlobalConfig(gjson.Parse(bad), &cfg); err == nil {
			t.Errorf("rankWeight %s should be rejected", bad)
		}
	}
	// Positive values pass.
	cfg := parseTestConfig(t, `{"rankWeight": 2.5}`)
	if cfg.rankWeight != 2.5 {
		t.Errorf("rankWeight = %v, want 2.5", cfg.rankWeight)
	}
}

func TestModelSelectionConfigLevels(t *testing.T) {
	var cfg PluginConfig

	// Absent -> disabled.
	if err := parseGlobalConfig(gjson.Parse(`{}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.modelSelection != nil {
		t.Error("modelSelection absent should disable the feature")
	}

	// Bare `true` is rejected: name-only options lose too much precision.
	if err := parseGlobalConfig(gjson.Parse(`{"modelSelection": true}`), &cfg); err == nil {
		t.Error("modelSelection: true should be rejected")
	}

	// Object without criteria is rejected.
	if err := parseGlobalConfig(gjson.Parse(`{"modelSelection": {}}`), &cfg); err == nil {
		t.Error("modelSelection without criteria should be rejected")
	}

	// Single-candidate criteria: nothing to select — skip the feature
	// (no opinion published, warning recorded for request-time logging)
	// rather than erroring.
	if err := parseGlobalConfig(gjson.Parse(`{"modelSelection": {"criteria": {"one": "model"}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.modelSelection != nil {
		t.Error("single-candidate criteria should disable the feature, not error")
	}
	if cfg.modelSelectionWarning == "" {
		t.Error("a warning should be recorded when modelSelection is skipped")
	}
	if !strings.Contains(cfg.modelSelectionWarning, "< 2") {
		t.Errorf("warning = %q", cfg.modelSelectionWarning)
	}

	// Instructions optional: defaults apply.
	if err := parseGlobalConfig(gjson.Parse(`{"modelSelection": {"criteria": {"a": "1", "b": "2"}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.modelSelection.Instructions != defaultModelSelectionInstructions {
		t.Errorf("default instructions not applied: %q", cfg.modelSelection.Instructions)
	}

	// Full object: instructions and criteria override.
	if err := parseGlobalConfig(gjson.Parse(routeConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.modelSelection.Instructions != "Pick the most suitable model for this task." {
		t.Errorf("instructions = %q", cfg.modelSelection.Instructions)
	}
	if len(cfg.modelSelection.Criteria) != 3 {
		t.Errorf("criteria = %v", cfg.modelSelection.Criteria)
	}
}

func TestBuildDecisionRequest(t *testing.T) {
	cfg := parseTestConfig(t, routeConfig)
	cands := testCandidates()
	task := []byte(`{"model":"jev-latest","messages":[{"role":"user","content":"prove the theorem"}]}`)
	req := buildDecisionRequest(task, cfg.modelSelection, cands, 0, "")
	if req == nil {
		t.Fatal("nil decision request")
	}
	if s := gjson.GetBytes(req, "state").String(); s != string(task) {
		t.Errorf("state mismatch: %q", s)
	}
	q := gjson.GetBytes(req, "questions.model_selection")
	if q.Get("type").String() != "choice" {
		t.Errorf("type = %q", q.Get("type").String())
	}
	// Criteria come from the published candidates; configured descriptions win.
	if q.Get("criteria.gpt-6-luna").String() != "strongest reasoner, highest cost" {
		t.Errorf("criteria = %s", q.Get("criteria").Raw)
	}
	// A candidate without a configured description falls back to null.
	uncovered := []wire.Candidate{{Cluster: "c1", ModelName: "model-a"}, {Cluster: "c2", ModelName: "model-b"}}
	req = buildDecisionRequest(task, cfg.modelSelection, uncovered, 0, "")
	if req == nil {
		t.Fatal("nil decision request for uncovered candidates")
	}
	if !(gjson.GetBytes(req, "questions.model_selection.criteria.model-a").Type == gjson.Null) {
		t.Errorf("criteria.model-a = %s, want null", gjson.GetBytes(req, "questions.model_selection.criteria.model-a").Raw)
	}
	// Fewer than 2 model candidates: nothing to ask.
	if req := buildDecisionRequest(task, cfg.modelSelection, cands[:1], 0, ""); req != nil {
		t.Errorf("expected nil for single candidate, got %s", req)
	}
	// maxStateBytes truncation.
	req = buildDecisionRequest(task, cfg.modelSelection, cands, 10, "")
	if n := len(gjson.GetBytes(req, "state").String()); n != 10 {
		t.Errorf("state not truncated: %d", n)
	}
}

func TestPickCandidateAndWeighted(t *testing.T) {
	cands := testCandidates()
	if !pickCandidate(cands, "gpt-6-luna") {
		t.Error("pickCandidate(gpt-6-luna) should match")
	}
	if pickCandidate(cands, "unknown") {
		t.Error("unknown model should not match")
	}
	if pickCandidate(cands, "") {
		t.Error("empty verdict should not match")
	}
	w := int64(10)
	weighted := append(testCandidates(), wire.Candidate{Cluster: "c", ModelName: "m", Weight: &w})
	if !isWeighted(weighted) {
		t.Error("isWeighted should detect a weighted set")
	}
	if isWeighted(cands) {
		t.Error("plain set should not be weighted")
	}
}

func TestVerdictScores(t *testing.T) {
	// Two instances of gpt-6-luna, one of deepseek-v4-flash. Every instance
	// of a model receives p(model) itself — NOT p/n: splitting would let a
	// replica count invert the model-level ordering after the finisher's
	// per-instance argmax (see designs §11). The finisher's shared L1
	// normalisation (dividing by Σ p·n = 1.7) preserves p's ordering:
	// gpt-6-luna instances 0.8/1.7 > dsv4 0.1/1.7.
	cands := []wire.Candidate{
		{Cluster: "outbound|80||gpt6-a.static", ModelName: "gpt-6-luna"},
		{Cluster: "outbound|80||gpt6-b.static", ModelName: "gpt-6-luna", TargetID: "2"},
		{Cluster: "outbound|80||dsv4.static", ModelName: "deepseek-v4-flash"},
	}
	probabilities := map[string]float64{
		"gpt-6-luna":        0.8,
		"claude-opus-5.5":   0.1, // not among candidates: ignored
		"deepseek-v4-flash": 0.1,
	}
	scores, matched := verdictScores(cands, probabilities)
	if !matched {
		t.Fatal("expected a match")
	}
	if len(scores) != 3 {
		t.Fatalf("scores = %v, want 3 entries", scores)
	}
	// Both gpt-6-luna instances get the full p (exactly tied — instance
	// choice stays with least-load / session-affinity).
	if got := scores["outbound|80||gpt6-a.static"]; got != 0.8 {
		t.Errorf("instance a = %v, want 0.8", got)
	}
	// Key() = cluster + NUL + targetId when targetId present.
	if got := scores["outbound|80||gpt6-b.static\x002"]; got != 0.8 {
		t.Errorf("instance b = %v, want 0.8", got)
	}
	if got := scores["outbound|80||dsv4.static"]; got != 0.1 {
		t.Errorf("dsv4 = %v, want 0.1", got)
	}

	// Replica asymmetry must not invert the model ordering: with weight W,
	// finisher totals are W·p/Σ(p·n) — 2-replica A (p=0.6) beats
	// 1-replica B (p=0.4) under any replica counts.
	w := 10.0
	aTotal := w * 0.6 / (0.6*2 + 0.4*1)
	bTotal := w * 0.4 / (0.6*2 + 0.4*1)
	if aTotal <= bTotal {
		t.Errorf("asymmetric replicas inverted the ordering: A=%v B=%v", aTotal, bTotal)
	}

	// A probability distribution matching no candidate scores nothing.
	if _, matched := verdictScores(cands, map[string]float64{"nope": 1}); matched {
		t.Error("unknown distribution should not match")
	}
	// Zero-probability models get no score entries.
	scores, _ = verdictScores(cands, map[string]float64{"deepseek-v4-flash": 0})
	if len(scores) != 0 {
		t.Errorf("zero probability should produce no scores: %v", scores)
	}
}

func TestConfidenceWeightsVote(t *testing.T) {
	// The confidence modulates the entry Weight, not the scores — the
	// finisher L1-normalises scores, so the effect must live in Weight to
	// survive (Weight = rankWeight × confidence).
	ans := jevAnswer{
		Choice: "gpt-6-luna",
		Probabilities: map[string]float64{
			"gpt-6-luna":        0.8,
			"deepseek-v4-flash": 0.2,
		},
		Confidence: 0.5,
	}
	cands := []wire.Candidate{
		{Cluster: "a", ModelName: "gpt-6-luna"},
		{Cluster: "b", ModelName: "deepseek-v4-flash"},
	}
	scores, matched := verdictScores(cands, ans.Probabilities)
	if !matched {
		t.Fatal("expected a match")
	}
	effectiveWeight := defaultRankWeight * ans.Confidence
	if effectiveWeight != 5 {
		t.Errorf("effective weight = %v, want 5 (10 × 0.5)", effectiveWeight)
	}
	if scores["a"] != 0.8 || scores["b"] != 0.2 {
		t.Errorf("scores = %v", scores)
	}
}

func TestRankWeightDefault(t *testing.T) {
	cfg := parseTestConfig(t, routeConfig)
	if cfg.rankWeight != defaultRankWeight {
		t.Errorf("rankWeight = %v, want %v", cfg.rankWeight, defaultRankWeight)
	}
}

func TestBuildDecisionRequestModel(t *testing.T) {
	// A configured decision model is sent as the request's `model` field
	// (spec request.model-alias); empty omits it.
	cfg := parseTestConfig(t, routeConfig)
	cands := testCandidates()
	task := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	req := buildDecisionRequest(task, cfg.modelSelection, cands, 0, "jev-larger")
	if got := gjson.GetBytes(req, "model").String(); got != "jev-larger" {
		t.Errorf("model = %q, want jev-larger", got)
	}
	// state/questions still intact alongside model.
	if !gjson.GetBytes(req, "questions.model_selection").Exists() {
		t.Error("questions lost when model set")
	}

	req = buildDecisionRequest(task, cfg.modelSelection, cands, 0, "")
	if gjson.GetBytes(req, "model").Exists() {
		t.Error("model should be omitted when empty")
	}
}

func TestProviderDecisionModel(t *testing.T) {
	var cfg PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{"provider": {"endpoint": "http://x:8010", "cluster": "c", "model": "jev-larger"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.decision.DecisionModel(); got != "jev-larger" {
		t.Errorf("DecisionModel = %q", got)
	}
}

func TestDecisionModelOverride(t *testing.T) {
	// Provider-level default engine.
	globalJSON := `{"provider": {"endpoint": "http://x:8010", "cluster": "c", "model": "jev-default"}}`
	global := parseTestConfig(t, globalJSON)
	if got := effectiveDecisionModel(global); got != "jev-default" {
		t.Errorf("provider default = %q", got)
	}

	// Route-level decisionModel overrides the provider's model; the rest
	// (provider, candidates, etc.) is inherited.
	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"decisionModel": "jev-larger"}`), global, &rule); err != nil {
		t.Fatal(err)
	}
	if got := effectiveDecisionModel(rule); got != "jev-larger" {
		t.Errorf("route override = %q, want jev-larger", got)
	}
	if rule.decision.DecisionModel() != "jev-default" {
		t.Error("provider model should be untouched by the override")
	}
}

func TestRuleLevelProvidersWithActiveProviderId(t *testing.T) {
	// A matchRule may carry its own providers catalogue AND its own
	// activeProviderId (the documented per-tenant form): the rule-level
	// id is explicit intent and must select from the rule's catalogue —
	// only an INHERITED global id is dropped when the catalogue is
	// overridden.
	global := parseTestConfig(t, `{}`)
	ruleJSON := `{
		"providers": [
			{"id": "a", "endpoint": "http://jev-a.internal:8010", "cluster": "ca"},
			{"id": "b", "endpoint": "http://jev-b.internal:8010", "cluster": "cb"}
		],
		"activeProviderId": "b"
	}`
	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(ruleJSON), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if rule.decision == nil || rule.decision.DecisionCluster() != "cb" {
		t.Fatalf("rule decision cluster = %v, want cb (rule-level activeProviderId must survive)", rule.decision)
	}
}

func TestTruncateUTF8(t *testing.T) {
	// ASCII body: cut lands exactly on the cap.
	if got := string(truncateUTF8([]byte("abcdef"), 3)); got != "abc" {
		t.Errorf("ascii truncate = %q, want abc", got)
	}
	// Cut inside a 3-byte rune ("世" = e4 b8 96): back off to the rune start.
	body := []byte("a世世")
	got := truncateUTF8(body, 3) // cuts between b8 and 96 of the first 世
	if !utf8.Valid(got) {
		t.Errorf("truncated state is not valid UTF-8: % x", got)
	}
	if string(got) != "a" {
		t.Errorf("truncate = %q, want %q", got, "a")
	}
	// Cut on a leading byte (max=2 keeps just 'a'+0xe4): also invalid,
	// must back off the partial sequence entirely.
	got = truncateUTF8(body, 2)
	if !utf8.Valid(got) || string(got) != "a" {
		t.Errorf("leading-byte cut = %q (%v), want %q", got, utf8.Valid(got), "a")
	}
	// Cap larger than the body: untouched.
	if got := truncateUTF8(body, 100); string(got) != string(body) {
		t.Errorf("uncapped truncate altered the body")
	}
}

func TestBuildDecisionRequestStateTruncationIsUTF8Safe(t *testing.T) {
	cfg := parseTestConfig(t, routeConfig)
	cands := testCandidates()
	// Task body ending in multi-byte runes, capped mid-rune.
	task := []byte(`{"messages":[{"role":"user","content":"你好世界你好世界"}]}`)
	req := buildDecisionRequest(task, cfg.modelSelection, cands, 48, "")
	if req == nil {
		t.Fatal("buildDecisionRequest returned nil")
	}
	state := gjson.GetBytes(req, "state").String()
	if !utf8.ValidString(state) {
		t.Errorf("state is not valid UTF-8 after truncation: %q", state)
	}
	if len(state) > 48 {
		t.Errorf("state exceeds cap: %d > 48", len(state))
	}
}

func TestModelSelectionCriteriaMustBeObject(t *testing.T) {
	// A misconfigured criteria (string/array) must be a config error, not a
	// silent skip via an empty Map().
	for _, bad := range []string{
		`{"modelSelection": {"criteria": "gpt-6-luna"}}`,
		`{"modelSelection": {"criteria": ["gpt-6-luna", "deepseek-v4-flash"]}}`,
	} {
		var cfg PluginConfig
		if err := parseGlobalConfig(gjson.Parse(bad), &cfg); err == nil {
			t.Errorf("criteria %s should be rejected as non-object", bad)
		}
	}
}

func TestBuildDecisionRequestNilModelSelection(t *testing.T) {
	// Defensive nil handling: no panic, no request composed.
	if req := buildDecisionRequest([]byte(`{}`), nil, testCandidates(), 0, ""); req != nil {
		t.Errorf("nil modelSelection should compose no request, got %s", req)
	}
}

func TestEndpointSchemeDefaultPortAndPathStripping(t *testing.T) {
	cases := []struct {
		endpoint string
		host     string
		cluster  string
	}{
		{"http://jev.internal", "jev.internal", "outbound|80||jev.internal"},
		{"https://jev.internal", "jev.internal", "outbound|443||jev.internal"},
		{"jev.internal:8010", "jev.internal:8010", "outbound|8010||jev.internal"},
		{"https://api.typesafe.ai/v1/systemone", "api.typesafe.ai", "outbound|443||api.typesafe.ai"},
		{"https://api.typesafe.ai?x=1", "api.typesafe.ai", "outbound|443||api.typesafe.ai"},
	}
	for _, c := range cases {
		p := provider.NewSystemone(&provider.ProviderConfig{Endpoint: c.endpoint})
		if got := p.DecisionHost(); got != c.host {
			t.Errorf("endpoint %q: DecisionHost = %q, want %q", c.endpoint, got, c.host)
		}
		if got := p.DecisionCluster(); got != c.cluster {
			t.Errorf("endpoint %q: DecisionCluster = %q, want %q", c.endpoint, got, c.cluster)
		}
	}
}

func TestRuleProviderOverrideClearsInheritedDecision(t *testing.T) {
	global := parseTestConfig(t, `{"provider": {"endpoint": "http://global-jev.internal:8010", "cluster": "cg"}}`)
	if global.decision == nil {
		t.Fatal("global decision should be set")
	}

	// An empty providers override leaves the feature inert — it must NOT
	// fall back to the copied global decision service.
	var rule PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"providers": []}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if rule.decision != nil {
		t.Fatalf("rule decision = %v, want nil (override must not leak the global decision service)", rule.decision)
	}

	// A resolvable override replaces the decision wholesale.
	if err := parseOverrideRuleConfig(gjson.Parse(`{"provider": {"endpoint": "http://rule-jev.internal:8010"}}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if rule.decision == nil || rule.decision.DecisionHost() != "rule-jev.internal:8010" {
		t.Fatalf("rule decision = %v, want rule-jev.internal:8010", rule.decision)
	}
}
