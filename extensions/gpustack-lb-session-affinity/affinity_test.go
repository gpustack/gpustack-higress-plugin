package main

import (
	"fmt"
	"math"
	"sort"
	"testing"
)

func clusters(n int) []candidate {
	out := make([]candidate, n)
	for i := range out {
		// Deliberately the real naming, differing only in the last character
		// or two: exactly the shape a poorly avalanching hash skews on.
		out[i] = candidate{Cluster: fmt.Sprintf("outbound|80||model-2-%d.static", 10+i)}
	}
	return out
}

// top returns this plugin's first choice, equivalent to what the finisher
// picks when this is the only entry.
func top(cands []candidate, key string) string {
	scores := affinityScores(cands, key)
	best, bestScore := "", math.Inf(-1)
	for _, c := range cands {
		if s := scores[c.Cluster]; s > bestScore {
			best, bestScore = c.Cluster, s
		}
	}
	return best
}

// rank returns the clusters ordered by descending score.
func rank(cands []candidate, key string) []string {
	scores := affinityScores(cands, key)
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Cluster)
	}
	sort.Slice(names, func(i, j int) bool { return scores[names[i]] > scores[names[j]] })
	return names
}

// The basic promise of stickiness: the same key over the same candidate set
// always gives the same result.
func TestSameKeySameChoice(t *testing.T) {
	cands := clusters(5)
	for _, key := range []string{"sess-a", "resp_68f0", "a-key-of-a-different-length"} {
		want := top(cands, key)
		for i := 0; i < 50; i++ {
			if got := top(cands, key); got != want {
				t.Fatalf("key %q: run %d gave %s, want %s", key, i, got, want)
			}
		}
	}
}

// Candidate order must not affect the result. The publisher sorts the set
// lexicographically, but this plugin must not rely on that -- once it does,
// anyone changing the publishing order later would reshuffle every session.
func TestOrderIndependent(t *testing.T) {
	cands := clusters(6)
	reversed := make([]candidate, len(cands))
	for i := range cands {
		reversed[len(cands)-1-i] = cands[i]
	}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("sess-%d", i)
		if a, b := top(cands, key), top(reversed, key); a != b {
			t.Fatalf("key %q: %s vs %s (reversed order)", key, a, b)
		}
	}
}

// **HRW's core property**, and the entire reason for not using hash%N:
// removing one candidate only reassigns the sessions that lived on it, and
// leaves every other one untouched.
//
// hash%N would disturb roughly (N-1)/N of the sessions here -- and the
// candidate set changes at precisely the moments stickiness matters most
// (scaling, instances ejected by passive health).
func TestRemovalOnlyDisturbsItsOwn(t *testing.T) {
	const keys = 5000
	cands := clusters(5)
	victim := cands[2].Cluster

	remaining := make([]candidate, 0, len(cands)-1)
	for _, c := range cands {
		if c.Cluster != victim {
			remaining = append(remaining, c)
		}
	}

	movedOffVictim, movedOthers := 0, 0
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("sess-%d", i)
		before, after := top(cands, key), top(remaining, key)
		switch {
		case before == victim:
			movedOffVictim++
		case before != after:
			movedOthers++
		}
	}

	if movedOthers != 0 {
		t.Errorf("%d/%d sessions moved that didn't have to; HRW must only "+
			"reassign the removed node's own share", movedOthers, keys)
	}
	if movedOffVictim == 0 {
		t.Fatal("no session was on the removed candidate; test is vacuous")
	}
}

// Adding a candidate back should likewise only take the share that belongs to
// it.
func TestAdditionOnlyTakesItsShare(t *testing.T) {
	const keys = 5000
	small := clusters(4)
	grown := clusters(5)
	newcomer := grown[4].Cluster

	disturbed := 0
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("sess-%d", i)
		before, after := top(small, key), top(grown, key)
		if before != after && after != newcomer {
			disturbed++
		}
	}
	if disturbed != 0 {
		t.Errorf("%d/%d sessions were reshuffled among the old candidates", disturbed, keys)
	}
}

