// GPUStack-local tests (no upstream counterpart) for request_priority.go.

package provider

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/upstream/jsonbody"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	wasmhost "github.com/higress-group/wasm-go/pkg/test"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeRequestPriorityTarget(t *testing.T) {
	cases := []struct {
		raw     string
		want    RequestPriorityTarget
		warning bool
	}{
		{"", RequestPriorityTargetNone, false},
		{"vllm", RequestPriorityTargetVllm, false},
		{" SGLang ", RequestPriorityTargetSglang, false},
		{"VLLM", RequestPriorityTargetVllm, false},
		{"torch", RequestPriorityTargetNone, true},
	}
	for _, c := range cases {
		got, warning := normalizeRequestPriorityTarget(c.raw)
		if got != c.want || (warning != "") != c.warning {
			t.Errorf("%q: normalize = (%q, %q), want (%q, warning=%v)", c.raw, got, warning, c.want, c.warning)
		}
	}
}

// The two backends order by opposite directions, so the same p must come out
// with opposite signs -- and 0 must stay 0 on both.
func TestBackendPriority(t *testing.T) {
	cases := []struct{ p, vllm, sglang int64 }{
		{0, 0, 0},
		{20, -20, 20},
		{5, -5, 5},
	}
	for _, c := range cases {
		if got := backendPriority(RequestPriorityTargetVllm, c.p); got != c.vllm {
			t.Errorf("vllm(%d) = %d, want %d", c.p, got, c.vllm)
		}
		if got := backendPriority(RequestPriorityTargetSglang, c.p); got != c.sglang {
			t.Errorf("sglang(%d) = %d, want %d", c.p, got, c.sglang)
		}
	}
}

