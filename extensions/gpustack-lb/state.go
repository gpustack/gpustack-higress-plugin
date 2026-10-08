package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
)

// sharedDataStore is the default stateStore: proxy-wasm shared data, no
// external dependency.
//
// The finisher role is the **only writer** of these two pieces of shared
// state; the context role only reads them.
//
// They have to live in shared data rather than per-VM memory: the two modes
// are **two filter instances** of the same binary, each with its own linear
// memory, and merging the binaries does not let them share variables. The cost
// is two CAS operations per request (+1 on selection, -1 on onStreamDone).
//
// ⚠️ Its scope is **one Envoy process**. Several gateway replicas each keep
// their own in-flight count and their own health verdict, so a fleet-wide view
// needs the redis backend (see redis_store.go).
type sharedDataStore struct{}

// LoadStates reads every candidate target's state synchronously -- shared data
// is a host-local lookup, so there is nothing to wait for and the request is
// never paused.
func (sharedDataStore) LoadStates(refs []targetRef, nowMs, maxAgeMs int64, done func(map[string]clusterState)) types.Action {
	states := make(map[string]clusterState, len(refs))
	for _, ref := range refs {
		states[ref.Key] = clusterState{
			// The budget is per target (Candidate.Key()), the health verdict
			// is per cluster: candidates sharing a backend share one ejection.
			Inflight: readInflight(ref.Key, nowMs, maxAgeMs),
			Health:   readHealth(ref.Cluster),
		}
	}
	done(states)
	return types.ActionContinue
}

// Reserve walks the targets in order and check-and-reserves the first one
// under its cap, entirely in-process (shared data is host-local). The CAS
// loop makes the check and the +1 one operation per key, so this backend
// enforces a hard cap too -- scoped to one Envoy process, like all its state.
func (sharedDataStore) Reserve(targets []reserveTarget, nowMs, maxAgeMs int64, done func(int, string) types.Action) types.Action {
	for i, t := range targets {
		reserved, ok := reserveInflight(t.Key, t.MaxRun, nowMs, maxAgeMs)
		if !ok {
			return done(reserveFailOpen, "")
		}
		if reserved {
			return done(i+1, tokenForStart(nowMs))
		}
	}
	return done(reserveExhausted, "")
}

func (sharedDataStore) Done(rec doneRecord) {
	if startMs, err := strconv.ParseInt(rec.Token, 10, 64); err == nil {
		removeInflight(rec.InflightKey, startMs, rec.NowMs, rec.MaxAgeMs)
	}
	switch rec.Outcome {
	case outcomeSuccess:
		recordSuccess(rec.Cluster)
	case outcomeFailure:
		recordFailure(rec.Cluster, rec.Threshold, rec.CooldownMs, rec.RampMs, rec.NowMs)
	}
}

const casMaxRetries = 10

// emptyInflightJSON is the wire form of a zero-valued inflightState: Starts is
// omitempty, so the empty state marshals to `{}`. reserveInflight writes it --
// and only it -- through the unconditional create path; see its doc.
const emptyInflightJSON = `{}`

// ⚠️ A config reload **must not reset** either of these; it may only
// reconcile the key set.
//
// Measured fact: any config change in the cluster causes every wasm plugin's
// extension config to be re-applied, not just the one that changed. Copying
// ai-proxy's resetSharedData() pattern would mean a single scale-up zeroes
// every instance's health state (a dead instance that was just ejected returns
// to the candidate set immediately) and every in-flight count (thundering
// herd) -- and scaling is precisely the moment load needs to be perceived
// correctly. Hence there is deliberately no reset function here.

func nowMillis() int64 { return time.Now().UnixMilli() }

