package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/tidwall/gjson"
)

// serviceKey must strip exactly the outbound|port|| prefix and nothing else:
// the key is what lands in a request header and what excludeClusters compares,
// so a wrong strip silently breaks re-selection.
func TestServiceKey(t *testing.T) {
	cases := []struct{ cluster, want string }{
		{"outbound|80||mock-a.lbtest.svc.cluster.local", "mock-a.lbtest.svc.cluster.local"},
		{"outbound|8080||svc", "svc"},
		// Not in outbound|port||service shape: returned unchanged rather than
		// guessed at. Unit tests build candidates with bare names.
		{"a", "a"},
		{"outbound|80||", "outbound|80||"},
		{"", ""},
		// Four parts is not enough: a non-outbound direction or a non-empty
		// subset names a different cluster, and stripping it would make
		// checkServiceKeys reject a route that has no real collision.
		{"custom|http|v1|svc", "custom|http|v1|svc"},
		{"inbound|80||svc", "inbound|80||svc"},
		{"outbound|80|v1|svc", "outbound|80|v1|svc"},
	}
	for _, tc := range cases {
		if got := serviceKey(tc.cluster); got != tc.want {
			t.Errorf("serviceKey(%q) = %q, want %q", tc.cluster, got, tc.want)
		}
	}
}

// The tried header records the service name alone, so two different clusters
// answering to one name could not be told apart: excluding one would silently
// drop the other's endpoints. gpustack cannot produce that, so it is a check.
func TestCheckServiceKeys(t *testing.T) {
	// Same cluster repeated (one provider DNS cluster, many endpoints) is
	// fine and is the intent: excluding the cluster is what a failure means.
	if err := checkServiceKeys([]candidateSpec{
		{Candidate: Candidate{Cluster: "outbound|80||a.ns.svc"}},
		{Candidate: Candidate{Cluster: "outbound|80||a.ns.svc", TargetID: "2"}},
	}); err != nil {
		t.Errorf("same cluster twice should be accepted, got %v", err)
	}
	// Same service name, different port = two clusters, indistinguishable.
	err := checkServiceKeys([]candidateSpec{
		{Candidate: Candidate{Cluster: "outbound|80||b.ns.svc"}},
		{Candidate: Candidate{Cluster: "outbound|8080||b.ns.svc"}},
	})
	if err == nil || !strings.Contains(err.Error(), "b.ns.svc") {
		t.Errorf("a service name claimed by two clusters should be rejected naming it, got %v", err)
	}
	// A custom four-part name and a plain outbound cluster are different
	// clusters even when their last segment coincides; neither is stripped to
	// the other's key, so there is no collision to report.
	if err := checkServiceKeys([]candidateSpec{
		{Candidate: Candidate{Cluster: "custom|http|v1|svc"}},
		{Candidate: Candidate{Cluster: "outbound|80||svc"}},
	}); err != nil {
		t.Errorf("a custom four-part name should not collide with the plain outbound cluster, got %v", err)
	}
}

// triedKey is derived, never stored: the service name is unambiguous across
// the route (checkServiceKeys), so the cluster name determines the key.
func TestTriedKeyIsServiceName(t *testing.T) {
	if got := triedKey(Candidate{Cluster: "outbound|80||a.ns.svc"}); got != "a.ns.svc" {
		t.Errorf("triedKey = %q, want %q", got, "a.ns.svc")
	}
	// Not in cluster shape: unchanged, which is what the hand-built test
	// candidates rely on.
	if got := triedKey(Candidate{Cluster: "a"}); got != "a" {
		t.Errorf("triedKey(bare) = %q, want %q", got, "a")
	}
}

// Both spellings of a candidate's identity must exclude it: the accumulator
// records the short key, while the legacy path (no accumulator header
// survived) passes the full cluster name from x-higress-target-cluster.
// Matching only the key would re-select the cluster that just failed.
func TestExclusionAcceptsBothSpellings(t *testing.T) {
	cands := []candidateSpec{
		{Candidate: Candidate{Cluster: "outbound|80||a.ns.svc"}},
		{Candidate: Candidate{Cluster: "outbound|80||b.ns.svc"}},
	}
	for _, tc := range []struct{ what, tried string }{
		{"short key (accumulator)", "a.ns.svc"},
		{"full cluster name (legacy fallback)", "outbound|80||a.ns.svc"},
	} {
		got := excludeClusters(cands, []string{tc.tried})
		if len(got) != 1 || got[0].Cluster != "outbound|80||b.ns.svc" {
			t.Errorf("%s: excluding %q removed the wrong set: %v", tc.what, tc.tried, got)
		}
	}
}

