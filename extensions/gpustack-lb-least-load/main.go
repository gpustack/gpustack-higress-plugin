// gpustack-lb-least-load is a **capability plugin** of the LB framework: it
// sends each request to the instance with the lowest current load.
//
//	795 gpustack-lb (context)          publishes the candidate set (inflight /
//	                                   penalty already included)
//	780 gpustack-lb-session-affinity   session stickiness (when deployed)
//	760 gpustack-lb-prefix             prefix affinity (enterprise edition)
//	740 this plugin                    reads the set -> scores load -> appends
//	                                   one ranking opinion
//	700 gpustack-lb (finisher)         L1-normalises, sums the weighted
//	                                   opinions, picks a candidate, writes the
//	                                   cluster header
//
// It **only states an opinion, it does not decide**: whenever this route has LB
// enabled and the candidates carry no weight, it scores every live candidate
// and the finisher combines all the entries.
//
// Priority within the capability band carries no meaning: the opinions are
// combined by weighted sum, so who writes first does not change the result. To
// change how much say this plugin has, change its `weight`. It is nonetheless
// the **floor** of the mechanism in the semantic sense -- it is the entry that
// always votes, and the built-in round-robin only applies when no capability
// plugin is installed at all.
//
// Three things it deliberately **does not** do (the same three as
// session-affinity), each corresponding to a silent failure mode:
//
//   - **Never calls ctx.DisableReroute().** That writes the request-level
//     property clear_route_cache=off, which takes effect across plugins, and
//     the finisher's x-higress-target-cluster write depends on the route being
//     re-evaluated.
//   - **Never reads shared data.** Load and penalty were already computed by
//     the publisher and come down with the candidate set (design §7.1).
//     Reading them again would return the same numbers plus N host calls.
//   - **Never touches the body at all**, so it never stops iteration.
//
// Design reference: the LB framework design doc §3.1, §4.2, §5, §7.1, §8.3.
package main

import (
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const pluginName = "gpustack-lb-least-load"

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		// ParseOverrideConfig rather than ParseConfig: matchRules need to
		// inherit enabled from defaultConfig, so that both "on globally, off
		// for a few routes" and "off globally, on for a few routes" can be
		// expressed.
		wrapper.ParseOverrideConfig(parseConfig, parseOverrideConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		// ProcessRequestBody is deliberately not registered: every input this
		// plugin needs is already in place in the header phase, and registering
		// it would only leave an opening for some future change to accidentally
		// stop iteration.
		//
		// WithRebuildAfterRequests is deliberately not set either: requests
		// inside the rebuild window get a 503, and this plugin sits on the LB
		// decision path. It holds no cross-request state, so there is no source
		// of memory growth.
	)
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.DontReadRequestBody()

	if !config.shouldEnable() {
		return types.ActionContinue
	}

	// No candidate set means LB is not enabled on this route (or this is the
	// fallback redirect pass, where the publisher deliberately publishes
	// nothing). Do nothing.
	set, ok := readCandidates()
	if !ok || len(set.Candidates) == 0 {
		return types.ActionContinue
	}

	// Weighted routes exit early (design §4.2): candidates carrying a weight
	// mean this route has a business split, and the finisher will ignore every
	// scoring opinion.
	if isWeighted(set.Candidates) {
		proxywasm.LogDebugf("%s: weighted route, skipping", pluginName)
		return types.ActionContinue
	}

	// **Do not skip on the grounds that somebody upstream already voted.**
	// Opinions are combined by weighted sum, so every entry contributes to the
	// total; skipping would simply drop the load dimension out of the vote.
	// That matters most in the case where the upstream entry falls away
	// entirely -- it may have scored only a subset of the set, and those
	// candidates may since have been ejected -- and this entry is what catches
	// the request.
	scores := loadScores(set.Candidates)
	appendRank(rankEntry{Name: pluginName, Weight: config.weight, Scores: scores})
	proxywasm.LogDebugf("%s: ranked %d candidates (weight %.2f)", pluginName, len(scores), config.weight)
	return types.ActionContinue
}