// The distribution has to be even. **This test is the guard for the mix64
// finalizer**: plain FNV-1a avalanches poorly, and our cluster names differ
// only in the last character or two (model-2-10 / model-2-11), so their high
// bits are highly correlated -- while HRW compares nothing but which score is
// highest.
//
// Measured maximum bin deviation (20000 keys):
//
//	candidates   plain FNV-1a   with mix64
//	         3           6.2%         0.7%
//	         5           8.4%         2.3%
//	        16          14.1%         6.6%
//	        32          23.2%        10.1%
//
// The skew grows with the candidate count, so 16 candidates are required here:
// at 8 both sit in the noise (3.2% vs 4.1%) and the test would still pass with
// the mixer removed, guarding nothing. With 40000 keys over 16 buckets, pure
// sampling noise peaks around 4%, so an 8% threshold lets the mix64 version
// pass reliably and the plain-FNV version (~14%) fail reliably.
func TestDistributionIsEven(t *testing.T) {
	const (
		keys      = 40000
		tolerance = 0.08
	)
	cands := clusters(16)
	counts := map[string]int{}
	for i := 0; i < keys; i++ {
		counts[top(cands, fmt.Sprintf("sess-%d", i))]++
	}

	if len(counts) != len(cands) {
		t.Fatalf("only %d/%d candidates ever won", len(counts), len(cands))
	}
	expected := float64(keys) / float64(len(cands))
	for cluster, n := range counts {
		if dev := math.Abs(float64(n)-expected) / expected; dev > tolerance {
			t.Errorf("%s got %d (%.1f%% off an even %.0f)", cluster, n, dev*100, expected)
		}
	}
}

// With a single candidate it must be chosen; the scoring must not return
// empty.
func TestSingleCandidate(t *testing.T) {
	cands := clusters(1)
	if got := top(cands, "whatever"); got != cands[0].Cluster {
		t.Errorf("got %q", got)
	}
}

// There has to be a separator between the session key and the cluster name, or
// ("ab","c") and ("a","bc") feed in the same stream. Session keys are
// user-controlled, and cluster names already contain '|' and '.'.
func TestKeyClusterBoundary(t *testing.T) {
	if rendezvousScore("ab", "c") == rendezvousScore("a", "bc") {
		t.Error("key/cluster boundary is not separated")
	}
}

// The output has to be an **exponential decay by rank**, not the raw HRW hash.
//
// Raw HRW values are roughly uniform over [0,1), so the sticky owner draws
// 0.95 on one session and 0.51 on the next -- under a weighted sum that makes
// "how strong is the stickiness" random per session. Under a first-match chain
// it did not matter (only the argmax was read); now it is fatal.
func TestScoresAreRankBasedNotRawHash(t *testing.T) {
	cands := clusters(4)
	for _, key := range []string{"u-1", "u-2", "u-99", "another-session"} {
		scores := affinityScores(cands, key)
		order := rank(cands, key)
		want := 1.0
		for _, name := range order {
			if math.Abs(scores[name]-want) > 1e-12 {
				t.Fatalf("key %q: %s got %v, want %v (the rank decay must not depend on the session key)",
					key, name, scores[name], want)
			}
			want *= affinityDecay
		}
	}
}

// The owner's advantage over the second choice must be **independent of the
// candidate count** -- otherwise stickiness flattens out as candidates are
// added, and a small load difference overturns it.
func TestHomeAdvantageIsIndependentOfCandidateCount(t *testing.T) {
	for _, n := range []int{2, 3, 8, 32} {
		order := rank(clusters(n), "u-42")
		scores := affinityScores(clusters(n), "u-42")
		if ratio := scores[order[0]] / scores[order[1]]; math.Abs(ratio-1/affinityDecay) > 1e-9 {
			t.Errorf("n=%d: owner/second = %v, want %v", n, ratio, 1/affinityDecay)
		}
	}
}

// With no session key it returns nil -- not a single score may be emitted, or
// every stateless request would be pinned to one place.
func TestNoKeyMeansNoScores(t *testing.T) {
	if got := affinityScores(clusters(3), ""); got != nil {
		t.Errorf("empty key must abstain, got %v", got)
	}
}

