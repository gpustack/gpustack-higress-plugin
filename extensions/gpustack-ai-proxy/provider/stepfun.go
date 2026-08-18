// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/provider/stepfun.go
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

const (
	stepfunDomain = "api.stepfun.com"
)

type stepfunProviderInitializer struct{}

func (m *stepfunProviderInitializer) ValidateConfig(config *ProviderConfig) error {
	if config.apiTokens == nil || len(config.apiTokens) == 0 {
		return errors.New("no apiToken found in provider config")
	}
	return nil
}

func (m *stepfunProviderInitializer) DefaultCapabilities() map[string]string {
	return map[string]string{
		// stepfun的chat接口path和OpenAI的chat接口一样
		string(ApiNameChatCompletion): PathOpenAIChatCompletions,
	}
}

func (m *stepfunProviderInitializer) CreateProvider(config ProviderConfig) (Provider, error) {
	config.setDefaultCapabilities(m.DefaultCapabilities())
	return &stepfunProvider{
		config:       config,
		contextCache: createContextCache(&config),
	}, nil
}

type stepfunProvider struct {
	config       ProviderConfig
	contextCache *contextCache
}

func (m *stepfunProvider) GetProviderType() string {
	return providerTypeStepfun
}

func (m *stepfunProvider) OnRequestHeaders(ctx wrapper.HttpContext, apiName ApiName) error {
	m.config.handleRequestHeaders(m, ctx, apiName)
	return nil
}

func (m *stepfunProvider) OnRequestBody(ctx wrapper.HttpContext, apiName ApiName, body []byte) (types.Action, error) {
	if !m.config.isSupportedAPI(apiName) {
		return types.ActionContinue, errUnsupportedApiName
	}
	return m.config.handleRequestBody(m, m.contextCache, ctx, apiName, body)
}

func (m *stepfunProvider) TransformRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, headers http.Header) {
	util.OverwriteRequestPathHeaderByCapability(headers, string(apiName), m.config.capabilities)
	util.OverwriteRequestHostHeader(headers, stepfunDomain)
	util.OverwriteRequestAuthorizationHeader(headers, "Bearer "+m.config.GetApiTokenInUse(ctx))
	headers.Del("Content-Length")
}
