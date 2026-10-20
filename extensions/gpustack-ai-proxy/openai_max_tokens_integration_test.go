// GPUStack-local test (no upstream counterpart): guards the max_tokens →
// max_completion_tokens normalization in the openai provider end to end.
//
// Per the OpenAI Chat Completions API spec, the o-series and gpt-5+ reasoning
// models reject the deprecated max_tokens parameter ("Unsupported parameter:
// 'max_tokens' is not supported with this model."), while legacy models keep
// accepting it — so the proxy rewrites per-target-model instead of pushing the
// distinction to callers. See gpustack/gpustack#6505.
//
// Runs on the in-process wasm host (test.RunTest) because the rewrite sits in
// openaiProvider.TransformRequestBody after defaultTransformRequestBody, whose
// proxywasm host calls are a nil-deref panic under plain `go test`.

package main

import (
	"encoding/json"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	wasmhost "github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var openaiMaxCompletionTokensConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"provider": map[string]interface{}{
			"type":      "openai",
			"apiTokens": []string{"sk-openai-max-completion-tokens"},
		},
	})
	return data
}()

// The mapped model name is what reaches OpenAI, so the rewrite must judge the
// mapped name, not the caller's original one.
var openaiMaxCompletionTokensMappedConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"provider": map[string]interface{}{
			"type":      "openai",
			"apiTokens": []string{"sk-openai-max-completion-tokens"},
			"modelMapping": map[string]string{
				"my-luna": "gpt-5.6-luna",
			},
		},
	})
	return data
}()

func openaiMaxTokensChatHeaders() [][2]string {
	return [][2]string{
		{":authority", "example.com"},
		{":path", "/v1/chat/completions"},
		{":method", "POST"},
		{"Content-Type", "application/json"},
	}
}

func TestOpenAIMaxCompletionTokensRewrite(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		t.Run("rewrites max_tokens for the issue's model", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(openaiMaxCompletionTokensConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(openaiMaxTokensChatHeaders())
			body := `{"model":"gpt-5.6-luna","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}],"stream":false}`
			action := host.CallOnHttpRequestBody([]byte(body))
			require.Equal(t, types.ActionContinue, action)

			processed := host.GetRequestBody()
			require.NotNil(t, processed)
			require.False(t, gjson.GetBytes(processed, "max_tokens").Exists(),
				"max_tokens must not reach OpenAI for gpt-5 family models: %s", processed)
			require.Equal(t, int64(1024), gjson.GetBytes(processed, "max_completion_tokens").Int())
			require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(processed, "model").String())
		})

		t.Run("keeps max_tokens for legacy models", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(openaiMaxCompletionTokensConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(openaiMaxTokensChatHeaders())
			body := `{"model":"gpt-4o","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
			action := host.CallOnHttpRequestBody([]byte(body))
			require.Equal(t, types.ActionContinue, action)

			processed := host.GetRequestBody()
			require.NotNil(t, processed)
			require.Equal(t, int64(1024), gjson.GetBytes(processed, "max_tokens").Int())
			require.False(t, gjson.GetBytes(processed, "max_completion_tokens").Exists())
		})

		t.Run("prefers explicit max_completion_tokens over max_tokens", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(openaiMaxCompletionTokensConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(openaiMaxTokensChatHeaders())
			body := `{"model":"o3-mini","max_tokens":100,"max_completion_tokens":200,"messages":[]}`
			action := host.CallOnHttpRequestBody([]byte(body))
			require.Equal(t, types.ActionContinue, action)

			processed := host.GetRequestBody()
			require.NotNil(t, processed)
			require.False(t, gjson.GetBytes(processed, "max_tokens").Exists(),
				"sending both parameters is rejected upstream, the deprecated one must go: %s", processed)
			require.Equal(t, int64(200), gjson.GetBytes(processed, "max_completion_tokens").Int())
		})

		t.Run("judges the mapped model name", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(openaiMaxCompletionTokensMappedConfig)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(openaiMaxTokensChatHeaders())
			body := `{"model":"my-luna","max_tokens":256,"messages":[]}`
			action := host.CallOnHttpRequestBody([]byte(body))
			require.Equal(t, types.ActionContinue, action)

			processed := host.GetRequestBody()
			require.NotNil(t, processed)
			require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(processed, "model").String())
			require.False(t, gjson.GetBytes(processed, "max_tokens").Exists())
			require.Equal(t, int64(256), gjson.GetBytes(processed, "max_completion_tokens").Int())
		})
	})
}
