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
