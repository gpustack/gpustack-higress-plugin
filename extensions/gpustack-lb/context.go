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
//
// Ordering on a fallback (redirect re-entry) pass, each stage narrowing the
// next: exclude every ATTEMPTED cluster (the accumulated x-gpustack-lb-tried
// header — excluding only the last one would let two failing main clusters
// alternate until the redirect budget expires); then groups (a group
// narrowing to [A,F] must keep F eligible even when the main-tier survivor B
// exists — the group's verdict outranks the tier); then the tier split (main
// tier while it has survivors in the narrowed set, the fallback tier only
// when it does not). A normal pass runs the same chain minus the exclusion.
// Only when the marker is present but no target header survived does the
// legacy behaviour (publish nothing, let the fallback ingress own the pass)
// apply.
func handleLBMode(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.DontReadRequestBody()

	// The untouched configured pool, PRE-exclusion: the degenerate guard's
	// last resort re-runs the rule narrowing over it (the post-exclusion
	// pool is empty precisely when everything allowed was tried, so it
	// cannot serve as its own rescue source).
	originalPool := config.candidates

	if v, err := proxywasm.GetHttpRequestHeader(fallbackFromHeader); err == nil && v != "" {
		if prev, terr := proxywasm.GetHttpRequestHeader(targetClusterHeader); terr != nil || prev == "" {
			// No LB-written target header: either a legacy fallback deployment
			// (its fallback ingress steers by this very marker) or a forged
			// marker on a request LB never touched. Publish nothing either way.
			proxywasm.LogDebugf("%s: fallback pass (%s) without a target header, skip publishing candidates", pluginName, v)
			return types.ActionContinue
		}
		tried := triedClusters()
		if len(tried) == 0 {
			// The accumulator header did not survive (very first pass of a
			// deployment that predates it, or stripped mid-chain): fall back
			// to excluding the last target header only.
			if prev, terr := proxywasm.GetHttpRequestHeader(targetClusterHeader); terr == nil && prev != "" {
				tried = []string{prev}
			}
		}
		narrowed := excludeClusters(config.candidates, tried)
		if len(narrowed) == len(config.candidates) {
			proxywasm.LogDebugf("%s: fallback pass (%s): no candidate excluded, keeping the full set", pluginName, v)
		} else {
			proxywasm.LogDebugf("%s: fallback pass (%s): excluding %d tried cluster(s), %d/%d candidates remain", pluginName, v, len(tried), len(narrowed), len(config.candidates))
		}
		config.candidates = narrowed
	}

	// The client's model name comes from the x-higress-llm-model header, which
	// model-router (priority 900) sets from the body's model field -- so it is
	// available here **without reading the body**. (This route's
	// exact-match-header-x-higress-llm-model annotation is itself proof the
	// header exists before 340, otherwise the route would not match at all.)
	clientModel, _ := proxywasm.GetHttpRequestHeader("x-higress-llm-model")

	// groups narrow the pool first (deterministic routing): state is
	// then loaded only for the surviving candidates, and every later stage
	// -- health filtering, failOpen, the capability band's scoring -- sees
	// only the narrowed subset, so a matched request can never silently
	// fall back to a candidate its group excluded. applyRouteGroups returns
	// config.candidates itself when no group matches, so an unmatched
	// request is literally the same code path as before this feature.
	if len(config.groups) > 0 {
		config.candidates = applyRouteGroups(config, clientModel)
	}

	// The tier split runs AFTER the rule narrowing: a rule's verdict
	// outranks the tier (a rule narrowing to [A, F] must keep the fallback
	// F eligible even when a main-tier survivor exists outside the rule).
	// A normal pass publishes the main tier only — the fallback tier takes
	// no share of healthy traffic. On a fallback pass the main tier serves
	// while it has survivors in the narrowed set; the fallback tier only
	// when it does not. The tier split narrows ONLY the published set:
	// config.candidates keeps the full (main+fallback) narrowed pool so
	// the health-aware drain inside LoadStates can still reach the
	// fallback tier when every main survivor turns out to be ejected.
	publishPool := config.candidates
	if main := mainTier(config.candidates); len(main) > 0 {
		config.candidates = main
	}

	// Degenerate guard: if every stage together emptied the set — all
	// rule-allowed candidates sit on tried clusters — writing nothing
	// under cluster_header is the 503 that does not flush. Re-run the
	// rule narrowing over the ORIGINAL pool (not the post-exclusion
	// rulePool, which is empty in exactly this situation) and restore
	// its tier split into BOTH the published set and the state-loading
	// pool (restoring only config.candidates would leave publishPool
	// empty and the rescued candidates stateless). The redirect budget
	// bounds the loop and health marking meanwhile drives the dead
	// instances out.
	if len(config.candidates) == 0 && len(originalPool) > 0 {
		rescueCfg := config
		rescueCfg.candidates = originalPool
		rescuePool := originalPool
		if len(rescueCfg.groups) > 0 {
			rescuePool = applyRouteGroups(rescueCfg, clientModel)
		}
		if rescue := tieredFallback(rescuePool); len(rescue) > 0 {
			proxywasm.LogDebugf("%s: all-tried dead end, rescuing %d rule-allowed candidate(s)", pluginName, len(rescue))
			// config.candidates takes the tier-restricted pool (the
			// published set), publishPool keeps the FULL rule pool —
			// the same shape as the normal path, so the state load and
			// the healthy-fallback drain can still reach a fallback
			// candidate when the rescued main tier turns out ejected.
			config.candidates = rescue
			publishPool = rescuePool
		}
	}

	// **Do not call ctx.DisableReroute()**: it stops the route from being
	// re-evaluated after the headers change, which is exactly what the
	// finisher's x-higress-target-cluster write depends on. Calling it makes
	// the override silently ineffective -- the request keeps using
	// weighted_clusters with no error anywhere.
	//
	// The action comes from the store, because whether this request has to wait
	// decides it: shared data answers in-process and returns ActionContinue,
	// redis pauses under watermark and resumes from the callback. Everything
	// below the load is identical either way, which is what keeps the two
	// backends from drifting apart.
	//
	// States are loaded for the FULL narrowed pool (main + fallback tier):
	// loading only the main tier would leave the drain below stateless.
	nowMs := nowMillis()
	return config.stateBackend().LoadStates(candidateRefs(publishPool), nowMs, config.maxInflightAgeMs,
		func(states map[string]clusterState) {
			// Prefer the healthy main tier, then drain to the healthy
			// fallback tier, and only as the last resort let failOpen
			// re-admit condemned instances (buildCandidateSet's own
			// fail-open logic). buildFailClosed enforces that preference
			// order without ever handing an ejected candidate back.
			if set := buildFailClosed(config, publishPool, nowMs, clientModel, states); len(set.Candidates) > 0 {
				publishCandidates(set)
				proxywasm.LogDebugf("%s: published %d/%d candidates (weighted=%t)",
					pluginName, len(set.Candidates), len(publishPool), isWeighted(set.Candidates))
				return
			}
			set := buildCandidateSet(config, nowMs, clientModel, states)
			publishCandidates(set)
			proxywasm.LogDebugf("%s: fail-open published %d/%d candidates (weighted=%t)",
				pluginName, len(set.Candidates), len(publishPool), isWeighted(set.Candidates))
		})
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

// buildCandidateSet applies every **health** filter to the state the store
// just loaded and produces the candidate set to publish.
//
// The concurrency cap is **deliberately not applied here**. Filtering on the
// loaded snapshot is a read-then-decide: requests on different workers (or
// different gateway replicas) can all observe inflight < maxRun and all be
// published, overshooting the cap by the number of admissions in the window.
// The cap is enforced by the finisher's atomic check-and-reserve instead
// (store.Reserve), where the check and the +1 are one operation; what this
// function publishes is the candidate's **cap itself** (MaxRunningRequests)
// alongside its load, so the finisher knows each target's budget. Stale
// snapshots therefore only affect *ranking*, never correctness of the cap.
//
// It takes the state as an argument rather than reading it, which makes it a
// **pure function**: the whole filter -- ejection, the penalty curve,
// fail-open re-admission -- is unit-testable without a wasm host, and it is
// the same code on both backends.
//
// A key missing from states has no state yet, which reads as zero in-flight
// requests and a clean health record. That is also exactly what a failed redis
// load produces, which is the fail-open posture: no state means nothing is
// filtered out.
func buildCandidateSet(config Config, nowMs int64, clientModel string, states map[string]clusterState) CandidateSet {
	var out CandidateSet

	type scratch struct {
		c       Candidate
		ejected bool
	}

	items := make([]scratch, 0, len(config.candidates))
	var maxLoad float64

	for _, c := range config.candidates {
		s := scratch{c: c.Candidate}

		// The rewrite target is resolved here: look up the mapping group for
		// this candidate's target and resolve the name the client sent. The
		// finisher receives a settled value and needs no mapping logic.
		s.c.ModelName = resolveCandidateModel(config, c.TargetID, clientModel)

		// The cap rides on the candidate so the finisher can reserve against
		// it; the load snapshot rides along for ranking only.
		s.c.MaxRunningRequests = c.maxRunningRequests
		state := states[c.Key()]
		s.c.Inflight = state.Inflight

		// provider candidates take no part in passive health marking: their
		// single DNS cluster has many endpoints, so ejecting the whole cluster
		// is an over-reaction.
		if c.Kind != KindProvider {
			h := state.Health
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
				s.c.Penalty = float64(h.RampUntil-nowMs) / float64(h.RampUntil-h.EjectedUntil)
				s.c.Probation = true
			}
		}

		// maxLoad only counts candidates that will actually be **published**.
		// Ejected ones take no part in the selection that follows, and
		// including them inflates P₀ -- especially an ejected instance, whose
		// in-flight count is often a batch of hung requests that never
		// returned: a high, stale number that would hold a recovering
		// candidate down far longer than it should.
		if !s.ejected {
			if l := float64(state.Inflight); l > maxLoad {
				maxLoad = l
			}
		}
		items = append(items, s)
	}

	// P₀ is the maximum load among the live candidates, plus 1 -- adaptive,
	// with no extra knob. At t=0 a recovering candidate sorts behind everyone
	// else, then linearly returns to normal.
	for i := range items {
		if items[i].c.Penalty > 0 {
			items[i].c.Penalty = items[i].c.Penalty * (maxLoad + 1)
		}
	}

	alive := make([]Candidate, 0, len(items))
	for _, s := range items {
		if s.ejected {
			continue
		}
		alive = append(alive, s.c)
	}

	// When everything is ejected, failOpen re-admits them all: with a
	// misconfigured path, or engines restarting en masse, it is better to send
	// the request and fail than to take the whole route out of service. (The
	// concurrency cap needs no equivalent here: over-cap is no longer a
	// publish filter, and the finisher's reserve answers "everything full"
	// with the cap-reject response on its own.)
	if len(alive) == 0 && config.shouldFailOpen() {
		for _, s := range items {
			alive = append(alive, s.c)
		}
	}

	out.Candidates = alive
	return out
}

