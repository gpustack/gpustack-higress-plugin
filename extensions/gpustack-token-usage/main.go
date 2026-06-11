package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// defaultClusterNameRegexps are matched against the FQDN field of Envoy's
// cluster_name ("outbound|<port>|<subset>|<fqdn>"). The realIPHeader /
// header_add trust headers are only injected into the upstream request when
// the FQDN matches one of these (or one of the user-supplied
// additionalClusterNameRegexps) so trusted headers never flow to third-party
// upstreams reached via the proxy.
var defaultClusterNameRegexps = []string{
	`^gpustack(-|\.|$)`,
	`^model-\d+-\d+(\.|$)`,
	`^provider-\d+(\.|$)`,
}

const (
	pluginName = "gpustack-token-usage"

	// defaultOrganizationIDHeader is the request header from which the
	// organization_id metric attribute is extracted. Override via the
	// `organizationIDHeader` config field.
	defaultOrganizationIDHeader = "X-Organization-Id"

	// defaultMaxResponseBodyBytes caps both the accumulated raw buffer and the
	// gzip-decoded output on the non-streaming sniff path. It bounds WASM-VM
	// memory against an enormous response (OOM) and a zip bomb (a tiny gzip
	// payload that inflates to gigabytes). A body exceeding the cap is not
	// tracked for usage — the plugin stays fail-open and never blocks traffic.
	// Override via the `maxResponseBodyBytes` config field. 10 MiB comfortably
	// covers large embedding batches while keeping peak memory bounded.
	defaultMaxResponseBodyBytes = 10 * 1024 * 1024
)

const (
	IsStreamingResponse        = "is_streaming_response"
	StatisticsRequestStartTime = "gpustack_request_start_time"
	StatisticsFirstTokenTime   = "gpustack_first_token_time"
	TimeToFirstTokenDuration   = "gpustack_llm_first_token_duration"

	IncompleteChunk     = "gpustack_incomplete_chunk"
	IncompleteChunkData = "gpustack_incomplete_chunk_data"
	UsageExtraKey       = "gpustack_usage_extra"
	SeenUsageChunk      = "gpustack_seen_usage_chunk"
	ProcessedUsageChunk = "gpustack_processed_usage_chunk"

	RequestModelKey        = "gpustack_request_model"
	FinalUsageKey          = "gpustack_final_usage"
	ProcessBodyKey         = "gpustack_process_body"
	InjectStreamOptionsKey = "gpustack_inject_stream_options"
	BaseMetricsKey         = "gpustack_base_metrics"
	MetricsTrackingKey     = "gpustack_metrics_tracking"
	MetricsStartedAtKey    = "gpustack_metrics_started_at"
	MetricsReportedKey     = "gpustack_metrics_reported"
	RequestHeadersKey      = "gpustack_headers"
	MultipartContentType   = "gpustack_multipart_content_type"

	// ResponseContentEncodingKey holds the upstream response's
	// content-encoding header (e.g. "gzip"), captured on the non-streaming
	// path. We deliberately keep gzip on non-streaming responses (the plugin
	// only sniffs them, never rewrites their body) so the client retains
	// compression; the accumulated body buffer is decoded once at
	// end-of-stream purely to read usage. Streaming requests instead strip
	// accept-encoding upstream because they DO rewrite the SSE chunks.
	ResponseContentEncodingKey = "gpustack_response_content_encoding"

	// NonStreamBodyBufferKey accumulates the (possibly gzip-compressed) raw
	// response chunks for a non-streaming response. The usage object sits at
	// the tail of a single JSON document after a potentially huge payload
	// (e.g. embedding vectors), so Envoy delivers it across multiple chunks;
	// parsing requires the complete body. Buffering the still-compressed
	// bytes is cheaper than the decoded form. Decoded + parsed once at
	// end-of-stream.
	NonStreamBodyBufferKey = "gpustack_nonstream_body_buffer"

	// BufferExceededKey latches once the non-streaming buffer passes
	// MaxResponseBodyBytes, so subsequent chunks short-circuit instead of
	// re-checking and re-logging. Usage is not recorded for such a response.
	BufferExceededKey = "gpustack_buffer_exceeded"

	// StripUsageChunkKey is set when the user's request body had explicit
	// stream_options.include_usage:false; we override to true to guarantee
	// usage telemetry, then drop the OpenAI usage-only chunk before the
	// client sees it (sniff-but-don't-leak).
	StripUsageChunkKey = "gpustack_strip_usage_chunk"

	// OutputChunkCountKey accumulates the number of streaming delta chunks
	// containing output content (OpenAI choices[0].delta.content non-empty,
	// Anthropic content_block_delta with non-empty text/json/thinking).
	// Reported as a fallback signal for token estimation when the canonical
	// usage chunk never arrives (e.g. client-disconnect mid-stream).
	OutputChunkCountKey = "gpustack_output_chunk_count"

	// RequestContentBytesKey holds the byte-length of extracted text content
	// from the request body (messages[].content / input[].content / system).
	// This is a downstream signal for input-token estimation in the
	// completed=false path; the byte→token ratio is content/locale-specific
	// and must be applied by the billing service, not by the proxy.
	RequestContentBytesKey = "gpustack_request_content_bytes"

	// ResponseCompletedKey records whether the response reached its normal
	// terminus. Set true when:
	//   - onStreamingResponseBody observed endOfStream=true (covers both
	//     SSE streams and non-streaming JSON, which reaches the same
	//     callback as a single chunk with endOfStream=true);
	//   - onHttpResponseHeaders skipped body reading (TTS/image path) and
	//     the upstream responded 2xx.
	// A mid-stream client disconnect / upstream reset never produces
	// endOfStream=true, so this stays false. Decouples the completed flag
	// from token counts so non-LLM endpoints (token fields legitimately 0)
	// are not confused with interrupted LLM streams.
	ResponseCompletedKey = "gpustack_response_completed"
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessResponseHeaders(onHttpResponseHeaders),
		wrapper.ProcessStreamingResponseBody(onStreamingResponseBody),
		wrapper.ProcessStreamDone(onHttpStreamDone),
		// Periodically rebuild the VM to bound long-running memory-leak
		// accumulation — aligned with gpustack-rate-limit and Higress AI
		// plugin conventions. Per-request transient memory on the
		// non-streaming sniff path peaks near 2×maxResponseBodyBytes
		// (raw buffer + decoded body), so 200 MiB leaves ample headroom.
		wrapper.WithRebuildAfterRequests[PluginConfig](1000),
		wrapper.WithRebuildMaxMemBytes[PluginConfig](200 * 1024 * 1024),
	)
}

// EndpointConfig holds the metrics reporting endpoint configuration.
type EndpointConfig struct {
	ServiceName string
	ServicePort int64
	Path        string
	TimeoutMs   uint32
}

