package main

import (
	"encoding/json"
	"fmt"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func host(t *testing.T, mode string, roles ...string) proxytest.HostEmulator {
	t.Helper()
	role := "edge"
	if len(roles) > 0 {
		role = roles[0]
	}
	cfg := fmt.Sprintf(`{"mode":%q,"role":%q,"serviceName":"control.dns","servicePort":80,"gatewayToken":"test-only"}`, mode, role)
	return hostWithConfig(t, cfg)
}

func hostWithConfig(t *testing.T, cfg string) proxytest.HostEmulator {
	t.Helper()
	vm := wrapper.NewCommonVmCtx(pluginName,
		wrapper.ParseOverrideConfig(parseConfig, parseOverrideConfig),
		wrapper.ProcessRequestHeaders(onHeaders), wrapper.ProcessRequestBody(onBody),
		wrapper.ProcessResponseHeaders(onResponseHeaders), wrapper.ProcessResponseBody(onResponseBody))
	h, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().WithVMContext(vm).WithPluginConfiguration([]byte(cfg)))
	t.Cleanup(reset)
	h.RegisterForeignFunction("get_log_level", func([]byte) []byte { return []byte{0, 0, 0, 0} })
	if h.StartVM() != types.OnVMStartStatusOK || h.StartPlugin() != types.OnPluginStartStatusOK {
		t.Fatal("plugin failed to start")
	}
	return h
}

func TestRouteActivationKeepsGlobalCredentialsAndSkipsUnmatchedRoutes(t *testing.T) {
	cfg := `{"enabled":false,"mode":"resolve","serviceName":"control.dns","servicePort":80,"gatewayToken":"test-only","_rules_":[{"_match_route_":["video-route"],"enabled":true,"mode":"transport","gatewayToken":"forged"}]}`
	h := hostWithConfig(t, cfg)
	id := "video_" + strings.Repeat("a", 32)
	for _, path := range []string{"/v1/models", "/v1/videos/sync", "/v1/videos/" + id} {
		c := h.InitializeHttpContext()
		h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", path}}, true)
		if h.GetSentLocalResponse(c) != nil || len(h.GetCalloutAttributesFromContext(c)) != 0 {
			t.Fatalf("unmatched path was intercepted: %s", path)
		}
	}
	_ = h.SetProperty([]string{"route_name"}, []byte("video-route"))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || header(calls[0].Headers, "x-gpustack-auth-token") != "test-only" {
		t.Fatal("matched video route did not retain its global resolver configuration")
	}
}

func header(headers [][2]string, name string) string {
	for _, item := range headers {
		if strings.EqualFold(item[0], name) {
			return item[1]
		}
	}
	return ""
}

func TestResolverRestoresTaskRouteAndClearsBootstrap(t *testing.T) {
	h := host(t, "resolve")
	id := "video_" + strings.Repeat("a", 32)
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id}, {"authorization", "Bearer user-key"}, {"x-organization-id", "7"}, {"x-gpustack-video-bootstrap", "1"}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 {
		t.Fatalf("callouts: %+v", calls)
	}
	if header(calls[0].Headers, "authorization") != "Bearer user-key" || header(calls[0].Headers, "x-gpustack-auth-token") != "test-only" {
		t.Fatal("metadata call lost caller authentication")
	}
	binding := Binding{ID: id, Model: "org/wan", Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	body, _ := json.Marshal(binding)
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, body)
	headers := h.GetCurrentRequestHeaders(c)
	if header(headers, "x-higress-llm-model") != "org/wan" || header(headers, pinnedClusterHeader) != binding.Cluster || header(headers, "x-gpustack-video-bootstrap") != "" {
		t.Fatalf("unresolved headers: %+v", headers)
	}
}

func TestTransportRefusesDefaultRouteWhenPinIsNotApplied(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{"cluster_name"}, []byte("outbound|80||model-2-5.static"))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id + "/content"}}, true)
	if reply := h.GetSentLocalResponse(c); reply == nil || reply.StatusCode != 503 {
		t.Fatalf("wrong replica was not rejected: %+v", reply)
	}
}

func TestContentRewritesNativePathAndStreamsUnchanged(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id + "/content?variant=video"}}, true)
	headers := h.GetCurrentRequestHeaders(c)
	if header(headers, ":path") != "/v1/videos/native-task/content?variant=video" || header(headers, "x-gpustack-model-instance") != binding.Destination {
		t.Fatalf("incorrect native request: %+v", headers)
	}
	h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "video/mp4"}}, false)
	video := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}
	h.CallOnResponseBody(c, video, true)
	if string(h.GetCurrentResponseBody(c)) != string(video) {
		t.Fatal("content was buffered or rewritten")
	}
}

func TestCollectionDoesNotExposeEngineTaskList(t *testing.T) {
	h := host(t, "transport")
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos"}}, true)
	if reply := h.GetSentLocalResponse(c); reply == nil || reply.StatusCode != 404 {
		t.Fatal("unscoped native collection was forwarded")
	}
	if len(h.GetCalloutAttributesFromContext(c)) != 0 {
		t.Fatal("collection treated as a single task")
	}
}

