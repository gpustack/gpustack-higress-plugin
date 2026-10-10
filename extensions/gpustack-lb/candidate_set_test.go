package main

import (
	"testing"
)

// buildCandidateSet became a pure function when the state moved behind the
// store: it takes the loaded state instead of reading it. That makes the whole
// filter -- ejection, the concurrency cap, the penalty curve, fail-open
// re-admission -- testable without a wasm host, and it is the same code on both
// backends, so these tests pin the behaviour for redis too.

func lbConfig(t *testing.T, raw string) Config {
	t.Helper()
	c := parse(t, raw)
	if !c.lbMode {
		t.Fatalf("config %s did not enable LB mode", raw)
	}
	return c
}

func published(set CandidateSet) map[string]Candidate {
	out := make(map[string]Candidate, len(set.Candidates))
	for _, c := range set.Candidates {
		out[c.Key()] = c
	}
	return out
}

const twoCandidates = `{"candidates":[
	{"cluster":"a","targetId":"1"},
	{"cluster":"b","targetId":"2"}
]}`

// No state is the fail-open shape: it is what a brand-new gateway sees, and
// also what a failed redis load produces. Nothing may be filtered out.
func TestBuildCandidateSetWithNoState(t *testing.T) {
	set := buildCandidateSet(lbConfig(t, twoCandidates), 1000, "", nil)
	if len(set.Candidates) != 2 {
		t.Fatalf("published %d candidates, want 2", len(set.Candidates))
	}
	for _, c := range set.Candidates {
		if c.Inflight != 0 || c.Penalty != 0 || c.Probation {
			t.Errorf("%s: %+v, want a clean candidate", c.Cluster, c)
		}
	}
}

func TestEjectedCandidateIsNotPublished(t *testing.T) {
	states := map[string]clusterState{
		"a\x001": {Health: healthState{EjectedUntil: 2000, RampUntil: 3000}},
		"b\x002": {Inflight: 5},
	}
	set := buildCandidateSet(lbConfig(t, twoCandidates), 1000, "", states)
	got := published(set)
	if len(got) != 1 {
		t.Fatalf("published %d candidates, want 1: %v", len(got), set.Candidates)
	}
	b, ok := got["b\x002"]
	if !ok {
		t.Fatalf("b was not published: %v", set.Candidates)
	}
	if b.Inflight != 5 {
		t.Errorf("b.Inflight = %d, want 5", b.Inflight)
	}
}

// The recovery penalty decays linearly from P0 = maxLoad + 1 to zero across the
// ramp window. P0 is adaptive so there is no extra knob: at t=0 the recovering
// candidate sorts behind every live one, then returns to normal.
func TestRecoveryPenaltyDecays(t *testing.T) {
	states := map[string]clusterState{
		// Cooldown over, halfway through the ramp.
		"a\x001": {Health: healthState{EjectedUntil: 1000, RampUntil: 2000}},
		"b\x002": {Inflight: 3},
	}
	set := buildCandidateSet(lbConfig(t, twoCandidates), 1500, "", states)
	got := published(set)
	a, ok := got["a\x001"]
	if !ok {
		t.Fatalf("a was not published: %v", set.Candidates)
	}
	if !a.Probation {
		t.Error("a.Probation = false, want true during the ramp window")
	}
	// ratio 0.5, maxLoad 3 -> 0.5 * (3+1)
	if a.Penalty != 2 {
		t.Errorf("a.Penalty = %v, want 2", a.Penalty)
	}
	if got["b\x002"].Penalty != 0 {
		t.Errorf("b.Penalty = %v, want 0", got["b\x002"].Penalty)
	}
}