// PluginConfig holds plugin configuration.
type PluginConfig struct {
	EnableOnPathSuffix      []string
	EnableUsageOnPathSuffix []string
	Endpoint                *EndpointConfig
	// HeaderAdd is shared between two purposes:
	//   1. Injected into upstream LLM requests (with realIPHeader) when
	//      cluster_name matches ClusterNameMatchers — so the GPUStack
	//      backend can validate the gateway-issued trust token.
	//   2. Attached to every metrics-report POST sent to Endpoint — the
	//      report endpoint also lives on the GPUStack backend and validates
	//      the same token. Sharing one config keeps the secret single-sourced.
	HeaderAdd            map[string]string
	ReportClient         wrapper.HttpClient
	RealIPHeader         string
	ClusterNameMatchers  []*regexp.Regexp
	OrganizationIDHeader string
	// MaxResponseBodyBytes caps the non-streaming sniff buffer and the
	// gzip-decoded size (see defaultMaxResponseBodyBytes).
	MaxResponseBodyBytes int
}

// ModelUsageMetrics is the JSON payload sent to the metrics reporting endpoint.
// StartedAt / CompletedAt are UnixMilli wall-clock stamps captured at request
// entry (after path/cluster filtering) and at report dispatch respectively.
// Both are needed because rate-limit accounting splits across the two:
// QueryLimits attribute requests at start, TokenLimits / TokenQuota attribute
// tokens at completion (a stream that crosses a calendar boundary lands in the
// period it ends in).
//
// Completed is true iff the canonical usage chunk was observed before the
// stream ended. When false (e.g. client-disconnect mid-stream), token fields
// may be 0 (OpenAI/vLLM) or partial (Anthropic message_start carries
// input_tokens early, so InputToken is usually populated even on cancel).
// OutputChunkCount and RequestContentBytes are downstream-side estimation
// inputs; the proxy never applies estimation ratios itself.
type ModelUsageMetrics struct {
	Model               string  `json:"model"`
	InputToken          int64   `json:"input_token"`
	OutputToken         int64   `json:"output_token"`
	TotalToken          int64   `json:"total_token"`
	InputCachedToken    int64   `json:"input_cached_token"`
	RequestCount        int     `json:"request_count"`
	Completed           bool    `json:"completed"`
	OutputChunkCount    int64   `json:"output_chunk_count"`
	RequestContentBytes int64   `json:"request_content_bytes"`
	StartedAt           int64   `json:"started_at"`
	CompletedAt         int64   `json:"completed_at"`
	UserID              *int64  `json:"user_id,omitempty"`
	ModelID             *int64  `json:"model_id,omitempty"`
	ModelRouteID        *int64  `json:"model_route_id,omitempty"`
	ProviderID          *int64  `json:"provider_id,omitempty"`
	AccessKey           *string `json:"access_key,omitempty"`
	OrganizationID      *string `json:"organization_id,omitempty"`
}

func matchPathSuffix(targetURI string, suffixes []string) bool {
	u, err := url.ParseRequestURI(targetURI)
	if err != nil {
		return false
	}
	path := u.Path
	for _, suffix := range suffixes {
		if len(suffix) > 0 && len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func (c *PluginConfig) shouldProcess(targetURI string) bool {
	matched := matchPathSuffix(targetURI, c.EnableOnPathSuffix)
	if matched {
		proxywasm.LogDebugf("shouldProcess: matched for path %s", targetURI)
	}
	return matched
}

func (c *PluginConfig) shouldInjectStreamOptions(targetURI string) bool {
	return matchPathSuffix(targetURI, c.EnableUsageOnPathSuffix)
}

func buildPathSuffixes(configField gjson.Result, defaults []string) []string {
	set := make(map[string]bool, len(defaults))
	for _, p := range defaults {
		set[p] = true
	}
	for _, suffix := range configField.Array() {
		path := suffix.String()
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			proxywasm.LogDebugf("buildPathSuffixes: %s is not a valid path suffix (must start with /), skipping", path)
			continue
		}
		set[path] = true
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	return result
}

func parseConfig(json gjson.Result, config *PluginConfig) error {
	config.EnableUsageOnPathSuffix = buildPathSuffixes(json.Get("enableUsageOnPathSuffix"), []string{
		"/chat/completions",
		"/completions",
	})
	config.EnableOnPathSuffix = buildPathSuffixes(json.Get("enableOnPathSuffix"), []string{
		"/chat/completions",
		"/completions",
		"/responses",
		"/messages",
		"/embeddings",
		"/rerank",
	})

	endpoint := json.Get("endpoint")
	if endpoint.Exists() && endpoint.Get("service_name").String() != "" {
		timeoutMs := uint32(endpoint.Get("timeout_ms").Uint())
		if timeoutMs == 0 {
			timeoutMs = 5000
		}
		config.Endpoint = &EndpointConfig{
			ServiceName: endpoint.Get("service_name").String(),
			ServicePort: endpoint.Get("service_port").Int(),
			Path:        endpoint.Get("path").String(),
			TimeoutMs:   timeoutMs,
		}
		config.ReportClient = wrapper.NewClusterClient(wrapper.FQDNCluster{
			FQDN: config.Endpoint.ServiceName,
			Port: config.Endpoint.ServicePort,
		})
	}

	config.HeaderAdd = make(map[string]string)
	for k, v := range json.Get("header_add").Map() {
		config.HeaderAdd[k] = v.String()
	}

	config.RealIPHeader = json.Get("realIPHeader").String()

	if maxBody := json.Get("maxResponseBodyBytes").Int(); maxBody > 0 {
		config.MaxResponseBodyBytes = int(maxBody)
	} else {
		config.MaxResponseBodyBytes = defaultMaxResponseBodyBytes
	}

	if orgHeader := json.Get("organizationIDHeader").String(); orgHeader != "" {
		config.OrganizationIDHeader = orgHeader
	} else {
		config.OrganizationIDHeader = defaultOrganizationIDHeader
	}

	patterns := append([]string(nil), defaultClusterNameRegexps...)
	for _, item := range json.Get("additionalClusterNameRegexps").Array() {
		if s := item.String(); s != "" {
			patterns = append(patterns, s)
		}
	}
	config.ClusterNameMatchers = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid cluster_name regexp %q: %w", p, err)
		}
		config.ClusterNameMatchers = append(config.ClusterNameMatchers, re)
	}

	return nil
}

// extractClusterFQDN returns the FQDN field of Envoy's
// "outbound|<port>|<subset>|<fqdn>". For non-Envoy-shaped values the raw
// string is returned so user-supplied regexps can still match literally.
func extractClusterFQDN(clusterName string) string {
	if parts := strings.SplitN(clusterName, "|", 4); len(parts) == 4 {
		return parts[3]
	}
	return clusterName
}

func matchesAnyCluster(fqdn string, matchers []*regexp.Regexp) bool {
	for _, m := range matchers {
		if m.MatchString(fqdn) {
			return true
		}
	}
	return false
}