func TestPriorityBody(t *testing.T) {
	sglang := RequestPriorityTargetSglang
	// Absent member is added.
	got := priorityBody(sglang, []byte(`{"model":"m","messages":[]}`), 20)
	if v := gjson.GetBytes(got, "priority").Int(); v != 20 {
		t.Errorf("priority = %d, want 20: %s", v, got)
	}
	// A client-supplied member is overwritten, whatever its value.
	got = priorityBody(sglang, []byte(`{"model":"m","priority":999}`), 7)
	if v := gjson.GetBytes(got, "priority").Int(); v != 7 {
		t.Errorf("client priority survived: %d, want 7: %s", v, got)
	}
	// 0 is written explicitly: SGLang treats an absent value as the lowest
	// priority, not the baseline.
	got = priorityBody(sglang, []byte(`{"model":"m"}`), 0)
	if !gjson.GetBytes(got, "priority").Exists() || gjson.GetBytes(got, "priority").Int() != 0 {
		t.Errorf("priority 0 must be written explicitly: %s", got)
	}
	// vLLM reads the body member on its pooling endpoints and in the same
	// direction as its header: negated, 0 stays 0, a client value is replaced.
	got = priorityBody(RequestPriorityTargetVllm, []byte(`{"model":"m","input":"x","priority":5}`), 20)
	if v := gjson.GetBytes(got, "priority").Int(); v != -20 {
		t.Errorf("vllm body priority = %d, want -20: %s", v, got)
	}
	got = priorityBody(RequestPriorityTargetVllm, []byte(`{"model":"m","priority":-999}`), 0)
	if v := gjson.GetBytes(got, "priority").Int(); v != 0 {
		t.Errorf("vllm body priority 0 = %d, want 0: %s", v, got)
	}
	// Non-JSON is returned untouched -- sjson would otherwise build a fresh
	// `{"priority":N}` object around the value and drop the original bytes.
	// A JSON array is not a member carrier either.
	for _, raw := range [][]byte{[]byte("not json"), []byte(`[1,2,3]`), []byte(`"str"`), nil} {
		if got := priorityBody(sglang, raw, 20); string(got) != string(raw) {
			t.Errorf("non-object body was modified: %q -> %s", raw, got)
		}
	}
	// Duplicate members: sjson touches one occurrence, so a body carrying
	// two must end with exactly one, whatever a last-wins decoder does.
	// (The duplicate check in onHttpRequestBody covers `model` only.)
	got = priorityBody(sglang, []byte(`{"model":"m","priority":0,"priority":999}`), 20)
	if dup, err := jsonbody.DuplicateTopLevelMember(got, "priority"); err != nil || dup {
		t.Errorf("duplicate priority members survived (dup=%v, err=%v): %s", dup, err, got)
	}
	var lastWins struct {
		Priority int64 `json:"priority"`
	}
	if err := json.Unmarshal(got, &lastWins); err != nil || lastWins.Priority != 20 {
		t.Errorf("last-wins decode sees %d (err=%v), want 20: %s", lastWins.Priority, err, got)
	}
	// Many duplicates: still one member, everything else intact. Removal is
	// a single pass, not one sjson round trip per copy.
	many := []byte(`{"model":"m","messages":[{"role":"user","content":"a","priority":"nested"}],"nested":{"priority":1},`)
	for i := 0; i < 2000; i++ {
		many = append(many, `"priority":3,`...)
	}
	many = append(many, `"stream":false}`...)
	got = priorityBody(sglang, many, 20)
	if dup, err := jsonbody.DuplicateTopLevelMember(got, "priority"); err != nil || dup {
		t.Errorf("many duplicates: dup=%v err=%v", dup, err)
	}
	if v := gjson.GetBytes(got, "priority").Int(); v != 20 {
		t.Errorf("many duplicates: priority = %d, want 20", v)
	}
	if v := gjson.GetBytes(got, "messages.0.priority").String(); v != "nested" {
		t.Errorf("many duplicates: nested member was lost: %s", got[:min(len(got), 120)])
	}
	if v := gjson.GetBytes(got, "nested.priority").Int(); v != 1 {
		t.Errorf("many duplicates: nested object was lost: %s", got[:min(len(got), 120)])
	}
	if !gjson.GetBytes(got, "stream").Exists() {
		t.Errorf("many duplicates: trailing member was lost")
	}
	// countTopLevelMember / stripTopLevelMember: top level only, key order
	// and raw values preserved, nil when nothing was removed.
	body := []byte(` {"a" : 1, "priority": 5, "b": {"priority": 7, "c": [1, 2]}, "priority": "x"} `)
	if n := countTopLevelMember(body, "priority"); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if n := countTopLevelMember([]byte(`[1]`), "priority"); n != 0 {
		t.Errorf("count on array = %d, want 0", n)
	}
	stripped := stripTopLevelMember(body, "priority")
	// Values are copied verbatim (spacing inside a nested value survives);
	// the top-level framing is rebuilt from gjson's normalised key raws.
	if want := `{"a":1,"b":{"priority": 7, "c": [1, 2]}}`; string(stripped) != want {
		t.Errorf("strip = %s, want %s", stripped, want)
	}
	if v := gjson.GetBytes(stripped, "a").Int(); v != 1 || gjson.GetBytes(stripped, "b.priority").Int() != 7 ||
		gjson.GetBytes(stripped, "priority").Exists() || !gjson.GetBytes(stripped, "b.c.1").Exists() {
		t.Errorf("strip lost or kept the wrong members: %s", stripped)
	}
	if stripTopLevelMember([]byte(`{"a":1}`), "priority") != nil {
		t.Errorf("strip must return nil when nothing was removed")
	}
	if stripTopLevelMember([]byte(`{"a":1,"priority":2}`), "priority") == nil {
		t.Errorf("strip must not return nil when something was removed")
	}
}

