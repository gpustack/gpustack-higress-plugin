package main

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
)

// The finisher role is the **only writer** of these two pieces of shared
// state; the context role only reads them.
//
// They have to live in shared data rather than per-VM memory: the two modes
// are **two filter instances** of the same binary, each with its own linear
// memory, and merging the binaries does not let them share variables. The cost
// is two CAS operations per request (+1 on selection, -1 on onStreamDone).

const casMaxRetries = 10

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

// addInflight records one in-flight request and returns the timestamp it
// wrote, so onHttpStreamDone can remove exactly that entry.
//
// It stores each request's start time rather than a counter: a counter cannot
// be lazily pruned, and the decrement path is where this kind of feature fails
// (early-terminated streams, client disconnects, upstream timeouts). Miss one
// decrement and the counter only grows, eventually starving that instance for
// good.
// A return of 0 means **nothing was written** (the read failed, or the CAS
// budget ran out); the caller uses that to skip the paired removeInflight,
// which would otherwise burn another CAS on a record that never existed.
func addInflight(cluster string, nowMs, maxAgeMs int64) int64 {
	key := SharedInflightPrefix + cluster
	for attempt := 0; attempt < casMaxRetries; attempt++ {
		st, cas, ok := loadInflight(key)
		if !ok {
			return 0
		}
		st.Starts = pruneStarts(st.Starts, nowMs, maxAgeMs)
		st.Starts = append(st.Starts, nowMs)
		if storeJSON(key, st, cas) {
			return nowMs
		}
	}
	proxywasm.LogWarnf("%s: gave up adding inflight for %s after %d CAS retries",
		pluginName, cluster, casMaxRetries)
	return 0
}

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

func removeInflight(cluster string, startMs, nowMs, maxAgeMs int64) {
	if startMs == 0 {
		return // addInflight did not write, so there is nothing to remove
	}
	key := SharedInflightPrefix + cluster
	for attempt := 0; attempt < removeCasRetries; attempt++ {
		st, cas, ok := loadInflight(key)
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
		if storeJSON(key, st, cas) {
			return
		}
	}
	proxywasm.LogWarnf("%s: gave up removing inflight for %s after %d CAS retries; "+
		"the entry will linger until it ages out", pluginName, cluster, removeCasRetries)
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
// The threshold is asymmetric: normally unhealthyThreshold (3 by default, so
// transient blips do not cause collateral damage), but 1 during recovery -- a
// just-released instance has zero in-flight requests and therefore looks
// optimal, so another failure should send it straight back rather than letting
// it repeatedly soak up traffic.
//
// probation is published by the context role along with the candidate set
// rather than recomputed here from rampMs: that window is configured in one
// place only, which removes a knob that would otherwise have to agree on both
// sides. It is also the more accurate semantic -- it means "this request was
// admitted during the recovery window", not "we are still in it right now".
func recordFailure(cluster string, probation bool, config Config, nowMs int64) {
	key := SharedHealthPrefix + cluster
	for attempt := 0; attempt < casMaxRetries; attempt++ {
		st, cas, ok := loadHealth(key)
		if !ok {
			return
		}
		threshold := config.unhealthyThreshold
		if probation {
			threshold = 1
		}
		st.Fails++
		if st.Fails >= threshold {
			st.Fails = 0
			// Stamp both instants together: the reader (context) derives the
			// penalty and probation from them and never needs to know
			// cooldownMs / rampMs. That fixes the window per occurrence, so
			// editing the config later cannot reinterpret an in-progress
			// cooldown as a different length.
			st.EjectedUntil = nowMs + config.cooldownMs
			st.RampUntil = st.EjectedUntil + config.rampMs
			proxywasm.LogWarnf("%s: ejecting %s until %d (ramp until %d)",
				pluginName, cluster, st.EjectedUntil, st.RampUntil)
		}
		if storeJSON(key, st, cas) {
			return
		}
	}
}

// ⚠️ **When the key does not exist this returns cas = 0, and SetSharedData
// with cas = 0 is an unconditional overwrite** (proxy-wasm specifies it never
// returns CasMismatch). So the **first** write to any key bypasses CAS: if two
// workers record the first in-flight request for a new cluster at the same
// time, one of them is silently clobbered.
//
// There is no "create only if absent" primitive in the current ABI, so this
// cannot be fixed. The impact is genuinely limited: the window only exists
// before the key is established (gateway start, or a new instance appearing),
// what is lost is the first couple of in-flight counts, and in-flight counting
// already self-corrects through lazy timestamp pruning. It is written down here
// so it is a known fact rather than something assumed away by "the CAS loop
// covers everything".
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
