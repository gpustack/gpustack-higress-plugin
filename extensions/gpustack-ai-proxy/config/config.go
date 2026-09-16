// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/config/config.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package config

import (
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/provider"
	"github.com/tidwall/gjson"
)

// @Name ai-proxy
// @Category custom
// @Phase UNSPECIFIED_PHASE
// @Priority 0
// @Title zh-CN AI代理
// @Description zh-CN 通过AI助手提供智能对话服务
// @IconUrl https://img.alicdn.com/imgextra/i1/O1CN018iKKih1iVx287RltL_!!6000000004419-2-tps-42-42.png
// @Version 0.1.0
//
// @Contact.name CH3CHO
// @Contact.url https://github.com/CH3CHO
// @Contact.email ch3cho@qq.com
//
// @Example
// { "provider": { "type": "qwen", "apiToken": "YOUR_DASHSCOPE_API_TOKEN", "modelMapping": { "*": "qwen-turbo" } } }
// @End
type PluginConfig struct {
	// @Title zh-CN AI服务提供商配置
	// @Description zh-CN AI服务提供商配置，包含API接口、模型和知识库文件等信息
	providerConfigs []provider.ProviderConfig `required:"true" yaml:"providers"`

	// @Title zh-CN tool/tool_calls 配对校验
	// @Description zh-CN 对 Chat Completions 请求做结构化的 tool/tool_calls 配对校验：off（默认，关闭）、strict（校验失败返回 400）
	toolCallValidation ToolCallValidationMode `yaml:"toolCallValidation"`
	// Set when toolCallValidation fell back to its default because the
	// configured value was unrecognised; logged by the caller of FromJson.
	toolCallValidationWarning string `yaml:"-"`

	activeProviderConfig *provider.ProviderConfig `yaml:"-"`
	activeProvider       provider.Provider        `yaml:"-"`
}

func (c *PluginConfig) FromJson(json gjson.Result) {
	// Parsed before the legacy `provider` branch below, which returns early.
	// Only assigned when the key is present, so a matchRule override that does
	// not mention it inherits the global value copied by parseOverrideRuleConfig.
	if modeJson := json.Get("toolCallValidation"); modeJson.Exists() {
		c.toolCallValidation, c.toolCallValidationWarning = normalizeToolCallValidationMode(modeJson.String())
	}

	if providersJson := json.Get("providers"); providersJson.Exists() && providersJson.IsArray() {
		c.providerConfigs = make([]provider.ProviderConfig, 0)
		for _, providerJson := range providersJson.Array() {
			providerConfig := provider.ProviderConfig{}
			providerConfig.FromJson(providerJson)
			c.providerConfigs = append(c.providerConfigs, providerConfig)
		}
	}

	if providerJson := json.Get("provider"); providerJson.Exists() && providerJson.IsObject() {
		// TODO: For legacy config support. To be removed later.
		providerConfig := provider.ProviderConfig{}
		providerConfig.FromJson(providerJson)
		c.providerConfigs = []provider.ProviderConfig{providerConfig}
		c.activeProviderConfig = &providerConfig
		// Legacy configuration is used and the active provider is determined.
		// We don't need to continue with the new configuration style.
		return
	}

	c.activeProviderConfig = nil

	activeProviderId := json.Get("activeProviderId").String()
	if activeProviderId != "" {
		for _, providerConfig := range c.providerConfigs {
			if providerConfig.GetId() == activeProviderId {
				c.activeProviderConfig = &providerConfig
				break
			}
		}
	}
}

func (c *PluginConfig) Validate() error {
	if c.activeProviderConfig == nil {
		return nil
	}
	if err := c.activeProviderConfig.Validate(); err != nil {
		return err
	}
	return nil
}

func (c *PluginConfig) Complete() error {
	if c.activeProviderConfig == nil {
		c.activeProvider = nil
		return nil
	}

	var err error

	c.activeProvider, err = provider.CreateProvider(*c.activeProviderConfig)
	if err != nil {
		return err
	}

	providerConfig := c.GetProviderConfig()
	return providerConfig.SetApiTokensFailover(c.activeProvider)
}

func (c *PluginConfig) GetProvider() provider.Provider {
	return c.activeProvider
}

// ToolCallValidationWarning returns a non-empty message when the configured
// toolCallValidation value was unrecognised and the default was substituted.
// It is deliberately not an error: see normalizeToolCallValidationMode.
func (c *PluginConfig) ToolCallValidationWarning() string {
	return c.toolCallValidationWarning
}

// GetToolCallValidationMode returns the effective tool/tool_calls pairing mode.
// See ToolCallValidationDefault for why an unset field means off.
func (c *PluginConfig) GetToolCallValidationMode() ToolCallValidationMode {
	if c.toolCallValidation == "" {
		return ToolCallValidationDefault
	}
	return c.toolCallValidation
}

func (c *PluginConfig) GetProviderConfig() *provider.ProviderConfig {
	return c.activeProviderConfig
}

// SetActiveProviderForTest replaces the runtime Provider after Complete(); intended for unit tests in package main only.
func (c *PluginConfig) SetActiveProviderForTest(p provider.Provider) {
	c.activeProvider = p
}
