package wire

import (
	"regexp"
	"strings"
	"testing"
)

func mustRules(t *testing.T, rules []TargetRule) TargetRules {
	t.Helper()
	r, err := CompileTargetRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// forwardPattern is what the rule's cluster side means for a given target:
// the target's groups replace the cluster regex's groups, whole, and what
// is outside the groups stays regex. The plugins never run it -- they only
// read backwards -- so it serves here as the oracle for the round trip.
func forwardPattern(t *testing.T, rule TargetRule, target string) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile("^(?:" + rule.Target + ")$").FindStringSubmatch(target)
	if m == nil {
		t.Fatalf("target %q does not match %q", target, rule.Target)
	}
	segments, groups := splitRegexGroups(strings.TrimSuffix(strings.TrimPrefix(rule.Cluster, "^"), "$"))
	if len(groups) != len(m)-1 {
		t.Fatalf("cluster %q: %d groups, target %q: %d", rule.Cluster, len(groups), rule.Target, len(m)-1)
	}
	var b strings.Builder
	b.WriteString("^")
	for i, seg := range segments {
		b.WriteString(seg)
		if i < len(groups) {
			b.WriteString("(?:" + regexp.QuoteMeta(m[i+1]) + ")")
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// splitRegexGroups cuts a regex into the segments between its top-level
// capture groups and the groups themselves.
func splitRegexGroups(src string) (segments, groups []string) {
	depth, start := 0, 0
	var cur strings.Builder
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\\':
			if depth == 0 {
				cur.WriteByte(src[i])
				if i+1 < len(src) {
					cur.WriteByte(src[i+1])
				}
			}
			i++
		case '[':
			if j := strings.IndexByte(src[i+1:], ']'); j >= 0 {
				if depth == 0 {
					cur.WriteString(src[i : i+j+2])
				}
				i += j + 1
			}
		case '(':
			if depth == 0 {
				segments = append(segments, cur.String())
				cur.Reset()
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				groups = append(groups, src[start:i])
			}
		default:
			if depth == 0 {
				cur.WriteByte(src[i])
			}
		}
	}
	segments = append(segments, cur.String())
	return segments, groups
}

// The round trip the configuration promises: every service name a rule's
// forward pattern accepts reads back as exactly the target it was built for,
// and nothing else does. Checked over the defaults and a set of shapes that
// each broke an earlier iteration -- literal text around groups, two groups,
// a "(" inside a character class, alternation inside a group.
func TestTargetRules_RoundTrip(t *testing.T) {
	rules := mustRules(t, []TargetRule{
		{Target: `^(\d+)$`, Cluster: `model-\d+-(\d+)$`},
		{Target: `^provider-(\d+)$`, Cluster: `provider-(\d+)$`},
		{Target: `^node-([a-z]+)-(\d+)$`, Cluster: `pool-\d+-([a-z]+)-node-(\d+)`},
		{Target: `^(a|b)-(\d+)$`, Cluster: `svc-(a|b)-(\d+)`},
		{Target: `^x(\d+)$`, Cluster: `svc-[a-z(]+-x(\d+)`},
		{Target: `^abc$`, Cluster: `svc-abc`},
	})
	for i, rule := range []TargetRule{
		{Target: `^(\d+)$`, Cluster: `model-\d+-(\d+)$`},
		{Target: `^provider-(\d+)$`, Cluster: `provider-(\d+)$`},
		{Target: `^node-([a-z]+)-(\d+)$`, Cluster: `pool-\d+-([a-z]+)-node-(\d+)`},
		{Target: `^(a|b)-(\d+)$`, Cluster: `svc-(a|b)-(\d+)`},
		{Target: `^x(\d+)$`, Cluster: `svc-[a-z(]+-x(\d+)`},
		{Target: `^abc$`, Cluster: `svc-abc`},
	} {
		for _, target := range []string{"7", "71", "provider-5", "node-blue-9", "a-4", "x77", "abc"} {
			m := regexp.MustCompile("^(?:" + rule.Target + ")$").FindStringSubmatch(target)
			if m == nil {
				continue // not this rule's target
			}
			fwd := forwardPattern(t, rule, target)
			for _, service := range []string{
				"model-42-" + m[len(m)-1], "model-9-" + m[len(m)-1] + "-x", "pool-3-blue-node-9", "svc-a-4",
				"svc-(abc-x77", "svc-abc", "provider-5", "model-42-7", "pool-3-red-node-9",
			} {
				accepts := fwd.MatchString(service)
				got := rules.TargetOf(service)
				if accepts && got != target {
					t.Errorf("rule %d: forward accepts %q for %q, but TargetOf gives %q", i, service, target, got)
				}
				if !accepts && got == target {
					t.Errorf("rule %d: forward rejects %q for %q, but TargetOf gives %q", i, service, target, got)
				}
			}
		}
	}
}

func TestTargetRules_Defaults(t *testing.T) {
	rules := mustRules(t, nil)
	if len(rules) != len(DefaultTargetPatterns) {
		t.Fatal("nil config did not yield the defaults")
	}
	for service, want := range map[string]string{
		"model-42-7":   "7",
		"provider-5":   "provider-5",
		"model-42-71":  "71",
		"some-svc":     "",
		"model-42-7-x": "",
		"":             "",
	} {
		if got := rules.TargetOf(service); got != want {
			t.Errorf("TargetOf(%q) = %q, want %q", service, got, want)
		}
	}
}

// A target and a service name are matched as a whole whatever the operator
// wrote: an unanchored target regex must not let "7-x" pass for "7".
func TestTargetRules_WholeMatch(t *testing.T) {
	rules := mustRules(t, []TargetRule{{Target: `(\d+)`, Cluster: `model-\d+-(\d+)`}})
	if got := rules.TargetOf("model-42-7-x"); got != "" {
		t.Errorf("TargetOf(model-42-7-x) = %q, want an empty target", got)
	}
	if got := rules.TargetOf("xmodel-42-7"); got != "" {
		t.Errorf("TargetOf(xmodel-42-7) = %q, want an empty target", got)
	}
}

// An ambiguous list: svc-x-9 yields "9", but "9" is claimed by the model rule,
// which cannot match svc-x-9. Refusing to wrap is the safe side.
func TestTargetRules_AmbiguousList(t *testing.T) {
	rules := mustRules(t, []TargetRule{
		{Target: `^(\d+)$`, Cluster: `model-\d+-(\d+)`},
		{Target: `^([a-z0-9]+)$`, Cluster: `svc-[a-z(]+-([a-z0-9]+)`},
	})
	if got := rules.TargetOf("svc-x-9"); got != "" {
		t.Errorf("TargetOf(svc-x-9) = %q, want an empty target", got)
	}
	if got := rules.TargetOf("model-42-7"); got != "7" {
		t.Errorf("TargetOf(model-42-7) = %q, want 7", got)
	}
	// order decides: with the svc rule first, "9" is claimed by it and both
	// directions agree
	ordered := mustRules(t, []TargetRule{
		{Target: `^([a-z0-9]+)$`, Cluster: `svc-[a-z(]+-([a-z0-9]+)`},
		{Target: `^(\d+)$`, Cluster: `model-\d+-(\d+)`},
	})
	if got := ordered.TargetOf("svc-x-9"); got != "9" {
		t.Errorf("ordered: TargetOf(svc-x-9) = %q, want 9", got)
	}
}

func TestCompileTargetRules_Rejected(t *testing.T) {
	tests := map[string]TargetRule{
		"group count differs":     {`^(\d+)$`, `model-(\d+)-(\d+)`},
		"cluster has no group":    {`^(\d+)$`, `model-\d+-7`},
		"the old template syntax": {`^(\d+)$`, `model-(\d+)-$1`},
		"class outside a group":   {`^\d+$`, `model-(\d+)`},
		"quantifier on a group":   {`^(\d+)?$`, `model-(\d+)`},
		"non-capturing group":     {`^(?:a)-(\d+)$`, `x-(\d+)`},
		"named group in target":   {`^(?P<n>\d+)$`, `model-(\d+)`},
		"invalid target regex":    {`^(\d+$`, `model-(\d+)`},
		"invalid cluster regex":   {`^(\d+)$`, `model-(\d+)(`},
		"empty target":            {``, `model-(\d+)`},
		"empty cluster":           {`^(\d+)$`, ``},
		"duplicate target regex":  {`^(\d+)$`, `model-(\d+)`},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			list := []TargetRule{r}
			if name == "duplicate target regex" {
				list = append(list, TargetRule{Target: r.Target, Cluster: `svc-(\d+)`})
			}
			if _, err := CompileTargetRules(list); err == nil {
				t.Errorf("CompileTargetRules(%+v) succeeded, want error", list)
			}
		})
	}
}
