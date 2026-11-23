// GPUStack-local file (no upstream counterpart): request-priority translation
// for gpustack/gpustack#6739.
//
// gpustack-ext-auth resolves the caller and, when the caller's key carries a
// priority, writes it as x-gpustack-request-priority: a non-negative integer,
// larger meaning more urgent, absent meaning "none configured". This file
// turns that into the scheduling input of the backend behind the active
// provider, which is decided per provider by `requestPriorityTarget`:
//
//	vllm   header x-vllm-priority = -p, body priority = -p
//	                                     (vLLM: lower value handled first. The
//	                                     generation endpoints read the header,
//	                                     the pooling ones the body only)
//	sglang body   priority         =  p  (SGLang: higher value scheduled first;
//	                                     body only -- it has no priority header)
//	unset   nothing
//
// Both backends see the same ordering for the same p; only the sign differs,
// because each backend's own direction differs. The translation is a sign,
// not a range mapping, so p = 0 lands on each backend's own baseline and
// GPustack never has to tell the plugin a max. SGLang must therefore NOT be
// started with --schedule-low-priority-values-first, which would flip its
// direction under this plugin's feet.
//
// Nothing here interprets the value: no clamping, no tiers, no defaults
// beyond 0. The source is trusted because ext-auth removes any client copy
// before this plugin runs and the transformer strips it earlier still; a
// malformed value is logged once and treated as 0 rather than failing the
// request, following the toolCallValidation rule that a config or data
// problem must never take the proxy down.

package provider

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
)

// RequestPriorityTarget names the backend family the active provider fronts,
// which is all the translation needs. Empty means the provider does not take
// a priority and the feature is off for it.
type RequestPriorityTarget string

const (
	RequestPriorityTargetNone   RequestPriorityTarget = ""
	RequestPriorityTargetVllm   RequestPriorityTarget = "vllm"
	RequestPriorityTargetSglang RequestPriorityTarget = "sglang"
)

// DefaultRequestPrioritySource is the header gpustack-ext-auth writes.
const DefaultRequestPrioritySource = "x-gpustack-request-priority"

// HeaderVllmPriority is vLLM's request-priority header. vLLM reads it ahead of
// the body field, so writing it unconditionally is also what neutralises a
// client-supplied body `priority`.
const HeaderVllmPriority = "x-vllm-priority"

// CtxKeyRequestPriority carries the parsed source value from the headers
// phase to the body phase (which writes the body member for either target)
// and to the retry path (which has to re-establish both forms).
const CtxKeyRequestPriority = "gpustack_request_priority"

// countTopLevelMember reports how many direct members of the top-level
// object are named key. Zero for anything that is not an object.
func countTopLevelMember(body []byte, key string) int {
	count := 0
	gjson.ParseBytes(body).ForEach(func(k, _ gjson.Result) bool {
		if k.String() == key {
			count++
		}
		return true
	})
	return count
}

// stripTopLevelMember returns body without every direct member named key,
// in one structural pass over the top-level object (values are copied as
// raw bytes, so nothing nested is re-encoded), or nil when there is no
// such member. Linear: removing duplicates one sjson.DeleteBytes at a time
// would be quadratic in client-controlled input.
func stripTopLevelMember(body []byte, key string) []byte {
	var out []byte
	removed := false
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		if k.String() == key {
			removed = true
			return true
		}
		if out == nil {
			out = make([]byte, 0, len(body))
			out = append(out, '{')
		} else {
			out = append(out, ',')
		}
		out = append(out, k.Raw...)
		out = append(out, ':')
		out = append(out, v.Raw...)
		return true
	})
	if !removed {
		return nil
	}
	return append(out, '}')
}

// requestPriorityBodyTarget decides whether a request to this API can carry
// a body `priority`. SGLang declares it on its OpenAI-compatible generation
// and pooling requests; vLLM's pooling endpoints read it from the body only
// (the header is read by the generation endpoints alone). The Anthropic
// endpoint has no such field, so a Claude request passed through natively
// is left alone rather than given a member the server ignores -- see the
// design note in designs/.
func requestPriorityBodyTarget(apiName ApiName) bool {
	switch apiName {
	case ApiNameChatCompletion, ApiNameCompletion, ApiNameResponses, ApiNameEmbeddings:
		return true
	}
	return false
}

