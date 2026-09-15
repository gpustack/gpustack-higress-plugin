package main

import (
	"fmt"
	"math"
	"testing"
)

// These tests only cover the **pure functions**: selectWeighted, roundRobin,
// combineRanks and bestTotal make no host calls, so plain go test runs them
// without a wasm host.
//
// selectScored reads filter state and is not covered here -- but its
// combination logic is split out into the pure combineRanks, so the part that
// is actually easy to get numerically wrong is covered.

// cands builds candidates that carry a weight (weighted mode).
func cands(specs ...[2]any) []Candidate {
	out := make([]Candidate, 0, len(specs))
	for _, s := range specs {
		w := int64(s[1].(int))
		out = append(out, Candidate{Cluster: s[0].(string), Weight: &w})
	}
	return out
}

// unweighted builds candidates with no weight (scored / round-robin path).
func unweighted(names ...string) []Candidate {
	out := make([]Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, Candidate{Cluster: n})
	}
	return out
}

// The criterion is decided by whether the candidates carry a weight; there is
// no separate selection switch.
func TestDispatchIsDecidedByWeightPresence(t *testing.T) {
	if !isWeighted(cands([2]any{"a", 10})) {
		t.Error("candidates carrying weight should dispatch to weighted")
	}
	if isWeighted(unweighted("a", "b")) {
		t.Error("candidates without weight should not dispatch to weighted")
	}
	// weight: 0 is meaningful (a canary dialled to 0%) and must not be treated
	// as absent.
	zero := int64(0)
	if !isWeighted([]Candidate{{Cluster: "a", Weight: &zero}}) {
		t.Error("weight:0 must still count as weighted — it means 0%, not absent")
	}
	// A mixture is treated as weighted, with the missing ones as 0 (never
	// selected) -- safer than ignoring the split.
	mixed := append(cands([2]any{"a", 10}), Candidate{Cluster: "b"})
	if !isWeighted(mixed) {
		t.Error("a mixed set should dispatch to weighted")
	}
}

func TestSelectWeightedSingleCandidate(t *testing.T) {
	set := CandidateSet{Candidates: cands([2]any{"a", 7})}
	for i := 0; i < 20; i++ {
		got := selectWeighted(set, fmt.Sprintf("req-%d", i))
		if got.Cluster != "a" {
			t.Fatalf("want a, got %s", got.Cluster)
		}
	}
}

// Determinism is the core property of weighted mode: the same x-request-id
// must always land on the same candidate, or an internal redirect (a fallback
// re-running the filter chain) would roll the request somewhere else.
func TestSelectWeightedIsDeterministic(t *testing.T) {
	set := CandidateSet{Candidates: cands(
		[2]any{"a", 10}, [2]any{"b", 30}, [2]any{"c", 60},
	)}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("req-%d", i)
		first := selectWeighted(set, id).Cluster
		for k := 0; k < 5; k++ {
			if got := selectWeighted(set, id).Cluster; got != first {
				t.Fatalf("req %s: %s then %s", id, first, got)
			}
		}
	}
}

func TestSelectWeightedDistribution(t *testing.T) {
	set := CandidateSet{Candidates: cands(
		[2]any{"a", 10}, [2]any{"b", 90},
	)}
	const n = 20000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[selectWeighted(set, fmt.Sprintf("req-%d", i)).Cluster]++
	}
	for cluster, want := range map[string]float64{"a": 0.10, "b": 0.90} {
		got := float64(counts[cluster]) / n
		if math.Abs(got-want) > 0.02 {
			t.Errorf("%s: want ~%.2f, got %.4f", cluster, want, got)
		}
	}
}

// When the weights sum to 0 (the field is absent, or they really are all zero)
// the candidates are treated as equal, and it **must not divide by zero**.
func TestSelectWeightedAllZeroIsEqualWeight(t *testing.T) {
	set := CandidateSet{Candidates: cands(
		[2]any{"a", 0}, [2]any{"b", 0}, [2]any{"c", 0},
	)}
	const n = 9000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[selectWeighted(set, fmt.Sprintf("req-%d", i)).Cluster]++
	}
	if len(counts) != 3 {
		t.Fatalf("expected all three candidates to be reachable, got %v", counts)
	}
	for cluster, c := range counts {
		if got := float64(c) / n; math.Abs(got-1.0/3) > 0.02 {
			t.Errorf("%s: want ~0.333, got %.4f", cluster, got)
		}
	}
}

