package main

import (
	"regexp"
	"strings"
	"sync"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// Metric names emitted on the Envoy /stats endpoint. Documented in README.md.
const (
	metricNameRequestTotal  = "gpustack_rate_limit_request_total"
	metricNameRejectedTotal = "gpustack_rate_limit_rejected_total"
	metricNameValueTotal    = "gpustack_rate_limit_value_total"

	// Per-quota-kind label values used by rejected_total / value_total.
	metricKindQuery         = "query"
	metricKindTokenRolling  = "token_rolling"
	metricKindTokenCalendar = "token_calendar"

	// result label values for request_total.
	metricResultPassed  = "passed"
	metricResultLimited = "limited"

	// Sentinel used when route_name is unset on a local route, etc.
	metricNoneLabel = "none"
)

// metricCounters caches the proxywasm.MetricCounter handle per stat name to
// avoid going through DefineCounterMetric on every increment. The map is
// process-global by design: counter handles are owned by the host and survive
// across HttpContext lifetimes; the map naturally re-populates on demand
// after a VM rebuild.
var metricCounters sync.Map // map[string]proxywasm.MetricCounter

// metricLabelSanitizeRE matches any byte that would break Envoy stat-name
// parsing or downstream Prometheus label extraction. We deliberately keep
// '.' in label values: Higress's bootstrap stats_tags extract the four AI
// slots (route / upstream / model / consumer) and their regex
// (e.g. `((.*?)\.)upstream`) backtracks correctly across dot-containing
// values like `qwen3-0.6b` or `ai-route-route-1.internal`, preserving the
// dot in the resulting Prometheus label value -- matching ai-statistics's
// observed behaviour. For slots that are NOT auto-extracted (rule / combo
// / kind / bucket / result), the dot is harmless: Envoy's Prometheus
// formatter converts every remaining '.' in the metric name to '_' before
// exposition, so bucket="qwen3-0.6b" and bucket="qwen3-0_6b" are
// indistinguishable in the scrape output.
var metricLabelSanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// sanitizeMetricLabel normalises a label value into characters acceptable in
// an Envoy stat name (alphanumerics plus '_-'). The empty string becomes
// "none" so the resulting stat name is always well-formed.
func sanitizeMetricLabel(s string) string {
	if s == "" {
		return metricNoneLabel
	}
	return metricLabelSanitizeRE.ReplaceAllString(s, "_")
}

// formatStatName assembles a stat name in the Higress AI-plugin convention:
//
//	route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.<metricName>.<extraKey>.<extraValue>...
//
// The first four label slots (route/upstream/model/consumer) align with the
// stat-tag extractors that Higress already ships in its envoy bootstrap
// (see istio/tools/packaging/common/envoy_bootstrap.json). With no extra
// deployment configuration:
//
//   - "ai_route" is auto-extracted by the regex
//     ^wasmcustom\.route\.((.*?)\.)upstream
//   - "ai_cluster" is auto-extracted by the regex
//     ^wasmcustom\..*?\.upstream\.((.*?)\.)model
//   - "ai_model" requires a metric-name suffix of input_token / output_token
//     to match Higress's regex, which our metrics don't have, so it is NOT
//     auto-extracted; the model.<value>. segment remains in the stat name.
//   - consumer / our extras (rule, combo, kind, bucket, result) have no
//     built-in extractor and remain in the stat name unless the operator
//     applies metric_relabel_configs at the Prometheus side (or, longer-term,
//     adds an EnvoyFilter to extend stats_tags).
//
// Aligning with the existing Higress slot order is the lowest-friction way
// to surface route + cluster as proper Prometheus labels right out of the
// box; the README documents how to extract the rest with metric_relabel.
//
// The "ai" prefix tags (ai_route, ai_cluster) come for free, but for that to
// happen the prefix MUST be exactly "route.<v>.upstream.<v>." -- do not
// reorder these four slots without also updating the bootstrap regex.
//
// Label values are sanitised inline. Callers must pass the four AI slots
// (route, cluster, model, consumer) as the leading labels, followed by
// metric.<metricName>, followed by any extra labels (rule, combo, kind,
// bucket, result).
func formatStatName(labels ...[2]string) string {
	var sb strings.Builder
	sb.Grow(len(labels) * 16)
	for i, kv := range labels {
		if i > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(kv[0])
		sb.WriteByte('.')
		sb.WriteString(sanitizeMetricLabel(kv[1]))
	}
	return sb.String()
}

// incrCounter increments a named counter by `by`, lazily defining the metric
// on first use. by == 0 is a no-op.
func incrCounter(stat string, by uint64) {
	if by == 0 {
		return
	}
	var counter proxywasm.MetricCounter
	if v, ok := metricCounters.Load(stat); ok {
		counter = v.(proxywasm.MetricCounter)
	} else {
		counter = proxywasm.DefineCounterMetric(stat)
		metricCounters.Store(stat, counter)
	}
	counter.Increment(by)
}

// aiLabels groups the four AI-routing label values that prefix every metric
// emitted by this plugin. They populate the route / upstream / model /
// consumer slots that align with Higress's pre-shipped stats_tags regex
// extractors -- see formatStatName for the exact stat-name layout and which
// of these become Prometheus labels by default.
type aiLabels struct {
	Route    string
	Cluster  string
	Model    string
	Consumer string
}

// statName builds the full Envoy stat name for a single metric emission.
// The result is `route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.<metricName>.<extra>...`,
// the slot ordering required by the Higress bootstrap stats_tags regex
// extractors documented on formatStatName -- reordering these five
// leading slots would silently break ai_route / ai_cluster / ai_model /
// ai_consumer auto-extraction.
//
// We append directly into a strings.Builder rather than going through
// formatStatName(labels...). This is the metric-emission hot path
// (called 1-N times per passed request, once on every rejection) and
// the previous implementation allocated an intermediate
// `[][2]string` of 5+len(extras) on every call. The builder still
// allocates its byte buffer, but that allocation is unavoidable and
// can be sized once with Grow.
func (a aiLabels) statName(metricName string, extras ...[2]string) string {
	var sb strings.Builder
	// Rough capacity estimate: each AI label segment averages ~16 bytes
	// (key + dot + sanitised value + dot), the metric name ~40 bytes,
	// each extra ~16 bytes. Overshooting is cheap; reallocating mid-
	// build is what we want to avoid.
	sb.Grow(120 + len(extras)*20)

	sb.WriteString("route.")
	sb.WriteString(sanitizeMetricLabel(a.Route))
	sb.WriteString(".upstream.")
	sb.WriteString(sanitizeMetricLabel(a.Cluster))
	sb.WriteString(".model.")
	sb.WriteString(sanitizeMetricLabel(a.Model))
	sb.WriteString(".consumer.")
	sb.WriteString(sanitizeMetricLabel(a.Consumer))
	sb.WriteString(".metric.")
	sb.WriteString(sanitizeMetricLabel(metricName))

	for _, kv := range extras {
		sb.WriteByte('.')
		sb.WriteString(kv[0])
		sb.WriteByte('.')
		sb.WriteString(sanitizeMetricLabel(kv[1]))
	}

	return sb.String()
}

// emitRequestOutcome increments request_total once per request, regardless
// of how many combinations the request matched. The ai_route / ai_cluster
// labels come for free via Higress's bootstrap stats_tags; rule / result
// are embedded in the stat name and require operator-side label
// extraction (see README "Metrics").
func emitRequestOutcome(ai aiLabels, rule, result string) {
	incrCounter(ai.statName(metricNameRequestTotal,
		[2]string{"rule", rule},
		[2]string{"result", result},
	), 1)
}

// emitRejected increments rejected_total for the (single) combination that
// tripped the lua/local short-circuit. Sum across all (combo, kind, bucket)
// equals request_total{result=limited}.
func emitRejected(ai aiLabels, rule, combo, kind, bucket string) {
	incrCounter(ai.statName(metricNameRejectedTotal,
		[2]string{"rule", rule},
		[2]string{"combo", combo},
		[2]string{"kind", kind},
		[2]string{"bucket", bucket},
	), 1)
}

// emitValue increments value_total by the amount that was just written into
// the bucket -- 1 for query (per passed request), or total_tokens for
// token_rolling / token_calendar (at stream done). count == 0 is a no-op.
func emitValue(ai aiLabels, rule, combo, kind, bucket string, count uint64) {
	incrCounter(ai.statName(metricNameValueTotal,
		[2]string{"rule", rule},
		[2]string{"combo", combo},
		[2]string{"kind", kind},
		[2]string{"bucket", bucket},
	), count)
}
