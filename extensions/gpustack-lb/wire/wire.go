// Package wire is the **cross-binary contract** of the LB plugin framework.
//
// It exists for one reason: the distribution boundary. Capability plugins
// (least-load, session-affinity, and the enterprise prefix plugin) are
// separately compiled, separately shipped wasm binaries, so they cannot share
// a `package main` with the finisher. Hand-mirroring the contract into each
// plugin eventually produces a **silent mismatch** -- the community edition
// adds a field, the enterprise binary does not follow, both keep running, and
// the only symptom is that the wrong candidate gets picked.
//
// So this package holds only what genuinely crosses a binary boundary. The
// shapes of the shared state (health, in-flight) are deliberately left out:
// only gpustack-lb reads and writes those, and publishing them here would just
// invite capability plugins to read shared data directly -- which is what §7.1
// of the design is trying to avoid. Filtering is applied by the publisher, and
// capability plugins should only ever see the already-filtered candidate set.
//
// Design reference: the LB framework design doc §3.1, §4.2, §7.1.
package wire

const (
	// FilterStateCandidates: the publisher writes the filtered candidate set
	// here; capability plugins and the finisher read it.
	FilterStateCandidates = "gpustack_lb_candidates"

	// FilterStateRanks is the opinion channel shared by **all** capability
	// plugins -- each one appends its own entry to this array.
	//
	// Sharing one key is safe: the filter chain for a single request runs
	// **serially** on one worker thread, so two capability plugins never
	// read-modify-write concurrently. (Contention only exists in shared data,
	// which is cross-request.)
	//
	// Array order does not matter -- the finisher combines the entries by
	// weighted sum, which is order-insensitive.
	FilterStateRanks = "gpustack_lb_ranks"

	KindInstance = "instance"
	KindProvider = "provider"
)

// Candidate is one candidate instance published to downstream plugins.
type Candidate struct {
	Cluster string `json:"cluster"`

	// Weight is a pointer because **the presence of the field is itself the
	// mode switch**:
	//
	//	weight present -> weighted dice roll, all scoring opinions ignored
	//	                  (this route carries a business traffic split)
	//	weight absent  -> capability plugins score, weighted sum decides
	//
	// A pointer rather than "treat 0 as absent", because weight: 0 is
	// meaningful -- when a canary is dialled down to 0% the candidate is still
	// in the set (healthy, usable), its share is just zero.
	//
	// A capability plugin that sees any non-nil Weight should **exit early**:
	// scoring across different models silently breaks the business split
	// (§4.2).
	Weight *int64 `json:"weight,omitempty"`

	// ModelName is the **already-resolved** rewrite target: the publisher
	// looks up the modelMappers group for this candidate's TargetID, resolves
	// it against the model name the client sent (the x-higress-llm-model
	// header), and fills it in here. Empty means no rewrite (by-pass).
	ModelName string `json:"modelName,omitempty"`

	// TargetID is the ModelRouteTarget this candidate belongs to. It is **part
	// of the candidate's identity**, not just a lookup for the publisher's
	// mapping table -- see Key.
	TargetID string `json:"targetId,omitempty"`

	Kind string `json:"kind"`

	// Inflight and Penalty are published separately rather than pre-combined
	// into one load scalar: there is more than one consumer and they each want
	// something different. Capability plugins read load **from here** -- do not
	// go read shared data directly, the publisher has already applied the
	// filters and computed the penalty.
	Inflight int64   `json:"inflight"`
	Penalty  float64 `json:"penalty"`

	// Probation: this candidate was just released from a cooldown and its
	// penalty has not fully decayed yet.
	Probation bool `json:"probation,omitempty"`
}

// candidateKeySep separates the two halves of Key.
//
// NUL is chosen because it **cannot occur in either half**: Envoy cluster names
// are built from a port, a subset and a DNS host, and a targetId comes from a
// JSON config field that gpustack populates with a ModelRouteTarget id. That is
// what makes Key injective -- with a printable separator, a cluster whose name
// happened to contain it would collide with a different (cluster, targetId)
// pair, and the symptom would be two candidates silently sharing one score.
const candidateKeySep = "\x00"

