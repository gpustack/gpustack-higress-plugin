package main

// Rendezvous hashing (HRW), plus the filter-state reads and writes.

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/tidwall/gjson"
)

// The aliases point at the **shared contract** rather than a locally
// redefined struct. That way what the finisher publishes is exactly the type
// this plugin parses, and a field mismatch is a compile error rather than a
// production symptom of "the opinion was written but had no effect".
// affinityDecay halves the score for each step down the ranking.
//
// 0.5 balances two requirements: too steep (say 0.1) and once the owner is
// ejected the second and third choices are nearly indistinguishable, losing
// stability after the transfer; too shallow (say 0.9) and the owner has too
// little advantage over the second choice, so a small load difference flips
// the session.
const affinityDecay = 0.5

type (
	candidate    = wire.Candidate
	candidateSet = wire.CandidateSet
	rankEntry    = wire.RankEntry
)

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
// carries a weight the whole set is weighted, and this plugin exits early.
func isWeighted(cands []candidate) bool {
	for i := range cands {
		if cands[i].Weight != nil {
			return true
		}
	}
	return false
}

// appendRank **appends** this plugin's opinion to the shared array.
//
// Read-modify-write is safe here: the filter chain for one request runs
// serially on a single worker thread, so two capability plugins never touch
// this key concurrently. (Contention only exists in shared data, which is
// cross-request.)
func appendRank(entry rankEntry) {
	var ranks []rankEntry
	if data, err := proxywasm.GetProperty([]string{wire.FilterStateRanks}); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &ranks); err != nil {
			// This plugin has the highest priority in the capability band, so
			// normally the array is still empty. Failing to parse it means
			// someone wrote garbage; discard and start over -- keeping an
			// unreadable array would only make the finisher fail to parse too,
			// taking our entry down with it.
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

// affinityScores emits an **exponential decay by HRW rank**, not the raw HRW
// hash value.
//
// ⚠️ This step is required once opinions are combined by weighted sum. An HRW
// hash is a **purely ordinal** quantity: it is roughly uniform over [0,1), so
// the sticky owner might draw 0.95 on one session and 0.51 on the next, which
// makes "how strong is the stickiness" **random per session** and entirely
// uncontrollable. Under a first-match chain that did not matter (only the
// argmax was read); under a weighted sum it is fatal.
//
// With 1, 0.5, 0.25, ... instead:
//
//   - The owner's advantage is **independent of the candidate count** (always
//     twice the second choice), unlike a linear rank which flattens out as
//     candidates are added.
//   - The ranking information is **still preserved**, so once the owner is
//     ejected the second choice remains clearly better than the third -- the
//     session stays stable after the transfer instead of bouncing per request.
//     A binary {1,0} would lose that.
//
// The finisher L1-normalises afterwards, so there is no need to divide by the
// sum here; with three candidates the votes actually cast are
// 0.571 / 0.286 / 0.143.
//
// The ranking is over **candidates**, keyed by wire.Candidate.Key(), not over
// clusters. Several candidates may share a cluster and differ only in targetId,
// and they must rank separately: they carry different rewrite targets, so
// collapsing them lets the finisher's tie-break choose the model at random --
// per request, for one session, which inverts the whole point of this plugin.
// Ranking by candidate also keeps each one on its own step of the decay curve,
// instead of a duplicate silently consuming a step and pushing everything below
// it down.
func affinityScores(cands []candidate, key string) map[string]float64 {
	if key == "" {
		return nil // abstain
	}
	type scored struct {
		candKey string
		hrw     float64
	}
	list := make([]scored, len(cands))
	for i := range cands {
		ck := cands[i].Key()
		list[i] = scored{ck, rendezvousScore(key, ck)}
	}
	// The chance of two HRW scores tying is about 2⁻⁵³, but when it does happen
	// there has to be a deterministic tiebreak, or the same key would rank
	// differently on different workers and stickiness would fail outright.
	// Fall back to the candidate key.
	sort.Slice(list, func(i, j int) bool {
		if list[i].hrw != list[j].hrw {
			return list[i].hrw > list[j].hrw
		}
		return list[i].candKey < list[j].candKey
	})

	out := make(map[string]float64, len(list))
	v := 1.0
	for _, e := range list {
		out[e.candKey] = v
		v *= affinityDecay
	}
	return out
}

// rendezvousScore is HRW's raw hash, used **only for ranking** and never
// published directly.
//
// **It has to be rendezvous (HRW), not hash % N.** The latter reshuffles every
// session whenever the candidate count changes -- and the candidate set changes
// at precisely the moments stickiness matters most: scaling, and instances
// being ejected by passive health. HRW's property is that removing one
// candidate only reassigns the sessions that lived on it, and leaves the rest
// untouched.
//
// candKey is wire.Candidate.Key() -- the candidate's identity, which for a
// candidate carrying a targetId is itself two fields joined by a NUL.
func rendezvousScore(key, candKey string) float64 {
	h := fnv.New64a()
	// **Length-prefix the session key** rather than writing a delimiter after
	// it. A single delimiter used to be enough, when the second half was a bare
	// cluster name containing no NUL. It no longer is: candKey may itself
	// contain a NUL, so ("a", "b\0c") and ("a\0b", "c") would feed in the same
	// byte stream -- and session keys are user-controlled (a JSON body field
	// can carry a \u0000 escape), so that is reachable rather than
	// theoretical. A length prefix is unambiguous whatever either half
	// contains.
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(key)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte(candKey))

	// FNV-1a avalanches poorly: inputs differing only in the last character or
	// two (model-2-10 / model-2-11 -- exactly our naming) produce highly
	// correlated high bits, and HRW compares nothing but which score is
	// highest, so some instances get systematically more traffic. Six
	// instructions of MurmurHash3's finalizer scatter the bits.
	//
	// This is not precautionary fastidiousness; the measured skew grows with
	// the candidate count: 14.1% -> 6.6% at 16 candidates, 23.2% -> 10.1% at
	// 32 (the numbers after mixing are sampling noise itself).
	// TestDistributionIsEven pins this down with 16 candidates.
	return unitFloat(mix64(h.Sum64()))
}

func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// unitFloat squeezes a 64-bit hash into [0,1).
//
// It takes the top 53 bits rather than float64(x)/2^64: a float64 mantissa is
// only 53 bits wide, so a direct conversion discards the low 11 and manufactures
// ties -- and the finisher breaks ties at random, which would destroy
// stickiness exactly. The top 53 bits are represented exactly, leaving a tie
// probability of about 2⁻⁵³.
func unitFloat(x uint64) float64 {
	return float64(x>>11) / float64(uint64(1)<<53)
}

// sessionKeyFromBody reads the session key out of a JSON body. It returns an
// empty string when there is none, which means abstain.
func sessionKeyFromBody(body []byte, key string) string {
	v := gjson.GetBytes(body, key)
	if !v.Exists() {
		return ""
	}
	// Strings only: gjson's String() renders numbers and objects into
	// plausible-looking strings too, which would let a field that merely
	// happens to share the name be taken as a session key.
	if v.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(v.String())
}
