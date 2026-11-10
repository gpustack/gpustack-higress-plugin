package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// streamSimulator mirrors processTokenUsage's pipeline using the plugin's
// own functions, minus the proxywasm.Log* calls: processTokenUsage cannot
// run directly under `go test` because the proxy-wasm hostcall mock
// (internal.RegisterMockWasmHost) is not registrable from outside the SDK,
// and any Log* call panics on the nil host. The raw-byte reassembly itself
// is exercised via the real sseStreamBufferer; only the per-event rewrite
// loop is mirrored, so keep it in sync if that loop changes.
type streamSimulator struct {
	buf      sseStreamBufferer
	mergeBuf []byte // mirrors ctx IncompleteChunkData (mergeLargeUsageChunks)
}

func (s *streamSimulator) process(chunk []byte, endOfStream bool) []byte {
	pre, complete, post := s.buf.feed(chunk, endOfStream, 1024*1024)
	var out []byte
	if complete != nil {
		var rtn [][]byte
		for seg := range bytes.SplitSeq(complete, []byte("\n\n")) {
			out2, newBuf := mergeSSEEventState(s.mergeBuf, seg)
			if out2 == nil {
				s.mergeBuf = newBuf
				rtn = append(rtn, []byte(""))
				continue
			}
			s.mergeBuf = newBuf
			seg = out2
			prefix, payload, hasData := splitSSEEvent(seg)
			if !hasData || !json.Valid(payload) {
				rtn = append(rtn, seg)
				continue
			}
			if usageJSONPath(payload) == "" {
				rtn = append(rtn, seg)
				continue
			}
			// Usage chunk takes the rewrite path — exactly what
			// processTokenUsage does once getUsageExtra returns non-nil (the
			// payload carries usage.total_tokens > 0, so it always does for
			// this shape).
			rtn = append(rtn, reassembleSSEEvent(prefix, payload))
		}
		out = bytes.Join(rtn, []byte("\n\n"))
	}
	merged := make([]byte, 0, len(pre)+len(out)+len(post))
	merged = append(merged, pre...)
	merged = append(merged, out...)
	merged = append(merged, post...)
	return merged
}

// TestProcessTokenUsageChunkBoundaryTerminator reproduces the scenario from
// gpustack/gpustack#6748: a TCP/TLS delivery boundary lands inside the "\n\n"
// separator between the final usage event and the "[DONE]" event, so one
// Envoy body chunk carries the complete usage JSON plus a 1–4 byte prefix
// fragment of the *next* event's "data:" line (shorter than "data:" itself,
// hence with no "\n\n" and no "data:" prefix anywhere in the chunk).
//
// Expected downstream framing: data: {usage}\n\ndata: [DONE]\n\n — two
// separate SSE events. The assertions document the current (buggy) behavior:
// the fragment is reordered ahead of the usage event and the event
// terminator is dropped, gluing the two events into one line — exactly the
// malformed `data: {usage...`: [DONE]` bytes the issue observed on the wire.
func TestProcessTokenUsageChunkBoundaryTerminator(t *testing.T) {
	usageChunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	cases := []struct {
		name     string
		fragment string // bytes of the next event delivered in chunk 1
		tail     string // remainder of the next event delivered in chunk 2
	}{
		{"1-byte fragment", "d", "ata: [DONE]\n\n"},
		{"2-byte fragment", "da", "ta: [DONE]\n\n"},
		{"3-byte fragment", "dat", "a: [DONE]\n\n"},
		{"4-byte fragment", "data", ": [DONE]\n\n"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := &streamSimulator{}
			// vLLM ends the stream with "data: {usage}\n\ndata: [DONE]\n\n";
			// the delivery boundary cuts inside that stream right after the
			// fragment bytes, so chunk 1 = usage event + "\n\n" + fragment.
			out1 := sim.process([]byte(usageChunk+"\n\n"+c.fragment), false)
			out2 := sim.process([]byte(c.tail), true)
			wire := string(out1) + string(out2)
			if len(sim.buf.raw) != 0 || len(sim.mergeBuf) != 0 {
				t.Errorf("buffers not flushed at end of stream: raw=%q merge=%q", string(sim.buf.raw), string(sim.mergeBuf))
			}

			// The invariant any correct implementation must hold: bytes on
			// the wire remain framed as two separate SSE events.
			want := usageChunk + "\n\ndata: [DONE]\n\n"
			if wire != want {
				t.Errorf("glued/malformed SSE stream on the wire:\n got: %q\nwant: %q", wire, want)
			}
			if !strings.HasSuffix(wire, "\n\n") {
				t.Errorf("event terminator \\n\\n lost at chunk boundary: %q", wire)
			}
		})
	}
}

