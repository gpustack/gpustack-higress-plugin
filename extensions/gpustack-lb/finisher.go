package main

import (
	"cmp"
	"encoding/json"
	"math/rand"
	"slices"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const (
	targetClusterHeader = "x-higress-target-cluster"
	fallbackFromHeader  = "x-higress-fallback-from"

	ctxKeyChosen        = "gpustack_lb_chosen"
	ctxKeyInflightToken = "gpustack_lb_inflight_token"
	ctxKeyContentType   = "gpustack_lb_content_type"
)

type chosen struct {
	cluster   string
	key       string
	modelName string
	kind      string
	probation bool
}

func finisherOnHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	// A fallback is an internal redirect that re-runs the whole filter chain.
	// The whole point of that pass is to go somewhere else, so it must not be
	// pinned back to the cluster that just failed: skip LB and strip any
	// header that may have survived.
	//
	// ⚠️ This header is **trusted, and it is an ordinary request header**, so a
	// client that sets it gets LB skipped -- and with cluster_header in place
	// that means no cluster at all, i.e. the 503 that does not flush. Nothing
	// here can tell it apart from one Envoy set (that is precisely what makes
	// it usable as a redirect marker), so the listener has to strip it:
	// internal_only_headers. The README records this as a prerequisite next to
	// the cluster_header patch.
	if v, err := proxywasm.GetHttpRequestHeader(fallbackFromHeader); err == nil && v != "" {
		_ = proxywasm.RemoveHttpRequestHeader(targetClusterHeader)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	set, ok := readCandidateSet()
	if !ok {
		// No candidate set means LB is not enabled on this route. Pass through
		// untouched.
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	// The candidate set is empty. **We must answer ourselves**: on this route
	// Envoy's own 503 for a missing cluster measurably fails to flush to the
	// client, and the request hangs until the client times out. Removing the
	// header is only cleanup; the line below is what actually makes this
	// fail-closed.
	if len(set.Candidates) == 0 {
		_ = proxywasm.RemoveHttpRequestHeader(targetClusterHeader)
		return reject(ctx, config)
	}

	// The reserve walk order: the winner of the usual selection first, then
	// the rest -- so "the best candidate is momentarily full" degrades to the
	// second-best rather than to a rejection.
	pick, totals := selectCandidate(set)
	ordered := reserveOrder(set, pick, totals)
	targets := make([]reserveTarget, 0, len(ordered))
	for i := range ordered {
		targets = append(targets, reserveTarget{
			targetRef: targetRef{Key: ordered[i].Key(), Cluster: ordered[i].Cluster},
			MaxRun:    ordered[i].MaxRunningRequests,
		})
	}

	// Check-and-reserve replaces the old "pick, then +1 unconditionally": the
	// cap check and the in-flight entry are now one atomic operation in the
	// store, so concurrent requests can no longer all observe room that only
	// existed once. The store's action is returned verbatim -- on redis it
	// pauses under watermark and finalizeSelection runs from the callback.
	return config.stateBackend().Reserve(targets, nowMillis(), config.maxInflightAgeMs,
		func(pick int, token string) types.Action {
			return finalizeSelection(ctx, config, ordered, pick, token)
		})
}

// reserveOrder fixes the order the finisher tries targets in: the usual
// selection's winner first, then every other candidate by descending score,
// de-duplicated by Candidate.Key() (same target = same budget, trying it
// twice can only waste a round-trip). Without scores, retain published order.
//
// A **weighted** set stops at the winner: weight is a business traffic split,
// and "the canary is full, send to stable instead" silently rewrites the
// split the route promised. Full is full; the caller rejects.
//
// A pure function (the winner is passed in because selectCandidate reads the
// request id, a host call), so the ordering is unit-testable.
func reserveOrder(set CandidateSet, pick *Candidate, totals []float64) []Candidate {
	if pick == nil {
		return nil
	}
	out := []Candidate{*pick}
	if isWeighted(set.Candidates) {
		return out
	}
	indices := make([]int, len(set.Candidates))
	for i := range indices {
		indices[i] = i
	}
	if len(totals) > 0 {
		// Shuffle before stable sorting so equal-score fallbacks share traffic.
		rand.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
		slices.SortStableFunc(indices, func(a, b int) int { return cmp.Compare(totals[b], totals[a]) })
	}
	seen := map[string]struct{}{pick.Key(): {}}
	for _, i := range indices {
		k := set.Candidates[i].Key()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, set.Candidates[i])
	}
	return out
}

// finalizeSelection is the tail of the decision, run synchronously on the
// shared-data backend and from the redis callback on the async one -- the
// action it returns is what the sync path hands back to the filter chain.
//
// pick comes from the store's Reserve: a 1-based index into ordered on
// success, reserveExhausted (every target full), or reserveFailOpen (the
// backend degraded -- route the preferred candidate without a reservation,
// the old soft-cap behaviour, rather than take the route down over a cache
// blip).
func finalizeSelection(ctx wrapper.HttpContext, config Config, ordered []Candidate, pick int, token string) types.Action {
	var c *Candidate
	switch {
	case pick == reserveExhausted:
		_ = proxywasm.RemoveHttpRequestHeader(targetClusterHeader)
		return rejectCap(ctx, config)
	case pick == reserveFailOpen:
		c = &ordered[0]
		token = "" // nothing was reserved, so there must be nothing to release
	default:
		c = &ordered[pick-1]
	}

	// Overwrite unconditionally if the client sent the header -- this plugin
	// is the only writer, so there is no ambiguity about its origin.
	if err := proxywasm.ReplaceHttpRequestHeader(targetClusterHeader, c.Cluster); err != nil {
		proxywasm.LogErrorf("%s: write %s failed: %v", pluginName, targetClusterHeader, err)
	}

	// The token identifies this request's in-flight entry; the store hands it
	// back at onHttpStreamDone so exactly this entry is released. An empty
	// token means nothing was written, and the release is skipped.
	ctx.SetContext(ctxKeyChosen, chosen{cluster: c.Cluster, key: c.Key(), modelName: c.ModelName, kind: c.Kind, probation: c.Probation})
	ctx.SetContext(ctxKeyInflightToken, token)
	proxywasm.LogDebugf("%s: selected %s (model=%s, reserve=%d)",
		pluginName, c.Cluster, c.ModelName, pick)

	// By this point the selection is done and the header is written -- so
	// **requests with no body, non-JSON bodies, or non-matching paths still
	// get a decision**. This route's weighted_clusters has been displaced by
	// cluster_header, so no header means no cluster: the decision must never
	// return early.
	if !needsRewrite(ctx, config, c) {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	ctx.SetContext(ctxKeyContentType, contentType)
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(config.maxBodyBytes)
	return types.HeaderStopIteration
}

// needsRewrite decides whether this request needs its body buffered to
// rewrite the model name. A candidate with no modelName is a by-pass (passed
// through verbatim), so the body is never read.
func needsRewrite(ctx wrapper.HttpContext, config Config, pick *Candidate) bool {
	if pick.ModelName == "" {
		return false
	}
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		return false
	}
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	matched := false
	for _, suffix := range config.enableOnPathSuffix {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	mt := baseMediaType(contentType)
	return (mt == mtJSON || mt == mtMultipart) && ctx.HasRequestBody()
}

func finisherOnBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	pick, ok := ctx.GetContext(ctxKeyChosen).(chosen)
	if !ok || pick.modelName == "" || len(body) == 0 {
		return types.ActionContinue
	}
	contentType, _ := ctx.GetContext(ctxKeyContentType).(string)
	// The finisher's resolver is the simple one: whatever the client sent, it
	// is rewritten to the chosen candidate's modelName (the publisher already
	// resolved the mapping via modelMappers).
	return rewriteBody(config, body, contentType, func(string) string { return pick.modelName })
}

// finisherOnStreamDone does two things at once because they already share the
// same hook: release the in-flight entry, and read the response properties for
// passive health marking. They go to the store as one record, which lets the
// redis backend settle both in a single round-trip.
func finisherOnStreamDone(ctx wrapper.HttpContext, config Config) {
	pick, ok := ctx.GetContext(ctxKeyChosen).(chosen)
	if !ok {
		return
	}
	now := nowMillis()
	token, _ := ctx.GetContext(ctxKeyInflightToken).(string)

	// provider candidates take no part in passive health marking: their single
	// DNS cluster has many endpoints, so ejecting the whole cluster is an
	// over-reaction -- leave the choice within the cluster to Envoy. The
	// in-flight entry is still released; only the health verdict is skipped.
	oc := outcomeIgnored
	if pick.kind != KindProvider {
		oc = outcomeSuccess
		if isConnectivityFailure() {
			oc = outcomeFailure
		}
	}

	// The threshold is resolved **here**, not in the store: normally
	// unhealthyThreshold, tightened to 1 when this request was admitted during
	// a recovery window. Deciding it once, on this side, is what keeps the two
	// backends from being able to answer the question differently.
	threshold := config.unhealthyThreshold
	if pick.probation {
		threshold = 1
	}

	config.stateBackend().Done(doneRecord{
		Cluster:     pick.cluster,
		InflightKey: pick.key,
		Token:       token,
		Outcome:     oc,
		NowMs:       now,
		MaxAgeMs:    config.maxInflightAgeMs,
		Threshold:   threshold,
		CooldownMs:  config.cooldownMs,
		RampMs:      config.rampMs,
	})
}

func readCandidateSet() (CandidateSet, bool) {
	var set CandidateSet
	data, err := proxywasm.GetProperty([]string{FilterStateCandidates})
	if err != nil || len(data) == 0 {
		return set, false
	}
	if err := json.Unmarshal(data, &set); err != nil {
		proxywasm.LogWarnf("%s: unparseable candidate set: %v", pluginName, err)
		return set, false
	}
	return set, true
}

// requestID is the hash source for the weighted dice roll.
//
// **When it is missing it must fall back to a random value, not the empty
// string**: the hash of "" is a constant, so every request lacking the header
// lands on the same candidate -- a 70/30 split silently becomes 100/0, with no
// signal at all. Envoy normally supplies the header, but
// `generate_request_id: false` or an upstream stripping it both trigger this.
//
// The random fallback loses nothing: determinism matters so that a retry of
// the *same* request lands in the same place, and with no request id there is
// no notion of the same request to begin with.
func requestID() string {
	if v, err := proxywasm.GetHttpRequestHeader("x-request-id"); err == nil && v != "" {
		return v
	}
	return strconv.FormatUint(rand.Uint64(), 36)
}
