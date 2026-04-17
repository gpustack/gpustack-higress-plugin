package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
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

	RequestModelKey  = "gpustack_request_model"
	RequestAccessKey = "gpustack_request_access_key"
	RequestUserIDKey = "gpustack_request_user_id"
	FinalUsageKey    = "gpustack_final_usage"
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
	ServiceName    string
	ServicePort    int64
	Path           string
	TimeoutMs      uint32
}

// PluginConfig holds plugin configuration.
type PluginConfig struct {
	RealIPToHeader     string
	EnableOnPathSuffix []string
	Endpoint           *EndpointConfig
	HeaderAdd          map[string]string
	ReportClient       wrapper.HttpClient
}

// ModelUsageMetrics is the JSON payload sent to the metrics reporting endpoint.
type ModelUsageMetrics struct {
	Model        string  `json:"model"`
	InputToken   int64   `json:"input_token"`
	OutputToken  int64   `json:"output_token"`
	TotalToken   int64   `json:"total_token"`
	RequestCount int     `json:"request_count"`
	UserID       *int64  `json:"user_id,omitempty"`
	ModelID      *int64  `json:"model_id,omitempty"`
	ProviderID   *int64  `json:"provider_id,omitempty"`
	AccessKey    *string `json:"access_key,omitempty"`
}

func (c *PluginConfig) shouldProcess(targetURI string) bool {
	u, err := url.ParseRequestURI(targetURI)
	if err != nil {
		proxywasm.LogDebugf("shouldProcess: invalid targetURI: %s", targetURI)
		return false
	}
	path := u.Path
	for _, suffix := range c.EnableOnPathSuffix {
		if len(suffix) > 0 && len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			proxywasm.LogDebugf("shouldProcess: matched suffix %s for path %s", suffix, path)
			return true
		}
	}
	return false
}

func parseConfig(json gjson.Result, config *PluginConfig) error {
	config.RealIPToHeader = json.Get("realIPToHeader").String()
	suffixes := json.Get("enableOnPathSuffix").Array()
	defaultSuffixes := map[string]bool{
		"/chat/completions": true,
		"/completions":      true,
		"/responses":        true,
		"/messages":         true,
	}
	for _, suffix := range suffixes {
		path := suffix.String()
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			proxywasm.LogDebugf("onParseConfig: %s is not a valid path suffix (must start with /), skipping", path)
			continue
		}
		defaultSuffixes[path] = true
	}
	config.EnableOnPathSuffix = []string{}
	for path := range defaultSuffixes {
		config.EnableOnPathSuffix = append(config.EnableOnPathSuffix, path)
	}

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

func realIpHandler(_ wrapper.HttpContext, headerName string) map[string]string {
	var (
		realIpStr string
	)
	if headerName == "" {
		return nil
	}

	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		proxywasm.LogDebugf("failed to get remote address, %s", err)
		return nil
	}
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		realIpStr = string(data)
	} else {
		realIpStr = host
	}

	return map[string]string{headerName: realIpStr}
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	if consumer, _ := proxywasm.GetHttpRequestHeader("x-mse-consumer"); consumer != "" {
		userID, accessKey := parseConsumerHeader(consumer)
		if accessKey != nil {
			ctx.SetContext(RequestAccessKey, *accessKey)
		}
		if userID != nil {
			ctx.SetContext(RequestUserIDKey, *userID)
		}
	}

	action := types.ActionContinue
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	if config.shouldProcess(ctx.Path()) && contentType == "application/json" {
		hs, err := proxywasm.GetHttpRequestHeaders()
		if err != nil {
			proxywasm.LogWarnf("failed to get request headers, skip handling body, %v", err)
			return action
		}
		ctx.SetContext(StatisticsRequestStartTime, time.Now().UnixMilli())
		ctx.SetContext("headers", hs)
		action = types.HeaderStopIteration
	}

	if action == types.ActionContinue {
		headers := realIpHandler(ctx, config.RealIPToHeader)
		for key, value := range headers {
			_ = proxywasm.AddHttpRequestHeader(key, value)
		}
		ctx.DontReadRequestBody()
	}
	return action
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

func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	proxywasm.LogDebug("processing request body")
	headers, ok := ctx.GetContext("headers").([][2]string)
	if !ok {
		proxywasm.LogWarn("failed to get headers from context, skip process body")
		return types.ActionContinue
	}
	ipHeaders := realIpHandler(ctx, config.RealIPToHeader)
	for key, value := range ipHeaders {
		headers = append(headers, [2]string{key, value})
	}

	if model := gjson.GetBytes(body, "model").String(); model != "" {
		ctx.SetContext(RequestModelKey, model)
	}

	stream := gjson.GetBytes(body, "stream")
	ctx.SetContext(IsStreamingResponse, stream.Exists() && stream.Bool())
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
	_ = proxywasm.ReplaceHttpRequestHeaders(headers)
	return types.ActionContinue
}

func onHttpResponseHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	_, ok := ctx.GetContext(StatisticsRequestStartTime).(int64)
	if !ok {
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

	model := ctx.GetStringContext(RequestModelKey, "")
	if model == "" {
		model = usage.Model
	}

	metrics := ModelUsageMetrics{
		Model:        model,
		InputToken:   usage.InputToken,
		OutputToken:  usage.OutputToken,
		TotalToken:   usage.TotalToken,
		RequestCount: 1,
		ModelID:      modelID,
		ProviderID:   providerID,
	}
	if userID, ok := ctx.GetContext(RequestUserIDKey).(int64); ok {
		metrics.UserID = &userID
	}
	if accessKey := ctx.GetStringContext(RequestAccessKey, ""); accessKey != "" {
		metrics.AccessKey = &accessKey
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
