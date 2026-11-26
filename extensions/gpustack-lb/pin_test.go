package main

import (
	"testing"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/tidwall/gjson"
)

func defaultTargetRules(t *testing.T) wire.TargetRules {
	t.Helper()
	r, err := wire.CompileTargetRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPinMatches(t *testing.T) {
	rules := defaultTargetRules(t)
	tests := []struct {
		target, service string
		want            bool
	}{
		{"7", "model-12-7", true},
		{"7", "model-3-7", true}, // the model is the route's; only the instance is matched
		{"provider-5", "provider-5", true},
		// instance 7 never matches 71, 17 or a provider
		{"7", "model-12-71", false},
		{"7", "model-12-17", false},
		{"7", "provider-7", false},
		// a provider pin never matches an instance, nor another provider
		{"provider-5", "model-provider-5", false},
		{"provider-5", "provider-51", false},
		{"5", "provider-5", false},
		// an arbitrary target is simply a miss
		{"7-x", "model-12-7", false},
		{"", "model-12-7", false},
		{"7", "", false},
	}
	for _, tt := range tests {
		if got := pinMatches(tt.target, tt.service, rules); got != tt.want {
			t.Errorf("pinMatches(%q, %q) = %v, want %v", tt.target, tt.service, got, tt.want)
		}
	}
}

// The rules are configuration: a target names whichever service names the
// operator's cluster regex says, its groups pairing with the target's.
func TestPinMatchesCustomRules(t *testing.T) {
	rules, err := wire.CompileTargetRules([]wire.TargetRule{
		{Target: `^([a-z]+)-(\d+)$`, Cluster: `pool-\d+-([a-z]+)-node-(\d+)`},
		{Target: `^(x\d+)$`, Cluster: `model-\d+-(x\d+)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		target, service string
		want            bool
	}{
		{"blue-9", "pool-3-blue-node-9", true},
		// another pool, another colour, another node: all misses
		{"blue-9", "pool-3-red-node-9", false},
		{"blue-9", "pool-3-blue-node-19", false},
		// the default rules are gone with the custom list
		{"7", "model-12-7", false},
		// the quantifier sits inside the group, which is replaced whole:
		// x7 is x7 and x77 is x77
		{"x7", "model-42-x77", false},
		{"x7", "model-42-x7", true},
		{"x77", "model-42-x77", true},
	}
	for _, tt := range tests {
		if got := pinMatches(tt.target, tt.service, rules); got != tt.want {
			t.Errorf("pinMatches(%q, %q) = %v, want %v", tt.target, tt.service, got, tt.want)
		}
	}
}

// targetPatterns is deployment-level: a finisher matchRule that omits the
// key inherits the global list, one that sets it overrides.
func TestOverrideInheritsTargetRules(t *testing.T) {
	custom := `{"targetPatterns":[{"target":"^n-(\\d+)$","cluster":"model-\\d+-n(\\d+)"}]}`
	var global Config
	if err := parseConfig(gjson.Parse(custom), &global); err != nil {
		t.Fatal(err)
	}

	var inherited Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"a"}]}`), global, &inherited); err != nil {
		t.Fatal(err)
	}
	if inherited.targetRules.TargetOf("model-42-n7") != "n-7" {
		t.Fatalf("rule without targetPatterns did not inherit the global rules: %q", inherited.targetRules.TargetOf("model-42-n7"))
	}

	var overridden Config
	other := `{"candidates":[{"cluster":"a"}],"targetPatterns":[{"target":"^(\\d+)$","cluster":"model-\\d+-(\\d+)"}]}`
	if err := parseOverrideConfig(gjson.Parse(other), global, &overridden); err != nil {
		t.Fatal(err)
	}
	if overridden.targetRules.TargetOf("model-42-7") != "7" {
		t.Fatalf("rule with targetPatterns did not override: %q", overridden.targetRules.TargetOf("model-42-7"))
	}
}

func TestParseConfigTargetPatterns(t *testing.T) {
	custom := `{"targetPatterns":[{"target":"^([a-z]+)$","cluster":"pool-([a-z]+)"}]}`
	tests := []struct {
		name, json string
		wantErr    bool
		custom     bool
	}{
		{"absent: defaults", `{}`, false, false},
		{"empty array: defaults", `{"targetPatterns":[]}`, false, false},
		{"custom", custom, false, true},
		{"not an array", `{"targetPatterns":"x"}`, true, false},
		{"a bare string, not a rule", `{"targetPatterns":["^pool-([a-z]+)$"]}`, true, false},
		{"group counts differ", `{"targetPatterns":[{"target":"^(\\d+)$","cluster":"model-(\\d+)-(\\d+)"}]}`, true, false},
		{"the old $n template", `{"targetPatterns":[{"target":"^(\\d+)$","cluster":"model-(\\d+)-$1"}]}`, true, false},
		{"cluster missing", `{"targetPatterns":[{"target":"^([a-z]+)$"}]}`, true, false},
		{"not reversible", `{"targetPatterns":[{"target":"^\\d+$","cluster":"pool-x"}]}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			err := parseConfig(gjson.Parse(tt.json), &cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConfig err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(cfg.targetRules) == 0 {
				t.Fatal("no target rules compiled")
			}
			// behaviour, not representation: what the compiled rules do
			isCustom := cfg.targetRules.TargetOf("pool-blue") == "blue"
			if isCustom != tt.custom {
				t.Fatalf("custom=%v, but TargetOf(pool-blue) = %q", tt.custom, cfg.targetRules.TargetOf("pool-blue"))
			}
			if !tt.custom && cfg.targetRules.TargetOf("model-12-7") != "7" {
				t.Fatal("defaults do not map model-12-7 to 7")
			}
		})
	}
}

func TestPinJoinsThroughCandidateSet(t *testing.T) {
	set := CandidateSet{Candidates: []Candidate{
		{Cluster: "outbound|80||model-12-71.static"},
		{Cluster: "outbound|80||model-12-7.static"},
	}}
	patterns := defaultTargetRules(t)
	for i := range set.Candidates {
		if pinMatches("7", clusterFQDN(set.Candidates[i].Cluster), patterns) {
			if got := set.Candidates[i].Cluster; got != "outbound|80||model-12-7.static" {
				t.Errorf("matched %q", got)
			}
		}
	}
}

func TestClusterFQDN(t *testing.T) {
	tests := []struct {
		cluster string
		want    string
	}{
		{"outbound|80||model-12-7.static", "model-12-7"},
		{"outbound|80||provider-5.static", "provider-5"},
		{"outbound|80||model-12-7.dns", "model-12-7"},
		{"outbound|80||provider-5.static.svc.cluster.local", "provider-5"},
		{"outbound|80||model-12-7", "model-12-7"},
		// a subset cluster addresses the same service
		{"outbound|80|v1|model-12-7.static", "model-12-7"},
		// A non-standard shape keeps its whole name: it is not something a
		// pin can ever name, so it must simply never match.
		{"inbound|80|v1|model-12-7.static", "inbound|80|v1|model-12-7"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := clusterFQDN(tt.cluster); got != tt.want {
			t.Errorf("clusterFQDN(%q) = %q, want %q", tt.cluster, got, tt.want)
		}
	}
}
