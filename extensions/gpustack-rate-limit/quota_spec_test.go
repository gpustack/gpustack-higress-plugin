package main

import (
	"testing"
	"time"
)

func TestQuotaSpecCompile(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name      string
		spec      QuotaSpec
		loc       *time.Location
		wantErr   bool
		wantKinds []PeriodKind
	}{
		{"each_day ok", QuotaSpec{EachDay: intPtr(1000)}, utc, false, []PeriodKind{PeriodEachDay}},
		{"each_month ok", QuotaSpec{EachMonth: intPtr(1000000)}, utc, false, []PeriodKind{PeriodEachMonth}},
		{"each_year ok", QuotaSpec{EachYear: intPtr(10000000)}, utc, false, []PeriodKind{PeriodEachYear}},
		{
			"day + month coexist",
			QuotaSpec{EachDay: intPtr(1000), EachMonth: intPtr(1000000)},
			utc, false,
			[]PeriodKind{PeriodEachDay, PeriodEachMonth},
		},
		{
			"all three coexist in fixed order",
			QuotaSpec{EachDay: intPtr(1), EachMonth: intPtr(2), EachYear: intPtr(3)},
			utc, false,
			[]PeriodKind{PeriodEachDay, PeriodEachMonth, PeriodEachYear},
		},
		{"none set", QuotaSpec{}, utc, true, nil},
		{"zero limit on solo period", QuotaSpec{EachMonth: intPtr(0)}, utc, true, nil},
		{"negative limit on solo period", QuotaSpec{EachMonth: intPtr(-1)}, utc, true, nil},
		{
			"zero limit invalid even alongside valid period",
			QuotaSpec{EachDay: intPtr(1), EachMonth: intPtr(0)},
			utc, true, nil,
		},
		{"nil location", QuotaSpec{EachMonth: intPtr(1)}, nil, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.spec
			err := s.Compile(c.loc)
			if (err != nil) != c.wantErr {
				t.Fatalf("Compile() err=%v, wantErr=%v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if len(s.periods) != len(c.wantKinds) {
				t.Fatalf("len(periods)=%d, want %d", len(s.periods), len(c.wantKinds))
			}
			for i, want := range c.wantKinds {
				if s.periods[i].kind != want {
					t.Errorf("periods[%d].kind=%q, want %q", i, s.periods[i].kind, want)
				}
				if s.periods[i].location != c.loc {
					t.Errorf("periods[%d].location=%v, want %v", i, s.periods[i].location, c.loc)
				}
			}
		})
	}
}

func TestQuotaSpecCompileIsIdempotent(t *testing.T) {
	// Compile may run more than once on the same spec when a global config
	// is reused as the basis for several rule overrides; calling it twice
	// must not duplicate periods.
	spec := QuotaSpec{EachDay: intPtr(1), EachMonth: intPtr(2)}
	if err := spec.Compile(time.UTC); err != nil {
		t.Fatalf("first Compile: %v", err)
	}
	if err := spec.Compile(time.UTC); err != nil {
		t.Fatalf("second Compile: %v", err)
	}
	if got := len(spec.periods); got != 2 {
		t.Errorf("len(periods)=%d after two Compiles, want 2", got)
	}
}

func TestQuotaSpecPeriodsBeforeCompile(t *testing.T) {
	spec := QuotaSpec{EachDay: intPtr(1)}
	if got := spec.Periods(); got != nil {
		t.Errorf("Periods() before Compile = %#v, want nil", got)
	}
}