// injectTrustHeaders writes realIPHeader and the configured header_add map
// into the request — but only when the upstream cluster's FQDN matches the
// configured trust-cluster regexps. For non-matching clusters (e.g. third-
// party LLM providers) this is a no-op so the gateway-issued trust token
// never leaks. Headers are Replaced (not Added) so a client-supplied value
// cannot co-exist with the gateway-injected one.
//
// Requires that cluster_name has already been resolved by a preceding filter
// when this runs in the request-headers phase. In Higress, model-router /
// model-mapper resolve the route (and thus the cluster) before any Wasm
// plugin priority < 900 runs; the recommended priority of 400 therefore
// guarantees cluster_name is populated. If the property is empty (no
// upstream resolved yet, or unrecognised flow) the function fail-closes:
// no headers are written, so the trust token cannot leak via a misordered
// filter chain.
func injectTrustHeaders(config PluginConfig) {
	if config.RealIPHeader == "" && len(config.HeaderAdd) == 0 {
		return
	}
	raw, err := proxywasm.GetProperty([]string{"cluster_name"})
	if err != nil || len(raw) == 0 {
		proxywasm.LogDebugf("injectTrustHeaders: cluster_name unavailable: %v", err)
		return
	}
	if !matchesAnyCluster(extractClusterFQDN(string(raw)), config.ClusterNameMatchers) {
		return
	}
	if config.RealIPHeader != "" {
		writeRealIPHeader(config.RealIPHeader)
	}
	for k, v := range config.HeaderAdd {
		if err := proxywasm.ReplaceHttpRequestHeader(k, v); err != nil {
			proxywasm.LogWarnf("injectTrustHeaders: failed to replace header %s: %v", k, err)
		}
	}
}

func writeRealIPHeader(name string) {
	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		proxywasm.LogDebugf("writeRealIPHeader: failed to get source address: %v", err)
		return
	}
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		host = string(data)
	}
	if err := proxywasm.ReplaceHttpRequestHeader(name, host); err != nil {
		proxywasm.LogWarnf("writeRealIPHeader: failed to replace header %s: %v", name, err)
	}
}

// baseMediaType returns the media type without parameters (e.g. "application/json; charset=utf-8" → "application/json").
func baseMediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}

// prepareMetrics checks whether this request should be tracked for metrics reporting.
// Returns true if the request body must be read to extract the model name.
func prepareMetrics(ctx wrapper.HttpContext) bool {
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	mt := baseMediaType(contentType)
	if mt != "application/json" && mt != "multipart/form-data" {
		return false
	}
	clusterNameBytes, _ := proxywasm.GetProperty([]string{"cluster_name"})
	if len(clusterNameBytes) > 0 {
		modelID, providerID := parseClusterName(string(clusterNameBytes))
		if modelID == nil && providerID == nil {
			proxywasm.LogDebugf("prepareMetrics: cluster %s not tracked", string(clusterNameBytes))
			return false
		}
	}
	ctx.SetContext(MetricsTrackingKey, true)
	ctx.SetContext(MetricsStartedAtKey, time.Now().UnixMilli())
	if mt == "multipart/form-data" {
		ctx.SetContext(MultipartContentType, contentType)
	}
	return true
}

// prepareStream checks whether response body reading (and optionally stream_options
// injection) is needed. Returns true if the request body must be read.
// Anthropic-style /messages endpoints include usage by default and skip injection.
func prepareStream(ctx wrapper.HttpContext, config PluginConfig) bool {
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	if baseMediaType(contentType) != "application/json" {
		return false
	}
	needBody := false
	if config.shouldProcess(ctx.Path()) {
		ctx.SetContext(ProcessBodyKey, true)
		ctx.SetContext(StatisticsRequestStartTime, time.Now().UnixMilli())
		needBody = true
	}
	if config.shouldInjectStreamOptions(ctx.Path()) {
		ctx.SetContext(InjectStreamOptionsKey, true)
		needBody = true
	}
	return needBody
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	// 0. Inject trust headers (real-IP + header_add) into the upstream
	//    request, gated by cluster_name match. Runs unconditionally — does
	//    not depend on path-suffix / metrics-tracking decisions below.
	injectTrustHeaders(config)

	// 1. Check if metrics tracking requires reading the request body.
	metricsNeedBody := prepareMetrics(ctx)

	// 2. Check if stream injection requires reading the request body.
	streamNeedBody := prepareStream(ctx, config)

	// 3. Neither needs the body: skip body read.
	if !metricsNeedBody && !streamNeedBody {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	hs, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		proxywasm.LogWarnf("onHttpRequestHeaders: failed to get request headers: %v", err)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	ctx.SetContext(RequestHeadersKey, hs)
	return types.HeaderStopIteration
}

func extractModelFromMultipart(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		proxywasm.LogDebugf("extractModelFromMultipart: failed to parse content type: %v", err)
		return ""
	}
	boundary, ok := params["boundary"]
	if !ok {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(io.LimitReader(part, 1024))
			return strings.TrimSpace(buf.String())
		}
	}
	return ""
}

func removeHeader(name string, headers [][2]string) [][2]string {
	var rtn = [][2]string{}
	for _, value := range headers {
		if strings.EqualFold(value[0], name) {
			continue
		}
		rtn = append(rtn, value)
	}
	return rtn
}

func buildBaseMetrics(ctx wrapper.HttpContext, config PluginConfig, headers [][2]string) *ModelUsageMetrics {
	m := &ModelUsageMetrics{
		Model:        ctx.GetStringContext(RequestModelKey, ""),
		RequestCount: 1,
	}
	orgHeader := config.OrganizationIDHeader
	for _, h := range headers {
		if strings.EqualFold(h[0], "x-mse-consumer") && h[1] != "" {
			m.UserID, m.AccessKey = parseConsumerHeader(h[1])
		}
		if strings.EqualFold(h[0], orgHeader) && h[1] != "" {
			v := h[1]
			m.OrganizationID = &v
		}
	}
	return m
}

