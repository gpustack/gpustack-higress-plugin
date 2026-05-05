package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// helper: compile a MatchRule or fail the test.
func mustHeaderRule(t *testing.T, name, value string) MatchRule {
	t.Helper()
	r := MatchRule{Source: SourceHeader, Name: name, Value: MatchValue(value)}
	if err := r.Compile(); err != nil {
		t.Fatalf("compile MatchRule(%s=%s): %v", name, value, err)
	}
	return r
}

func TestCollectChecksOrderingAndModes(t *testing.T) {
	cfg := &PluginConfig{
		RuleName: "myrule",
		LimitCombinations: []LimitCombination{
			{
				Name:        "query-only",
				Match:       []MatchRule{mustHeaderRule(t, "x-api-key", "k1")},
				QueryLimits: &RateQuota{PerMinute: intPtr(100)},
			},
			{
				Name:        "token-only",
				Match:       []MatchRule{mustHeaderRule(t, "x-api-key", "k1")},
				TokenLimits: &RateQuota{PerMinute: intPtr(200000)},
			},
			{
				Name:        "both",
				Match:       []MatchRule{mustHeaderRule(t, "x-api-key", "k1")},
				QueryLimits: &RateQuota{PerMinute: intPtr(50)},
				TokenLimits: &RateQuota{PerMinute: intPtr(1000)},
			},
			{
				Name:        "miss",
				Match:       []MatchRule{mustHeaderRule(t, "x-api-key", "never-matches")},
				QueryLimits: &RateQuota{PerMinute: intPtr(10)},
			},
		},
	}

	headers := [][2]string{{"x-api-key", "k1"}}
	reqID := "req-xyz"
	entries, pending := collectChecks(cfg, headers, reqID, time.Now())

	// Expected entries: query-only (1 q) + token-only (1 t) + both (1 q + 1 t) = 4
	wantKeys := []string{
		"myrule|query-only|header:x-api-key=k1|q:60s",
		"myrule|token-only|header:x-api-key=k1|t:60s",
		"myrule|both|header:x-api-key=k1|q:60s",
		"myrule|both|header:x-api-key=k1|t:60s",
	}
	if len(entries) != len(wantKeys) {
		t.Fatalf("len(entries)=%d, want %d", len(entries), len(wantKeys))
	}
	for i, w := range wantKeys {
		if entries[i].Key != w {
			t.Errorf("entries[%d].Key=%q, want %q", i, entries[i].Key, w)
		}
	}

	// Walk entries and verify window/quota/mode/member/count/ttl.
	// All combos here use per_minute (60s window), so TTL = window * 2 = 120s.
	wantTuples := []struct {
		window int64
		quota  int
		mode   string
		count  int64
		ttl    int64
	}{
		{60, 100, modeCheckAndAdd, 1, 120},    // query-only
		{60, 200000, modeCheck, 0, 120},        // token-only
		{60, 50, modeCheckAndAdd, 1, 120},      // both-query
		{60, 1000, modeCheck, 0, 120},          // both-token
	}
	for i, w := range wantTuples {
		if entries[i].Window != w.window {
			t.Errorf("entries[%d].Window=%d, want %d", i, entries[i].Window, w.window)
		}
		if entries[i].Quota != w.quota {
			t.Errorf("entries[%d].Quota=%d, want %d", i, entries[i].Quota, w.quota)
		}
		if entries[i].Mode != w.mode {
			t.Errorf("entries[%d].Mode=%q, want %q", i, entries[i].Mode, w.mode)
		}
		if entries[i].Member != reqID {
			t.Errorf("entries[%d].Member=%q, want %q", i, entries[i].Member, reqID)
		}
		if entries[i].Count != w.count {
			t.Errorf("entries[%d].Count=%d, want %d", i, entries[i].Count, w.count)
		}
		if entries[i].TTL != w.ttl {
			t.Errorf("entries[%d].TTL=%d, want %d", i, entries[i].TTL, w.ttl)
		}
	}

	// pending entries: token-only + both-token, in the order they were emitted.
	if len(pending) != 2 {
		t.Fatalf("len(pending)=%d, want 2", len(pending))
	}
	if pending[0].Key != "myrule|token-only|header:x-api-key=k1|t:60s" {
		t.Errorf("pending[0].Key=%q, unexpected", pending[0].Key)
	}
	if pending[1].Key != "myrule|both|header:x-api-key=k1|t:60s" {
		t.Errorf("pending[1].Key=%q, unexpected", pending[1].Key)
	}
	if pending[0].FixedWindow != 60 || pending[0].Quota != 200000 || pending[0].CalendarSpec != nil {
		t.Errorf("pending[0] = %+v", pending[0])
	}
	if pending[1].FixedWindow != 60 || pending[1].Quota != 1000 || pending[1].CalendarSpec != nil {
		t.Errorf("pending[1] = %+v", pending[1])
	}
}

