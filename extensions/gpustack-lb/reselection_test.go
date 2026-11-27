package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

// A route of two main targets (a, b) and two fallback targets (f, g), the
// shape every budget case below is stated against.
func budgetPool() []candidateSpec {
	return []candidateSpec{
		{Candidate: Candidate{Cluster: "outbound|80||a.ns.svc"}},
		{Candidate: Candidate{Cluster: "outbound|80||b.ns.svc"}},
		{Candidate: Candidate{Cluster: "outbound|80||f.ns.svc", Fallback: true}},
		{Candidate: Candidate{Cluster: "outbound|80||g.ns.svc", Fallback: true}},
	}
}

func clusters(cands []candidateSpec) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Cluster)
	}
	return out
}

func equal(a, b []string) bool {
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

// The budget counts re-selections, not attempts: a tier entered N times has
// spent N-1, and it closes when that reaches its budget. The first attempt
// is always allowed, whatever the budget.
func TestReselectionBudgetCountsReselectionsNotAttempts(t *testing.T) {
	pool := budgetPool()
	tests := []struct {
		name   string
		tried  []string
		max    int64
		wantOn bool
	}{
		{"nothing tried: open under a zero budget", nil, 0, true},
		{"one attempt, budget 0: no re-selection allowed", []string{"a.ns.svc"}, 0, false},
		{"one attempt, budget 1: one re-selection left", []string{"a.ns.svc"}, 1, true},
		{"two attempts, budget 1: spent", []string{"a.ns.svc", "b.ns.svc"}, 1, false},
		{"default budget (0): closed after the first attempt", []string{"a.ns.svc"}, defaultMaxReselections, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reselectionBudgetFor(pool, tt.tried, Config{maxReselections: tt.max}).main
			if got != tt.wantOn {
				t.Errorf("main open = %v, want %v", got, tt.wantOn)
			}
		})
	}
}

// The two tiers are budgeted independently: what the main tier spent says
// nothing about the fallback tier, and vice versa.
func TestReselectionBudgetTiersAreIndependent(t *testing.T) {
	pool := budgetPool()
	tests := []struct {
		name                  string
		tried                 []string
		max, fallbackMax      int64
		wantMain, wantFallbck bool
	}{
		{
			"main exhausted, fallback never entered: fallback open",
			[]string{"a.ns.svc", "b.ns.svc"}, 1, 0, false, true,
		},
		{
			"fallback budget 0 (default): closed after its first attempt",
			[]string{"a.ns.svc", "b.ns.svc", "f.ns.svc"}, 1, 0, false, false,
		},
		{
			"fallback budget 1: one more fallback target allowed",
			[]string{"a.ns.svc", "b.ns.svc", "f.ns.svc"}, 1, 1, false, true,
		},
		{
			"fallback budget 1, both fallback targets tried: closed",
			[]string{"a.ns.svc", "b.ns.svc", "f.ns.svc", "g.ns.svc"}, 1, 1, false, false,
		},
		{
			"main budget untouched by fallback attempts",
			[]string{"a.ns.svc", "f.ns.svc", "g.ns.svc"}, 1, 0, true, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reselectionBudgetFor(pool, tt.tried, Config{
				maxReselections:         tt.max,
				fallbackMaxReselections: tt.fallbackMax,
			})
			if got.main != tt.wantMain || got.fallback != tt.wantFallbck {
				t.Errorf("budget = main %v / fallback %v, want main %v / fallback %v",
					got.main, got.fallback, tt.wantMain, tt.wantFallbck)
			}
		})
	}
}

// The accumulator records the service name; the legacy path (no accumulator
// header survived) passes the full cluster name. Both must count as an
// attempt, for the same reason excludeClusters accepts both: counting only
// one spelling would report a tier as never entered and reopen it.
func TestReselectionBudgetAcceptsBothSpellings(t *testing.T) {
	pool := budgetPool()
	for _, tried := range [][]string{
		{"a.ns.svc", "b.ns.svc"},
		{"outbound|80||a.ns.svc", "outbound|80||b.ns.svc"},
	} {
		got := reselectionBudgetFor(pool, tried, Config{maxReselections: 1})
		if got.main {
			t.Errorf("tried %v: main tier still open, want closed", tried)
		}
	}
	// A tried cluster that is no longer a candidate belongs to no tier.
	got := reselectionBudgetFor(pool, []string{"gone.ns.svc"}, Config{maxReselections: 0})
	if !got.main {
		t.Errorf("a stale tried entry closed the main tier: %+v", got)
	}
}

