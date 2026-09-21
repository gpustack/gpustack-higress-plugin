package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

func main() {}

func init() {
	wrapper.SetCtx(
		"gpustack-ip-acl",
		// Two-level configuration: matchRule (route/ingress) configs inherit
		// the global defaultConfig and may override specific consumers or
		// realIPHeader.
		wrapper.ParseOverrideConfig(parseGlobalConfig, parseOverrideRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

// headerConsumer is the Higress convention for the authenticated caller
// identity (stock Higress: injected by key-auth; in the GPUStack deployment:
// by gpustack-ext-auth at AUTHN/360). Other Higress plugins hard-code it too.
const headerConsumer = "x-mse-consumer"

// IpAclConfig mirrors the enterprise API key IP ACL rule set, keyed by
// consumer. Each consumer carries an optional allow list (whitelist when
// non-empty) plus a deny list that always takes precedence. A consumer with
// no rule — or a request without a consumer header — is unrestricted.
//
// Consumer keys may contain a single `*` wildcard: a trailing `gpustack-*`
// matches by prefix, a leading `*-ci` by suffix, and a bare `*` is the
// catch-all default. Exact names always win over wildcards; wildcard rules
// are evaluated in their declaration order (first match wins), so put
// narrower patterns before broader ones.
type IpAclConfig struct {
	exact        map[string]*ConsumerAcl
	patterns     []consumerPattern
	realIPHeader string // optional trusted header (e.g. x-forwarded-for); falls back to the downstream source address
}

type consumerPattern struct {
	pattern string // without the `*`
	prefix  bool   // true: gpustack-* (match prefix); false: *-ci (match suffix)
	acl     *ConsumerAcl
}

// ConsumerAcl is one consumer's rule set: deny first, then a non-empty
// allow list requires at least one match. Both empty means unrestricted.
type ConsumerAcl struct {
	allowed []*net.IPNet
	denied  []*net.IPNet
}

func parseConfig(json gjson.Result, config *IpAclConfig) error {
	// A malformed root (`[]`, `null`, a string) makes every field lookup
	// appear absent, which would silently disable the ACL — reject it so
	// parseGlobalConfig installs the deny-all fallback.
	if json.Exists() && !json.IsObject() {
		return errors.New("config must be a JSON object")
	}
	config.realIPHeader = json.Get("realIPHeader").String()

	consumers := json.Get("consumers")
	if !consumers.Exists() {
		return nil
	}
	// A typo (e.g. `consumers: [...]`) must fail loudly, not silently
	// no-op the whole ACL — that would be a fail-open.
	if !consumers.IsObject() {
		return fmt.Errorf("consumers must be a JSON object")
	}
	// ForEach walks the object in document order, which preserves wildcard
	// priority: an entry's position decides which pattern matches first.
	var parseErr error
	consumers.ForEach(func(name, rule gjson.Result) bool {
		acl, err := parseConsumerAcl(name.String(), rule)
		if err != nil {
			parseErr = err
			return false // stop walking
		}
		if key, prefix, ok := splitWildcard(name.String()); ok {
			config.patterns = append(config.patterns, consumerPattern{pattern: key, prefix: prefix, acl: acl})
		} else {
			if config.exact == nil {
				config.exact = make(map[string]*ConsumerAcl)
			}
			config.exact[name.String()] = acl
		}
		return true
	})
	return parseErr
}

// validConsumerKeys is the closed set of keys a consumer rule may carry.
var validConsumerKeys = map[string]bool{"allowedCidrs": true, "deniedCidrs": true}

func parseConsumerAcl(name string, rule gjson.Result) (*ConsumerAcl, error) {
	// A non-object value (`alice: "oops"`) or a typo'd key (`allowedCidr`)
	// would both parse as an empty ACL — unrestricted — turning a broken
	// policy into a fail-open one. Reject both.
	if !rule.IsObject() {
		return nil, fmt.Errorf("consumers.%s must be a JSON object", name)
	}
	for key := range rule.Map() {
		if !validConsumerKeys[key] {
			return nil, fmt.Errorf("consumers.%s: unknown field %q (expected allowedCidrs/deniedCidrs)", name, key)
		}
	}
	acl := &ConsumerAcl{}
	var err error
	if acl.allowed, err = parseCIDRList(rule.Get("allowedCidrs")); err != nil {
		return nil, fmt.Errorf("consumers.%s.allowedCidrs: %w", name, err)
	}
	if acl.denied, err = parseCIDRList(rule.Get("deniedCidrs")); err != nil {
		return nil, fmt.Errorf("consumers.%s.deniedCidrs: %w", name, err)
	}
	return acl, nil
}

// denyAllConfig is the fail-closed fallback for broken configuration.
// wasm-go's RuleMatcher drops a global parse error whenever any matchRule
// exists and leaves the global config zero-valued (pkg/matcher/rule_matcher.go
// — the same hazard documented for gpustack-ext-auth), and returning the
// error when no matchRule exists fails VM init and removes the filter
// entirely. Either way the ACL would silently stop enforcing — fail-OPEN.
// So the parsers below swallow parse errors, log them loudly, and pin this
// deny-all config instead: a broken config stops traffic rather than
// widening access. Requests without a consumer header (unauthenticated
// routes) remain unrestricted — ACL semantics are consumer-keyed.
func denyAllConfig() IpAclConfig {
	_, v4all, _ := net.ParseCIDR("0.0.0.0/0")
	_, v6all, _ := net.ParseCIDR("::/0")
	return IpAclConfig{
		patterns: []consumerPattern{{
			pattern: "", prefix: true, // bare `*`: catch-all consumer
			acl: &ConsumerAcl{denied: []*net.IPNet{v4all, v6all}},
		}},
	}
}

// logCriticalf logs via proxy-wasm; it recovers so unit tests (no wasm
// host, where the mock hostcall panics) can exercise the fail-closed paths.
func logCriticalf(format string, args ...any) {
	defer func() { _ = recover() }()
	proxywasm.LogCriticalf(format, args...)
}

// parseGlobalConfig parses the plugin-wide defaultConfig.
func parseGlobalConfig(json gjson.Result, config *IpAclConfig) error {
	if err := parseConfig(json, config); err != nil {
		logCriticalf("gpustack-ip-acl: invalid defaultConfig (%v); failing closed with deny-all", err)
		*config = denyAllConfig()
	}
	return nil
}

// parseOverrideRuleConfig parses a matchRule-level (route/ingress) config on
// top of the global one. Semantics:
//   - realIPHeader: override when set in the rule, otherwise inherit.
//   - consumers: PER-KEY OVERRIDE — a consumer (exact name or wildcard
//     pattern) redeclared in the rule replaces its global rule entirely;
//     consumers not mentioned are inherited as-is. A route cannot "remove"
//     a global consumer, but it can redeclare one with empty lists to
//     effectively unrestrict it there.
//
// Aliasing safety: `global` arrives by value but its map/slice headers still
// alias the stored global config (the same value is passed to every
// matchRule's parser), so maps/slices are copied before mutation. Everything
// is built into a temporary struct and only assigned to `*rule` after
// parsing succeeds, so an invalid override leaves the rule untouched rather
// than partially mutated.
func parseOverrideRuleConfig(json gjson.Result, global IpAclConfig, rule *IpAclConfig) error {
	// A malformed rule root (scalar/array) makes every field lookup below
	// appear absent, silently inheriting the global ACL — fail closed.
	if json.Exists() && !json.IsObject() {
		logCriticalf("gpustack-ip-acl: matchRule config is not a JSON object; failing closed with deny-all")
		*rule = denyAllConfig()
		return nil
	}
	temp := IpAclConfig{realIPHeader: global.realIPHeader}
	if v := json.Get("realIPHeader").String(); v != "" {
		temp.realIPHeader = v
	}
	if len(global.exact) > 0 {
		temp.exact = make(map[string]*ConsumerAcl, len(global.exact))
		for k, v := range global.exact {
			temp.exact[k] = v
		}
	}
	// Copy the pattern slice so appends never touch the global backing array.
	// Rule-specific patterns are collected separately and PREPENDED: an
	// inherited catch-all (`*`) must not shadow a narrower pattern the route
	// adds — otherwise a route-level deny could never match.
	temp.patterns = append(temp.patterns, global.patterns...)
	var newPatterns []consumerPattern

	consumers := json.Get("consumers")
	if !consumers.Exists() {
		*rule = assembleOverride(temp, newPatterns)
		return nil
	}
	if !consumers.IsObject() {
		// Fail closed, same rationale as the global parser: a typo must not
		// silently leave the route unrestricted.
		logCriticalf("gpustack-ip-acl: matchRule consumers is not a JSON object; failing closed with deny-all")
		*rule = denyAllConfig()
		return nil
	}
	var parseErr error
	consumers.ForEach(func(name, consumerRule gjson.Result) bool {
		acl, err := parseConsumerAcl(name.String(), consumerRule)
		if err != nil {
			parseErr = err
			return false // stop walking
		}
		if key, prefix, ok := splitWildcard(name.String()); ok {
			for i := range temp.patterns {
				if temp.patterns[i].pattern == key && temp.patterns[i].prefix == prefix {
					temp.patterns[i].acl = acl // override in place, keeps position
					return true
				}
			}
			newPatterns = append(newPatterns, consumerPattern{pattern: key, prefix: prefix, acl: acl})
		} else {
			if temp.exact == nil {
				temp.exact = make(map[string]*ConsumerAcl)
			}
			temp.exact[name.String()] = acl
		}
		return true
	})
	if parseErr != nil {
		// Fail closed, same rationale as parseGlobalConfig: a broken override
		// must deny on its routes, not fall back to a possibly-empty config.
		logCriticalf("gpustack-ip-acl: invalid matchRule config (%v); failing closed with deny-all", parseErr)
		*rule = denyAllConfig()
		return nil
	}
	*rule = assembleOverride(temp, newPatterns)
	return nil
}

// assembleOverride finalizes an override config: rule-specific new wildcard
// patterns run before the inherited ones so route-level rules win over any
// broader inherited pattern (including a global catch-all `*`).
func assembleOverride(temp IpAclConfig, newPatterns []consumerPattern) IpAclConfig {
	if len(newPatterns) > 0 {
		temp.patterns = append(newPatterns, temp.patterns...)
	}
	return temp
}

// splitWildcard decomposes a key containing exactly one `*` into the literal
// part plus its position. `gpustack-*` → ("gpustack-", prefix), `*-ci` →
// ("-ci", suffix), `*` → ("", prefix, catch-all). Keys with no `*` or with
// more than one `*` are treated as exact names.
func splitWildcard(key string) (pattern string, prefix bool, ok bool) {
	if strings.Count(key, "*") != 1 {
		return "", false, false
	}
	if key == "*" {
		return "", true, true // catch-all
	}
	if strings.HasPrefix(key, "*") {
		return key[1:], false, true
	}
	if strings.HasSuffix(key, "*") {
		return key[:len(key)-1], true, true
	}
	return "", false, false
}

// lookupACL resolves a consumer name to its rule: exact entries first, then
// wildcard patterns in declaration order.
func (c *IpAclConfig) lookupACL(consumer string) *ConsumerAcl {
	if acl := c.exact[consumer]; acl != nil {
		return acl
	}
	for _, p := range c.patterns {
		if p.prefix && strings.HasPrefix(consumer, p.pattern) {
			return p.acl
		}
		if !p.prefix && strings.HasSuffix(consumer, p.pattern) {
			return p.acl
		}
	}
	return nil
}

func parseCIDRList(result gjson.Result) ([]*net.IPNet, error) {
	if !result.Exists() {
		return nil, nil
	}
	// A field that exists but is not an array (e.g. a single string instead
	// of a list) is a config typo — fail loudly rather than silently
	// dropping the whole list (fail-open).
	if !result.IsArray() {
		return nil, fmt.Errorf("must be a JSON array of CIDR strings")
	}
	var networks []*net.IPNet
	for _, item := range result.Array() {
		if item.Type != gjson.String {
			return nil, fmt.Errorf("CIDR entries must be strings, got %s", item.Type.String())
		}
		cidr := strings.TrimSpace(item.String())
		if cidr == "" {
			// An explicit empty entry is a config typo; skipping it silently
			// would turn `allowedCidrs: [""]` into "no allow list" =
			// unrestricted — a fail-open.
			return nil, errors.New("CIDR entries must not be empty")
		}
		// Accept a bare host address as a /32 (or /128) network, matching
		// the enterprise form where "203.0.113.7" and "203.0.113.0/24" are
		// both valid entries.
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			networks = append(networks, network)
			continue
		}
		if ip := net.ParseIP(cidr); ip != nil {
			// Normalize to the 4-byte representation when possible so the
			// stored IPNet never mixes 16-byte IPs with 4-byte masks (and
			// vice versa) in later Contains comparisons.
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip = v4
				bits = 32
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		return nil, fmt.Errorf("invalid CIDR '%s'", cidr)
	}
	return networks, nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config IpAclConfig) types.Action {
	if len(config.exact) == 0 && len(config.patterns) == 0 {
		return types.ActionContinue
	}

	// The consumer header is set by key-auth after the API key is validated,
	// so it is the gateway-trusted analogue of the enterprise per-API-key
	// identity. A missing header (unauthenticated route) is unrestricted.
	consumer, err := proxywasm.GetHttpRequestHeader(headerConsumer)
	if err != nil || consumer == "" {
		return types.ActionContinue
	}
	acl := config.lookupACL(consumer)
	if acl == nil || (len(acl.allowed) == 0 && len(acl.denied) == 0) {
		return types.ActionContinue
	}

	clientIP, err := resolveClientIP(config.realIPHeader)
	if err != nil {
		proxywasm.LogWarnf("gpustack-ip-acl: failed to resolve client IP: %v", err)
		// Fail closed: unable to determine the caller's IP means unable to
		// evaluate the ACL.
		return reject(ctx, "Unable to resolve client IP")
	}

	if denied, reason := enforce(clientIP, acl.allowed, acl.denied); denied {
		proxywasm.LogInfof("gpustack-ip-acl: rejected consumer %q from %s: %s", consumer, clientIP, reason)
		return reject(ctx, reason)
	}
	return types.ActionContinue
}

// enforce implements the enterprise evaluation order: deny takes precedence,
// then a non-empty allow list requires at least one match.
func enforce(clientIP string, allowed, denied []*net.IPNet) (bool, string) {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		// Mirror the enterprise behavior: an unparseable client IP is rejected,
		// fail-closed.
		return true, fmt.Sprintf("Invalid client IP: %s", clientIP)
	}
	// Normalize to the 4-byte representation for IPv4 so Contains never
	// compares mismatched lengths against the normalized parseCIDRList nets.
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, network := range denied {
		if network.Contains(ip) {
			return true, "Client IP denied for this API key"
		}
	}
	if len(allowed) > 0 {
		for _, network := range allowed {
			if network.Contains(ip) {
				return false, ""
			}
		}
		return true, "Client IP not in API key whitelist"
	}
	return false, ""
}

// resolveClientIP returns the caller IP, preferring the configured trusted
// header (first value of a comma-separated list such as X-Forwarded-For) and
// falling back to the downstream source address. A trailing port
// (`1.2.3.4:5678`, `[2001:db8::1]:443`) is stripped — net.ParseIP would
// otherwise reject the pair and enforcement would fail closed on a
// well-formed value.
func resolveClientIP(header string) (string, error) {
	if header != "" {
		value, err := proxywasm.GetHttpRequestHeader(header)
		if err == nil && value != "" {
			if ip, ok := parseForwardedIP(value); ok {
				return ip, nil
			}
		}
	}
	data, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		host = string(data)
	}
	// Some proxy configurations report a bracketed IPv6 without a port
	// (`[2001:db8::1]`); SplitHostPort fails on that form, so strip the
	// brackets here too or net.ParseIP will reject the value.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host, nil
}

// parseForwardedIP extracts the client IP from a forwarded-header value:
// the first entry of a comma-separated list, with an optional `;params`
// suffix and an optional trailing port (`1.2.3.4:5678`, `[2001:db8::1]:443`)
// stripped. A bracketed IPv6 without a port (`[2001:db8::1]`) keeps only the
// brackets stripped. Pure so it is unit-testable without a proxy-wasm host.
func parseForwardedIP(value string) (string, bool) {
	ip := strings.TrimSpace(strings.Split(value, ",")[0])
	if ip == "" {
		return "", false
	}
	ip = strings.TrimSpace(strings.Split(ip, ";")[0])
	if ip == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(ip); err == nil {
		return host, true
	}
	if strings.HasPrefix(ip, "[") && strings.HasSuffix(ip, "]") {
		return ip[1 : len(ip)-1], true
	}
	return ip, true
}

func reject(ctx wrapper.HttpContext, reason string) types.Action {
	// json.Marshal, not fmt %q: a client IP (attacker-controlled via headers)
	// can reach the message, and Go quoting is not always valid JSON.
	body, err := json.Marshal(map[string]string{"message": reason})
	if err != nil {
		body = []byte(`{"message":"request rejected"}`)
	}
	headers := [][2]string{{"Content-Type", "application/json"}}
	// Pin the route before the local reply: this filter has read headers and
	// other filters may still rewrite routing headers — without this the
	// queued reply can be dropped (same contract as gpustack-ext-auth).
	ctx.DisableReroute()
	// ActionPause after a local reply, matching the request-validation shape
	// documented in CLAUDE.md — not ActionContinue.
	_ = proxywasm.SendHttpResponseWithDetail(403, "gpustack.ip.acl.rejected", headers, body, -1)
	return types.ActionPause
}
