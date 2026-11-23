package provider

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemoneProviderInitializer_DefaultCapabilities(t *testing.T) {
	initializer := &systemoneProviderInitializer{}

	capabilities := initializer.DefaultCapabilities()
	expected := map[string]string{
		string(ApiNameSystemone): PathSystemone,
		string(ApiNameModels):    PathOpenAIModels,
	}

	assert.Equal(t, expected, capabilities)
}

func TestSystemoneProvider_GetApiName(t *testing.T) {
	provider := &systemoneProvider{}

	assert.Equal(t, ApiNameSystemone, provider.GetApiName("/v1/systemone"))
	assert.Equal(t, ApiNameModels, provider.GetApiName("/v1/models"))
	assert.Equal(t, ApiName(""), provider.GetApiName("/v1/chat/completions"))
}

func TestSystemoneEndpointHost(t *testing.T) {
	assert.Equal(t, "api.typesafe.ai", systemoneEndpointHost("https://api.typesafe.ai"))
	assert.Equal(t, "jev.internal:8010", systemoneEndpointHost("http://jev.internal:8010"))
	assert.Equal(t, "jev.internal:8010", systemoneEndpointHost("http://jev.internal:8010/jev"))
	assert.Equal(t, "", systemoneEndpointHost(""))
}

func TestSystemoneProvider_TransformRequestHeaders(t *testing.T) {
	ctx := newMockMultipartHttpContext()
	ctx.SetContext("systemone-token-key", "tok")
	provider := &systemoneProvider{
		config: ProviderConfig{
			apiTokens:         []string{"tok"},
			systemoneEndpoint: "http://jev.internal:8010",
			failover:          &failover{ctxApiTokenInUse: "systemone-token-key"},
		},
	}

	headers := http.Header{}
	provider.TransformRequestHeaders(ctx, ApiNameSystemone, headers)

	assert.Equal(t, "jev.internal:8010", headers.Get(":authority"))
	assert.Equal(t, "Bearer tok", headers.Get("Authorization"))
}

func TestSystemoneProvider_TransformRequestHeadersDefaultHost(t *testing.T) {
	provider := &systemoneProvider{
		config: ProviderConfig{},
	}

	headers := http.Header{}
	provider.TransformRequestHeaders(newMockMultipartHttpContext(), ApiNameSystemone, headers)

	assert.Equal(t, "api.typesafe.ai", headers.Get(":authority"))
	// No configured token: no credential is injected, the inbound
	// Authorization passes through for anonymous self-hosted services.
	assert.Empty(t, headers.Get("Authorization"))
}

func TestOpenAIProvider_GetApiNameClaimsSystemone(t *testing.T) {
	provider, err := (&openaiProviderInitializer{}).CreateProvider(ProviderConfig{})
	require.NoError(t, err)
	openai := provider.(*openaiProvider)

	assert.Equal(t, ApiNameSystemone, openai.GetApiName("/v1/systemone"))
	// Standard OpenAI paths keep the shared classification instead of an
	// empty claim, so original-protocol entries retain body processing.
	assert.Equal(t, ApiNameChatCompletion, openai.GetApiName("/v1/chat/completions"))
	assert.Equal(t, ApiNameModels, openai.GetApiName("/v1/models"))
	// Non-OpenAI standard paths keep the shared classification too —
	// identical to the pre-handler behavior where main.go's shared lookup
	// decided for openai entries.
	assert.Equal(t, ApiNameGeminiGenerateContent, openai.GetApiName("/v1beta/models/x:generateContent"))
	// A genuinely unknown path stays unclaimed.
	assert.Equal(t, ApiName(""), openai.GetApiName("/v1/unknown"))
}

func TestOpenAIProviderInitializer_DefaultCapabilities(t *testing.T) {
	initializer := &openaiProviderInitializer{}

	capabilities := initializer.DefaultCapabilities()

	// The decision contract must be routable through openaiCustomUrl base
	// urls as well as direct endpoint urls.
	assert.Equal(t, PathSystemone, capabilities[string(ApiNameSystemone)])
	assert.Equal(t, PathOpenAIChatCompletions, capabilities[string(ApiNameChatCompletion)])
}

func TestOpenAIDirectPath(t *testing.T) {
	// A custom url pointing straight at the decision endpoint must be
	// detected as direct, so its suffix is not appended again.
	assert.True(t, isDirectPath("/v1/systemone"))
	assert.True(t, isDirectPath("/v1/chat/completions"))
	assert.False(t, isDirectPath("/proxy/v1"))
}

