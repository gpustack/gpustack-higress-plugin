package wire

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TargetPatternsKey is the configuration key both gpustack-generic-proxy-router
// and gpustack-lb read the target rules from. They must be configured alike:
// the router derives a target from the cluster that served a create and puts
// it in the ID, the LB derives one from each candidate and compares it with
// the pin. Two sets of rules would make every wrapped ID a pin miss.
const TargetPatternsKey = "targetPatterns"

// TargetRule is one entry of targetPatterns: a pair of regexes whose capture
// groups correspond by position --
//
//	target:  '^(\d+)$'              what a target looks like
//	cluster: 'model-\d+-(\d+)$'     the service names it stands for
//
// Everything outside a group is a regex and takes no part in the mapping;
// everything inside a group is the mapped value, and the n-th group of one
// side IS the n-th group of the other. With the defaults:
//
//	target "2"               -> the candidate whose service name is model-<model>-2
//	service name model-42-2  -> the target "2", written into a wrapped ID
//
// Both sides are compiled anchored, so a target or a service name is always
// matched as a whole. The group counts must be equal: that is the one
// structural check, and it replaces a template syntax -- nothing is
// substituted into a pattern, a group is replaced whole or not at all.
//
// For the target to be rebuildable from a service name, `target` must be a
// plain sequence of literals and capture groups (`^provider-(\d+)$`: the
// literal "provider-", then group 1); anything else is rejected at load time.
//
// The target regexes of a list must not overlap: a target is claimed by the
// FIRST rule whose `target` matches it. An identical regex twice is rejected
// at load time; any other overlap makes TargetOf refuse to wrap (the native
// ID goes out) rather than risk the wrong owner.
type TargetRule struct {
	Target  string `json:"target"`
	Cluster string `json:"cluster"`
}

// DefaultTargetPatterns are used when targetPatterns is not configured. They
// are GPUStack's cluster naming contract: `model-<model>-<instance>` yields
// the instance id, `provider-<id>` is kept whole.
var DefaultTargetPatterns = []TargetRule{
	{Target: `^(\d+)$`, Cluster: `model-\d+-(\d+)$`},
	{Target: `^provider-(\d+)$`, Cluster: `provider-(\d+)$`},
}

// compiledTargetRule is a TargetRule ready to be read backwards.
type compiledTargetRule struct {
	target  *regexp.Regexp
	cluster *regexp.Regexp
	// literals are the literal segments of `target` around its capture
	// groups: len(literals) == number of groups + 1. A target is rebuilt by
	// interleaving them with the groups the service name yielded.
	literals []string
}

// TargetRules is a compiled targetPatterns list.
type TargetRules []compiledTargetRule

// legacyTemplate matches the `$n` of the template syntax this replaced.
var legacyTemplate = regexp.MustCompile(`\$\d`)

// CompileTargetRules compiles the configured rules, with the defaults for an
// empty list.
func CompileTargetRules(rules []TargetRule) (TargetRules, error) {
	if len(rules) == 0 {
		rules = DefaultTargetPatterns
	}
	compiled := make(TargetRules, 0, len(rules))
	seen := map[string]int{}
	for i, r := range rules {
		c, err := compileTargetRule(r)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", TargetPatternsKey, i, err)
		}
		// The same target regex twice is the one overlap that can be seen
		// without deciding regex intersection. The general case -- two
		// different regexes with a common target -- is caught at run time
		// by TargetOf refusing to wrap (see there).
		if prev, dup := seen[r.Target]; dup {
			return nil, fmt.Errorf("%s[%d]: target %q is already claimed by rule %d; target regexes must not overlap", TargetPatternsKey, i, r.Target, prev)
		}
		seen[r.Target] = i
		compiled = append(compiled, c)
	}
	return compiled, nil
}