func TestCollectChecksAllCombosMiss(t *testing.T) {
	cfg := &PluginConfig{
		RuleName: "r",
		LimitCombinations: []LimitCombination{
			{
				Name:        "c",
				Match:       []MatchRule{mustHeaderRule(t, "x", "expected")},
				QueryLimits: &RateQuota{PerMinute: intPtr(10)},
			},
		},
	}
	entries, pending := collectChecks(cfg, [][2]string{{"x", "actual"}}, "rid", time.Now())
	if len(entries) != 0 {
		t.Errorf("entries=%v, want empty", entries)
	}
	if len(pending) != 0 {
		t.Errorf("pending=%v, want empty", pending)
	}
}

func TestCollectChecksMultiDimensionKey(t *testing.T) {
	cfg := &PluginConfig{
		RuleName: "rr",
		LimitCombinations: []LimitCombination{
			{
				Name: "c",
				Match: []MatchRule{
					mustHeaderRule(t, "x-api-key", "premium-abc"),
					func() MatchRule {
						r := MatchRule{Source: SourceParam, Name: "model", Value: "gpt-4"}
						if err := r.Compile(); err != nil {
							t.Fatalf("compile param rule: %v", err)
						}
						return r
					}(),
				},
				QueryLimits: &RateQuota{PerHour: intPtr(500)},
			},
		},
	}
	headers := [][2]string{
		{"x-api-key", "premium-abc"},
		{":path", "/chat?model=gpt-4"},
	}
	entries, _ := collectChecks(cfg, headers, "rid", time.Now())
	if len(entries) != 1 {
		t.Fatalf("len(entries)=%d, want 1", len(entries))
	}
	want := "rr|c|header:x-api-key=premium-abc|param:model=gpt-4|q:3600s"
	if entries[0].Key != want {
		t.Errorf("key=%q, want %q", entries[0].Key, want)
	}
}

func TestCollectChecksSkipsQuotaWithoutWindow(t *testing.T) {
	// A RateQuota that is non-nil but has no window should be treated as "not effective".
	cfg := &PluginConfig{
		RuleName: "r",
		LimitCombinations: []LimitCombination{
			{
				Name:        "c",
				Match:       []MatchRule{mustHeaderRule(t, "x", "v")},
				QueryLimits: &RateQuota{}, // empty
			},
		},
	}
	entries, _ := collectChecks(cfg, [][2]string{{"x", "v"}}, "rid", time.Now())
	if len(entries) != 0 {
		t.Errorf("expected no entries for an empty quota, got %v", entries)
	}
}

