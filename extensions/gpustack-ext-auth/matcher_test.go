package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Envoy hands header names down lower-cased, but a CR is written by hand and
// routinely spells them X-Real-IP. Matching has to be case-insensitive in both
// directions or such an entry silently never fires.
func TestHeaderMatchersFoldCase(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		matches []string
		misses  []string
	}{
		{
			name:    "exact",
			json:    `[{"exact": "X-Real-IP"}]`,
			matches: []string{"x-real-ip", "X-Real-IP", "X-REAL-IP"},
			misses:  []string{"x-real-ipx", "x-real-i", "", "real-ip"},
		},
		{
			name:    "prefix",
			json:    `[{"prefix": "X-GPUStack-"}]`,
			matches: []string{"x-gpustack-access-key", "X-GPUStack-Auth-Token"},
			misses:  []string{"x-gpustack", "gpustack-x-", "", "x-gpustac"},
		},
		{
			name:    "suffix",
			json:    `[{"suffix": "-Key"}]`,
			matches: []string{"x-api-key", "X-GPUStack-Access-KEY"},
			misses:  []string{"key", "-ke", "", "x-key-header"},
		},
		{
			name:    "contains",
			json:    `[{"contains": "GPUStack"}]`,
			matches: []string{"x-gpustack-key"},
			misses:  []string{"x-api-key", ""},
		},
		{
			name:    "regex",
			json:    `[{"regex": "^x-gpustack-.*-key$"}]`,
			matches: []string{"x-gpustack-access-key", "X-GPUStack-Access-Key"},
			misses:  []string{"x-gpustack-key", "y-gpustack-a-key"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := buildHeaderMatcher(gjson.Parse(tc.json).Array())
			if err != nil {
				t.Fatalf("buildHeaderMatcher: %v", err)
			}
			for _, name := range tc.matches {
				if !matchesHeader(m, name) {
					t.Errorf("%q should match", name)
				}
			}
			for _, name := range tc.misses {
				if matchesHeader(m, name) {
					t.Errorf("%q should not match", name)
				}
			}
		})
	}
}

// A shorter input than the pattern must not panic on the fixed-length slice the
// prefix and suffix forms take.
func TestHeaderMatchersHandleShortInput(t *testing.T) {
	for _, json := range []string{`[{"prefix": "x-long-header-name"}]`, `[{"suffix": "x-long-header-name"}]`} {
		m, err := buildHeaderMatcher(gjson.Parse(json).Array())
		if err != nil {
			t.Fatalf("buildHeaderMatcher: %v", err)
		}
		if matchesHeader(m, "x-") {
			t.Errorf("%s matched a shorter name", json)
		}
	}
}

// An absent list must mean "matches nothing", never "matches everything" --
// callers use it to decide which client headers cross to the auth service.
func TestNilHeaderMatcherMatchesNothing(t *testing.T) {
	m, err := buildHeaderMatcher(nil)
	if err != nil {
		t.Fatalf("buildHeaderMatcher: %v", err)
	}
	if m != nil || matchesHeader(m, "anything") {
		t.Error("an empty list must not match")
	}
}

// Upstream ranges over a map to pick the pattern, so an entry carrying two of
// them behaves differently between VMs built from the same CR.
func TestPatternPrecedenceIsDeterministic(t *testing.T) {
	for range 20 {
		m, err := buildHeaderMatcher(gjson.Parse(`[{"prefix": "b", "exact": "a"}]`).Array())
		if err != nil {
			t.Fatalf("buildHeaderMatcher: %v", err)
		}
		if !matchesHeader(m, "a") || matchesHeader(m, "bc") {
			t.Fatal("exact must win over prefix, every time")
		}
	}
}
