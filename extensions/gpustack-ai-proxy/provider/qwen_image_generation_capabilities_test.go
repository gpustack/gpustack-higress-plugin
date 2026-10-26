// GPUStack-local test (no upstream counterpart): guards the OpenAI Images
// entry of the qwen provider's default capabilities.
//
// DashScope serves qwen-image-* generation/editing models on a plain OpenAI
// Images surface at /compatible-mode/v1/images/generations (verified live:
// flat OpenAI body in, {created,data[].url,usage} out), but only on the
// per-workspace maas.aliyuncs.com domains. Upstream's qwen provider declares
// no image-generation capability at all, so an inbound /v1/images/generations
// would fail isSupportedAPI with "unsupported API name".

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestQwenProvider_CompatibleModeSupportsImageGeneration(t *testing.T) {
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "qwen",
		"apiTokens": ["sk-test"],
		"qwenEnableCompatible": true
	}`))

	provider, err := (&qwenProviderInitializer{}).CreateProvider(*config)
	assert.NoError(t, err)
	assert.Equal(t, providerTypeQwen, provider.GetProviderType())

	merged := provider.(*qwenProvider).config
	assert.True(t, merged.IsSupportedAPI(ApiNameImageGeneration),
		"an inbound /v1/images/generations must pass isSupportedAPI instead of failing with 'unsupported API name'")
	assert.Equal(
		t,
		qwenCompatibleImageGenerationPath,
		merged.capabilities[string(ApiNameImageGeneration)],
		"the image-generation entry must rewrite onto DashScope's OpenAI-compatible images endpoint",
	)
}

func TestQwenProvider_NativeModeStillRejectsImageGeneration(t *testing.T) {
	// The native image surface (/api/v1/services/aigc/multimodal-generation/
	// generation) speaks a nested input/parameters dialect; forwarding a flat
	// OpenAI Images body there cannot work, so the explicit error stays the
	// honest outcome.
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "qwen",
		"apiTokens": ["sk-test"],
		"qwenEnableCompatible": false
	}`))

	provider, err := (&qwenProviderInitializer{}).CreateProvider(*config)
	assert.NoError(t, err)

	merged := provider.(*qwenProvider).config
	assert.False(t, merged.IsSupportedAPI(ApiNameImageGeneration))
}
