// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Minimal Envoy counter plumbing, currently used only by the tool/tool_calls
// pairing check. See gpustack/gpustack#6210.

package main

import (
	"regexp"
	"strings"
	"sync"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

const (
	// metricNameToolCallPairingRejected counts requests rejected by the
	// structural tool/tool_calls pairing check. There is no separate "detected
	// but forwarded" counter because there is no such mode: toolCallValidation
	// is off or strict, so every violation the walk sees becomes a 400.
	metricNameToolCallPairingRejected = "gpustack_ai_proxy_tool_call_pairing_rejected_total"

	// metricNoneLabel keeps a stat name well-formed when a label value is
	// unavailable (no route name on a direct-response path, no consumer on an
	// unauthenticated route, ...).
	metricNoneLabel = "none"

	// headerConsumer is the Higress convention for the authenticated caller
	// identity, written by key-auth / jwt-auth. It is what lets an operator
	// attribute a malformed request back to an API key.
	headerConsumer = "x-mse-consumer"
)

// metricCounters caches the proxywasm.MetricCounter handle per stat name.
// Process-global by design: counter handles are owned by the host and outlive
// any single HttpContext; the map re-populates on demand after a VM rebuild.
var metricCounters sync.Map // map[string]proxywasm.MetricCounter

// metricLabelSanitizeRE matches any byte that would break Envoy stat-name
// parsing. '.' is deliberately kept: Higress's bootstrap stats_tags regexes
// backtrack correctly across dot-containing values such as `qwen3-0.6b`, and
// Envoy's Prometheus formatter turns any remaining '.' into '_' at exposition
// time anyway.
var metricLabelSanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func sanitizeMetricLabel(s string) string {
	if s == "" {
		return metricNoneLabel
	}
	return metricLabelSanitizeRE.ReplaceAllString(s, "_")
}

// aiStatName assembles a stat name in the Higress AI-plugin convention:
//
//	route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.<name>.<extraK>.<extraV>...
//
// The leading four slots align with the stat-tag extractors Higress already
// ships in its Envoy bootstrap, so `ai_route` and `ai_cluster` become real
// Prometheus labels with no extra deployment config. `ai_model` is not
// auto-extracted (Higress's regex requires an input_token/output_token
// terminal), and neither are our extras -- those stay in the stat name until
// the operator adds metric_relabel_configs. Do not reorder the leading slots.
func aiStatName(route, cluster, model, consumer, name string, extras ...[2]string) string {
	var sb strings.Builder
	sb.Grow(120 + len(extras)*20)
	sb.WriteString("route.")
	sb.WriteString(sanitizeMetricLabel(route))
	sb.WriteString(".upstream.")
	sb.WriteString(sanitizeMetricLabel(cluster))
	sb.WriteString(".model.")
	sb.WriteString(sanitizeMetricLabel(model))
	sb.WriteString(".consumer.")
	sb.WriteString(sanitizeMetricLabel(consumer))
	sb.WriteString(".metric.")
	sb.WriteString(sanitizeMetricLabel(name))
	for _, kv := range extras {
		sb.WriteByte('.')
		sb.WriteString(kv[0])
		sb.WriteByte('.')
		sb.WriteString(sanitizeMetricLabel(kv[1]))
	}
	return sb.String()
}

// incrCounter increments a named counter by 1, lazily defining it on first use.
func incrCounter(stat string) {
	if v, ok := metricCounters.Load(stat); ok {
		v.(proxywasm.MetricCounter).Increment(1)
		return
	}
	counter := proxywasm.DefineCounterMetric(stat)
	if actual, loaded := metricCounters.LoadOrStore(stat, counter); loaded {
		counter = actual.(proxywasm.MetricCounter)
	}
	counter.Increment(1)
}

// readEnvoyProperty reads a string Envoy property, returning "" on a miss.
func readEnvoyProperty(name string) string {
	raw, err := proxywasm.GetProperty([]string{name})
	if err != nil || len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// emitToolCallPairingRejected records one rejection.
//
// **Every label here is bounded by configuration, never by the request.**
// `route_name` and `cluster_name` come from the Envoy route table, and `rule`
// has six possible values; the resulting stat count is routes x clusters x 6.
// The request's `model` (a body field) and `x-mse-consumer` (a header, which
// nothing stops a caller from setting on a route without key-auth in front)
// are deliberately NOT labels: a rejected request is cheap to send -- 400, no
// upstream call -- so a caller that could pick a label value would mint a new
// Envoy stat and a permanent metricCounters entry per request and grow both
// without bound. They go on the WARN log line instead, which is append-only
// and still gives an operator the API-key attribution the issue asked for.
//
// The model / consumer slots still exist in the stat name because Higress's
// pre-shipped stats_tags regexes match on the literal `.model.` and
// `.consumer.` separators to extract ai_route / ai_cluster; dropping the slots
// would break that extraction. They carry the fixed sentinel.
//
// The property reads happen here, on the rejection path, rather than being
// captured per-request up front: rejections are rare and the check is off by
// default, so making every request pay for labels it will never emit would be
// the wrong trade.
func emitToolCallPairingRejected(rule toolPairingRule) {
	incrCounter(aiStatName(
		readEnvoyProperty("route_name"),
		readEnvoyProperty("cluster_name"),
		metricNoneLabel,
		metricNoneLabel,
		metricNameToolCallPairingRejected,
		[2]string{"rule", string(rule)},
	))
}
