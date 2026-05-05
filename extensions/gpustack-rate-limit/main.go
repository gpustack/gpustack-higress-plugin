package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	pluginName          = "gpustack-rate-limit"
	defaultRejectedCode = 429
	// Default response body when RejectedMsg is unset. A non-empty body is
	// required because Envoy's local-reply path treats an empty body as a
	// degenerate response in some filter-chain configurations (the reply is
	// queued but never flushed to the client).
	defaultRejectedMsg = "Too many requests"
	luaSuccessReply    = "SUCCESS"

	// Lua script mode identifiers.
	modeCheckAndAdd = "check_and_add"
	modeCheck       = "check"
	modeAdd         = "add"

	// ttlGraceSeconds is the safety margin added to calendar-aligned quota
	// TTLs to absorb clock skew across Higress instances. Modest by design --
	// the lua ZREMRANGEBYSCORE in pass 1 cleans up any leftover data on the
	// next request anyway, so over-shooting the TTL is harmless.
	ttlGraceSeconds = 300

	// Internal HttpContext keys.
	ctxKeyRequestID    = "gpustack-rate-limit:request-id"
	ctxKeyTokenPending = "gpustack-rate-limit:token-pending"
	ctxKeyAILabels     = "gpustack-rate-limit:ai-labels"

	// Header conventions used to populate aiLabels.Model / aiLabels.Consumer.
	// x-higress-llm-model is injected by gpustack-set-header-pre-route (or
	// upstream Higress AI route logic) before our request-headers phase runs;
	// x-mse-consumer is the standard Higress consumer-identity header.
	headerLLMModel = "x-higress-llm-model"
	headerConsumer = "x-mse-consumer"
)

//go:embed multi_level_limit.lua
var multiLevelLimitScript string

// Default path-filter lists, applied when the config source does not contain
// the corresponding field. The defaults target common AI inference endpoints
// so the plugin is a no-op for health checks, doc pages, static assets, etc.
var (
	defaultEnableOnPathSuffix = []string{
		"/completions",
		"/embeddings",
		"/images/generations",
		"/audio/speech",
		"/fine_tuning/jobs",
		"/moderations",
		"/image-synthesis",
		"/video-synthesis",
		"/rerank",
		"/messages",
		"/responses",
	}
	defaultEnableOnPathPrefix = []string{
		"/model/proxy",
	}
)

// tokenPendingAdd carries the info needed to perform the response-phase
// "add" for a TokenLimits or TokenQuota combination.
//
// Exactly one of FixedWindow / CalendarSpec is set:
//   - FixedWindow: rolling RateQuota (TokenLimits). Window is fixed for the
//     life of the request.
//   - CalendarSpec: calendar-aligned QuotaSpec (TokenQuota). Window is
//     recomputed at response phase against the response-time clock so a
//     stream that crosses the period boundary writes its tokens into the
//     period it ends in (consistent with onHttpStreamDone semantics).
type tokenPendingAdd struct {
	Key          string
	Quota        int
	FixedWindow  int64
	CalendarSpec *QuotaSpec

	// Metric labels (mirror the corresponding LimitEntry produced by
	// collectChecks, so onHttpStreamDone can emit value_total without
	// looking the matching entry up again).
	Combo  string
	Kind   string
	Bucket string
}

// windowSeconds returns the lua window argument for the response-phase add.
func (p *tokenPendingAdd) windowSeconds(now time.Time) int64 {
	if p.CalendarSpec != nil {
		w, _ := p.CalendarSpec.GetWindowAndQuota(now)
		return w
	}
	return p.FixedWindow
}

// ttlSeconds returns the Redis EXPIRE TTL the lua script should set for this
// combo, recomputed against `now` so calendar-aligned combos shrink toward
// the period end as the period progresses.
func (p *tokenPendingAdd) ttlSeconds(now time.Time) int64 {
	if p.CalendarSpec != nil {
		return calendarTTL(p.CalendarSpec, now)
	}
	return rollingTTL(p.FixedWindow)
}

