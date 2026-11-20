package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	pluginName          = "gpustack-video"
	callerState         = "gpustack_video_caller"
	bindingState        = "gpustack_video_binding"
	pinnedClusterHeader = "x-gpustack-video-cluster"
	controlPrefix       = "/v2/video-tasks/gateway"
	ctxOperation        = "video_operation"
	ctxTask             = "video_task"
)

var nativeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var publicID = routing.PublicID

type Config struct {
	enabled bool
	mode    string
	role    string
	token   string
	client  wrapper.HttpClient
	timeout uint32
}

type Binding struct {
	ID           string            `json:"id"`
	Model        string            `json:"model"`
	Cluster      string            `json:"cluster"`
	Destination  string            `json:"destination"`
	Path         string            `json:"path"`
	Capabilities *Capabilities     `json:"capabilities"`
	Forward      bool              `json:"forward"`
	Ticket       string            `json:"ticket"`
	NativeModel  string            `json:"native_model"`
	Asset        bool              `json:"asset"`
	Headers      map[string]string `json:"headers"`
	Response     map[string]any    `json:"response"`
	StatusOnly   bool              `json:"status_only"`
	History      bool              `json:"history"`
	ManagedFiles bool              `json:"managed_files"`
	FileBackend  string            `json:"file_backend"`
}

type RelayGrant struct {
	ID        string `json:"task_id"`
	Ticket    string `json:"ticket"`
	Operation string `json:"operation,omitempty"`
}

func main() {}

func init() {
	wrapper.SetCtx(pluginName, wrapper.ParseOverrideConfig(parseConfig, parseOverrideConfig),
		wrapper.ProcessRequestHeaders(onHeaders),
		wrapper.ProcessRequestBody(onBody),
		wrapper.ProcessResponseHeaders(onResponseHeaders),
		wrapper.ProcessResponseBody(onResponseBody))
}

func parseConfig(raw gjson.Result, config *Config) error {
	config.enabled = !raw.Get("enabled").Exists() || raw.Get("enabled").Bool()
	config.role = raw.Get("role").String()
	if config.role == "" {
		config.role = "edge"
	}
	if config.role != "edge" && config.role != "cluster" {
		return errors.New("invalid video gateway role")
	}
	config.mode = raw.Get("mode").String()
	if config.mode != "resolve" && config.mode != "transport" {
		return errors.New("video mode must be resolve or transport")
	}
	config.token = raw.Get("gatewayToken").String()
	service := raw.Get("serviceName").String()
	if service == "" || config.token == "" {
		return errors.New("video control service and gateway credential are required")
	}
	port := raw.Get("servicePort").Int()
	if port == 0 {
		port = 80
	}
	timeout := raw.Get("timeoutMs").Int()
	if timeout == 0 {
		timeout = 10000
	}
	if timeout < 1 || timeout > int64(^uint32(0)) || port < 1 || port > 65535 {
		return errors.New("invalid video control timeout or port")
	}
	config.timeout = uint32(timeout)
	config.client = wrapper.NewClusterClient(wrapper.FQDNCluster{FQDN: service, Port: port})
	return nil
}

func parseOverrideConfig(raw gjson.Result, global Config, config *Config) error {
	// Per-ingress rules only enable the plugin; credentials and its role come
	// exclusively from the gateway's default configuration.
	*config = global
	if raw.Get("enabled").Exists() {
		config.enabled = raw.Get("enabled").Bool()
	}
	return nil
}

func operation(method, rawPath string) (string, string) { return routing.Operation(method, rawPath) }

func validStatusSnapshot(binding Binding, op, id string) bool {
	if op != "status" || binding.ID != id || binding.Response == nil || binding.Response["id"] != id {
		return false
	}
	switch binding.Response["status"] {
	case "failed", "completed", "cancelled", "expired":
		return true
	default:
		return false
	}
}

func setState(key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return proxywasm.SetProperty([]string{key}, data)
}

func getState(key string, value any) bool {
	data, err := proxywasm.GetProperty([]string{key})
	return err == nil && json.Unmarshal(data, value) == nil
}