// A candidate with weight 0 must never be selected -- it is still in the set
// (healthy, usable), its share is simply zero. This is the property a canary
// needs when it is dialled down to 0%.
func TestSelectWeightedSkipsZeroWeight(t *testing.T) {
	set := CandidateSet{Candidates: cands(
		[2]any{"a", 0}, [2]any{"b", 100},
	)}
	for i := 0; i < 5000; i++ {
		if got := selectWeighted(set, fmt.Sprintf("req-%d", i)).Cluster; got == "a" {
			t.Fatalf("zero-weight candidate selected at i=%d", i)
		}
	}
}

// The interval split depends on candidate order, so the order has to be fixed
// (the publisher sorts by full cluster name). This test pins down why the
// order cannot be changed casually: reorder the same candidates and the same
// request id may land on a different one.
func TestSelectWeightedIsOrderSensitive(t *testing.T) {
	forward := CandidateSet{Candidates: cands([2]any{"a", 50}, [2]any{"b", 50})}
	reverse := CandidateSet{Candidates: cands([2]any{"b", 50}, [2]any{"a", 50})}

	differs := false
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("req-%d", i)
		if selectWeighted(forward, id).Cluster != selectWeighted(reverse, id).Cluster {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("expected interval assignment to depend on candidate order; " +
			"if this ever passes, the sorted-by-cluster-name contract is no longer load-bearing")
	}
}

// With the candidate set unchanged, rebuilding the same set must produce the
// same split -- the premise behind "removing an instance and adding it back
// does not redistribute traffic".
func TestSelectWeightedStableAcrossRebuilds(t *testing.T) {
	build := func() CandidateSet {
		return CandidateSet{Candidates: cands(
			[2]any{"a", 11}, [2]any{"b", 12}, [2]any{"c", 77},
		)}
	}
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("req-%d", i)
		if selectWeighted(build(), id).Cluster != selectWeighted(build(), id).Cluster {
			t.Fatalf("unstable assignment for %s", id)
		}
	}
}

func TestRoundRobinCycles(t *testing.T) {
	list := unweighted("a", "b", "c")

	rrCursor = 0
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, roundRobin(list).Cluster)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: want %s, got %s (%v)", i, want[i], got[i], got)
		}
	}
}

// The round-robin counter lives in per-VM memory, so a VM rebuild resets it --
// harmless, because the union of several independent even rotations is still
// even. This pins down that it restarts from the beginning after a reset.
func TestRoundRobinResetIsHarmless(t *testing.T) {
	list := unweighted("a", "b")
	rrCursor = 0
	first := roundRobin(list).Cluster
	rrCursor = 0
	if again := roundRobin(list).Cluster; again != first {
		t.Fatalf("after reset want %s, got %s", first, again)
	}
}

// -- Weighted sum (the llm-d model).

func ranked(name string, weight float64, scores map[string]float64) RankEntry {
	return RankEntry{Name: name, Weight: weight, Scores: scores}
}

// With a single entry, L1 normalisation does not change the argmax, but it
// must scale the scores so the entry casts exactly one vote.
func TestCombineSingleRankNormalisesToOneVote(t *testing.T) {
	list := unweighted("a", "b", "c")
	totals, ok := combineRanks(list, []RankEntry{
		ranked("x", 1, map[string]float64{"a": 1, "b": 3, "c": 4}),
	})
	if !ok {
		t.Fatal("expected a contribution")
	}
	want := []float64{0.125, 0.375, 0.5} // /8
	sum := 0.0
	for i := range want {
		if math.Abs(totals[i]-want[i]) > 1e-9 {
			t.Errorf("position %d: got %v, want %v", i, totals[i], want[i])
		}
		sum += totals[i]
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("a single weight-1 rank must cast exactly one vote, got %v", sum)
	}
}

