package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The parse is a pure function; the per-request evaluation is too (inputs
// injected). Host calls (header reads, filter state) live in applyRouteRules
// and are out of unit-test scope -- the same split as buildCandidateSet.

const threeTargets = `{
	"candidates": [
		{"cluster": "a", "targetId": "t1"},
		{"cluster": "b", "targetId": "t2"},
		{"cluster": "c", "targetId": "t3"}
	],
	"routeRules": [
		{
			"name": "vip",
			"match": {
				"headers": [
					{"name": "x-gpustack-tier", "exact": "vip"},
					{"name": "x-api-key", "prefix": "sk-vip-*"}
				],
				"model": "deepseek-r1*"
			},
			"targets": ["t1"]
		},
		{
			"name": "long",
			"match": {"contextLength": {"min": 65536}},
			"targets": ["t3"]
		}
	]
}`

func TestParseRouteRules(t *testing.T) {
	c := lbConfig(t, threeTargets)
	if len(c.routeRules) != 2 {
		t.Fatalf("parsed %d rules, want 2", len(c.routeRules))
	}
	r0 := c.routeRules[0]
	if r0.name != "vip" || len(r0.headers) != 2 || r0.model != "deepseek-r1*" || len(r0.targets) != 1 || r0.targets[0] != "t1" {
		t.Errorf("rule 0 parsed wrong: %+v", r0)
	}
	// The glob habit is tolerated: a trailing '*' is stripped.
	if r0.headers[1].prefix != "sk-vip-" {
		t.Errorf("prefix = %q, want %q (trailing * stripped)", r0.headers[1].prefix, "sk-vip-")
	}
	r1 := c.routeRules[1]
	if r1.minLength == nil || *r1.minLength != 65536 || r1.maxLength != nil {
		t.Errorf("rule 1 parsed wrong: %+v", r1)
	}
}

func TestParseRouteRulesRejectsBadConfigs(t *testing.T) {
	base := func(rules string) string {
		return `{"candidates":[{"cluster":"a","targetId":"t1"}],"routeRules":` + rules + `}`
	}
	cases := []struct {
		name, rules, wantErr string
	}{
		{"unknown target", `[{"match":{},"targets":["nope"]}]`, "not a candidate targetId"},
		{"empty targets", `[{"match":{},"targets":[]}]`, "non-empty array"},
		{"header without matcher", `[{"match":{"headers":[{"name":"x"}]},"targets":["t1"]}]`, "exactly one of exact / prefix"},
		{"header both matchers", `[{"match":{"headers":[{"name":"x","exact":"a","prefix":"b"}]},"targets":["t1"]}]`, "exactly one of exact / prefix"},
		{"min above max", `[{"match":{"contextLength":{"min":10,"max":5}},"targets":["t1"]}]`, "greater than max"},
		{"not an array", `{"x":1}`, "must be an array"},
		{"duplicate target", `[{"match":{},"targets":["t1","t1"]}]`, "twice"},
		{"match not an object", `[{"match":[{"model":"m*"}],"targets":["t1"]}]`, "match must be an object"},
	}
	for _, tc := range cases {
		var c Config
		err := parseConfig(gjson.Parse(base(tc.rules)), &c)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

func noHeaders(string) string { return "" }

func TestEvaluateRouteRulesMatching(t *testing.T) {
	rules := lbConfig(t, threeTargets).routeRules

	// Rule 0 needs both headers plus the model prefix.
	hdrs := map[string]string{
		"x-gpustack-tier": "vip",
		"x-api-key":       "sk-vip-001",
	}
	getter := func(n string) string { return hdrs[n] }
	if tg, ok := evaluateRouteRules(rules, "deepseek-r1-distill", getter, 0, false); !ok || tg[0] != "t1" {
		t.Errorf("vip rule: targets=%v matched=%v, want [t1] true", tg, ok)
	}
	// Model does not match -> falls through to rule 1 only if long enough.
	if _, ok := evaluateRouteRules(rules, "gpt-4o", getter, 100, true); ok {
		t.Errorf("short gpt-4o request matched a rule; want no match (full pool)")
	}
	if tg, ok := evaluateRouteRules(rules, "gpt-4o", getter, 100000, true); !ok || tg[0] != "t3" {
		t.Errorf("long gpt-4o: targets=%v matched=%v, want [t3] true", tg, ok)
	}
	// Missing headers: rule 0 fails, and without a length rule 1 does too.
	if _, ok := evaluateRouteRules(rules, "deepseek-r1", noHeaders, 0, false); ok {
		t.Errorf("no headers, no length: want no match")
	}
}

func TestEvaluateRouteRulesLengthSemantics(t *testing.T) {
	rules := lbConfig(t, threeTargets).routeRules

	// A length rule without a measurement never matches.
	if _, ok := evaluateRouteRules(rules, "whatever", noHeaders, 0, false); ok {
		t.Errorf("no measurement matched the min-length rule")
	}
	// Exactly at min counts (>= semantics).
	if tg, ok := evaluateRouteRules(rules, "whatever", noHeaders, 65536, true); !ok || tg[0] != "t3" {
		t.Errorf("length == min did not match: %v %v", tg, ok)
	}
	// Below min does not.
	if _, ok := evaluateRouteRules(rules, "whatever", noHeaders, 65535, true); ok {
		t.Errorf("length = min-1 matched")
	}
}

func TestEvaluateRouteRulesCatchAll(t *testing.T) {
	raw := `{"candidates":[{"cluster":"a","targetId":"t1"},{"cluster":"b","targetId":"t2"}],
		"routeRules":[{"name":"fallback","match":{},"targets":["t2"]}]}`
	rules := lbConfig(t, raw).routeRules
	if tg, ok := evaluateRouteRules(rules, "", noHeaders, 0, false); !ok || tg[0] != "t2" {
		t.Errorf("empty match should always hit: %v %v", tg, ok)
	}
}

func TestParseRouteRulesAbsentIsOff(t *testing.T) {
	c := lbConfig(t, twoCandidates)
	if len(c.routeRules) != 0 {
		t.Fatalf("no routeRules in config, got %d", len(c.routeRules))
	}
}

// excludeCluster is the fallback re-selection primitive (a pure function;
// the header reads stay in handleLBMode).
func TestExcludeCluster(t *testing.T) {
	config := lbConfig(t, threeTargets) // a/t1, b/t2, c/t3

	// Excluding b leaves a and c, in the parse-time sorted order.
	got := excludeClusters(config.candidates, []string{"b"})
	if len(got) != 2 || got[0].TargetID != "t1" || got[1].TargetID != "t3" {
		t.Errorf("excludeClusters([b]) = %v", got)
	}

	// A cluster nothing sits on excludes nothing.
	if got := excludeClusters(config.candidates, []string{"zzz"}); len(got) != 3 {
		t.Errorf("excluding an unknown cluster changed the set: %v", got)
	}

	// Several targets on one cluster share its fate; the ones on other
	// clusters survive.
	shared := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"a","targetId":"2"},
		{"cluster":"b","targetId":"3"}
	]}`)
	got = excludeClusters(shared.candidates, []string{"a"})
	if len(got) != 1 || got[0].Cluster != "b" {
		t.Errorf("excluding a shared cluster = %v, want only b/3", got)
	}

	// The empty-exclusion case (every candidate on the failed cluster) is
	// handled by the caller republishing the full set; the function itself
	// just returns empty, which is testable here.
	if got := excludeClusters(shared.candidates, []string{"b"}); len(got) != 2 {
		t.Errorf("excluding b = %v, want the two a-cluster candidates", got)
	}
}

// firstTier: higher-tier candidates take no share of normal traffic and
// only join the set when a re-selection pass drains every lower tier.
func TestFirstTier(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2","fallback":true},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)

	first := firstTier(config.candidates)
	if len(first) != 1 || first[0].Cluster != "a" {
		t.Errorf("firstTier = %v, want only a/1", first)
	}

	// The tier composition mirrors the re-selection drain order: exclude the
	// failed first-tier cluster, keep its survivors; only when the tier is
	// gone does the next one serve.
	step1 := excludeClusters(config.candidates, []string{"a"})
	if m := firstTier(step1); len(m) != 2 {
		t.Errorf("after the only tier-0 cluster fails, firstTier should be the standby tier, got %v", m)
	}
	if len(step1) != 2 {
		t.Errorf("standby tier should survive the exclusion: %v", step1)
	}

	// An all-standby route still serves: its lowest present tier IS its
	// first tier (there is no tier 0 to wait for).
	allFb := lbConfig(t, `{"candidates":[{"cluster":"a","targetId":"1","fallback":true}]}`)
	if m := firstTier(allFb.candidates); len(m) != 1 {
		t.Errorf("all-standby route: firstTier = %v, want the standby candidate", m)
	}
	if m := firstTier(nil); len(m) != 0 {
		t.Errorf("empty pool: firstTier = %v", m)
	}
}

// The A -> B -> A ping-pong guard: excluding only the LAST tried cluster
// lets two failing main clusters alternate until the redirect budget
// expires; excludeClusters over the ACCUMULATED set drains to the next
// tier instead.
func TestExcludeClustersAccumulated(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2"},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)
	// Two passes attempted a and b; only the fallback tier survives.
	got := excludeClusters(config.candidates, []string{"a", "b"})
	if len(got) != 1 || got[0].Cluster != "c" {
		t.Errorf("excluding [a,b] = %v, want only c/3", got)
	}
	// Empty tried set is a no-op (first pass).
	if got := excludeClusters(config.candidates, nil); len(got) != 3 {
		t.Errorf("empty tried set changed the pool: %v", got)
	}
}

func TestMatchModelPattern(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"", "anything", true},
		{"*", "anything", true},
		{"deepseek-r1", "deepseek-r1", true},
		{"deepseek-r1", "deepseek-r15", false},
		{"deepseek-r1*", "deepseek-r15", true},
		{"deepseek-r1*", "gpt-4o", false},
	}
	for _, tc := range cases {
		if got := matchModelPattern(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchModelPattern(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

// The drain must not pre-filter by ejection: provider candidates are
// exempt from passive health (one DNS cluster, many endpoints), so a
// first-tier provider carrying a shared or lingering ejection record still
// serves. Filtering it out here would hand traffic to a higher tier that
// should not receive it.
func TestTierDrainKeepsEjectedProvider(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"p","targetId":"1","kind":"provider","tier":0},
		{"cluster":"c","targetId":"2","tier":1}
	]}`)
	states := map[string]clusterState{
		"p\u00001": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	set := buildFailClosed(firstTierConfig(t, config), config.candidates, nowMillis(), "m", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "p" {
		t.Errorf("ejected first-tier provider should still serve, got %v", set.Candidates)
	}

	// An ejected INSTANCE in the first tier does drain to the next tier.
	inst := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1","tier":0},
		{"cluster":"c","targetId":"2","tier":1}
	]}`)
	istates := map[string]clusterState{
		"a\u00001": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	set = buildFailClosed(firstTierConfig(t, inst), inst.candidates, nowMillis(), "m", istates)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "c" {
		t.Errorf("ejected first-tier instance should drain to tier 1, got %v", set.Candidates)
	}
}

// buildFailClosed implements the strict preference order: the healthy
// candidates of the lowest tier, then the next tier, and never a
// re-admitted ejected candidate.
func TestBuildFailClosed(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2"},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)
	now := nowMillis()
	// All healthy: the first tier serves, the standby stays out.
	set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", nil)
	if len(set.Candidates) != 2 || set.Candidates[0].Fallback || set.Candidates[1].Fallback {
		t.Errorf("healthy first tier should serve, got %v", set.Candidates)
	}
	// Every first-tier survivor ejected: the healthy standby serves, not
	// the ejected B.
	states := map[string]clusterState{
		"a\u00001": {Health: healthState{EjectedUntil: 1 << 62}},
		"b\u00002": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	set = buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "c" {
		t.Errorf("ejected first tier should drain to the healthy standby, got %v", set.Candidates)
	}
	// Everything ejected (standby included): nothing fail-closed.
	states["c\u00003"] = clusterState{Health: healthState{EjectedUntil: 1 << 62}}
	if set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states); len(set.Candidates) != 0 {
		t.Errorf("fully-ejected pool should fail closed, got %v", set.Candidates)
	}
}

// A route with no tier configured is the default for every existing
// deployment, so its behavior is pinned explicitly rather than left to
// fall out of the drain: one tier holding the whole pool, published
// together, and fail-closed once every member is ejected.
func TestNoTiersKeepsSinglePoolBehavior(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2"}
	]}`)
	now := nowMillis()
	set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", nil)
	if len(set.Candidates) != 2 {
		t.Fatalf("untiered pool should publish together, got %v", set.Candidates)
	}
	for _, cand := range set.Candidates {
		if cand.Tier != 0 || cand.Fallback {
			t.Errorf("untiered candidate should be tier 0 / not fallback, got %+v", cand)
		}
	}
	states := map[string]clusterState{
		"a\x001": {Health: healthState{EjectedUntil: 1 << 62}},
		"b\x002": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	if set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states); len(set.Candidates) != 0 {
		t.Errorf("fully-ejected untiered pool should fail closed, got %v", set.Candidates)
	}
}

