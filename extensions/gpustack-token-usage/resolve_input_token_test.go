package main

import (
	"testing"

	"github.com/higress-group/wasm-go/pkg/tokenusage"
)

func TestResolveInputToken(t *testing.T) {
	tests := []struct {
		name  string
		usage tokenusage.TokenUsage
		want  int64
	}{
		{
			name: "OpenAI: input already includes cached tokens",
			usage: tokenusage.TokenUsage{
				InputToken:        100,
				OutputToken:       20,
				InputTokenDetails: map[string]int64{"cached_tokens": 80},
			},
			want: 100,
		},
		{
			name: "Anthropic: cache read/creation added back (issue 6405)",
			usage: tokenusage.TokenUsage{
				InputToken:                       10,
				AnthropicCacheReadInputToken:     5000,
				AnthropicCacheCreationInputToken: 200,
			},
			want: 5210,
		},
		{
			name:  "no usage",
			usage: tokenusage.TokenUsage{},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveInputToken(tt.usage); got != tt.want {
				t.Errorf("resolveInputToken() = %d, want %d", got, tt.want)
			}
		})
	}
}
