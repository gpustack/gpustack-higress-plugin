package main

// routeRules: deterministic request->pool narrowing, applied by the context
// role (the publisher) before the candidate set is built. This is a **hard
// constraint**, not a scoring opinion: a rule that matches removes every
// candidate outside its `targets` from the set, so the capability band and
// the finisher can only pick within the narrowed pool. That is why this
// lives here and not in a rank entry -- the weighted sum cannot express
// "must go to this pool", and a high-weight entry can always be outvoted.
//
// Matching inputs, all read **live** at the publisher's position (340):
//
//   - headers: at 340 both the 810 strip and the 360 ext-auth injection
//     have already happened, so a live read is the post-authn view -- more
//     accurate than any earlier snapshot;
//   - model: the x-higress-llm-model header the publisher already reads;
//   - contextLength: gpustack_request_context.lengthBytes (written by
//     generic-proxy-router, which is the only participant that still knows
//     the length after a body rewrite drops Content-Length), falling back
//     to the Content-Length header on paths nothing rewrote.
//
// Design reference: designs/request-tagging-and-route-rules.md.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/tidwall/gjson"
)

// routeHeaderMatch matches one request header. Exactly one of exact / prefix
// must be set at parse time; a rule's header entries are AND-ed.
type routeHeaderMatch struct {
	name   string
	exact  string
	prefix string
}

// routeRule is one routeRules[] entry.
type routeRule struct {
	name string

	headers []routeHeaderMatch

	// model matches the client's model name with model-mapper's pattern
	// syntax (exact / "prefix*" / "*"). Empty means no model condition.
	model string

	// minLength / maxLength bound the request body length in bytes. nil
	// means unbounded on that side. A rule that sets either requires a
	// measurement: no length available -> the rule does not match (the
	// request stays on the full pool -- safe degradation, never a wrong
	// pool).
	minLength *int64
	maxLength *int64

	targets []string
}

// parseRouteRules is a pure function (host calls are in the callers), so it
// is directly unit-testable. candidateIDs is the set of valid targetId
// values from the same config's `candidates`; a rule referencing anything
// else is an **error, not a silent skip** -- a typo'd targetId would
// otherwise read as "narrow to a pool that does not exist" and, depending
// on the narrowing result, either reject traffic or route to the wrong
// place with nothing in the config saying why.
func parseRouteRules(j gjson.Result, candidateIDs map[string]struct{}) ([]routeRule, error) {
	if !j.Exists() || j.Type == gjson.Null {
		return nil, nil
	}
	if !j.IsArray() {
		return nil, errors.New("routeRules must be an array")
	}
	var rules []routeRule
	for i, r := range j.Array() {
		if !r.IsObject() {
			return nil, fmt.Errorf("routeRules[%d] must be an object", i)
		}
		what := fmt.Sprintf("routeRules[%d]", i)
		if n := r.Get("name").String(); n != "" {
			what = fmt.Sprintf("routeRules[%d] (%s)", i, n)
		}

		rule := routeRule{name: r.Get("name").String()}

		// A malformed `match` must not read as "no conditions": the nested
		// lookups below would find nothing and the rule would silently
		// become a catch-all, shadowing every rule after it. Non-object
		// values (an array of bare conditions, a string, ...) are config
		// errors instead.
		if m := r.Get("match"); m.Exists() && m.Type != gjson.Null && !m.IsObject() {
			return nil, fmt.Errorf("%s match must be an object", what)
		}

		if hs := r.Get("match.headers"); hs.Exists() && hs.Type != gjson.Null {
			if !hs.IsArray() {
				return nil, fmt.Errorf("%s match.headers must be an array", what)
			}
			for _, h := range hs.Array() {
				hm := routeHeaderMatch{name: h.Get("name").String()}
				if hm.name == "" {
					return nil, fmt.Errorf("%s match.headers[].name must not be empty", what)
				}
				hm.exact = h.Get("exact").String()
				hm.prefix = h.Get("prefix").String()
				if (hm.exact == "") == (hm.prefix == "") {
					// Both set is ambiguous, neither set matches nothing.
					return nil, fmt.Errorf("%s match.headers[%q] must set exactly one of exact / prefix", what, hm.name)
				}
				// Tolerance for the glob habit: a trailing '*' on a prefix
				// is stripped rather than matched literally.
				hm.prefix = strings.TrimSuffix(hm.prefix, "*")
				rule.headers = append(rule.headers, hm)
			}
		}

		rule.model = r.Get("match.model").String()

		if cl := r.Get("match.contextLength"); cl.Exists() && cl.Type != gjson.Null {
			if !cl.IsObject() {
				return nil, fmt.Errorf("%s match.contextLength must be an object", what)
			}
			for _, side := range []struct {
				key   string
				dst   **int64
				label string
			}{{"min", &rule.minLength, "min"}, {"max", &rule.maxLength, "max"}} {
				if v := cl.Get(side.key); v.Exists() && v.Type != gjson.Null {
					n, err := intField(v, fmt.Sprintf("%s match.contextLength.%s", what, side.label))
					if err != nil {
						return nil, err
					}
					if n < 0 {
						return nil, fmt.Errorf("%s match.contextLength.%s must not be negative, got %d", what, side.label, n)
					}
					*side.dst = &n
				}
			}
			if rule.minLength != nil && rule.maxLength != nil && *rule.minLength > *rule.maxLength {
				return nil, fmt.Errorf("%s match.contextLength.min (%d) is greater than max (%d)", what, *rule.minLength, *rule.maxLength)
			}
		}
		targets, err := parseRuleTargets(r.Get("targets"), what, candidateIDs)
		if err != nil {
			return nil, err
		}
		rule.targets = targets
		rules = append(rules, rule)
	}
	return rules, nil
}

