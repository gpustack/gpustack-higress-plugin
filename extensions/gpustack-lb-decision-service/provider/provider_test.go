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