// processRequestBody extracts the model name, sets stream state, optionally
// forces stream_options.include_usage:true, and computes request content
// bytes for downstream input-token estimation. Returns the (possibly modified)
// headers slice.
//
// Force-include-usage policy: when the path is in EnableUsageOnPathSuffix and
// the request is streaming, we always inject include_usage:true regardless of
// what the client sent. If the client had an explicit include_usage:false, we
// also set StripUsageChunkKey so the response-side will drop the OpenAI
// usage-only chunk before it leaves the proxy — the client's contract is
// preserved while the proxy still gets reliable usage telemetry.
func processRequestBody(ctx wrapper.HttpContext, body []byte, headers [][2]string) [][2]string {
	if multipartCT, ok := ctx.GetContext(MultipartContentType).(string); ok {
		if model := extractModelFromMultipart(body, multipartCT); model != "" {
			ctx.SetContext(RequestModelKey, model)
		}
		ctx.SetContext(IsStreamingResponse, false)
		return headers
	}

	if model := gjson.GetBytes(body, "model").String(); model != "" {
		ctx.SetContext(RequestModelKey, model)
	}

	if cb := extractRequestContentBytes(body); cb > 0 {
		ctx.SetContext(RequestContentBytesKey, cb)
	}

	stream := gjson.GetBytes(body, "stream")
	if ctx.GetBoolContext(ProcessBodyKey, false) {
		streaming := stream.Exists() && stream.Bool()
		ctx.SetContext(IsStreamingResponse, streaming)
		// Streaming responses get their SSE chunks rewritten (usage extras
		// injected / usage-only chunk stripped), so we must see plaintext:
		// strip accept-encoding upstream. Non-streaming responses are only
		// sniffed, never rewritten, so we leave accept-encoding intact and
		// decode the buffered body locally at end-of-stream — the client
		// keeps gzip+chunked.
		if streaming {
			headers = removeHeader("accept-encoding", headers)
		}
	}

	if !ctx.GetBoolContext(InjectStreamOptionsKey, false) {
		return headers
	}
	if !stream.Exists() || !stream.Bool() {
		return headers
	}

	includeUsage := gjson.GetBytes(body, "stream_options.include_usage")
	if includeUsage.Exists() && !includeUsage.Bool() {
		ctx.SetContext(StripUsageChunkKey, true)
	}
	if includeUsage.Exists() && includeUsage.Bool() {
		return headers
	}

	proxywasm.LogDebug("forcing stream_options.include_usage=true on request body")
	newBody, err := sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		proxywasm.LogErrorf("failed to set json body, %v", err)
		return headers
	}
	if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
		proxywasm.LogWarnf("failed to replace new body %s, %v", string(newBody), err)
		return headers
	}
	return removeHeader("content-length", headers)
}

// extractRequestContentBytes sums the byte length of text-bearing fields in
// common AI request shapes:
//   - OpenAI Chat Completions / Anthropic Messages: messages[].content
//     (string, or array of blocks with type=text)
//   - OpenAI Responses API: input[].content (same shape)
//   - Anthropic top-level system (string or array of text blocks)
//
// Image / audio / file blocks are deliberately excluded — their byte size
// (often huge base64) bears no useful relation to token cost. Returns 0 if
// no recognized shape matches; callers should treat 0 as "unknown".
//
// This is a downstream estimation input, not an authoritative count. The
// byte→token ratio depends on tokenizer + content language and must be
// applied by the billing service.
func extractRequestContentBytes(body []byte) int64 {
	var total int64
	visit := func(arr gjson.Result) {
		arr.ForEach(func(_, m gjson.Result) bool {
			content := m.Get("content")
			if !content.Exists() {
				return true
			}
			if content.Type == gjson.String {
				total += int64(len(content.String()))
				return true
			}
			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					if t := block.Get("type").String(); t == "text" || t == "" {
						if text := block.Get("text"); text.Type == gjson.String {
							total += int64(len(text.String()))
						}
					}
					return true
				})
			}
			return true
		})
	}
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		visit(msgs)
	}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		visit(input)
	}
	if sys := gjson.GetBytes(body, "system"); sys.Exists() {
		switch {
		case sys.Type == gjson.String:
			total += int64(len(sys.String()))
		case sys.IsArray():
			sys.ForEach(func(_, block gjson.Result) bool {
				if text := block.Get("text"); text.Type == gjson.String {
					total += int64(len(text.String()))
				}
				return true
			})
		}
	}
	return total
}

func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	proxywasm.LogDebug("processing request body")
	headers, ok := ctx.GetContext(RequestHeadersKey).([][2]string)
	if !ok {
		proxywasm.LogWarn("failed to get headers from context, skip process body")
		return types.ActionContinue
	}
	headers = processRequestBody(ctx, body, headers)
	ctx.SetContext(BaseMetricsKey, buildBaseMetrics(ctx, config, headers))
	_ = proxywasm.ReplaceHttpRequestHeaders(headers)
	return types.ActionContinue
}

func onHttpResponseHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	metricsTracking := ctx.GetBoolContext(MetricsTrackingKey, false)
	processBody := ctx.GetBoolContext(ProcessBodyKey, false)
	if !metricsTracking && !processBody {
		return types.ActionContinue
	}
	if !processBody {
		// Reporting is deferred to onHttpStreamDone — a single emission
		// point that fires for clean completions, upstream errors, and
		// downstream-cancel resets alike. Don't read the response body
		// for this path (TTS/STT/image or other non-text upstream).
		// The body callback is skipped, so the completed signal has to
		// be set here based on the upstream status: 2xx is taken as
		// "normally completed" since these endpoints typically have no
		// token usage and request-count is the billable unit.
		if status, err := proxywasm.GetHttpResponseHeader(":status"); err == nil &&
			len(status) > 0 && status[0] == '2' {
			ctx.SetContext(ResponseCompletedKey, true)
		}
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	isStreaming := ctx.GetBoolContext(IsStreamingResponse, false)
	if isStreaming {
		contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
		if strings.Contains(contentType, "application/json") {
			ctx.SetContext(IsStreamingResponse, false)
			captureResponseContentEncoding(ctx)
			return types.HeaderStopIteration
		}
		return types.ActionContinue
	}
	captureResponseContentEncoding(ctx)
	return types.HeaderStopIteration
}

// captureResponseContentEncoding stashes the upstream content-encoding so the
// non-streaming body path can decode the accumulated buffer before parsing
// usage. Only meaningful for the non-streaming path: streaming requests
// already stripped accept-encoding upstream, so their responses are plaintext.
func captureResponseContentEncoding(ctx wrapper.HttpContext) {
	if enc, err := proxywasm.GetHttpResponseHeader("content-encoding"); err == nil && enc != "" {
		ctx.SetContext(ResponseContentEncodingKey, enc)
	}
}

func onStreamingResponseBody(ctx wrapper.HttpContext, config PluginConfig, data []byte, endOfStream bool) []byte {
	// Non-streaming responses (embeddings, stream:false completions, and
	// stream:true requests whose upstream answered with a single JSON body)
	// are sniff-only: pass every chunk through untouched (client keeps
	// gzip+chunked) while buffering a private copy, then decode+parse usage
	// once at end-of-stream.
	if !ctx.GetBoolContext(IsStreamingResponse, false) {
		handleNonStreamingResponseBody(ctx, data, endOfStream, config.MaxResponseBodyBytes)
		if endOfStream {
			ctx.SetContext(ResponseCompletedKey, true)
		}
		return data
	}

	result := processTokenUsage(ctx, data, config.MaxResponseBodyBytes)
	if endOfStream {
		ctx.SetContext(ResponseCompletedKey, true)
		if ctx.GetBoolContext(SeenUsageChunk, false) && !ctx.GetBoolContext(ProcessedUsageChunk, false) {
			proxywasm.LogWarnf("no usage is found in any chunk with usage bytes")
		}
	}
	return result
}

