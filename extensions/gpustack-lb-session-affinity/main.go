// gpustack-lb-session-affinity is a **capability plugin** of the LB framework:
// it sends the requests of one session consistently to the same instance.
//
//	795 gpustack-lb (context)   publishes the candidate set into filter state
//	780 this plugin             reads the set -> rendezvous ranking -> appends
//	                            one scored opinion
//	700 gpustack-lb (finisher)  L1-normalises, sums the weighted opinions,
//	                            picks a candidate, writes the cluster header
//
// It **only states an opinion, it does not decide**: it scores every candidate
// and the finisher combines all the entries. Because it scores the whole set,
// a candidate becoming unavailable does not invalidate this entry -- the
// relative preference among the rest still holds, which is exactly what
// consistent hashing is for.
//
// Priority within the capability band carries no meaning: the opinions are
// combined by weighted sum, so who writes first does not change the result. To
// change how much say this plugin has, change its `weight`.
//
// Three things it deliberately **does not** do, each corresponding to a silent
// failure mode:
//
//   - **Never calls ctx.DisableReroute().** That writes the request-level
//     property clear_route_cache=off, which takes effect across plugins, and
//     the finisher's x-higress-target-cluster write depends on the route being
//     re-evaluated. Calling it makes the override silently ineffective: the
//     request keeps using weighted_clusters with no error anywhere.
//   - **Never reads shared data.** Load and health were already filtered by
//     the publisher and come down with the candidate set (design §7.1).
//     Reading them again would return the same numbers plus a host call.
//   - **Never writes the cluster header.** Only the finisher writes it, so
//     there is no ambiguity about its origin.
//
// Design reference: the LB framework design doc §3.1, §4.2, §7.1, §8.3.
package main

import (
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const pluginName = "gpustack-lb-session-affinity"

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		// ParseOverrideConfig rather than ParseConfig: matchRules need to
		// inherit the deployment-level knobs from defaultConfig (weight,
		// enableOnPathSuffix), or setting them globally would be silently
		// reset to the defaults by every rule.
		wrapper.ParseOverrideConfig(parseConfig, parseOverrideConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		// WithRebuildAfterRequests is deliberately not set: requests inside
		// the rebuild window get a 503, and this plugin sits on the LB
		// decision path. It also holds no cross-request state, so there is no
		// source of memory growth.
	)
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	// No candidate set means LB is not enabled on this route (or this is the
	// fallback redirect pass, where the publisher deliberately publishes
	// nothing). Do nothing.
	set, ok := readCandidates()
	if !ok || len(set.Candidates) == 0 {
		return skip(ctx)
	}

	// Weighted routes exit early (design §4.2): candidates carrying a weight
	// mean this route has a business split, and the finisher will ignore every
	// scoring opinion. Continuing would be pure waste -- and it would leave an
	// opinion in the logs that never took effect, which misleads whoever is
	// troubleshooting.
	if isWeighted(set.Candidates) {
		proxywasm.LogDebugf("%s: weighted route, skipping", pluginName)
		return skip(ctx)
	}

	// **Header sources take precedence over body sources as a group**,
	// regardless of their relative position in sessionKeys. Reading a header
	// is free, whereas every body source requires stopping iteration to buffer
	// the body; buffering first and only then discovering that a header was
	// available all along is a pure loss. Order among the headers, and among
	// the body keys, still follows the array strictly.
	if src, key := firstHeaderKey(config); key != "" {
		publish(set, config, key, src)
		return skip(ctx)
	}

	// With no body source there is nothing further to do.
	//
	// ctx.HasRequestBody() is part of the gate, not an optimisation: it reports
	// whether end_of_stream already arrived on the headers. If it did, Envoy
	// never calls decodeData, so returning HeaderStopIteration below would
	// **stall the request until the client gives up** -- the wasm-go wrapper
	// does not guard this, and a bodyless request that still carries
	// `content-type: application/json` on a /responses or /messages path passes
	// both of bodyLookupApplies' gates. The finisher checks the same thing
	// before it stops iteration to buffer.
	if !config.hasBodySource() || !ctx.HasRequestBody() || !bodyLookupApplies(config) {
		return skip(ctx)
	}
	// The buffer limit has to be raised **here**, not left to the finisher.
	//
	// This plugin stops iteration at 780 and the finisher only calls
	// SetRequestBodyBufferLimit at 700, so by the time the finisher raises it
	// Envoy has already buffered this body under whatever the route default is
	// -- 1 MiB in stock Envoy, and Higress deployments run values as low as
	// 32 KiB. A /v1/messages body carrying images would then get a 413 at this
	// point in the chain, meaning **installing this plugin changes which
	// requests succeed**, which a capability plugin must never do.
	//
	// maxBodyBytes therefore defaults to the same value as gpustack-lb's, and
	// the two must be configured together. The direction matters: raising it to
	// match the finisher is the fix, *lowering* it below the finisher's is what
	// causes the 413 this comment used to warn about.
	ctx.SetRequestBodyBufferLimit(config.maxBodyBytes)
	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if len(body) == 0 || !config.hasBodySource() {
		return types.ActionContinue
	}
	// The candidate set was read once in the header phase, and has to be read
	// again here: the whole body buffering sits in between, and stashing it on
	// the HttpContext would only keep a second copy of the same JSON.
	set, ok := readCandidates()
	if !ok || len(set.Candidates) == 0 || isWeighted(set.Candidates) {
		return types.ActionContinue
	}

	for _, src := range config.sessionKeys {
		if src.isHeader() {
			continue
		}
		if key := sessionKeyFromBody(body, src.name); key != "" {
			publish(set, config, key, src)
			return types.ActionContinue
		}
	}
	return types.ActionContinue
}

// firstHeaderKey returns the first header source that yields a value, in array
// order.
func firstHeaderKey(config Config) (keySource, string) {
	for _, src := range config.sessionKeys {
		if !src.isHeader() {
			continue
		}
		v, err := proxywasm.GetHttpRequestHeader(src.name)
		if err != nil {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			return src, v
		}
	}
	return keySource{}, ""
}

// skip is the common "no opinion this time" tail.
//
// DontReadRequestBody only sets a flag on this context and does not affect the
// finisher's body buffering (wasm-go's implementation is a single field
// assignment), so turning it off here is safe.
func skip(ctx wrapper.HttpContext) types.Action {
	ctx.DontReadRequestBody()
	return types.ActionContinue
}

// publish appends the scored opinion to the shared array.
//
// **Requests with no session key never reach this function** -- callers only
// invoke it with a non-empty key. That is the single most important point in
// this plugin: the overwhelming majority of requests (stateless
// chat/completions, the first request of a session) have no key, and hashing
// the empty string would give all of them the same ranking and pin them to one
// instance. Load balancing would fail outright, and silently. Abstaining
// instead simply leaves the total to the other entries (least-load, or the
// built-in round-robin when there are none).
func publish(set candidateSet, config Config, key string, src keySource) {
	scores := affinityScores(set.Candidates, key)
	if len(scores) == 0 {
		return
	}
	appendRank(rankEntry{Name: pluginName, Weight: config.weight, Scores: scores})
	proxywasm.LogDebugf("%s: ranked %d candidates from %s %q (weight %.2f)",
		pluginName, len(scores), src.kind, src.name, config.weight)
}
