// String matchers for the `allowed_headers` / `allowed_upstream_headers` /
// `allowed_client_headers` config lists.
//
// Derived from the Higress ext-auth plugin's `expr` package.
// Upstream: https://github.com/alibaba/higress/blob/c8b82797c51a97faca46e2ae12990453f5026802/plugins/wasm-go/extensions/ext-auth/expr/matcher.go
//           https://github.com/alibaba/higress/blob/c8b82797c51a97faca46e2ae12990453f5026802/plugins/wasm-go/extensions/ext-auth/expr/repeated_string_matcher.go
// Trimmed to the header-matching use case (the path whitelist/blacklist half of
// upstream `expr` is not carried over -- see config.go) and given deterministic
// pattern precedence; keep this attribution when editing.

package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Match pattern keys accepted inside a matcher entry, e.g. `{"exact": "cookie"}`.
const (
	matchPatternExact    = "exact"
	matchPatternPrefix   = "prefix"
	matchPatternSuffix   = "suffix"
	matchPatternContains = "contains"
	matchPatternRegex    = "regex"

	matchIgnoreCase = "ignore_case"
)

// matchPatternOrder fixes the precedence used when one entry carries more than
// one pattern key. Upstream ranges over a map here, so which pattern wins is
// whatever Go's randomized map iteration picks that run -- a config like
// `{"exact": "a", "prefix": "b"}` would silently behave differently between
// two VMs built from the same CR. Ordering it makes the outcome a property of
// the config rather than of the runtime.
var matchPatternOrder = []string{
	matchPatternExact,
	matchPatternPrefix,
	matchPatternSuffix,
	matchPatternContains,
	matchPatternRegex,
}

// Case-insensitive matching is EqualFold rather than lower-casing the input:
// same result, no allocation whatever the input looks like, and about a third
// faster. The prefix and suffix forms compare a fixed-length slice, which is
// sound because these only ever see header names -- ASCII by specification, so
// case folding cannot change a string's byte length.

// matcher reports whether a string satisfies a configured pattern.
type matcher interface {
	Match(s string) bool
}

type exactMatcher struct {
	target     string
	ignoreCase bool
}

func (m *exactMatcher) Match(s string) bool {
	if m.ignoreCase {
		return strings.EqualFold(s, m.target)
	}
	return s == m.target
}

type prefixMatcher struct {
	target     string
	ignoreCase bool
}

func (m *prefixMatcher) Match(s string) bool {
	if m.ignoreCase {
		if len(s) < len(m.target) {
			return false
		}
		return strings.EqualFold(s[:len(m.target)], m.target)
	}
	return strings.HasPrefix(s, m.target)
}

type suffixMatcher struct {
	target     string
	ignoreCase bool
}

func (m *suffixMatcher) Match(s string) bool {
	if m.ignoreCase {
		if len(s) < len(m.target) {
			return false
		}
		return strings.EqualFold(s[len(s)-len(m.target):], m.target)
	}
	return strings.HasSuffix(s, m.target)
}

type containsMatcher struct {
	target     string
	ignoreCase bool
}

func (m *containsMatcher) Match(s string) bool {
	if m.ignoreCase {
		return strings.Contains(strings.ToLower(s), m.target)
	}
	return strings.Contains(s, m.target)
}

type regexMatcher struct {
	regex *regexp.Regexp
}

func (m *regexMatcher) Match(s string) bool {
	return m.regex.MatchString(s)
}

// repeatedMatcher is a disjunction: it matches when any of its members does.
// A nil *repeatedMatcher matches nothing, which is what an absent config list
// should mean (see matchesHeader).
type repeatedMatcher struct {
	matchers []matcher
}

func (m *repeatedMatcher) Match(s string) bool {
	for _, sub := range m.matchers {
		if sub.Match(s) {
			return true
		}
	}
	return false
}

func newMatcher(pattern, target string, ignoreCase bool) (matcher, error) {
	if ignoreCase && pattern != matchPatternRegex {
		target = strings.ToLower(target)
	}
	switch pattern {
	case matchPatternExact:
		return &exactMatcher{target: target, ignoreCase: ignoreCase}, nil
	case matchPatternPrefix:
		return &prefixMatcher{target: target, ignoreCase: ignoreCase}, nil
	case matchPatternSuffix:
		return &suffixMatcher{target: target, ignoreCase: ignoreCase}, nil
	case matchPatternContains:
		return &containsMatcher{target: target, ignoreCase: ignoreCase}, nil
	case matchPatternRegex:
		if ignoreCase && !strings.HasPrefix(target, "(?i)") {
			target = "(?i)" + target
		}
		re, err := regexp.Compile(target)
		if err != nil {
			return nil, fmt.Errorf("compile regex matcher %q: %w", target, err)
		}
		return &regexMatcher{regex: re}, nil
	}
	return nil, fmt.Errorf("unknown string matcher type %q", pattern)
}

// buildHeaderMatcher compiles a list of matcher entries. Header names are
// compared case-insensitively unconditionally: HTTP header names are
// case-insensitive by definition, and Envoy hands them to us lower-cased, so a
// case-sensitive `{"exact": "X-Real-IP"}` in a CR would never fire.
//
// Returns nil for an empty or absent list, which callers must read as "matches
// nothing" rather than "matches everything".
func buildHeaderMatcher(entries []gjson.Result) (matcher, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	built := make([]matcher, 0, len(entries))
	for i, entry := range entries {
		sub, err := buildOneMatcher(entry, true)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		built = append(built, sub)
	}
	return &repeatedMatcher{matchers: built}, nil
}

func buildOneMatcher(entry gjson.Result, forceIgnoreCase bool) (matcher, error) {
	ignoreCase := forceIgnoreCase
	if !ignoreCase {
		ignoreCase = entry.Get(matchIgnoreCase).Bool()
	}
	for _, pattern := range matchPatternOrder {
		value := entry.Get(pattern)
		if value.Exists() && value.String() != "" {
			return newMatcher(pattern, value.String(), ignoreCase)
		}
	}
	return nil, fmt.Errorf("no known match pattern in %s (want one of %s)",
		entry.Raw, strings.Join(matchPatternOrder, ", "))
}

// matchesHeader is the nil-safe way to consult an optional matcher.
func matchesHeader(m matcher, name string) bool {
	if m == nil {
		return false
	}
	return m.Match(name)
}