// TestProcessTokenUsageRawBufferScenarios covers the other raw-buffering
// shapes the fix must handle without corrupting the stream.
func TestProcessTokenUsageRawBufferScenarios(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	t.Run("event split mid-payload across chunks", func(t *testing.T) {
		head := delta[:len(delta)-10]
		tail := delta[len(delta)-10:] + "\n\n"
		sim := &streamSimulator{}
		out1 := sim.process([]byte(head), false)
		if len(out1) != 0 {
			t.Fatalf("incomplete event should be held, got %q", string(out1))
		}
		out2 := sim.process([]byte(tail), false)
		if string(out2) != delta+"\n\n" {
			t.Fatalf("reassembled event corrupted: %q", string(out2))
		}
	})

	t.Run("final event without trailing terminator flushed at end of stream", func(t *testing.T) {
		sim := &streamSimulator{}
		out1 := sim.process([]byte(delta+"\n\n"+usage), false)
		out2 := sim.process(nil, true)
		if string(out1)+string(out2) != delta+"\n\n"+usage {
			t.Fatalf("end-of-stream flush corrupted: %q + %q", string(out1), string(out2))
		}
	})

	t.Run("chunk containing no event boundary is fully held", func(t *testing.T) {
		sim := &streamSimulator{}
		out := sim.process([]byte(`data: {"par`), false)
		if len(out) != 0 || string(sim.buf.raw) != `data: {"par` {
			t.Fatalf("out=%q rawBuf=%q", string(out), string(sim.buf.raw))
		}
	})

	t.Run("three-way split of usage and DONE events", func(t *testing.T) {
		sim := &streamSimulator{}
		var wire string
		for i, part := range []string{usage + "\n\nd", "ata: [DO", "NE]\n\n"} {
			wire += string(sim.process([]byte(part), i == 2))
		}
		want := usage + "\n\ndata: [DONE]\n\n"
		if wire != want {
			t.Fatalf("wire = %q, want %q", wire, want)
		}
	})
}

// TestProcessTokenUsageCRLFSplitAcrossChunks guards against normalizing the
// held tail: a "\r" delivered alone at the end of a chunk is the first half
// of a CRLF; normalizing it to "\n" would fabricate an event boundary when
// the next chunk begins with "\n", splitting the event mid-JSON.
func TestProcessTokenUsageCRLFSplitAcrossChunks(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	rawStream := delta + "\r\n\r\n" + usage + "\r\n\r\n" + "data: [DONE]\r\n\r\n"

	sim := &streamSimulator{}
	// Cut after the "\r" of "hi"}'s CRLF terminator, and again inside the
	// next event: `...content":"hi"}}]}\r` | `\n\r` | `\ndata: {...}` ...
	cut1 := len(delta) + 1 // includes the lone trailing "\r"
	var wire string
	wire += string(sim.process([]byte(rawStream[:cut1]), false))
	wire += string(sim.process([]byte(rawStream[cut1:cut1+1]), false)) // the "\n"
	wire += string(sim.process([]byte(rawStream[cut1+1:]), true))

	want := delta + "\n\n" + usage + "\n\ndata: [DONE]\n\n"
	if wire != want {
		t.Fatalf("CRLF stream corrupted:\n got: %q\nwant: %q", wire, want)
	}
}

// TestProcessTokenUsageHeartbeatForwarding ensures complete SSE comment
// lines (": keep-alive") are forwarded immediately instead of being held
// until the next data event, so a client cannot time out during a long
// prefill while the upstream is still sending heartbeats.
func TestProcessTokenUsageHeartbeatForwarding(t *testing.T) {
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	sim := &streamSimulator{}
	out1 := sim.process([]byte(": keep-alive\n"), false)
	if string(out1) != ": keep-alive\n" {
		t.Fatalf("heartbeat not forwarded immediately: %q", string(out1))
	}
	out2 := sim.process([]byte(": keep-alive\r\n"), false)
	if string(out2) != ": keep-alive\r\n" {
		t.Fatalf("CRLF heartbeat not forwarded immediately: %q", string(out2))
	}
	// A partially delivered data line is still held.
	out3 := sim.process([]byte(usage[:20]), false)
	if len(out3) != 0 {
		t.Fatalf("partial data line should be held, got %q", string(out3))
	}
	wire := string(out1) + string(out2) + string(out3) + string(sim.process([]byte(usage[20:]+"\n\n"), true))
	want := ": keep-alive\n: keep-alive\r\n" + usage + "\n\n"
	if wire != want {
		t.Fatalf("wire = %q, want %q", wire, want)
	}
}

