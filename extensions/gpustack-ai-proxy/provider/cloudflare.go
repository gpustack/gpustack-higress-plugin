// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/provider/cloudflare.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package provider

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const (
	cloudflareDomain = "api.cloudflare.com"
	// https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/
	cloudflareChatCompletionPath = "/client/v4/accounts/{account_id}/ai/v1/chat/completions"
)

type cloudflareProviderInitializer struct {
}

func (c *cloudflareProviderInitializer) ValidateConfig(config *ProviderConfig) error {
	if config.apiTokens == nil || len(config.apiTokens) == 0 {
		return errors.New("no apiToken found in provider config")
	}
	return nil
}
func (c *cloudflareProviderInitializer) DefaultCapabilities() map[string]string {
	return map[string]string{
		string(ApiNameChatCompletion): cloudflareChatCompletionPath,
	}
}

func (c *cloudflareProviderInitializer) CreateProvider(config ProviderConfig) (Provider, error) {
	config.setDefaultCapabilities(c.DefaultCapabilities())
	return &cloudflareProvider{
		config:       config,
		contextCache: createContextCache(&config),
	}, nil
}

type cloudflareProvider struct {
	config       ProviderConfig
	contextCache *contextCache
}

func (c *cloudflareProvider) GetProviderType() string {
	return providerTypeCloudflare
}

func (c *cloudflareProvider) OnRequestHeaders(ctx wrapper.HttpContext, apiName ApiName) error {
	c.config.handleRequestHeaders(c, ctx, apiName)
	return nil
}

func (c *cloudflareProvider) OnRequestBody(ctx wrapper.HttpContext, apiName ApiName, body []byte) (types.Action, error) {
	if !c.config.isSupportedAPI(apiName) {
		return types.ActionContinue, errUnsupportedApiName
	}
	return c.config.handleRequestBody(c, c.contextCache, ctx, apiName, body)
}

func (c *cloudflareProvider) TransformRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, headers http.Header) {
	util.OverwriteRequestPathHeader(headers, strings.Replace(cloudflareChatCompletionPath, "{account_id}", c.config.cloudflareAccountId, 1))
	util.OverwriteRequestHostHeader(headers, cloudflareDomain)
	util.OverwriteRequestAuthorizationHeader(headers, "Bearer "+c.config.GetApiTokenInUse(ctx))
}
