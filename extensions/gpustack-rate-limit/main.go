package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
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
	if err := InitRedisClusterClient(raw, config); err != nil {
		return err
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

	if raw.Get("redis").Exists() {
		if err := InitRedisClusterClient(raw, config); err != nil {
			return err
		}
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

	keys, args, pending := collectChecks(&config, headers, reqID)
	if len(pending) > 0 {
		ctx.SetContext(ctxKeyTokenPending, pending)
	}
	if len(keys) == 0 {
		// Either no combo's match rules hit, or all combos had quota=0/empty
		// windows. Both surface here as a silent no-op which is the most
		// common "why isn't my limit firing?" misconfiguration symptom.
		proxywasm.LogDebugf("%s: req=%s no combo matched / no effective quota, skipping", pluginName, reqID)
		return types.ActionContinue
	}

	if err := config.RedisClient.Eval(
		multiLevelLimitScript,
		len(keys),
		keys,
		args,
		func(response resp.Value) { handleLuaResponse(config, reqID, response) },
	); err != nil {
		proxywasm.LogWarnf("%s: req=%s redis eval dispatch failed, fail-open: %v", pluginName, reqID, err)
		return types.ActionContinue
	}
	// StopAllIterationAndWatermark: stop header/body/trailer iteration and
	// watermark the downstream connection while Redis is in flight. Matches
	// higress cluster-key-rate-limit / ai-token-ratelimit.
	return types.HeaderStopAllIterationAndWatermark
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
func collectChecks(config *PluginConfig, headers [][2]string, reqID string) (keys, args []interface{}, pending []tokenPendingAdd) {
	nowTime := time.Now()
	now := nowTime.Unix()
	args = []interface{}{strconv.FormatInt(now, 10)}

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

		// QueryLimits (rolling): check-and-add with count=1
		if combo.QueryLimits != nil {
			if win, quota := combo.QueryLimits.GetWindowAndQuota(); quota > 0 {
				key := strings.Join(append(base, "q:"+combo.QueryLimits.KeyPart()), "|")
				keys = append(keys, key)
				args = append(args,
					strconv.FormatInt(win, 10),
					strconv.Itoa(quota),
					modeCheckAndAdd,
					reqID,
					"1",
					strconv.FormatInt(rollingTTL(win), 10),
				)
			}
		}

		// TokenLimits (rolling): check-only in the request phase; pending add in response phase.
		if combo.TokenLimits != nil {
			if win, quota := combo.TokenLimits.GetWindowAndQuota(); quota > 0 {
				key := strings.Join(append(base, "t:"+combo.TokenLimits.KeyPart()), "|")
				keys = append(keys, key)
				args = append(args,
					strconv.FormatInt(win, 10),
					strconv.Itoa(quota),
					modeCheck,
					reqID,
					"0",
					strconv.FormatInt(rollingTTL(win), 10),
				)
				pending = append(pending, tokenPendingAdd{Key: key, FixedWindow: win, Quota: quota})
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
				keys = append(keys, key)
				args = append(args,
					strconv.FormatInt(win, 10),
					strconv.Itoa(quota),
					modeCheck,
					reqID,
					"0",
					strconv.FormatInt(calendarTTL(combo.TokenQuota, nowTime), 10),
				)
				pending = append(pending, tokenPendingAdd{Key: key, CalendarSpec: combo.TokenQuota, Quota: quota})
			}
		}
	}
	return keys, args, pending
}

// handleLuaResponse handles the Lua script result for the request phase.
//
// The script returns an array {status, limited_key}. Reading via
// response.Array() mirrors higress cluster-key-rate-limit's known-good
// callback path under this Envoy/wasm-go build.
//
// reqID is plumbed through so both the malformed-reply Warn and the
// rejection Info log carry the request identifier, allowing operators to
// correlate plugin logs with Envoy access logs / upstream traces.
func handleLuaResponse(config PluginConfig, reqID string, response resp.Value) {
	result := response.Array()
	if len(result) < 1 {
		// Lua should always return a 2-element array (SUCCESS or LIMITED).
		// A shorter / nil result means lua panicked, redis returned an error
		// the SDK couldn't unmarshal, or some unexpected protocol response.
		// Fail-open consistent with the dispatch-error policy.
		proxywasm.LogWarnf("%s: req=%s malformed lua reply (len=%d), fail-open", pluginName, reqID, len(result))
		_ = proxywasm.ResumeHttpRequest()
		return
	}
	if result[0].String() == luaSuccessReply {
		_ = proxywasm.ResumeHttpRequest()
		return
	}
	limitedKey := ""
	if len(result) >= 2 {
		limitedKey = result[1].String()
	}
	rejected(config, reqID, limitedKey)
}

// rejected sends a local reply terminating the request.
//
// CRITICAL: the body MUST be non-empty. Under Higress's AI-route filter chain
// an empty-body local reply is queued by Envoy but never flushed to the
// downstream client (access log shows response_code=429 but bytes_sent=0 and
// response_flags=DC with response_code_details=downstream_remote_disconnect).
// defaultRejectedMsg ensures a non-empty body when RejectedMsg is unset.
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
	body := config.RejectedMsg
	if body == "" {
		body = defaultRejectedMsg
	}
	headers := buildRejectedHeaders(config, limitedKey)
	proxywasm.LogInfof("%s: rate limit exceeded, req=%s key=%s", pluginName, reqID, limitedKey)
	_ = proxywasm.SendHttpResponseWithDetail(
		uint32(code),
		pluginName+".rejected",
		headers,
		[]byte(body),
		-1,
	)
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
	keys := make([]interface{}, 0, len(pending))
	args := []interface{}{strconv.FormatInt(nowTime.Unix(), 10)}
	for _, p := range pending {
		keys = append(keys, p.Key)
		args = append(args,
			strconv.FormatInt(p.windowSeconds(nowTime), 10),
			strconv.Itoa(p.Quota),
			modeAdd,
			member,
			strconv.FormatInt(tokens, 10),
			strconv.FormatInt(p.ttlSeconds(nowTime), 10),
		)
	}

	if err := config.RedisClient.Eval(multiLevelLimitScript, len(keys), keys, args, nil); err != nil {
		proxywasm.LogWarnf("%s: token-limits add dispatch failed: %v", pluginName, err)
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
