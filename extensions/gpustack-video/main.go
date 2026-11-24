package main

import (
	"encoding/json"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const pluginName = "gpustack-video"

type Target struct {
	Model        string        `json:"model"`
	Remote       bool          `json:"remote"`
	Capabilities *Capabilities `json:"capabilities"`
}

type Config struct {
	Targets map[string]Target `json:"targets"`
}

func main() {}

func init() {
	wrapper.SetCtx(pluginName, wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHeaders),
		wrapper.ProcessRequestBody(onBody))
}

func parseConfig(raw gjson.Result, config *Config) error {
	return json.Unmarshal([]byte(raw.Raw), config)
}

func fail(status uint32, message string) types.Action {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "video_gateway_error", "code": status,
	}})
	_ = proxywasm.SendHttpResponse(status, [][2]string{{"content-type", "application/json"}}, body, -1)
	return types.ActionPause
}

func onHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	method, _ := proxywasm.GetHttpRequestHeader(":method")
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	op, _ := routing.Operation(method, path)
	if op == "" {
		ctx.DontReadRequestBody()
		return fail(404, "Video operation is not supported")
	}

	// Resolve the cluster after ordinary model routing and load balancing.
	ctx.DisableReroute()
	cluster, _ := proxywasm.GetProperty([]string{"cluster_name"})
	target := config.Targets[string(cluster)]
	if target.Remote && target.Model != "" {
		// The next gateway applies its own model routing and H3 capabilities.
		_ = proxywasm.ReplaceHttpRequestHeader("x-gpustack-model", target.Model)
	}
	if op != "create" {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if !ctx.HasRequestBody() {
		return fail(400, "Invalid video request")
	}
	ctx.SetContext("video_target", target)
	ctx.SetRequestBodyBufferLimit(100 * 1024 * 1024)
	return types.HeaderStopIteration
}

func onBody(ctx wrapper.HttpContext, _ Config, body []byte) types.Action {
	target, ok := ctx.GetContext("video_target").(Target)
	if !ok {
		return types.ActionContinue
	}
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	request, err := parseRequest(body, contentType)
	if err != nil {
		return fail(400, "Invalid video request")
	}
	if truthy(request.Values["stream"]) {
		return fail(400, "Video jobs do not support streaming")
	}
	changed := false
	if target.Model != "" && request.Values["model"] != target.Model {
		request.Values["model"] = target.Model
		changed = true
	}
	if target.Capabilities != nil {
		if err := adaptH3(&request, *target.Capabilities); err != nil {
			return fail(400, err.Error())
		}
		changed = true
	}
	if !changed {
		return types.ActionContinue
	}
	encoded, encodedType, err := request.Encode()
	if err != nil {
		return fail(400, "Invalid video request")
	}
	if proxywasm.ReplaceHttpRequestBody(encoded) != nil {
		return fail(503, "Video request adaptation failed")
	}
	if strings.HasPrefix(encodedType, "multipart/") || encodedType != contentType {
		_ = proxywasm.ReplaceHttpRequestHeader("content-type", encodedType)
	}
	_ = proxywasm.RemoveHttpRequestHeader("content-length")
	return types.ActionContinue
}