// The chance of two HRW scores tying is about 2⁻⁵³, but when it happens there
// has to be a deterministic tiebreak, or the same key would rank differently
// on different workers and stickiness would fail outright.
func TestRankingIsDeterministic(t *testing.T) {
	cands := clusters(6)
	first := rank(cands, "u-42")
	for i := 0; i < 50; i++ {
		got := rank(cands, "u-42")
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d position %d: %s != %s", i, j, got[j], first[j])
			}
		}
	}
}

// The raw hash still has to keep 53 bits of precision -- the ranking depends
// on it, and discarding low bits manufactures false ties.
func TestRawScorePrecision(t *testing.T) {
	seen := map[float64]bool{}
	for i := 0; i < 10000; i++ {
		seen[rendezvousScore(fmt.Sprintf("k%d", i), "outbound|80||model-2-10.static")] = true
	}
	if len(seen) != 10000 {
		t.Errorf("%d distinct scores from 10000 keys; collisions mean lost precision", len(seen))
	}
	for i := 0; i < 1000; i++ {
		s := rendezvousScore(fmt.Sprintf("k%d", i), "c")
		if s < 0 || s >= 1 {
			t.Fatalf("score %v out of [0,1)", s)
		}
	}
}

func TestUnitFloatEdges(t *testing.T) {
	if got := unitFloat(0); got != 0 {
		t.Errorf("unitFloat(0) = %v", got)
	}
	if got := unitFloat(^uint64(0)); got >= 1 {
		t.Errorf("unitFloat(max) = %v, must stay below 1", got)
	}
}

// gpustack serves several model names on one route from the same cluster,
// separated only by targetId, and each candidate carries its own rewrite
// target. Ranking by cluster collapsed them: the later one overwrote the
// earlier in the map, the duplicate still consumed a decay step (pushing every
// candidate below it down a rank), and the finisher then tied and picked a
// model at random for what was supposed to be one sticky session.
func TestSameClusterCandidatesRankSeparately(t *testing.T) {
	cands := []candidate{
		{Cluster: "X", TargetID: "2", ModelName: "qwen3-32b"},
		{Cluster: "X", TargetID: "3", ModelName: "deepseek-v3"},
		{Cluster: "Y", TargetID: "4", ModelName: "llama-70b"},
	}
	scores := affinityScores(cands, "session-1")

	if len(scores) != len(cands) {
		t.Fatalf("got %d scores for %d candidates: %v", len(scores), len(cands), scores)
	}
	// The decay curve must still be 1, 0.5, 0.25 -- no step swallowed by the
	// duplicate, so the last distinct candidate is not pushed below its rank.
	want := map[float64]bool{1: true, 0.5: true, 0.25: true}
	for _, v := range scores {
		if !want[v] {
			t.Errorf("unexpected score %v in %v", v, scores)
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Errorf("missing decay steps %v in %v", want, scores)
	}
}

// Stickiness has to hold *per candidate*, not per cluster: the two candidates
// on cluster X rewrite to different models, so a session that flips between
// them flips the model it is served.
func TestStickinessIsStableAcrossSameClusterCandidates(t *testing.T) {
	cands := []candidate{
		{Cluster: "X", TargetID: "2"},
		{Cluster: "X", TargetID: "3"},
		{Cluster: "Y", TargetID: "4"},
	}
	first := affinityScores(cands, "session-1")
	for i := 0; i < 50; i++ {
		again := affinityScores(cands, "session-1")
		for k, v := range first {
			if again[k] != v {
				t.Fatalf("score for %q moved from %v to %v", k, v, again[k])
			}
		}
	}

	// Different sessions must not all land on the same candidate, or the split
	// between the two same-cluster targets would be no split at all.
	owners := map[string]int{}
	for i := 0; i < 2000; i++ {
		s := affinityScores(cands, fmt.Sprintf("session-%d", i))
		for k, v := range s {
			if v == 1.0 {
				owners[k]++
			}
		}
	}
	if len(owners) != 3 {
		t.Fatalf("all three candidates should own some sessions, got %v", owners)
	}
}