// handleNonStreamingResponseBody accumulates raw response chunks and, at
// end-of-stream, decodes them per the upstream content-encoding and parses
// usage into FinalUsageKey. The chunks themselves are never modified — the
// caller forwards them verbatim — so this is pure observation. On any decode
// failure (e.g. an unsupported encoding such as brotli) it logs and returns
// without a usage record, keeping the plugin fail-open.
//
// maxBytes bounds both the accumulated raw buffer and the decoded body so a
// huge response or a zip bomb cannot OOM the WASM VM; an over-cap body is
// dropped (no usage recorded) rather than blocked.
func handleNonStreamingResponseBody(ctx wrapper.HttpContext, data []byte, endOfStream bool, maxBytes int) {
	if ctx.GetBoolContext(BufferExceededKey, false) {
		return
	}
	buf := ctx.GetByteSliceContext(NonStreamBodyBufferKey, nil)
	if len(buf)+len(data) > maxBytes {
		proxywasm.LogWarnf("handleNonStreamingResponseBody: response body exceeded %d bytes; stopping buffering to prevent OOM, usage not recorded", maxBytes)
		ctx.SetContext(NonStreamBodyBufferKey, nil)
		ctx.SetContext(BufferExceededKey, true)
		return
	}
	buf = append(buf, data...)
	ctx.SetContext(NonStreamBodyBufferKey, buf)
	if !endOfStream {
		return
	}
	ctx.SetContext(NonStreamBodyBufferKey, nil)

	encoding := ctx.GetStringContext(ResponseContentEncodingKey, "")
	body, err := decodeResponseBody(buf, encoding, maxBytes)
	if err != nil {
		proxywasm.LogWarnf("handleNonStreamingResponseBody: cannot decode response body (content-encoding=%q): %v; usage not recorded", encoding, err)
		return
	}
	usage := tokenusage.GetTokenUsage(ctx, body)
	if usage.TotalToken > 0 {
		ctx.SetContext(FinalUsageKey, usage)
	}
}

