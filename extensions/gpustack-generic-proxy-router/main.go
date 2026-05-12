package main

import (
	"errors"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	pluginName          = "gpustack-generic-proxy-router"
	defaultPrefix       = "/model/proxy/"
	defaultTargetHeader = "x-higress-llm-model"
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

type PluginConfig struct {
	prefix           string
	targetHeader     string
	aliasNameMapping map[string]string
}

func parseConfig(json gjson.Result, config *PluginConfig) error {
	config.prefix = json.Get("prefix").String()
	if config.prefix == "" {
		config.prefix = defaultPrefix
	}
	if !strings.HasSuffix(config.prefix, "/") {
		config.prefix += "/"
	}

	config.targetHeader = json.Get("targetHeader").String()
	if config.targetHeader == "" {
		config.targetHeader = defaultTargetHeader
	}

	config.aliasNameMapping = make(map[string]string)
	for k, v := range json.Get("aliasNameMapping").Map() {
		if k == "" {
			continue
		}
		config.aliasNameMapping[k] = v.String()
	}
	if len(config.aliasNameMapping) == 0 {
		return errors.New("aliasNameMapping must not be empty")
	}
	return nil
}

// extractID returns the {id} segment of path after the configured prefix.
// Returns an empty string if path does not match the prefix or the id is empty.
func extractID(path, prefix string) string {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id, _, _ := strings.Cut(path[len(prefix):], "/")
	return id
}

func onHttpRequestHeaders(_ wrapper.HttpContext, config PluginConfig) types.Action {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		return types.ActionContinue
	}

	id := extractID(path, config.prefix)
	if id == "" {
		return types.ActionContinue
	}

	target, ok := config.aliasNameMapping[id]
	if !ok || target == "" {
		proxywasm.LogDebugf("%s: no alias mapping for id %q", pluginName, id)
		return types.ActionContinue
	}

	if err := proxywasm.ReplaceHttpRequestHeader(config.targetHeader, target); err != nil {
		proxywasm.LogWarnf("%s: failed to set header %s: %v", pluginName, config.targetHeader, err)
	}
	return types.ActionContinue
}
