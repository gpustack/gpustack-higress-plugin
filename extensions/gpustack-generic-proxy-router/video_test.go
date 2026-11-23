package main

import (
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func TestAsyncVideoCannotReachCatchAllModelRouteWithNativeID(t *testing.T) {
	vm := wrapper.NewCommonVmCtx(pluginName, wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders))
	host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().
		WithVMContext(vm).WithPluginConfiguration([]byte(`{"videoJobs":true,"aliasNameMapping":{"9":"wan"}}`)))
	t.Cleanup(reset)
	host.RegisterForeignFunction("get_log_level", func([]byte) []byte { return []byte{0, 0, 0, 0} })
	if host.StartVM() != types.OnVMStartStatusOK || host.StartPlugin() != types.OnPluginStartStatusOK {
		t.Fatal("model router failed to start")
	}
	for _, test := range []struct{ path, fallback string }{
		{"/model/proxy/9/v1/videos/native-id", ""},
		{"/MODEL/PROXY/9/V1/VIDEOS/native-id/content", ""},
		{"/v1/videos", ""},
		{"/model/proxy/9/v1/chat/completions", "/v1/videos/native-id"},
	} {
		context := host.InitializeHttpContext()
		host.CallOnRequestHeaders(context, [][2]string{
			{":authority", "gateway.test"}, {":method", "GET"}, {":path", test.path},
			{"x-higress-llm-model", "wan"},
			{"x-gpustack-fallback-path", test.fallback},
		}, true)
		if reply := host.GetSentLocalResponse(context); reply == nil || reply.StatusCode != 404 {
			t.Fatalf("unbound task request was forwarded: %s, %+v", test.path, reply)
		}
	}
}

func TestVideoRoutingAndOrdinaryFallbackKeepTheirOwnTargets(t *testing.T) {
	vm := wrapper.NewCommonVmCtx(pluginName, wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders), wrapper.ProcessRequestBody(onHttpRequestBody))
	host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().
		WithVMContext(vm).WithPluginConfiguration([]byte(`{"videoJobs":true}`)))
	t.Cleanup(reset)
	host.RegisterForeignFunction("get_log_level", func([]byte) []byte { return []byte{0, 0, 0, 0} })
	if host.StartVM() != types.OnVMStartStatusOK || host.StartPlugin() != types.OnPluginStartStatusOK {
		t.Fatal("model router failed to start")
	}
	for _, method := range []string{"GET", "DELETE"} {
		context := host.InitializeHttpContext()
		host.CallOnRequestHeaders(context, [][2]string{
			{":authority", "gateway.test"}, {":method", method}, {":path", "/v1/videos/video_0123456789abcdef0123456789abcdef"},
			{"x-higress-fallback-from", "failed-cluster"},
			{"x-higress-target-cluster", "caller-target"}, {"x-higress-llm-model", "caller-model"},
		}, true)
		headers := map[string]string{}
		for _, header := range host.GetCurrentRequestHeaders(context) {
			headers[header[0]] = header[1]
		}
		if headers["x-gpustack-video-bootstrap"] != "1" || headers["x-higress-target-cluster"] != "" || headers["x-higress-llm-model"] != "" {
			t.Fatalf("video request kept an untrusted fallback target: %+v", headers)
		}
	}
	context := host.InitializeHttpContext()
	host.CallOnRequestHeaders(context, [][2]string{
		{":authority", "gateway.test"}, {":method", "POST"}, {":path", "/v1/chat/completions"},
		{"content-type", "application/json"}, {"x-higress-fallback-from", "failed-cluster"},
		{"x-higress-target-cluster", "next-cluster"}, {"x-higress-llm-model", "public-alias"},
	}, false)
	body := []byte(`{"model":"failed-backend","messages":[]}`)
	host.CallOnRequestBody(context, body, true)
	headers := map[string]string{}
	for _, header := range host.GetCurrentRequestHeaders(context) {
		headers[header[0]] = header[1]
	}
	if headers["x-higress-llm-model"] != "public-alias" || headers["x-higress-target-cluster"] != "next-cluster" || headers["x-gpustack-video-bootstrap"] != "" {
		t.Fatalf("ordinary fallback lost its selected route: %+v", headers)
	}
	if string(host.GetCurrentRequestBody(context)) != string(body) {
		t.Fatal("ordinary fallback rewrote the backend model a second time")
	}
}