func TestCollectChecksTokenQuotaCalendar(t *testing.T) {
	cfg := &PluginConfig{
		RuleName: "myrule",
		Timezone: "Asia/Shanghai",
		LimitCombinations: []LimitCombination{
			{
				Name:        "rolling-and-monthly",
				Match:       []MatchRule{mustHeaderRule(t, "x-mse-consumer", "tenant-a")},
				TokenLimits: &RateQuota{PerMinute: intPtr(200000)},
				TokenQuota:  &QuotaSpec{EachMonth: intPtr(1000000)},
			},
			{
				Name:       "yearly-only",
				Match:      []MatchRule{mustHeaderRule(t, "x-mse-consumer", "tenant-a")},
				TokenQuota: &QuotaSpec{EachYear: intPtr(10000000)},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	headers := [][2]string{{"x-mse-consumer", "tenant-a"}}
	entries, pending := collectChecks(cfg, headers, "req-xyz", time.Now())

	// Three entries: rolling token + monthly quota + yearly quota.
	// Top-level Timezone is NOT encoded into the key -- it's a deployment-wide constant.
	wantKeys := []string{
		"myrule|rolling-and-monthly|header:x-mse-consumer=tenant-a|t:60s",
		"myrule|rolling-and-monthly|header:x-mse-consumer=tenant-a|t:each_month",
		"myrule|yearly-only|header:x-mse-consumer=tenant-a|t:each_year",
	}
	if len(entries) != len(wantKeys) {
		t.Fatalf("len(entries)=%d, want %d", len(entries), len(wantKeys))
	}
	for i, w := range wantKeys {
		if entries[i].Key != w {
			t.Errorf("entries[%d].Key=%q, want %q", i, entries[i].Key, w)
		}
	}

	// All token-flavoured checks must be mode=check, count=0.
	for i := range wantKeys {
		if entries[i].Mode != modeCheck {
			t.Errorf("entries[%d].Mode=%q, want %q", i, entries[i].Mode, modeCheck)
		}
		if entries[i].Count != 0 {
			t.Errorf("entries[%d].Count=%d, want 0", i, entries[i].Count)
		}
	}

	// Rolling entry (entries[0]): fixed 60s window, TTL = window * 2 = 120s.
	if entries[0].Window != 60 {
		t.Errorf("rolling token window=%d, want 60", entries[0].Window)
	}
	if entries[0].TTL != 120 {
		t.Errorf("rolling token ttl=%d, want 120", entries[0].TTL)
	}
	// Calendar entries (entries[1], entries[2]): window in [1, 366d],
	// TTL must be (period_end - now + grace).
	for _, idx := range []int{1, 2} {
		if entries[idx].Window < 1 || entries[idx].Window > 366*86400 {
			t.Errorf("entries[%d].Window=%d outside expected range [1, 366d]", idx, entries[idx].Window)
		}
		// TTL >= grace floor; TTL <= 366d + grace (year is the largest period).
		if entries[idx].TTL <= 60 || entries[idx].TTL > 366*86400+ttlGraceSeconds+60 {
			t.Errorf("entries[%d].TTL=%d outside expected range", idx, entries[idx].TTL)
		}
	}

	// pending entries: one FixedWindow (60s) + two CalendarSpec entries, in emit order.
	if len(pending) != 3 {
		t.Fatalf("len(pending)=%d, want 3", len(pending))
	}
	if pending[0].FixedWindow != 60 || pending[0].CalendarSpec != nil {
		t.Errorf("pending[0] should be fixed-window: %+v", pending[0])
	}
	if pending[1].CalendarSpec == nil || pending[1].FixedWindow != 0 {
		t.Errorf("pending[1] should be calendar: %+v", pending[1])
	}
	if pending[2].CalendarSpec == nil || pending[2].FixedWindow != 0 {
		t.Errorf("pending[2] should be calendar: %+v", pending[2])
	}
	if !strings.HasSuffix(pending[1].Key, "|t:each_month") {
		t.Errorf("pending[1].Key=%q, want suffix |t:each_month", pending[1].Key)
	}
	if !strings.HasSuffix(pending[2].Key, "|t:each_year") {
		t.Errorf("pending[2].Key=%q, want suffix |t:each_year", pending[2].Key)
	}

	// Sanity: the calendar specs in pending should be bound to the deployment-wide SH location.
	if got := pending[1].CalendarSpec.location.String(); got != "Asia/Shanghai" {
		t.Errorf("pending[1] calendar location=%q, want Asia/Shanghai", got)
	}
}

func TestRollingTTL(t *testing.T) {
	cases := []struct {
		window int64
		want   int64
	}{
		{1, 2},
		{60, 120},
		{3600, 7200},
		{86400, 172800},
	}
	for _, c := range cases {
		if got := rollingTTL(c.window); got != c.want {
			t.Errorf("rollingTTL(%d) = %d, want %d", c.window, got, c.want)
		}
	}
}

func TestCalendarTTLCoversFullPeriod(t *testing.T) {
	// Regression test for the bug where calendar quotas used `window * 2`
	// for TTL, producing a near-zero TTL at period start (e.g. 2s at 00:00:01
	// for an each_month combo) and causing the key to be evicted before the
	// next request, prematurely resetting the quota. The fix sets TTL to
	// (period_end - now + grace), so even at the very first second of a
	// period the TTL covers nearly the whole period.
	utc := time.UTC
	monthly := mustCompileQuota(t, QuotaSpec{EachMonth: intPtr(1000000)}, utc)
	yearly := mustCompileQuota(t, QuotaSpec{EachYear: intPtr(1)}, utc)
	daily := mustCompileQuota(t, QuotaSpec{EachDay: intPtr(1)}, utc)

	cases := []struct {
		name    string
		spec    *QuotaSpec
		now     time.Time
		wantMin int64 // ttl must be at least this many seconds
		wantMax int64 // and no more than this
	}{
		{
			"monthly at first second of month covers ~30 days",
			&monthly,
			time.Date(2026, 4, 1, 0, 0, 1, 0, utc),
			// Apr has 30 days, so periodEnd-now ≈ 30d. With grace=300, range:
			29*86400 + ttlGraceSeconds, 31*86400 + ttlGraceSeconds + 1,
		},
		{
			"monthly mid-month covers remaining ~half month",
			&monthly,
			time.Date(2026, 4, 15, 0, 0, 0, 0, utc),
			14*86400 + ttlGraceSeconds, 17*86400 + ttlGraceSeconds + 1,
		},
		{
			"monthly last second of month is grace-only",
			&monthly,
			time.Date(2026, 4, 30, 23, 59, 59, 0, utc),
			ttlGraceSeconds, ttlGraceSeconds + 5,
		},
		{
			"yearly at first second of year covers ~365 days",
			&yearly,
			time.Date(2026, 1, 1, 0, 0, 1, 0, utc),
			364*86400 + ttlGraceSeconds, 366*86400 + ttlGraceSeconds + 1,
		},
		{
			"daily at first second of day covers ~24 hours",
			&daily,
			time.Date(2026, 4, 15, 0, 0, 1, 0, utc),
			86400 - 5 + ttlGraceSeconds, 86400 + ttlGraceSeconds + 1,
		},
		{
			"december → january rollover",
			&yearly,
			time.Date(2026, 12, 31, 23, 59, 0, 0, utc),
			60 + ttlGraceSeconds - 5, 60 + ttlGraceSeconds + 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := calendarTTL(c.spec, c.now)
			if got < c.wantMin || got > c.wantMax {
				t.Errorf("calendarTTL = %d, want in [%d, %d]", got, c.wantMin, c.wantMax)
			}
		})
	}
}

func TestTokenPendingAddTTLSeconds(t *testing.T) {
	utc := time.UTC

	t.Run("rolling uses window * 2", func(t *testing.T) {
		p := tokenPendingAdd{FixedWindow: 60}
		got := p.ttlSeconds(time.Date(2026, 4, 15, 12, 0, 0, 0, utc))
		if got != 120 {
			t.Errorf("ttlSeconds=%d, want 120", got)
		}
	})

	t.Run("calendar shrinks toward period end", func(t *testing.T) {
		spec := mustCompileQuota(t, QuotaSpec{EachMonth: intPtr(1)}, utc)
		p := tokenPendingAdd{CalendarSpec: &spec}

		earlyTTL := p.ttlSeconds(time.Date(2026, 4, 1, 0, 0, 1, 0, utc))
		lateTTL := p.ttlSeconds(time.Date(2026, 4, 30, 23, 59, 0, 0, utc))
		if earlyTTL <= lateTTL {
			t.Errorf("earlyTTL=%d should be > lateTTL=%d", earlyTTL, lateTTL)
		}
	})
}

func TestTokenPendingAddWindowSeconds(t *testing.T) {
	utc, _ := time.LoadLocation("UTC")

	t.Run("fixed window", func(t *testing.T) {
		p := tokenPendingAdd{FixedWindow: 60}
		got := p.windowSeconds(time.Date(2026, 4, 15, 12, 0, 0, 0, utc))
		if got != 60 {
			t.Errorf("windowSeconds=%d, want 60", got)
		}
	})

	t.Run("calendar spec recomputes from now", func(t *testing.T) {
		spec := mustCompileQuota(t, QuotaSpec{EachMonth: intPtr(1000)}, utc)
		p := tokenPendingAdd{CalendarSpec: &spec}
		// Mid-month: should be roughly two weeks in seconds.
		got := p.windowSeconds(time.Date(2026, 4, 15, 12, 0, 0, 0, utc))
		want := int64(14*86400 + 12*3600)
		if got != want {
			t.Errorf("windowSeconds=%d, want %d", got, want)
		}
	})

	t.Run("calendar spec at period boundary clamps", func(t *testing.T) {
		spec := mustCompileQuota(t, QuotaSpec{EachMonth: intPtr(1000)}, utc)
		p := tokenPendingAdd{CalendarSpec: &spec}
		got := p.windowSeconds(time.Date(2026, 4, 1, 0, 0, 0, 0, utc))
		if got != 1 {
			t.Errorf("windowSeconds=%d, want 1 (clamped)", got)
		}
	})
}

// validatedGlobal builds a PluginConfig with the given combos and runs Validate
// to compile match rules / quotas. Used as the "global" input to
// parseOverrideRuleConfig in aggregation tests.
func validatedGlobal(t *testing.T, ruleName, tz string, suffix []string, combos []LimitCombination) PluginConfig {
	t.Helper()
	g := PluginConfig{
		RuleName:           ruleName,
		Timezone:           tz,
		EnableOnPathSuffix: suffix,
		LimitCombinations:  combos,
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("global Validate: %v", err)
	}
	return g
}

func makeConsumerCombo(name string, monthlyLimit int) LimitCombination {
	return LimitCombination{
		Name:       name,
		Match:      []MatchRule{{Source: SourceConsumer, Value: "*"}},
		TokenQuota: &QuotaSpec{EachMonth: intPtr(monthlyLimit)},
	}
}

func TestParseOverrideRuleConfigAggregation(t *testing.T) {
	global := validatedGlobal(t, "ai-budget", "UTC", []string{"/v1/chat/completions"},
		[]LimitCombination{makeConsumerCombo("per-consumer", 5000000)})

	overrideJSON := `{
        "limit_combinations": [
            {
                "name": "model-gpt-4",
                "match": [{"source": "consumer", "value": "*"}],
                "token_quota": {"each_month": 1000000}
            }
        ]
    }`

	var config PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(overrideJSON), global, &config); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if len(config.LimitCombinations) != 2 {
		t.Fatalf("len(combos)=%d, want 2", len(config.LimitCombinations))
	}
	if config.LimitCombinations[0].Name != "per-consumer" {
		t.Errorf("[0].Name=%q, want per-consumer (global combo first)", config.LimitCombinations[0].Name)
	}
	if config.LimitCombinations[1].Name != "model-gpt-4" {
		t.Errorf("[1].Name=%q, want model-gpt-4 (override combo last)", config.LimitCombinations[1].Name)
	}

	// Aliasing safety: global must not have been mutated.
	if len(global.LimitCombinations) != 1 || global.LimitCombinations[0].Name != "per-consumer" {
		t.Errorf("global LimitCombinations mutated: %+v", global.LimitCombinations)
	}
}

func TestParseOverrideRuleConfigInheritsCombosWhenAbsent(t *testing.T) {
	global := validatedGlobal(t, "ai-budget", "UTC", []string{"/v1/chat/completions"},
		[]LimitCombination{makeConsumerCombo("per-consumer", 5000000)})

	// Override only changes a scalar field, no limit_combinations.
	overrideJSON := `{"rejected_msg": "Try later"}`

	var config PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(overrideJSON), global, &config); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}

	if config.RejectedMsg != "Try later" {
		t.Errorf("RejectedMsg=%q, want %q", config.RejectedMsg, "Try later")
	}
	if len(config.LimitCombinations) != 1 || config.LimitCombinations[0].Name != "per-consumer" {
		t.Errorf("expected inherited global combos, got %+v", config.LimitCombinations)
	}
}