// rollingTTL is the TTL for a rolling-window combo. 2x the window matches the
// historical behaviour: any data older than 2*window since the last write is
// guaranteed to be outside the next read's window.
func rollingTTL(window int64) int64 {
	return window * 2
}

// calendarTTL is the TTL for a calendar-aligned combo at the given moment.
// Equal to the time remaining in the current period plus a small grace, so
// the key lives at least until the period boundary regardless of how early
// in the period the data was written. Without this, the dynamic window value
// (small at period start) would produce a near-zero TTL and the key would be
// evicted before the next request arrived, prematurely resetting the quota.
func calendarTTL(spec *QuotaSpec, now time.Time) int64 {
	end := spec.periodEnd(now)
	ttl := end.Unix() - now.Unix() + ttlGraceSeconds
	if ttl < 1 {
		ttl = 1
	}
	return ttl
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		// Two-level configuration: rule-level inherits global and may override specific fields.
		wrapper.ParseOverrideConfig(parseGlobalConfig, parseOverrideRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessStreamDone(onHttpStreamDone),
		// Periodically rebuild the VM to avoid long-running memory-leak accumulation.
		wrapper.WithRebuildAfterRequests[PluginConfig](1000),
		wrapper.WithRebuildMaxMemBytes[PluginConfig](200 * 1024 * 1024),
	)
}

// parseGlobalConfig parses the plugin-wide JSON configuration.
//
// Redis is initialized unconditionally here: it is the base dependency inherited
// by every rule-level configuration. LimitCombinations is optional at the
// global level -- when the global config only supplies base fields (Redis,
// rejected_code, etc.) and the actual rate-limit rules live in per-route
// overrides, LimitCombinations may be empty and full validation is skipped.
// Rule-level parsing always runs Validate so mistakes surface there.
func parseGlobalConfig(raw gjson.Result, config *PluginConfig) error {
	if err := json.Unmarshal([]byte(raw.Raw), config); err != nil {
		return fmt.Errorf("unmarshal plugin config: %w", err)
	}
	applyPathFilterDefaults(raw, config)
	// gjson treats an explicit `redis: null` as Exists()==true (Raw="null"),
	// so we must additionally require an object before delegating to
	// initRedisBackend; otherwise a `redis: null` literal in defaultConfig
	// fails parseGlobalConfig and leaves m.globalConfig as the zero value,
	// which then propagates empty path filters into every matchRule override.
	if redisRaw := raw.Get("redis"); redisRaw.IsObject() {
		backend, err := initRedisBackend(raw)
		if err != nil {
			return err
		}
		config.Backend = backend
	} else {
		proxywasm.LogInfof("%s: no redis configured, using local (per-instance) rate limiting mode", pluginName)
		config.Backend = &localBackend{}
	}
	if len(config.LimitCombinations) == 0 {
		return nil
	}
	return config.Validate()
}

// applyPathFilterDefaults fills in the default enable-on-path lists when the
// caller did not specify them. An explicitly provided list (even empty) is
// preserved, so users can opt out of either filter by writing an empty array.
func applyPathFilterDefaults(raw gjson.Result, config *PluginConfig) {
	if !raw.Get("enable_on_path_suffix").Exists() {
		config.EnableOnPathSuffix = append([]string(nil), defaultEnableOnPathSuffix...)
	}
	if !raw.Get("enable_on_path_prefix").Exists() {
		config.EnableOnPathPrefix = append([]string(nil), defaultEnableOnPathPrefix...)
	}
}

