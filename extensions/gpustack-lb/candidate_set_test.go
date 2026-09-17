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
		"a": {Health: healthState{EjectedUntil: 2000, RampUntil: 3000}},
		"b": {Inflight: 5},
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
		"a": {Health: healthState{EjectedUntil: 1000, RampUntil: 2000}},
		"b": {Inflight: 3},
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
		"a": {Health: healthState{EjectedUntil: 1000, RampUntil: 2000}},
		"b": {Inflight: 2},
		// Ejected, and carrying a pile of leaked in-flight entries.
		"c": {Inflight: 500, Health: healthState{EjectedUntil: 9000, RampUntil: 9000}},
	}
	set := buildCandidateSet(config, 1500, "", states)
	// maxLoad is b's 2, not c's 500: 0.5 * (2+1)
	if got := published(set)["a\x001"].Penalty; got != 1.5 {
		t.Errorf("a.Penalty = %v, want 1.5", got)
	}
}

func TestOverCapCandidateIsNotPublished(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1","maxRunningRequests":4},
		{"cluster":"b","targetId":"2"}
	]}`)
	set := buildCandidateSet(config, 1000, "", map[string]clusterState{"a": {Inflight: 4}})
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "b" {
		t.Fatalf("published %v, want only b", set.Candidates)
	}
}

// failOpen re-admits everything health ejected, because a misconfigured cluster
// name or a fleet-wide engine restart turns "reject" into a guaranteed 100%
// failure. It deliberately does **not** re-admit over-capacity candidates --
// that is a rate-limiting semantic, not a health one.
func TestFailOpenReadmitsEjectedButNotOverCap(t *testing.T) {
	config := lbConfig(t, `{"candidates":[
		{"cluster":"a","targetId":"1"},
		{"cluster":"b","targetId":"2","maxRunningRequests":1}
	]}`)
	states := map[string]clusterState{
		"a": {Health: healthState{EjectedUntil: 5000, RampUntil: 6000}},
		"b": {Inflight: 9},
	}
	set := buildCandidateSet(config, 1000, "", states)
	if len(set.Candidates) != 1 || set.Candidates[0].Cluster != "a" {
		t.Fatalf("published %v, want only the ejected a re-admitted", set.Candidates)
	}
}

func TestFailClosedPublishesNothing(t *testing.T) {
	config := lbConfig(t, `{"health":{"failOpen":false},"candidates":[{"cluster":"a","targetId":"1"}]}`)
	states := map[string]clusterState{"a": {Health: healthState{EjectedUntil: 5000, RampUntil: 6000}}}
	if set := buildCandidateSet(config, 1000, "", states); len(set.Candidates) != 0 {
		t.Fatalf("published %v, want nothing", set.Candidates)
	}
}

// A provider's single DNS cluster has many endpoints behind it, so one failed
// request must not take the whole thing out. Its in-flight count still counts.
func TestProviderIgnoresHealthButNotLoad(t *testing.T) {
	config := lbConfig(t, `{"candidates":[{"cluster":"p","targetId":"1","kind":"provider"}]}`)
	states := map[string]clusterState{
		"p": {Inflight: 7, Health: healthState{EjectedUntil: 5000, RampUntil: 6000}},
	}
	set := buildCandidateSet(config, 1000, "", states)
	if len(set.Candidates) != 1 {
		t.Fatalf("published %v, want the provider", set.Candidates)
	}
	if set.Candidates[0].Inflight != 7 || set.Candidates[0].Probation {
		t.Errorf("provider candidate = %+v, want inflight 7 and no probation", set.Candidates[0])
	}
}

// Several candidates may share a cluster and differ only in targetId. They
// share the backend, so they share the state -- but each resolves its own
// rewrite target.
func TestSharedClusterCandidatesShareStateAndResolveSeparately(t *testing.T) {
	config := lbConfig(t, `{
		"candidates":[
			{"cluster":"a","targetId":"1"},
			{"cluster":"a","targetId":"2"}
		],
		"modelMappers":{"1":{"gpt-4o":"qwen3-32b"},"2":{"*":"deepseek-v3"}}
	}`)
	set := buildCandidateSet(config, 1000, "gpt-4o", map[string]clusterState{"a": {Inflight: 4}})
	got := published(set)
	if len(got) != 2 {
		t.Fatalf("published %d candidates, want 2", len(got))
	}
	if got["a\x001"].Inflight != 4 || got["a\x002"].Inflight != 4 {
		t.Error("candidates on one cluster disagree about its in-flight count")
	}
	if got["a\x001"].ModelName != "qwen3-32b" {
		t.Errorf("targetId 1 resolved to %q", got["a\x001"].ModelName)
	}
	if got["a\x002"].ModelName != "deepseek-v3" {
		t.Errorf("targetId 2 resolved to %q", got["a\x002"].ModelName)
	}
}