func TestStatusOnlyReturnsThePublicID(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	caller, _ := json.Marshal(map[string]string{"authorization": "Bearer user"})
	_ = h.SetProperty([]string{callerState}, caller)
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id}}, true)
	h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
	h.CallOnResponseBody(c, []byte(`{"id":"native-task","status":"completed","progress":100}`), true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || !strings.HasSuffix(header(calls[0].Headers, ":path"), "/observed") {
		t.Fatal("status observation was not persisted")
	}
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{}`))
	var result map[string]any
	if json.Unmarshal(h.GetCurrentResponseBody(c), &result) != nil || result["id"] != id || result["progress"] != float64(100) {
		t.Fatalf("incorrect response: %s", h.GetCurrentResponseBody(c))
	}
}

func TestClusterGatewayRequiresADelegationGrant(t *testing.T) {
	h := host(t, "resolve", "cluster")
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/video_" + strings.Repeat("a", 32)}}, true)
	if reply := h.GetSentLocalResponse(c); reply == nil || reply.StatusCode != 403 {
		t.Fatal("untrusted request reached cluster task routing")
	}
	if len(h.GetCalloutAttributesFromContext(c)) != 0 {
		t.Fatal("missing grant reached control plane")
	}
}

func TestSubmissionResponseWaitsForDurableConfirmation(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static"}
	headers, _ := json.Marshal(map[string]string{"authorization": "Bearer user"})
	model, _ := json.Marshal("public-route")
	_ = h.SetProperty([]string{callerState}, headers)
	_ = h.SetProperty([]string{"gpustack_video_model"}, model)
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "POST"}, {":path", "/v1/videos"}, {"content-type", "application/json"}, {"x-gpustack-model-instance", binding.Destination}}, false)
	h.CallOnRequestBody(c, []byte(`{"model":"engine-model","prompt":"private prompt"}`), true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 {
		t.Fatal("missing task registration")
	}
	var registration struct {
		Parameters map[string]any `json:"request_parameters"`
	}
	if json.Unmarshal(calls[0].Body, &registration) != nil || registration.Parameters["prompt"] != "private prompt" || registration.Parameters["model"] != "public-route" {
		t.Fatal("task registration lost submitted parameters")
	}
	data, _ := json.Marshal(binding)
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, data)
	action := h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
	if action != types.HeaderStopIteration {
		t.Fatal("unconfirmed response headers were sent")
	}
	h.CallOnResponseBody(c, []byte(`{"id":"native-task","status":"queued"}`), true)
	calls = h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || !strings.HasSuffix(header(calls[0].Headers, ":path"), "/accepted") {
		t.Fatalf("pending metadata calls: %d", len(calls))
	}
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "503"}}, nil, []byte(`{}`))
	if header(h.GetCurrentResponseHeaders(c), ":status") != "503" {
		t.Fatal("metadata failure returned HTTP 200")
	}
	if strings.Contains(string(h.GetCurrentResponseBody(c)), "native-task") {
		t.Fatal("native ID leaked on confirmation failure")
	}
}

func TestClusterBindingCannotChangeTheSharedPublicID(t *testing.T) {
	h := host(t, "transport", "cluster")
	id := "video_" + strings.Repeat("a", 32)
	grant, _ := json.Marshal(RelayGrant{ID: id, Ticket: "internal-signed-grant"})
	caller, _ := json.Marshal(map[string]string{"authorization": "Bearer cluster-registration-key"})
	model, _ := json.Marshal("engine-model")
	_ = h.SetProperty([]string{routing.RelayState}, grant)
	_ = h.SetProperty([]string{callerState}, caller)
	_ = h.SetProperty([]string{"gpustack_video_model"}, model)
	_ = h.SetProperty([]string{"cluster_name"}, []byte("outbound|80||model-2-4.static"))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "POST"}, {":path", "/v1/videos"}, {"content-type", "application/json"}, {"x-gpustack-model-instance", "model-2-4.static"}}, false)
	h.CallOnRequestBody(c, []byte(`{"model":"engine-model","prompt":"hello"}`), true)
	call := h.GetCalloutAttributesFromContext(c)[0]
	if header(call.Headers, ":path") != "/v2/video-tasks/relay/bind" {
		t.Fatal("cluster gateway tried to create another task")
	}
	data, _ := json.Marshal(Binding{ID: "video_" + strings.Repeat("b", 32), Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static"})
	h.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "200"}}, nil, data)
	if reply := h.GetSentLocalResponse(c); reply == nil || reply.StatusCode != 503 {
		t.Fatal("changed public ID accepted")
	}
}

func TestManagedContentUsesWorkerGrantAndDoesNotAppendEngineSuffix(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||video-files-worker-3.static", Destination: "video-files-worker-3.static", Path: "/files/video-content/" + id, Asset: true, Headers: map[string]string{"x-gpustack-video-file-grant": "scoped-grant"}}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id + "/content"}, {"x-gpustack-model-instance", "model-2-4.static"}, {"x-gpustack-video-file-grant", "forged"}}, true)
	headers := h.GetCurrentRequestHeaders(c)
	if header(headers, ":path") != binding.Path || header(headers, "x-gpustack-model-instance") != "" || header(headers, "x-gpustack-video-file-grant") != "scoped-grant" {
		t.Fatalf("invalid file request: %+v", headers)
	}
}

func TestCompletedFileStatusIsReturnedWithoutContactingEngine(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||video-files-worker-3.static", Response: map[string]any{"id": id, "status": "completed", "file_state": "ready"}}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id}}, true)
	reply := h.GetSentLocalResponse(c)
	if reply == nil || reply.StatusCode != 200 || !strings.Contains(string(reply.Data), `"completed"`) {
		t.Fatalf("file status missing: %+v", reply)
	}
	if len(h.GetCalloutAttributesFromContext(c)) != 0 {
		t.Fatal("completed file queried the engine")
	}
}

func TestResolverRestoresRouteForStatusSnapshotWithoutAnEngine(t *testing.T) {
	h := host(t, "resolve")
	id := "video_" + strings.Repeat("a", 32)
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id}, {"authorization", "Bearer user-key"}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 {
		t.Fatal("missing authorized metadata lookup")
	}
	binding := Binding{ID: id, Model: "org/wan", StatusOnly: true, Response: map[string]any{"id": id, "status": "failed"}}
	body, _ := json.Marshal(binding)
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, body)
	headers := h.GetCurrentRequestHeaders(c)
	if header(headers, "x-higress-llm-model") != "org/wan" || header(headers, pinnedClusterHeader) != "" || h.GetSentLocalResponse(c) != nil {
		t.Fatal("status snapshot skipped the original route policies")
	}
}

func TestTransportReturnsOnlyTerminalStatusSnapshotsWithoutAnEngine(t *testing.T) {
	for _, tc := range []struct {
		method, suffix, status string
		accepted               bool
	}{
		{"GET", "", "failed", true}, {"GET", "", "completed", true},
		{"GET", "", "cancelled", true}, {"GET", "", "expired", true},
		{"GET", "", "queued", false}, {"GET", "/content", "failed", false},
		{"DELETE", "", "failed", false},
	} {
		t.Run(tc.method+tc.suffix+tc.status, func(t *testing.T) {
			h := host(t, "transport")
			id := "video_" + strings.Repeat("a", 32)
			binding := Binding{ID: id, Model: "wan", StatusOnly: true, Response: map[string]any{"id": id, "status": tc.status}}
			data, _ := json.Marshal(binding)
			_ = h.SetProperty([]string{bindingState}, data)
			c := h.InitializeHttpContext()
			h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", tc.method}, {":path", "/v1/videos/" + id + tc.suffix}}, true)
			reply := h.GetSentLocalResponse(c)
			if reply == nil || (reply.StatusCode == 200) != tc.accepted {
				t.Fatalf("invalid snapshot reply: %+v", reply)
			}
			if len(h.GetCalloutAttributesFromContext(c)) != 0 {
				t.Fatal("status snapshot contacted an engine")
			}
		})
	}
}

func TestAuthenticatedHistoryKeepsIndependentRouteAfterModelDeletion(t *testing.T) {
	h := host(t, "resolve")
	id := "video_" + strings.Repeat("a", 32)
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id + "/content"}, {"authorization", "Bearer user"}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	data, _ := json.Marshal(Binding{ID: id, Model: "deleted/model", Cluster: "outbound|80||video-files-worker-3.static", Destination: "video-files-worker-3.static", Path: "/files/video-content/" + id, Asset: true, History: true})
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, data)
	if header(h.GetCurrentRequestHeaders(c), routing.BootstrapHeader) != "1" {
		t.Fatal("history depends on deleted model route")
	}
}

func TestNonJobEngineErrorsAndDownloadErrorsDoNotExposeExceptions(t *testing.T) {
	for _, suffix := range []string{"", "/content"} {
		t.Run(suffix, func(t *testing.T) {
			h := host(t, "transport")
			id := "video_" + strings.Repeat("a", 32)
			binding := Binding{ID: id, Model: "wan", Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
			data, _ := json.Marshal(binding)
			_ = h.SetProperty([]string{bindingState}, data)
			_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
			c := h.InitializeHttpContext()
			h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "GET"}, {":path", "/v1/videos/" + id + suffix}}, true)
			h.CallOnResponseHeaders(c, [][2]string{{":status", "503"}, {"content-type", "application/json"}}, false)
			h.CallOnResponseBody(c, []byte(`{"error":{"message":"/private/model token=secret"}}`), true)
			body := string(h.GetCurrentResponseBody(c))
			if strings.Contains(body, "private") || strings.Contains(body, "secret") {
				t.Fatal(body)
			}
			if header(h.GetCurrentResponseHeaders(c), ":status") != "503" {
				t.Fatal("status changed")
			}
		})
	}
}