// parseOverrideRuleConfig parses a rule-level (route/host/service) JSON configuration.
//
// Inheritance & aggregation:
//   - Scalar / pointer fields (rule_name, timezone, rejected_code, redis ...)
//     follow the classic "inherit from global, override if redeclared" pattern.
//   - enable_on_path_suffix / enable_on_path_prefix follow the same per-field
//     replace-or-inherit pattern.
//   - limit_combinations is AGGREGATED: rule-level combos are appended onto
//     global combos. Every request matched by this rule applies BOTH the
//     deployment-wide scope (declared at defaultConfig) AND the route-specific
//     scope (declared at matchRule), checked atomically in a single Redis Eval.
//     Validate enforces that combo names are unique across the merged list, so
//     a route cannot accidentally duplicate or shadow a global combo.
//
// Aliasing safety: global is logically read-only here (the same value is
// passed to every matchRule's parser). json.Unmarshal of a slice field
// resets the existing slice and reuses its backing array, which would
// corrupt global through aliasing if we didn't detach the slices first.
func parseOverrideRuleConfig(raw gjson.Result, global PluginConfig, config *PluginConfig) error {
	*config = global

	// Detach slice fields from global's backing arrays before json.Unmarshal
	// so the override decode cannot mutate global.
	config.LimitCombinations = nil
	config.EnableOnPathSuffix = nil
	config.EnableOnPathPrefix = nil

	if err := json.Unmarshal([]byte(raw.Raw), config); err != nil {
		return fmt.Errorf("unmarshal rule config: %w", err)
	}

	// Restore inherited path filter lists when the override did not redeclare
	// them. (For path filters we want classic "replace if present, inherit if
	// absent" behaviour, not aggregation -- a route's path scope is its own.)
	if !raw.Get("enable_on_path_suffix").Exists() {
		config.EnableOnPathSuffix = append([]string(nil), global.EnableOnPathSuffix...)
	}
	if !raw.Get("enable_on_path_prefix").Exists() {
		config.EnableOnPathPrefix = append([]string(nil), global.EnableOnPathPrefix...)
	}

	// Aggregate limit_combinations: prepend global's combos, append rule-level.
	// Order matters for the lua check pass -- the first over-quota combo
	// short-circuits with LIMITED, so global scopes are checked before
	// route-specific ones (typically the wider bucket fails first if both are
	// going to fail, which gives a clearer rejection signal).
	ruleCombos := config.LimitCombinations
	config.LimitCombinations = make([]LimitCombination, 0, len(global.LimitCombinations)+len(ruleCombos))
	config.LimitCombinations = append(config.LimitCombinations, global.LimitCombinations...)
	config.LimitCombinations = append(config.LimitCombinations, ruleCombos...)

	if redisRaw := raw.Get("redis"); redisRaw.IsObject() {
		backend, err := initRedisBackend(raw)
		if err != nil {
			return err
		}
		config.Backend = backend
	}
	return config.Validate()
}

// onHttpRequestHeaders handles the request phase for both QueryLimits and TokenLimits:
//   - QueryLimits -> mode=check_and_add, count=1    (request-count sliding window)
//   - TokenLimits -> mode=check,         count=0    (reject early if the token quota
//                                                    is already exhausted)
//
// Matched TokenLimits combinations are recorded in ctx; onHttpStreamDone then
// performs the response-phase "add" using the real total_tokens.
func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	// Pin the route cache: during the async Redis dispatch, other filters
	// (or Higress internals) may invalidate the selected route which in turn
	// breaks SendHttpResponseWithDetail delivery.
	ctx.DisableReroute()

	if !isPathEnabled(&config) {
		return types.ActionContinue
	}

	headers, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		proxywasm.LogWarnf("%s: get request headers failed, fail-open: %v", pluginName, err)
		return types.ActionContinue
	}

	reqID := uuid.NewString()
	ctx.SetContext(ctxKeyRequestID, reqID)

	// Capture the AI-routing labels once at request phase. We deliberately do
	// NOT call proxywasm.GetProperty / re-read headers inside the (possibly
	// async) handleEvalResult callback, both for performance and because
	// CLAUDE.md flags hostcalls between callback entry and
	// SendHttpResponseWithDetail as risky on some Higress builds. Stash for
	// onHttpStreamDone too -- token value_total emission uses the same labels.
	ai := readAILabels(headers)
	ctx.SetContext(ctxKeyAILabels, ai)

	nowTime := time.Now()
	entries, pending := collectChecks(&config, headers, reqID, nowTime)
	if len(pending) > 0 {
		ctx.SetContext(ctxKeyTokenPending, pending)
	}
	if len(entries) == 0 {
		// Either no combo's match rules hit, or all combos had quota=0/empty
		// windows. Both surface here as a silent no-op which is the most
		// common "why isn't my limit firing?" misconfiguration symptom.
		proxywasm.LogDebugf("%s: req=%s no combo matched / no effective quota, skipping", pluginName, reqID)
		return types.ActionContinue
	}

	action, err := config.Backend.BatchEval(nowTime.Unix(), entries, func(result EvalResult) {
		handleEvalResult(config, ai, reqID, entries, result)
	})
	if err != nil {
		proxywasm.LogWarnf("%s: req=%s backend eval dispatch failed, fail-open: %v", pluginName, reqID, err)
		return types.ActionContinue
	}
	return action
}

