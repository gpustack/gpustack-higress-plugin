package main

import (
	"encoding/json"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const ctxKeyCtxContentType = "gpustack_lb_ctx_content_type"

// contextOnHeaders splits two ways: with candidates configured it takes the LB
// path, otherwise it degrades to model-mapper's existing behaviour (for routes
// where LB is not enabled).
func contextOnHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	if config.lbMode {
		return handleLBMode(ctx, config)
	}
	return handleLegacyMode(ctx, config)
}

// handleLBMode does exactly three things: filter, publish, continue. It stays
// in the header phase throughout and never touches the body.
func handleLBMode(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.DontReadRequestBody()

	// A fallback is an internal redirect that re-runs the whole filter chain.
	// The point of that pass is to go somewhere else, so no candidate set is
	// published -- the finisher finds nothing and skips LB.
	if v, err := proxywasm.GetHttpRequestHeader("x-higress-fallback-from"); err == nil && v != "" {
		proxywasm.LogDebugf("%s: fallback pass (%s), skip publishing candidates", pluginName, v)
		return types.ActionContinue
	}

	// The client's model name comes from the x-higress-llm-model header, which
	// model-router (priority 900) sets from the body's model field -- so it is
	// available here **without reading the body**. (This route's
	// exact-match-header-x-higress-llm-model annotation is itself proof the
	// header exists before 795, otherwise the route would not match at all.)
	clientModel, _ := proxywasm.GetHttpRequestHeader("x-higress-llm-model")

	set := buildCandidateSet(config, nowMillis(), clientModel)
	publishCandidates(set)
	proxywasm.LogDebugf("%s: published %d/%d candidates (weighted=%t)",
		pluginName, len(set.Candidates), len(config.candidates), isWeighted(set.Candidates))

	// **Do not call ctx.DisableReroute()**: it stops the route from being
	// re-evaluated after the headers change, which is exactly what the
	// finisher's x-higress-target-cluster write depends on. Calling it makes
	// the override silently ineffective -- the request keeps using
	// weighted_clusters with no error anywhere.
	return types.ActionContinue
}

// handleLegacyMode is higress model-mapper's existing path, preserving its
// decision order verbatim.
func handleLegacyMode(ctx wrapper.HttpContext, config Config) types.Action {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		ctx.DontReadRequestBody()
		return types.ActionContinue
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

	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	mediaType := baseMediaType(contentType)
	bodyCapable := (mediaType == mtJSON || mediaType == mtMultipart) && ctx.HasRequestBody()

	if !matched || !bodyCapable {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	// Verbatim from higress: on a non-LB route the routing decision was
	// already made by an earlier plugin and this path only edits the body, so
	// reroute is disabled. In LB mode this **must not** happen (see
	// handleLBMode).
	ctx.DisableReroute()
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(config.maxBodyBytes)
	ctx.SetContext(ctxKeyCtxContentType, contentType)

	return types.HeaderStopIteration
}

func contextOnBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	// In LB mode DontReadRequestBody is already in effect so this should never
	// be called; if it somehow is, pass through and never rewrite -- rewriting
	// is the finisher's job.
	if config.lbMode || len(body) == 0 {
		return types.ActionContinue
	}
	contentType, _ := ctx.GetContext(ctxKeyCtxContentType).(string)
	// The non-LB resolver is model-mapper's existing priority order:
	// exact -> first matching prefix -> defaultModel -> unchanged.
	return rewriteBody(config, body, contentType, func(old string) string {
		return resolveModel(config, old)
	})
}

