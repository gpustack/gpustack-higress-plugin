package main

import "testing"

func TestRateQuotaGetWindowAndQuota(t *testing.T) {
	cases := []struct {
		name      string
		q         RateQuota
		wantWin   int64
		wantQuota int
	}{
		{"per second", RateQuota{PerSecond: intPtr(10)}, 1, 10},
		{"per minute", RateQuota{PerMinute: intPtr(60)}, 60, 60},
		{"per hour", RateQuota{PerHour: intPtr(1000)}, 3600, 1000},
		{"per day", RateQuota{PerDay: intPtr(10000)}, 86400, 10000},
		{"custom", RateQuota{PerCustom: intPtr(500), CustomWindowSeconds: intPtr(30)}, 30, 500},
		{"custom missing window", RateQuota{PerCustom: intPtr(500)}, 0, 0},
		{"empty", RateQuota{}, 0, 0},
		{"precedence second>minute", RateQuota{PerSecond: intPtr(10), PerMinute: intPtr(60)}, 1, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, q := c.q.GetWindowAndQuota()
			if w != c.wantWin || q != c.wantQuota {
				t.Errorf("got (%d,%d), want (%d,%d)", w, q, c.wantWin, c.wantQuota)
			}
		})
	}
}

func TestRateQuotaKeyPart(t *testing.T) {
	cases := []struct {
		name string
		q    RateQuota
		want string
	}{
		{"per second", RateQuota{PerSecond: intPtr(10)}, "1s"},
		{"per minute", RateQuota{PerMinute: intPtr(60)}, "60s"},
		{"per hour", RateQuota{PerHour: intPtr(1000)}, "3600s"},
		{"per day", RateQuota{PerDay: intPtr(1)}, "86400s"},
		{"custom", RateQuota{PerCustom: intPtr(500), CustomWindowSeconds: intPtr(45)}, "45s"},
		{"empty", RateQuota{}, ""},
		{"zero quota", RateQuota{PerMinute: intPtr(0)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.q.KeyPart()
			if got != c.want {
				t.Errorf("KeyPart() = %q, want %q", got, c.want)
			}
		})
	}
}
