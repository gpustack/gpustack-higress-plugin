// GPUStack-local test (no upstream counterpart): guards the request-priority
// translation end to end on the in-process wasm host -- gpustack/gpustack#6739.
//
// The translation touches proxywasm host state (request headers, ctx, body),
// so it cannot run under plain `go test`; see openai_max_tokens_integration_
// test.go for the pattern. Pure helpers are covered in
// provider/request_priority_test.go.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	wasmhost "github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// requestPriorityConfig builds a single openai provider; "\x00" omits the
// target key entirely.
func requestPriorityConfig(target string) json.RawMessage {
	provider := map[string]interface{}{
		"type":      "openai",
		"apiTokens": []string{"sk-request-priority"},
	}
	if target != "\x00" {
		provider["requestPriorityTarget"] = target
	}
	data, _ := json.Marshal(map[string]interface{}{"provider": provider})
	return data
}

func requestPriorityHeaders(path string, extra ...[2]string) [][2]string {
	headers := [][2]string{
		{":authority", "example.com"},
		{":path", path},
		{":method", "POST"},
		{"Content-Type", "application/json"},
	}
	return append(headers, extra...)
}

func headerValue(headers [][2]string, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

func TestRequestPriorityVllmTarget(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		config := requestPriorityConfig("vllm")

		t.Run("translates the ext-auth header and drops it", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "20"}))
			headers := host.GetRequestHeaders()
			require.Equal(t, "-20", headerValue(headers, "x-vllm-priority"))
			require.Empty(t, headerValue(headers, "x-gpustack-request-priority"),
				"the internal source header must not reach the upstream")
		})

		t.Run("writes the baseline when the caller has no priority", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions"))
			require.Equal(t, "0", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
		})

		t.Run("overwrites a client-supplied vllm header", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-vllm-priority", "5"}, [2]string{"x-gpustack-request-priority", "20"}))
			require.Equal(t, "-20", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
		})

		t.Run("a malformed source value is the baseline, not a failure", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "abc"}))
			require.Equal(t, "0", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
		})

		// The server-authorised path relays the header without ext-auth's table
		// validation, so the int32 bound has to hold here too: vLLM rejects a
		// header outside it instead of scheduling the request.
		t.Run("a value beyond int32 is the baseline, not a failure", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "2147483648"}))
			require.Equal(t, "0", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
		})

		t.Run("the top of the int32 range is accepted", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "2147483647"}))
			require.Equal(t, "-2147483647", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
		})

		// vLLM's pooling endpoints read the body member only, so the body is
		// written in the header's direction (-p) for either target.
		t.Run("writes the body member with the header's sign", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/embeddings",
				[2]string{"x-gpustack-request-priority", "20"}))
			body := `{"model":"m","input":"x","priority":5}`
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(body)))
			require.Equal(t, "-20", headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
			require.Equal(t, int64(-20), gjson.GetBytes(host.GetRequestBody(), "priority").Int(),
				"a client-supplied pooling priority must be replaced: %s", host.GetRequestBody())
		})
	})
}

