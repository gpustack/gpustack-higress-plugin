package main

import (
	"regexp"
	"testing"
)

func TestExtractClusterFQDN(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"envoy model", "outbound|80||model-1-2.static", "model-1-2.static"},
		{"envoy gpustack", "outbound|80||gpustack-server.gpustack.svc.cluster.local", "gpustack-server.gpustack.svc.cluster.local"},
		{"envoy provider with subset", "outbound|443|primary|provider-5.static", "provider-5.static"},
		{"raw fqdn", "raw-fqdn", "raw-fqdn"},
		{"three fields only", "outbound|80|", "outbound|80|"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractClusterFQDN(tt.in); got != tt.want {
				t.Errorf("extractClusterFQDN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultClusterMatchers(t *testing.T) {
	matchers := make([]*regexp.Regexp, 0, len(defaultClusterNameRegexps))
	for _, p := range defaultClusterNameRegexps {
		matchers = append(matchers, regexp.MustCompile(p))
	}
	tests := []struct {
		fqdn string
		want bool
	}{
		{"gpustack", true},
		{"gpustack-server", true},
		{"gpustack.gpustack.svc.cluster.local", true},
		{"gpustack-server.gpustack.svc.cluster.local", true},
		{"gpustackish-other", false},
		{"model-1-2", true},
		{"model-1-2.static", true},
		{"model-99-100.dns", true},
		{"model-bad", false},
		{"model-1", false},
		{"provider-5", true},
		{"provider-5.static", true},
		{"provider-bad", false},
		{"unrelated.svc.cluster.local", false},
	}
	for _, tt := range tests {
		t.Run(tt.fqdn, func(t *testing.T) {
			if got := matchesAnyCluster(tt.fqdn, matchers); got != tt.want {
				t.Errorf("matchesAnyCluster(%q) = %v, want %v", tt.fqdn, got, tt.want)
			}
		})
	}
}

func TestAdditionalClusterMatcher(t *testing.T) {
	matchers := []*regexp.Regexp{regexp.MustCompile(`^internal-svc(\.|$)`)}
	if !matchesAnyCluster("internal-svc.ns.svc.cluster.local", matchers) {
		t.Fatal("additional regexp did not match")
	}
	if matchesAnyCluster("internal-other", matchers) {
		t.Fatal("additional regexp matched unrelated FQDN")
	}
}