// **This is the core benefit of moving to a weighted sum**: stickiness holds
// against a trivial load gap but yields to a significant one. Under a
// first-match chain stickiness won unconditionally; under min-max
// normalisation stickiness is always overturned (min-max stretches both a gap
// of 1 and a gap of 1000 to the full 1.0/0.0). Only L1 can express *how far
// apart* they are.
func TestStickinessYieldsOnlyToRealLoadGaps(t *testing.T) {
	list := unweighted("home", "other")
	// affinity: 0.5^(rank-1), with home as the sticky owner
	aff := ranked("affinity", 1, map[string]float64{"home": 1.0, "other": 0.5})

	tests := []struct {
		name          string
		homeLoad, oth float64 // raw 1/(1+load)
		want          string
	}{
		// load 101 vs 100: raw 0.00980 vs 0.00990, near neutral -> stickiness holds
		{"trivial load gap", 1.0 / 102, 1.0 / 101, "home"},
		// load 1000 vs 10: raw 0.000999 vs 0.0909, far apart -> load wins
		{"large load gap", 1.0 / 1001, 1.0 / 11, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			load := ranked("least-load", 1, map[string]float64{"home": tt.homeLoad, "other": tt.oth})
			totals, ok := combineRanks(list, []RankEntry{aff, load})
			if !ok {
				t.Fatal("expected a contribution")
			}
			got := "home"
			if totals[1] > totals[0] {
				got = "other"
			}
			if got != tt.want {
				t.Errorf("picked %s, want %s (totals %v)", got, tt.want, totals)
			}
		})
	}
}

// The weight is a vote multiplier: with the same scores, doubling the weight
// doubles the contribution.
func TestWeightScalesContribution(t *testing.T) {
	list := unweighted("a", "b")
	scores := map[string]float64{"a": 3, "b": 1}
	one, _ := combineRanks(list, []RankEntry{ranked("x", 1, scores)})
	two, _ := combineRanks(list, []RankEntry{ranked("x", 2, scores)})
	for i := range one {
		if math.Abs(two[i]-2*one[i]) > 1e-9 {
			t.Errorf("position %d: weight 2 gave %v, want 2× %v", i, two[i], one[i])
		}
	}
}

// An absent, zero, negative or NaN weight all become 1 -- zero is not a
// meaningful value ("do not participate" belongs in the plugin's own enabled
// switch, or in simply not publishing an entry).
func TestWeightFallsBackToOne(t *testing.T) {
	list := unweighted("a", "b")
	scores := map[string]float64{"a": 3, "b": 1}
	base, _ := combineRanks(list, []RankEntry{ranked("x", 1, scores)})
	for _, w := range []float64{0, -5, math.NaN()} {
		got, _ := combineRanks(list, []RankEntry{ranked("x", w, scores)})
		for i := range base {
			if math.Abs(got[i]-base[i]) > 1e-9 {
				t.Errorf("weight %v: position %d got %v, want %v", w, i, got[i], base[i])
			}
		}
	}
}

// Scoring only a subset is legal (prefix affinity has exactly this shape):
// candidates absent from the map score 0, and the ones present are normalised
// **within the subset**.
func TestSubsetScoringIsNormalisedWithinTheSubset(t *testing.T) {
	list := unweighted("a", "b", "c")
	totals, ok := combineRanks(list, []RankEntry{
		ranked("prefix", 1, map[string]float64{"a": 1, "b": 1}),
	})
	if !ok {
		t.Fatal("expected a contribution")
	}
	if math.Abs(totals[0]-0.5) > 1e-9 || math.Abs(totals[1]-0.5) > 1e-9 || totals[2] != 0 {
		t.Errorf("got %v, want [0.5 0.5 0]", totals)
	}
}