// readAILabels gathers the four AI-routing label values used as metric
// prefixes. Misses on any individual slot fall through to "none" so the
// resulting stat name is always well-formed (sanitizeMetricLabel maps "" to
// "none" too, this is just explicit).
func readAILabels(headers [][2]string) aiLabels {
	return aiLabels{
		Route:    readRouteName(),
		Cluster:  readClusterName(),
		Model:    findHeaderOrNone(headers, headerLLMModel),
		Consumer: findHeaderOrNone(headers, headerConsumer),
	}
}

// readClusterName fetches the Envoy upstream cluster_name property. Resolved
// after route selection (which has happened by the time onHttpRequestHeaders
// runs because we DisableReroute earlier in the same phase).
func readClusterName() string {
	raw, err := proxywasm.GetProperty([]string{"cluster_name"})
	if err != nil || len(raw) == 0 {
		return metricNoneLabel
	}
	return string(raw)
}

// findHeaderOrNone looks up a header value (case-insensitive) and returns
// metricNoneLabel for misses or empty strings, so the resulting stat name
// is always well-formed.
func findHeaderOrNone(headers [][2]string, name string) string {
	for _, kv := range headers {
		if strings.EqualFold(kv[0], name) && kv[1] != "" {
			return kv[1]
		}
	}
	return metricNoneLabel
}

// readRouteName fetches the Envoy route_name property, returning a stable
// fallback when unset (e.g. on direct response paths). The fallback flows
// through to metric labels, so we explicitly use sanitizeMetricLabel's empty
// sentinel "none" to keep label values uniform.
func readRouteName() string {
	raw, err := proxywasm.GetProperty([]string{"route_name"})
	if err != nil || len(raw) == 0 {
		return metricNoneLabel
	}
	return string(raw)
}

// isPathEnabled reads ":path" from the current request and delegates to
// matchPathFilters. If ":path" cannot be read (extremely unlikely for any
// HTTP/1 or HTTP/2 request), the plugin chooses to bypass the limit for
// that request, matching the fail-open behavior used elsewhere.
func isPathEnabled(config *PluginConfig) bool {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		// :path is mandatory in HTTP/2 and synthesized for HTTP/1 by Envoy;
		// reaching here is a host-side anomaly. Surface it so an unexpected
		// silent fail-open isn't blamed on bad config.
		proxywasm.LogWarnf("%s: read :path failed, fail-open: %v", pluginName, err)
		return false
	}
	if !matchPathFilters(config, path) {
		// Most common reason a request appears un-rate-limited: its path
		// isn't covered by enable_on_path_suffix / enable_on_path_prefix.
		// Debug-level so high-QPS deployments can keep it off in prod.
		proxywasm.LogDebugf("%s: path %q not enabled by path filters, skipping", pluginName, path)
		return false
	}
	return true
}