func TestCalendarPeriodGetWindowAndQuota(t *testing.T) {
	utc := time.UTC
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	cases := []struct {
		name      string
		spec      QuotaSpec
		loc       *time.Location
		now       time.Time
		wantWin   int64
		wantQuota int
	}{
		{
			"each_day mid-day UTC",
			QuotaSpec{EachDay: intPtr(1000)}, utc,
			time.Date(2026, 4, 15, 12, 34, 56, 0, utc),
			12*3600 + 34*60 + 56,
			1000,
		},
		{
			"each_day at midnight UTC clamps to 1s",
			QuotaSpec{EachDay: intPtr(1000)}, utc,
			time.Date(2026, 4, 15, 0, 0, 0, 0, utc),
			1,
			1000,
		},
		{
			"each_month mid-month UTC",
			QuotaSpec{EachMonth: intPtr(1000000)}, utc,
			time.Date(2026, 4, 15, 12, 0, 0, 0, utc),
			14*86400 + 12*3600,
			1000000,
		},
		{
			"each_month first second UTC clamps to 1s",
			QuotaSpec{EachMonth: intPtr(1000000)}, utc,
			time.Date(2026, 4, 1, 0, 0, 0, 0, utc),
			1,
			1000000,
		},
		{
			"each_year jan 2 UTC",
			QuotaSpec{EachYear: intPtr(1)}, utc,
			time.Date(2026, 1, 2, 0, 0, 0, 0, utc),
			86400,
			1,
		},
		{
			"each_month with Asia/Shanghai location shifts boundary",
			// SH period start = Apr 1 00:00 SH = Mar 31 16:00 UTC
			// diff = (Apr 1 00:30 UTC) - (Mar 31 16:00 UTC) = 8h 30m
			QuotaSpec{EachMonth: intPtr(100)}, sh,
			time.Date(2026, 4, 1, 0, 30, 0, 0, utc),
			8*3600 + 30*60,
			100,
		},
		{
			"each_day with Asia/Shanghai location",
			QuotaSpec{EachDay: intPtr(1)}, sh,
			time.Date(2026, 4, 15, 1, 0, 0, 0, sh),
			3600,
			1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := mustCompileQuota(t, c.spec, c.loc)
			periods := spec.Periods()
			if len(periods) != 1 {
				t.Fatalf("len(periods)=%d, want 1", len(periods))
			}
			win, quota := periods[0].GetWindowAndQuota(c.now)
			if win != c.wantWin || quota != c.wantQuota {
				t.Errorf("got (%d, %d), want (%d, %d)", win, quota, c.wantWin, c.wantQuota)
			}
		})
	}

	t.Run("uncompiled period returns zero", func(t *testing.T) {
		// A zero-value calendarPeriod (no kind, no location) must report
		// (0, 0) so a misconfigured / un-Compile()d spec is treated as
		// "no quota" rather than panicking.
		var p calendarPeriod
		win, quota := p.GetWindowAndQuota(time.Date(2026, 4, 15, 0, 0, 0, 0, utc))
		if win != 0 || quota != 0 {
			t.Errorf("got (%d, %d), want (0, 0)", win, quota)
		}
	})
}

func TestCalendarPeriodKeyPart(t *testing.T) {
	cases := []struct {
		name string
		spec QuotaSpec
		loc  *time.Location
		want string
	}{
		{"each_day", QuotaSpec{EachDay: intPtr(1)}, time.UTC, "each_day"},
		{"each_month", QuotaSpec{EachMonth: intPtr(1)}, time.UTC, "each_month"},
		{"each_year", QuotaSpec{EachYear: intPtr(1)}, time.UTC, "each_year"},
		{"key is timezone-independent", QuotaSpec{EachMonth: intPtr(1)}, mustLoadLocation(t, "Asia/Shanghai"), "each_month"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustCompileQuota(t, c.spec, c.loc)
			periods := s.Periods()
			if len(periods) != 1 {
				t.Fatalf("len(periods)=%d, want 1", len(periods))
			}
			if got := periods[0].KeyPart(); got != c.want {
				t.Errorf("KeyPart() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestQuotaSpecPeriodsMultiple(t *testing.T) {
	spec := mustCompileQuota(t, QuotaSpec{
		EachDay:   intPtr(1000),
		EachMonth: intPtr(20000),
		EachYear:  intPtr(200000),
	}, time.UTC)
	periods := spec.Periods()
	if len(periods) != 3 {
		t.Fatalf("len(periods)=%d, want 3", len(periods))
	}
	wantKinds := []PeriodKind{PeriodEachDay, PeriodEachMonth, PeriodEachYear}
	wantQuotas := []int{1000, 20000, 200000}
	for i := range periods {
		if periods[i].kind != wantKinds[i] {
			t.Errorf("periods[%d].kind=%q, want %q", i, periods[i].kind, wantKinds[i])
		}
		if periods[i].quota != wantQuotas[i] {
			t.Errorf("periods[%d].quota=%d, want %d", i, periods[i].quota, wantQuotas[i])
		}
		if got := periods[i].KeyPart(); got != string(wantKinds[i]) {
			t.Errorf("periods[%d].KeyPart()=%q, want %q", i, got, wantKinds[i])
		}
	}
}