// TestProcessTokenUsageHeartbeatBlankLineSplit guards the split-heartbeat
// sequence ": keep-alive\n" | "\n" | ": keep-alive\n": the standalone blank
// line must flush immediately instead of piling up at the head of the held
// buffer and blocking every subsequent heartbeat.
func TestProcessTokenUsageHeartbeatBlankLineSplit(t *testing.T) {
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	sim := &streamSimulator{}
	var wire string
	wire += string(sim.process([]byte(": keep-alive\n"), false))
	wire += string(sim.process([]byte("\n"), false))
	wire += string(sim.process([]byte(": keep-alive\n"), false))
	wire += string(sim.process([]byte(usage+"\n\n"), true))

	want := ": keep-alive\n\n: keep-alive\n" + usage + "\n\n"
	if wire != want {
		t.Fatalf("split heartbeat starved downstream:\n got: %q\nwant: %q", wire, want)
	}
}

// TestProcessTokenUsageOversizedPassthrough ensures that once a pending
// event exceeds the buffer cap and is forwarded verbatim, its remaining
// fragments keep flowing verbatim until the event terminator arrives — the
// rewrite loop must never touch a partially-delivered oversized event.
func TestProcessTokenUsageOversizedPassthrough(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	// Small cap so the pending usage event (longer than the cap) trips the
	// overflow path; its terminator "\n\n" is split across callbacks.
	var b sseStreamBufferer
	p1, c1, q1 := b.feed([]byte(delta+"\n\n"+usage[:30]), false, len(usage)/2)
	if c1 == nil || string(c1) != delta+"\n\n" {
		t.Fatalf("complete prefix = %q", string(c1))
	}
	if p1 != nil || q1 != nil {
		t.Fatalf("nothing verbatim expected: pre=%q post=%q", string(p1), string(q1))
	}
	// Second piece makes the pending tail exceed the cap: the complete
	// prefix stays first and the oversized tail is emitted after it
	// (post), passthrough engaged. Order must remain prefix-then-tail.
	p2, c2, q2 := b.feed([]byte(usage[30:len(usage)-1]), false, len(usage)/2)
	if p2 != nil || c2 != nil || string(q2) != usage[:len(usage)-1] {
		t.Fatalf("oversized event mishandled: pre=%q complete=%q post=%q", string(p2), string(c2), string(q2))
	}
	if !b.pass {
		t.Fatalf("passthrough not engaged")
	}
	// The rest of the oversized event arrives fragment by fragment, still in
	// passthrough: its final "}", then the terminator's two newlines split
	// across callbacks (the first is held back so the "\n\n" is scanned as a
	// whole on the next callback).
	p3, c3, q3 := b.feed([]byte("}"), false, len(usage)/2)
	if c3 != nil || string(p3) != "}" || q3 != nil {
		t.Fatalf("passthrough fragment mishandled: pre=%q complete=%q post=%q", string(p3), string(c3), string(q3))
	}
	p4, c4, q4 := b.feed([]byte("\n"), false, len(usage)/2)
	if c4 != nil || q4 != nil || string(p4) != "" {
		t.Fatalf("terminator's first newline should be held one callback: pre=%q complete=%q post=%q", string(p4), string(c4), string(q4))
	}
	// Second "\n" plus the next event: boundary reached, passthrough ends and
	// the remainder is processed normally (and byte-faithfully).
	done := "data: [DONE]\n\n"
	p5, c5, q5 := b.feed([]byte("\n"+done), false, len(usage)/2)
	if string(p5) != "\n\n" || q5 != nil {
		t.Fatalf("verbatim terminator = %q post=%q", string(p5), string(q5))
	}
	if string(c5) != done {
		t.Fatalf("post-boundary remainder = %q", string(c5))
	}
	if b.pass || len(b.raw) != 0 {
		t.Fatalf("state not clean: pass=%v raw=%q", b.pass, string(b.raw))
	}
}