var warnOnce sync.Map

func warnfOnce(key, format string, args ...interface{}) {
	if _, loaded := warnOnce.LoadOrStore(key, true); loaded {
		return
	}
	log.Warnf(format, args...)
}

// normalizeRequestPriorityTarget canonicalises the configured spelling. An
// unknown value is not a config error -- see the package comment -- but is
// reported so the caller can log it; it then behaves as unset.
func normalizeRequestPriorityTarget(raw string) (RequestPriorityTarget, string) {
	canonical := RequestPriorityTarget(strings.ToLower(strings.TrimSpace(raw)))
	switch canonical {
	case RequestPriorityTargetNone, RequestPriorityTargetVllm, RequestPriorityTargetSglang:
		return canonical, ""
	}
	return RequestPriorityTargetNone, fmt.Sprintf(
		"invalid requestPriorityTarget %q, expected one of: vllm, sglang; request priority is disabled for this provider", raw)
}

// requestPriorityMax bounds the value to what the backends accept as a 32-bit
// scheduling value (vLLM validates its header against exactly this range and
// rejects the request otherwise). ext-auth bounds the table the same way;
// this is the consumer-side copy of that rule, because the server-authorised
// path relays the header without it.
const requestPriorityMax = int64(1<<31 - 1)

// parseRequestPriority reads the source header. Absent is 0. A value that is
// not a non-negative 32-bit integer is 0 plus one warning -- one per VM, not
// per distinct bad value: the key is client-influenced here, and a per-value
// key would let a client grow the warnOnce map without bound.
func parseRequestPriority(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	p, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || p < 0 || p > requestPriorityMax {
		warnfOnce("request_priority_invalid", "[gpustack-ai-proxy] request priority %q is not a non-negative 32-bit integer; using 0 (warned once, later bad values are silent)", raw)
		return 0
	}
	return p
}

// backendPriority is the value the target backend expects for GPustack
// priority p. vLLM schedules the lower value first, SGLang the higher one.
func backendPriority(target RequestPriorityTarget, p int64) int64 {
	if target == RequestPriorityTargetVllm {
		return -p
	}
	return p
}

// ApplyRequestPriorityHeaders runs once per pass in the request-headers
// phase (main.go), ahead of the provider's own header rewrite.
//
// The source header is always removed: it is this gateway's internal
// protocol and must not reach a model worker or a third-party provider,
// whether or not the target consumes it. For vllm the header is written
// unconditionally -- on 0 as well -- because a written header is what makes
// vLLM's generation endpoints ignore a client-supplied body `priority`; the
// body phase additionally overwrites that body member for both targets.
//
// The parsed value goes into the ctx for every target that consumes it, so
// that the retry path (provider/retry.go), which rebuilds headers from
// scratch and replays the client's original body, can re-establish the
// translation without re-reading a header it already removed.
func ApplyRequestPriorityHeaders(ctx wrapper.HttpContext, source string, target RequestPriorityTarget) {
	raw, _ := proxywasm.GetHttpRequestHeader(source)
	_ = proxywasm.RemoveHttpRequestHeader(source)
	p := parseRequestPriority(raw)
	switch target {
	case RequestPriorityTargetVllm:
		_ = proxywasm.ReplaceHttpRequestHeader(HeaderVllmPriority, strconv.FormatInt(backendPriority(target, p), 10))
		ctx.SetContext(CtxKeyRequestPriority, p)
	case RequestPriorityTargetSglang:
		ctx.SetContext(CtxKeyRequestPriority, p)
	}
}

// applyRequestPriorityRetry re-establishes the translation on a retried
// request: sendRetryRequest builds the header set afresh (losing
// x-vllm-priority) and replays the body saved before the body phase (so a
// client-supplied `priority` is back). Must run after the retry's
// own header/body transform so nothing rebuilt afterwards can drop it.
func (c *ProviderConfig) applyRequestPriorityRetry(ctx wrapper.HttpContext, headers http.Header, body []byte) (http.Header, []byte) {
	p, ok := requestPriorityFromContext(ctx)
	if !ok {
		return headers, body
	}
	return c.requestPriorityRetry(p, headers, body)
}

