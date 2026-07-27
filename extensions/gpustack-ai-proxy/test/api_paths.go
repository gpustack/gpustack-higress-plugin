// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/c8b82797c51a97faca46e2ae12990453f5026802/plugins/wasm-go/extensions/ai-proxy/test/api_paths.go
// Forked into gpustack/gpustack-higress-plugins at higress commit c8b82797c51a.
// Local modifications may diverge from upstream; keep this attribution when editing.

package test

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	wasmtest "github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

func openAICustomEndpointConfig(customURL string) json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"provider": map[string]interface{}{
			"type":      "openai",
			"apiTokens": []string{"sk-openai-test-custom-endpoint"},
			"modelMapping": map[string]string{
				"*": "gpt-4o-mini",
			},
			"openaiCustomUrl": customURL,
		},
	})
	return data
}

var openAICustomAudioTranscriptionsEndpointConfig = openAICustomEndpointConfig("https://custom.openai.com/v1/audio/transcriptions")
var openAICustomAudioTranslationsEndpointConfig = openAICustomEndpointConfig("https://custom.openai.com/v1/audio/translations")
var openAICustomRealtimeEndpointConfig = openAICustomEndpointConfig("https://custom.openai.com/v1/realtime")
var openAICustomRealtimeSessionsEndpointConfig = openAICustomEndpointConfig("https://custom.openai.com/v1/realtime/sessions")

func RunApiPathRegressionTests(t *testing.T) {
	wasmtest.RunTest(t, func(t *testing.T) {
		t.Run("openai direct custom endpoint audio transcriptions", func(t *testing.T) {
			host, status := wasmtest.NewTestHost(openAICustomAudioTranscriptionsEndpointConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/audio/transcriptions"},
				{":method", "POST"},
				{"Content-Type", "application/json"},
			})
			require.Equal(t, types.HeaderStopIteration, action)

			requestHeaders := host.GetRequestHeaders()
			pathValue, hasPath := wasmtest.GetHeaderValue(requestHeaders, ":path")
			require.True(t, hasPath)
			require.Equal(t, "/v1/audio/transcriptions", pathValue)
		})

		t.Run("openai direct custom endpoint audio translations", func(t *testing.T) {
			host, status := wasmtest.NewTestHost(openAICustomAudioTranslationsEndpointConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/audio/translations"},
				{":method", "POST"},
				{"Content-Type", "application/json"},
			})
			require.Equal(t, types.HeaderStopIteration, action)

			requestHeaders := host.GetRequestHeaders()
			pathValue, hasPath := wasmtest.GetHeaderValue(requestHeaders, ":path")
			require.True(t, hasPath)
			require.Equal(t, "/v1/audio/translations", pathValue)
		})

		t.Run("openai direct custom endpoint realtime", func(t *testing.T) {
			host, status := wasmtest.NewTestHost(openAICustomRealtimeEndpointConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/realtime"},
				{":method", "GET"},
				{"Connection", "Upgrade"},
				{"Upgrade", "websocket"},
				{"Sec-WebSocket-Version", "13"},
				{"Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="},
			})
			require.True(t, action == types.ActionContinue || action == types.HeaderStopIteration)

			requestHeaders := host.GetRequestHeaders()
			pathValue, hasPath := wasmtest.GetHeaderValue(requestHeaders, ":path")
			require.True(t, hasPath)
			require.Equal(t, "/v1/realtime", pathValue)
		})

		t.Run("openai non-direct endpoint appends mapped realtime suffix", func(t *testing.T) {
			host, status := wasmtest.NewTestHost(openAICustomRealtimeSessionsEndpointConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			action := host.CallOnHttpRequestHeaders([][2]string{
				{":authority", "example.com"},
				{":path", "/v1/realtime"},
				{":method", "GET"},
				{"Connection", "Upgrade"},
				{"Upgrade", "websocket"},
				{"Sec-WebSocket-Version", "13"},
				{"Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="},
			})
			require.True(t, action == types.ActionContinue || action == types.HeaderStopIteration)

			requestHeaders := host.GetRequestHeaders()
			pathValue, hasPath := wasmtest.GetHeaderValue(requestHeaders, ":path")
			require.True(t, hasPath)
			require.Equal(t, "/v1/realtime/sessions/realtime", pathValue)
		})

	})
}
