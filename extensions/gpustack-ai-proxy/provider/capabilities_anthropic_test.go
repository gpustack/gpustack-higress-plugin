// GPUStack-local test (no upstream counterpart): guards the Anthropic entries
// of the capabilities whitelist in ProviderConfig.FromJson.
//
// The whitelist drops unrecognized keys silently, and a dropped
// ApiNameAnthropicMessages is not a cosmetic loss: onHttpRequestHeaders decides
// whether to rewrite an inbound /v1/messages to /v1/chat/completions purely by
// asking IsSupportedAPI. A regression here therefore reads exactly like "the
// operator never configured it" -- no error, no log, just protocol conversion
// coming back.

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestFromJson_KeepsAnthropicCapabilities(t *testing.T) {
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "openai",
		"capabilities": {
			"anthropic/v1/messages": "/v1/messages",
			"anthropic/v1/messages/count_tokens": "/v1/messages/count_tokens",
			"anthropic/v1/complete": "/v1/complete"
		}
	}`))

	assert.Equal(t, "/v1/messages", config.capabilities[string(ApiNameAnthropicMessages)])
	assert.Equal(
		t,
		"/v1/messages/count_tokens",
		config.capabilities[string(ApiNameAnthropicCountTokens)],
	)
	assert.Equal(t, "/v1/complete", config.capabilities[string(ApiNameAnthropicComplete)])

	// What the plugin actually consults on the conversion path.
	assert.True(t, config.IsSupportedAPI(ApiNameAnthropicMessages))
	assert.True(t, config.IsSupportedAPI(ApiNameAnthropicCountTokens))
}

func TestFromJson_StillFiltersUnknownCapabilities(t *testing.T) {
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "openai",
		"capabilities": {
			"anthropic/v1/messages": "/v1/messages",
			"not/an/api": "/nope"
		}
	}`))

	assert.Len(t, config.capabilities, 1)
	assert.NotContains(t, config.capabilities, "not/an/api")
}

func TestOpenAIProvider_AnthropicCapabilityIsAdditive(t *testing.T) {
	// An openai provider that declares Anthropic must keep its own defaults:
	// capabilities are a top-up, not a replacement, which is the whole reason
	// this is preferable to swapping the provider type.
	config := &ProviderConfig{}
	config.FromJson(gjson.Parse(`{
		"type": "openai",
		"capabilities": {
			"anthropic/v1/messages": "/v1/messages",
			"openai/v1/chatcompletions": "/custom/v1/chat/completions"
		}
	}`))

	provider, err := (&openaiProviderInitializer{}).CreateProvider(*config)
	assert.NoError(t, err)
	assert.Equal(t, providerTypeOpenAI, provider.GetProviderType())

	merged := provider.(*openaiProvider).config
	assert.True(t, merged.IsSupportedAPI(ApiNameAnthropicMessages))
	assert.True(t, merged.IsSupportedAPI(ApiNameChatCompletion))
	assert.True(t, merged.IsSupportedAPI(ApiNameResponses))
	assert.True(t, merged.IsSupportedAPI(ApiNameAudioSpeech))
	assert.True(t, merged.IsSupportedAPI(ApiNameImageGeneration))

	// Both halves of setDefaultCapabilities' contract, which only fills a key
	// when it is absent: a declared path wins over the default for the same
	// API, and an API the operator never mentioned still gets its default.
	assert.Equal(
		t,
		"/custom/v1/chat/completions",
		merged.capabilities[string(ApiNameChatCompletion)],
	)
	assert.Equal(t, PathOpenAIResponses, merged.capabilities[string(ApiNameResponses)])
}
