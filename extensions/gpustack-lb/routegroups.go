package main

// groups: deterministic request->pool narrowing, applied by the context
// role (the publisher) before the candidate set is built. This is a **hard
// constraint**, not a scoring opinion: a group that matches removes every
// candidate outside it from the set, so the capability band and the finisher
// can only pick within the narrowed pool. That is why this lives here and
// not in a rank entry -- the weighted sum cannot express "must go to this
// pool", and a high-weight entry can always be outvoted.
//
// A group is a named matcher. Candidates list the group names they belong
// to in `candidates[].groups`, and the name they list is the group's `name`.
// One word for one idea: the config section is `groups`, a candidate's
// membership is `groups`, and selecting by name is what a group does.
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

// warnf is proxywasm.LogWarnf behind a variable so parse-time warnings stay
// testable: the SDK's hostcall mock panics outside a wasm VM, and the parse
// path runs in unit tests. Tests swap it for a capture.
var warnf = proxywasm.LogWarnf

// routeHeaderMatch matches one request header. Exactly one of exact / prefix
// must be set at parse time; a group's header entries are AND-ed.
type routeHeaderMatch struct {
	name   string
	exact  string
	prefix string
}

// routeGroup is one `groups[]` entry: a named matcher plus the pool it
// narrows to.
type routeGroup struct {
	// name is the group's identity AND its selector: the group narrows to
	// the candidates whose `groups` contain this name. Required, and unique
	// by last-wins — a redefinition replaces the earlier one in place, so the
	// list order (and therefore first-match) is preserved. See parseRouteGroups.
	name string

	headers []routeHeaderMatch

	// model matches the client's model name with model-mapper's pattern
	// syntax (exact / "prefix*" / "*"). Empty means no model condition.
	model string

	// minLength / maxLength bound the request body length in bytes. nil
	// means unbounded on that side. A group that sets either requires a
	// measurement: no length available -> the group does not match (the
	// request stays on the full pool -- safe degradation, never a wrong
	// pool).
	minLength *int64
	maxLength *int64

	// targets is the legacy selector, retained for one minor version and
	// removed in 0.4.0. It names targetIds, so it breaks whenever a target is
	// rebuilt. When present it selects by targetId and `name` is only a
	// label; when absent the group selects by its own name.
	targets []string
}

// parseRouteGroups is a pure function (host calls are in the callers), so it
// is directly unit-testable. candidateIDs is the set of valid targetId
// values from the same config's `candidates`; a legacy `targets` entry
// referencing anything else is an **error, not a silent skip** -- a typo'd
// targetId would otherwise read as "narrow to a pool that does not exist"
// and, depending on the narrowing result, either reject traffic or route to
// the wrong place with nothing in the config saying why.
func parseRouteGroups(j gjson.Result, candidateIDs map[string]struct{}) ([]routeGroup, error) {
	if !j.Exists() || j.Type == gjson.Null {
		return nil, nil
	}
	if !j.IsArray() {
		return nil, errors.New("groups must be an array")
	}
	var groups []routeGroup
	// name -> index in groups. A duplicate name replaces the earlier group IN
	// PLACE: the list is ordered (first match wins), so a redefinition must
	// keep the original slot or the override would silently change which
	// group a request hits. Last-wins for content, first-wins for position.
	byName := make(map[string]int, len(j.Array()))
	for i, r := range j.Array() {
		if !r.IsObject() {
			return nil, fmt.Errorf("groups[%d] must be an object", i)
		}
		// name is the selector, so it is not a label: a group without one
		// would select nothing and read as a no-op. The type check matters
		// because gjson renders `true` as "true" -- a non-string would
		// silently become a routing identity that could replace a real group.
		nameRes := r.Get("name")
		if nameRes.Type != gjson.String || nameRes.String() == "" {
			return nil, fmt.Errorf("groups[%d] must set name as a non-empty string: it is what candidates[].groups references", i)
		}
		name := nameRes.String()
		what := fmt.Sprintf("groups[%d] (%s)", i, name)

		grp := routeGroup{name: name}

		// A malformed `match` must not read as "no conditions": the nested
		// lookups below would find nothing and the group would silently
		// become a catch-all, shadowing every group after it. Non-object
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
				grp.headers = append(grp.headers, hm)
			}
		}

		grp.model = r.Get("match.model").String()

		if cl := r.Get("match.contextLength"); cl.Exists() && cl.Type != gjson.Null {
			if !cl.IsObject() {
				return nil, fmt.Errorf("%s match.contextLength must be an object", what)
			}
			for _, side := range []struct {
				key   string
				dst   **int64
				label string
			}{{"min", &grp.minLength, "min"}, {"max", &grp.maxLength, "max"}} {
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
			if grp.minLength != nil && grp.maxLength != nil && *grp.minLength > *grp.maxLength {
				return nil, fmt.Errorf("%s match.contextLength.min (%d) is greater than max (%d)", what, *grp.minLength, *grp.maxLength)
			}
		}
		// `targets` is the legacy selector. Its presence is what selects by
		// targetId; without it the group selects by its own name. There is
		// no precedence question: the field is either there or not.
		targetsRes := r.Get("targets")
		if hasTargets := targetsRes.Exists() && targetsRes.Type != gjson.Null; hasTargets {
			targets, err := parseLegacyTargets(targetsRes, what, candidateIDs)
			if err != nil {
				return nil, err
			}
			grp.targets = targets
		}
		// Duplicate name: replace the earlier definition in place so the
		// override keeps the original slot in the first-match order.
		if at, dup := byName[name]; dup {
			warnf("groups: name %q redefined at index %d; overriding the definition at index %d",
				name, i, at)
			groups[at] = grp
			continue
		}
		byName[name] = len(groups)
		groups = append(groups, grp)
	}
	return groups, nil
}

