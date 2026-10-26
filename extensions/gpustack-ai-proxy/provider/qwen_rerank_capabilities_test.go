// GPUStack-local test (no upstream counterpart): guards the Cohere-style rerank
// entry of the qwen provider's default capabilities.
//
// GPUStack exposes rerank on /v1/rerank (the de-facto standard Cohere/Jina
// spelling, what GPUStack's UI and llama-box backends speak), which main.go
// resolves to ApiNameCohereV1Rerank -- while the qwen provider only declared
// qwen/v1/rerank (the /v1/reranks spelling mirroring DashScope's own path).
// A regression here reads exactly like the reported failure: the request dies
// at isSupportedAPI with "unsupported API name" before anything reaches
// DashScope.

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestQwenProvider_CompatibleModeSupportsCohereRerank(t *testing.T) {
	// GPUStack's QwenConfig defaults qwenEnableCompatible to true
	// (schemas/model_provider.py _default_override), so this is the branch
	// every GPUStack qwen provider actually takes.
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
	assert.True(t, merged.IsSupportedAPI(ApiNameCohereV1Rerank),
		"an inbound /v1/rerank must pass isSupportedAPI instead of failing with 'unsupported API name'")
	assert.Equal(
		t,
		qwenCompatibleTextRerankPath,
		merged.capabilities[string(ApiNameCohereV1Rerank)],
		"the Cohere-style entry must rewrite onto DashScope's compatible rerank endpoint",
	)

	// The pre-existing qwen-spelling entry is untouched: capabilities are a
	// top-up, and the /v1/reranks inbound spelling keeps working.
	assert.True(t, merged.IsSupportedAPI(ApiNameQwenV1Rerank))
	assert.Equal(t, qwenCompatibleTextRerankPath, merged.capabilities[string(ApiNameQwenV1Rerank)])
}

func TestQwenProvider_NativeModeStillRejectsCohereRerank(t *testing.T) {
	// Native-mode DashScope rerank (/api/v1/services/rerank/text-rerank/
	// text-rerank) speaks input.query/input.documents, so forwarding a
	// Cohere-shaped body there cannot work; the explicit error is the honest
	// outcome until a real converter exists.
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "qwen",
		"apiTokens": ["sk-test"],
		"qwenEnableCompatible": false
	}`))

	provider, err := (&qwenProviderInitializer{}).CreateProvider(*config)
	assert.NoError(t, err)

	merged := provider.(*qwenProvider).config
	assert.False(t, merged.IsSupportedAPI(ApiNameCohereV1Rerank))
	assert.True(t, merged.IsSupportedAPI(ApiNameQwenV1Rerank))
}