// requestPriorityRetry is the pure half of applyRequestPriorityRetry.
// The retry is a JSON chat completion by construction (see retry.go), so
// only the target gate applies here.
func (c *ProviderConfig) requestPriorityRetry(p int64, headers http.Header, body []byte) (http.Header, []byte) {
	switch c.requestPriorityTarget {
	case RequestPriorityTargetVllm:
		headers.Set(HeaderVllmPriority, strconv.FormatInt(backendPriority(RequestPriorityTargetVllm, p), 10))
		body = priorityBody(c.requestPriorityTarget, body, p)
	case RequestPriorityTargetSglang:
		body = priorityBody(c.requestPriorityTarget, body, p)
	}
	return headers, body
}

// requestPriorityBodyContentType reports whether a request body of this
// content type can carry a JSON member. An absent type is taken as JSON: the
// endpoints in requestPriorityBodyTarget have no other body format, and
// priorityBody leaves a non-JSON body untouched anyway. Case-insensitive and
// suffixed types included, as media types are.
func requestPriorityBodyContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return contentType == "" || strings.Contains(contentType, "json")
}

// priorityBody writes the backend's `priority` body member with the value
// backendPriority gives for target, overwriting any client-supplied one.
// Pure: the caller owns the host calls.
//
// Both targets write it. SGLang has no priority header. vLLM reads the
// header on its generation endpoints but only the body on its pooling ones
// (`/v1/embeddings` etc.), so for vLLM the body member is what carries the
// priority there, and what neutralises a client-supplied value everywhere
// else; the two never disagree because both derive from the same p.
//
// Every existing `priority` member is removed first, not just rewritten:
// sjson touches one occurrence, so a body carrying `{"priority":0,
// "priority":999}` would keep the second, client-chosen copy for a
// last-wins decoder (Python, the backend's own). With more than one copy the
// object is rebuilt in one pass (stripTopLevelMember) rather than deleting
// one at a time, which would be quadratic in client-controlled input; a
// single copy is replaced in place.
//
// The object check is load-bearing: sjson.SetBytes does not fail on a
// non-JSON input, it silently *builds* `{"priority":N}` around the value and
// discards the original bytes -- so without this gate a non-JSON body that
// slipped past the content-type check would be replaced wholesale.
func priorityBody(target RequestPriorityTarget, body []byte, p int64) []byte {
	if !gjson.ParseBytes(body).IsObject() {
		return body
	}
	if countTopLevelMember(body, "priority") > 1 {
		if stripped := stripTopLevelMember(body, "priority"); stripped != nil {
			body = stripped
		}
	}
	injected, err := sjson.SetBytes(body, "priority", backendPriority(target, p))
	if err != nil {
		// Defensive only: an object that parsed above always takes the member.
		return body
	}
	return injected
}

// requestPriorityBody is the body-phase half for either target: every gate
// that says "this body carries no priority" returns the input unchanged, so
// the caller can compare slices to decide whether anything was done.
//
// The value comes from the ctx the headers phase filled, so a request that
// never reached ApplyRequestPriorityHeaders (no active provider then, or a
// different one) is left alone rather than given a default.
func (c *ProviderConfig) requestPriorityBody(ctx wrapper.HttpContext, apiName ApiName, body []byte) []byte {
	if c.requestPriorityTarget == RequestPriorityTargetNone || !requestPriorityBodyTarget(apiName) {
		return body
	}
	contentType, _ := proxywasm.GetHttpRequestHeader(util.HeaderContentType)
	if !requestPriorityBodyContentType(contentType) {
		return body
	}
	p, ok := requestPriorityFromContext(ctx)
	if !ok {
		return body
	}
	return priorityBody(c.requestPriorityTarget, body, p)
}

// requestPriorityFromContext reads what the headers phase stored. Absent
// means this provider takes no priority (or the headers phase did not run),
// in which case there is nothing to inject.
func requestPriorityFromContext(ctx wrapper.HttpContext) (int64, bool) {
	v, ok := ctx.GetContext(CtxKeyRequestPriority).(int64)
	return v, ok
}