func TestVllmProvider_GetApiNameClaimsSystemone(t *testing.T) {
	provider := &vllmProvider{}

	assert.Equal(t, ApiNameSystemone, provider.GetApiName("/v1/systemone"))
	assert.Equal(t, ApiNameChatCompletion, provider.GetApiName("/v1/chat/completions"))
}

func TestDropRedundantSystemoneAssets(t *testing.T) {
	t.Run("both_fields_drop_assets", func(t *testing.T) {
		body := []byte(`{"state":"s","images":["data:image/png;base64,AAA"],"assets":{"img-0":{"type":"image","format":"png","data_base64":"AAA"}},"questions":{}}`)
		out, err := dropRedundantSystemoneAssets(body)
		require.NoError(t, err)
		assert.NotContains(t, string(out), `"assets"`)
		assert.Contains(t, string(out), `"images"`)
	})

	t.Run("assets_only_kept_untouched", func(t *testing.T) {
		// The reverse translation is deliberately not done: an assets-only
		// request to an images-spelling engine is a client-side miss.
		body := []byte(`{"state":"s","assets":{"img-0":{"type":"image"}},"questions":{}}`)
		out, err := dropRedundantSystemoneAssets(body)
		require.NoError(t, err)
		assert.JSONEq(t, string(body), string(out))
	})
}

func TestSystemoneBodyHasMedia(t *testing.T) {
	assert.True(t, systemoneBodyHasMedia([]byte(`{"state":"s","assets":{"img-0":{"type":"image"}}}`)))
	assert.True(t, systemoneBodyHasMedia([]byte(`{"state":"s","images":["data:image/png;base64,AAA"]}`)))
	assert.False(t, systemoneBodyHasMedia([]byte(`{"state":"s","questions":{}}`)))
	assert.False(t, systemoneBodyHasMedia([]byte(`{}`)))
}

func TestSystemoneValidateConfig_DangerZoneHeader(t *testing.T) {
	initializer := &systemoneProviderInitializer{}

	for _, valid := range []string{"", "auto", "true", "false"} {
		assert.NoError(t, initializer.ValidateConfig(&ProviderConfig{systemoneDangerZoneHeader: valid}), "value %q", valid)
	}
	assert.Error(t, initializer.ValidateConfig(&ProviderConfig{systemoneDangerZoneHeader: "yes"}))
}

func TestSystemoneProvider_TransformSystemoneBody(t *testing.T) {
	provider := &systemoneProvider{
		config: ProviderConfig{
			systemoneModel: "jev-latest",
		},
	}

	t.Run("both_fields_drop_top_level_images", func(t *testing.T) {
		body := []byte(`{"state":"s","images":["data:image/png;base64,AAA"],"assets":{"img-0":{"type":"image","format":"png","data_base64":"AAA"}},"questions":{}}`)
		out, err := provider.transformSystemoneBody(body)
		require.NoError(t, err)
		assert.NotContains(t, string(out), `"images"`)
		assert.Contains(t, string(out), `"img-0"`)
	})

	t.Run("images_only_kept_untouched", func(t *testing.T) {
		// A translation would have to invent asset placeholders and rewrite
		// the references; only the redundant spelling is removed.
		body := []byte(`{"state":"s","images":["data:image/png;base64,AAA"],"questions":{}}`)
		out, err := provider.transformSystemoneBody(body)
		require.NoError(t, err)
		assert.Contains(t, string(out), `"images"`)
	})

	t.Run("model_default_applied_when_absent", func(t *testing.T) {
		body := []byte(`{"state":"s","questions":{}}`)
		out, err := provider.transformSystemoneBody(body)
		require.NoError(t, err)
		assert.Contains(t, string(out), `"model":"jev-latest"`)
	})

	t.Run("explicit_model_preserved", func(t *testing.T) {
		body := []byte(`{"model":"jev-1.14","state":"s","questions":{}}`)
		out, err := provider.transformSystemoneBody(body)
		require.NoError(t, err)
		assert.Contains(t, string(out), `"model":"jev-1.14"`)
	})

	t.Run("empty_body_untouched", func(t *testing.T) {
		out, err := provider.transformSystemoneBody([]byte{})
		require.NoError(t, err)
		assert.Empty(t, string(out))
	})
}
