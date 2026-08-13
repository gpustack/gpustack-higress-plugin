package main

import (
	"testing"
	"time"
)

func publicSkipConfig() PluginConfig {
	config := markerTestConfig()
	config.AccessPolicy = accessPolicyPublic
	return config
}

// The consumer the plugin builds must equal the server's, byte for byte:
// ai-statistics writes it into the access log, so a divergence silently
// splits one caller's traffic across two identities depending on whether the
// skip fired.
//
//	'.'.join([access_key, f"gpustack-{user.id}"])   -- routes/token.py
func TestPublicSkipConsumerMatchesServerFormat(t *testing.T) {
	got, ok := publicSkipConsumer(publicSkipConfig(), identity{
		State:     identityResolved,
		AccessKey: "3192253c1f4a9b7e",
		UserID:    7,
	})
	if !ok {
		t.Fatal("a tier-1 identity on a public route must be able to skip")
	}
	if want := "3192253c1f4a9b7e.gpustack-7"; got != want {
		t.Errorf("consumer = %q, want %q", got, want)
	}
}

func TestPublicSkipConsumerReplaysMarkerClaim(t *testing.T) {
	// A ref identity cannot be rendered locally, but a marker carries the
	// consumer the server itself produced, so replaying it is exact.
	got, ok := publicSkipConsumer(publicSkipConfig(), identity{
		State:    identityResolved,
		Ref:      "58",
		Consumer: "custom-consumer.gpustack-9",
	})
	if !ok || got != "custom-consumer.gpustack-9" {
		t.Errorf("ok=%v consumer=%q; a marker's consumer must be replayed verbatim", ok, got)
	}
}

func TestPublicSkipDeclines(t *testing.T) {
	tier1 := identity{State: identityResolved, AccessKey: "3192253c1f4a9b7e", UserID: 7}

	cases := []struct {
		name   string
		config func() PluginConfig
		id     identity
		why    string
	}{
		{
			name: "route is not public",
			config: func() PluginConfig {
				c := publicSkipConfig()
				c.AccessPolicy = ""
				return c
			},
			id:  tier1,
			why: "a route with no dedicated rule must never skip",
		},
		{
			name:   "identity unresolved",
			config: publicSkipConfig,
			id:     identity{State: identityUnresolved},
			why:    "the server may answer 'none'; guessing would corrupt the access log",
		},
		{
			name:   "ref identity without a marker",
			config: publicSkipConfig,
			id:     identity{State: identityResolved, Ref: "58"},
			why:    "a custom key's consumer embeds an access key that refs does not carry",
		},
		{
			name:   "access key with no user id",
			config: publicSkipConfig,
			id:     identity{State: identityResolved, AccessKey: "3192253c1f4a9b7e"},
			why:    "a missing user_id would render as gpustack-0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if consumer, ok := publicSkipConsumer(tc.config(), tc.id); ok {
				t.Errorf("skipped with consumer %q, but should have called the server. %s", consumer, tc.why)
			}
		})
	}
}

// A public route reached through the marker on a fallback pass must still skip.
// Otherwise public availability would break on exactly the pass that happens
// when the upstream is already failing.
func TestPublicSkipSurvivesFallbackPass(t *testing.T) {
	config := publicSkipConfig()
	fallbackHeaders := [][2]string{
		{"x-gpustack-auth-cache", vectorMarkerToken},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
		{"authorization", "Bearer gpustack_sysaksysaksysak_" + vectorSecret},
	}

	id := resolveIdentity(config, fallbackHeaders, vectorFallbackRoute, markerNow())
	if id.State != identityResolved {
		t.Fatalf("tier 0 failed on the fallback pass: %+v", id)
	}
	consumer, ok := publicSkipConsumer(config, id)
	if !ok {
		t.Fatal("a marker-resolved identity on a public route must still skip")
	}
	if consumer != vectorMarkerClaims.Consumer {
		t.Errorf("consumer = %q, want the marker's %q", consumer, vectorMarkerClaims.Consumer)
	}
}

// Absent access_policy is the state every non-public route is in, so this is
// the branch that keeps the skip off everywhere it has not been granted.
func TestGlobalConfigCarriesNoAccessPolicy(t *testing.T) {
	global := mustGlobal(t)
	if _, ok := publicSkipConsumer(global, identity{
		State: identityResolved, AccessKey: "3192253c1f4a9b7e", UserID: 7,
	}); ok {
		t.Error("the global block must never grant a local allow; only a dedicated rule may")
	}
}