func TestRequestPrioritySglangTarget(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		config := requestPriorityConfig("sglang")
		chat := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

		t.Run("writes the body member and writes no header", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "20"}))
			require.Empty(t, headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
			require.Empty(t, headerValue(host.GetRequestHeaders(), "x-gpustack-request-priority"))

			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(chat)))
			require.Equal(t, int64(20), gjson.GetBytes(host.GetRequestBody(), "priority").Int())
		})

		t.Run("overwrites a client-supplied body priority", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
				[2]string{"x-gpustack-request-priority", "7"}))
			body := `{"model":"m","priority":999,"messages":[]}`
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(body)))
			require.Equal(t, int64(7), gjson.GetBytes(host.GetRequestBody(), "priority").Int())
		})

		t.Run("writes an explicit 0 for a caller without priority", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions"))
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(chat)))
			require.True(t, gjson.GetBytes(host.GetRequestBody(), "priority").Exists())
			require.Equal(t, int64(0), gjson.GetBytes(host.GetRequestBody(), "priority").Int())
		})

		// The Claude -> OpenAI conversion rebuilds the body from the Claude
		// schema; injecting before it would be lost. This is the case the
		// injection point after TransformRequestBody exists for.
		t.Run("survives the Claude to OpenAI conversion", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/messages",
				[2]string{"x-gpustack-request-priority", "20"}))
			claude := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(claude)))
			require.Equal(t, int64(20), gjson.GetBytes(host.GetRequestBody(), "priority").Int())
		})

		t.Run("leaves an image generation body alone", func(t *testing.T) {
			host, status := wasmhost.NewTestHost(config)
			defer host.Reset()
			require.Equal(t, types.OnPluginStartStatusOK, status)

			host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/images/generations",
				[2]string{"x-gpustack-request-priority", "20"}))
			body := `{"model":"m","prompt":"hi"}`
			require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(body)))
			require.False(t, gjson.GetBytes(host.GetRequestBody(), "priority").Exists())
		})
	})
}

// A provider without a target, or with a typo in it, is untouched except that
// the internal source header never travels upstream.
func TestRequestPriorityNoTarget(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		for name, config := range map[string]json.RawMessage{
			"unset": requestPriorityConfig("\x00"),
			"typo":  requestPriorityConfig("torch"),
		} {
			t.Run(name, func(t *testing.T) {
				host, status := wasmhost.NewTestHost(config)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
					[2]string{"x-gpustack-request-priority", "20"}))
				require.Empty(t, headerValue(host.GetRequestHeaders(), "x-vllm-priority"))
				require.Empty(t, headerValue(host.GetRequestHeaders(), "x-gpustack-request-priority"))

				body := `{"model":"m","messages":[]}`
				require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(body)))
				require.False(t, gjson.GetBytes(host.GetRequestBody(), "priority").Exists())
			})
		}
	})
}

// The generic provider forwards the body without handleRequestBody, so its
// own OnRequestBody has to write the member.
func TestRequestPriorityGenericProvider(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		config := json.RawMessage(`{"provider":{"type":"generic","genericHost":"http://127.0.0.1:9","apiTokens":["t"],"requestPriorityTarget":"sglang"}}`)
		host, status := wasmhost.NewTestHost(config)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(requestPriorityHeaders("/v1/chat/completions",
			[2]string{"x-gpustack-request-priority", "20"}))
		require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(`{"model":"m","messages":[],"priority":999}`)))
		require.Equal(t, int64(20), gjson.GetBytes(host.GetRequestBody(), "priority").Int())
		require.Empty(t, headerValue(host.GetRequestHeaders(), "x-gpustack-request-priority"))
	})
}

// A body the plugin declines to read is never rewritten, so the media-type
// gate that decides that must not be stricter than the priority's own:
// case variants and application/*+json are JSON.
func TestRequestPriorityJsonContentTypeVariants(t *testing.T) {
	wasmhost.RunTest(t, func(t *testing.T) {
		config := requestPriorityConfig("sglang")
		for _, ct := range []string{"Application/JSON; Charset=UTF-8", "application/vnd.api+json", "APPLICATION/JSON"} {
			t.Run(ct, func(t *testing.T) {
				host, status := wasmhost.NewTestHost(config)
				defer host.Reset()
				require.Equal(t, types.OnPluginStartStatusOK, status)

				headers := requestPriorityHeaders("/v1/chat/completions", [2]string{"x-gpustack-request-priority", "20"})
				for i, h := range headers {
					if strings.EqualFold(h[0], "Content-Type") {
						headers[i][1] = ct
					}
				}
				host.CallOnHttpRequestHeaders(headers)
				require.Equal(t, types.ActionContinue, host.CallOnHttpRequestBody([]byte(`{"model":"m","messages":[],"priority":999}`)))
				require.Equal(t, int64(20), gjson.GetBytes(host.GetRequestBody(), "priority").Int(),
					"Content-Type %q bypassed the body rewrite", ct)
			})
		}
	})
}
