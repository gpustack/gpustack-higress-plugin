package main

import (
	"net"
	"testing"

	"github.com/tidwall/gjson"
)

const aliceConfig = `{
	"consumers": {
		"alice": {
			"allowedCidrs": ["192.168.1.0/24", "203.0.113.7"],
			"deniedCidrs": ["192.168.1.99", "10.0.0.0/8"]
		}
	}
}`

func mustParse(t *testing.T, raw string) *IpAclConfig {
	t.Helper()
	var config IpAclConfig
	if err := parseConfig(gjson.Parse(raw), &config); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return &config
}

func TestParseConfig(t *testing.T) {
	config := mustParse(t, `{"consumers":{"a":{"allowedCidrs":["192.168.1.0/24","203.0.113.7"],"deniedCidrs":["10.0.0.0/8","2001:db8::/32"]},"b":{}}}`)
	if len(config.exact) != 2 {
		t.Fatalf("expected 2 exact consumers, got %d", len(config.exact))
	}
	a := config.exact["a"]
	if len(a.allowed) != 2 || len(a.denied) != 2 {
		t.Fatalf("expected 2 allowed + 2 denied, got %d/%d", len(a.allowed), len(a.denied))
	}
	if got := a.allowed[1].String(); got != "203.0.113.7/32" {
		t.Fatalf("bare host should be a /32, got %s", got)
	}
}

func TestParseConfigTypeTyposFailLoud(t *testing.T) {
	// `consumers` not an object must error (not silently no-op the ACL).
	var config IpAclConfig
	if err := parseConfig(gjson.Parse(`{"consumers":["alice"]}`), &config); err == nil {
		t.Error("consumers as array should fail loudly")
	}
	// allowedCidrs as a single string must error (not silently drop the list).
	if err := parseConfig(gjson.Parse(`{"consumers":{"a":{"allowedCidrs":"192.168.0.0/16"}}}`), &config); err == nil {
		t.Error("allowedCidrs as a string should fail loudly")
	}
	// Omitted fields are still fine (unrestricted).
	if err := parseConfig(gjson.Parse(`{}`), &config); err != nil {
		t.Errorf("empty config should parse: %v", err)
	}
	if err := parseConfig(gjson.Parse(`{"consumers":{"a":{}}}`), &config); err != nil {
		t.Errorf("consumer with no lists should parse: %v", err)
	}
	// The override parser carries the same contract, failing closed.
	global := mustParse(t, `{"consumers":{"a":{"deniedCidrs":["10.0.0.0/8"]}}}`)
	var rule IpAclConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"consumers":"oops"}`), *global, &rule); err != nil {
		t.Errorf("override consumers typo should fail closed (nil), got %v", err)
	}
	if acl := rule.lookupACL("a"); acl == nil || len(acl.denied) != 2 {
		t.Error("override consumers typo should deny-all")
	}
}

func TestParseConfigRejectsMalformedRootAndRules(t *testing.T) {
	for _, raw := range []string{
		`[]`,                             // root is an array
		`null`,                           // root is null
		`"oops"`,                         // root is a string
		`{"consumers":{"alice":"oops"}}`, // consumer value not an object
		`{"consumers":{"alice":{"allowedCidr":"192.168.0.0/16"}}}`, // typo'd key
	} {
		var config IpAclConfig
		if err := parseConfig(gjson.Parse(raw), &config); err == nil {
			t.Errorf("%s should fail loudly", raw)
		}
	}
}

func TestOverrideRuleConfigRootTypeFailsClosed(t *testing.T) {
	global := mustParse(t, `{"consumers":{"a":{"deniedCidrs":["10.0.0.0/8"]}}}`)
	for _, raw := range []string{`"oops"`, `[]`, `42`} {
		var rule IpAclConfig
		if err := parseOverrideRuleConfig(gjson.Parse(raw), *global, &rule); err != nil {
			t.Errorf("scalar rule root %s should fail closed (nil), got %v", raw, err)
		}
		if acl := rule.lookupACL("a"); acl == nil || len(acl.denied) != 2 {
			t.Errorf("scalar rule root %s should deny-all", raw)
		}
	}
}

func TestOverrideNewPatternsBeatInheritedCatchAll(t *testing.T) {
	// Global installs a catch-all; the route adds a narrower pattern. The
	// route-level pattern must win despite the inherited `*` being declared
	// globally.
	global := mustParse(t, `{"consumers":{"*":{"allowedCidrs":["10.0.0.0/8"]}}}`)
	var rule IpAclConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"consumers":{"*-ci":{"deniedCidrs":["203.0.113.0/24"]}}}`), *global, &rule); err != nil {
		t.Fatal(err)
	}
	acl := rule.lookupACL("team-ci")
	if acl == nil || len(acl.denied) != 1 {
		t.Fatal("*-ci must match before the inherited *")
	}
	// Consumers matching only the catch-all still use the global rule.
	acl = rule.lookupACL("someone-else")
	if acl == nil || len(acl.allowed) != 1 {
		t.Error("inherited catch-all must still apply to everyone else")
	}
}

