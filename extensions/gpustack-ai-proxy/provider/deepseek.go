// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/provider/deepseek.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package provider

import (
	"errors"
	"net/http"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

// deepseekProvider is the provider for deepseek Ai service.

const (
	deepseekDomain                = "api.deepseek.com"
	deepseekAnthropicMessagesPath = "/anthropic/v1/messages"
)

type deepseekProviderInitializer struct{}

func (m *deepseekProviderInitializer) ValidateConfig(config *ProviderConfig) error {
	if len(config.apiTokens) == 0 {
		return errors.New("no apiToken found in provider config")
	}
	return nil
}

func (m *deepseekProviderInitializer) DefaultCapabilities() map[string]string {
	return map[string]string{
		string(ApiNameChatCompletion): PathOpenAIChatCompletions,
		string(ApiNameModels):         PathOpenAIModels,
		// DeepSeek serves the OpenAI Responses API natively, so it is a path
		// mapping and not a protocol conversion.
		// https://api-docs.deepseek.com/zh-cn/guides/responses_api
		//
		// The docs show base_url "https://api.deepseek.com" (i.e. /responses);
		// the /v1 prefix is DeepSeek's OpenAI-compatibility alias and serves the
		// same endpoint, which is already what ApiNameChatCompletion and
		// ApiNameModels above rely on. Keep both on the same shape.
		string(ApiNameResponses):         PathOpenAIResponses,
		string(ApiNameAnthropicMessages): deepseekAnthropicMessagesPath,
	}
}

func (m *deepseekProviderInitializer) CreateProvider(config ProviderConfig) (Provider, error) {
	config.setDefaultCapabilities(m.DefaultCapabilities())
	return &deepseekProvider{
		config:       config,
		contextCache: createContextCache(&config),
	}, nil
}

type deepseekProvider struct {
	config       ProviderConfig
	contextCache *contextCache
}

func (m *deepseekProvider) GetProviderType() string {
	return providerTypeDeepSeek
}

func (m *deepseekProvider) OnRequestHeaders(ctx wrapper.HttpContext, apiName ApiName) error {
	m.config.handleRequestHeaders(m, ctx, apiName)
	return nil
}

func (m *deepseekProvider) OnRequestBody(ctx wrapper.HttpContext, apiName ApiName, body []byte) (types.Action, error) {
	if !m.config.isSupportedAPI(apiName) {
		return types.ActionContinue, errUnsupportedApiName
	}
	return m.config.handleRequestBody(m, m.contextCache, ctx, apiName, body)
}

func (m *deepseekProvider) TransformRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, headers http.Header) {
	util.OverwriteRequestPathHeaderByCapability(headers, string(apiName), m.config.capabilities)
	util.OverwriteRequestHostHeader(headers, deepseekDomain)
	util.OverwriteRequestAuthorizationHeader(headers, "Bearer "+m.config.GetApiTokenInUse(ctx))
	headers.Del("Content-Length")
}
