package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSplitSSEEvent(t *testing.T) {
	cases := []struct {
		name        string
		block       string
		wantPrefix  string
		wantPayload string
		wantHasData bool
	}{
		{
			name:        "plain data line (chat completions)",
			block:       `data: {"choices":[{"delta":{"content":"hi"}}]}`,
			wantPrefix:  "",
			wantPayload: `{"choices":[{"delta":{"content":"hi"}}]}`,
			wantHasData: true,
		},
		{
			name:        "data line without space after colon",
			block:       `data:{"a":1}`,
			wantPrefix:  "",
			wantPayload: `{"a":1}`,
			wantHasData: true,
		},
		{
			name:        "event + data (responses api)",
			block:       "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0}",
			wantPrefix:  "event: response.output_item.added",
			wantPayload: `{"type":"response.output_item.added","output_index":0}`,
			wantHasData: true,
		},
		{
			name:        "event + id + data",
			block:       "event: response.completed\nid: evt_1\ndata: {\"response\":{\"usage\":{\"total_tokens\":10}}}",
			wantPrefix:  "event: response.completed\nid: evt_1",
			wantPayload: `{"response":{"usage":{"total_tokens":10}}}`,
			wantHasData: true,
		},
		{
			name:        "event + id + retry + comment + data all preserved",
			block:       ": keep-alive\nevent: message\nid: 42\nretry: 10000\ndata: {\"x\":1}",
			wantPrefix:  ": keep-alive\nevent: message\nid: 42\nretry: 10000",
			wantPayload: `{"x":1}`,
			wantHasData: true,
		},
		{
			name:        "id and retry without data (no payload)",
			block:       "id: 99\nretry: 3000",
			wantHasData: false,
		},
		{
			name:        "comment-only block (no data)",
			block:       ": keep-alive",
			wantHasData: false,
		},
		{
			name:        "bare event line (no data)",
			block:       "event: ping",
			wantHasData: false,
		},
		{
			name:        "non-sse json body (rate-limit rejection)",
			block:       `{"error":{"message":"Too many requests","type":"rate_limit_exceeded"}}`,
			wantHasData: false,
		},
		{
			name:        "multiple data lines join with newline",
			block:       "data: line1\ndata: line2",
			wantPrefix:  "",
			wantPayload: "line1\nline2",
			wantHasData: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prefix, payload, hasData := splitSSEEvent([]byte(c.block))
			if hasData != c.wantHasData {
				t.Fatalf("hasData = %v, want %v", hasData, c.wantHasData)
			}
			if !hasData {
				return
			}
			if string(prefix) != c.wantPrefix {
				t.Errorf("prefix = %q, want %q", string(prefix), c.wantPrefix)
			}
			if string(payload) != c.wantPayload {
				t.Errorf("payload = %q, want %q", string(payload), c.wantPayload)
			}
		})
	}
}

func TestReassembleSSEEvent(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		payload string
		want    string
	}{
		{
			name:    "no prefix yields plain data line",
			prefix:  "",
			payload: `{"a":1}`,
			want:    `data: {"a":1}`,
		},
		{
			name:    "event prefix preserved",
			prefix:  "event: response.completed",
			payload: `{"response":{"usage":{}}}`,
			want:    "event: response.completed\ndata: {\"response\":{\"usage\":{}}}",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reassembleSSEEvent([]byte(c.prefix), []byte(c.payload))
			if string(got) != c.want {
				t.Errorf("got %q, want %q", string(got), c.want)
			}
		})
	}
}