// readInflight counts the in-flight entries that have not aged out.
//
// **The age filter has to be applied on the read side too**, even though the
// writer already prunes on every reserve and release. Those prunes only run
// when this key is *touched*, and a key whose target stops being selected can
// go a long time untouched:
//
//	maxRunningRequests entries leak (hung stream, client disconnect)
//	  -> the leaked members sit in the entry list past maxInflightAgeMs
//	  -> nothing touches the key, so nothing prunes them
//
// failOpen does not rescue this either: the re-admission there deliberately
// excludes overCap. So a purely read-side condition would hold the candidate
// out permanently -- the exact starvation that storing timestamps instead of a
// counter was meant to make impossible. (The redis backend has the same rule
// for the same reason: its reader counts with a ZCOUNT lower bound rather than
// relying on the writer's pruning.)
//
// This does not duplicate the knob. maxAgeMs is the same config.maxInflightAgeMs
// the finisher prunes with, inherited through the same deployment-level path,
// so the two cannot disagree. The read stays non-destructive: counting here is
// free, whereas writing the pruned array back would mean a CAS on every
// candidate of every request.
func readInflight(key string, nowMs, maxAgeMs int64) int64 {
	data, _, err := proxywasm.GetSharedData(SharedInflightPrefix + key)
	if err != nil || len(data) == 0 {
		return 0
	}
	var st inflightState
	if err := json.Unmarshal(data, &st); err != nil {
		return 0
	}
	var n int64
	for _, ts := range st.Starts {
		if nowMs-ts < maxAgeMs {
			n++
		}
	}
	return n
}

func readHealth(cluster string) healthState {
	var st healthState
	data, _, err := proxywasm.GetSharedData(SharedHealthPrefix + cluster)
	if err != nil || len(data) == 0 {
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return healthState{}
	}
	return st
}

// reserveInflight checks one key's live in-flight count against its cap and,
// when there is room, records one entry -- as a single CAS read-modify-write,
// so the check and the +1 cannot interleave with another worker's.
//
// reserved reports whether an entry was written; ok reports whether the
// backend is healthy. ok == false (the read failed, or the CAS budget ran
// out) means the caller should **fail open** rather than read the target as
// full -- the same posture every other shared-data error takes here.
//
// It stores each request's start time rather than a counter: a counter cannot
// be lazily pruned, and the decrement path is where this kind of feature fails
// (early-terminated streams, client disconnects, upstream timeouts). Miss one
// decrement and the counter only ever grows, eventually starving that target
// for good.
//
// ⚠️ **A reservation never travels in an unconditional write.** loadInflight
// returns cas = 0 when the key is absent, and SetSharedData with cas = 0 is
// unconditional (the SDK documents that it never returns CasMismatch and
// always succeeds -- there is no create-only-if-absent primitive in the ABI).
// Appending through that path is the race where two first reservations for a
// maxRun:1 target both read empty, both write their own entry, and both are
// admitted with one overwrite silently losing the other's count.
//
// The missing key is therefore initialized **in two phases**: the unconditional
// write only ever carries the empty state -- an init that loses to another
// init loses nothing, because empty == empty -- and the loop then re-loads the
// key (now with a real, non-zero cas) before appending. Every reservation goes
// through a compare-and-swap against a cas from a successful load, so once the
// key exists the check-and-reserve is atomic.
//
// The residual window, stated honestly: between one worker's not-found read
// and its empty init write, another worker may have initialized *and*
// appended; the init write then resets the list to empty and that entry is
// lost. It can only strike in the first moments of a brand-new key (gateway
// start, or a target appearing), can only *under*-count (never double-admit
// on its own), and self-heals as soon as the key is established. Fully
// closing it would need a create-if-absent primitive the ABI does not offer;
// the redis backend has no such window at all (one script does the check and
// the add), which is the backend to prefer wherever strict caps matter.
func reserveInflight(key string, maxRun, nowMs, maxAgeMs int64) (reserved, ok bool) {
	k := SharedInflightPrefix + key
	for attempt := 0; attempt < casMaxRetries; attempt++ {
		st, cas, loaded := loadInflight(k)
		if !loaded {
			return false, false
		}
		if cas == 0 {
			// Key absent: initialize with the empty state only, then loop to
			// re-load with a real cas. Never append through this write --
			// see the note above.
			if err := proxywasm.SetSharedData(k, []byte(emptyInflightJSON), 0); err != nil {
				proxywasm.LogWarnf("%s: init inflight key %s failed: %v", pluginName, key, err)
				return false, false
			}
			continue
		}
		st.Starts = pruneStarts(st.Starts, nowMs, maxAgeMs)
		if maxRun > 0 && int64(len(st.Starts)) >= maxRun {
			// At or over cap -- a verdict, not an error: the caller moves on
			// to the next target in its order.
			return false, true
		}
		st.Starts = append(st.Starts, nowMs)
		if storeJSON(k, st, cas) {
			return true, true
		}
	}
	proxywasm.LogWarnf("%s: gave up reserving inflight for %s after %d CAS retries",
		pluginName, key, casMaxRetries)
	return false, false
}

