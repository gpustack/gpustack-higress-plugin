package main

import "testing"

func TestPluginConfigValidate(t *testing.T) {
	validCombo := func() LimitCombination {
		return LimitCombination{
			Name:        "c1",
			Match:       []MatchRule{{Source: SourceHeader, Name: "x-api-key", Value: "abc"}},
			QueryLimits: &RateQuota{PerMinute: intPtr(100)},
		}
	}

	cases := []struct {
		name    string
		cfg     PluginConfig
		wantErr bool
	}{
		{
			name:    "ok",
			cfg:     PluginConfig{RuleName: "r", LimitCombinations: []LimitCombination{validCombo()}},
			wantErr: false,
		},
		{
			name:    "empty rule_name",
			cfg:     PluginConfig{LimitCombinations: []LimitCombination{validCombo()}},
			wantErr: true,
		},
		{
			name:    "empty combinations",
			cfg:     PluginConfig{RuleName: "r"},
			wantErr: true,
		},
		{
			name:    "rejected_code out of range",
			cfg:     PluginConfig{RuleName: "r", LimitCombinations: []LimitCombination{validCombo()}, RejectedCode: 99},
			wantErr: true,
		},
		{
			name: "duplicate combo name",
			cfg: PluginConfig{
				RuleName:          "r",
				LimitCombinations: []LimitCombination{validCombo(), validCombo()},
			},
			wantErr: true,
		},
		{
			name: "missing combo name",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Match:       []MatchRule{{Source: SourceHeader, Name: "x", Value: "y"}},
					QueryLimits: &RateQuota{PerMinute: intPtr(10)},
				}},
			},
			wantErr: true,
		},
		{
			name: "empty match",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:        "c1",
					QueryLimits: &RateQuota{PerMinute: intPtr(10)},
				}},
			},
			wantErr: true,
		},
		{
			name: "no limits configured",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:  "c1",
					Match: []MatchRule{{Source: SourceHeader, Name: "x", Value: "y"}},
				}},
			},
			wantErr: true,
		},
		{
			name: "bad regexp in match",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:        "c1",
					Match:       []MatchRule{{Source: SourceHeader, Name: "x", Value: "regexp:[["}},
					QueryLimits: &RateQuota{PerMinute: intPtr(10)},
				}},
			},
			wantErr: true,
		},
		{
			name: "bad source in match",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:        "c1",
					Match:       []MatchRule{{Source: "XYZ", Name: "x", Value: "y"}},
					QueryLimits: &RateQuota{PerMinute: intPtr(10)},
				}},
			},
			wantErr: true,
		},
		{
			name: "token_quota only (no rolling limits)",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:       "c1",
					Match:      []MatchRule{{Source: SourceConsumer, Value: "*"}},
					TokenQuota: &QuotaSpec{EachMonth: intPtr(1000000)},
				}},
			},
			wantErr: false,
		},
		{
			name: "top-level timezone applied to token_quota",
			cfg: PluginConfig{
				RuleName: "r",
				Timezone: "Asia/Shanghai",
				LimitCombinations: []LimitCombination{{
					Name:       "c1",
					Match:      []MatchRule{{Source: SourceConsumer, Value: "*"}},
					TokenQuota: &QuotaSpec{EachMonth: intPtr(100)},
				}},
			},
			wantErr: false,
		},
		{
			name: "bad top-level timezone rejected",
			cfg: PluginConfig{
				RuleName: "r",
				Timezone: "Bogus/Zone",
				LimitCombinations: []LimitCombination{{
					Name:       "c1",
					Match:      []MatchRule{{Source: SourceConsumer, Value: "*"}},
					TokenQuota: &QuotaSpec{EachMonth: intPtr(100)},
				}},
			},
			wantErr: true,
		},
		{
			name: "token_quota with no period set",
			cfg: PluginConfig{
				RuleName: "r",
				LimitCombinations: []LimitCombination{{
					Name:       "c1",
					Match:      []MatchRule{{Source: SourceConsumer, Value: "*"}},
					TokenQuota: &QuotaSpec{},
				}},
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}
