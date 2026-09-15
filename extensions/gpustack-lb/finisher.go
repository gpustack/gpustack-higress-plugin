package main

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const (
	targetClusterHeader = "x-higress-target-cluster"
	fallbackFromHeader  = "x-higress-fallback-from"

	ctxKeyChosen      = "gpustack_lb_chosen"
	ctxKeyStartMs     = "gpustack_lb_start_ms"
	ctxKeyContentType = "gpustack_lb_content_type"
)

type chosen struct {
	cluster   string
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

	pick := selectCandidate(set)
	if pick == nil {
		// The candidate set is empty. **We must answer ourselves**: on this
		// route Envoy's own 503 for a missing cluster measurably fails to
		// flush to the client, and the request hangs until the client times
		// out. Removing the header is only cleanup; the line below is what
		// actually makes this fail-closed.
		_ = proxywasm.RemoveHttpRequestHeader(targetClusterHeader)
		return reject(ctx, config)
	}

	// Overwrite unconditionally if the client sent the header -- this plugin
	// is the only writer, so there is no ambiguity about its origin.
	if err := proxywasm.ReplaceHttpRequestHeader(targetClusterHeader, pick.Cluster); err != nil {
		proxywasm.LogErrorf("%s: write %s failed: %v", pluginName, targetClusterHeader, err)
	}

	startMs := addInflight(pick.Cluster, nowMillis(), config.maxInflightAgeMs)
	ctx.SetContext(ctxKeyChosen, chosen{cluster: pick.Cluster, modelName: pick.ModelName, kind: pick.Kind, probation: pick.Probation})
	ctx.SetContext(ctxKeyStartMs, startMs)
	proxywasm.LogDebugf("%s: selected %s (model=%s, weighted=%t)",
		pluginName, pick.Cluster, pick.ModelName, isWeighted(set.Candidates))

	// By this point the selection is done and the header is written -- so
	// **requests with no body, non-JSON bodies, or non-matching paths still
	// get a decision**. This route's weighted_clusters has been displaced by
	// cluster_header, so no header means no cluster: the decision must never
	// return early.
	if !needsRewrite(ctx, config, pick) {
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
// same hook: decrement the in-flight count, and read the response properties
// for passive health marking.
func finisherOnStreamDone(ctx wrapper.HttpContext, config Config) {
	pick, ok := ctx.GetContext(ctxKeyChosen).(chosen)
	if !ok {
		return
	}
	now := nowMillis()

	if startMs, ok := ctx.GetContext(ctxKeyStartMs).(int64); ok {
		removeInflight(pick.cluster, startMs, now, config.maxInflightAgeMs)
	}

	// provider candidates take no part in passive health marking: their single
	// DNS cluster has many endpoints, so ejecting the whole cluster is an
	// over-reaction -- leave the choice within the cluster to Envoy.
	if pick.kind == KindProvider {
		return
	}

	if isConnectivityFailure() {
		recordFailure(pick.cluster, pick.probation, config, now)
		return
	}
	recordSuccess(pick.cluster)
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
