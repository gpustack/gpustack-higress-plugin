package main

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const pluginName = "gpustack-ip"

// defaultClusterNameRegexps are matched against the FQDN field of Envoy's
// cluster_name ("outbound|<port>|<subset>|<fqdn>"). Headers are only injected
// when the FQDN matches one of these (or one of the user-supplied
// additionalClusterNameRegexps) so trusted headers never flow to third-party
// upstreams reached via the proxy.
var defaultClusterNameRegexps = []string{
	`^gpustack(-|\.|$)`,
	`^model-\d+-\d+(\.|$)`,
	`^provider-\d+(\.|$)`,
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

type PluginConfig struct {
	realIPHeader        string
	headerAdd           map[string]string
	clusterNameMatchers []*regexp.Regexp
}

func parseConfig(json gjson.Result, config *PluginConfig) error {
	config.realIPHeader = json.Get("realIPHeader").String()

	config.headerAdd = make(map[string]string)
	for k, v := range json.Get("header_add").Map() {
		config.headerAdd[k] = v.String()
	}
	if config.realIPHeader == "" && len(config.headerAdd) == 0 {
		return errors.New("at least one of realIPHeader or header_add must be configured")
	}

	patterns := append([]string(nil), defaultClusterNameRegexps...)
	for _, item := range json.Get("additionalClusterNameRegexps").Array() {
		if s := item.String(); s != "" {
			patterns = append(patterns, s)
		}
	}
	config.clusterNameMatchers = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid cluster_name regexp %q: %w", p, err)
		}
		config.clusterNameMatchers = append(config.clusterNameMatchers, re)
	}
	return nil
}

// extractClusterFQDN returns the FQDN field of Envoy's
// "outbound|<port>|<subset>|<fqdn>". For non-Envoy-shaped values the raw
// string is returned so user-supplied regexps can still match literally.
func extractClusterFQDN(clusterName string) string {
	if parts := strings.SplitN(clusterName, "|", 4); len(parts) == 4 {
		return parts[3]
	}
	return clusterName
}

func matchesAnyCluster(fqdn string, matchers []*regexp.Regexp) bool {
	for _, m := range matchers {
		if m.MatchString(fqdn) {
			return true
		}
	}
	return false
}

func onHttpRequestHeaders(_ wrapper.HttpContext, config PluginConfig) types.Action {
	raw, err := proxywasm.GetProperty([]string{"cluster_name"})
	if err != nil || len(raw) == 0 {
		proxywasm.LogDebugf("gpustack-ip: cluster_name unavailable: %v", err)
		return types.ActionContinue
	}
	fqdn := extractClusterFQDN(string(raw))
	if !matchesAnyCluster(fqdn, config.clusterNameMatchers) {
		return types.ActionContinue
	}

	if config.realIPHeader != "" {
		addRealIPHeader(config.realIPHeader)
	}
	for k, v := range config.headerAdd {
		if err := proxywasm.ReplaceHttpRequestHeader(k, v); err != nil {
			proxywasm.LogWarnf("gpustack-ip: failed to replace header %s: %v", k, err)
		}
	}
	return types.ActionContinue
}

func addRealIPHeader(name string) {
	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		proxywasm.LogDebugf("gpustack-ip: failed to get source address: %v", err)
		return
	}
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		host = string(data)
	}
	if err := proxywasm.ReplaceHttpRequestHeader(name, host); err != nil {
		proxywasm.LogWarnf("gpustack-ip: failed to replace header %s: %v", name, err)
	}
}
