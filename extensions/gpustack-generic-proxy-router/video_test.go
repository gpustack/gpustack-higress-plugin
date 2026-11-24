package main

import (
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func TestNativeVideoUsesModelRouteWithoutAcceptingClientCluster(t *testing.T) {
	vm := wrapper.NewCommonVmCtx(pluginName, wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders))
	host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().
		WithVMContext(vm).WithPluginConfiguration([]byte(`{"videoRoutes":true}`)))
	t.Cleanup(reset)
	host.RegisterForeignFunction("get_log_level", func([]byte) []byte { return []byte{0, 0, 0, 0} })
	if host.StartVM() != types.OnVMStartStatusOK || host.StartPlugin() != types.OnPluginStartStatusOK {
		t.Fatal("model router failed to start")
	}
	for _, test := range []struct {
		method, path, model string
		status              uint32
	}{
		{"GET", "/v1/videos/native-id", "org/wan", 0},
		{"GET", "/v1/videos/native-id/content", "org/wan", 0},
		{"DELETE", "/v1/videos/native-id", "org/wan", 0},
		{"GET", "/v1/videos/native-id", "", 400},
		{"GET", "/v1/videos", "org/wan", 404},
		{"GET", "/v1/videos/sync", "org/wan", 404},
		{"DELETE", "/v1/videos/sync", "org/wan", 404},
	} {
		context := host.InitializeHttpContext()
		host.CallOnRequestHeaders(context, [][2]string{
			{":authority", "gateway.test"}, {":method", test.method}, {":path", test.path},
			{"x-gpustack-model", test.model}, {"x-higress-target-cluster", "injected"},
		}, true)
		reply := host.GetSentLocalResponse(context)
		if test.status != 0 {
			if reply == nil || reply.StatusCode != test.status {
				t.Fatalf("unexpected rejection for %s: %+v", test.path, reply)
			}
			continue
		}
		headers := map[string]string{}
		for _, header := range host.GetCurrentRequestHeaders(context) {
			headers[header[0]] = header[1]
		}
		if reply != nil || headers["x-higress-llm-model"] != test.model || headers["x-higress-target-cluster"] != "" {
			t.Fatalf("unexpected video route: %+v, %+v", reply, headers)
		}
	}
}