// matchPathFilters reports whether path matches any of the configured
// enable_on_path_suffix / enable_on_path_prefix entries. The two lists are
// OR-combined: a suffix "*" or an empty prefix is a wildcard that matches
// every path. The query string is stripped before matching.
func matchPathFilters(config *PluginConfig, path string) bool {
	if idx := strings.IndexByte(path, '?'); idx != -1 {
		path = path[:idx]
	}
	for _, suffix := range config.EnableOnPathSuffix {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			return true
		}
	}
	for _, prefix := range config.EnableOnPathPrefix {
		if prefix == "" || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// collectChecks walks every combination and its match rules to build the
// Lua script arguments for the request phase, and to collect token-flavoured
// "add" tasks to execute in the response phase.
//
// Redis key layout:
//
//	<rule_name>|<combo.Name>|<dim1>|<dim2>|...|q:<window>      (rolling request count)
//	<rule_name>|<combo.Name>|<dim1>|<dim2>|...|t:<window>      (rolling token count)
//	<rule_name>|<combo.Name>|<dim1>|<dim2>|...|t:<period>      (calendar token quota)
//
// For the calendar token quota the <period> fragment is a stable label (e.g.
// "each_month") rather than a window-second number, so the key does not
// rotate within a period. The dynamic window-seconds value is sent only as
// a lua ARGV.
func collectChecks(config *PluginConfig, headers [][2]string, reqID string, nowTime time.Time) (entries []LimitEntry, pending []tokenPendingAdd) {
	for i := range config.LimitCombinations {
		combo := &config.LimitCombinations[i]

		// Dimension match first; every limit kind on this combo shares the result.
		base := []string{config.RuleName, combo.Name}
		matched := true
		for j := range combo.Match {
			v := combo.Match[j].Match(headers)
			if v == nil {
				matched = false
				break
			}
			base = append(base, combo.Match[j].KeyPart(*v))
		}
		if !matched {
			continue
		}

		// Pre-compute the dimension fragment shared by every limit kind on this
		// combo. base[2:] is the slice of <source>:<name>=<value> fragments
		// produced by MatchRule.KeyPart; joining with '|' mirrors the layout
		// embedded inside the Redis key, but the bucket label here intentionally
		// excludes the trailing kind:period segment so the same value labels
		// every metric that pertains to this combo+match-result.
		bucket := strings.Join(base[2:], "|")

		// QueryLimits (rolling): check-and-add with count=1
		if combo.QueryLimits != nil {
			if win, quota := combo.QueryLimits.GetWindowAndQuota(); quota > 0 {
				key := strings.Join(append(base, "q:"+combo.QueryLimits.KeyPart()), "|")
				entries = append(entries, LimitEntry{
					Key:    key,
					Window: win,
					Quota:  quota,
					Mode:   modeCheckAndAdd,
					Member: reqID,
					Count:  1,
					TTL:    rollingTTL(win),
					Combo:  combo.Name,
					Kind:   metricKindQuery,
					Bucket: bucket,
				})
			}
		}

		// TokenLimits (rolling): check-only in the request phase; pending add in response phase.
		if combo.TokenLimits != nil {
			if win, quota := combo.TokenLimits.GetWindowAndQuota(); quota > 0 {
				key := strings.Join(append(base, "t:"+combo.TokenLimits.KeyPart()), "|")
				entries = append(entries, LimitEntry{
					Key:    key,
					Window: win,
					Quota:  quota,
					Mode:   modeCheck,
					Member: reqID,
					Count:  0,
					TTL:    rollingTTL(win),
					Combo:  combo.Name,
					Kind:   metricKindTokenRolling,
					Bucket: bucket,
				})
				pending = append(pending, tokenPendingAdd{
					Key:         key,
					FixedWindow: win,
					Quota:       quota,
					Combo:       combo.Name,
					Kind:        metricKindTokenRolling,
					Bucket:      bucket,
				})
			}
		}

		// TokenQuota (calendar): check-only in the request phase; pending add in response phase.
		// Window is recomputed at response time to handle period rollover during a long stream.
		// TTL is set to (period_end - now + grace) so the key survives until the period ends
		// regardless of how early in the period the first write happens (a dynamic short
		// window at period start would otherwise produce a 1-2 second TTL).
		if combo.TokenQuota != nil {
			if win, quota := combo.TokenQuota.GetWindowAndQuota(nowTime); quota > 0 {
				key := strings.Join(append(base, "t:"+combo.TokenQuota.KeyPart()), "|")
				entries = append(entries, LimitEntry{
					Key:    key,
					Window: win,
					Quota:  quota,
					Mode:   modeCheck,
					Member: reqID,
					Count:  0,
					TTL:    calendarTTL(combo.TokenQuota, nowTime),
					Combo:  combo.Name,
					Kind:   metricKindTokenCalendar,
					Bucket: bucket,
				})
				pending = append(pending, tokenPendingAdd{
					Key:          key,
					CalendarSpec: combo.TokenQuota,
					Quota:        quota,
					Combo:        combo.Name,
					Kind:         metricKindTokenCalendar,
					Bucket:       bucket,
				})
			}
		}
	}
	return entries, pending
}

// handleEvalResult handles the backend result for the request phase.
// On success it resumes the paused request (no-op for localBackend which never
// stops the request). On failure it sends a rate-limit rejection.
//
// Metric emission rules (see README "Metrics"):
//   - request_total{result=passed|limited} +1 per request
//   - On PASSED: value_total{combo,kind,bucket} += entry.Count for every
//     check_and_add entry (currently only query_limits). Token entries wait
//     for onHttpStreamDone.
//   - On LIMITED: rejected_total{combo,kind,bucket} +1 for the entry whose
//     Key matches result.LimitedKey. Lua/local short-circuit guarantees
//     exactly one entry attribution per rejected request.
//
// Rejection metrics are emitted *after* rejected() so the path between
// callback entry and SendHttpResponseWithDetail stays minimal -- the same
// hazard CLAUDE.md flags for the redis async case.
func handleEvalResult(config PluginConfig, ai aiLabels, reqID string, entries []LimitEntry, result EvalResult) {
	if result.Status == luaSuccessReply {
		emitRequestOutcome(ai, config.RuleName, metricResultPassed)
		for i := range entries {
			e := &entries[i]
			if e.Mode == modeCheckAndAdd {
				emitValue(ai, config.RuleName, e.Combo, e.Kind, e.Bucket, uint64(e.Count))
			}
		}
		// Resuming the paused request is the redis-backend's responsibility
		// (handled inside its callback wrapper). Local-backend never paused
		// the request, so calling Resume here would be a host-side no-op at
		// best and a wasted call on every passed request at worst.
		return
	}
	rejected(config, reqID, result.LimitedKey)
	emitRequestOutcome(ai, config.RuleName, metricResultLimited)
	for i := range entries {
		e := &entries[i]
		if e.Key == result.LimitedKey {
			emitRejected(ai, config.RuleName, e.Combo, e.Kind, e.Bucket)
			break
		}
	}
}

// rejected sends a local reply terminating the request.
//
// CRITICAL: the body MUST be non-empty AND valid JSON. Two coupled hazards:
//
//  1. Empty body: Envoy's local-reply path queues `headers + end_stream` with a
//     nil body pointer as a degenerate response in some filter chains and never
//     flushes it (access log: 429 + bytes_sent=0 + response_flags=DC).
//  2. Non-JSON body in an AI streaming context: when the request had `stream:true`,
//     gpustack-token-usage's streaming response body handler runs *before* this
//     local reply reaches the client (priority 910 vs ours 600 — token-usage
//     decodes first, sets IsStreamingResponse=true, and on encode parses every
//     chunk through mergeLargeUsageChunks). That helper treats non-JSON chunks
//     as "incomplete" SSE fragments, buffers them into IncompleteChunkData,
//     and returns nil; processTokenUsage then replaces the response body with
//     an empty string. The downstream client sees `Content-Length: 28` headers
//     followed by zero body bytes.
//
// Wrapping the message in a JSON envelope makes `json.Valid` succeed inside
// mergeLargeUsageChunks, so the chunk passes through unchanged. The OpenAI
// `{"error":{...}}` shape is also what real OpenAI / Higress AI clients expect
// for HTTP-level errors, so this doubles as a usability improvement.
//
// The x-ratelimit-limited-key header echoes the Redis key that triggered the
// rejection. It is gated on ShowLimitQuotaHeader (default false) because the
// key fragment may include sensitive identifiers (consumer / api_key / user-id)
// that we don't want to leak to arbitrary downstream clients by default.
func rejected(config PluginConfig, reqID, limitedKey string) {
	code := config.RejectedCode
	if code == 0 {
		code = defaultRejectedCode
	}
	msg := config.RejectedMsg
	if msg == "" {
		msg = defaultRejectedMsg
	}
	body := buildRejectedBody(msg, code)
	headers := buildRejectedHeaders(config, limitedKey)
	headers = append(headers, [2]string{"content-type", "application/json"})
	proxywasm.LogInfof("%s: rate limit exceeded, req=%s key=%s", pluginName, reqID, limitedKey)
	if err := proxywasm.SendHttpResponseWithDetail(
		uint32(code),
		pluginName+".rejected",
		headers,
		body,
		-1,
	); err != nil {
		// Failure here means the rejection response did not get queued by
		// the host. Symptoms downstream are typically a stalled connection
		// or a default Envoy 5xx; logging is the only signal an operator
		// gets that this was the rate-limit branch. Logging AFTER the
		// hostcall is safe (the CLAUDE.md "callback minimality" hazard
		// concerns hostcalls BEFORE SendHttpResponseWithDetail, not after).
		proxywasm.LogWarnf("%s: SendHttpResponseWithDetail failed for req=%s: %v", pluginName, reqID, err)
	}
}

// buildRejectedBody returns the JSON envelope used as the rate-limit rejection
// body. The shape mirrors OpenAI / Anthropic error responses so existing AI
// clients can deserialize it, and -- crucially -- it is valid JSON, which
// prevents gpustack-token-usage's streaming SSE parser from buffering it as
// an "incomplete chunk" and silently zeroing out the response body. If the
// caller-supplied msg is already a JSON object, it is used verbatim so the
// operator can override the schema without further escaping.
func buildRejectedBody(msg string, code int) []byte {
	if json.Valid([]byte(msg)) && len(msg) > 0 && msg[0] == '{' {
		return []byte(msg)
	}
	envelope := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
	}{}
	envelope.Error.Message = msg
	envelope.Error.Type = "rate_limit_exceeded"
	envelope.Error.Code = code
	out, err := json.Marshal(envelope)
	if err != nil {
		// json.Marshal of a fixed shape with a string field cannot realistically
		// fail; fall back to a hard-coded valid JSON payload so the body still
		// passes mergeLargeUsageChunks's json.Valid check.
		return []byte(`{"error":{"message":"Too many requests","type":"rate_limit_exceeded","code":429}}`)
	}
	return out
}

