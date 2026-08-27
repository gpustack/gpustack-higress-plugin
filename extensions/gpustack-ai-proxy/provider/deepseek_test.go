// GPUStack-local test (no upstream counterpart): guards DeepSeek's Responses
// API capability.
//
// DeepSeek serves the Responses API natively, so the only thing standing
// between an inbound /v1/responses and the upstream is the capability entry --
// there is no converter to fall back on. Drop the entry and OnRequestBody
// returns errUnsupportedApiName, which surfaces as a hard failure rather than a
// degraded response, so it is worth pinning.
//
// https://api-docs.deepseek.com/zh-cn/guides/responses_api

package provider

import (
	"testing"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestDeepSeekProviderInitializer_DefaultCapabilities(t *testing.T) {
	capabilities := (&deepseekProviderInitializer{}).DefaultCapabilities()

	assert.Equal(t, map[string]string{
		string(ApiNameChatCompletion):    PathOpenAIChatCompletions,
		string(ApiNameModels):            PathOpenAIModels,
		string(ApiNameResponses):         PathOpenAIResponses,
		string(ApiNameAnthropicMessages): deepseekAnthropicMessagesPath,
	}, capabilities)
}

func TestDeepSeekProvider_SupportsResponses(t *testing.T) {
	config := ProviderConfig{apiTokens: []string{"test-token"}}
	p, err := (&deepseekProviderInitializer{}).CreateProvider(config)
	assert.NoError(t, err)

	merged := p.(*deepseekProvider).config
	assert.True(t, merged.IsSupportedAPI(ApiNameResponses))
	// The pre-existing surfaces must survive the addition.
	assert.True(t, merged.IsSupportedAPI(ApiNameChatCompletion))
	assert.True(t, merged.IsSupportedAPI(ApiNameModels))
	assert.True(t, merged.IsSupportedAPI(ApiNameAnthropicMessages))

	// What TransformRequestHeaders ends up doing to the :path header. DeepSeek's
	// docs show base_url without /v1; the /v1 prefix is its OpenAI-compat alias
	// for the same endpoint, and it is the shape the other capabilities use.
	assert.Equal(
		t,
		PathOpenAIResponses,
		util.MapRequestPathByCapability(
			string(ApiNameResponses),
			PathOpenAIResponses,
			merged.capabilities,
		),
	)
}

func TestDeepSeekProvider_ResponsesGenericWiring(t *testing.T) {
	// DeepSeek declares no TransformRequestBody, so a Responses request rides
	// the generic path: model mapping via defaultTransformRequestBody, and
	// streaming detection off the request body's `stream` field. Both are keyed
	// on apiName, so assert ApiNameResponses is in those switches.
	config := ProviderConfig{}
	assert.True(t, config.needToProcessRequestBody(ApiNameResponses))
	assert.True(t, config.isStreamingAPI(ApiNameResponses, []byte(`{"stream":true}`)))
	assert.False(t, config.isStreamingAPI(ApiNameResponses, []byte(`{"stream":false}`)))
}

func TestDeepSeekProvider_ResponsesCapabilityIsConfigurable(t *testing.T) {
	// An operator pointing DeepSeek at a different Responses path must survive
	// the FromJson capability whitelist.
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "deepseek",
		"capabilities": {"openai/v1/responses": "/responses"}
	}`))

	p, err := (&deepseekProviderInitializer{}).CreateProvider(*config)
	assert.NoError(t, err)

	merged := p.(*deepseekProvider).config
	assert.Equal(t, "/responses", merged.capabilities[string(ApiNameResponses)])
	assert.Equal(t, PathOpenAIChatCompletions, merged.capabilities[string(ApiNameChatCompletion)])
}