func TestParseCandidateGroups(t *testing.T) {
	raw := `{"candidates":[
		{"cluster":"a","targetId":"1","groups":["vip","long"]},
		{"cluster":"b","targetId":"2"},
		{"cluster":"c","targetId":"3","groups":[]}
	],"groups":[
		{"name":"vip","match":{}},
		{"name":"long","match":{}}
	]}`
	c := lbConfig(t, raw)
	want := [][]string{{"vip", "long"}, nil, nil}
	for i, w := range want {
		got := c.candidates[i].groups
		if len(got) != len(w) {
			t.Fatalf("candidate %d groups = %v, want %v", i, got, w)
		}
		for j := range w {
			if got[j] != w[j] {
				t.Errorf("candidate %d groups[%d] = %q, want %q", i, j, got[j], w[j])
			}
		}
	}
}

func TestParseCandidateGroupsRejectsBad(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","targetId":"1","groups":"vip"}]}`,
		`{"candidates":[{"cluster":"a","targetId":"1","groups":[""]}]}`,
		`{"candidates":[{"cluster":"a","targetId":"1","groups":["vip","vip"]}]}`,
		// gjson renders `true` as "true"; a non-string must not become a
		// routing identity that could match a group named "true".
		`{"candidates":[{"cluster":"a","targetId":"1","groups":[true]}],
			"groups":[{"name":"true","match":{}}]}`,
		`{"candidates":[{"cluster":"a","targetId":"1","groups":[1]}]}`,
	} {
		var c Config
		err := parseConfig(gjson.Parse(raw), &c)
		if err == nil {
			t.Errorf("expected error for %s", raw)
		} else if !strings.Contains(err.Error(), "groups") {
			t.Errorf("error for %s should mention groups, got %v", raw, err)
		}
	}
}

// A candidate may only name a group that selects by membership, else it is
// unreachable by that name with nothing in the config saying why.
func TestCandidateGroupsMustNameAGroup(t *testing.T) {
	raw := `{"candidates":[{"cluster":"a","targetId":"1","groups":["ghost"]}],
		"groups":[{"name":"vip","match":{}}]}`
	var c Config
	err := parseConfig(gjson.Parse(raw), &c)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("naming an absent group should name it, got %v", err)
	}
}

