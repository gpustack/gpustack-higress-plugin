package main

import (
	"encoding/json"
	"hash/fnv"
	"math"
	"math/rand"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// rrCursor is the round-robin counter used as the last-resort fallback when no
// capability plugin has an opinion.
//
// It deliberately stays in **per-VM memory** rather than shared data: every
// worker thread round-robins evenly over the candidate set, and the union of
// several independent even rotations is still even -- all that is lost is the
// global ordering, which round-robin never promised anyway. Putting it in
// shared data would cost a CAS per request for zero benefit on the hot path.
// (In-flight counting is the opposite case: its reader is another wasm module,
// so it has to go through shared data.)
//
// The start is **random** rather than 0: Envoy runs one VM per worker thread,
// and if they all started at 0 the first few requests on a fresh gateway would
// hit the same candidate on every thread at once. In steady state the phases
// drift apart naturally, but the startup instant is synchronised. A random
// start removes that transient clustering at no cost. One OS thread owns one VM
// and wasm callbacks run serially, so no synchronisation is needed here.
var rrCursor = rand.Uint64()

// selectCandidate returns the chosen candidate, or nil when the candidate set
// is empty (the caller then rejects).
//
// **The criterion is decided by whether the candidates carry a weight**; there
// is no separate selection switch:
//
//	weight present -> this route has a business split (canary / provider cost),
//	                  so roll the weighted dice and ignore every scoring
//	                  opinion -- scoring across different models silently
//	                  breaks the split
//	weight absent  -> the candidates are interchangeable, so let the capability
//	                  plugins score; with no opinions at all, round-robin
//
// "Does this route have a split" is already encoded in the presence of the
// weight field. Adding a selection enum would be a second representation of
// the same fact, and the two would eventually disagree.
// The request id is read here rather than by the caller because only the
// weighted branch uses it: reading it costs a host call, and on the scored path
// a miss also burns a random draw, both for a value that is then discarded.
func selectCandidate(set CandidateSet) *Candidate {
	if len(set.Candidates) == 0 {
		return nil
	}
	if isWeighted(set.Candidates) {
		return selectWeighted(set, requestID())
	}
	return selectScored(set)
}

// isWeighted: if **any** candidate carries a weight, the whole set is treated
// as weighted. reconcile either writes a weight on every candidate or on none;
// a mixture means the config is broken, and in that case treating the missing
// ones as 0 (never selected) is safer than ignoring the split.
func isWeighted(cands []Candidate) bool {
	for i := range cands {
		if cands[i].Weight != nil {
			return true
		}
	}
	return false
}

func weightOf(c *Candidate) int64 {
	if c.Weight == nil {
		return 0
	}
	return *c.Weight
}

// selectWeighted reproduces Envoy's weighted-interval algorithm.
//
// The candidate order is fixed by the publisher, sorted by full cluster name,
// and is not re-sorted here -- as long as the sort key is stable, the same
// candidates plus the same weights always produce the same interval split.
// Otherwise removing an instance and adding it back would redistribute all the
// traffic.
//
// When every weight is 0 (the field is absent, or they really are all zero)
// the candidates are treated as equal; never divide by zero.
// uniformPick treats the candidates as equally weighted, still keyed by the
// request id so the choice stays deterministic for a retry of the same request.
func uniformPick(set CandidateSet, reqID string) *Candidate {
	idx := int(hash64(reqID) % uint64(len(set.Candidates)))
	return &set.Candidates[idx]
}

func selectWeighted(set CandidateSet, reqID string) *Candidate {
	if len(set.Candidates) == 0 {
		return nil
	}
	var total int64
	for i := range set.Candidates {
		w := weightOf(&set.Candidates[i])
		if w <= 0 {
			continue
		}
		// Overflowing int64 would wrap the total negative, and a negative total
		// falls into the uniform-hash branch below by accident rather than by
		// decision. Take that branch deliberately instead: with a total this
		// meaningless, treating the candidates as equal is the only defensible
		// reading. parseLB rejects negative weights, so w is always positive.
		//
		// **No logging here** -- this function is pure so that it can be unit
		// tested, and a host ABI call panics outside a wasm host. parseLB
		// rejects an overflowing set at config load, which is both the louder
		// and the earlier place to say so; this is the backstop.
		if total > math.MaxInt64-w {
			return uniformPick(set, reqID)
		}
		total += w
	}
	if total <= 0 {
		return uniformPick(set, reqID)
	}

	point := int64(hash64(reqID) % uint64(total))
	var acc int64
	for i := range set.Candidates {
		w := weightOf(&set.Candidates[i])
		if w <= 0 {
			continue
		}
		acc += w
		if point < acc {
			return &set.Candidates[i]
		}
	}
	// Unreachable short of a float or overflow anomaly; fall back to the
	// last candidate rather than panicking.
	return &set.Candidates[len(set.Candidates)-1]
}

// selectScored performs a **weighted sum** (the llm-d model), not a
// first-match fallback chain.
//
//	total(c) = Σ_entry ( Weight_entry × Scores_entry[c] / Σ_c' Scores_entry[c'] )
//
// Three things happen here and are deliberately not delegated to the
// capability plugins:
//
//  1. **L1 normalisation** (each entry divided by its own sum, so each casts
//     one vote). Doing it here means a plugin that normalises badly cannot
//     dominate the other entries just by writing large numbers -- and for a
//     separately shipped enterprise plugin, this is the only place that can be
//     enforced.
//  2. **The weight default** (<= 0 becomes 1).
//  3. **Negative scores treated as 0.** L1 is meaningless over negatives.
//
// Why L1 rather than min-max: min-max unconditionally maps the best candidate
// to 1.0 and the worst to 0.0, so **a one-request difference votes exactly as
// hard as a thousand-request difference**. Measured, for two candidates at
// load 100 vs 101: min-max gives 1.0/0.0 (a full-strength preference that
// flips stickiness outright), L1 gives 0.5025/0.4975 (near neutral, so
// stickiness holds). L1 preserves the information about *how far apart* the
// candidates actually are, and that is the premise on which the weights can be
// interpreted at all.
func selectScored(set CandidateSet) *Candidate {
	ranks, ok := readRanks()
	if !ok || len(ranks) == 0 {
		return roundRobin(set.Candidates)
	}
	totals, contributed := combineRanks(set.Candidates, ranks)
	if !contributed {
		return roundRobin(set.Candidates)
	}
	pick := bestTotal(set.Candidates, totals)
	if pick != nil {
		proxywasm.LogDebugf("%s: picked %s by weighted sum over %d ranks",
			pluginName, pick.Cluster, len(ranks))
	}
	return pick
}

// combineRanks is a pure function: L1 normalisation plus weighted
// accumulation.
//
// It is split out so it can be unit-tested -- selectScored itself has to read
// filter state, and host ABI calls panic outside a wasm host. The combination
// logic is the one place in this mechanism that is easy to get numerically
// wrong, and it cannot be covered by cluster testing alone.
//
// contributed is false when no entry cast a valid vote, in which case the
// caller should fall back to round-robin.
func combineRanks(cands []Candidate, ranks []RankEntry) (totals []float64, contributed bool) {
	totals = make([]float64, len(cands))

	for _, r := range ranks {
		// Normalise only over the **live candidates**: a capability plugin's
		// map may name a candidate that is no longer in the set (the whole
		// capability band sits between its read and the finisher's).
		//
		// The lookup key is Candidate.Key(), not Cluster. Several candidates
		// may share a cluster and differ only in targetId, and this sum
		// iterates over candidates -- so with a cluster key their shared score
		// would be added once per duplicate, handing that cluster a multiple of
		// the vote it actually cast.
		sum := 0.0
		for i := range cands {
			if v, ok := r.Scores[cands[i].Key()]; ok && v > 0 {
				sum += v
			}
		}
		if sum <= 0 {
			// This entry has no valid score (it abstained, everything was
			// zero, or the candidates it scored are all gone). Skip it rather
			// than spreading the vote evenly: spreading would hand a free
			// bonus to the subset in its map, when it actually said nothing.
			continue
		}

		w := r.Weight
		if !(w > 0) { // also rejects 0, negatives and NaN
			w = defaultRankWeight
		}
		for i := range cands {
			if v, ok := r.Scores[cands[i].Key()]; ok && v > 0 {
				totals[i] += w * v / sum
			}
		}
		contributed = true
	}
	return totals, contributed
}

func readRanks() ([]RankEntry, bool) {
	data, err := proxywasm.GetProperty([]string{FilterStateRanks})
	if err != nil || len(data) == 0 {
		return nil, false
	}
	var ranks []RankEntry
	if err := json.Unmarshal(data, &ranks); err != nil {
		proxywasm.LogWarnf("%s: unparseable rank list: %v", pluginName, err)
		return nil, false
	}
	return ranks, true
}

// bestTotal picks the candidate with the highest total. Ties are broken by
// reservoir sampling -- on an idle cluster every candidate scoring the same is
// the normal case, not an edge case, and without randomisation it would cause
// a thundering herd.
func bestTotal(cands []Candidate, totals []float64) *Candidate {
	best := -1
	bestScore := 0.0
	ties := 0
	for i := range cands {
		switch {
		case best < 0 || totals[i] > bestScore:
			best, bestScore, ties = i, totals[i], 1
		case totals[i] == bestScore:
			ties++
			if rand.Intn(ties) == 0 {
				best = i
			}
		}
	}
	if best < 0 {
		return nil
	}
	// Deliberately **no logging here**: bestTotal is a pure function meant to
	// be unit-tested, and host ABI calls panic outside a wasm host.
	// selectScored does the logging.
	return &cands[best]
}

func roundRobin(cands []Candidate) *Candidate {
	// The caller (selectCandidate) already rejects an empty candidate set, but
	// a modulo by zero panics, and a panic inside wasm means this request
	// simply fails -- so guard again here.
	if len(cands) == 0 {
		return nil
	}
	idx := int(rrCursor % uint64(len(cands)))
	rrCursor++
	return &cands[idx]
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
