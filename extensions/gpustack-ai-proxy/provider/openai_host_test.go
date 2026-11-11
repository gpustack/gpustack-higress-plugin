// GPUStack-local test (no upstream counterpart): guards the GPUStack-local host
// attribution behavior in TransformRequestHeaders for the openai provider.

package provider

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOpenaiProviderTransformRequestHeadersHost covers the GPUStack-local host
// attribution behavior in TransformRequestHeaders:
//  1. customDomain (openaiCustomUrl) always wins;
//  2. otherwise the gpustack-lb chosen cluster (x-higress-target-cluster)
//     becomes the Host, so worker-side traffic attribution is accurate;
//  3. without either, the default openai domain (api.openai.com) is used.
func TestOpenaiProviderTransformRequestHeadersHost(t *testing.T) {
	newProvider := func() *openaiProvider {
		return &openaiProvider{
			config: ProviderConfig{
				protocol:  protocolOriginal,
				apiTokens: []string{"test-token"},
				failover:  &failover{ctxApiTokenInUse: "openai-token-key"},
			},
		}
	}
	newCtx := func() *mockMultipartHttpContext {
		ctx := newMockMultipartHttpContext()
		ctx.SetContext("openai-token-key", "test-token")
		return ctx
	}

	t.Run("custom domain wins over target cluster", func(t *testing.T) {
		p := newProvider()
		p.customDomain = "custom.example.com"
		headers := http.Header{}
		headers.Set(":authority", "gateway.example.com")
		headers.Set(TargetClusterHeader, "outbound|8080||instance-1")

		p.TransformRequestHeaders(newCtx(), "", headers)

		assert.Equal(t, "custom.example.com", headers.Get(":authority"))
	})

	t.Run("target cluster is used as host when present", func(t *testing.T) {
		p := newProvider()
		headers := http.Header{}
		headers.Set(":authority", "gateway.example.com")
		headers.Set(TargetClusterHeader, "outbound|8080||instance-1")

		p.TransformRequestHeaders(newCtx(), "", headers)

		assert.Equal(t, "outbound-8080--instance-1", headers.Get(":authority"))
	})

	t.Run("default openai domain is the fallback", func(t *testing.T) {
		p := newProvider()
		headers := http.Header{
			":authority": []string{"gateway.example.com"},
		}

		p.TransformRequestHeaders(newCtx(), "", headers)

		assert.Equal(t, defaultOpenaiDomain, headers.Get(":authority"))
	})
}
