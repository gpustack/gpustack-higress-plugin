package main

import (
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func TestNativeVideoTransportDoesNotWrapEngineResponse(t *testing.T) {
	vm := wrapper.NewCommonVmCtx(pluginName, wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHeaders), wrapper.ProcessRequestBody(onBody))
	host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().WithVMContext(vm).
		WithPluginConfiguration([]byte(`{}`)))
	t.Cleanup(reset)
	host.RegisterForeignFunction("get_log_level", func([]byte) []byte { return []byte{0, 0, 0, 0} })
	if host.StartVM() != types.OnVMStartStatusOK || host.StartPlugin() != types.OnPluginStartStatusOK {
		t.Fatal("video plugin failed to start")
	}
	context := host.InitializeHttpContext()
	host.CallOnRequestHeaders(context, [][2]string{{":method", "POST"}, {":path", "/v1/videos"}, {"content-type", "application/json"}}, false)
	request := []byte(`{"model":"wan","prompt":"A city","stream":false}`)
	host.CallOnRequestBody(context, request, true)
	if reply := host.GetSentLocalResponse(context); reply != nil {
		t.Fatalf("unexpected control response: %+v", reply)
	}
	if string(host.GetCurrentRequestBody(context)) != string(request) {
		t.Fatal("an ordinary video request should remain intact")
	}
	response := []byte(`{"id":"native-id","status":"queued"}`)
	host.CallOnResponseHeaders(context, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
	host.CallOnResponseBody(context, response, true)
	if string(host.GetCurrentResponseBody(context)) != string(response) {
		t.Fatal("the native task ID was changed")
	}
}
