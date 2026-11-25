package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The parse is a pure function; the per-request evaluation is too (inputs
// injected). Host calls (header reads, filter state) live in applyRouteGroups
// and are out of unit-test scope -- the same split as buildCandidateSet.

const threeTargets = `{
	"candidates": [
		{"cluster": "a", "targetId": "t1"},
		{"cluster": "b", "targetId": "t2"},
		{"cluster": "c", "targetId": "t3"}
	],
	"groups": [
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

func TestParseRouteGroups(t *testing.T) {
	c := lbConfig(t, threeTargets)
	if len(c.groups) != 2 {
		t.Fatalf("parsed %d groups, want 2", len(c.groups))
	}
	r0 := c.groups[0]
	if r0.name != "vip" || len(r0.headers) != 2 || r0.model != "deepseek-r1*" || len(r0.targets) != 1 || r0.targets[0] != "t1" {
		t.Errorf("group 0 parsed wrong: %+v", r0)
	}
	// The glob habit is tolerated: a trailing '*' is stripped.
	if r0.headers[1].prefix != "sk-vip-" {
		t.Errorf("prefix = %q, want %q (trailing * stripped)", r0.headers[1].prefix, "sk-vip-")
	}
	r1 := c.groups[1]
	if r1.minLength == nil || *r1.minLength != 65536 || r1.maxLength != nil {
		t.Errorf("group 1 parsed wrong: %+v", r1)
	}
}

func TestParseRouteGroupsRejectsBadConfigs(t *testing.T) {
	base := func(raw string) string {
		return `{"candidates":[{"cluster":"a","targetId":"t1"}],"groups":` + raw + `}`
	}
	cases := []struct {
		name, raw, wantErr string
	}{
		{"unknown target", `[{"name":"r","match":{},"targets":["nope"]}]`, "not a candidate targetId"},
		{"empty targets", `[{"name":"r","match":{},"targets":[]}]`, "non-empty array"},
		{"header without matcher", `[{"name":"r","match":{"headers":[{"name":"x"}]},"targets":["t1"]}]`, "exactly one of exact / prefix"},
		{"header both matchers", `[{"name":"r","match":{"headers":[{"name":"x","exact":"a","prefix":"b"}]},"targets":["t1"]}]`, "exactly one of exact / prefix"},
		{"min above max", `[{"name":"r","match":{"contextLength":{"min":10,"max":5}},"targets":["t1"]}]`, "greater than max"},
		{"not an array", `{"x":1}`, "must be an array"},
		{"duplicate target", `[{"name":"r","match":{},"targets":["t1","t1"]}]`, "twice"},
		{"non-string target", `[{"name":"r","match":{},"targets":[true]}]`, "must be a string"},
		{"match not an object", `[{"name":"r","match":[{"model":"m*"}],"targets":["t1"]}]`, "match must be an object"},
	}
	for _, tc := range cases {
		var c Config
		err := parseConfig(gjson.Parse(base(tc.raw)), &c)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

func noHeaders(string) string { return "" }

func TestEvaluateRouteGroupsMatching(t *testing.T) {
	groups := lbConfig(t, threeTargets).groups

	// Group 0 needs both headers plus the model prefix.
	hdrs := map[string]string{
		"x-gpustack-tier": "vip",
		"x-api-key":       "sk-vip-001",
	}
	getter := func(n string) string { return hdrs[n] }
	if r, ok := evaluateRouteGroups(groups, "deepseek-r1-distill", getter, 0, false); !ok || r.targets[0] != "t1" {
		t.Errorf("vip group: targets=%v matched=%v, want [t1] true", r.targets, ok)
	}
	// Model does not match -> falls through to group 1 only if long enough.
	if _, ok := evaluateRouteGroups(groups, "gpt-4o", getter, 100, true); ok {
		t.Errorf("short gpt-4o request matched a group; want no match (full pool)")
	}
	if r, ok := evaluateRouteGroups(groups, "gpt-4o", getter, 100000, true); !ok || r.targets[0] != "t3" {
		t.Errorf("long gpt-4o: targets=%v matched=%v, want [t3] true", r.targets, ok)
	}
	// Missing headers: group 0 fails, and without a length group 1 does too.
	if _, ok := evaluateRouteGroups(groups, "deepseek-r1", noHeaders, 0, false); ok {
		t.Errorf("no headers, no length: want no match")
	}
}

func TestEvaluateRouteGroupsLengthSemantics(t *testing.T) {
	groups := lbConfig(t, threeTargets).groups

	// A length condition without a measurement never matches.
	if _, ok := evaluateRouteGroups(groups, "whatever", noHeaders, 0, false); ok {
		t.Errorf("no measurement matched the min-length group")
	}
	// Exactly at min counts (>= semantics).
	if r, ok := evaluateRouteGroups(groups, "whatever", noHeaders, 65536, true); !ok || r.targets[0] != "t3" {
		t.Errorf("length == min did not match: %v %v", r.targets, ok)
	}
	// Below min does not.
	if _, ok := evaluateRouteGroups(groups, "whatever", noHeaders, 65535, true); ok {
		t.Errorf("length = min-1 matched")
	}
}

func TestEvaluateRouteGroupsCatchAll(t *testing.T) {
	raw := `{"candidates":[{"cluster":"a","targetId":"t1"},{"cluster":"b","targetId":"t2"}],
		"groups":[{"name":"fallback","match":{},"targets":["t2"]}]}`
	groups := lbConfig(t, raw).groups
	if r, ok := evaluateRouteGroups(groups, "", noHeaders, 0, false); !ok || r.targets[0] != "t2" {
		t.Errorf("empty match should always hit: %v %v", r.targets, ok)
	}
}

func TestParseRouteGroupsAbsentIsOff(t *testing.T) {
	c := lbConfig(t, twoCandidates)
	if len(c.groups) != 0 {
		t.Fatalf("no groups in config, got %d", len(c.groups))
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

// mainTier: fallback-marked candidates take no share of normal traffic and
// only join the set when a re-selection pass drains the main tier.
func TestMainTier(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2","fallback":true},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)

	main := mainTier(config.candidates)
	if len(main) != 1 || main[0].Cluster != "a" {
		t.Errorf("mainTier = %v, want only a/1", main)
	}

	// The tier composition mirrors the re-selection drain order: exclude the
	// failed main cluster, keep surviving main candidates; only when the
	// main tier is gone does the fallback tier serve.
	step1 := excludeClusters(config.candidates, []string{"a"})
	if m := mainTier(step1); len(m) != 0 {
		t.Errorf("after the only main cluster fails, mainTier should be empty, got %v", m)
	}
	if len(step1) != 2 {
		t.Errorf("fallback tier should survive the exclusion: %v", step1)
	}

	// All-fallback routes degrade to publishing everything (a route whose
	// only targets are fallback targets must still serve).
	allFb := lbConfig(t, `{"candidates":[{"cluster":"a","targetId":"1","fallback":true}]}`)
	if m := mainTier(allFb.candidates); len(m) != 0 {
		t.Errorf("all-fallback route: mainTier = %v", m)
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

// The health-aware tier drain: when every publishable main-tier survivor
// is ejected, the fallback tier of the narrowed set serves — but an
// EJECTED fallback candidate must not be handed back either.
func TestFallbackTierOnly(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)
	// No state: the healthy fallback is returned.
	got := fallbackTierOnly(config.candidates, nil)
	if len(got) != 1 || got[0].Cluster != "c" {
		t.Errorf("healthy fallback drain = %v", got)
	}
	// The fallback itself ejected: nothing to drain to.
	states := map[string]clusterState{
		"c\u00003": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	if got := fallbackTierOnly(config.candidates, states); got != nil {
		t.Errorf("ejected fallback returned: %v", got)
	}
	// A main-tier-only set has no drain target.
	main := lbConfig(t, `{"candidates":[{"cluster":"a","targetId":"1"}]}`)
	if got := fallbackTierOnly(main.candidates, nil); got != nil {
		t.Errorf("main-only set returned a fallback: %v", got)
	}
}

// buildFailClosed implements the strict preference order: healthy main
// tier, then healthy fallback tier, and never a re-admitted ejected
// candidate.
func TestBuildFailClosed(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2"},
		{"cluster":"c","targetId":"3","fallback":true}
	]}`)
	now := nowMillis()
	// All healthy: the main tier serves, the fallback stays out.
	set := buildFailClosed(mainOnlyConfig(t, config), config.candidates, now, "m", nil)
	if len(set.Candidates) != 2 || set.Candidates[0].Fallback || set.Candidates[1].Fallback {
		t.Errorf("healthy main tier should serve, got %v", set.Candidates)
	}
	// Every main survivor ejected: the healthy fallback F serves, not the
	// ejected B.
	states := map[string]clusterState{
		"a\u00001": {Health: healthState{EjectedUntil: 1 << 62}},
		"b\u00002": {Health: healthState{EjectedUntil: 1 << 62}},
	}
	set = buildFailClosed(mainOnlyConfig(t, config), config.candidates, now, "m", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "c" {
		t.Errorf("ejected mains should drain to the healthy fallback, got %v", set.Candidates)
	}
	// Everything ejected (fallback included): nothing fail-closed.
	states["c\u00003"] = clusterState{Health: healthState{EjectedUntil: 1 << 62}}
	if set := buildFailClosed(mainOnlyConfig(t, config), config.candidates, now, "m", states); len(set.Candidates) != 0 {
		t.Errorf("fully-ejected pool should fail closed, got %v", set.Candidates)
	}
}

func mainOnlyConfig(t *testing.T, c Config) Config {
	t.Helper()
	if main := mainTier(c.candidates); len(main) > 0 {
		c.candidates = main
	}
	return c
}