// parseRuleTargets validates the targets array against the config's
// candidate targetIds. Same convention as modelMappers: a malformed value
// must not be read as "absent".
func parseRuleTargets(ts gjson.Result, what string, candidateIDs map[string]struct{}) ([]string, error) {
	if !ts.Exists() || ts.Type == gjson.Null || !ts.IsArray() || len(ts.Array()) == 0 {
		return nil, fmt.Errorf("%s targets must be a non-empty array", what)
	}
	var targets []string
	seen := make(map[string]struct{}, len(ts.Array()))
	for _, t := range ts.Array() {
		id := t.String()
		if id == "" {
			return nil, fmt.Errorf("%s targets[] must not be empty", what)
		}
		if _, ok := candidateIDs[id]; !ok {
			return nil, fmt.Errorf("%s targets references %q, which is not a candidate targetId in this config", what, id)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%s targets lists %q twice", what, id)
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	return targets, nil
}

// matchModelPattern matches the client's model name with model-mapper's
// single-pattern syntax: "*" matches everything, a trailing '*' is a prefix
// match, anything else is exact. Empty pattern means "no condition".
func matchModelPattern(pattern, clientModel string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(clientModel, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == clientModel
}

// evaluateRouteRules picks the first matching rule. All inputs are injected
// (headerGetter returns "" for an absent header), keeping this pure and
// unit-testable -- host calls live in applyRouteRules.
func evaluateRouteRules(rules []routeRule, clientModel string, headerGetter func(string) string, length int64, hasLength bool) (targets []string, matched bool) {
	for _, rule := range rules {
		ok := true
		for _, h := range rule.headers {
			v := headerGetter(h.name)
			if v == "" {
				ok = false
				break
			}
			if h.exact != "" {
				ok = v == h.exact
			} else {
				ok = strings.HasPrefix(v, h.prefix)
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		if !matchModelPattern(rule.model, clientModel) {
			continue
		}
		if rule.minLength != nil || rule.maxLength != nil {
			// A length rule without a measurement does not match: the
			// request stays on the full pool rather than being routed on
			// a guess.
			if !hasLength {
				continue
			}
			if rule.minLength != nil && length < *rule.minLength {
				continue
			}
			if rule.maxLength != nil && length > *rule.maxLength {
				continue
			}
		}
		return rule.targets, true
	}
	return nil, false
}

// applyRouteRules evaluates config.routeRules against this request and
// returns the narrowed candidate list. No rules configured, or no rule
// matching, returns config.candidates unchanged -- behaviour identical to
// before the feature existed.
func applyRouteRules(config Config, clientModel string) []candidateSpec {
	if len(config.routeRules) == 0 {
		return config.candidates
	}
	headerGetter := func(name string) string {
		v, err := proxywasm.GetHttpRequestHeader(name)
		if err != nil {
			return ""
		}
		return v
	}
	length, hasLength := requestLengthBytes()
	targets, matched := evaluateRouteRules(config.routeRules, clientModel, headerGetter, length, hasLength)
	if !matched {
		return config.candidates
	}
	want := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		want[t] = struct{}{}
	}
	// Order is preserved: the candidates were sorted at parse time and the
	// weighted-interval / scoring code downstream relies on that order.
	out := make([]candidateSpec, 0, len(config.candidates))
	for _, c := range config.candidates {
		if _, ok := want[c.TargetID]; ok {
			out = append(out, c)
		}
	}
	proxywasm.LogDebugf("%s: route rules narrowed %d candidates to %d", pluginName, len(config.candidates), len(out))
	return out
}

// requestLengthBytes resolves the request body length: the filter-state
// measurement first (it survives body rewrites, which drop Content-Length),
// the Content-Length header second (intact exactly on paths nothing
// rewrote -- the two sources are complementary).
func requestLengthBytes() (int64, bool) {
	if data, err := proxywasm.GetProperty([]string{FilterStateRequestContext}); err == nil && len(data) > 0 {
		var rc RequestContext
		if err := json.Unmarshal(data, &rc); err == nil && rc.LengthBytes != nil {
			return *rc.LengthBytes, true
		}
	}
	if v, err := proxywasm.GetHttpRequestHeader("content-length"); err == nil && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}