// The retry path rebuilds headers from scratch and replays the pre-phase
// body, so both forms have to be re-established from the ctx value.
func TestRequestPriorityRetry(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[],"priority":999}`)
	headers := func() http.Header { return http.Header{":Path": []string{"/v1/chat/completions"}} }

	gotHeaders, gotBody := (&ProviderConfig{requestPriorityTarget: RequestPriorityTargetVllm}).requestPriorityRetry(20, headers(), chat)
	if v := gotHeaders.Get(HeaderVllmPriority); v != "-20" {
		t.Errorf("vllm retry header = %q, want -20", v)
	}
	if v := gjson.GetBytes(gotBody, "priority").Int(); v != -20 {
		t.Errorf("vllm retry body priority = %d, want -20: %s", v, gotBody)
	}

	gotHeaders, gotBody = (&ProviderConfig{requestPriorityTarget: RequestPriorityTargetSglang}).requestPriorityRetry(20, headers(), chat)
	if v := gjson.GetBytes(gotBody, "priority").Int(); v != 20 {
		t.Errorf("sglang retry body priority = %d, want 20: %s", v, gotBody)
	}
	if v := gotHeaders.Get(HeaderVllmPriority); v != "" {
		t.Errorf("sglang retry must not write the vllm header, got %q", v)
	}

	// A provider without a target gets neither, which is also what the
	// retry looked like before the feature existed.
	gotHeaders, gotBody = (&ProviderConfig{}).requestPriorityRetry(20, headers(), chat)
	if len(gotHeaders) != 1 || string(gotBody) != string(chat) {
		t.Errorf("no-target retry was modified: %v %s", gotHeaders, gotBody)
	}
}

// hostVMContext is the minimum a host emulator needs from the plugin side
// to open an HTTP context in a library package test: the main package gets
// this from wrapper.SetCtx in its init().
type hostVMContext struct{ types.DefaultVMContext }

func (v *hostVMContext) NewPluginContext(uint32) types.PluginContext { return &hostPluginContext{} }

type hostPluginContext struct{ types.DefaultPluginContext }

func (p *hostPluginContext) NewHttpContext(uint32) types.HttpContext {
	return &types.DefaultHttpContext{}
}

// provider.context inserts a system message by rebuilding the body from the
// provider's request schema, which has no `priority` member; the cached
// path is synchronous, so it runs under the in-process host without an
// HTTP call. The host emulator is created first because go mode borrows
// its VM context for the host calls (replaceRequestBody, GetHttpRequestBody).
func TestContextInsertionKeepsSglangPriority(t *testing.T) {
	proxywasm.SetVMContext(&hostVMContext{})
	host, status := wasmhost.NewTestHost(json.RawMessage(`{}`))
	defer host.Reset()
	require.Equal(t, types.OnPluginStartStatusOK, status)
	host.InitHttp()
	// Opening the request headers phase is what makes the stream the
	// emulator's active one, i.e. the one host calls act on.
	host.CallOnHttpRequestHeaders([][2]string{{":path", "/v1/chat/completions"}, {"content-type", "application/json"}})

	wasmhost.RunGoTest(t, func(t *testing.T) {
		pc := &ProviderConfig{typ: providerTypeOpenAI, requestPriorityTarget: RequestPriorityTargetSglang}
		cache := &contextCache{loaded: true, content: "file context", config: pc}
		ctx := createHttpContext()
		ctx.SetContext(CtxKeyRequestPriority, int64(20))
		p := &openaiProvider{config: *pc}

		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"priority":20}`)
		cache.insertContext(ctx, p, cache.content, nil, body)

		got, err := proxywasm.GetHttpRequestBody(0, 1<<20)
		require.NoError(t, err)
		require.Equal(t, int64(20), gjson.GetBytes(got, "priority").Int(), "priority lost in the rebuild: %s", got)
		require.Equal(t, "system", gjson.GetBytes(got, "messages.0.role").String(), "the context message itself went missing: %s", got)
	})
}

func TestRequestPriorityBodyGates(t *testing.T) {
	// Only JSON-bearing content types qualify; multipart never does.
	for _, ct := range []string{"", "application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if !requestPriorityBodyContentType(ct) {
			t.Errorf("content type %q should qualify", ct)
		}
	}
	for _, ct := range []string{"multipart/form-data; boundary=x", "text/plain", "audio/wav"} {
		if requestPriorityBodyContentType(ct) {
			t.Errorf("content type %q should not qualify", ct)
		}
	}

	// SGLang declares the member on its generation and pooling endpoints, and
	// its Anthropic endpoint has none.
	for _, api := range []ApiName{ApiNameChatCompletion, ApiNameCompletion, ApiNameResponses, ApiNameEmbeddings} {
		if !requestPriorityBodyTarget(api) {
			t.Errorf("%s should carry a priority", api)
		}
	}
	for _, api := range []ApiName{ApiNameAnthropicMessages, ApiNameImageGeneration, ""} {
		if requestPriorityBodyTarget(api) {
			t.Errorf("%s should not carry a priority", api)
		}
	}
}

// Well-formed values only: a malformed one is logged through wasm-go's
// logger, which is nil under plain `go test`; that path is covered on the
// in-process host in request_priority_integration_test.go.
func TestParseRequestPriority(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0}, {"20", 20}, {" 7 ", 7}, {"0", 0},
	}
	for _, c := range cases {
		if got := parseRequestPriority(c.raw); got != c.want {
			t.Errorf("parse(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}