// splitSSEEvent then reassembleSSEEvent is the round-trip used by the rewrite
// path; for an unmodified payload it must reproduce the original block.
func TestSSEEventRoundTrip(t *testing.T) {
	blocks := []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		"event: response.completed\ndata: {\"response\":{\"usage\":{\"total_tokens\":10}}}",
		"event: response.output_text.delta\nid: evt_9\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}",
		"event: message\nid: 42\nretry: 10000\ndata: {\"x\":1}",
		// Multi-line data: each line must keep its own "data: " prefix after
		// reassembly (SSE concatenates them with \n on the client side).
		"data: line1\ndata: line2",
		"event: multi\ndata: a\ndata: b\ndata: c",
	}
	for _, block := range blocks {
		prefix, payload, hasData := splitSSEEvent([]byte(block))
		if !hasData {
			t.Fatalf("expected data for %q", block)
		}
		got := reassembleSSEEvent(prefix, payload)
		if string(got) != block {
			t.Errorf("round-trip mismatch:\n got %q\nwant %q", string(got), block)
		}
	}
}

func TestUsageJSONPath(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "openai chat top-level usage",
			payload: `{"choices":[],"usage":{"total_tokens":150}}`,
			want:    "usage",
		},
		{
			name:    "anthropic message_delta top-level usage",
			payload: `{"type":"message_delta","usage":{"output_tokens":42}}`,
			want:    "usage",
		},
		{
			name:    "openai responses nested usage",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`,
			want:    "response.usage",
		},
		{
			name:    "responses delta without usage",
			payload: `{"type":"response.output_text.delta","delta":"hello"}`,
			want:    "",
		},
		{
			name:    "anthropic message_start carries message.usage, not top-level",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":50}}}`,
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := usageJSONPath([]byte(c.payload)); got != c.want {
				t.Errorf("usageJSONPath(%s) = %q, want %q", c.payload, got, c.want)
			}
		})
	}
}

// process_data_with_token must inject the extras at response.usage for the
// Responses API shape, leaving the rest of the envelope intact.
func TestProcessDataWithTokenResponsesAPI(t *testing.T) {
	origin := `{"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`
	result := process_data_with_token([]byte(origin), "response.usage", map[string]any{
		"time_to_first_token_ms":   int64(100),
		"time_per_output_token_ms": float64(12.5),
		"tokens_per_second":        float64(80.0),
	})

	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	resp, ok := m["response"].(map[string]any)
	if !ok {
		t.Fatalf("response field missing")
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("response.usage field missing")
	}
	if usage["time_to_first_token_ms"] != float64(100) {
		t.Errorf("time_to_first_token_ms = %v, want 100", usage["time_to_first_token_ms"])
	}
	if usage["tokens_per_second"] != float64(80.0) {
		t.Errorf("tokens_per_second = %v, want 80", usage["tokens_per_second"])
	}
	// original fields preserved
	if usage["total_tokens"] != float64(12) {
		t.Errorf("total_tokens = %v, want 12", usage["total_tokens"])
	}
	if resp["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", resp["model"])
	}
}