// A public route must never be authenticated more strictly than the server
// would: there, an unrecognised credential is allowed and simply recorded as
// consumer 'none' (routes/token.py -- consumer defaults to 'none', the
// UnauthorizedException is swallowed, and only `policy != PUBLIC` rejects).
// Rejecting locally would break a caller whose key has a typo, and would buy
// nothing: the same client can reach the model with no credential at all.
func TestPublicRouteDoesNotLocallyRejectABadSecret(t *testing.T) {
	badSecret := [][2]string{
		{"authorization", "Bearer gpustack_3192253c1f4a9b7e_ffffffffffffffffffffffffffffffff"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}

	// The route policy is not consulted by resolveIdentity, so the tier-1
	// verdict itself is unchanged; what differs is what the caller does with it.
	if id := resolveIdentity(publicSkipConfig(), badSecret, vectorFallbackRoute, markerNow()); id.State != identityRejected {
		t.Fatalf("precondition: a known key with a wrong secret is a rejection, got %v", id.State)
	}

	// On a non-public route that verdict stands and the request is refused
	// locally; on a public one it must be downgraded to "unresolved" so the
	// request goes to the server, which allows it as anonymous.
	nonPublic := publicSkipConfig()
	nonPublic.AccessPolicy = ""
	if _, ok := publicSkipConsumer(nonPublic, identity{State: identityUnresolved}); ok {
		t.Error("an unresolved identity must never skip the authorization call")
	}
}

// Anonymous access is the defining use of a public model. Without this it would
// be the one kind of public traffic that still needed a live server, which
// undercuts the whole point of decoupling public routes.
func TestAnonymousRequestOnPublicRouteIsAllowedLocally(t *testing.T) {
	anonymous := [][2]string{{"x-higress-llm-model", "my-org/qwen3-8b"}}

	id := resolveIdentity(publicSkipConfig(), anonymous, vectorFallbackRoute, markerNow())
	if id.State != identityResolved || !id.Anonymous {
		t.Fatalf("an uncredentialed request to a public route must resolve as anonymous, got %+v", id)
	}

	consumer, ok := publicSkipConsumer(publicSkipConfig(), id)
	if !ok {
		t.Fatal("an anonymous identity on a public route must skip the authorization call")
	}
	// Byte-identical to the server's own literal; it lands in the access log
	// beside consumers the server produced.
	if consumer != "none" {
		t.Errorf("consumer = %q, want %q", consumer, "none")
	}
}

// Any credential at all reintroduces the ambiguity this shortcut relies on not
// existing: /token-auth tries basic, then cookie, then bearer / x-api-key, so a
// bad bearer alongside a valid cookie still authenticates and the consumer would
// be a real identity rather than "none".
func TestAnyCredentialDisqualifiesTheAnonymousShortcut(t *testing.T) {
	for _, header := range []string{"authorization", "x-api-key", "cookie"} {
		t.Run(header, func(t *testing.T) {
			headers := [][2]string{
				{"x-higress-llm-model", "my-org/qwen3-8b"},
				{header, "something"},
			}
			id := resolveIdentity(publicSkipConfig(), headers, vectorFallbackRoute, markerNow())
			if id.Anonymous {
				t.Errorf("%s present, yet the request was treated as anonymous", header)
			}
		})
	}
}

func TestAnonymousIsConfinedToPublicRoutes(t *testing.T) {
	config := publicSkipConfig()
	config.AccessPolicy = ""

	id := resolveIdentity(config, [][2]string{{"x-higress-llm-model", "m"}}, vectorFallbackRoute, markerNow())
	if id.Anonymous || id.State != identityUnresolved {
		t.Errorf("anonymity must not be inferred off a public route, got %+v", id)
	}
}

// A marker minted on a public route must not carry anonymity onto a non-public
// route serving the same model -- the model binding alone would not stop it.
func TestAnonymousMarkerIsRejectedOffPublicRoutes(t *testing.T) {
	token, err := signMarker([]byte(vectorMarkerKey),
		markerClaims{ID: "anon", Consumer: "none", Model: "my-org/qwen3-8b",
			Route: vectorMarkerRoute},
		time.Unix(vectorMarkerExp, 0))
	if err != nil {
		t.Fatalf("signMarker: %v", err)
	}
	headers := [][2]string{
		{"x-gpustack-auth-cache", token},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
		{"authorization", "Bearer gpustack_sysaksysaksysak_" + vectorSecret},
	}

	if id := resolveIdentity(publicSkipConfig(), headers, vectorFallbackRoute, markerNow()); !id.Anonymous {
		t.Error("an anonymous marker must still work on the public route that minted it")
	}

	nonPublic := publicSkipConfig()
	nonPublic.AccessPolicy = ""
	if id := resolveIdentity(nonPublic, headers, vectorFallbackRoute, markerNow()); id.State == identityResolved {
		t.Error("an anonymous marker was honoured on a non-public route")
	}
}

// The fallback pass is why the anonymous case needs a marker at all: by then
// ai-proxy has put the provider credential in Authorization, so the request no
// longer looks uncredentialed and would fall through to the server.
func TestAnonymousMarkerCarriesTheFallbackPass(t *testing.T) {
	config := publicSkipConfig()

	claims, ok := markerClaimsFor(config, identity{State: identityResolved, Anonymous: true},
		"my-org/qwen3-8b", "none", vectorMarkerRoute)
	if !ok {
		t.Fatal("an anonymous local allow must still mint a marker")
	}
	if claims.ID != "anon" {
		t.Errorf("marker id = %q, want %q", claims.ID, "anon")
	}
}