// name is the selector, not an optional label.
func TestGroupNameRequired(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":[{"cluster":"a","targetId":"1"}],
			"groups":[{"match":{}}]}`,
		// gjson renders `true` as "true": without the type check this would
		// pass the emptiness guard and could replace a real group named "true".
		`{"candidates":[{"cluster":"a","targetId":"1"}],
			"groups":[{"name":true,"match":{}}]}`,
		`{"candidates":[{"cluster":"a","targetId":"1"}],
			"groups":[{"name":"","match":{}}]}`,
	} {
		var c Config
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("expected error for %s", raw)
		}
	}
}

// A duplicate name keeps the FIRST position and takes the LAST content: the
// list is first-match, so an override that moved to its own slot would
// silently change which group a request hits.
func TestGroupNameLastWinsInPlace(t *testing.T) {
	var warnings []string
	warnf = func(format string, args ...interface{}) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	defer func() { warnf = proxywasm.LogWarnf }()

	raw := `{"candidates":[
		{"cluster":"a","targetId":"1","groups":["vip"]},
		{"cluster":"b","targetId":"2","groups":["mid"]}
	],"groups":[
		{"name":"vip","match":{"model":"first*"}},
		{"name":"mid","match":{}},
		{"name":"vip","match":{"model":"second*"}}
	]}`
	c := lbConfig(t, raw)
	if len(c.groups) != 2 {
		t.Fatalf("groups = %d, want 2 (duplicate collapsed)", len(c.groups))
	}
	if c.groups[0].name != "vip" || c.groups[0].model != "second*" {
		t.Errorf("groups[0] = %q/%q, want vip/second* (first slot, last content)",
			c.groups[0].name, c.groups[0].model)
	}
	if c.groups[1].name != "mid" {
		t.Errorf("groups[1].name = %q, want mid (order preserved)", c.groups[1].name)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "vip") {
		t.Errorf("warnings = %v, want one naming vip", warnings)
	}
}

// A group selects the candidates whose groups list its own name.
func TestNarrowByGroupName(t *testing.T) {
	cands := []candidateSpec{
		{Candidate: Candidate{Cluster: "a", TargetID: "1"}, groups: []string{"vip"}},
		{Candidate: Candidate{Cluster: "b", TargetID: "2"}, groups: []string{"vip", "long"}},
		{Candidate: Candidate{Cluster: "c", TargetID: "3"}, groups: []string{"other"}},
		{Candidate: Candidate{Cluster: "d", TargetID: "4"}}, // no groups
	}
	got := narrowByGroup(cands, &routeGroup{name: "vip"})
	if len(got) != 2 || got[0].TargetID != "1" || got[1].TargetID != "2" {
		t.Errorf("group vip = %v, want [1 2]", targetIDs(got))
	}
	for _, c := range got {
		if c.TargetID == "4" {
			t.Error("ungrouped candidate survived a group it is not in")
		}
	}
}

// The legacy selector keeps working for one minor version.
func TestNarrowByGroupLegacyTargets(t *testing.T) {
	cands := []candidateSpec{
		{Candidate: Candidate{Cluster: "a", TargetID: "1"}, groups: []string{"vip"}},
		{Candidate: Candidate{Cluster: "b", TargetID: "2"}},
	}
	got := narrowByGroup(cands, &routeGroup{name: "legacy", targets: []string{"2"}})
	if len(got) != 1 || got[0].TargetID != "2" {
		t.Errorf("targets=[2] = %v, want [2]", targetIDs(got))
	}
}

// Fallback candidates survive a group that would otherwise exclude them.
func TestNarrowByGroupKeepsFallback(t *testing.T) {
	cands := []candidateSpec{
		{Candidate: Candidate{Cluster: "a", TargetID: "1"}, groups: []string{"vip"}},
		{Candidate: Candidate{Cluster: "b", TargetID: "2", Fallback: true}},
	}
	got := narrowByGroup(cands, &routeGroup{name: "vip"})
	if len(got) != 2 {
		t.Fatalf("fallback was narrowed away: %v", targetIDs(got))
	}
	if !got[1].Fallback {
		t.Errorf("order changed: %v", targetIDs(got))
	}
	// A group that matches nothing still keeps the fallback.
	if got := narrowByGroup(cands, &routeGroup{name: "nobody"}); len(got) != 1 || !got[0].Fallback {
		t.Errorf("fallback should survive a non-matching group, got %v", targetIDs(got))
	}
}

// A legacy `targets` group ignores membership, so a candidate naming it is
// just as unreachable as one naming an absent group -- and gets its own
// message so the fix is obvious. A mixed config stays legal as long as
// nothing references the targets group by name.
func TestCandidateGroupsRejectLegacyTargetsGroup(t *testing.T) {
	referenced := `{"candidates":[{"cluster":"a","targetId":"1","groups":["legacy"]}],
		"groups":[{"name":"legacy","match":{},"targets":["1"]}]}`
	err := parseConfig(gjson.Parse(referenced), &Config{})
	if err == nil || !strings.Contains(err.Error(), "legacy targets") {
		t.Errorf("naming a targets group should be rejected as such, got %v", err)
	}

	mixed := `{"candidates":[
		{"cluster":"a","targetId":"1","groups":["vip"]},
		{"cluster":"b","targetId":"2"}],
		"groups":[{"name":"vip","match":{}},{"name":"legacy","match":{},"targets":["2"]}]}`
	if err := parseConfig(gjson.Parse(mixed), &Config{}); err != nil {
		t.Errorf("an unreferenced targets group should stay legal, got %v", err)
	}
}

// cluster, targetId and legacy targets[] are routing identities: gjson
// renders a non-string as its string form, so `true` would become "true" and
// match whatever is literally named that.
func TestIdentityFieldsTypeChecked(t *testing.T) {
	for name, raw := range map[string]string{
		"cluster":   `{"candidates":[{"cluster":true,"targetId":"1"}]}`,
		"targetId":  `{"candidates":[{"cluster":"a","targetId":true}]}`,
		"targets[]": `{"candidates":[{"cluster":"a","targetId":"1"}],"groups":[{"name":"g","match":{},"targets":[true]}]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &Config{}); err == nil {
			t.Errorf("%s: a non-string identity should be rejected for %s", name, raw)
		}
	}
	// Absent stays legal: a candidate need not carry a targetId.
	if err := parseConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), &Config{}); err != nil {
		t.Errorf("absent targetId should parse, got %v", err)
	}
}

// An unrecognised section name is rejected rather than ignored: ignoring it
// would publish the full pool silently, which looks exactly like "no group
// matched".
func TestOldRouteRulesKeyRejected(t *testing.T) {
	raw := `{"candidates":[{"cluster":"a","targetId":"1"}],
		"routeRules":[{"name":"vip","match":{}}]}`
	err := parseConfig(gjson.Parse(raw), &Config{})
	if err == nil || !strings.Contains(err.Error(), "groups") {
		t.Errorf("the old key should be rejected pointing at groups, got %v", err)
	}
}

func targetIDs(cands []candidateSpec) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.TargetID)
	}
	return out
}