func TestIsOutputDeltaChunkResponsesAPI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "responses output_text delta",
			body: `{"type":"response.output_text.delta","delta":"hello"}`,
			want: true,
		},
		{
			name: "responses function_call_arguments delta",
			body: `{"type":"response.function_call_arguments.delta","delta":"{\"a\":"}`,
			want: true,
		},
		{
			name: "responses empty delta",
			body: `{"type":"response.output_text.delta","delta":""}`,
			want: false,
		},
		{
			name: "responses output_item.added is not a delta",
			body: `{"type":"response.output_item.added","output_index":0}`,
			want: false,
		},
		{
			name: "responses completed is not a delta",
			body: `{"type":"response.completed","response":{"usage":{"total_tokens":10}}}`,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOutputDeltaChunk([]byte(c.body)); got != c.want {
				t.Errorf("isOutputDeltaChunk(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// Guard: the prefix detached from a data line never accidentally leaks the
// "data:" token, which would double-prefix on reassembly.
func TestSplitSSEEventNoDataLeakIntoPrefix(t *testing.T) {
	block := "event: response.completed\ndata: {\"k\":\"v\"}"
	prefix, _, _ := splitSSEEvent([]byte(block))
	if bytes.Contains(prefix, []byte("data:")) {
		t.Errorf("prefix unexpectedly contains data line: %q", string(prefix))
	}
}

func TestMergeSSEEventState(t *testing.T) {
	t.Run("complete chat-completions block passes through", func(t *testing.T) {
		block := `data: {"choices":[{"delta":{"content":"hi"}}]}`
		out, buf := mergeSSEEventState(nil, []byte(block))
		if string(out) != block || buf != nil {
			t.Fatalf("out=%q buf=%q", string(out), string(buf))
		}
	})

	t.Run("complete responses event passes through", func(t *testing.T) {
		block := "event: response.completed\ndata: {\"response\":{\"usage\":{\"total_tokens\":10}}}"
		out, buf := mergeSSEEventState(nil, []byte(block))
		if string(out) != block || buf != nil {
			t.Fatalf("out=%q buf=%q", string(out), string(buf))
		}
	})

	t.Run("non-sse json body (rejection) passes through, never buffered", func(t *testing.T) {
		block := `{"error":{"message":"Too many requests"}}`
		out, buf := mergeSSEEventState(nil, []byte(block))
		if string(out) != block || buf != nil {
			t.Fatalf("out=%q buf=%q", string(out), string(buf))
		}
	})

	t.Run("event-only control line passes through, never eaten", func(t *testing.T) {
		block := "event: ping"
		out, buf := mergeSSEEventState(nil, []byte(block))
		if string(out) != block || buf != nil {
			t.Fatalf("out=%q buf=%q", string(out), string(buf))
		}
	})

	t.Run("[DONE] terminator passes through, never buffered", func(t *testing.T) {
		for _, block := range []string{"data: [DONE]", "data:[DONE]", "data: [DONE] "} {
			out, buf := mergeSSEEventState(nil, []byte(block))
			if string(out) != block || buf != nil {
				t.Fatalf("block %q: out=%q buf=%q", block, string(out), string(buf))
			}
		}
	})

	// The key Responses-API scenario: a large "response.completed" event whose
	// data payload is split across two Envoy delivery chunks. The first half is
	// buffered (with its event-type prefix); the continuation completes it and
	// the event is emitted intact.
	t.Run("responses event split across two envoy chunks", func(t *testing.T) {
		head := "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_"
		tail := `tokens":5,"output_tokens":7,"total_tokens":12}}}`

		out, buf := mergeSSEEventState(nil, []byte(head))
		if out != nil {
			t.Fatalf("expected nil out while accumulating, got %q", string(out))
		}
		if len(buf) == 0 {
			t.Fatalf("expected buffered head")
		}

		out, buf2 := mergeSSEEventState(buf, []byte(tail))
		if buf2 != nil {
			t.Fatalf("expected cleared buffer, got %q", string(buf2))
		}
		want := head + tail
		if string(out) != want {
			t.Fatalf("reassembled = %q, want %q", string(out), want)
		}
		// And the reassembled block is parseable as a complete responses event.
		_, payload, hasData := splitSSEEvent(out)
		if !hasData || usageJSONPath(payload) != "response.usage" {
			t.Fatalf("reassembled event not recognized: hasData=%v payload=%q", hasData, string(payload))
		}
	})

	t.Run("split across three envoy chunks", func(t *testing.T) {
		parts := []string{
			"event: response.completed\ndata: {\"response\":{\"usage\":{",
			`"input_tokens":5,`,
			`"output_tokens":7,"total_tokens":12}}}`,
		}
		var buf []byte
		var out []byte
		for i, p := range parts {
			out, buf = mergeSSEEventState(buf, []byte(p))
			if i < len(parts)-1 {
				if out != nil {
					t.Fatalf("part %d: expected nil out, got %q", i, string(out))
				}
			}
		}
		if buf != nil {
			t.Fatalf("expected cleared buffer at end, got %q", string(buf))
		}
		_, payload, _ := splitSSEEvent(out)
		if usageJSONPath(payload) != "response.usage" {
			t.Fatalf("final payload not a usage event: %q", string(payload))
		}
	})
}