func TestParseOverrideRuleConfigDuplicateNameRejected(t *testing.T) {
	global := validatedGlobal(t, "ai-budget", "UTC", []string{"/v1/chat/completions"},
		[]LimitCombination{makeConsumerCombo("per-consumer", 5000000)})

	overrideJSON := `{
        "limit_combinations": [
            {
                "name": "per-consumer",
                "match": [{"source": "consumer", "value": "*"}],
                "token_quota": {"each_month": 100000}
            }
        ]
    }`

	var config PluginConfig
	err := parseOverrideRuleConfig(gjson.Parse(overrideJSON), global, &config)
	if err == nil {
		t.Fatal("expected duplicate combo name error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q, want substring \"duplicate\"", err.Error())
	}
}

func TestParseOverrideRuleConfigPathFilterAliasingSafe(t *testing.T) {
	global := validatedGlobal(t, "ai-budget", "UTC", []string{"/foo"},
		[]LimitCombination{makeConsumerCombo("per-consumer", 5000000)})
	originalSuffix := append([]string(nil), global.EnableOnPathSuffix...)

	// Override redeclares enable_on_path_suffix.
	overrideJSON := `{"enable_on_path_suffix": ["/bar"]}`

	var config PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(overrideJSON), global, &config); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if !slices.Equal(config.EnableOnPathSuffix, []string{"/bar"}) {
		t.Errorf("config.EnableOnPathSuffix=%v, want [/bar]", config.EnableOnPathSuffix)
	}
	// global must still see the original ["/foo"] -- no aliasing corruption.
	if !slices.Equal(global.EnableOnPathSuffix, originalSuffix) {
		t.Errorf("global.EnableOnPathSuffix=%v, want %v (corrupted by aliasing)",
			global.EnableOnPathSuffix, originalSuffix)
	}
}

