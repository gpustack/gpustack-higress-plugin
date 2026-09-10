package main

import "testing"

// One payload shape per upstream the plugin actually sees. A miss here is not
// a crash but a NULL column, which reads exactly like "this endpoint has no id"
// and so would go unnoticed until someone tries to locate a record by it.
func TestUpstreamResponseID(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "openai chat completion chunk",
			payload: `{"id":"chatcmpl-Bx1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`,
			want:    "chatcmpl-Bx1",
		},
		{
			name:    "openai chat completion body",
			payload: `{"id":"chatcmpl-Bx2","object":"chat.completion","usage":{"total_tokens":9}}`,
			want:    "chatcmpl-Bx2",
		},
		{
			// The Responses API wraps the whole response object in its events,
			// so the id is one level down -- a top-level probe alone misses it.
			name:    "responses api event",
			payload: `{"type":"response.created","response":{"id":"resp_68a","status":"in_progress"}}`,
			want:    "resp_68a",
		},
		{
			name:    "responses api completed event",
			payload: `{"type":"response.completed","response":{"id":"resp_68b","usage":{"total_tokens":12}}}`,
			want:    "resp_68b",
		},
		{
			// Only message_start carries it; the deltas that follow do not,
			// which is why the captured value has to survive the whole stream.
			name:    "anthropic message_start",
			payload: `{"type":"message_start","message":{"id":"msg_01A","usage":{"input_tokens":4}}}`,
			want:    "msg_01A",
		},
		{
			name:    "anthropic message_delta carries none",
			payload: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			want:    "",
		},
		{
			name:    "openai embeddings shape has no id",
			payload: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"total_tokens":3}}`,
			want:    "",
		},
		{
			// vLLM does stamp one, and it is taken: an embeddings call has no
			// other id to locate its record by, so gating the probe on the
			// endpoint would discard it exactly where it is most needed. Same
			// shape as embeddingsBody() in decode_response_test.go.
			name:    "vllm embeddings shape carries one",
			payload: `{"id":"embd-1","object":"list","model":"qwen3-vl-embedding-2b","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"total_tokens":38}}`,
			want:    "embd-1",
		},
		{
			// Some rerank upstreams put a top-level numeric field next to
			// their results; gjson would render it as a string.
			name:    "top-level index is not an id",
			payload: `{"index":0,"results":[{"index":0,"relevance_score":0.9}]}`,
			want:    "",
		},
		{
			// Raw DashScope, which names it request_id. Unreachable while
			// ai-proxy converts the body (it runs ahead of this plugin on the
			// encode chain and maps request_id onto id), so this covers
			// `protocol: original` passthrough.
			name:    "raw dashscope text generation",
			payload: `{"request_id":"7574ee8f-38a3","output":{"choices":[{"finish_reason":"stop"}]},"usage":{"total_tokens":11}}`,
			want:    "7574ee8f-38a3",
		},
		{
			name:    "raw dashscope embedding",
			payload: `{"request_id":"1d3b5d0f-1e2a","output":{"embeddings":[{"text_index":0,"embedding":[0.1]}]},"usage":{"total_tokens":4}}`,
			want:    "1d3b5d0f-1e2a",
		},
		{
			// gjson would happily render a number as a string; an id that is
			// not a string is not this endpoint's response id.
			name:    "numeric id is not an id",
			payload: `{"id":42,"object":"list"}`,
			want:    "",
		},
		{
			name:    "empty string id is absent",
			payload: `{"id":""}`,
			want:    "",
		},
		{
			name:    "not json",
			payload: `[DONE]`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamResponseID([]byte(tc.payload)); got != tc.want {
				t.Errorf("upstreamResponseID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A chat-completions chunk has both a top-level id and, for some upstreams,
// nested objects that could carry one. The top-level probe must win, since
// that is the id the caller's SDK exposes.
//
// `request_id` is probed last for the same reason: an upstream that adds one
// beside a converted body's own `id` must not displace it.
func TestUpstreamResponseIDPrefersTheTopLevel(t *testing.T) {
	payload := `{"id":"chatcmpl-outer","request_id":"trace-1","response":{"id":"resp_inner"},"message":{"id":"msg_inner"}}`
	if got := upstreamResponseID([]byte(payload)); got != "chatcmpl-outer" {
		t.Errorf("upstreamResponseID() = %q, want the top-level id", got)
	}
}

// Absent has to stay distinguishable from empty on the wire: an embeddings
// call legitimately has no upstream response id, and a column of empty strings
// would read as though the id had arrived and been blank.
func TestOptionalStringAndInt64(t *testing.T) {
	if optionalString("") != nil {
		t.Error("an empty string must report as absent")
	}
	if got := optionalString("chatcmpl-1"); got == nil || *got != "chatcmpl-1" {
		t.Errorf("optionalString lost its value: %v", got)
	}
	if optionalInt64(nil) != nil {
		t.Error("an unset context value must report as absent")
	}
	if optionalInt64("42") != nil {
		t.Error("a non-int64 context value must report as absent, not panic")
	}
	// Zero is a real measurement -- a first chunk inside the same millisecond
	// as the request -- and must not collapse into "never measured".
	if got := optionalInt64(int64(0)); got == nil || *got != 0 {
		t.Errorf("optionalInt64(0) = %v, want a pointer to 0", got)
	}
}
