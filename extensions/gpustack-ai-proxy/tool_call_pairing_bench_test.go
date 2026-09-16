// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
//
// Cost of the pairing walk relative to the reflection-based decode that
// dominates the Anthropic->OpenAI conversion (gpustack/gpustack#6217, where
// that decode measured ~1.9 s/MB + ~7.5 ms/message in the wasm sandbox).
// Absolute numbers here are native-Go and so are NOT comparable to the wasm
// figures in that issue; the *ratio* between the two benchmarks is the point.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// benchBody builds a Chat Completions body of `rounds` conversation rounds
// whose total size is close to targetBytes, shaped like a Claude Code history:
// every round is user -> assistant(tool_calls) -> tool -> assistant(text).
func benchBody(rounds, targetBytes int) []byte {
	// Four messages per round; pad the user content so the whole document
	// lands near targetBytes.
	const overheadPerRound = 420
	padding := (targetBytes / rounds) - overheadPerRound
	if padding < 0 {
		padding = 0
	}
	filler := strings.Repeat("x", padding)

	var sb strings.Builder
	sb.Grow(targetBytes + 1024)
	sb.WriteString(`{"model":"qwen3","stream":true,"messages":[`)
	for i := 0; i < rounds; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		id := fmt.Sprintf("call_%06d", i)
		fmt.Fprintf(&sb, `{"role":"user","content":"round %d %s"},`, i, filler)
		fmt.Fprintf(&sb, `{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}]},`, id)
		fmt.Fprintf(&sb, `{"role":"tool","tool_call_id":%q,"content":"22C, sunny"},`, id)
		fmt.Fprintf(&sb, `{"role":"assistant","content":"It is 22C in Shanghai (round %d)."}`, i)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func BenchmarkValidateToolCallPairing(b *testing.B) {
	for _, tc := range []struct {
		name   string
		rounds int
		bytes  int
	}{
		{"200rounds_1.2MB", 200, 1200 * 1024},
		{"400rounds_1.2MB", 400, 1200 * 1024},
		{"200rounds_2.4MB", 200, 2400 * 1024},
	} {
		body := benchBody(tc.rounds, tc.bytes)
		b.Run(tc.name+"/pairing_walk", func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if v := validateToolCallPairing(body); v != nil {
					b.Fatalf("generated body should be well formed, got %s", v.Detail)
				}
			}
		})
		// The baseline: what the Anthropic->OpenAI converter does to the same
		// document before it even starts transforming it.
		b.Run(tc.name+"/json_unmarshal_baseline", func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var req struct {
					Model    string        `json:"model"`
					Stream   bool          `json:"stream"`
					Messages []chatMessage `json:"messages"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// chatMessage mirrors the shape provider.chatMessage decodes into, so the
// baseline pays a comparable reflection cost.
type chatMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallId string `json:"tool_call_id"`
	ToolCalls  []struct {
		Id       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}
