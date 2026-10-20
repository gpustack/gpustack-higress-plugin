// GPUStack-local test (no upstream counterpart): guards the max_tokens →
// max_completion_tokens normalization helpers in max_completion_tokens.go,
// currently wired into the openai provider.
//
// Per the OpenAI Chat Completions API spec, the o-series and gpt-5+ reasoning
// models reject the deprecated max_tokens parameter, so the proxy rewrites it
// to max_completion_tokens for those models while leaving legacy models (where
// max_tokens remains the documented parameter) untouched. See
// gpustack/gpustack#6505.

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequiresMaxCompletionTokens(t *testing.T) {
	openAITests := []struct {
		model string
		want  bool
	}{
		// Reasoning families per the OpenAI spec — max_tokens rejected.
		{"gpt-5.6-luna", true},
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5-nano", true},
		{"gpt-5-chat-latest", true},
		{"GPT-5.6-LUNA", true},
		{"o1", true},
		{"o1-mini", true},
		{"o1-preview", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		// Namespaced the way the GPUStack server normalizes them.
		{"openai/gpt-5.6-luna", true},
		{"openai/o3-mini", true},
		// Legacy models — max_tokens stays valid, must NOT be rewritten.
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4.1", false},
		{"gpt-4-turbo", false},
		{"gpt-3.5-turbo", false},
		{"text-embedding-3-small", false},
		{"qwen3-32b", false},
		{"deepseek-r1", false},
		// The namespace is judged by its last segment only.
		{"gpt-5/ollama", false},
		{"", false},
	}
	for _, tt := range openAITests {
		assert.Equal(t, tt.want, requiresMaxCompletionTokens(tt.model, openaiMaxCompletionTokenModelPrefixes), "model=%q", tt.model)
	}

	// Other providers extend the same judgment with their own restricted
	// families, independently of the OpenAI list.
	t.Run("custom provider prefix list", func(t *testing.T) {
		custom := []string{"glm-5", "hunyuan-turbo-l"}
		assert.True(t, requiresMaxCompletionTokens("glm-5-air", custom))
		assert.True(t, requiresMaxCompletionTokens("some-ns/Hunyuan-Turbo-L", custom))
		assert.False(t, requiresMaxCompletionTokens("gpt-5.6-luna", custom))
		assert.False(t, requiresMaxCompletionTokens("", custom))
	})
}

func TestRewriteMaxTokensToMaxCompletionTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "renames max_tokens for the issue's model",
			input: `{"model":"gpt-5.6-luna","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`,
			want:  `{"model":"gpt-5.6-luna","max_completion_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`,
		},
		{
			name:  "keeps other fields and ordering stable",
			input: `{"model":"o3-mini","stream":true,"temperature":0.7,"max_tokens":512,"messages":[]}`,
			want:  `{"model":"o3-mini","stream":true,"temperature":0.7,"max_completion_tokens":512,"messages":[]}`,
		},
		{
			name:  "prefers explicit max_completion_tokens and drops max_tokens",
			input: `{"model":"gpt-5","max_tokens":100,"max_completion_tokens":200,"messages":[]}`,
			want:  `{"model":"gpt-5","max_completion_tokens":200,"messages":[]}`,
		},
		{
			name:  "drops null max_tokens instead of renaming",
			input: `{"model":"o1","max_tokens":null,"messages":[]}`,
			want:  `{"model":"o1","messages":[]}`,
		},
		{
			name:  "no max_tokens leaves the body byte-identical",
			input: `{"model":"gpt-5","messages":[]}`,
			want:  `{"model":"gpt-5","messages":[]}`,
		},
		{
			name:  "legacy model is not rewritten",
			input: `{"model":"gpt-4o","max_tokens":1024,"messages":[]}`,
			want:  `{"model":"gpt-4o","max_tokens":1024,"messages":[]}`,
		},
		{
			name:  "namespaced reasoning model is rewritten",
			input: `{"model":"openai/gpt-5.6-luna","max_tokens":256,"messages":[]}`,
			want:  `{"model":"openai/gpt-5.6-luna","max_completion_tokens":256,"messages":[]}`,
		},
		{
			name:  "float max_tokens value is preserved verbatim",
			input: `{"model":"o4-mini","max_tokens":1024.5,"messages":[]}`,
			want:  `{"model":"o4-mini","max_completion_tokens":1024.5,"messages":[]}`,
		},
		{
			// Pins GetBytes/sjson first-occurrence semantics for the ForEach-based
			// capture: with pathological duplicate keys the first value is
			// renamed and the duplicate survives — byte-for-byte what the
			// previous GetBytes-based implementation produced.
			name:  "duplicate keys follow first-occurrence semantics",
			input: `{"model":"gpt-5","max_tokens":1,"max_tokens":2,"messages":[]}`,
			want:  `{"model":"gpt-5","max_completion_tokens":1,"max_tokens":2,"messages":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewriteMaxTokensToMaxCompletionTokens([]byte(tt.input), openaiMaxCompletionTokenModelPrefixes)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
		})
	}

	// Same helpers, a different provider's restricted families.
	t.Run("custom provider prefix list", func(t *testing.T) {
		input := `{"model":"glm-5-air","max_tokens":512,"messages":[]}`
		got, err := rewriteMaxTokensToMaxCompletionTokens([]byte(input), []string{"glm-5"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"model":"glm-5-air","max_completion_tokens":512,"messages":[]}`, string(got))

		// The OpenAI families are not restricted under this provider's list.
		got, err = rewriteMaxTokensToMaxCompletionTokens([]byte(`{"model":"gpt-4o","max_tokens":512,"messages":[]}`), []string{"glm-5"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"model":"gpt-4o","max_tokens":512,"messages":[]}`, string(got))
	})
}

// The TransformRequestBody wiring itself (rewrite applied after model mapping,
// only for chat completions) is covered by the wasm-host integration test in
// openai_max_tokens_integration_test.go: defaultTransformRequestBody calls
// proxywasm host functions, which need the in-process VM the host framework
// installs and are a nil-deref panic under plain `go test`.