func compileTargetRule(r TargetRule) (compiledTargetRule, error) {
	c := compiledTargetRule{}
	if r.Target == "" || r.Cluster == "" {
		return c, errors.New("both target and cluster are required")
	}
	target, err := regexp.Compile("^(?:" + r.Target + ")$")
	if err != nil {
		return c, fmt.Errorf("target: invalid regex %q: %w", r.Target, err)
	}
	literals, groups, err := splitTargetRegex(r.Target)
	if err != nil {
		return c, fmt.Errorf("target %q: %w", r.Target, err)
	}
	if len(groups) != target.NumSubexp() {
		return c, fmt.Errorf("target %q: unsupported capture group", r.Target)
	}
	cluster, err := regexp.Compile("^(?:" + r.Cluster + ")$")
	if err != nil {
		return c, fmt.Errorf("cluster: invalid regex %q: %w", r.Cluster, err)
	}
	if cluster.NumSubexp() != target.NumSubexp() {
		return c, fmt.Errorf("cluster %q has %d capture group(s) but target %q has %d: the groups pair by position",
			r.Cluster, cluster.NumSubexp(), r.Target, target.NumSubexp())
	}
	// `$1` in a regex is a valid end anchor followed by a digit, so a rule
	// written in the older template syntax (`model-(\d+)-$1`) compiles, has
	// the right group count, and can never match anything. Say so instead.
	if legacyTemplate.MatchString(r.Target) || legacyTemplate.MatchString(r.Cluster) {
		return c, errors.New("$n is not supported: the capture groups of target and cluster pair by position, nothing is substituted")
	}
	return compiledTargetRule{target: target, cluster: cluster, literals: literals}, nil
}

// TargetOf returns the target a service name stands for: the first rule
// whose cluster regex matches it, with the service name's groups interleaved
// into the target's literals. "" when no rule matches, when a group the
// target needs did not take part, or when the target it would yield is
// claimed by an earlier rule (an ambiguous list -- see TargetRule). The name
// is the bare service name: Envoy cluster prefix and resolution suffix
// already removed by the caller.
func (rs TargetRules) TargetOf(service string) string {
	for i, r := range rs {
		m := r.cluster.FindStringSubmatch(service)
		if m == nil {
			continue
		}
		var b strings.Builder
		for g, lit := range r.literals {
			b.WriteString(lit)
			if g < len(m)-1 {
				b.WriteString(m[g+1])
			}
		}
		t := b.String()
		// The rebuilt target must satisfy the target regex with the very
		// groups the service name yielded -- a rule whose two sides do not
		// agree on a value simply does not claim it.
		if v := r.target.FindStringSubmatch(t); v == nil || !sameGroups(v[1:], m[1:]) {
			continue
		}
		if rs.ruleFor(t) != i {
			continue
		}
		return t
	}
	return ""
}

// ruleFor is the index of the rule that claims a target: the first whose
// target regex matches it.
func (rs TargetRules) ruleFor(target string) int {
	for i, r := range rs {
		if r.target.MatchString(target) {
			return i
		}
	}
	return -1
}

func sameGroups(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitTargetRegex cuts a target regex (anchors optional) into its literal
// segments and the sources of its capture groups, and rejects everything
// that is not a sequence of the two: a shorthand class or a quantifier
// outside a group, an anchor inside, a non-capturing or named group. Those
// cannot be rebuilt from a service name. A literal may use "\x" escapes
// for punctuation ("\-", "\.").
func splitTargetRegex(src string) (literals, groups []string, err error) {
	s := strings.TrimPrefix(src, "^")
	s = strings.TrimSuffix(s, "$")
	var cur strings.Builder
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if depth > 0 {
			switch ch {
			case '\\':
				i++
			case '[':
				if j := strings.IndexByte(s[i+1:], ']'); j >= 0 {
					i += j + 1
				}
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					groups = append(groups, s[start:i])
				}
			}
			continue
		}
		switch {
		case ch == '(':
			if i+1 < len(s) && s[i+1] == '?' {
				return nil, nil, errors.New("only plain capture groups are supported, not (?:...) or named groups")
			}
			literals = append(literals, cur.String())
			cur.Reset()
			start = i + 1
			depth = 1
		case ch == '\\' && i+1 < len(s):
			nxt := s[i+1]
			if isClassShorthand(nxt) {
				return nil, nil, fmt.Errorf("%c%s is a character class: put it inside a capture group", ch, string(nxt))
			}
			cur.WriteByte(nxt)
			i++
		case strings.ContainsRune(".[]|*+?{}()^$", rune(ch)):
			return nil, nil, fmt.Errorf("%q is not a literal: quantifiers, classes and alternations must be inside a capture group", string(ch))
		default:
			cur.WriteByte(ch)
		}
	}
	literals = append(literals, cur.String())
	return literals, groups, nil
}

func isClassShorthand(c byte) bool {
	switch c {
	case 'd', 'D', 'w', 'W', 's', 'S':
		return true
	}
	return false
}
