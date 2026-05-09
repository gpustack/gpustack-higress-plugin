package main

import (
	"reflect"
	"testing"
)

func TestRateQuotaLimits(t *testing.T) {
	cases := []struct {
		name string
		q    RateQuota
		want []rateLimit
	}{
		{"per second", RateQuota{PerSecond: intPtr(10)}, []rateLimit{{window: 1, quota: 10}}},
		{"per minute", RateQuota{PerMinute: intPtr(60)}, []rateLimit{{window: 60, quota: 60}}},
		{"per hour", RateQuota{PerHour: intPtr(1000)}, []rateLimit{{window: 3600, quota: 1000}}},
		{"per day", RateQuota{PerDay: intPtr(10000)}, []rateLimit{{window: 86400, quota: 10000}}},
		{"custom", RateQuota{PerCustom: intPtr(500), CustomWindowSeconds: intPtr(30)}, []rateLimit{{window: 30, quota: 500}}},
		{"custom missing window", RateQuota{PerCustom: intPtr(500)}, nil},
		{"custom zero window", RateQuota{PerCustom: intPtr(500), CustomWindowSeconds: intPtr(0)}, nil},
		{"empty", RateQuota{}, nil},
		{"zero quota dropped", RateQuota{PerMinute: intPtr(0)}, nil},
		{"negative quota dropped", RateQuota{PerMinute: intPtr(-5)}, nil},
		{
			"second + minute coexist",
			RateQuota{PerSecond: intPtr(10), PerMinute: intPtr(60)},
			[]rateLimit{{window: 1, quota: 10}, {window: 60, quota: 60}},
		},
		{
			"all five windows",
			RateQuota{
				PerSecond:           intPtr(10),
				PerMinute:           intPtr(300),
				PerHour:             intPtr(1000),
				PerDay:              intPtr(10000),
				PerCustom:           intPtr(500),
				CustomWindowSeconds: intPtr(45),
			},
			[]rateLimit{
				{window: 1, quota: 10},
				{window: 60, quota: 300},
				{window: 3600, quota: 1000},
				{window: 86400, quota: 10000},
				{window: 45, quota: 500},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.q.Limits()
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Limits() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestRateQuotaLimitsNilReceiver(t *testing.T) {
	var r *RateQuota
	if got := r.Limits(); got != nil {
		t.Errorf("nil RateQuota.Limits() = %#v, want nil", got)
	}
}

func TestRateLimitKeyPart(t *testing.T) {
	cases := []struct {
		l    rateLimit
		want string
	}{
		{rateLimit{window: 1, quota: 10}, "1s"},
		{rateLimit{window: 60, quota: 60}, "60s"},
		{rateLimit{window: 3600, quota: 1000}, "3600s"},
		{rateLimit{window: 86400, quota: 1}, "86400s"},
		{rateLimit{window: 45, quota: 500}, "45s"},
	}
	for _, c := range cases {
		if got := c.l.KeyPart(); got != c.want {
			t.Errorf("rateLimit{window:%d}.KeyPart() = %q, want %q", c.l.window, got, c.want)
		}
	}
}