// TestProcessTokenUsageOversizedOrdering guards the emission order when an
// over-cap partial event shares a callback with a preceding complete event:
// the complete (rewritten) prefix must reach the client before the
// verbatim oversized tail.
func TestProcessTokenUsageOversizedOrdering(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	// The single callback holds a complete delta event plus an over-cap
	// pending usage event (cap is the simulator's 1 MiB, so drive overflow
	// through the raw bufferer with a tiny cap instead).
	var b sseStreamBufferer
	pre, complete, post := b.feed([]byte(delta+"\n\n"+usage), false, len(usage)-1)
	if string(complete) != delta+"\n\n" || pre != nil || string(post) != usage {
		t.Fatalf("order broken: pre=%q complete=%q post=%q", string(pre), string(complete), string(post))
	}
	if !b.pass {
		t.Fatalf("passthrough not engaged")
	}
	wire := string(pre) + string(complete) + string(post)
	if wire != delta+"\n\n"+usage {
		t.Fatalf("stream order corrupted: %q", wire)
	}
}

// TestProcessTokenUsageBareCRStream ensures a stream using bare-CR line
// endings (legal per the SSE spec) still reaches the client incrementally
// instead of accumulating until end-of-stream or the buffer cap.
func TestProcessTokenUsageBareCRStream(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	rawStream := delta + "\r\r" + usage + "\r\r"

	sim := &streamSimulator{}
	out1 := sim.process([]byte(rawStream[:len(delta)+2]), false) // event + full "\r\r"
	if string(out1) != delta+"\n\n" {
		t.Fatalf("bare-CR event not delivered incrementally: %q", string(out1))
	}
	wire := string(out1) + string(sim.process([]byte(rawStream[len(delta)+2:]), true))
	if wire != delta+"\n\n"+usage+"\n\n" {
		t.Fatalf("bare-CR stream corrupted:\n got: %q\nwant: %q", wire, delta+"\n\n"+usage+"\n\n")
	}
}

// TestProcessTokenUsagePassthroughCRLFSplit exercises the delimiter holdback
// in passthrough: a "\r\n\r\n" terminator split as "\r\n\r" | "\n" must still
// be recognized, so the event following the oversized one is processed
// normally instead of being swallowed by passthrough.
func TestProcessTokenUsagePassthroughCRLFSplit(t *testing.T) {
	delta := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	done := "data: [DONE]\r\n\r\n"

	// Oversized event (exceeds the tiny cap) using CRLF framing.
	var b sseStreamBufferer
	_, c1, _ := b.feed([]byte(delta+"\r\n\r\n"+usage[:30]), false, len(usage)/2)
	if c1 == nil {
		t.Fatalf("complete prefix missing")
	}
	p2, c2, q2 := b.feed([]byte(usage[30:]), false, len(usage)/2)
	if c2 != nil || p2 != nil {
		t.Fatalf("unexpected: pre=%q complete=%q", string(p2), string(c2))
	}
	if !b.pass {
		t.Fatalf("passthrough not engaged")
	}
	emitted := len(q2) - len(b.raw)
	rest := usage[emitted:]
	// Terminator split as "\r\n\r" | "\n" then the next event.
	p3, c3, _ := b.feed([]byte(rest+"\r\n\r"), false, len(usage)/2)
	if c3 != nil {
		t.Fatalf("passthrough leaked into rewrite loop: complete=%q", string(c3))
	}
	_ = p3
	p4, c4, _ := b.feed([]byte("\n"+done), false, len(usage)/2)
	if len(c4) == 0 || !bytes.Contains(c4, []byte("[DONE]")) {
		t.Fatalf("event after oversized one not processed: pre=%q complete=%q", string(p4), string(c4))
	}
	if b.pass {
		t.Fatalf("passthrough not released")
	}
}

// TestProcessTokenUsagePassthroughEOS ensures that when the stream ends
// while passthrough is active, the oversized event's terminator is still
// scanned so a final usage event after it gets processed normally.
func TestProcessTokenUsagePassthroughEOS(t *testing.T) {
	usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

	// Engage passthrough directly, then end the stream with the oversized
	// event's terminator followed by an ordinary final usage event.
	sim := &streamSimulator{}
	sim.buf.pass = true
	wire := string(sim.process([]byte("data: {\"oversized\""), false)) +
		string(sim.process([]byte(":payload}"), false)) +
		string(sim.process([]byte("\n\n"+usage), true))
	want := "data: {\"oversized\":payload}\n\n" + usage
	if wire != want {
		t.Fatalf("final usage event not processed at EOS during passthrough:\n got: %q\nwant: %q", wire, want)
	}
}

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
