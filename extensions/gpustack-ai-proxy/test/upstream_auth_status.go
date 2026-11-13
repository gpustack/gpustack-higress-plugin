package test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
)

// GPUStack-local tests for remapping upstream 401/403 to 502.
// See gpustack/gpustack#6646.

var upstreamAuthStatusConfig = func() json.RawMessage {
	data, _ := json.Marshal(map[string]interface{}{
		"provider": map[string]interface{}{
			"type":      "openai",
			"apiTokens": []string{"sk-token-a"},
			"modelMapping": map[string]string{
				"*": "gpt-3.5-turbo",
			},
		},
	})
	return data
}()

func upstreamAuthStatusRequestHeaders() [][2]string {
	return [][2]string{
		{":authority", "example.com"},
		{":path", "/v1/embeddings"},
		{":method", "POST"},
		{"Content-Type", "application/json"},
	}
}

func upstreamAuthStatusRequestBody() []byte {
	return []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
}

func RunUpstreamAuthStatusRemapTests(t *testing.T) {
	t.Run("upstream 401 is remapped to 502 with body preserved", func(t *testing.T) {
		host, status := test.NewTestHost(upstreamAuthStatusConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(upstreamAuthStatusRequestHeaders())
		host.CallOnHttpRequestBody(upstreamAuthStatusRequestBody())
		setUpstreamResponse(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "401"},
			{"Content-Type", "application/json"},
			{"WWW-Authenticate", `Bearer realm="upstream"`},
		})
		host.CallOnHttpResponseBody([]byte(`{"error":"Unauthorized"}`))
		host.CompleteHttp()

		responseHeaders := host.GetResponseHeaders()
		responseStatus, hasStatus := test.GetHeaderValue(responseHeaders, ":status")
		require.True(t, hasStatus, ":status header should exist")
		require.Equal(t, "502", responseStatus, "upstream 401 must be remapped to 502")

		_, hasWwwAuth := test.GetHeaderValue(responseHeaders, "WWW-Authenticate")
		require.False(t, hasWwwAuth, "upstream WWW-Authenticate challenge must be removed")

		require.Contains(t, string(host.GetResponseBody()), `{"error":"Unauthorized"}`,
			"upstream error body must be preserved verbatim")
	})

	t.Run("upstream 403 is remapped to 502", func(t *testing.T) {
		host, status := test.NewTestHost(upstreamAuthStatusConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(upstreamAuthStatusRequestHeaders())
		host.CallOnHttpRequestBody(upstreamAuthStatusRequestBody())
		setUpstreamResponse(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "403"},
			{"Content-Type", "application/json"},
		})
		host.CompleteHttp()

		responseHeaders := host.GetResponseHeaders()
		responseStatus, hasStatus := test.GetHeaderValue(responseHeaders, ":status")
		require.True(t, hasStatus, ":status header should exist")
		require.Equal(t, "502", responseStatus, "upstream 403 must be remapped to 502")
	})

	t.Run("upstream 400 passes through unchanged", func(t *testing.T) {
		host, status := test.NewTestHost(upstreamAuthStatusConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(upstreamAuthStatusRequestHeaders())
		host.CallOnHttpRequestBody(upstreamAuthStatusRequestBody())
		setUpstreamResponse(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "400"},
			{"Content-Type", "application/json"},
		})
		host.CompleteHttp()

		responseHeaders := host.GetResponseHeaders()
		responseStatus, hasStatus := test.GetHeaderValue(responseHeaders, ":status")
		require.True(t, hasStatus, ":status header should exist")
		require.Equal(t, "400", responseStatus, "upstream 400 must not be remapped")
	})

	t.Run("upstream 429 passes through unchanged", func(t *testing.T) {
		host, status := test.NewTestHost(upstreamAuthStatusConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(upstreamAuthStatusRequestHeaders())
		host.CallOnHttpRequestBody(upstreamAuthStatusRequestBody())
		setUpstreamResponse(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "429"},
			{"Content-Type", "application/json"},
		})
		host.CompleteHttp()

		responseHeaders := host.GetResponseHeaders()
		responseStatus, hasStatus := test.GetHeaderValue(responseHeaders, ":status")
		require.True(t, hasStatus, ":status header should exist")
		require.Equal(t, "429", responseStatus, "upstream 429 must not be remapped")
	})

	t.Run("remap is logged", func(t *testing.T) {
		host, status := test.NewTestHost(upstreamAuthStatusConfig)
		defer host.Reset()
		require.Equal(t, types.OnPluginStartStatusOK, status)

		host.CallOnHttpRequestHeaders(upstreamAuthStatusRequestHeaders())
		host.CallOnHttpRequestBody(upstreamAuthStatusRequestBody())
		setUpstreamResponse(host)
		host.CallOnHttpResponseHeaders([][2]string{
			{":status", "401"},
			{"Content-Type", "application/json"},
		})
		host.CompleteHttp()

		warnLogs := host.GetWarnLogs()
		found := false
		for _, log := range warnLogs {
			if strings.Contains(log, "upstream_auth_error_remapped") {
				found = true
				break
			}
		}
		require.True(t, found, "remap should emit an upstream_auth_error_remapped warning log")
	})
}