func TestApplyReselectionBudget(t *testing.T) {
	pool := budgetPool()
	tests := []struct {
		name   string
		budget reselectionBudget
		want   []string
	}{
		{"both open: the pool itself", reselectionBudget{true, true}, clusters(pool)},
		{
			"main closed: fallback tier only",
			reselectionBudget{false, true},
			[]string{"outbound|80||f.ns.svc", "outbound|80||g.ns.svc"},
		},
		{
			"fallback closed: main tier only",
			reselectionBudget{true, false},
			[]string{"outbound|80||a.ns.svc", "outbound|80||b.ns.svc"},
		},
		{"both closed: nothing to publish", reselectionBudget{false, false}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusters(applyReselectionBudget(pool, tt.budget))
			if !equal(got, tt.want) {
				t.Errorf("pool = %v, want %v", got, tt.want)
			}
		})
	}
	// The common pass must not allocate a copy of the route's pool.
	if got := applyReselectionBudget(pool, reselectionBudget{true, true}); &got[0] != &pool[0] {
		t.Errorf("both tiers open: pool was copied")
	}
}

func TestParseReselectionBudgets(t *testing.T) {
	tests := []struct {
		name                  string
		raw                   string
		wantMax, wantFallback int64
	}{
		{"unset: neither tier re-selects", `{}`, 0, 0},
		{"explicit zero keeps the main tier from re-selecting", `{"maxReselections":0}`, 0, defaultFallbackMaxReselections},
		{"fallback opts in", `{"fallbackMaxReselections":3}`, 0, 3},
		{"integral float accepted", `{"maxReselections":2.0}`, 2, defaultFallbackMaxReselections},
		{"null is unset", `{"maxReselections":null}`, defaultMaxReselections, defaultFallbackMaxReselections},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parse(t, tt.raw)
			if c.maxReselections != tt.wantMax || c.fallbackMaxReselections != tt.wantFallback {
				t.Errorf("budgets = %d / %d, want %d / %d",
					c.maxReselections, c.fallbackMaxReselections, tt.wantMax, tt.wantFallback)
			}
		})
	}
}

// 0 is a configured value here, so the truncating shortcuts are wrong in
// both directions: a negative would reopen the tier, a fraction would read
// as a whole number.
func TestParseReselectionBudgetsRejectsBadValues(t *testing.T) {
	for _, raw := range []string{
		`{"maxReselections":-1}`,
		`{"fallbackMaxReselections":-1}`,
		`{"maxReselections":1.5}`,
		`{"fallbackMaxReselections":"3"}`,
	} {
		var c Config
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("parseConfig(%s) accepted, want an error", raw)
		}
	}
}

// Deployment-level, inherited on presence: a rule that omits the key takes
// the global's -- including an explicit 0, which inheritInt64 would drop.
func TestOverrideInheritsReselectionBudgets(t *testing.T) {
	global := parse(t, `{"mode":"context","maxReselections":4,"fallbackMaxReselections":2}`)
	tests := []struct {
		name                  string
		rule                  string
		wantMax, wantFallback int64
	}{
		{"rule silent: inherits", `{"candidates":[{"cluster":"a"}]}`, 4, 2},
		{"rule overrides one key, inherits the other", `{"candidates":[{"cluster":"a"}],"maxReselections":9}`, 9, 2},
		{"rule sets an explicit zero", `{"candidates":[{"cluster":"a"}],"fallbackMaxReselections":0}`, 4, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := parseOverrideConfig(gjson.Parse(tt.rule), global, &c); err != nil {
				t.Fatalf("parseOverrideConfig: %v", err)
			}
			if c.maxReselections != tt.wantMax || c.fallbackMaxReselections != tt.wantFallback {
				t.Errorf("budgets = %d / %d, want %d / %d",
					c.maxReselections, c.fallbackMaxReselections, tt.wantMax, tt.wantFallback)
			}
		})
	}

	// A global that sets 0 is inherited as 0: not re-selecting is a choice.
	zero := parse(t, `{"mode":"context","maxReselections":0}`)
	var c Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), zero, &c); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if c.maxReselections != 0 {
		t.Errorf("global maxReselections 0 became %d on the rule", c.maxReselections)
	}

	// A zero-valued global (its parse failed) leaves the rule's defaults.
	var d Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), Config{}, &d); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if d.maxReselections != defaultMaxReselections || d.fallbackMaxReselections != defaultFallbackMaxReselections {
		t.Errorf("zero global: budgets = %d / %d, want the defaults", d.maxReselections, d.fallbackMaxReselections)
	}
}