// decodeResponseBody returns the plaintext body for the given content-encoding.
// Supports identity (empty/"identity") and gzip; any other encoding is an
// error. The gzip path is bounded by maxBytes via an io.LimitReader so a
// highly-compressed zip bomb cannot inflate without limit; a body that decodes
// past the cap is an error (usage simply not recorded). Pure (no host calls)
// so it is unit-testable without a proxy-wasm host.
func decodeResponseBody(body []byte, contentEncoding string, maxBytes int) ([]byte, error) {
	switch enc := strings.ToLower(strings.TrimSpace(contentEncoding)); {
	case enc == "" || enc == "identity":
		return body, nil
	case strings.Contains(enc, "gzip"):
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		// Read one byte past the cap so an exactly-at-cap body still succeeds
		// while anything larger is detected as overflow.
		out, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
		if err != nil {
			return nil, err
		}
		if len(out) > maxBytes {
			return nil, fmt.Errorf("decoded body exceeds %d bytes (possible zip bomb)", maxBytes)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", contentEncoding)
	}
}

// onHttpStreamDone is the single emission point for the metrics report.
// proxy-wasm fires this hook regardless of how the stream ended — clean
// completion, upstream 5xx, downstream client disconnect, or filter-chain
// reset — so even mid-stream cancels produce a report (with completed=false
// and best-effort token data captured up to the disconnect). reportMetrics
// is idempotent via MetricsReportedKey so this is safe to call alongside
// any earlier emission paths if they get reintroduced later.
func onHttpStreamDone(ctx wrapper.HttpContext, config PluginConfig) {
	if !ctx.GetBoolContext(MetricsTrackingKey, false) {
		return
	}
	reportMetrics(ctx, config)
}

func processTokenUsage(ctx wrapper.HttpContext, data []byte, maxBufferBytes int) []byte {
	requestStartTime, ok := ctx.GetContext(StatisticsRequestStartTime).(int64)
	if !ok {
		return data
	}
	if ctx.GetContext(StatisticsFirstTokenTime) == nil {
		firstTokenTime := time.Now().UnixMilli()
		ctx.SetContext(StatisticsFirstTokenTime, firstTokenTime)
		ctx.SetContext(TimeToFirstTokenDuration, firstTokenTime-requestStartTime)
		proxywasm.LogDebugf("processTokenUsage: firstTokenTime=%d, timeToFirstTokenDuration=%d", firstTokenTime, firstTokenTime-requestStartTime)
	}
	chunks := bytes.SplitSeq(wrapper.UnifySSEChunk(data), []byte("\n\n"))
	var rtn = [][]byte{}
	for chunk := range chunks {
		chunk = mergeLargeUsageChunks(ctx, chunk, maxBufferBytes)
		if chunk == nil {
			rtn = append(rtn, []byte(""))
			continue
		}
		// An SSE event block may carry an "event:"/"id:"/comment prefix ahead
		// of its "data:" payload (OpenAI Responses API, e.g.
		// "event: response.completed\ndata: {...}"). Split the data payload out
		// for inspection while keeping the prefix so it can be re-emitted.
		prefix, payload, hasData := splitSSEEvent(chunk)
		if !hasData || !json.Valid(payload) {
			rtn = append(rtn, chunk)
			continue
		}

		// Always-on per-chunk observation: count delta chunks (downstream
		// uses this as fallback output-token estimator) and greedily capture
		// Anthropic message_start usage so a mid-stream cancel still keeps
		// input_tokens / cache token data on the report.
		countOutputDeltaChunk(ctx, payload)
		captureAnthropicMessageStart(ctx, payload)

		// Usage lives at top-level "usage" (OpenAI Chat Completions, Anthropic
		// message_delta) or at "response.usage" (OpenAI Responses API). Empty
		// path means this chunk carries no usage object.
		usagePath := usageJSONPath(payload)
		if usagePath == "" {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(SeenUsageChunk, true)
		proxywasm.LogDebugf("processTokenUsage: valid chunk: %s", string(payload))
		usageExtra := getUsageExtra(ctx, payload)
		if usageExtra == nil {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(ProcessedUsageChunk, true)

		// Strip OpenAI usage-only chunk when the user originally set
		// include_usage:false. FinalUsageKey was already populated by
		// getUsageExtra above, so the report still has the data; the
		// client just doesn't see the chunk it didn't ask for.
		if ctx.GetBoolContext(StripUsageChunkKey, false) && usagePath == "usage" && isOpenAIUsageOnlyChunk(payload) {
			proxywasm.LogDebugf("processTokenUsage: stripping OpenAI usage chunk (client requested include_usage=false)")
			continue
		}

		modified := process_data_with_token(payload, usagePath, usageExtra)
		proxywasm.LogDebugf("processTokenUsage: modified: %s", string(modified))
		rtn = append(rtn, reassembleSSEEvent(prefix, modified))
	}
	return bytes.Join(rtn, []byte("\n\n"))
}

// splitSSEEvent separates a single SSE event block (already newline-normalized
// by UnifySSEChunk, so only "\n" line endings) into its non-data prefix lines
// (event:/id:/retry:/comment) and the concatenated data payload. Per the SSE
// spec, a "data:" field strips one optional leading space and multiple "data:"
// lines in one event join with "\n". hasData is false when the block carries no
// data field at all — an SSE control line, a keep-alive comment, or a non-SSE
// body such as a JSON rate-limit rejection — in which case the caller forwards
// the block verbatim. Pure (no host calls) so it is unit-testable.
func splitSSEEvent(block []byte) (prefix []byte, payload []byte, hasData bool) {
	lines := bytes.Split(block, []byte("\n"))
	var prefixLines, dataLines [][]byte
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			v := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
			dataLines = append(dataLines, v)
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		prefixLines = append(prefixLines, line)
	}
	if len(dataLines) == 0 {
		return block, nil, false
	}
	return bytes.Join(prefixLines, []byte("\n")), bytes.Join(dataLines, []byte("\n")), true
}

// reassembleSSEEvent rebuilds an SSE event block from its prefix lines and a
// (possibly rewritten) data payload. With no prefix it is the plain
// "data: <payload>" shape used by OpenAI Chat Completions / Anthropic; with a
// prefix (e.g. an "event:" line) the prefix is preserved so Responses-API
// clients that dispatch on the event type still work. A payload that splitSSEEvent
// joined from multiple "data:" lines (carrying literal newlines) is re-split so
// each line gets its own "data: " prefix — emitting one "data:" line with
// embedded newlines would violate the SSE framing and break downstream parsers.
func reassembleSSEEvent(prefix, payload []byte) []byte {
	var dataPart []byte
	if bytes.Contains(payload, []byte("\n")) {
		lines := bytes.Split(payload, []byte("\n"))
		dataLines := make([][]byte, 0, len(lines))
		for _, line := range lines {
			dataLines = append(dataLines, append([]byte("data: "), line...))
		}
		dataPart = bytes.Join(dataLines, []byte("\n"))
	} else {
		dataPart = append([]byte("data: "), payload...)
	}
	if len(prefix) == 0 {
		return dataPart
	}
	out := make([]byte, 0, len(prefix)+len(dataPart)+1)
	out = append(out, prefix...)
	out = append(out, '\n')
	out = append(out, dataPart...)
	return out
}

// usageJSONPath returns the gjson/sjson path at which the usage object lives in
// this chunk, or "" when none is present. OpenAI Chat Completions, vLLM and
// Anthropic message_delta expose it at top-level "usage"; the OpenAI Responses
// API nests it under the response envelope at "response.usage".
func usageJSONPath(payload []byte) string {
	if gjson.GetBytes(payload, "usage").Exists() {
		return "usage"
	}
	if gjson.GetBytes(payload, "response.usage").Exists() {
		return "response.usage"
	}
	return ""
}

// countOutputDeltaChunk increments OutputChunkCountKey when this chunk
// carries non-empty generated content. Detects:
//   - OpenAI streaming: choices[*].delta with non-empty content / tool_calls
//     / function_call. The final usage-only chunk has choices=[] and is
//     therefore correctly excluded.
//   - Anthropic streaming: type="content_block_delta" with non-empty
//     delta.text / delta.partial_json / delta.thinking.
func countOutputDeltaChunk(ctx wrapper.HttpContext, data []byte) {
	if !isOutputDeltaChunk(data) {
		return
	}
	prev, _ := ctx.GetContext(OutputChunkCountKey).(int64)
	ctx.SetContext(OutputChunkCountKey, prev+1)
}

func isOutputDeltaChunk(data []byte) bool {
	if choices := gjson.GetBytes(data, "choices"); choices.IsArray() {
		hit := false
		choices.ForEach(func(_, c gjson.Result) bool {
			delta := c.Get("delta")
			if !delta.Exists() {
				return true
			}
			if content := delta.Get("content"); content.Exists() && content.String() != "" {
				hit = true
				return false
			}
			if tc := delta.Get("tool_calls"); tc.IsArray() && len(tc.Array()) > 0 {
				hit = true
				return false
			}
			if fc := delta.Get("function_call"); fc.Exists() {
				hit = true
				return false
			}
			return true
		})
		if hit {
			return true
		}
	}
	if gjson.GetBytes(data, "type").String() == "content_block_delta" {
		delta := gjson.GetBytes(data, "delta")
		if text := delta.Get("text"); text.Exists() && text.String() != "" {
			return true
		}
		if pj := delta.Get("partial_json"); pj.Exists() && pj.String() != "" {
			return true
		}
		if th := delta.Get("thinking"); th.Exists() && th.String() != "" {
			return true
		}
	}
	// OpenAI Responses API streaming: incremental output arrives as events
	// whose type ends in ".delta" (response.output_text.delta,
	// response.function_call_arguments.delta, response.reasoning_summary_text.delta,
	// ...) carrying a non-empty string "delta". Non-delta lifecycle events
	// (response.output_item.added, response.completed, ...) are excluded.
	if t := gjson.GetBytes(data, "type").String(); strings.HasSuffix(t, ".delta") {
		if d := gjson.GetBytes(data, "delta"); d.Type == gjson.String && d.String() != "" {
			return true
		}
	}
	return false
}

// captureAnthropicMessageStart eagerly stores the message_start usage block
// in FinalUsageKey. Anthropic's message_start is the first SSE event and
// already carries input_tokens / cache_*_input_tokens; capturing it early
// means a mid-stream client disconnect still leaves the report with
// trustworthy input-side accounting. Subsequent message_delta usage events
// (which carry the final output_tokens) overwrite this naturally.
func captureAnthropicMessageStart(ctx wrapper.HttpContext, data []byte) {
	if gjson.GetBytes(data, "type").String() != "message_start" {
		return
	}
	if !gjson.GetBytes(data, "message.usage").Exists() {
		return
	}
	usage := tokenusage.GetTokenUsage(ctx, data)
	if usage.InputToken > 0 || usage.AnthropicCacheReadInputToken > 0 || usage.AnthropicCacheCreationInputToken > 0 {
		ctx.SetContext(FinalUsageKey, usage)
	}
}

// isOpenAIUsageOnlyChunk identifies the OpenAI/vLLM final usage chunk —
// choices is an empty array and usage is populated. Used to gate stripping
// when the client originally set stream_options.include_usage:false.
func isOpenAIUsageOnlyChunk(data []byte) bool {
	choices := gjson.GetBytes(data, "choices")
	if !choices.IsArray() {
		return false
	}
	if len(choices.Array()) != 0 {
		return false
	}
	return gjson.GetBytes(data, "usage").Exists()
}

// parseConsumerHeader parses x-mse-consumer value of the form [access_key.]gpustack-<user_id>.
func parseConsumerHeader(consumer string) (userID *int64, accessKey *string) {
	if consumer == "" || strings.EqualFold(consumer, "none") {
		return
	}
	const prefix = "gpustack-"
	const sep = "." + prefix
	if idx := strings.LastIndex(consumer, sep); idx >= 0 {
		ak := consumer[:idx]
		if ak != "" {
			accessKey = &ak
		}
		if id, err := strconv.ParseInt(consumer[idx+len(sep):], 10, 64); err == nil {
			userID = &id
		}
	} else if strings.HasPrefix(consumer, prefix) {
		if id, err := strconv.ParseInt(consumer[len(prefix):], 10, 64); err == nil {
			userID = &id
		}
	} else {
		accessKey = &consumer
	}
	return
}

// parseRouteName extracts the numeric AI route id from a Higress route_name.
// Formats: "ai-route-route-<id>.internal" or "ai-route-route-<id>.fallback.internal".
// The dot-suffix is optional (mirrors parseClusterName's provider branch) so a
// bare "ai-route-route-<id>" is also accepted.
func parseRouteName(routeName string) *int64 {
	const prefix = "ai-route-route-"
	if !strings.HasPrefix(routeName, prefix) {
		return nil
	}
	idStr := routeName[len(prefix):]
	if dotIdx := strings.Index(idStr, "."); dotIdx != -1 {
		idStr = idStr[:dotIdx]
	}
	if idStr == "" {
		return nil
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

// parseClusterName extracts modelID or providerID from an Envoy cluster name.
// Envoy format: "outbound|<port>|<subset>|<fqdn>" where fqdn is "model-<id>-<instance-id>[.suffix]"
// or "provider-<id>[.suffix]". The optional suffix is .static or .dns.
func parseClusterName(clusterName string) (modelID *int64, providerID *int64) {
	// Extract the FQDN (4th field) from Envoy's "outbound|port|subset|fqdn" format.
	name := clusterName
	if parts := strings.SplitN(clusterName, "|", 4); len(parts) == 4 {
		name = parts[3]
	}

	if strings.HasPrefix(name, "model-") {
		rest := name[len("model-"):]
		idx := strings.Index(rest, "-")
		if idx > 0 {
			if id, err := strconv.ParseInt(rest[:idx], 10, 64); err == nil {
				return &id, nil
			}
		}
	} else if strings.HasPrefix(name, "provider-") {
		rest := name[len("provider-"):]
		// Strip optional suffix (.static, .dns, etc.) before parsing the ID.
		if dotIdx := strings.Index(rest, "."); dotIdx > 0 {
			rest = rest[:dotIdx]
		}
		if !strings.Contains(rest, "-") {
			if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
				return nil, &id
			}
		}
	}
	return nil, nil
}

func reportMetrics(ctx wrapper.HttpContext, config PluginConfig) {
	if config.ReportClient == nil {
		return
	}
	if ctx.GetBoolContext(MetricsReportedKey, false) {
		return
	}
	// Skip local-reply responses generated by other filters (e.g.
	// gpustack-rate-limit's 429, route_not_found, direct_response). Those
	// requests never reached an LLM so they don't belong in the token-usage
	// account; the originating filter emits its own metric.
	//
	// We use upstream.address as the discriminator rather than
	// response_code_details: filter ordering puts token-usage *before*
	// rate-limit in the response phase, and response_code_details is
	// finalized by Envoy at stream destruction (after our onHttpStreamDone
	// runs), so it's empty when we'd want to read it. upstream.address is
	// set the moment Envoy opens the upstream connection and is therefore
	// reliably populated for any request that actually reached an LLM.
	// Upstream-origin 4xx/5xx keep upstream.address populated and are
	// still reported (real LLM-bound traffic, even if it failed).
	if addr, err := proxywasm.GetProperty([]string{"upstream", "address"}); err != nil || len(addr) == 0 {
		proxywasm.LogDebugf("reportMetrics: skipping local-reply (no upstream connection)")
		return
	}
	base, ok := ctx.GetContext(BaseMetricsKey).(*ModelUsageMetrics)
	if !ok {
		proxywasm.LogDebugf("reportMetrics: no base metrics, skipping")
		return
	}

	clusterNameBytes, err := proxywasm.GetProperty([]string{"cluster_name"})
	if err != nil || len(clusterNameBytes) == 0 {
		proxywasm.LogDebugf("reportMetrics: no cluster_name, skipping")
		return
	}
	clusterName := string(clusterNameBytes)
	modelID, providerID := parseClusterName(clusterName)
	if modelID == nil && providerID == nil {
		proxywasm.LogDebugf("reportMetrics: cluster_name %s does not match expected pattern, skipping", clusterName)
		return
	}
	ctx.SetContext(MetricsReportedKey, true)

	var modelRouteID *int64
	if routeNameBytes, err := proxywasm.GetProperty([]string{"route_name"}); err == nil && len(routeNameBytes) > 0 {
		modelRouteID = parseRouteName(string(routeNameBytes))
	}

	usage, _ := ctx.GetContext(FinalUsageKey).(tokenusage.TokenUsage)
	model := base.Model
	if model == "" {
		model = usage.Model
	}
	startedAt, _ := ctx.GetContext(MetricsStartedAtKey).(int64)
	chunkCount, _ := ctx.GetContext(OutputChunkCountKey).(int64)
	contentBytes, _ := ctx.GetContext(RequestContentBytesKey).(int64)
	// Completed reflects whether the response reached its normal terminus,
	// independently of whether any tokens were emitted. This decouples
	// non-LLM endpoints (TTS/image — token fields legitimately 0) from
	// interrupted LLM streams (token fields also 0 but request did not
	// finish). Set by onStreamingResponseBody when endOfStream=true and by
	// the response-headers fast path on 2xx upstream status.
	completed := ctx.GetBoolContext(ResponseCompletedKey, false)
	metrics := ModelUsageMetrics{
		Model:               model,
		InputToken:          usage.InputToken,
		OutputToken:         usage.OutputToken,
		TotalToken:          usage.TotalToken,
		InputCachedToken:    resolveInputCachedToken(usage),
		RequestCount:        base.RequestCount,
		Completed:           completed,
		OutputChunkCount:    chunkCount,
		RequestContentBytes: contentBytes,
		StartedAt:           startedAt,
		CompletedAt:         time.Now().UnixMilli(),
		UserID:              base.UserID,
		AccessKey:           base.AccessKey,
		OrganizationID:      base.OrganizationID,
		ModelID:             modelID,
		ModelRouteID:        modelRouteID,
		ProviderID:          providerID,
	}

	body, err := json.Marshal(metrics)
	if err != nil {
		proxywasm.LogErrorf("reportMetrics: failed to marshal metrics: %v", err)
		return
	}

	reportHeaders := [][2]string{{"content-type", "application/json"}}
	for k, v := range config.HeaderAdd {
		reportHeaders = append(reportHeaders, [2]string{k, v})
	}

	path := config.Endpoint.Path
	if path == "" {
		path = "/"
	}

	if err = config.ReportClient.Post(
		path,
		reportHeaders,
		body,
		func(statusCode int, _ http.Header, _ []byte) {
			if statusCode/100 != 2 {
				proxywasm.LogWarnf("reportMetrics: unexpected status %d for route %s", statusCode, clusterName)
			} else {
				proxywasm.LogDebugf("reportMetrics: reported for route %s, status=%d", clusterName, statusCode)
			}
		},
		config.Endpoint.TimeoutMs,
	); err != nil {
		proxywasm.LogErrorf("reportMetrics: dispatch failed for route %s: %v", clusterName, err)
	}
}

// resolveInputCachedToken returns cached input (prompt) tokens across upstream
// formats: OpenAI/vLLM expose them in usage.prompt_tokens_details.cached_tokens;
// Anthropic reports cache hits separately via cache_read_input_tokens
// (cache_creation is new tokens being written, not a hit, so it is excluded).
func resolveInputCachedToken(usage tokenusage.TokenUsage) int64 {
	return usage.InputTokenDetails["cached_tokens"] + usage.AnthropicCacheReadInputToken
}

// process_data_with_token injects the computed usage extras (TTFT / TPOT / TPS)
// into the usage object of the data payload. usagePath is the JSON path of the
// usage object resolved by usageJSONPath ("usage" for OpenAI Chat Completions /
// Anthropic, "response.usage" for the OpenAI Responses API), so the extras land
// in the right place regardless of the response shape.
func process_data_with_token(payload []byte, usagePath string, usageExtra map[string]any) []byte {
	var err error
	var rtn = string(payload)
	for path, value := range usageExtra {
		var new_data string
		new_data, err = sjson.Set(rtn, fmt.Sprintf("%s.%s", usagePath, path), value)
		if err != nil {
			continue
		}
		rtn = new_data
	}
	return []byte(rtn)
}

func getUsageExtra(ctx wrapper.HttpContext, data []byte) map[string]any {
	var usageExtraInfo map[string]any
	extra := ctx.GetContext(UsageExtraKey)
	if extra != nil {
		return extra.(map[string]any)
	}
	usage := tokenusage.GetTokenUsage(ctx, data)
	if usage.TotalToken == 0 {
		return nil
	}
	proxywasm.LogDebugf("onStreamingResponseBody: token usage: total=%d, output=%d", usage.TotalToken, usage.OutputToken)
	firstTokenTime := ctx.GetContext(StatisticsFirstTokenTime).(int64)
	if firstTokenTime == 0 {
		return nil
	}

	ctx.SetContext(FinalUsageKey, usage)

	responseEndTime := time.Now().UnixMilli()
	outputTokenDuration := responseEndTime - firstTokenTime
	timeToFirstTokenDuration := ctx.GetContext(TimeToFirstTokenDuration).(int64)
	proxywasm.LogDebugf("onStreamingResponseBody: responseEndTime=%d, outputTokenDuration=%d, timeToFirstTokenDuration=%d", responseEndTime, outputTokenDuration, timeToFirstTokenDuration)
	var timePerOutputToken float64 = 0
	if usage.OutputToken > 1 {
		timePerOutputToken = float64(outputTokenDuration) / float64(usage.OutputToken-1)
	}
	var tokensPerSecond float64 = 0
	if outputTokenDuration > 0 {
		tokensPerSecond = float64(usage.OutputToken-1) / (float64(outputTokenDuration) / 1000)
	}

	usageExtraInfo = map[string]any{
		"time_to_first_token_ms":   timeToFirstTokenDuration,
		"time_per_output_token_ms": math.Round(timePerOutputToken*100) / 100,
		"tokens_per_second":        math.Round(tokensPerSecond*100) / 100,
	}
	ctx.SetContext(UsageExtraKey, usageExtraInfo)
	return usageExtraInfo
}

// mergeLargeUsageChunks reassembles a single SSE event whose data payload was
// split across Envoy delivery chunks. A large event (e.g. the Responses API's
// "response.completed", which embeds the whole response object) frequently
// arrives in fragments; the usage tail then lands in a chunk that, on its own,
// is not valid JSON. This buffers the raw block — event-type prefix included —
// until the data payload parses as JSON, then returns the complete block.
//
// Returns nil while still accumulating (caller emits an empty placeholder).
// Forwards verbatim any block that is already complete, empty, or carries no
// "data:" field at all (SSE control lines, keep-alive comments, or a non-SSE
// body such as a JSON rate-limit rejection — never buffered, so the client
// still receives it).
func mergeLargeUsageChunks(ctx wrapper.HttpContext, chunk []byte, maxBufferBytes int) []byte {
	buffered := ctx.GetByteSliceContext(IncompleteChunkData, nil)

	// Bound the reassembly buffer the same way handleNonStreamingResponseBody
	// bounds its accumulator: a malformed or never-completing stream must not
	// grow the buffer without limit and OOM the WASM VM. On overflow, flush
	// whatever was accumulated (the client already received empty placeholders
	// for those segments) and stop reassembling — fail-open, usage for this
	// event simply goes unrecorded.
	if maxBufferBytes > 0 && len(buffered)+len(chunk) > maxBufferBytes {
		proxywasm.LogWarnf("mergeLargeUsageChunks: incomplete SSE buffer exceeded %d bytes; flushing and stopping reassembly (usage may be unrecorded)", maxBufferBytes)
		ctx.SetContext(IncompleteChunk, false)
		ctx.SetContext(IncompleteChunkData, nil)
		if len(buffered) == 0 {
			return chunk
		}
		return append(append([]byte(nil), buffered...), chunk...)
	}

	out, newBuffer := mergeSSEEventState(buffered, chunk)
	if len(newBuffer) > 0 {
		ctx.SetContext(IncompleteChunk, true)
		ctx.SetContext(IncompleteChunkData, newBuffer)
		proxywasm.LogDebugf("the delta is stored: %s", string(newBuffer))
	} else {
		ctx.SetContext(IncompleteChunk, false)
		ctx.SetContext(IncompleteChunkData, nil)
	}
	return out
}

// mergeSSEEventState is the pure core of mergeLargeUsageChunks: given the raw
// block buffered so far (nil/empty when not mid-event) and the next
// "\n\n"-delimited segment, it returns the block to emit (nil while still
// accumulating) and the buffer to carry into the next call. No ctx/host calls,
// so it is unit-testable.
//
//   - mid-accumulation (buffered non-empty): the segment is the raw
//     continuation of the previous event's data payload, appended verbatim;
//     emit once the reassembled payload is valid JSON.
//   - fresh segment with no "data:" field (SSE control line, comment, or a
//     non-SSE JSON body such as a rate-limit rejection): forward verbatim.
//   - fresh complete / empty data payload, or the "[DONE]" stream terminator
//     (a deliberately non-JSON sentinel): forward verbatim. Without the
//     "[DONE]" exception it would be mistaken for a truncated JSON fragment,
//     buffered forever, and the client would never see the end-of-stream mark.
//   - fresh truncated data payload: buffer the whole raw block (event-type
//     prefix included) so the prefix survives reassembly.
func mergeSSEEventState(buffered, chunk []byte) (out []byte, newBuffer []byte) {
	if len(buffered) > 0 {
		combined := append(append([]byte(nil), buffered...), chunk...)
		if _, payload, _ := splitSSEEvent(combined); json.Valid(payload) {
			return combined, nil
		}
		return nil, combined
	}
	_, payload, hasData := splitSSEEvent(chunk)
	trimmed := bytes.TrimSpace(payload)
	if !hasData || json.Valid(payload) || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return chunk, nil
	}
	return nil, append([]byte(nil), chunk...)
}
