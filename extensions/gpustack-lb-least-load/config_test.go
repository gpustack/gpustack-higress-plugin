package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

func parse(t *testing.T, raw string) Config {
	t.Helper()
	var c Config
	if err := parseConfig(gjson.Parse(raw), &c); err != nil {
		t.Fatalf("parseConfig(%s): %v", raw, err)
	}
	return c
}

// Installing this CR already means you want it, so it defaults to on.
func TestEnabledDefaultsTrue(t *testing.T) {
	if !parse(t, `{}`).shouldEnable() {
		t.Error("empty config should be enabled")
	}
	if !parse(t, `{"enabled":true}`).shouldEnable() {
		t.Error("explicit true")
	}
	if parse(t, `{"enabled":false}`).shouldEnable() {
		t.Error("explicit false must be honoured")
	}
}

// "Unset" and "explicitly false" must be distinguishable, or the inheritance
// logic has no way to decide whether to override.
func TestEnabledPresenceIsDistinguishable(t *testing.T) {
	if p := parse(t, `{}`).enabled; p != nil {
		t.Errorf("unset should stay nil, got %v", *p)
	}
	if p := parse(t, `{"enabled":false}`).enabled; p == nil || *p {
		t.Errorf("explicit false should survive as a real false, got %v", p)
	}
}

// Both shapes have to be expressible.
func TestOverrideInheritance(t *testing.T) {
	tests := []struct {
		name   string
		global string
		rule   string
		want   bool
	}{
		{"global on, rule silent → on", `{}`, `{}`, true},
		{"global on, rule off → off", `{}`, `{"enabled":false}`, false},
		{"global off, rule silent → off", `{"enabled":false}`, `{}`, false},
		{"global off, rule on → on", `{"enabled":false}`, `{"enabled":true}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global := parse(t, tt.global)
			var c Config
			if err := parseOverrideConfig(gjson.Parse(tt.rule), global, &c); err != nil {
				t.Fatalf("parseOverrideConfig: %v", err)
			}
			if got := c.shouldEnable(); got != tt.want {
				t.Errorf("shouldEnable = %v, want %v", got, tt.want)
			}
		})
	}
}

// When the global config fails to parse, rule_matcher **swallows the error and
// leaves globalConfig at its zero value**, so inheritance receives a zero-valued
// Config. The pointer on enabled exists for exactly this moment: the zero
// value's nil is inherited as nil and read as the default true, rather than
// silently degrading every route to round-robin.
func TestZeroGlobalDoesNotDisableEverything(t *testing.T) {
	var zeroGlobal Config

	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{}`), zeroGlobal, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if !c.shouldEnable() {
		t.Error("a zero-valued global must not silently disable load awareness")
	}
}

// ── weight ──

// Defaults to 1; <= 0 and invalid values all fall back to 1 (0 is not a
// meaningful value -- "do not participate" is expressed with enabled).
func TestWeightDefaultsAndGuards(t *testing.T) {
	tests := []struct {
		raw  string
		want float64
	}{
		{`{}`, 1},
		{`{"weight":2}`, 2},
		{`{"weight":0.5}`, 0.5},
		{`{"weight":0}`, 1},
		{`{"weight":-3}`, 1},
		{`{"weight":"nonsense"}`, 1},
	}
	for _, tt := range tests {
		if got := parse(t, tt.raw).weight; got != tt.want {
			t.Errorf("%s → weight = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestWeightInheritance(t *testing.T) {
	global := parse(t, `{"weight":3}`)
	tests := []struct {
		name string
		rule string
		want float64
	}{
		{"silent rule inherits", `{}`, 3},
		{"rule overrides explicitly", `{"weight":5}`, 5},
		// The rule wrote an invalid value: parseConfig has already fallen back
		// to 1, and j.Get("weight") exists, so nothing is inherited -- an
		// explicit mistake beats inheritance, which avoids "I configured it and
		// got something else".
		{"rule wrote an invalid value", `{"weight":-1}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := parseOverrideConfig(gjson.Parse(tt.rule), global, &c); err != nil {
				t.Fatalf("parseOverrideConfig: %v", err)
			}
			if c.weight != tt.want {
				t.Errorf("weight = %v, want %v", c.weight, tt.want)
			}
		})
	}
}

// rule_matcher retries a failed rule parse against a backup JSON using the
// **same** &rule.config. enabled is only written when the key is present, so
// without a reset an earlier `enabled: false` would survive into a retry whose
// JSON omits it -- and parseOverrideConfig only inherits when the pointer is
// nil, so neither the default nor the global value could take effect. The route
// would stay silently degraded to round-robin.
func TestReparseClearsEnabled(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"enabled":false}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.shouldEnable() {
		t.Fatal("precondition: explicit false should disable")
	}

	if err := parseConfig(gjson.Parse(`{}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.enabled != nil {
		t.Errorf("enabled should be cleared on reparse, got %v", *c.enabled)
	}
	if !c.shouldEnable() {
		t.Error("a reparse without the key must fall back to the default, not the stale value")
	}
}

// gjson's Bool() coerces a non-boolean to false, and here false is a meaningful
// setting -- so a typo would read as a deliberate disable, silently dropping
// the load opinion and leaving the route on round-robin.
func TestEnabledMustBeAStrictBoolean(t *testing.T) {
	var c Config
	for _, raw := range []string{
		`{"enabled":"yes"}`,
		`{"enabled":"false"}`,
		`{"enabled":0}`,
		`{"enabled":[]}`,
	} {
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("%s must be rejected, not read as an explicit false", raw)
		}
	}

	// null reads as unset, so the default (and any inherited global) still wins.
	if err := parseConfig(gjson.Parse(`{"enabled":null}`), &c); err != nil {
		t.Fatalf("null should be treated as unset: %v", err)
	}
	if c.enabled != nil {
		t.Errorf("null should leave enabled unset, got %v", *c.enabled)
	}
	if !c.shouldEnable() {
		t.Error("unset must read as the default, true")
	}
}
