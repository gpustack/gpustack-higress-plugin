// gpustack-lb is the **single binary** of the LB plugin framework; the `mode`
// setting decides which role an instance plays. Both roles are deployed (one
// WasmPlugin CR each, same url):
//
//	mode: context   AUTHN/795  read config -> filter -> publish the candidate
//	                           set into filter state. On non-LB routes it
//	                           degrades to model-mapper's existing behaviour.
//	  ↓ capability band (session-affinity 780 / prefix 760 / least-load 740)
//	    each appends a scored opinion
//	mode: finisher  AUTHN/700  weighted sum -> pick a candidate -> write the
//	                           cluster header -> buffer the body to rewrite the
//	                           model name -> book-keeping in onStreamDone
//
// **Two CRs are structurally necessary**: capability plugins have to run
// between "publish the candidates" and "make the decision", and a single
// filter instance cannot be at both points in the chain. The binary, though,
// only needs to be one -- merging removed two hand-maintained copies of the
// wire contract and 112 duplicated lines of multipart walking.
//
// When deployed, the context CR keeps the name `gpustack-model-mapper` and the
// same config location, so it can replace that plugin in place.
//
// Design reference: the LB framework design doc §2 / §4 / §6 / §7.
package main

import (
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const pluginName = "gpustack-lb"

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		// ParseOverrideConfig rather than ParseConfig: matchRules need to
		// inherit the **deployment-level** knobs from defaultConfig (mode,
		// health windows, reject body, body params). Topology is not
		// inherited -- see parseOverrideConfig for the reasoning.
		wrapper.ParseOverrideConfig(parseConfig, parseOverrideConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessStreamDone(onHttpStreamDone),
		// WithRebuildAfterRequests is deliberately not set: requests inside the
		// rebuild window get a 503, and both roles on this chain sit on the LB
		// decision path. The other reason is that the round-robin cursor lives
		// in per-VM memory, so a rebuild would reset it.
	)
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	if config.mode == modeFinisher {
		return finisherOnHeaders(ctx, config)
	}
	return contextOnHeaders(ctx, config)
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if config.mode == modeFinisher {
		return finisherOnBody(ctx, config, body)
	}
	return contextOnBody(ctx, config, body)
}

func onHttpStreamDone(ctx wrapper.HttpContext, config Config) {
	// Only the finisher holds the selection result, and only it writes shared
	// state.
	if config.mode == modeFinisher {
		finisherOnStreamDone(ctx, config)
	}
}