// triedClusters reads the accumulated attempted-clusters header written by
// the finisher on every pass (comma-separated). Empty when the header is
// absent (first pass, or a chain that stripped it).
func triedClusters() []string {
	v, err := proxywasm.GetHttpRequestHeader(triedClustersHeader)
	if err != nil || v == "" {
		return nil
	}
	var out []string
	for _, c := range strings.Split(v, ",") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// excludeClusters removes every candidate of the given clusters — the
// fallback re-selection primitive over the ATTEMPTED set. Pure function so
// it is directly unit-testable; the host calls stay in handleLBMode.
//
// Two spellings of one identity reach this function, so both are accepted:
// the accumulator records the service name (see triedKey), while the legacy
// path — no accumulator header survived — passes the full cluster name from
// x-higress-target-cluster. Matching only one would silently re-select the
// cluster that just failed and never drain to fallback.
//
// Drift between this and recordTriedCluster does not fail loudly: nothing
// would ever match, and every re-selection pass would see the full pool
// again -- spending the redirect budget retrying the instance that just
// failed. TestExclusionAcceptsBothSpellings pins the pairing.
func excludeClusters(cands []candidateSpec, clusters []string) []candidateSpec {
	if len(clusters) == 0 {
		return cands
	}
	exclude := make(map[string]struct{}, len(clusters))
	for _, c := range clusters {
		exclude[c] = struct{}{}
	}
	out := make([]candidateSpec, 0, len(cands))
	for _, c := range cands {
		if _, bad := exclude[triedKey(c.Candidate)]; bad {
			continue
		}
		if _, bad := exclude[c.Cluster]; bad {
			continue
		}
		out = append(out, c)
	}
	return out
}

// triedKey is the identity a candidate contributes to the tried-clusters
// accumulator: the service name alone, so the header stays short and survives
// a port change. It is derived rather than stored -- the finisher reads the
// published candidate, and there is nothing about the key that the cluster
// name does not already determine (checkServiceKeys guarantees the service
// name is unambiguous across the route).
func triedKey(c Candidate) string {
	return serviceKey(c.Cluster)
}

// mainTier keeps only the non-fallback candidates. The inverse filter is
// the fallback tier: those candidates join the published set only on a
// re-selection pass whose main tier is exhausted.
func mainTier(cands []candidateSpec) []candidateSpec {
	out := make([]candidateSpec, 0, len(cands))
	for _, c := range cands {
		if !c.Fallback {
			out = append(out, c)
		}
	}
	return out
}

// fallbackTierOnly returns the fallback-tier candidates of a narrowed set
// that are not ejected — the health-aware drain target when the main tier
// produced nothing publishable. nil when there is no healthy fallback
// candidate. Pure over the loaded state, so it is unit-testable.
func fallbackTierOnly(cands []candidateSpec, states map[string]clusterState) []candidateSpec {
	var out []candidateSpec
	now := nowMillis()
	for _, c := range cands {
		if !c.Fallback {
			continue
		}
		if h := states[c.Key()].Health; now < h.EjectedUntil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// tieredFallback restores a tier split over an emptied pool: the main tier
// of the full (post-rule) pool when it has any candidate, otherwise the
// fallback tier. Used by the degenerate all-tried guard, which must
// republish SOMETHING (an empty cluster_header set is a non-flushing 503)
// without letting the escape hatch bypass the matched route rule or the
// tier discipline.
func tieredFallback(fullPool []candidateSpec) []candidateSpec {
	if main := mainTier(fullPool); len(main) > 0 {
		return main
	}
	return fullPool
}

// buildFailClosed is the strict preference order over the narrowed pool
// with fail-open OFF: healthy main tier first, then the healthy fallback
// tier. It bypasses buildCandidateSet's fail-open re-admission entirely
// (that path hands back the very instances the health verdict condemned);
// an empty result tells the caller the pool has nothing left and the
// fail-open build is the last resort. Pure over the loaded state, so it
// is unit-testable.
var noFailOpen = false

func buildFailClosed(config Config, publishPool []candidateSpec, nowMs int64, clientModel string, states map[string]clusterState) CandidateSet {
	// The published set is the main tier when it has survivors in the
	// narrowed pool (set by handleLBMode); the fallback tier of the FULL
	// narrowed pool (publishPool, main + fallback) is the drain target --
	// the main-tier config alone no longer contains it.
	fc := config
	fc.failOpen = &noFailOpen
	if set := buildCandidateSet(fc, nowMs, clientModel, states); len(set.Candidates) > 0 {
		return set
	}
	fbc := fc
	fbc.candidates = fallbackTierOnly(publishPool, states)
	if fbc.candidates == nil {
		return CandidateSet{}
	}
	return buildCandidateSet(fbc, nowMs, clientModel, states)
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
