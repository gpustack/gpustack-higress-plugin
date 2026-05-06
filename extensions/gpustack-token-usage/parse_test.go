package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"testing"
)

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

func TestParseConsumerHeader(t *testing.T) {
	cases := []struct {
		input     string
		wantUID   *int64
		wantAK    *string
	}{
		{"", nil, nil},
		{"none", nil, nil},
		{"None", nil, nil},
		{"NONE", nil, nil},
		{"gpustack-42", int64Ptr(42), nil},
		{"mykey.gpustack-42", int64Ptr(42), strPtr("mykey")},
		{"a.b.gpustack-42", int64Ptr(42), strPtr("a.b")},
		{".gpustack-42", int64Ptr(42), nil},
		{"mykey", nil, strPtr("mykey")},
		{"gpustack-abc", nil, nil}, // non-numeric id → both nil (invalid, won't be generated)
		{"gpustack-", nil, nil},    // empty id → both nil (invalid, won't be generated)
	}

	for _, c := range cases {
		uid, ak := parseConsumerHeader(c.input)

		if (uid == nil) != (c.wantUID == nil) || (uid != nil && *uid != *c.wantUID) {
			t.Errorf("parseConsumerHeader(%q) userID = %v, want %v", c.input, uid, c.wantUID)
		}
		if (ak == nil) != (c.wantAK == nil) || (ak != nil && *ak != *c.wantAK) {
			t.Errorf("parseConsumerHeader(%q) accessKey = %v, want %v", c.input, ak, c.wantAK)
		}
	}
}

func TestParseClusterName(t *testing.T) {
	cases := []struct {
		input          string
		wantModelID    *int64
		wantProviderID *int64
	}{
		// Envoy outbound format
		{"outbound|80||model-1-2.static", int64Ptr(1), nil},
		{"outbound|80||model-99-0.static", int64Ptr(99), nil},
		{"outbound|80||model-1-2.dns", int64Ptr(1), nil},
		{"outbound|80||provider-5.static", nil, int64Ptr(5)},
		{"outbound|80||provider-100.dns", nil, int64Ptr(100)},
		// Bare names (fallback, no pipe-separated prefix)
		{"model-1-2.static", int64Ptr(1), nil},
		{"provider-5.static", nil, int64Ptr(5)},
		// Invalid / unrelated
		{"", nil, nil},
		{"outbound|80||other-service.static", nil, nil},
		{"outbound|80||model-abc-2.static", nil, nil},  // non-numeric model id
		{"outbound|80||model-1.static", nil, nil},      // missing instance-id
		{"outbound|80||provider-5-x.static", nil, nil}, // extra dash
		{"outbound|80||provider-abc.static", nil, nil}, // non-numeric provider id
	}

	for _, c := range cases {
		mid, pid := parseClusterName(c.input)

		if (mid == nil) != (c.wantModelID == nil) || (mid != nil && *mid != *c.wantModelID) {
			t.Errorf("parseRouteName(%q) modelID = %v, want %v", c.input, mid, c.wantModelID)
		}
		if (pid == nil) != (c.wantProviderID == nil) || (pid != nil && *pid != *c.wantProviderID) {
			t.Errorf("parseRouteName(%q) providerID = %v, want %v", c.input, pid, c.wantProviderID)
		}
	}
}

func TestParseRouteName(t *testing.T) {
	cases := []struct {
		input    string
		wantID   *int64
	}{
		{"ai-route-route-1.internal", int64Ptr(1)},
		{"ai-route-route-42.internal", int64Ptr(42)},
		{"ai-route-route-7.fallback.internal", int64Ptr(7)},
		{"ai-route-route-100.fallback.internal", int64Ptr(100)},
		{"ai-route-route-1", int64Ptr(1)},        // suffix is optional
		// Invalid / unrelated
		{"", nil},
		{"ai-route-route-.internal", nil},        // empty id
		{"ai-route-route-abc.internal", nil},     // non-numeric id
		{"ai-route-route-", nil},                 // empty id, no suffix
		{"other-route-1.internal", nil},          // wrong prefix
	}

	for _, c := range cases {
		got := parseRouteName(c.input)
		if (got == nil) != (c.wantID == nil) || (got != nil && *got != *c.wantID) {
			t.Errorf("parseRouteName(%q) = %v, want %v", c.input, got, c.wantID)
		}
	}
}

func buildMultipartBody(t *testing.T, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, value := range fields {
		fw, err := w.CreateFormField(name)
		if err != nil {
			t.Fatalf("CreateFormField(%q): %v", name, err)
		}
		fmt.Fprint(fw, value)
	}
	w.Close()
	return buf.Bytes(), fmt.Sprintf("multipart/form-data; boundary=%s", w.Boundary())
}