// buildRejectedHeaders returns the response headers attached to a rate-limit
// rejection. Echoing the limited Redis key back to the client is gated on
// ShowLimitQuotaHeader (default false): the key fragment may include sensitive
// identifiers (consumer / api_key / user-id) and we don't want to leak that
// to arbitrary downstream callers by default.
func buildRejectedHeaders(config PluginConfig, limitedKey string) [][2]string {
	if !config.ShowLimitQuotaHeader {
		return nil
	}
	return [][2]string{
		{"x-ratelimit-limited-key", limitedKey},
	}
}

// onHttpStreamDone runs the response-phase "add" for TokenLimits once the
// stream has fully ended.
//
// total_token is taken from the Envoy filter-state "ai_log", which is published
// by an upstream AI plugin (ai-statistics / gpustack-token-usage / ...) via
// wrapper.HttpContext.WriteUserAttributeToLogWithKey(wrapper.AILogKey). This
// plugin deliberately does not parse the SSE body itself, to avoid duplicating
// work already done by the upstream plugin.
//
// Requirement: gpustack-rate-limit must run after the plugin that publishes
// "ai_log" in the response-phase filter order.
func onHttpStreamDone(ctx wrapper.HttpContext, config PluginConfig) {
	pending, ok := ctx.GetContext(ctxKeyTokenPending).([]tokenPendingAdd)
	if !ok || len(pending) == 0 {
		return
	}
	tokens := readTotalTokensFromAILog()
	if tokens <= 0 {
		proxywasm.LogDebugf("%s: total_token unavailable from filter state %q, token-limits add skipped",
			pluginName, wrapper.AILogKey)
		return
	}
	member := ctx.GetStringContext(ctxKeyRequestID, uuid.NewString())
	proxywasm.LogDebugf("%s: stream done, req=%s tokens=%d pending=%d",
		pluginName, member, tokens, len(pending))

	nowTime := time.Now()
	addEntries := make([]LimitEntry, 0, len(pending))
	for _, p := range pending {
		addEntries = append(addEntries, LimitEntry{
			Key:    p.Key,
			Window: p.windowSeconds(nowTime),
			Quota:  p.Quota,
			Mode:   modeAdd,
			Member: member,
			Count:  tokens,
			TTL:    p.ttlSeconds(nowTime),
			Combo:  p.Combo,
			Kind:   p.Kind,
			Bucket: p.Bucket,
		})
	}

	if _, err := config.Backend.BatchEval(nowTime.Unix(), addEntries, nil); err != nil {
		proxywasm.LogWarnf("%s: token-limits add dispatch failed: %v", pluginName, err)
		return
	}
	// Successful add (sync for local, fire-and-forget for redis): record the
	// per-bucket token consumption. The redis async path may not have actually
	// committed yet by the time we increment, but BatchEval returning nil error
	// is the closest "intent to write" signal we have without plumbing a
	// response callback for add-only flows.
	ai, ok := ctx.GetContext(ctxKeyAILabels).(aiLabels)
	if !ok {
		// Onthe rare path where the request-headers handler did not run (e.g.
		// the request was short-circuited before our phase), fall back to the
		// "none" sentinel for every slot so the stat name is still emittable.
		ai = aiLabels{
			Route:    metricNoneLabel,
			Cluster:  metricNoneLabel,
			Model:    metricNoneLabel,
			Consumer: metricNoneLabel,
		}
	}
	for _, p := range pending {
		emitValue(ai, config.RuleName, p.Combo, p.Kind, p.Bucket, uint64(tokens))
	}
}

// readTotalTokensFromAILog reads total_token from the Envoy filter-state "ai_log".
//
// The filter-state content is a wrapper.MarshalStr-escaped JSON string; we
// invert it with wrapper.UnmarshalStr and then query the field via gjson.
// The field name "total_token" comes from tokenusage.CtxKeyTotalToken.
func readTotalTokensFromAILog() int64 {
	data, err := proxywasm.GetProperty([]string{wrapper.AILogKey})
	if err != nil || len(data) == 0 {
		return 0
	}
	raw := wrapper.UnmarshalStr(fmt.Sprintf(`"%s"`, string(data)))
	if raw == "" {
		return 0
	}
	return gjson.Get(raw, tokenusage.CtxKeyTotalToken).Int()
}