// An entry that comes up empty (abstained, all zeros, or every candidate it
// scored is gone) must be **skipped entirely** rather than spread evenly --
// spreading would hand a free bonus to the subset in its map when it actually
// said nothing.
func TestEmptyRankIsSkippedNotSpread(t *testing.T) {
	list := unweighted("a", "b")
	cases := []RankEntry{
		ranked("silent", 1, nil),
		ranked("zeros", 1, map[string]float64{"a": 0, "b": 0}),
		ranked("ghosts", 1, map[string]float64{"gone": 1.0}),
		ranked("negatives", 1, map[string]float64{"a": -1, "b": -2}),
	}
	for _, r := range cases {
		t.Run(r.Name, func(t *testing.T) {
			totals, ok := combineRanks(list, []RankEntry{r})
			if ok {
				t.Errorf("should not have contributed, got %v", totals)
			}
			for i, v := range totals {
				if v != 0 {
					t.Errorf("position %d got %v, want 0", i, v)
				}
			}
		})
	}
	// But as long as one entry is valid, the combination counts as contributing.
	if _, ok := combineRanks(list, []RankEntry{cases[0], ranked("real", 1, map[string]float64{"a": 1})}); !ok {
		t.Error("a valid rank alongside empty ones must still contribute")
	}
}

// Negative scores are treated as 0: they cannot take part in L1 (which is
// meaningless over negatives) and must not shrink the sum.
func TestNegativeScoresTreatedAsZero(t *testing.T) {
	list := unweighted("a", "b")
	totals, _ := combineRanks(list, []RankEntry{
		ranked("x", 1, map[string]float64{"a": 3, "b": -100}),
	})
	if math.Abs(totals[0]-1.0) > 1e-9 || totals[1] != 0 {
		t.Errorf("got %v, want [1 0]", totals)
	}
}

func TestBestTotalPicksHighest(t *testing.T) {
	list := unweighted("a", "b", "c")
	got := bestTotal(list, []float64{0.1, 0.9, 0.5})
	if got == nil || got.Cluster != "b" {
		t.Fatalf("want b, got %v", got)
	}
}

// Ties are broken by reservoir sampling to avoid a thundering herd -- but the
// randomisation must stay within the tied set.
func TestBestTotalTiesStayWithinTiedSet(t *testing.T) {
	list := unweighted("a", "b", "c")
	totals := []float64{1.0, 1.0, 0.5}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		got := bestTotal(list, totals)
		if got == nil {
			t.Fatal("unexpected nil")
		}
		if got.Cluster == "c" {
			t.Fatal("picked a lower-scoring candidate")
		}
		seen[got.Cluster] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected both tied candidates to be reachable, saw %v", seen)
	}
}

func TestSelectCandidateEmptySetReturnsNil(t *testing.T) {
	if got := selectCandidate(CandidateSet{}); got != nil {
		t.Fatalf("want nil for empty candidate set, got %v", got)
	}
}

// An empty candidate set must never reach the modulo -- a panic inside wasm
// means this request simply fails. The caller already guards once; this is the
// second layer.
func TestSelectorsGuardEmptyCandidates(t *testing.T) {
	if got := roundRobin(nil); got != nil {
		t.Errorf("roundRobin(nil) = %v, want nil", got)
	}
	if got := selectWeighted(CandidateSet{}, "req"); got != nil {
		t.Errorf("selectWeighted(empty) = %v, want nil", got)
	}
}

// The round-robin start is random (one per VM, so worker threads are not in
// phase at startup), but within a single VM it must still be a strict
// rotation: from whatever start, len(cands) consecutive calls must cover every
// candidate exactly once.
//
// The uint64 wrap point is deliberately not tested: 2^64 is not a multiple of
// the candidate count, so the 2^64-1 -> 0 step repeats one candidate. That gap
// is real, but at 1M rps it takes about 580,000 years to reach, and writing
// code for it would be pure over-engineering.
//
// In production the candidate count also changes per request as health
// filtering kicks in, so a strict rotation only holds within a window where
// the candidate set is unchanged anyway -- round-robin never promised order,
// only evenness.
func TestRoundRobinCoversAllFromAnyStart(t *testing.T) {
	list := unweighted("a", "b", "c")
	for _, start := range []uint64{0, 1, 2, 7, 1 << 40} {
		rrCursor = start
		seen := map[string]int{}
		for i := 0; i < len(list); i++ {
			seen[roundRobin(list).Cluster]++
		}
		if len(seen) != len(list) {
			t.Fatalf("start=%d: covered %v, want all %d candidates", start, seen, len(list))
		}
		for cluster, n := range seen {
			if n != 1 {
				t.Fatalf("start=%d: %s hit %d times in one cycle", start, cluster, n)
			}
		}
	}
}

