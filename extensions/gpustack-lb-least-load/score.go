package main

// Load scoring, plus the filter state reads and writes.

import (
	"encoding/json"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// The type aliases point at the **shared contract** rather than a locally
// redefined struct. That way what the finisher publishes is the very type this
// plugin parses, and a field mismatch shows up at compile time instead of
// manifesting in production as "the ranking was written but had no effect".
type (
	candidate    = wire.Candidate
	candidateSet = wire.CandidateSet
	rankEntry    = wire.RankEntry
)

// loadScores converts each candidate's effective load into a raw
// "higher is better" score.
//
//	effective load = Inflight + Penalty
//	raw score      = 1 / (1 + effective load)
//
// **The two terms can simply be added** because the publisher has already
// converted the penalty into the same unit as the in-flight count:
// Penalty = decay fraction × (max in-flight across the set + 1). So this plugin
// does not need to know rampMs, and does not need to read any shared state --
// P₀ adapts to how busy the cluster is.
//
// ⚠️ **This transform carries meaning; it is not decorative** (it was described
// that way back when the design used a fallback chain, which no longer holds
// after the switch to a weighted sum). The finisher L1-normalises every entry,
// so the **shape** of f(load) directly decides how concentrated this entry's
// vote is:
//
//	load   0 → 1     raw 1.0 / 0.5       = 2×     idle to one in flight is a big deal
//	load 100 → 101   raw 0.00990/0.00980 = 1.01×  one more out of a hundred is noise
//
// In other words 1/(1+load) encodes diminishing marginal sensitivity, which
// matches the intuition for LLM serving: when the candidates are close this
// entry votes evenly (near neutral, letting stickiness and the others speak),
// and only when they are far apart does it vote sharply. Swapping in -load or
// (max-load)/max gives completely different behaviour, so **do not** replace it
// on the grounds that "the argmax is the same" -- that reasoning only held
// under the fallback chain.
//
// No L1 normalisation here: the finisher does it uniformly (see the comment on
// wire.RankEntry).
// The map is keyed by Candidate.Key(), not Cluster: several candidates may
// share a cluster and differ only in targetId. They legitimately score the same
// here -- one cluster is one backend with one in-flight count -- but they still
// need **one map entry each**, or the finisher's L1 sum counts the shared entry
// once per duplicate and inflates that cluster's vote.
func loadScores(cands []candidate) map[string]float64 {
	scores := make(map[string]float64, len(cands))
	for i := range cands {
		scores[cands[i].Key()] = loadScore(effectiveLoad(&cands[i]))
	}
	return scores
}

func effectiveLoad(c *candidate) float64 {
	load := float64(c.Inflight) + c.Penalty
	if load < 0 {
		return 0
	}
	return load
}

func loadScore(load float64) float64 { return 1.0 / (1.0 + load) }

// **Ties are not broken here.** On an idle cluster every candidate is at zero
// in flight, which is the normal case rather than an edge case; emitting
// exactly equal scores (after L1 each gets 1/N, the same constant for everyone,
// automatically neutral) leaves the reservoir sampling to the finisher's
// bestTotal -- that is already implemented and tested, and randomising again
// here would be a second implementation of it, which would eventually disagree.
//
// Equal loads must produce **bit-for-bit equal** float64 values, or the
// reservoir sampling never triggers at all. loadScore is pure arithmetic with
// no random term, which TestEqualLoadsTieExactly pins down.

func readCandidates() (candidateSet, bool) {
	var set candidateSet
	data, err := proxywasm.GetProperty([]string{wire.FilterStateCandidates})
	if err != nil || len(data) == 0 {
		return set, false
	}
	if err := json.Unmarshal(data, &set); err != nil {
		proxywasm.LogWarnf("%s: unparseable candidate set: %v", pluginName, err)
		return set, false
	}
	return set, true
}

// isWeighted uses the same criterion as the finisher: if **any** candidate
// carries a weight, the whole set is treated as weighted and this plugin exits
// early.
func isWeighted(cands []candidate) bool {
	for i := range cands {
		if cands[i].Weight != nil {
			return true
		}
	}
	return false
}

// appendRank **appends** this plugin's opinion to the shared ranking array.
//
// Read-modify-write is safe here: the filter chain for a single request runs
// serially on one worker thread, so two capability plugins never touch this key
// concurrently. (Contention only exists in shared data, which is
// cross-request.)
func appendRank(entry rankEntry) {
	var ranks []rankEntry
	if data, err := proxywasm.GetProperty([]string{wire.FilterStateRanks}); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &ranks); err != nil {
			// This plugin has the lowest priority in the capability band, so
			// normally the array may already hold affinity's entry. Failing to
			// parse it means somebody wrote something broken; drop it and start
			// over -- keeping an unreadable array would only make the finisher
			// fail to parse it too, voiding our own entry along with it.
			proxywasm.LogWarnf("%s: discarding unparseable rank list: %v", pluginName, err)
			ranks = nil
		}
	}
	ranks = append(ranks, entry)

	data, err := json.Marshal(ranks)
	if err != nil {
		proxywasm.LogWarnf("%s: marshal ranks failed: %v", pluginName, err)
		return
	}
	if err := proxywasm.SetProperty([]string{wire.FilterStateRanks}, data); err != nil {
		proxywasm.LogWarnf("%s: publish ranks failed: %v", pluginName, err)
	}
}
