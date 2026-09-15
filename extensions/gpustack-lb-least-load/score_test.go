package main

import (
	"math"
	"testing"
)

func c(cluster string, inflight int64, penalty float64) candidate {
	return candidate{Cluster: cluster, Inflight: inflight, Penalty: penalty}
}

// The score must decrease strictly as load rises -- that is the whole content
// of the "higher is better" convention, and the only premise under which the
// finisher's argmax picks the least busy candidate.
func TestScoreIsStrictlyDecreasing(t *testing.T) {
	prev := math.Inf(1)
	for _, load := range []float64{0, 0.5, 1, 2, 5, 12, 100, 1e6} {
		s := loadScore(load)
		if s >= prev {
			t.Errorf("load %v → %v, not below the previous %v", load, s, prev)
		}
		if s <= 0 || s > 1 {
			t.Errorf("load %v → %v, outside (0,1]", load, s)
		}
		prev = s
	}
}

// The penalty is in the same unit as the in-flight count and can simply be
// added: the publisher has already converted it into in-flight terms
// (Penalty = decay fraction × (max in-flight + 1)), so this plugin does not
// need to know rampMs.
func TestPenaltyIsAdditiveWithInflight(t *testing.T) {
	if a, b := loadScore(effectiveLoad(ptr(c("x", 5, 0)))), loadScore(effectiveLoad(ptr(c("x", 0, 5)))); a != b {
		t.Errorf("5 in flight and a penalty of 5 should score the same: %v vs %v", a, b)
	}
	if got := effectiveLoad(ptr(c("x", 2, 3.5))); got != 5.5 {
		t.Errorf("effectiveLoad = %v, want 5.5", got)
	}
}

// Equal loads must produce **bit-for-bit equal** float64 values, or the
// finisher's reservoir sampling never triggers -- on an idle cluster every
// candidate is at zero in flight, which is the normal case rather than an edge
// case, and without randomisation it would cause a thundering herd.
func TestEqualLoadsTieExactly(t *testing.T) {
	scores := loadScores([]candidate{c("a", 0, 0), c("b", 0, 0), c("c", 0, 0)})
	if scores["a"] != scores["b"] || scores["b"] != scores["c"] {
		t.Errorf("equal loads must tie bit-for-bit, got %v", scores)
	}
	// Equal penalties must tie too.
	same := loadScores([]candidate{c("a", 1, 2.5), c("b", 1, 2.5)})
	if same["a"] != same["b"] {
		t.Errorf("got %v", same)
	}
}

// The candidate set is written into filter state by **another binary**, so it
// is treated as external input. load = -1 would make 1/(1+load) divide by zero,
// and -1 sits squarely inside the range of negative values.
func TestAbsurdInputsAreClamped(t *testing.T) {
	tests := []struct {
		name string
		in   candidate
	}{
		{"negative penalty", c("x", 0, -1)},
		{"negative inflight", c("x", -5, 0)},
		{"cancelling out to -1", c("x", 2, -3)},
		{"large negatives", c("x", -1000, -1000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := loadScore(effectiveLoad(&tt.in))
			if math.IsInf(s, 0) || math.IsNaN(s) || s <= 0 || s > 1 {
				t.Errorf("score = %v, must stay in (0,1]", s)
			}
		})
	}
}

// Walk through the worked example from the design doc: A at 12 in flight, B at
// 2, C just released from cooldown (0 in flight, penalty 7.8). C has the lowest
// in-flight count, but the penalty holds it between A and B -- without the
// penalty it would be saturated the instant it came back.
func TestWorkedExampleFromDesign(t *testing.T) {
	scores := loadScores([]candidate{
		c("A", 12, 0),
		c("B", 2, 0),
		c("C", 0, 7.8),
	})
	if !(scores["B"] > scores["C"] && scores["C"] > scores["A"]) {
		t.Errorf("want B > C > A, got %v", scores)
	}
	// Only once the penalty has decayed to 1.3 should C overtake B (the
	// crossover sits around B's load of 2).
	decayed := loadScores([]candidate{c("B", 2, 0), c("C", 0, 1.3)})
	if decayed["C"] <= decayed["B"] {
		t.Errorf("decayed C should overtake B, got %v", decayed)
	}
}

// Candidates carrying a weight must trigger an early exit: this route has a
// business split, the finisher will ignore every scoring opinion, continuing is
// pure waste, and it would leave an entry in the logs that never took effect.
func TestIsWeighted(t *testing.T) {
	zero, ten := int64(0), int64(10)
	tests := []struct {
		name string
		in   []candidate
		want bool
	}{
		{"none carry one", []candidate{c("a", 0, 0), c("b", 0, 0)}, false},
		{"all carry one", []candidate{{Cluster: "a", Weight: &ten}}, true},
		// weight: 0 is a valid value (a canary dialled down to 0%) and must not
		// be taken for "unset".
		{"a zero weight", []candidate{{Cluster: "a", Weight: &zero}}, true},
		{"mixed", []candidate{c("a", 0, 0), {Cluster: "b", Weight: &ten}}, true},
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

// Every candidate must get a score, with none left out -- a missing candidate
// reads at the finisher as "this entry does not cover it", which can keep a
// perfectly selectable instance from being selected.
func TestEveryCandidateScored(t *testing.T) {
	cands := []candidate{c("a", 1, 0), c("b", 2, 0), c("c", 3, 0)}
	scores := loadScores(cands)
	if len(scores) != len(cands) {
		t.Fatalf("got %d scores for %d candidates: %v", len(scores), len(cands), scores)
	}
	for _, cc := range cands {
		if _, ok := scores[cc.Cluster]; !ok {
			t.Errorf("%s missing", cc.Cluster)
		}
	}
}

func ptr(c candidate) *candidate { return &c }

// Candidates sharing a cluster legitimately score the same -- one cluster is
// one backend with one in-flight count -- but they still need one map entry
// each. With a cluster key they shared an entry, and the finisher's L1 sum
// (which iterates candidates) then counted that one score once per duplicate,
// handing the cluster a multiple of the vote it actually cast.
func TestSameClusterCandidatesGetOneEntryEach(t *testing.T) {
	cands := []candidate{
		{Cluster: "X", TargetID: "2", Inflight: 4},
		{Cluster: "X", TargetID: "3", Inflight: 4},
		{Cluster: "Y", TargetID: "4", Inflight: 1},
	}
	scores := loadScores(cands)

	if len(scores) != len(cands) {
		t.Fatalf("got %d scores for %d candidates: %v", len(scores), len(cands), scores)
	}
	// Same cluster, same load, so the two must still score identically.
	if scores[cands[0].Key()] != scores[cands[1].Key()] {
		t.Errorf("candidates on one cluster should score the same: %v", scores)
	}
	// And the less loaded cluster must still win.
	if scores[cands[2].Key()] <= scores[cands[0].Key()] {
		t.Errorf("Y is less loaded and should score higher: %v", scores)
	}
}