// tokenForStart renders the shared-data token: the start timestamp is what
// this backend stores as the entry's identity. Two requests reserving the
// same key within the same millisecond share a token, and the removal then
// takes one of the two entries rather than "the right one" -- harmless, the
// entries are indistinguishable (same key, same start instant).
func tokenForStart(startMs int64) string { return strconv.FormatInt(startMs, 10) }

// removeInflight removes the entry whose start timestamp matches exactly.
// If there is no match (it was already lazily pruned) it just prunes, without
// raising an error.
//
// The decrement gets **twice** the retry budget of the increment: losing an
// increment merely undercounts one in-flight request, whereas losing a
// decrement leaves the entry in place until maxInflightAgeMs (10 minutes by
// default) prunes it, and for that whole window the cluster publishes an
// inflated Inflight and least-load keeps steering traffic away from a healthy
// instance. The costs are asymmetric, so the budgets should not be equal.
const removeCasRetries = casMaxRetries * 2

func removeInflight(key string, startMs, nowMs, maxAgeMs int64) {
	if startMs == 0 {
		return // nothing was reserved, so there is nothing to remove
	}
	k := SharedInflightPrefix + key
	for attempt := 0; attempt < removeCasRetries; attempt++ {
		st, cas, ok := loadInflight(k)
		if !ok {
			return
		}
		before := len(st.Starts)
		st.Starts = pruneStarts(st.Starts, nowMs, maxAgeMs)
		removed := false
		for i, ts := range st.Starts {
			if ts == startMs {
				st.Starts = append(st.Starts[:i], st.Starts[i+1:]...)
				removed = true
				break
			}
		}
		// Nothing expired and the timestamp was not found: there is no change
		// to write, and writing anyway would only add contention on this
		// cluster's hottest key.
		if !removed && len(st.Starts) == before {
			return
		}
		if storeJSON(k, st, cas) {
			return
		}
	}
	proxywasm.LogWarnf("%s: gave up removing inflight for %s after %d CAS retries; "+
		"the entry will linger until it ages out", pluginName, key, removeCasRetries)
}

func pruneStarts(starts []int64, nowMs, maxAgeMs int64) []int64 {
	out := starts[:0]
	for _, ts := range starts {
		if nowMs-ts < maxAgeMs {
			out = append(out, ts)
		}
	}
	return out
}

// recordSuccess resets the consecutive failure count.
//
// It only read-modify-writes when the count is non-zero, so the overwhelming
// majority of normal requests do not pay for a CAS.
//
// **It must retry.** Dropping one reset lets Fails accumulate across
// *non-consecutive* failures, while its definition is the count of
// **consecutive** ones. The consequence is that a few scattered blips can
// eject a healthy instance, and recovery is not automatic -- it takes a later
// success that happens to win its CAS. And "one success and one failure
// landing on the same cluster together" is exactly the contended case here.
func recordSuccess(cluster string) {
	key := SharedHealthPrefix + cluster
	for attempt := 0; attempt < casMaxRetries; attempt++ {
		st, cas, ok := loadHealth(key)
		if !ok || st.Fails == 0 {
			return
		}
		st.Fails = 0
		if storeJSON(key, st, cas) {
			return
		}
	}
	proxywasm.LogWarnf("%s: gave up resetting fail count for %s after %d CAS retries",
		pluginName, cluster, casMaxRetries)
}

