package main

import (
	"encoding/json"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

// reject answers with an explanatory response of our own when the candidate
// set is empty.
//
// These three rules come from gpustack-rate-limit's hard-won experience: under
// Higress's AI route filter chain, SendHttpResponseWithDetail returns nil (the
// host accepted it) yet fails to flush to the downstream client, showing up as
// response_code=503 + bytes_sent=0 + response_flags=DC.
//
//  1. Call DisableReroute() **before** rejecting. This is the **only** place
//     it may be called -- that path never wrote a cluster header, and without
//     it the response does not flush on the stop-iteration path.
//  2. Return ActionPause (== HeaderStopIteration) after
//     SendHttpResponseWithDetail. Not ActionContinue (the filter chain keeps
//     iterating and the body is dropped), and not
//     HeaderStopAllIterationAndWatermark (it sits under watermark waiting for
//     a resume that never comes).
//  3. **The body must be non-empty**, and valid JSON for streaming requests.
//     Envoy's local-reply path treats an empty body as a degenerate response:
//     it queues it but never flushes.
//
// Why Envoy's own error is not good enough: measurements on this route show
// that the 503 Envoy synthesises when a cluster is missing also fails to reach
// the client -- the client hangs until its own timeout. Deleting the cluster
// header is only a cleanup step; real fail-closed behaviour has to be this
// function sending a response.
func reject(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.DisableReroute()

	body := config.rejectBody
	if err := proxywasm.SendHttpResponseWithDetail(
		uint32(config.rejectStatus),
		pluginName+".no_candidate",
		[][2]string{{"content-type", "application/json"}},
		body,
		-1,
	); err != nil {
		// Reaching here means even the host refused it. Nothing more can be
		// done, but leave a log line -- note it comes **after**
		// SendHttpResponse, not before: extra hostcalls between callback entry
		// and SendHttpResponse can confuse Envoy's local-reply path (also
		// learned from rate-limit).
		proxywasm.LogErrorf("%s: SendHttpResponseWithDetail failed: %v", pluginName, err)
	}
	return types.ActionPause
}

// rejectCap answers the client when every candidate is at or over its
// maxRunningRequests.
//
// Same delivery rules as reject (they were learned on the same filter chain:
// DisableReroute first, ActionPause after, non-empty JSON body). The one
// difference is the **response**: this is a rate-limit verdict, not a health
// one, and reporting it as "no healthy model instance available" (the 503) is
// actively misleading -- clients cannot tell "back off" from "this deployment
// is broken", which is exactly how gpustack/gpustack#6309 read in the field.
// Defaults to 429 with an explicit message; both configurable next to the
// no-candidate pair.
//
// On the async (redis) path this runs from the store's reserve callback,
// where the request is paused under watermark; the SendHttpResponse below
// terminates the stream, and the store deliberately does not resume (the
// rate-limit pattern -- resuming would race the local-reply queueing).
func rejectCap(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.DisableReroute()

	if err := proxywasm.SendHttpResponseWithDetail(
		uint32(config.capRejectStatus),
		pluginName+".max_inflight",
		[][2]string{{"content-type", "application/json"}},
		config.capRejectBody,
		-1,
	); err != nil {
		proxywasm.LogErrorf("%s: SendHttpResponseWithDetail failed: %v", pluginName, err)
	}
	return types.ActionPause
}

// buildRejectBody wraps the message in an OpenAI-style error envelope.
//
// JSON is the right contract for AI clients; and on a streaming request the
// downstream token-usage plugin processes the response body chunk by chunk,
// where a non-JSON rejection body has historically been swallowed as an
// incomplete SSE fragment. If the message is already a JSON object it is
// passed through verbatim.
func buildRejectBody(message, errType string, status int64) []byte {
	trimmed := strings.TrimSpace(message)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// An empty body is not allowed (rule 3 above), so fall back to the
		// smallest valid JSON.
		return []byte(`{"error":{"message":"no healthy model instance available"}}`)
	}
	return data
}
