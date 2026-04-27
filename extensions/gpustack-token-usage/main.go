package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
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

const (
	pluginName = "gpustack-token-usage"
)

const (
	IsStreamingResponse        = "is_streaming_response"
	StatisticsRequestStartTime = "gpustack_request_start_time"
	StatisticsFirstTokenTime   = "gpustack_first_token_time"
	TimeToFirstTokenDuration   = "gpustack_llm_first_token_duration"

	IncompleteChunk     = "gpustack_incomplete_chunk"
	IncompleteChunkData = "gpustack_incomplete_chunk_data"
	UsageExtraKey       = "gpustack_usage_extra"
	ModifiedKey         = "gpustack_usage_modified"
	SeenUsageChunk      = "gpustack_seen_usage_chunk"
	ProcessedUsageChunk = "gpustack_processed_usage_chunk"

	RequestModelKey        = "gpustack_request_model"
	FinalUsageKey          = "gpustack_final_usage"
	ProcessBodyKey         = "gpustack_process_body"
	InjectStreamOptionsKey = "gpustack_inject_stream_options"
	BaseMetricsKey         = "gpustack_base_metrics"
	MetricsTrackingKey     = "gpustack_metrics_tracking"
	RequestHeadersKey      = "gpustack_headers"
	MultipartContentType   = "gpustack_multipart_content_type"
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
	RealIPToHeader          string
	EnableOnPathSuffix      []string
	EnableUsageOnPathSuffix []string
	Endpoint                *EndpointConfig
	HeaderAdd               map[string]string
	ReportClient            wrapper.HttpClient
}

// ModelUsageMetrics is the JSON payload sent to the metrics reporting endpoint.
type ModelUsageMetrics struct {
	Model        string  `json:"model"`
	InputToken   int64   `json:"input_token"`
	OutputToken  int64   `json:"output_token"`
	TotalToken   int64   `json:"total_token"`
	InputCachedToken int64 `json:"input_cached_token"`
	RequestCount int     `json:"request_count"`
	UserID       *int64  `json:"user_id,omitempty"`
	ModelID      *int64  `json:"model_id,omitempty"`
	ProviderID   *int64  `json:"provider_id,omitempty"`
	AccessKey    *string `json:"access_key,omitempty"`
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
	config.RealIPToHeader = json.Get("realIPToHeader").String()
	config.EnableUsageOnPathSuffix = buildPathSuffixes(json.Get("enableUsageOnPathSuffix"), []string{
		"/chat/completions",
		"/completions",
	})
	config.EnableOnPathSuffix = buildPathSuffixes(json.Get("enableOnPathSuffix"), []string{
		"/chat/completions",
		"/completions",
		"/responses",
		"/messages",
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

	return nil
}

// baseMediaType returns the media type without parameters (e.g. "application/json; charset=utf-8" → "application/json").
func baseMediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}

// getRealIPHeader returns the configured header name and the resolved client IP.
// Returns empty strings when not configured or the source address is unavailable.
func getRealIPHeader(config PluginConfig) (name, ip string) {
	if config.RealIPToHeader == "" {
		return
	}
	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		proxywasm.LogDebugf("getRealIPHeader: failed to get source address: %v", err)
		return
	}
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		host = string(data)
	}
	return config.RealIPToHeader, host
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
	// 1. Check if metrics tracking requires reading the request body.
	metricsNeedBody := prepareMetrics(ctx)

	// 2. Check if stream injection requires reading the request body.
	streamNeedBody := prepareStream(ctx, config)

	// 3. Neither needs the body: inject real IP header now and skip body read.
	if !metricsNeedBody && !streamNeedBody {
		if name, ip := getRealIPHeader(config); name != "" {
			_ = proxywasm.AddHttpRequestHeader(name, ip)
		}
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

func buildBaseMetrics(ctx wrapper.HttpContext) *ModelUsageMetrics {
	m := &ModelUsageMetrics{
		Model:        ctx.GetStringContext(RequestModelKey, ""),
		RequestCount: 1,
	}
	if headers, ok := ctx.GetContext(RequestHeadersKey).([][2]string); ok {
		for _, h := range headers {
			if strings.EqualFold(h[0], "x-mse-consumer") && h[1] != "" {
				m.UserID, m.AccessKey = parseConsumerHeader(h[1])
				break
			}
		}
	}
	return m
}

// processRequestBody extracts the model name, sets stream state, and optionally injects
// stream_options.include_usage. Returns the (possibly modified) headers slice.
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

	stream := gjson.GetBytes(body, "stream")
	if ctx.GetBoolContext(ProcessBodyKey, false) {
		ctx.SetContext(IsStreamingResponse, stream.Exists() && stream.Bool())
	}

	if !ctx.GetBoolContext(InjectStreamOptionsKey, false) {
		return headers
	}

	includeUsage := gjson.GetBytes(body, "stream_options.include_usage")
	if stream.Exists() && stream.Bool() && !includeUsage.Exists() {
		proxywasm.LogDebug("setting include_usage to request body")
		newBody, err := sjson.SetBytes(body, "stream_options.include_usage", true)
		if err != nil {
			proxywasm.LogErrorf("failed to set json body, %v", err)
		} else if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
			proxywasm.LogWarnf("failed to replace new body %s, %v", string(newBody), err)
		} else {
			headers = removeHeader("content-length", headers)
		}
	} else {
		proxywasm.LogDebug("no need to modify request body")
		if includeUsage.Exists() && !includeUsage.Bool() {
			ctx.SetContext(StatisticsRequestStartTime, nil)
		}
	}
	return headers
}