// An absurd set of weights must not wrap the int64 total negative: that lands
// in the "total <= 0" branch anyway, but by accident and only for the
// deployment that configured it. Detecting the overflow makes the fallback to
// equal treatment deliberate, and keeps a candidate from being reachable only
// through a wrapped modulo.
func TestWeightedTotalOverflowFallsBackToEqual(t *testing.T) {
	huge := int64(math.MaxInt64 - 1)
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "a", Weight: &huge},
		{Cluster: "b", Weight: &huge},
	}}

	seen := map[string]int{}
	for i := 0; i < 400; i++ {
		got := selectWeighted(set, fmt.Sprintf("req-%d", i))
		if got == nil {
			t.Fatal("unexpected nil")
		}
		seen[got.Cluster]++
	}
	if len(seen) != 2 {
		t.Fatalf("both candidates should stay reachable, saw %v", seen)
	}
	// Equal treatment, so neither should be starved. The bound is loose on
	// purpose -- this asserts "not wedged", not a distribution.
	for cluster, n := range seen {
		if n < 100 {
			t.Errorf("%s picked %d/400 times, expected roughly even", cluster, n)
		}
	}
}

// gpustack serves several model names on one route from the same cluster,
// separated only by targetId, and each carries its own rewrite target. Scores
// are keyed by Candidate.Key() so those candidates stay distinct through the
// weighted sum.
//
// Keying by cluster instead collapsed them three ways at once: the score map
// lost an entry, the L1 sum (which iterates candidates) counted the shared
// score once per duplicate and doubled that cluster's vote, and the resulting
// exact tie sent the finisher's tie-break to random -- so one sticky session
// flipped between two different models request by request.
func TestSameClusterCandidatesStayDistinct(t *testing.T) {
	cands := []Candidate{
		{Cluster: "X", TargetID: "2", ModelName: "qwen3-32b"},
		{Cluster: "X", TargetID: "3", ModelName: "deepseek-v3"},
		{Cluster: "Y", TargetID: "4", ModelName: "llama-70b"},
	}

	// An affinity-shaped entry: rank decay over the three candidates.
	scores := map[string]float64{
		cands[0].Key(): 1.0,
		cands[1].Key(): 0.5,
		cands[2].Key(): 0.25,
	}
	if len(scores) != len(cands) {
		t.Fatalf("one map entry per candidate expected, got %d for %d", len(scores), len(cands))
	}

	totals, ok := combineRanks(cands, []RankEntry{ranked("affinity", 1, scores)})
	if !ok {
		t.Fatal("expected a contribution")
	}

	// Strictly ordered, so the top-ranked candidate wins outright: no tie, and
	// therefore no random model rewrite.
	if !(totals[0] > totals[1] && totals[1] > totals[2]) {
		t.Fatalf("totals should be strictly ordered, got %v", totals)
	}
	for i := 0; i < 200; i++ {
		if got := bestTotal(cands, totals); got.ModelName != "qwen3-32b" {
			t.Fatalf("sticky pick flipped to %q", got.ModelName)
		}
	}

	// The vote is L1-normalised over candidates, so it sums to the entry's
	// weight -- the duplicated cluster gets no bonus for appearing twice.
	sum := totals[0] + totals[1] + totals[2]
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("weighted sum = %v, want 1.0", sum)
	}
	// X appears twice, Y once. X's aggregate must reflect its two *scores*
	// (1.0+0.5 of 1.75), not a doubling of one shared score.
	if x, y := totals[0]+totals[1], totals[2]; math.Abs(x-1.5/1.75) > 1e-9 || math.Abs(y-0.25/1.75) > 1e-9 {
		t.Errorf("aggregate X=%v Y=%v, want %v / %v", x, y, 1.5/1.75, 0.25/1.75)
	}
}