// buildCandidateSet reads the shared state, applies every filter, and produces
// the candidate set to publish.
//
// All the filters (health, concurrency, kind) are applied here, so capability
// plugins never need to know anything about health -- that is the direct
// benefit of putting filtering in the publisher (design §7.1).
func buildCandidateSet(config Config, nowMs int64, clientModel string) CandidateSet {
	var out CandidateSet

	type scratch struct {
		c        Candidate
		ejected  bool
		overCap  bool
		maxRun   int64
		inflight int64
		penalty  float64
	}

	items := make([]scratch, 0, len(config.candidates))
	var maxLoad float64

	for _, c := range config.candidates {
		s := scratch{c: c.Candidate}

		// The rewrite target is resolved here: look up the mapping group for
		// this candidate's target and resolve the name the client sent. The
		// finisher receives a settled value and needs no mapping logic.
		s.c.ModelName = resolveCandidateModel(config, c.TargetID, clientModel)

		s.maxRun = c.maxRunningRequests
		s.inflight = readInflight(c.Cluster, nowMs, config.maxInflightAgeMs)
		s.c.Inflight = s.inflight

		// provider candidates take no part in passive health marking: their
		// single DNS cluster has many endpoints, so ejecting the whole cluster
		// is an over-reaction.
		if c.Kind != KindProvider {
			h := readHealth(c.Cluster)
			// Both instants were stamped by the finisher at the moment of
			// ejection; this side only reads them and never computes a window
			// -- which is why cooldownMs / rampMs live in one place only.
			if nowMs < h.EjectedUntil {
				s.ejected = true
			} else if h.RampUntil > h.EjectedUntil && nowMs < h.RampUntil {
				// Recovery: a just-released instance has zero in-flight
				// requests, so without a penalty it is immediately the optimal
				// choice and gets saturated the moment it returns. The penalty
				// decays linearly from 1 to 0.
				s.penalty = float64(h.RampUntil-nowMs) / float64(h.RampUntil-h.EjectedUntil)
				s.c.Probation = true
			}
		}

		// ⚠️ **maxRunningRequests is a soft cap, not a reservation.** This reads
		// a snapshot at 795 and the finisher increments at 700, so requests on
		// different worker threads can all observe inflight < maxRun, all be
		// published, and all be routed -- overshooting the cap by up to the
		// number of admissions in that window.
		//
		// Closing that would mean an atomic check-and-reserve here plus a
		// release on every exit path, which is precisely the machinery §6.3
		// rejected for half-open health: a cross-thread CAS test-and-set whose
		// release path, missed once, wedges the instance for good. Trading a
		// bounded overshoot for a permanent-starvation bug is the wrong side of
		// that deal, and the exact enforcement job belongs to
		// gpustack-rate-limit, which is built for it.
		if s.maxRun > 0 && s.inflight >= s.maxRun {
			s.overCap = true
		}

		// maxLoad only counts candidates that will actually be **published**.
		// Ejected and over-cap ones take no part in the selection that
		// follows, and including them inflates P₀ -- especially an ejected
		// instance, whose in-flight count is often a batch of hung requests
		// that never returned: a high, stale number that would hold a
		// recovering candidate down far longer than it should.
		if !s.ejected && !s.overCap {
			if l := float64(s.inflight); l > maxLoad {
				maxLoad = l
			}
		}
		items = append(items, s)
	}

	// P₀ is the maximum load among the live candidates, plus 1 -- adaptive,
	// with no extra knob. At t=0 a recovering candidate sorts behind everyone
	// else, then linearly returns to normal.
	for i := range items {
		if items[i].penalty > 0 {
			items[i].c.Penalty = items[i].penalty * (maxLoad + 1)
		}
	}

	alive := make([]Candidate, 0, len(items))
	for _, s := range items {
		if s.ejected || s.overCap {
			continue
		}
		alive = append(alive, s.c)
	}

	// When everything is ejected, failOpen re-admits them all: with a
	// misconfigured path, or engines restarting en masse, it is better to send
	// the request and fail than to take the whole route out of service. Note
	// this only opens up for **health** failures; over-capacity is excluded,
	// which is a deliberate rate-limiting semantic.
	if len(alive) == 0 && config.shouldFailOpen() {
		for _, s := range items {
			if s.overCap {
				continue
			}
			alive = append(alive, s.c)
		}
	}

	out.Candidates = alive
	return out
}

func publishCandidates(set CandidateSet) {
	data, err := json.Marshal(set)
	if err != nil {
		proxywasm.LogErrorf("%s: marshal candidates failed: %v", pluginName, err)
		return
	}
	if err := proxywasm.SetProperty([]string{FilterStateCandidates}, data); err != nil {
		proxywasm.LogErrorf("%s: publish candidates failed: %v", pluginName, err)
	}
}

// readInflight counts the in-flight entries that have not aged out.
//
// **The age filter has to be applied on the read side too**, even though the
// finisher already prunes on every +1 and -1. Those prunes only run when this
// candidate is *selected*, and that is exactly what stops happening once it is
// filtered out:
//
//	maxRunningRequests entries leak (hung stream, client disconnect)
//	  -> readInflight >= maxRun, so buildCandidateSet marks it overCap
//	  -> it is never published, so the finisher never selects it
//	  -> addInflight/removeInflight never run on its key
//	  -> nothing ever prunes it, and it stays over cap forever
//
// failOpen does not rescue this either: the re-admission below deliberately
// excludes overCap. So a purely read-side condition would hold the candidate
// out permanently -- the exact starvation that storing timestamps instead of a
// counter was meant to make impossible.
//
// This does not duplicate the knob. maxAgeMs is the same config.maxInflightAgeMs
// the finisher prunes with, inherited through the same deployment-level path,
// so the two cannot disagree. The read stays non-destructive: counting here is
// free, whereas writing the pruned array back would mean a CAS on every
// candidate of every request.
func readInflight(cluster string, nowMs, maxAgeMs int64) int64 {
	data, _, err := proxywasm.GetSharedData(SharedInflightPrefix + cluster)
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