// parseLegacyTargets validates the legacy `targets` array against the
// config's candidate targetIds. Same convention as modelMappers: a malformed
// value must not be read as "absent".
func parseLegacyTargets(ts gjson.Result, what string, candidateIDs map[string]struct{}) ([]string, error) {
	if !ts.Exists() || ts.Type == gjson.Null || !ts.IsArray() || len(ts.Array()) == 0 {
		return nil, fmt.Errorf("%s targets must be a non-empty array", what)
	}
	var targets []string
	seen := make(map[string]struct{}, len(ts.Array()))
	for _, t := range ts.Array() {
		// A targetId is a routing identity, so a non-string is a config
		// error rather than its string form: gjson renders `true` as "true",
		// which would then match a candidate whose targetId is literally
		// "true".
		if t.Type != gjson.String {
			return nil, fmt.Errorf("%s targets[] must be a string, got %s", what, t.Raw)
		}
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

// selects reports whether a candidate belongs to this group's pool. The
// group's name is the selector: a candidate is in when its `groups` lists
// that name. `targets` is the legacy selector -- when present it selects by
// targetId and the name is only a label. Fallback candidates are handled by
// the caller and never reach here as a rejection -- see applyRouteGroups.
func (g *routeGroup) selects(c candidateSpec) bool {
	if len(g.targets) > 0 {
		for _, t := range g.targets {
			if c.TargetID == t {
				return true
			}
		}
		return false
	}
	for _, name := range c.groups {
		if name == g.name {
			return true
		}
	}
	return false
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

// evaluateRouteGroups picks the first group whose conditions hold and returns
// it. All inputs are injected (headerGetter returns "" for an absent header),
// keeping this pure and unit-testable -- host calls live in applyRouteGroups.
// The caller reads the returned group's selector (its name, or the legacy
// targets); returning the group rather than just the selector is what lets it
// tell the two apart.
func evaluateRouteGroups(groups []routeGroup, clientModel string, headerGetter func(string) string, length int64, hasLength bool) (*routeGroup, bool) {
	for i := range groups {
		grp := &groups[i]
		ok := true
		for _, h := range grp.headers {
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
		if !matchModelPattern(grp.model, clientModel) {
			continue
		}
		if grp.minLength != nil || grp.maxLength != nil {
			// A length condition without a measurement does not match: the
			// request stays on the full pool rather than being routed on a
			// guess.
			if !hasLength {
				continue
			}
			if grp.minLength != nil && length < *grp.minLength {
				continue
			}
			if grp.maxLength != nil && length > *grp.maxLength {
				continue
			}
		}
		return grp, true
	}
	return nil, false
}

// applyRouteGroups evaluates config.groups against this request and returns
// the narrowed candidate list. No groups configured, or none matching,
// returns config.candidates unchanged -- behaviour identical to before the
// feature existed.
func applyRouteGroups(config Config, clientModel string) []candidateSpec {
	if len(config.groups) == 0 {
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
	grp, matched := evaluateRouteGroups(config.groups, clientModel, headerGetter, length, hasLength)
	if !matched {
		return config.candidates
	}
	out := narrowByGroup(config.candidates, grp)
	proxywasm.LogDebugf("%s: group %q narrowed %d candidates to %d", pluginName, grp.name, len(config.candidates), len(out))
	return out
}

// narrowByGroup is the pure half of applyRouteGroups: which candidates a
// matched group keeps, in their original order (the candidates were sorted at
// parse time and the weighted-interval / scoring code downstream relies on
// that order).
//
// Fallback candidates always survive. They take no share of normal traffic --
// the tier split publishes only the lowest tier -- so keeping them eligible
// does not weaken the group; it is what lets a group-pinned request still
// reach the route-level fallback pool once its pinned targets fail. Narrowing
// them out instead makes the failure path unreachable: verified live, a group
// pinning one target turned every fallback into dead capacity and returned 503.
func narrowByGroup(cands []candidateSpec, grp *routeGroup) []candidateSpec {
	out := make([]candidateSpec, 0, len(cands))
	for _, c := range cands {
		if c.Fallback {
			out = append(out, c)
			continue
		}
		if !grp.selects(c) {
			continue
		}
		out = append(out, c)
	}
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