// recordFailure records one "never reached the application" failure, ejecting
// the cluster into a cooldown once the threshold is hit.
//
// The threshold arrives **already resolved** by the caller: normally
// unhealthyThreshold (3 by default, so transient blips do not cause collateral
// damage), but 1 during recovery -- a just-released instance has zero in-flight
// requests and therefore looks optimal, so another failure should send it
// straight back rather than letting it repeatedly soak up traffic. Resolving it
// in the finisher rather than here keeps both backends from deciding the
// question separately, and differently.
//
// Probation itself is published by the context role along with the candidate
// set rather than recomputed from rampMs: that window is configured in one
// place only, which removes a knob that would otherwise have to agree on both
// sides. It is also the more accurate semantic -- it means "this request was
// admitted during the recovery window", not "we are still in it right now".
func recordFailure(cluster string, threshold, cooldownMs, rampMs, nowMs int64) {
	key := SharedHealthPrefix + cluster
	for attempt := 0; attempt < casMaxRetries; attempt++ {
		st, cas, ok := loadHealth(key)
		if !ok {
			return
		}
		st.Fails++
		if st.Fails >= threshold {
			st.Fails = 0
			// Stamp both instants together: the reader (context) derives the
			// penalty and probation from them and never needs to know
			// cooldownMs / rampMs. That fixes the window per occurrence, so
			// editing the config later cannot reinterpret an in-progress
			// cooldown as a different length.
			st.EjectedUntil = nowMs + cooldownMs
			st.RampUntil = st.EjectedUntil + rampMs
			proxywasm.LogWarnf("%s: ejecting %s until %d (ramp until %d)",
				pluginName, cluster, st.EjectedUntil, st.RampUntil)
		}
		if storeJSON(key, st, cas) {
			return
		}
	}
}

// ⚠️ **When the key does not exist this returns cas = 0, and SetSharedData
// with cas = 0 is an unconditional overwrite** (the SDK documents that it
// never returns CasMismatch and always succeeds). So a write made with the
// cas this function hands back for a missing key bypasses CAS entirely.
//
// There is no "create only if absent" primitive in the current ABI. The
// callers therefore never let a meaningful value travel in that write:
// reserveInflight initializes a missing key with the empty state only and
// appends through real compare-and-swaps (see its doc for the two-phase
// scheme and the residual, brand-new-key window); removeInflight returns
// without writing when the key is absent. The health path (recordSuccess /
// recordFailure) still takes the first-write race, where the impact is a
// missed or double-counted *failure tally* -- cosmetic next to admitting or
// rejecting a request, and written down here so it stays a known fact rather
// than something assumed away by "the CAS loop covers everything".
func loadInflight(key string) (inflightState, uint32, bool) {
	var st inflightState
	data, cas, err := proxywasm.GetSharedData(key)
	if err != nil {
		if errors.Is(err, types.ErrorStatusNotFound) {
			return st, cas, true
		}
		return st, 0, false
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &st)
	}
	return st, cas, true
}

func loadHealth(key string) (healthState, uint32, bool) {
	var st healthState
	data, cas, err := proxywasm.GetSharedData(key)
	if err != nil {
		if errors.Is(err, types.ErrorStatusNotFound) {
			return st, cas, true
		}
		return st, 0, false
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &st)
	}
	return st, cas, true
}

// storeJSON returns false on a CAS conflict, meaning the caller should retry.
// Any other error is logged and reported as "success" so the hot path does not
// spin.
func storeJSON(key string, v any, cas uint32) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return true
	}
	err = proxywasm.SetSharedData(key, data, cas)
	if err == nil {
		return true
	}
	if errors.Is(err, types.ErrorStatusCasMismatch) {
		return false
	}
	proxywasm.LogWarnf("%s: shared data write %s failed: %v", pluginName, key, err)
	return true
}