func fail(status int, message string) types.Action {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "video_gateway_error", "code": status}})
	_ = proxywasm.SendHttpResponseWithDetail(uint32(status), "video_gateway_error", [][2]string{{"content-type", "application/json"}}, body, -1)
	return types.ActionPause
}

func call(config Config, method, path string, body any, done func(int, []byte)) error {
	var headers map[string]string
	if !getState(callerState, &headers) {
		return errors.New("missing video caller context")
	}
	headers["x-gpustack-auth-token"] = config.token
	headers["content-type"] = "application/json"
	requestHeaders := make([][2]string, 0, len(headers))
	for name, value := range headers {
		requestHeaders = append(requestHeaders, [2]string{name, value})
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	return config.client.Call(method, path, requestHeaders, data,
		func(status int, _ http.Header, response []byte) { done(status, response) }, config.timeout)
}

func onHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	if !config.enabled {
		ctx.DontReadRequestBody()
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	method, _ := proxywasm.GetHttpRequestHeader(":method")
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	op, id := operation(method, path)
	if op == "" {
		ctx.DontReadRequestBody()
		ctx.DontReadResponseBody()
		if strings.HasPrefix(strings.SplitN(path, "?", 2)[0], "/v1/videos") {
			return fail(404, "Video operation is not supported")
		}
		return types.ActionContinue
	}
	ctx.SetContext(ctxOperation, op)
	if op == "delete" {
		parsed, err := url.ParseRequestURI(path)
		if err != nil {
			return fail(400, "Invalid video action")
		}
		value := parsed.Query().Get("keep_task")
		if len(parsed.Query()["keep_task"]) > 1 || (value != "" && value != "true" && value != "false") {
			return fail(400, "Invalid video action")
		}
		ctx.SetContext("video_keep_task", value == "true")
	}
	var grant RelayGrant
	if config.role == "cluster" && (!getState(routing.RelayState, &grant) || grant.Ticket == "" || !publicID.MatchString(grant.ID)) {
		return fail(403, "Video delegation is required")
	}
	if config.role == "cluster" && id != "" && id != grant.ID {
		return fail(404, "Video task is unavailable")
	}
	if config.mode == "resolve" {
		ctx.DontReadRequestBody()
		ctx.DontReadResponseBody()
		headers := map[string]string{}
		for _, name := range []string{"authorization", "proxy-authorization", "cookie", "x-api-key", "x-organization-id", "x-forwarded-for"} {
			if value, err := proxywasm.GetHttpRequestHeader(name); err == nil && value != "" {
				headers[name] = value
			}
		}
		if setState(callerState, headers) != nil {
			return fail(503, "Video routing is unavailable")
		}
		if op == "create" {
			model, _ := proxywasm.GetHttpRequestHeader("x-higress-llm-model")
			if model == "" || setState("gpustack_video_model", model) != nil {
				return fail(400, "Missing video model")
			}
			return types.ActionContinue
		}
		method, controlPath := "GET", controlPrefix+"/"+id+"/resolve"
		if op == "delete" || op == "content" {
			controlPath += "?operation=" + op
		}
		var payload any
		if config.role == "cluster" {
			method = "POST"
			controlPath = "/v2/video-tasks/relay/resolve"
			if op == "delete" || op == "content" {
				grant.Operation = op
			}
			payload = grant
		}
		err := call(config, method, controlPath, payload, func(status int, body []byte) {
			var binding Binding
			if status != 200 || json.Unmarshal(body, &binding) != nil || binding.ID != id || binding.Model == "" || (binding.StatusOnly && !validStatusSnapshot(binding, op, id)) || (!binding.StatusOnly && !strings.HasPrefix(binding.Cluster, "outbound|")) {
				if status < 400 || status > 599 {
					status = 503
				}
				fail(status, "Video task is unavailable")
				return
			}
			if setState(bindingState, binding) != nil {
				fail(503, "Video routing is unavailable")
				return
			}
			_ = proxywasm.ReplaceHttpRequestHeader("x-higress-llm-model", binding.Model)
			if binding.StatusOnly {
				_ = proxywasm.RemoveHttpRequestHeader(pinnedClusterHeader)
			} else {
				_ = proxywasm.ReplaceHttpRequestHeader(pinnedClusterHeader, binding.Cluster)
			}
			if binding.History {
				_ = proxywasm.ReplaceHttpRequestHeader(routing.BootstrapHeader, "1")
			} else {
				_ = proxywasm.RemoveHttpRequestHeader(routing.BootstrapHeader)
			}
			proxywasm.ResumeHttpRequest()
		})
		if err != nil {
			return fail(503, "Video routing is unavailable")
		}
		return types.ActionPause
	}
	ctx.DisableReroute()
	if op == "create" || op == "delete" {
		disableRetries()
	}
	if op != "create" {
		ctx.DontReadRequestBody()
		var binding Binding
		if !getState(bindingState, &binding) || binding.ID != id {
			return fail(503, "Video routing is unavailable")
		}
		if binding.StatusOnly {
			if !validStatusSnapshot(binding, op, id) {
				return fail(503, "Invalid video task response")
			}
			encoded, _ := json.Marshal(binding.Response)
			ctx.DontReadResponseBody()
			_ = proxywasm.SendHttpResponse(200, [][2]string{{"content-type", "application/json"}, {"cache-control", "no-store"}}, encoded, -1)
			return types.ActionPause
		}
		cluster, _ := proxywasm.GetProperty([]string{"cluster_name"})
		if string(cluster) != binding.Cluster {
			return fail(503, "Video routing is unavailable")
		}
		if binding.Response != nil {
			encoded, _ := json.Marshal(binding.Response)
			ctx.DontReadResponseBody()
			_ = proxywasm.SendHttpResponse(200, [][2]string{{"content-type", "application/json"}, {"cache-control", "no-store"}}, encoded, -1)
			return types.ActionPause
		}
		if binding.Asset {
			_ = proxywasm.RemoveHttpRequestHeader("x-gpustack-model-instance")
		} else {
			_ = proxywasm.ReplaceHttpRequestHeader("x-gpustack-model-instance", binding.Destination)
		}
		_ = proxywasm.RemoveHttpRequestHeader("x-gpustack-video-file-grant")
		for name, value := range binding.Headers {
			_ = proxywasm.ReplaceHttpRequestHeader(name, value)
		}
		_ = proxywasm.RemoveHttpRequestHeader("accept-encoding")
		ctx.SetContext(ctxTask, binding)
		if binding.Forward {
			if setRelayHeader(binding) != nil {
				return fail(503, "Video routing is unavailable")
			}
		}
		nativePath := binding.Path
		if op == "content" && !binding.Asset {
			nativePath += "/content"
		}
		parsed, _ := url.ParseRequestURI(path)
		query := parsed.Query()
		if op == "delete" && !binding.Forward {
			query.Del("keep_task")
		}
		if value := query.Encode(); value != "" {
			nativePath += "?" + value
		}
		_ = proxywasm.ReplaceHttpRequestHeader(":path", nativePath)
		if op == "delete" && !binding.Forward {
			return startDeletion(ctx, config, binding)
		}
		return types.ActionContinue
	}
	_ = proxywasm.RemoveHttpRequestHeader("accept-encoding")
	ctx.SetRequestBodyBufferLimit(100 * 1024 * 1024)
	return types.HeaderStopIteration
}

func disableRetries() {
	// Mutations must not be replayed by client retry or hedge overrides.
	for _, name := range []string{"x-envoy-retry-on", "x-envoy-retry-grpc-on", "x-envoy-hedge-on-per-try-timeout", "x-envoy-max-retries"} {
		_ = proxywasm.RemoveHttpRequestHeader(name)
	}
	_ = proxywasm.ReplaceHttpRequestHeader("x-envoy-max-retries", "0")
}

func onBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if config.mode != "transport" || ctx.GetStringContext(ctxOperation, "") != "create" {
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
	var model string
	if !getState("gpustack_video_model", &model) {
		return fail(503, "Video routing is unavailable")
	}
	destination, _ := proxywasm.GetHttpRequestHeader("x-gpustack-model-instance")
	nativeModel, _ := request.Values["model"].(string)
	controlPath := controlPrefix + "/prepare"
	payload := map[string]any{"model": model, "destination": destination, "native_model": nativeModel, "request_parameters": request.Snapshot(model)}
	if config.role == "cluster" {
		var grant RelayGrant
		if !getState(routing.RelayState, &grant) {
			return fail(503, "Video routing is unavailable")
		}
		controlPath = "/v2/video-tasks/relay/bind"
		payload = map[string]any{"ticket": grant.Ticket, "task_id": grant.ID, "destination": destination}
	}
	err = call(config, "POST", controlPath, payload, func(status int, data []byte) {
		var binding Binding
		if status != 200 || json.Unmarshal(data, &binding) != nil || !publicID.MatchString(binding.ID) {
			if status < 400 || status > 599 {
				status = 503
			}
			fail(status, "Video submission could not be prepared")
			return
		}
		cluster, _ := proxywasm.GetProperty([]string{"cluster_name"})
		if string(cluster) != binding.Cluster || binding.Destination != destination {
			fail(503, "Video routing is unavailable")
			return
		}
		if config.role == "cluster" {
			var grant RelayGrant
			if !getState(routing.RelayState, &grant) || binding.ID != grant.ID {
				fail(503, "Video routing is unavailable")
				return
			}
		}
		ctx.SetContext(ctxTask, binding)
		if binding.Forward && setRelayHeader(binding) != nil {
			fail(503, "Video routing is unavailable")
			return
		}
		rewrite := binding.Capabilities != nil

		if binding.NativeModel != "" && request.Values["model"] != binding.NativeModel {
			request.Values["model"] = binding.NativeModel
			rewrite = true
		}
		if binding.Capabilities != nil {
			if err := adaptH3(&request, *binding.Capabilities); err != nil {
				fail(400, err.Error())
				return
			}
		}
		if binding.ManagedFiles && binding.FileBackend == "SGLang" {
			if _, present := request.Values["output_path"]; present {
				delete(request.Values, "output_path")
				rewrite = true
			}
		}
		if rewrite {
			encoded, encodedType, err := request.Encode()
			if err != nil {
				fail(400, "Invalid video request")
				return
			}
			_ = proxywasm.ReplaceHttpRequestHeader("content-type", encodedType)
			_ = proxywasm.RemoveHttpRequestHeader("content-length")
			if proxywasm.ReplaceHttpRequestBody(encoded) != nil {
				fail(503, "Video request adaptation failed")
				return
			}
		}
		proxywasm.ResumeHttpRequest()
	})
	if err != nil {
		return fail(503, "Video submission could not be prepared")
	}
	return types.ActionPause
}

func onResponseHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	op := ctx.GetStringContext(ctxOperation, "")
	if config.mode != "transport" || op == "" {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	status, _ := proxywasm.GetHttpResponseHeader(":status")
	success := len(status) == 3 && status[0] == '2'
	if op == "content" && success {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	if !success {
		ctx.SetContext("video_error_status", status)
	}
	contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
	if success && (!ctx.HasResponseBody() || !strings.Contains(strings.ToLower(contentType), "json")) {
		return fail(503, "Invalid video task response")
	}
	_ = proxywasm.RemoveHttpResponseHeader("content-length")
	ctx.BufferResponseBody()
	ctx.SetResponseBodyBufferLimit(64 * 1024)
	return types.HeaderStopIteration
}

func onResponseBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if config.mode != "transport" {
		return types.ActionContinue
	}
	binding, ok := ctx.GetContext(ctxTask).(Binding)
	if !ok {
		return types.ActionContinue
	}
	errorStatus, _ := strconv.Atoi(ctx.GetStringContext("video_error_status", ""))
	upstreamError := errorStatus >= 400 && errorStatus <= 599
	if ctx.GetStringContext(ctxOperation, "") == "content" {
		if upstreamError {
			return responseError(errorStatus, "Video content is unavailable. Check the task and storage status.")
		}
		return types.ActionContinue
	}
	var job map[string]any
	if json.Unmarshal(body, &job) != nil {
		if upstreamError {
			return responseError(errorStatus, "Video request failed. Check the model instance logs for details.")
		}
		return responseError(503, "Invalid video task response")
	}
	statusCode, _ := proxywasm.GetHttpResponseHeader(":status")
	if len(statusCode) != 3 || statusCode[0] != '2' {
		if ctx.GetStringContext(ctxOperation, "") != "status" || job["status"] != "failed" || job["id"] == nil {
			return responseError(errorStatus, "Video request failed. Check the model instance logs for details.")
		}
	}
	if ctx.GetStringContext(ctxOperation, "") == "delete" {
		return onDeletionResponse(ctx, config, binding, job)
	}
	id, ok := job["id"].(string)
	if binding.Forward {
		if !ok || id != binding.ID {
			return responseError(503, "Invalid video task response")
		}
		if ctx.GetStringContext(ctxOperation, "") != "create" {
			job["model"] = binding.Model
			encoded, _ := json.Marshal(job)
			_ = proxywasm.ReplaceHttpResponseBody(encoded)
			return types.ActionContinue
		}
		err := call(config, "POST", controlPrefix+"/"+binding.ID+"/confirmed", nil, func(status int, _ []byte) {
			if status != 200 {
				responseError(503, "Video submission could not be confirmed; do not resubmit")
			}
			proxywasm.ResumeHttpResponse()
		})
		if err != nil {
			return responseError(503, "Video submission could not be confirmed; do not resubmit")
		}
		return types.ActionPause
	}
	if !ok || !nativeID.MatchString(id) {
		return responseError(503, "Invalid video task response")
	}
	observation := publicJob(job, Binding{ID: id, Model: binding.Model})
	job = publicJob(job, binding)
	encoded, _ := json.Marshal(job)
	if ctx.GetStringContext(ctxOperation, "") != "create" {
		if binding.Path != "/v1/videos/"+id {
			return responseError(503, "Invalid video task response")
		}
		return persistObservation(ctx, config, binding, id, observation, encoded)
	}
	destination, _ := proxywasm.GetProperty([]string{"cluster_name"})
	parts := strings.SplitN(string(destination), "||", 2)
	if len(parts) != 2 || parts[1] != binding.Destination {
		return responseError(503, "Video submission target could not be confirmed; do not resubmit")
	}
	controlPath := controlPrefix + "/" + binding.ID + "/accepted"
	payload := map[string]any{"native_id": id, "destination": parts[1], "job": observation}
	if config.role == "cluster" {
		var grant RelayGrant
		if !getState(routing.RelayState, &grant) {
			return responseError(503, "Video submission could not be confirmed; do not resubmit")
		}
		controlPath = "/v2/video-tasks/relay/accepted"
		payload["task_id"] = grant.ID
		payload["ticket"] = grant.Ticket
	}
	err := call(config, "POST", controlPath, payload, func(status int, _ []byte) {
		if status != 200 {
			responseError(503, "Video submission could not be confirmed; do not resubmit")
			proxywasm.ResumeHttpResponse()
			return
		}
		_ = proxywasm.RemoveHttpResponseHeader("content-length")
		_ = proxywasm.ReplaceHttpResponseBody(encoded)
		proxywasm.ResumeHttpResponse()
	})
	if err != nil {
		return responseError(503, "Video submission could not be confirmed; do not resubmit")
	}
	return types.ActionPause
}

func responseError(status int, message string) types.Action {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "video_gateway_error"}})
	_ = proxywasm.ReplaceHttpResponseHeader(":status", fmt.Sprint(status))
	_ = proxywasm.ReplaceHttpResponseHeader("content-type", "application/json")
	_ = proxywasm.RemoveHttpResponseHeader("content-length")
	_ = proxywasm.ReplaceHttpResponseBody(body)
	return types.ActionContinue
}

func setRelayHeader(binding Binding) error {
	if !publicID.MatchString(binding.ID) || binding.Ticket == "" {
		return errors.New("missing video relay grant")
	}
	data, err := json.Marshal(RelayGrant{ID: binding.ID, Ticket: binding.Ticket})
	if err != nil {
		return err
	}
	return proxywasm.ReplaceHttpRequestHeader(routing.RelayHeader, string(data))
}