func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	proxywasm.LogDebug("processing request body")
	headers, ok := ctx.GetContext(RequestHeadersKey).([][2]string)
	if !ok {
		proxywasm.LogWarn("failed to get headers from context, skip process body")
		return types.ActionContinue
	}
	if name, ip := getRealIPHeader(config); name != "" {
		headers = append(headers, [2]string{name, ip})
	}
	headers = processRequestBody(ctx, body, headers)
	ctx.SetContext(BaseMetricsKey, buildBaseMetrics(ctx))
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
		reportMetrics(ctx, config)
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	isStreaming := ctx.GetBoolContext(IsStreamingResponse, false)
	if isStreaming {
		contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
		if strings.Contains(contentType, "application/json") {
			ctx.SetContext(IsStreamingResponse, false)
			return types.HeaderStopIteration
		}
		return types.ActionContinue
	}
	return types.HeaderStopIteration
}

func onStreamingResponseBody(ctx wrapper.HttpContext, config PluginConfig, data []byte, endOfStream bool) []byte {
	result := processTokenUsage(ctx, data)
	if endOfStream {
		if ctx.GetBoolContext(SeenUsageChunk, false) && !ctx.GetBoolContext(ProcessedUsageChunk, false) {
			proxywasm.LogWarnf("no usage is found in any chunk with usage bytes")
		}
		reportMetrics(ctx, config)
	}
	return result
}

func processTokenUsage(ctx wrapper.HttpContext, data []byte) []byte {
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
	isStreamingResponse := ctx.GetBoolContext(IsStreamingResponse, false)
	if !isStreamingResponse {
		dur, _ := ctx.GetContext(TimeToFirstTokenDuration).(int64)
		usage := tokenusage.GetTokenUsage(ctx, data)
		if usage.TotalToken > 0 {
			ctx.SetContext(FinalUsageKey, usage)
		}
		if dur <= 0 || usage.OutputToken == 0 {
			return data
		}
		tps := math.Round(float64(usage.OutputToken)/(float64(dur)/1000)*100) / 100
		newData, err := sjson.SetBytes(data, "usage.tokens_per_second", tps)
		if err != nil {
			return data
		}
		_ = proxywasm.ReplaceHttpResponseHeader("content-length", strconv.Itoa(len(newData)))
		return newData
	}

	chunks := bytes.SplitSeq(wrapper.UnifySSEChunk(data), []byte("\n\n"))
	var rtn = [][]byte{}
	for chunk := range chunks {
		if ctx.GetBoolContext(ModifiedKey, false) {
			rtn = append(rtn, chunk)
			continue
		}
		chunk = mergeLargeUsageChunks(ctx, chunk)
		if chunk == nil {
			rtn = append(rtn, []byte(""))
			continue
		}
		trimed_data := bytes.TrimPrefix(chunk, []byte("data: "))
		if !json.Valid(trimed_data) {
			rtn = append(rtn, chunk)
			continue
		}
		result := gjson.GetBytes(trimed_data, "usage")
		if !result.Exists() {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(SeenUsageChunk, true)
		proxywasm.LogDebugf("processTokenUsage: valid chunk: %s", string(trimed_data))
		usageExtra := getUsageExtra(ctx, trimed_data)
		if usageExtra == nil {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(ProcessedUsageChunk, true)
		modified := process_data_with_token(trimed_data, usageExtra)
		proxywasm.LogDebugf("processTokenUsage: modified: %s", string(modified))
		rtn = append(rtn, append([]byte("data: "), modified...))
		ctx.SetContext(ModifiedKey, true)
	}
	return bytes.Join(rtn, []byte("\n\n"))
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

	usage, _ := ctx.GetContext(FinalUsageKey).(tokenusage.TokenUsage)
	model := base.Model
	if model == "" {
		model = usage.Model
	}
	metrics := ModelUsageMetrics{
		Model:        model,
		InputToken:   usage.InputToken,
		OutputToken:  usage.OutputToken,
		TotalToken:   usage.TotalToken,
		InputCachedToken: resolveInputCachedToken(usage),
		RequestCount: base.RequestCount,
		UserID:       base.UserID,
		AccessKey:    base.AccessKey,
		ModelID:      modelID,
		ProviderID:   providerID,
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

func process_data_with_token(data []byte, usageExtra map[string]any) []byte {
	var err error
	var rtn = string(bytes.TrimPrefix(data, []byte("data: ")))
	for path, value := range usageExtra {
		var new_data string
		new_data, err = sjson.Set(rtn, fmt.Sprintf("usage.%s", path), value)
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

func mergeLargeUsageChunks(ctx wrapper.HttpContext, chunk []byte) []byte {
	trimed_data := bytes.TrimPrefix(chunk, []byte("data: "))
	if json.Valid(trimed_data) {
		ctx.SetContext(IncompleteChunk, false)
		return chunk
	}
	if len(bytes.TrimSpace(trimed_data)) == 0 {
		return chunk
	}
	ctx.SetContext(IncompleteChunk, true)
	deltaMessage := ctx.GetByteSliceContext(IncompleteChunkData, []byte{})
	trimed_data = append(deltaMessage, trimed_data...)
	proxywasm.LogDebugf("the delta is stored: %s", string(trimed_data))

	if !json.Valid(trimed_data) {
		ctx.SetContext(IncompleteChunkData, trimed_data)
		return nil
	}
	ctx.SetContext(IncompleteChunk, false)
	ctx.SetContext(IncompleteChunkData, nil)
	return append([]byte("data: "), trimed_data...)
}