// P0 counts only the candidates that will actually be published. An ejected
// instance's in-flight count is often a batch of hung requests that never
// returned -- a high, stale number that would hold a recovering candidate down
// far longer than it should.
func TestPenaltyBaseIgnoresEjectedLoad(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2"},
		{"cluster":"c","targetId":"3"}
	]}`)
	states := map[string]clusterState{
		"a\x001": {Health: healthState{EjectedUntil: 1000, RampUntil: 2000}},
		"b\x002": {Inflight: 2},
		// Ejected, and carrying a pile of leaked in-flight entries.
		"c\x003": {Inflight: 500, Health: healthState{EjectedUntil: 9000, RampUntil: 9000}},
	}
	set := buildCandidateSet(config, 1500, "", states)
	// maxLoad is b's 2, not c's 500: 0.5 * (2+1)
	if got := published(set)["a\x001"].Penalty; got != 1.5 {
		t.Errorf("a.Penalty = %v, want 1.5", got)
	}
}

// The concurrency cap is no longer a publish filter: the snapshot a filter
// could act on is stale by the time the finisher runs, so the cap is enforced
// by the finisher's atomic reserve instead. What publishing must do is carry
// the cap (and the load, for ranking) so the reserve knows each target's
// budget.
func TestOverCapCandidateIsPublishedWithItsCap(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1","maxRunningRequests":4},
		{"cluster":"b","targetId":"2"}
	]}`)
	set := buildCandidateSet(config, 1000, "", map[string]clusterState{"a\x001": {Inflight: 4}})
	got := published(set)
	if len(got) != 2 {
		t.Fatalf("published %v, want both candidates (cap is the finisher's job)", set.Candidates)
	}
	if got["a\x001"].MaxRunningRequests != 4 {
		t.Errorf("a.MaxRunningRequests = %d, want 4 published for the reserve", got["a\x001"].MaxRunningRequests)
	}
	if got["a\x001"].Inflight != 4 {
		t.Errorf("a.Inflight = %d, want 4 for ranking", got["a\x001"].Inflight)
	}
	if got["b\x002"].MaxRunningRequests != 0 {
		t.Errorf("b.MaxRunningRequests = %d, want 0 (unlimited)", got["b\x002"].MaxRunningRequests)
	}
}

