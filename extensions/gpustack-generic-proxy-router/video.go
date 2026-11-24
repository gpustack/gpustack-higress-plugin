package main

import (
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func rejectVideo(status uint32, body string) (types.Action, bool) {
	_ = proxywasm.SendHttpResponse(status, [][2]string{{"content-type", "application/json"}}, []byte(body), -1)
	return types.ActionPause, true
}

func routeVideo(ctx wrapper.HttpContext, config PluginConfig) (types.Action, bool) {
	method, _ := proxywasm.GetHttpRequestHeader(":method")
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	canonical, model, video := routing.CanonicalPath(path, config.prefix, config.aliasNameMapping)
	fallback, _ := proxywasm.GetHttpRequestHeader("x-gpustack-fallback-path")
	_, _, fallbackVideo := routing.CanonicalPath(fallback, config.prefix, config.aliasNameMapping)
	if !video && !fallbackVideo {
		return types.ActionContinue, false
	}

	// Routing uses the model alias and the regular auth chain. A client cannot
	// select an upstream cluster or replay an internal fallback on these paths.
	for _, header := range []string{"x-gpustack-fallback-path", "x-higress-fallback-from",
		"x-higress-target-cluster", "x-gpustack-model-instance", "x-envoy-retry-on",
		"x-envoy-retry-grpc-on", "x-envoy-hedge-on-per-try-timeout"} {
		_ = proxywasm.RemoveHttpRequestHeader(header)
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-envoy-max-retries", "0")
	op, _ := routing.Operation(method, canonical)
	if !video || op == "" {
		return rejectVideo(404, `{"error":{"message":"Video operation is not supported"}}`)
	}
	if canonical != path {
		_ = proxywasm.ReplaceHttpRequestHeader(":path", canonical)
	}
	if (op == "create" || op == "sync") && model == "" && ctx.HasRequestBody() {
		// Use the ordinary JSON/multipart model routing path.
		_ = proxywasm.RemoveHttpRequestHeader("x-gpustack-model")
		_ = proxywasm.RemoveHttpRequestHeader(config.targetHeader)
		return types.ActionContinue, false
	}
	if model == "" {
		model, _ = proxywasm.GetHttpRequestHeader("x-gpustack-model")
	}
	if strings.TrimSpace(model) == "" {
		return rejectVideo(400, `{"error":{"message":"Missing video model"}}`)
	}
	_ = proxywasm.RemoveHttpRequestHeader("x-gpustack-model")
	writeRoutingHeaders(config, model)
	ctx.DontReadRequestBody()
	return types.ActionContinue, true
}
