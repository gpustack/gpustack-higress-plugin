package provider

import (
	"net/http"
	"reflect"
	"testing"
)

func TestEffectiveAPITokens(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProviderConfig
		want []string
	}{
		{"list wins over legacy", ProviderConfig{APIToken: "legacy", APITokens: []string{"a", "b"}}, []string{"a", "b"}},
		{"legacy alone", ProviderConfig{APIToken: "legacy"}, []string{"legacy"}},
		{"empty entries dropped", ProviderConfig{APITokens: []string{"a", "", "c"}}, []string{"a", "c"}},
		{"only empty entries -> anonymous", ProviderConfig{APITokens: []string{"", ""}}, nil},
		{"explicit empty list beats legacy apiToken", ProviderConfig{APIToken: "legacy", APITokens: []string{}}, nil},
		{"nothing -> anonymous", ProviderConfig{}, nil},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveAPITokens(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: EffectiveAPITokens() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAuthHeadersPerAttempt(t *testing.T) {
	s := NewSystemone(&ProviderConfig{Endpoint: "https://jev", APITokens: []string{"primary", "fallback"}})

	hs := http.Header{}
	s.AuthHeaders(hs, "primary")
	if got := hs.Get("Authorization"); got != "Bearer primary" {
		t.Errorf("Authorization = %q, want Bearer primary", got)
	}

	// The retry attempt must send the fallback credential, not whatever
	// the provider was configured with first.
	hs2 := http.Header{}
	s.AuthHeaders(hs2, "fallback")
	if got := hs2.Get("Authorization"); got != "Bearer fallback" {
		t.Errorf("Authorization = %q, want Bearer fallback", got)
	}

	// Anonymous attempt: no header at all.
	hs3 := http.Header{}
	s.AuthHeaders(hs3, "")
	if got := hs3.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty for anonymous callout", got)
	}
}

func TestEffectiveClusters(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProviderConfig
		want []string
	}{
		{"list wins over legacy", ProviderConfig{Cluster: "legacy", Clusters: []string{"a", "b"}}, []string{"a", "b"}},
		{"legacy alone", ProviderConfig{Cluster: "legacy"}, []string{"legacy"}},
		{"empty entries dropped", ProviderConfig{Clusters: []string{"a", "", "c"}}, []string{"a", "c"}},
		{"only empty entries -> none", ProviderConfig{Clusters: []string{"", ""}}, nil},
		{"explicit empty list beats legacy cluster", ProviderConfig{Cluster: "legacy", Clusters: []string{}}, nil},
		{"nothing -> none", ProviderConfig{}, nil},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveClusters(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: EffectiveClusters() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecisionClusterMultiClusterRandomPick(t *testing.T) {
	clusters := []string{"outbound|8010||jev-0.static", "outbound|8010||jev-1.static", "outbound|8010||jev-2.static"}
	s := NewSystemone(&ProviderConfig{Endpoint: "http://10.0.0.1:8010", Clusters: clusters})

	if got := s.DecisionClusters(); !reflect.DeepEqual(got, clusters) {
		t.Fatalf("DecisionClusters() = %v, want %v", got, clusters)
	}

	// Every pick must land inside the configured set, and over enough
	// draws every cluster must be hit — the uniform random pick is what
	// spreads decision traffic across GPUStack model-instance replicas.
	// With 300 draws over 3 clusters the chance of one never appearing
	// is 3*(2/3)^300 ≈ 0, so this cannot flake.
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		pick := s.DecisionCluster()
		valid := false
		for _, c := range clusters {
			if pick == c {
				valid = true
				break
			}
		}
		if !valid {
			t.Fatalf("DecisionCluster() = %q, want one of %v", pick, clusters)
		}
		seen[pick]++
	}
	if len(seen) != len(clusters) {
		t.Errorf("random pick covered %d of %d clusters: %v", len(seen), len(clusters), seen)
	}
}

func TestDecisionClusterSingleForms(t *testing.T) {
	// Single configured cluster: deterministic, no randomness.
	single := NewSystemone(&ProviderConfig{Endpoint: "http://10.0.0.1:8010", Cluster: "outbound|8010||jev.static"})
	for i := 0; i < 10; i++ {
		if got := single.DecisionCluster(); got != "outbound|8010||jev.static" {
			t.Fatalf("DecisionCluster() = %q, want outbound|8010||jev.static", got)
		}
	}

	// No configured cluster: the endpoint-derived default applies.
	derived := NewSystemone(&ProviderConfig{Endpoint: "https://jev.internal:8010"})
	if got := derived.DecisionClusters(); !reflect.DeepEqual(got, []string{"outbound|8010||jev.internal"}) {
		t.Errorf("DecisionClusters() = %v, want [outbound|8010||jev.internal]", got)
	}

	// Explicit empty clusters list: derives from the endpoint too — the
	// operator retired the explicit cluster, not the endpoint.
	empty := NewSystemone(&ProviderConfig{Endpoint: "http://jev.internal", Clusters: []string{}})
	if got := empty.DecisionCluster(); got != "outbound|80||jev.internal" {
		t.Errorf("DecisionCluster() = %q, want outbound|80||jev.internal", got)
	}
}

func TestDecisionHeaders(t *testing.T) {
	// clusterNameHeader carries the chosen cluster's service name (segment
	// 4 of the outbound|<port>||<service> form), derived per attempt so the
	// value follows the random cluster pick — an entrypoint shared by
	// several members routes by it. No header when the field is unset or
	// the cluster is not in the outbound form.
	cfg := &ProviderConfig{
		Endpoint:          "http://d3.internal:8010",
		ClusterNameHeader: "X-GPUStack-Model-Instance",
	}
	p := NewSystemone(cfg)

	cases := []struct {
		name    string
		cluster string
		want    [][2]string
	}{
		{"static registry form", "outbound|80||model-12-35.static", [][2]string{
			{"X-GPUStack-Model-Instance", "model-12-35.static"},
		}},
		{"worker proxy dns form", "outbound|10176||model-12-41.dns", [][2]string{
			{"X-GPUStack-Model-Instance", "model-12-41.dns"},
		}},
		{"not the outbound form", "api.typesafe.ai", nil},
	}
	for _, c := range cases {
		if got := p.DecisionHeaders(c.cluster); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: DecisionHeaders(%q) = %v, want %v", c.name, c.cluster, got, c.want)
		}
	}

	bare := NewSystemone(&ProviderConfig{Endpoint: "http://d3.internal:8010"})
	if got := bare.DecisionHeaders("outbound|80||model-12-35.static"); got != nil {
		t.Errorf("no header configured: DecisionHeaders() = %v, want nil", got)
	}
}