func TestExtractModelFromMultipart(t *testing.T) {
	cases := []struct {
		name        string
		buildBody   func(t *testing.T) ([]byte, string)
		wantModel   string
	}{
		{
			name: "model field present",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": "gpt-4o", "input": "hello"})
			},
			wantModel: "gpt-4o",
		},
		{
			name: "model field with surrounding whitespace",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": "  whisper-1  "})
			},
			wantModel: "whisper-1",
		},
		{
			name: "model field absent",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"input": "hello"})
			},
			wantModel: "",
		},
		{
			name: "model field empty",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": ""})
			},
			wantModel: "",
		},
		{
			name: "invalid content-type (no boundary)",
			buildBody: func(t *testing.T) ([]byte, string) {
				body, _ := buildMultipartBody(t, map[string]string{"model": "m"})
				return body, "multipart/form-data"
			},
			wantModel: "",
		},
		{
			name: "body mismatches boundary",
			buildBody: func(t *testing.T) ([]byte, string) {
				return []byte("--wrongboundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt-4\r\n--wrongboundary--"), "multipart/form-data; boundary=correctboundary"
			},
			wantModel: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, ct := c.buildBody(t)
			got := extractModelFromMultipart(body, ct)
			if got != c.wantModel {
				t.Errorf("got %q, want %q", got, c.wantModel)
			}
		})
	}
}

func TestExtractRequestContentBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			name: "openai chat string content",
			body: `{"model":"gpt-4o","messages":[{"role":"system","content":"you are helpful"},{"role":"user","content":"hello"}]}`,
			want: int64(len("you are helpful") + len("hello")),
		},
		{
			name: "openai chat multimodal content array",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			want: int64(len("describe")),
		},
		{
			name: "anthropic with system field string + messages",
			body: `{"model":"claude","system":"act as expert","messages":[{"role":"user","content":"hi"}]}`,
			want: int64(len("act as expert") + len("hi")),
		},
		{
			name: "anthropic system as array of text blocks",
			body: `{"system":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}],"messages":[]}`,
			want: int64(len("part1") + len("part2")),
		},
		{
			name: "openai responses api input array",
			body: `{"input":[{"role":"user","content":"q"}]}`,
			want: int64(len("q")),
		},
		{
			name: "image-only content yields zero",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			want: 0,
		},
		{
			name: "empty body",
			body: `{}`,
			want: 0,
		},
		{
			name: "non-string content type without text block ignored",
			body: `{"messages":[{"role":"user","content":[{"type":"file","file":{"id":"f"}}]}]}`,
			want: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractRequestContentBytes([]byte(c.body))
			if got != c.want {
				t.Errorf("extractRequestContentBytes(%s) = %d, want %d", c.body, got, c.want)
			}
		})
	}
}

func TestIsOutputDeltaChunk(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		// OpenAI shapes
		{
			name: "openai delta with content text",
			body: `{"choices":[{"delta":{"content":"hi"}}]}`,
			want: true,
		},
		{
			name: "openai delta empty content",
			body: `{"choices":[{"delta":{"content":""}}]}`,
			want: false,
		},
		{
			name: "openai delta with tool_calls",
			body: `{"choices":[{"delta":{"tool_calls":[{"id":"t1","function":{"name":"x"}}]}}]}`,
			want: true,
		},
		{
			name: "openai delta with function_call",
			body: `{"choices":[{"delta":{"function_call":{"name":"x"}}}]}`,
			want: true,
		},
		{
			name: "openai usage-only chunk has empty choices",
			body: `{"choices":[],"usage":{"total_tokens":150}}`,
			want: false,
		},
		// Anthropic shapes
		{
			name: "anthropic content_block_delta with text",
			body: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
			want: true,
		},
		{
			name: "anthropic content_block_delta with empty text",
			body: `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`,
			want: false,
		},
		{
			name: "anthropic content_block_delta partial_json",
			body: `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
			want: true,
		},
		{
			name: "anthropic message_start (not a delta)",
			body: `{"type":"message_start","message":{"usage":{"input_tokens":50}}}`,
			want: false,
		},
		{
			name: "anthropic message_delta (not a content delta)",
			body: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isOutputDeltaChunk([]byte(c.body))
			if got != c.want {
				t.Errorf("isOutputDeltaChunk(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestIsOpenAIUsageOnlyChunk(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "canonical usage-only chunk",
			body: `{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`,
			want: true,
		},
		{
			name: "delta chunk with content",
			body: `{"choices":[{"delta":{"content":"hi"}}]}`,
			want: false,
		},
		{
			name: "non-empty choices with usage piggybacked",
			body: `{"choices":[{"delta":{}}],"usage":{"total_tokens":150}}`,
			want: false,
		},
		{
			name: "anthropic message_delta does not match",
			body: `{"type":"message_delta","usage":{"output_tokens":42}}`,
			want: false,
		},
		{
			name: "no choices field",
			body: `{"usage":{"total_tokens":150}}`,
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isOpenAIUsageOnlyChunk([]byte(c.body))
			if got != c.want {
				t.Errorf("isOpenAIUsageOnlyChunk(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}