func TestParseConfigInvalid(t *testing.T) {
	var config IpAclConfig
	if err := parseConfig(gjson.Parse(`{"consumers":{"a":{"allowedCidrs":["not-an-ip"]}}}`), &config); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestParseForwardedIP(t *testing.T) {
	cases := []struct {
		value string
		want  string
		ok    bool
	}{
		{"1.2.3.4", "1.2.3.4", true},
		{"1.2.3.4, 5.6.7.8, 9.10.11.12", "1.2.3.4", true}, // first of the list
		{"1.2.3.4:5678", "1.2.3.4", true},                 // port stripped (IPv4)
		{"[2001:db8::1]:443", "2001:db8::1", true},        // port stripped (IPv6)
		{"[2001:db8::1]", "2001:db8::1", true},            // bracketed IPv6 without port
		{"2001:db8::1", "2001:db8::1", true},              // bare IPv6 survives
		{"  1.2.3.4 ;param=1", "1.2.3.4", true},           // params stripped
		{"", "", false},
		{" , 5.6.7.8", "", false}, // first entry empty → fall back to source address
	}
	for _, c := range cases {
		got, ok := parseForwardedIP(c.value)
		if ok != c.ok || got != c.want {
			t.Errorf("parseForwardedIP(%q) = (%q, %v), want (%q, %v)", c.value, got, ok, c.want, c.ok)
		}
	}
}

func TestHeaderConsumerIsHigressConvention(t *testing.T) {
	if headerConsumer != "x-mse-consumer" {
		t.Fatalf("headerConsumer must stay the Higress convention, got %s", headerConsumer)
	}
}

func TestEnforce(t *testing.T) {
	config := mustParse(t, aliceConfig)
	acl := config.lookupACL("alice")

	cases := []struct {
		ip     string
		denied bool
	}{
		{"192.168.1.10", false}, // in allow, not denied
		{"192.168.1.99", true},  // deny takes precedence over allow
		{"192.168.2.10", true},  // outside allow list
		{"10.1.2.3", true},      // denied range
		{"172.16.0.1", true},    // outside allow list
		{"garbage", true},       // invalid IP fails closed
	}
	for _, c := range cases {
		denied, _ := enforce(c.ip, acl.allowed, acl.denied)
		if denied != c.denied {
			t.Errorf("ip %s: expected denied=%v", c.ip, c.denied)
		}
	}
}

func TestEnforceDenyOnly(t *testing.T) {
	config := mustParse(t, `{"consumers":{"a":{"deniedCidrs":["10.0.0.0/8"]}}}`)
	acl := config.lookupACL("a")
	if denied, _ := enforce("192.0.2.1", acl.allowed, acl.denied); denied {
		t.Error("empty allow list should not be a whitelist")
	}
	if denied, _ := enforce("10.0.0.1", acl.allowed, acl.denied); !denied {
		t.Error("denied range should reject")
	}
}

func TestParseCIDRListBareHostNormalization(t *testing.T) {
	config := mustParse(t, `{"consumers":{"a":{"allowedCidrs":["203.0.113.7","2001:db8::1"]}}}`)
	acl := config.lookupACL("a")
	// Bare IPv4 hosts must be stored in the 4-byte representation so Contains
	// never compares mismatched lengths.
	if got := len(acl.allowed[0].IP); got != net.IPv4len {
		t.Errorf("bare IPv4 host should be 4 bytes, got %d", got)
	}
	if got := len(acl.allowed[1].IP); got != net.IPv6len {
		t.Errorf("bare IPv6 host should be 16 bytes, got %d", got)
	}
	// And enforcement still matches both forms, including the IPv4-mapped
	// representation a client may send.
	if denied, _ := enforce("203.0.113.7", acl.allowed, acl.denied); denied {
		t.Error("bare IPv4 host should match itself")
	}
	if denied, _ := enforce("2001:db8::1", acl.allowed, acl.denied); denied {
		t.Error("bare IPv6 host should match itself")
	}
}

func TestEnforceIPv6(t *testing.T) {
	config := mustParse(t, `{"consumers":{"a":{"allowedCidrs":["2001:db8::/32"]}}}`)
	acl := config.lookupACL("a")
	if denied, _ := enforce("2001:db8::1", acl.allowed, acl.denied); denied {
		t.Error("IPv6 in allow range should pass")
	}
	if denied, _ := enforce("2001:db9::1", acl.allowed, acl.denied); !denied {
		t.Error("IPv6 outside allow range should be rejected")
	}
}

func TestSplitWildcard(t *testing.T) {
	cases := []struct {
		key    string
		prefix bool
		ok     bool
	}{
		{"gpustack-*", true, true},
		{"*-ci", false, true},
		{"*", true, true}, // catch-all
		{"alice", false, false},
		{"a*b*", false, false}, // more than one * → exact
		{"a*b", false, false},  // * in the middle → exact
	}
	for _, c := range cases {
		_, prefix, ok := splitWildcard(c.key)
		if ok != c.ok || (ok && prefix != c.prefix) {
			t.Errorf("splitWildcard(%q) = prefix=%v ok=%v", c.key, prefix, ok)
		}
	}
}

func TestLookupACLWildcard(t *testing.T) {
	// Declaration order matters: narrower patterns first, `*` last.
	config := mustParse(t, `{"consumers":{
		"gpustack-admin": {"deniedCidrs": ["9.9.9.9"]},
		"gpustack-*": {"allowedCidrs": ["10.0.0.0/8"]},
		"*-ci": {"deniedCidrs": ["203.0.113.0/24"]},
		"*": {"deniedCidrs": ["198.51.100.0/24"]}
	}}`)

	// Exact name wins over wildcard.
	if got := config.lookupACL("gpustack-admin"); got != config.exact["gpustack-admin"] {
		t.Error("exact name should win over wildcard")
	}
	// Prefix wildcard.
	if acl := config.lookupACL("gpustack-dev"); acl == nil || len(acl.allowed) != 1 {
		t.Error("gpustack-* should match gpustack-dev")
	}
	// Suffix wildcard (declared before `*`).
	if acl := config.lookupACL("team-ci"); acl == nil || len(acl.denied) != 1 || acl.denied[0].String() != "203.0.113.0/24" {
		t.Error("*-ci should match team-ci")
	}
	// Catch-all fallback.
	if acl := config.lookupACL("someone-else"); acl == nil || len(acl.denied) != 1 || acl.denied[0].String() != "198.51.100.0/24" {
		t.Error("* should be the catch-all default")
	}
}

func TestLookupACLNoMatchUnrestricted(t *testing.T) {
	config := mustParse(t, `{"consumers":{"gpustack-*": {"deniedCidrs": ["10.0.0.0/8"]}}}`)
	if acl := config.lookupACL("other-team"); acl != nil {
		t.Error("consumer matching no rule should be unrestricted")
	}
}

func TestOverrideRuleConfig(t *testing.T) {
	global := mustParse(t, `{
		"realIPHeader": "x-forwarded-for",
		"consumers": {
			"alice": {"allowedCidrs": ["192.168.0.0/16"]},
			"bob": {"deniedCidrs": ["10.0.0.0/8"]},
			"gpustack-*": {"allowedCidrs": ["10.0.0.0/8"]}
		}
	}`)

	var rule IpAclConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{
		"consumers": {
			"alice": {"allowedCidrs": ["203.0.113.0/24"]},
			"gpustack-*": {"deniedCidrs": ["198.51.100.0/24"]}
		}
	}`), *global, &rule); err != nil {
		t.Fatal(err)
	}

	// realIPHeader inherited.
	if rule.realIPHeader != "x-forwarded-for" {
		t.Errorf("realIPHeader should inherit global, got %q", rule.realIPHeader)
	}
	// alice overridden.
	alice := rule.lookupACL("alice")
	if alice == nil || len(alice.allowed) != 1 || alice.allowed[0].String() != "203.0.113.0/24" {
		t.Error("alice should be overridden by the rule")
	}
	// bob inherited untouched.
	bob := rule.lookupACL("bob")
	if bob == nil || len(bob.denied) != 1 || bob.denied[0].String() != "10.0.0.0/8" {
		t.Error("bob should be inherited from global")
	}
	// Wildcard overridden in place (position preserved, still before nothing else).
	gw := rule.lookupACL("gpustack-dev")
	if gw == nil || len(gw.allowed) != 0 || len(gw.denied) != 1 || gw.denied[0].String() != "198.51.100.0/24" {
		t.Error("gpustack-* should be overridden by the rule")
	}
	// Global must remain unmodified.
	if g := global.lookupACL("alice"); g == nil || g.allowed[0].String() != "192.168.0.0/16" {
		t.Error("global config must not be mutated by override parsing")
	}
	if len(global.patterns) != 1 {
		t.Error("global pattern slice must not be mutated")
	}
}

func TestOverrideRuleConfigErrorFailsClosed(t *testing.T) {
	global := mustParse(t, `{"consumers":{"alice":{"allowedCidrs":["192.168.0.0/16"]}}}`)

	// The rule target already holds a valid config from a previous parse.
	rule := mustParse(t, `{"consumers":{"bob":{"deniedCidrs":["10.0.0.0/8"]}}}`)

	// An invalid override must fail CLOSED: deny-all, never a partial
	// application of the new config.
	if err := parseOverrideRuleConfig(gjson.Parse(`{"consumers":{"bad":{"allowedCidrs":["not-an-ip"]}}}`), *global, rule); err != nil {
		t.Fatalf("override parse should swallow the error and fail closed, got %v", err)
	}
	for _, consumer := range []string{"bob", "alice", "anyone"} {
		acl := rule.lookupACL(consumer)
		if acl == nil || len(acl.denied) != 2 {
			t.Errorf("broken override should deny consumer %q", consumer)
		}
	}
	denied, reason := enforce("192.168.0.1", rule.lookupACL("bob").allowed, rule.lookupACL("bob").denied)
	if !denied || reason == "" {
		t.Error("broken override must reject traffic")
	}
}

func TestBrokenGlobalConfigFailsClosed(t *testing.T) {
	var config IpAclConfig
	// parseGlobalConfig must swallow the strict error and pin deny-all —
	// returning the error would be dropped by wasm-go's RuleMatcher when
	// matchRules exist (fail-open), see the denyAllConfig doc comment.
	if err := parseGlobalConfig(gjson.Parse(`{"consumers":["not-an-object"]}`), &config); err != nil {
		t.Fatalf("global parse should swallow the error and fail closed, got %v", err)
	}
	acl := config.lookupACL("anyone")
	if acl == nil || len(acl.denied) != 2 {
		t.Fatal("broken global config should deny every consumer")
	}
	if denied, _ := enforce("203.0.113.7", acl.allowed, acl.denied); !denied {
		t.Error("broken global config must reject traffic")
	}
	if denied, _ := enforce("2001:db8::1", acl.allowed, acl.denied); !denied {
		t.Error("broken global config must reject IPv6 traffic too")
	}
}

func TestParseCIDRListRejectsBadElements(t *testing.T) {
	for _, raw := range []string{
		`{"consumers":{"a":{"allowedCidrs":[""]}}}`,             // empty entry
		`{"consumers":{"a":{"allowedCidrs":[null]}}}`,           // null entry
		`{"consumers":{"a":{"allowedCidrs":[42]}}}`,             // non-string entry
		`{"consumers":{"a":{"deniedCidrs":["10.0.0.0/8",""]}}}`, // empty among valid
	} {
		var config IpAclConfig
		if err := parseConfig(gjson.Parse(raw), &config); err == nil {
			t.Errorf("%s should fail loudly", raw)
		}
	}
}

func TestOverrideRuleConfigRealIPHeaderOverride(t *testing.T) {
	global := mustParse(t, `{"realIPHeader": "x-forwarded-for"}`)
	var rule IpAclConfig
	if err := parseOverrideRuleConfig(gjson.Parse(`{"realIPHeader": "x-real-ip"}`), *global, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.realIPHeader != "x-real-ip" {
		t.Errorf("realIPHeader should be overridden, got %q", rule.realIPHeader)
	}
}