// Key is the candidate's identity, and **the key used in RankEntry.Scores**.
//
// It is not the cluster name. gpustack supports several model names on one
// route backed by the **same cluster**, distinguished only by targetId -- and
// since the rewrite target is resolved per targetId, those candidates have
// different ModelName values while sharing a cluster. Keying scores by cluster
// alone made them indistinguishable to every capability plugin, with three
// compounding effects:
//
//   - the later candidate **overwrote** the earlier one in the scores map, so
//     the map had fewer entries than the candidate set;
//   - it still **consumed a rank slot**, pushing every subsequent candidate one
//     step down the decay curve, which distorted the whole set rather than just
//     the pair;
//   - the finisher's L1 sum iterates over *candidates*, so the shared score was
//     **counted twice**, giving the duplicated cluster double the aggregate
//     vote.
//
// And because the two then tied exactly, the finisher's tie-break picked
// between them at random -- so a sticky session flipped its model rewrite from
// request to request, which is precisely the opposite of what the affinity
// plugin exists to do.
//
// The cluster alone remains the right key for **shared state** (in-flight
// counts, passive health): those describe the backend, and candidates sharing a
// cluster genuinely share one backend. Only *scoring* identity needed
// splitting.
//
// ⚠️ Every capability plugin must key its Scores map with this function,
// including separately shipped ones. A plugin still keying by cluster does not
// fail loudly -- it just silently reverts to the behaviour above.
func (c Candidate) Key() string {
	if c.TargetID == "" {
		return c.Cluster
	}
	return c.Cluster + candidateKeySep + c.TargetID
}

// CandidateSet is the value stored under FilterStateCandidates.
//
// There is no selection field: which criterion applies is decided by whether
// the candidates themselves carry a weight. One fact should not have two
// representations, or the two will disagree.
type CandidateSet struct {
	Candidates []Candidate `json:"candidates"`
}

// RankEntry is one capability plugin's opinion, appended to the
// FilterStateRanks array.
//
// Opinions combine by **weighted sum** (the llm-d scorer model), not by a
// first-match fallback chain: the finisher L1-normalises each entry and
// accumulates the weighted contributions, then takes the highest total.
//
//	total(c) = Σ_entry ( Weight_entry × Scores_entry[c] / Σ_c' Scores_entry[c'] )
//
// Three hard requirements on capability plugins follow from that:
//
//  1. **Scores must be non-negative.** L1 normalisation is meaningless over
//     negative values. The finisher treats negatives as zero, but that is a
//     guard rail, not the contract.
//  2. **The "shape" of the scores carries meaning, not just their ordering.**
//     Under a first-match chain only the argmax mattered, so any strictly
//     decreasing transform was equivalent. Under a weighted sum, how
//     **spread out** the scores are decides how concentrated this entry's
//     vote is. Candidates that are close together should vote evenly (near
//     neutral); only genuinely far-apart candidates should vote sharply.
//  3. **Do not L1-normalise yourself.** The finisher does it uniformly -- that
//     way a plugin that normalises badly (especially a separately shipped
//     enterprise one) cannot dominate the other entries just by writing large
//     numbers.
//
// Scoring only a **subset** of the candidate set is legal: candidates absent
// from the map score 0 in this entry. Prefix affinity is exactly that shape --
// only instances that already have the prefix cached should score.
//
// **Scores is keyed by Candidate.Key(), not by Cluster.** Two candidates can
// share a cluster and differ only in targetId; see the Key doc for what keying
// by cluster silently did to the arithmetic.
//
// Name is only used for troubleshooting logs; it takes no part in the maths.
type RankEntry struct {
	Name string `json:"name"`

	// Weight is this entry's vote multiplier, **published by the plugin
	// itself** rather than configured on the finisher.
	//
	// This is one deliberate divergence from llm-d, motivated by the
	// distribution boundary: the enterprise prefix plugin is compiled and
	// shipped separately, so the finisher cannot possibly know in advance what
	// weight to give a plugin it has never seen. Letting the plugin carry its
	// own weight keeps "install a new capability plugin" equal to "install one
	// CR", with no change on the finisher side.
	//
	// Absent or <= 0 is treated as 1. Zero is not a meaningful value -- "do
	// not participate" belongs in the plugin's own enabled switch, or in
	// simply not publishing an entry -- so this is a bare float64 rather than
	// a pointer (§9 rule 5: no pointer needed when the zero value and "unset"
	// mean the same thing).
	Weight float64 `json:"weight,omitempty"`

	Scores map[string]float64 `json:"scores"`
}
