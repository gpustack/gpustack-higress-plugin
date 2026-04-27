package main

import (
	"testing"
	"time"
)

func TestQuotaSpecCompile(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name     string
		spec     QuotaSpec
		loc      *time.Location
		wantErr  bool
		wantKind PeriodKind
	}{
		{"each_day ok", QuotaSpec{EachDay: intPtr(1000)}, utc, false, PeriodEachDay},
		{"each_month ok", QuotaSpec{EachMonth: intPtr(1000000)}, utc, false, PeriodEachMonth},
		{"each_year ok", QuotaSpec{EachYear: intPtr(10000000)}, utc, false, PeriodEachYear},
		{"none set", QuotaSpec{}, utc, true, ""},
		{"two set", QuotaSpec{EachDay: intPtr(1), EachMonth: intPtr(1)}, utc, true, ""},
		{"three set", QuotaSpec{EachDay: intPtr(1), EachMonth: intPtr(1), EachYear: intPtr(1)}, utc, true, ""},
		{"zero limit", QuotaSpec{EachMonth: intPtr(0)}, utc, true, ""},
		{"negative limit", QuotaSpec{EachMonth: intPtr(-1)}, utc, true, ""},
		{"nil location", QuotaSpec{EachMonth: intPtr(1)}, nil, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.spec
			err := s.Compile(c.loc)
			if (err != nil) != c.wantErr {
				t.Fatalf("Compile() err=%v, wantErr=%v", err, c.wantErr)
			}
			if err == nil && s.kind != c.wantKind {
				t.Errorf("kind=%q, want %q", s.kind, c.wantKind)
			}
		})
	}
}

func TestQuotaSpecGetWindowAndQuota(t *testing.T) {
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
			win, quota := spec.GetWindowAndQuota(c.now)
			if win != c.wantWin || quota != c.wantQuota {
				t.Errorf("got (%d, %d), want (%d, %d)", win, quota, c.wantWin, c.wantQuota)
			}
		})
	}

	t.Run("uncompiled returns zero", func(t *testing.T) {
		spec := QuotaSpec{EachMonth: intPtr(100)}
		win, quota := spec.GetWindowAndQuota(time.Date(2026, 4, 15, 0, 0, 0, 0, utc))
		if win != 0 || quota != 0 {
			t.Errorf("got (%d, %d), want (0, 0)", win, quota)
		}
	})
}

func TestQuotaSpecKeyPart(t *testing.T) {
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
			if got := s.KeyPart(); got != c.want {
				t.Errorf("KeyPart() = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("uncompiled returns empty", func(t *testing.T) {
		s := QuotaSpec{EachMonth: intPtr(1)}
		if got := s.KeyPart(); got != "" {
			t.Errorf("KeyPart() = %q, want empty", got)
		}
	})
}