// failOpen re-admits everything health ejected when **everything** is ejected,
// because a misconfigured cluster name or a fleet-wide engine restart turns
// "reject" into a guaranteed 100% failure. There is no over-cap exclusion any
// more because over-cap is no longer a filter here at all -- the finisher's
// reserve owns that verdict.
func TestFailOpenReadmitsEjected(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2","maxRunningRequests":1}
	]}`)
	// Both ejected; b additionally at its (snapshot) cap, which must not
	// keep it out of the re-admission any more.
	states := map[string]clusterState{
		"a\x001": {Health: healthState{EjectedUntil: 5000, RampUntil: 6000}},
		"b\x002": {Inflight: 9, Health: healthState{EjectedUntil: 5000, RampUntil: 6000}},
	}
	set := buildCandidateSet(config, 1000, "", states)
	if len(set.Candidates) != 2 {
		t.Fatalf("published %v, want both ejected candidates re-admitted", set.Candidates)
	}
}

func TestFailClosedPublishesNothing(t *testing.T) {
	config := lbConfig(t, `{"health":{"failOpen":false},"candidates":[{"cluster":"a","targetId":"1"}]}`)
	states := map[string]clusterState{"a\x001": {Health: healthState{EjectedUntil: 5000, RampUntil: 6000}}}
	if set := buildCandidateSet(config, 1000, "", states); len(set.Candidates) != 0 {
		t.Fatalf("published %v, want nothing", set.Candidates)
	}
}

// A provider's single DNS cluster has many endpoints behind it, so one failed
// request must not take the whole thing out. Its in-flight count still counts.
func TestProviderIgnoresHealthButNotLoad(t *testing.T) {
	config := lbConfig(t, `{"candidates":[{"cluster":"p","targetId":"1","kind":"provider"}]}`)
	states := map[string]clusterState{
		"p\x001": {Inflight: 7, Health: healthState{EjectedUntil: 5000, RampUntil: 6000}},
	}
	set := buildCandidateSet(config, 1000, "", states)
	if len(set.Candidates) != 1 {
		t.Fatalf("published %v, want the provider", set.Candidates)
	}
	if set.Candidates[0].Inflight != 7 || set.Candidates[0].Probation {
		t.Errorf("provider candidate = %+v, want inflight 7 and no probation", set.Candidates[0])
	}
}

// Several candidates may share a cluster and differ only in targetId. Their
// **health** is the shared backend's (one ejection verdict for the cluster),
// but their in-flight budgets are separate: maxRunningRequests is configured
// per target, and merging the counts would spend target 2's requests out of
// target 1's budget.
func TestSharedClusterCandidatesShareHealthNotBudget(t *testing.T) {
	config := lbConfig(t, `{
		"candidates":[
			{"cluster":"a","targetId":"1","maxRunningRequests":2},
			{"cluster":"a","targetId":"2"}
		],
		"modelMappers":{"1":{"gpt-4o":"qwen3-32b"},"2":{"*":"deepseek-v3"}}
	}`)
	// Both ejected (cluster-level health), target 1 carrying 4 in-flight.
	states := map[string]clusterState{
		"a\x001": {Inflight: 4, Health: healthState{EjectedUntil: 9000, RampUntil: 9000}},
		"a\x002": {Inflight: 0, Health: healthState{EjectedUntil: 9000, RampUntil: 9000}},
	}
	set := buildCandidateSet(config, 1000, "gpt-4o", states)
	got := published(set)
	if len(got) != 2 {
		t.Fatalf("published %d candidates, want 2", len(got))
	}
	if got["a\x001"].Inflight != 4 || got["a\x002"].Inflight != 0 {
		t.Errorf("candidates on one cluster share an in-flight budget: %+v", got)
	}
	if got["a\x001"].ModelName != "qwen3-32b" {
		t.Errorf("targetId 1 resolved to %q", got["a\x001"].ModelName)
	}
	if got["a\x002"].ModelName != "deepseek-v3" {
		t.Errorf("targetId 2 resolved to %q", got["a\x002"].ModelName)
	}
}

func TestReserveOrderWinnerFirstThenPublishedOrder(t *testing.T) {
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "a", TargetID: "1"},
		{Cluster: "b", TargetID: "2"},
		{Cluster: "a", TargetID: "1"}, // duplicate Key: dropped
		{Cluster: "c", TargetID: "3"},
	}}
	pick := &set.Candidates[1] // b wins
	got := reserveOrder(set, pick, nil)
	if len(got) != 3 || got[0].Cluster != "b" || got[1].Cluster != "a" || got[2].Cluster != "c" {
		t.Errorf("reserveOrder = %v, want [b a c]", got)
	}
}

// A weighted set is a business traffic split; "the canary is full, send to
// stable" would silently rewrite the split the route promised. The walk stops
// at the winner.
func TestReserveOrderWeightedStopsAtWinner(t *testing.T) {
	w := int64(70)
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "a", TargetID: "1", Weight: &w},
		{Cluster: "b", TargetID: "2", Weight: &w},
	}}
	got := reserveOrder(set, &set.Candidates[0], []float64{0, 1})
	if len(got) != 1 || got[0].Cluster != "a" {
		t.Errorf("reserveOrder = %v, want only the winner", got)
	}
}

func TestReserveOrderNoPickIsEmpty(t *testing.T) {
	set := CandidateSet{Candidates: []Candidate{{Cluster: "a"}}}
	if got := reserveOrder(set, nil, nil); got != nil {
		t.Errorf("reserveOrder(nil pick) = %v, want nil", got)
	}
}

func TestReserveOrderPreservesScoresAfterFullWinner(t *testing.T) {
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "a", Inflight: 90, MaxRunningRequests: 100},
		{Cluster: "b", Inflight: 1, MaxRunningRequests: 1},
		{Cluster: "c", Inflight: 2, MaxRunningRequests: 100},
	}}
	totals, _ := combineRanks(set.Candidates, []RankEntry{{
		Weight: 1,
		Scores: map[string]float64{"a": 1.0 / 91, "b": 1.0 / 2, "c": 1.0 / 3},
	}})
	ordered := reserveOrder(set, bestTotal(set.Candidates, totals), totals)
	for _, c := range ordered {
		if c.MaxRunningRequests > 0 && c.Inflight >= c.MaxRunningRequests {
			continue
		}
		if c.Cluster != "c" {
			t.Fatalf("fallback = %s (inflight=%d), want c (inflight=2)", c.Cluster, c.Inflight)
		}
		return
	}
	t.Fatal("no available candidate")
}

func TestReserveOrderRandomizesTiedFallbacks(t *testing.T) {
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "a", TargetID: "1"},
		{Cluster: "b", TargetID: "2"},
		{Cluster: "a", TargetID: "1"},
		{Cluster: "c", TargetID: "3"},
		{Cluster: "c", TargetID: "4"},
	}}
	seen := map[string]bool{}
	for range 200 {
		ordered := reserveOrder(set, &set.Candidates[1], []float64{1, 2, 1, 1, 0})
		if len(ordered) != 4 || ordered[0].Cluster != "b" || ordered[3].TargetID != "4" {
			t.Fatalf("unexpected reserve order: %+v", ordered)
		}
		seen[ordered[1].Key()] = true
	}
	if len(seen) != 2 {
		t.Fatalf("equal-score fallbacks should both be reachable, saw %v", seen)
	}
	if set.Candidates[0].Cluster != "a" || set.Candidates[1].Cluster != "b" {
		t.Fatal("reserve order mutated the published candidates")
	}
}
