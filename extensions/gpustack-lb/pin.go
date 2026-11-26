package main

import (
	"encoding/json"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

// pin reasons on the rejection header. gpustack's per-route redirect predicate
// has to list this value next to no_candidate / max_inflight, or a miss is
// re-entered as one more 4xx until the redirect budget is spent -- harmless
// (every pass is a cheap local reply that pins and misses again) but noisy.
const rejectReasonPinMiss = "pin_miss"

// pinCandidate routes a job follow-up to the instance the pin names.
//
// It deliberately does NOT go through selectCandidate / Reserve:
//
//   - The job exists on exactly one instance. Moving to the "next best"
//     candidate when the owner is full, ejected or simply not the argmax is
//     not a degradation, it is a guaranteed 404 -- so there is nothing to
//     select and nothing to walk.
//   - A status poll takes no capacity worth reserving. Rejecting it with 429
//     because the owner is at maxRunningRequests would hide a finished job
//     behind a cap that has nothing to do with it.
//   - A weighted set must be overridden: the business split is for new work,
//     and a follow-up is not new work.
//
// The pin names the target **within the route** -- `7` for an instance,
// `provider-5` for a provider -- and is matched against the published
// candidate set only, never turned into a cluster name on its own. The model
// is deliberately not part of the pin: every instance candidate of a route
// belongs to the route's one model, so within the set the instance id is
// unambiguous, and the route id (which is what the caller has) is not the
// model id anyway. The cluster header written is the candidate's own, port
// and resolution suffix included. That is also what bounds the mechanism:
// a pin can only name a target that is already a legitimate candidate of
// this route. One the context role filtered out (removed, ejected, narrowed
// away by a group) is a miss, and the honest answer to a miss is 404: the
// job lived in that engine's memory.
//
// The chosen candidate is still recorded in ctx so onStreamDone reports the
// health outcome: a poll that cannot connect to its instance is real
// evidence about that instance. No in-flight entry is written, so there is
// no token to release.
func pinCandidate(ctx wrapper.HttpContext, set CandidateSet, pin VideoPin, rules wire.TargetRules) types.Action {
	if c := pinnedCandidate(set, pin, rules); c != nil {
		if err := proxywasm.ReplaceHttpRequestHeader(targetClusterHeader, c.Cluster); err != nil {
			proxywasm.LogErrorf("%s: write %s failed: %v", pluginName, targetClusterHeader, err)
		}
		// Probation is carried like finalizeSelection carries it: a pinned
		// poll that cannot connect to a just-recovered instance must count
		// with the tightened threshold, or the recovering instance stays
		// eligible for new work on the strength of evidence never recorded.
		ctx.SetContext(ctxKeyChosen, chosen{cluster: c.Cluster, key: c.Key(), modelName: c.ModelName, kind: c.Kind, probation: c.Probation})
		proxywasm.LogDebugf("%s: pinned to %s (model=%s)", pluginName, c.Cluster, c.ModelName)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	proxywasm.LogInfof("%s: pin miss: %s is not a candidate of this route", pluginName, pin.Target)
	_ = proxywasm.RemoveHttpRequestHeader(targetClusterHeader)
	return rejectPinMiss(ctx)
}

// rejectPinMiss answers a pin that names no candidate of this route. Same
// delivery rules as reject / rejectCap (DisableReroute first, ActionPause
// after, non-empty JSON body) -- learned on the same filter chain. The
// status is a fixed 404: it is the literal truth (the resource is not
// reachable through this route any more) and the one status the OpenAI SDK
// surfaces as "not found" rather than as a server fault.
func rejectPinMiss(ctx wrapper.HttpContext) types.Action {
	ctx.DisableReroute()

	if err := proxywasm.SendHttpResponseWithDetail(
		404,
		pluginName+"."+rejectReasonPinMiss,
		[][2]string{{"content-type", "application/json"}, {rejectionHeader, rejectReasonPinMiss}},
		buildRejectBody("video not found: the instance that owns this video is no longer available",
			"invalid_request_error", 404),
		-1,
	); err != nil {
		proxywasm.LogErrorf("%s: SendHttpResponseWithDetail failed: %v", pluginName, err)
	}
	return types.ActionPause
}

// readVideoPin reads the pin the entry plugin left in filter state. Absent,
// empty or unparseable all read as "no pin" -- a corrupt value must degrade
// to ordinary selection, not to a request with no decision (cluster_header
// routing: no header means no cluster).
//
// The target's shape is not checked here: what a valid target looks like is
// targetPatterns' business, and the join is a whole-string comparison against
// targets derived from the published candidates, so a stray target can only
// ever be a miss.
func readVideoPin() (VideoPin, bool) {
	var pin VideoPin
	data, err := proxywasm.GetProperty([]string{FilterStateVideoPin})
	if err != nil || len(data) == 0 {
		return pin, false
	}
	if err := json.Unmarshal(data, &pin); err != nil {
		proxywasm.LogWarnf("%s: unparseable video pin: %v", pluginName, err)
		return pin, false
	}
	if pin.Target == "" {
		proxywasm.LogWarnf("%s: video pin without a target; ignoring", pluginName)
		return pin, false
	}
	switch pin.Mode {
	case "":
		pin.Mode = PinModeJob
	case PinModeJob, PinModeSelect:
	default:
		proxywasm.LogWarnf("%s: unknown video pin mode %q; ignoring pin", pluginName, pin.Mode)
		return pin, false
	}
	return pin, true
}

// pinnedCandidate returns the candidate the pin names, nil on a miss.
func pinnedCandidate(set CandidateSet, pin VideoPin, rules wire.TargetRules) *Candidate {
	if pin.Target == "" {
		return nil
	}
	for i := range set.Candidates {
		if pinMatches(pin.Target, clusterFQDN(set.Candidates[i].Cluster), rules) {
			return &set.Candidates[i]
		}
	}
	return nil
}

// pinMatches reports whether the candidate's service name (Envoy prefix and
// resolution suffix removed) IS the target, that is, reads back as the
// target through targetPatterns. One direction, one definition -- the same
// TargetOf the router used to put the target into the ID -- so instance 7
// never matches 71 and no pattern can be wider than the target it was
// built for.
func pinMatches(target, service string, rules wire.TargetRules) bool {
	return target != "" && rules.TargetOf(service) == target
}

// clusterFQDN reduces an Envoy cluster name to the service name a pin can
// name: the `outbound|<port>|<subset>|` prefix dropped, subset or not (a
// subset cluster addresses the same service, and the router's wrap prefix
// derives the target the same way), then everything from the first dot on
// removed (`model-1-2.static` -> `model-1-2`, `provider-5.static.svc.cluster.local`
// -> `provider-5`). The first dot, not the last: the resolution suffix is
// one or more labels (`static`, `dns`) and may be followed by a search
// domain. Any other shape keeps its whole name and so never matches.
func clusterFQDN(cluster string) string {
	parts := strings.SplitN(cluster, "|", 4)
	name := cluster
	if len(parts) == 4 && parts[0] == "outbound" && parts[3] != "" {
		name = parts[3]
	}
	if i := strings.IndexByte(name, '.'); i != -1 {
		return name[:i]
	}
	return name
}
