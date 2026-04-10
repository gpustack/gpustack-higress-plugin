package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
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

// Constants for log keys in Filter State
const (
	pluginName = "gpustack-token-usage"
)

// Constants for context keys
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
)

func main() {}

func init() {
	wrapper.SetCtx(
		// Plugin name
		pluginName,
		// Set custom function for parsing plugin configuration
		wrapper.ParseConfig(parseConfig),
		// Set custom function for processing request headers
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		// Set custom function for processing request body
		wrapper.ProcessRequestBody(onHttpRequestBody),
		// Set custom function for processing response headers
		wrapper.ProcessResponseHeaders(onHttpResponseHeaders),
		// Set custom function for processing streaming response body
		wrapper.ProcessStreamingResponseBody(onStreamingResponseBody),
	)
}

// PluginConfig Custom plugin configuration
type PluginConfig struct {
	RealIPToHeader     string
	EnableOnPathSuffix []string
}

func (c *PluginConfig) shouldProcess(targetURI string) bool {
	// check target uri is vaild or not
	u, err := url.ParseRequestURI(targetURI)
	if err != nil {
		proxywasm.LogDebugf("shouldProcess: invalid targetURI: %s", targetURI)
		return false
	}
	// filterred by path suffix
	path := u.Path
	for _, suffix := range c.EnableOnPathSuffix {
		if len(suffix) > 0 && len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			proxywasm.LogDebugf("shouldProcess: matched suffix %s for path %s", suffix, path)
			return true
		}
	}
	return false
}

// The YAML configuration filled in the console will be automatically converted to JSON,
// so we can directly parse the configuration from this JSON parameter
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
	return nil
}

func realIpHandler(_ wrapper.HttpContext, headerName string) map[string]string {
	var (
		realIpStr string
	)
	// Get all request headers
	if headerName == "" {
		return nil
	}

	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		proxywasm.LogDebugf("failed to get remote address, %s", err)
		return nil
	}
	// Only keeps the host without port
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		realIpStr = string(data)
	} else {
		realIpStr = host
	}

	return map[string]string{headerName: realIpStr}
}

// onHttpRequestHeaders processes the request headers and logs them if enabled
func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	action := types.ActionContinue
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	if config.shouldProcess(ctx.Path()) && contentType == "application/json" {
		hs, err := proxywasm.GetHttpRequestHeaders()
		if err != nil {
			proxywasm.LogWarnf("failed to get request headers, skip handling body, %v", err)
			return action
		}
		// request start time is used by Time to First Token
		ctx.SetContext(StatisticsRequestStartTime, time.Now().UnixMilli())
		// only read the body if content-type is application/json
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

// onHttpRequestBody is called if the request is openai completions API like /v1/chat/completions
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
	stream := gjson.GetBytes(body, "stream")
	ctx.SetContext(IsStreamingResponse, stream.Exists() && stream.Bool())
	includeUsage := gjson.GetBytes(body, "stream_options.include_usage")
	if stream.Exists() && stream.Bool() && !includeUsage.Exists() {
		proxywasm.LogDebug("setting include_usage to request body")
		newBody, err := sjson.SetBytes(body, "stream_options.include_usage", true)
		if err != nil {
			proxywasm.LogErrorf("failed to set json body, %v", err)
		} else if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
			// replace body failed
			proxywasm.LogWarnf("failed to replace new body %s, %v", string(newBody), err)
		} else {
			// set succeed and replace succeed
			headers = removeHeader("content-length", headers)
		}
	} else {
		proxywasm.LogDebug("no need to modify request body")
		if includeUsage.Exists() && !includeUsage.Bool() {
			// specify not to include usage
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
		// Edge case: request said stream=true, but server responded with non-streaming JSON (e.g., error)
		// Trust the response content-type over the request parameter
		contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
		if strings.Contains(contentType, "application/json") {
			ctx.SetContext(IsStreamingResponse, false)
			return types.HeaderStopIteration
		}
		return types.ActionContinue
	}
	return types.HeaderStopIteration
}

// Requires to calculate time_to_first_token_ms, time_per_output_token_ms and tokens_per_second.
func onStreamingResponseBody(ctx wrapper.HttpContext, config PluginConfig, data []byte, endOfStream bool) []byte {
	// Get requestStartTime from http context
	requestStartTime, ok := ctx.GetContext(StatisticsRequestStartTime).(int64)
	if !ok {
		return data
	}
	// If this is the first chunk, record first token duration metric and span attribute
	if ctx.GetContext(StatisticsFirstTokenTime) == nil {
		firstTokenTime := time.Now().UnixMilli()
		ctx.SetContext(StatisticsFirstTokenTime, firstTokenTime)
		ctx.SetContext(TimeToFirstTokenDuration, firstTokenTime-requestStartTime)
		proxywasm.LogDebugf("onStreamingResponseBody: firstTokenTime=%d, timeToFirstTokenDuration=%d", firstTokenTime, firstTokenTime-requestStartTime)
	}
	isStreamingResponse := ctx.GetBoolContext(IsStreamingResponse, false)
	if !isStreamingResponse {
		// TTFT and TPOT are streaming-specific; only inject TPS for non-streaming.
		// TPS = output_tokens / total_response_time_seconds
		dur, ok := ctx.GetContext(TimeToFirstTokenDuration).(int64)
		if !ok || dur <= 0 {
			return data
		}
		usage := tokenusage.GetTokenUsage(ctx, data)
		if usage.OutputToken == 0 {
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
			// to avoid the two chunk didn't join with \n\n
			rtn = append(rtn, []byte(""))
			continue
		}
		trimed_data := bytes.TrimPrefix(chunk, []byte("data: "))
		if !json.Valid(trimed_data) {
			// if is part of the json
			rtn = append(rtn, chunk)
			continue
		}
		result := gjson.GetBytes(trimed_data, "usage")
		// no usage found
		if !result.Exists() {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(SeenUsageChunk, true)
		proxywasm.LogDebugf("valid chunk: %s", string(trimed_data))
		usageExtra := getUsageExtra(ctx, trimed_data)
		if usageExtra == nil {
			rtn = append(rtn, chunk)
			continue
		}
		ctx.SetContext(ProcessedUsageChunk, true)
		modified := process_data_with_token(trimed_data, usageExtra)
		proxywasm.LogDebugf("modified: %s", string(modified))
		rtn = append(rtn, append([]byte("data: "), modified...))
		ctx.SetContext(ModifiedKey, true)
	}
	result := bytes.Join(rtn, []byte("\n\n"))
	// At the end of stream, log if we saw usage chunks but failed to process any
	if endOfStream && ctx.GetBoolContext(SeenUsageChunk, false) && !ctx.GetBoolContext(ProcessedUsageChunk, false) {
		proxywasm.LogWarnf("no usage is found in any chunk with usage bytes")
	}
	return result
}

func process_data_with_token(data []byte, usageExtra map[string]any) []byte {
	var err error
	// trim data: prefix
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
	// alread has extra info stored
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
	// end of streaming
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
