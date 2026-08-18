// This file is forked from the Higress ai-proxy plugin.
// Upstream: https://github.com/alibaba/higress/blob/aae6fbce36a2d1dd7afff007a265ecbebdd8a6f1/plugins/wasm-go/extensions/ai-proxy/export_test.go
// Forked into gpustack/gpustack-higress-plugins at higress commit aae6fbce36a2.
// Local modifications may diverge from upstream; keep this attribution when editing.

package main

import (
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/config"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

// NeedsClaudeResponseConversionForTest exposes needsClaudeResponseConversion for unit tests.
func NeedsClaudeResponseConversionForTest(ctx wrapper.HttpContext) bool {
	return needsClaudeResponseConversion(ctx)
}

// ParseGlobalConfigForTest exposes parseGlobalConfig for unit tests.
func ParseGlobalConfigForTest(json gjson.Result, pluginConfig *config.PluginConfig) error {
	return parseGlobalConfig(json, pluginConfig)
}

// ParseOverrideRuleConfigForTest exposes parseOverrideRuleConfig for unit tests.
func ParseOverrideRuleConfigForTest(json gjson.Result, global config.PluginConfig, pluginConfig *config.PluginConfig) error {
	return parseOverrideRuleConfig(json, global, pluginConfig)
}

// OnStreamingResponseBodyForTest exposes onStreamingResponseBody for unit tests.
func OnStreamingResponseBodyForTest(ctx wrapper.HttpContext, pluginConfig config.PluginConfig, chunk []byte, isLastChunk bool) []byte {
	return onStreamingResponseBody(ctx, pluginConfig, chunk, isLastChunk)
}
