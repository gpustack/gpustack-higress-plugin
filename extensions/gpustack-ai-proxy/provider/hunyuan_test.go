// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/36f2f1ac5c7305c5a46c296c617b45d1fbacb7d3/plugins/wasm-go/extensions/ai-proxy/provider/hunyuan_test.go
// Forked into gpustack/gpustack-higress-plugins at higress commit 36f2f1ac5c73.
// Local modifications may diverge from upstream; keep this attribution when editing.

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHunyuanProviderGetApiName(t *testing.T) {
	provider := &hunyuanProvider{}

	tests := []struct {
		name string
		path string
		want ApiName
	}{
		{
			name: "chat completions",
			path: PathOpenAIChatCompletions,
			want: ApiNameChatCompletion,
		},
		{
			name: "embeddings",
			path: PathOpenAIEmbeddings,
			want: ApiNameEmbeddings,
		},
		{
			name: "embeddings with route prefix",
			path: "/gateway" + PathOpenAIEmbeddings,
			want: ApiNameEmbeddings,
		},
		{
			name: "unknown path defaults to chat completion",
			path: "/v1/unknown",
			want: ApiNameChatCompletion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, provider.GetApiName(tt.path))
		})
	}
}