// firstTierConfig mirrors what handleLBMode hands buildFailClosed: the
// config narrowed to the published (lowest) tier, with the FULL narrowed
// pool passed separately as the drain source.
func firstTierConfig(t *testing.T, c Config) Config {
	t.Helper()
	if first := firstTier(c.candidates); len(first) > 0 {
		c.candidates = first
	}
	return c
}

// N-tier drain: the ordinal chain generalises past two tiers, and a tier
// whose survivors are all ejected is skipped rather than handed back.
func TestOrderedTierDrain(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1","tier":0},
		{"cluster":"b","targetId":"2","tier":1},
		{"cluster":"c","targetId":"3","tier":2}
	]}`)
	now := nowMillis()

	if got := tierOrder(config.candidates); len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("tierOrder = %v, want [0 1 2]", got)
	}
	// Normal pass publishes tier 0 only.
	if first := firstTier(config.candidates); len(first) != 1 || first[0].Cluster != "a" {
		t.Errorf("firstTier = %v, want a/1", first)
	}

	// Tier 0 ejected -> tier 1 serves.
	states := map[string]clusterState{
		"a\u00001": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "b" {
		t.Errorf("tier 0 ejected should drain to tier 1, got %v", set.Candidates)
	}
	// Tiers 0 and 1 ejected -> tier 2 serves.
	states["b\u00002"] = clusterState{Health: healthState{EjectedUntil: 1 << 62}}
	set = buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "c" {
		t.Errorf("tiers 0,1 ejected should drain to tier 2, got %v", set.Candidates)
	}
	// All three ejected -> fail closed (the caller's fail-open build is
	// the last resort, not this function).
	states["c\u00003"] = clusterState{Health: healthState{EjectedUntil: 1 << 62}}
	if set := buildFailClosed(firstTierConfig(t, config), config.candidates, now, "m", states); len(set.Candidates) != 0 {
		t.Errorf("fully-ejected 3-tier pool should fail closed, got %v", set.Candidates)
	}

	// The tried-set exclusion composes with the drain: a and b attempted
	// leaves tier 2 as the first tier present.
	step := excludeClusters(config.candidates, []string{"a", "b"})
	if first := firstTier(step); len(first) != 1 || first[0].Cluster != "c" {
		t.Errorf("after excluding a,b firstTier = %v, want c/3", first)
	}
}

// Tiers are ordinals, not indices: gaps carry no meaning and a missing
// tier is simply not a step.
func TestTierOrderNonContiguous(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1","tier":5},
		{"cluster":"b","targetId":"2","tier":0},
		{"cluster":"c","targetId":"3","tier":5}
	]}`)
	got := tierOrder(config.candidates)
	if len(got) != 2 || got[0] != 0 || got[1] != 5 {
		t.Fatalf("tierOrder = %v, want [0 5]", got)
	}
	if first := firstTier(config.candidates); len(first) != 1 || first[0].Cluster != "b" {
		t.Errorf("firstTier = %v, want b/2 (tier 0)", first)
	}
	set := buildFailClosed(firstTierConfig(t, config), config.candidates, nowMillis(), "m",
		map[string]clusterState{"b\u00002": {Health: healthState{EjectedUntil: 1 << 62}}})
	if len(set.Candidates) != 2 {
		t.Errorf("tier 0 ejected should drain straight to tier 5, got %v", set.Candidates)
	}
}
