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
	var counter proxywasm.MetricCounter
	if v, ok := metricCounters.Load(stat); ok {
		counter = v.(proxywasm.MetricCounter)
	} else {
		counter = proxywasm.DefineCounterMetric(stat)
		metricCounters.Store(stat, counter)
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

// emitToolCallPairingRejected records one rejection. The label values are read
// here rather than passed down because this runs at most once per request, on
// an error path -- the hostcall cost is irrelevant and keeping the happy path
// free of them matters more.
func emitToolCallPairingRejected(model string, rule toolPairingRule) {
	consumer, _ := proxywasm.GetHttpRequestHeader(headerConsumer)
	incrCounter(aiStatName(
		readEnvoyProperty("route_name"),
		readEnvoyProperty("cluster_name"),
		model,
		consumer,
		metricNameToolCallPairingRejected,
		[2]string{"rule", string(rule)},
	))
}