func TestParseOverrideRuleConfigInheritsPathFiltersWhenAbsent(t *testing.T) {
	global := validatedGlobal(t, "ai-budget", "UTC", []string{"/foo", "/bar"},
		[]LimitCombination{makeConsumerCombo("per-consumer", 5000000)})

	// Override does not redeclare enable_on_path_suffix.
	overrideJSON := `{"rejected_code": 503}`

	var config PluginConfig
	if err := parseOverrideRuleConfig(gjson.Parse(overrideJSON), global, &config); err != nil {
		t.Fatalf("parseOverrideRuleConfig: %v", err)
	}
	if !slices.Equal(config.EnableOnPathSuffix, []string{"/foo", "/bar"}) {
		t.Errorf("inherited suffix=%v, want [/foo /bar]", config.EnableOnPathSuffix)
	}
}

func TestCollectChecksRegexpCaptureSharedBucket(t *testing.T) {
	// regexp_capture lets the API form (<ak>.gpustack-1) and the UI form
	// (gpustack-1) produce the same key fragment, so both end up in the same
	// Redis bucket.
	cfg := &PluginConfig{
		RuleName: "ai-budget",
		Timezone: "UTC",
		LimitCombinations: []LimitCombination{
			{
				Name: "per-user",
				Match: []MatchRule{
					{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-.+)$`},
				},
				TokenQuota: &QuotaSpec{EachMonth: intPtr(1000000)},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	apiEntries, _ := collectChecks(cfg, [][2]string{{"x-mse-consumer", "ak-x.gpustack-1"}}, "rid-1", time.Now())
	uiEntries, _ := collectChecks(cfg, [][2]string{{"x-mse-consumer", "gpustack-1"}}, "rid-2", time.Now())

	if len(apiEntries) != 1 || len(uiEntries) != 1 {
		t.Fatalf("len(apiEntries)=%d len(uiEntries)=%d, want 1 each", len(apiEntries), len(uiEntries))
	}
	if apiEntries[0].Key != uiEntries[0].Key {
		t.Errorf("expected shared bucket: api=%v ui=%v", apiEntries[0].Key, uiEntries[0].Key)
	}
	want := "ai-budget|per-user|consumer=gpustack-1|t:each_month"
	if apiEntries[0].Key != want {
		t.Errorf("key=%q, want %q", apiEntries[0].Key, want)
	}
}

func TestBuildRejectedHeaders(t *testing.T) {
	t.Run("hidden by default", func(t *testing.T) {
		got := buildRejectedHeaders(PluginConfig{}, "ai-budget|per-consumer|consumer=alice|t:each_month")
		if got != nil {
			t.Errorf("expected nil headers when ShowLimitQuotaHeader is false, got %v", got)
		}
	})

	t.Run("emitted when explicitly enabled", func(t *testing.T) {
		key := "ai-budget|per-consumer|consumer=alice|t:each_month"
		got := buildRejectedHeaders(PluginConfig{ShowLimitQuotaHeader: true}, key)
		if len(got) != 1 {
			t.Fatalf("len(headers)=%d, want 1", len(got))
		}
		if got[0][0] != "x-ratelimit-limited-key" {
			t.Errorf("header name=%q, want x-ratelimit-limited-key", got[0][0])
		}
		if got[0][1] != key {
			t.Errorf("header value=%q, want %q", got[0][1], key)
		}
	})

	t.Run("emits empty value when limitedKey is empty", func(t *testing.T) {
		got := buildRejectedHeaders(PluginConfig{ShowLimitQuotaHeader: true}, "")
		if len(got) != 1 || got[0][1] != "" {
			t.Errorf("expected single header with empty value, got %v", got)
		}
	})
}

func TestMatchPathFilters(t *testing.T) {
	defaultCfg := &PluginConfig{
		EnableOnPathSuffix: []string{"/completions", "/messages"},
		EnableOnPathPrefix: []string{"/model/proxy"},
	}
	cases := []struct {
		name string
		cfg  *PluginConfig
		path string
		want bool
	}{
		{"suffix hit", defaultCfg, "/v1/chat/completions", true},
		{"suffix hit second", defaultCfg, "/v1/messages", true},
		{"prefix hit", defaultCfg, "/model/proxy/foo", true},
		{"query stripped before suffix", defaultCfg, "/v1/chat/completions?stream=true", true},
		{"unrelated path miss", defaultCfg, "/v1/unrelated", false},
		{"wildcard suffix", &PluginConfig{EnableOnPathSuffix: []string{"*"}}, "/anything", true},
		{"empty prefix matches all", &PluginConfig{EnableOnPathPrefix: []string{""}}, "/anything", true},
		{"both lists empty", &PluginConfig{}, "/anything", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchPathFilters(c.cfg, c.path); got != c.want {
				t.Errorf("matchPathFilters(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

func TestSanitizeMetricLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "none"},
		{"alice", "alice"},
		{"consumer=alice", "consumer_alice"},
		// '.' is preserved on purpose: Higress's bootstrap stats_tags regex
		// for ai_route / ai_cluster / ai_model / ai_consumer backtrack
		// correctly across dots and surface them in the Prometheus label
		// value (e.g. ai_model="qwen3-0.6b"). See metrics.go.
		{"header:x-higress-llm-model=qwen3-0.6b", "header_x-higress-llm-model_qwen3-0.6b"},
		{"a|b|c", "a_b_c"},
		{"192.168.1.1", "192.168.1.1"},
		{"ai-route-route-1.internal", "ai-route-route-1.internal"},
		{"outbound|80||model-1.static", "outbound_80__model-1.static"},
		{"foo-bar_baz", "foo-bar_baz"}, // hyphens and underscores preserved
	}
	for _, c := range cases {
		if got := sanitizeMetricLabel(c.in); got != c.want {
			t.Errorf("sanitizeMetricLabel(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatStatName(t *testing.T) {
	// Bare label-only assembly: each (key, value) pair becomes a `key.value`
	// segment joined with '.'.
	got := formatStatName(
		[2]string{"route", "ai-route-1.internal"},
		[2]string{"upstream", "outbound|80||model-1.static"},
		[2]string{"metric", metricNameRequestTotal},
		[2]string{"result", "passed"},
	)
	want := "route.ai-route-1.internal.upstream.outbound_80__model-1.static.metric." + metricNameRequestTotal + ".result.passed"
	if got != want {
		t.Errorf("formatStatName=%q, want %q", got, want)
	}

	// No labels: empty string (caller must include at least the metric. slot).
	if got := formatStatName(); got != "" {
		t.Errorf("formatStatName empty=%q, want empty", got)
	}

	// Bucket label sanitisation flows through. '.' is preserved (see metrics.go).
	bucketStat := formatStatName(
		[2]string{"bucket", "consumer=alice|header:x-higress-llm-model=qwen3-0.6b"},
	)
	if !strings.Contains(bucketStat, "bucket.consumer_alice_header_x-higress-llm-model_qwen3-0.6b") {
		t.Errorf("bucket sanitisation not applied: %q", bucketStat)
	}
}

func TestAILabelsStatName(t *testing.T) {
	// statName must produce the exact prefix layout that Higress's existing
	// stats_tags regex extractors expect (route.X.upstream.Y.model.Z...).
	ai := aiLabels{
		Route:    "ai-route-1",
		Cluster:  "outbound|80||model-1.static",
		Model:    "qwen3-0.6b",
		Consumer: "alice",
	}
	got := ai.statName(metricNameRejectedTotal,
		[2]string{"rule", "r1"},
		[2]string{"combo", "c1"},
		[2]string{"kind", metricKindQuery},
		[2]string{"bucket", "header:x-higress-llm-model=qwen3-0.6b"},
	)
	wantPrefix := "route.ai-route-1.upstream.outbound_80__model-1.static.model.qwen3-0.6b.consumer.alice.metric." + metricNameRejectedTotal
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("statName missing AI-aligned prefix:\n got=%q\nwant prefix=%q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, ".bucket.header_x-higress-llm-model_qwen3-0.6b") {
		t.Errorf("statName missing sanitised bucket suffix:\n got=%q", got)
	}

	// Empty slots default to "none" so the stat name is always well-formed.
	empty := aiLabels{}.statName(metricNameRequestTotal, [2]string{"result", "passed"})
	if !strings.HasPrefix(empty, "route.none.upstream.none.model.none.consumer.none.metric.") {
		t.Errorf("empty AI labels did not collapse to 'none': %q", empty)
	}
}

func TestCollectChecksMetricLabels(t *testing.T) {
	cfg := &PluginConfig{
		RuleName: "myrule",
		Timezone: "UTC",
		LimitCombinations: []LimitCombination{
			{
				Name: "all-three",
				Match: []MatchRule{
					mustHeaderRule(t, "x-mse-consumer", "alice"),
					mustHeaderRule(t, "x-higress-llm-model", "qwen3-0.6b"),
				},
				QueryLimits: &RateQuota{PerMinute: intPtr(10)},
				TokenLimits: &RateQuota{PerMinute: intPtr(1000)},
				TokenQuota:  &QuotaSpec{EachMonth: intPtr(60000)},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	headers := [][2]string{
		{"x-mse-consumer", "alice"},
		{"x-higress-llm-model", "qwen3-0.6b"},
	}
	entries, pending := collectChecks(cfg, headers, "rid", time.Now())
	if len(entries) != 3 {
		t.Fatalf("len(entries)=%d, want 3", len(entries))
	}

	wantBucket := "header:x-mse-consumer=alice|header:x-higress-llm-model=qwen3-0.6b"
	wantKinds := []string{metricKindQuery, metricKindTokenRolling, metricKindTokenCalendar}
	for i, e := range entries {
		if e.Combo != "all-three" {
			t.Errorf("entries[%d].Combo=%q, want %q", i, e.Combo, "all-three")
		}
		if e.Kind != wantKinds[i] {
			t.Errorf("entries[%d].Kind=%q, want %q", i, e.Kind, wantKinds[i])
		}
		if e.Bucket != wantBucket {
			t.Errorf("entries[%d].Bucket=%q, want %q", i, e.Bucket, wantBucket)
		}
	}

	// pending entries (token_rolling + token_calendar) must mirror the labels.
	if len(pending) != 2 {
		t.Fatalf("len(pending)=%d, want 2", len(pending))
	}
	if pending[0].Kind != metricKindTokenRolling || pending[0].Combo != "all-three" || pending[0].Bucket != wantBucket {
		t.Errorf("pending[0] labels = %+v", pending[0])
	}
	if pending[1].Kind != metricKindTokenCalendar || pending[1].Combo != "all-three" || pending[1].Bucket != wantBucket {
		t.Errorf("pending[1] labels = %+v", pending[1])
	}
}

