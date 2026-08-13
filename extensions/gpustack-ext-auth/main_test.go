package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func mustHeaderMatcher(t *testing.T, json string) matcher {
	t.Helper()
	m, err := buildHeaderMatcher(gjson.Parse(json).Array())
	if err != nil {
		t.Fatalf("buildHeaderMatcher(%s): %v", json, err)
	}
	return m
}

// testConfig mirrors the shape GPUStack actually ships: the allowed_headers
// list from the current ext-auth CR, plus the marker header.
func testConfig(t *testing.T) PluginConfig {
	t.Helper()
	return PluginConfig{
		Authz: Authz{
			RequestMethod: http.MethodGet,
			Path:          "/token-auth",
			TimeoutMillis: 30000,
			AllowedHeaders: mustHeaderMatcher(t, `[
				{"exact":"X-Real-IP"},
				{"exact":"X-Forwarded-For"},
				{"exact":"x-higress-llm-model"},
				{"exact":"x-api-key"},
				{"exact":"cookie"},
				{"exact":"X-GPUStack-Auth-Token"}
			]`),
			HeadersToAdd: map[string]string{"X-GPUStack-Auth-Token": "derived-token"},
		},
		AuthCache: AuthCache{Header: "x-gpustack-auth-cache"},
	}
}

var testRequestInfo = requestInfo{
	Method: "POST",
	Path:   "/v1/chat/completions",
	Host:   "gateway.example.com",
	Scheme: "https",
}

func TestBuildAuthzRequestHeadersFormAForwardsCredential(t *testing.T) {
	config := testConfig(t)
	requestHeaders := [][2]string{
		{"authorization", "Bearer gpustack_3192253c1f4a9b7e_" + vectorSecret},
		{"cookie", "session=real-user-cookie"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
		{"accept", "*/*"},
	}

	got := buildAuthzRequestHeaders(config, testRequestInfo, requestHeaders, identity{State: identityUnresolved})

	// Authorization is forwarded even though allowed_headers never lists it --
	// that unconditional forward is what makes today's /token-auth work.
	if got.Get("Authorization") == "" {
		t.Error("form A must forward the credential; the server does the authenticating")
	}
	if got.Get("Cookie") != "session=real-user-cookie" {
		t.Error("cookie is in allowed_headers and must survive form A")
	}
	if got.Get(headerAccessKey) != "" {
		t.Error("form A must not assert an identity it has not verified")
	}
	if got.Get("Accept") != "" {
		t.Error("a header outside allowed_headers must not cross to the authorization service")
	}
	if got.Get("X-Gpustack-Auth-Token") != "derived-token" {
		t.Error("headers_to_add must be applied")
	}
}

func TestBuildAuthzRequestHeadersFormBStripsEveryCredential(t *testing.T) {
	config := testConfig(t)
	requestHeaders := [][2]string{
		{"authorization", "Bearer gpustack_3192253c1f4a9b7e_" + vectorSecret},
		{"x-api-key", "gpustack_3192253c1f4a9b7e_" + vectorSecret},
		{"cookie", "session=real-user-cookie"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}

	got := buildAuthzRequestHeaders(config, testRequestInfo, requestHeaders,
		identity{State: identityResolved, AccessKey: "3192253c1f4a9b7e", UserID: 7})

	if got.Get(headerAccessKey) != "3192253c1f4a9b7e" {
		t.Fatalf("form B must assert the resolved access key, got %q", got.Get(headerAccessKey))
	}
	// Leaving any of these behind would have the server authenticate the
	// credential anyway, which is the entire cost form B exists to remove.
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if got.Get(name) != "" {
			t.Errorf("form B leaked credential header %s = %q", name, got.Get(name))
		}
	}
	if got.Get("X-Higress-Llm-Model") != "my-org/qwen3-8b" {
		t.Error("stripping credentials must not disturb the routing headers policy is evaluated against")
	}
}

func TestBuildAuthzRequestHeadersDropsClientSuppliedAssertion(t *testing.T) {
	config := testConfig(t)
	// Widen allowed_headers so a client-supplied assertion would otherwise be
	// copied through -- the worst case this guard exists for.
	config.Authz.AllowedHeaders = mustHeaderMatcher(t, `[{"prefix":"x-"}]`)

	requestHeaders := [][2]string{
		{"x-gpustack-access-key", "victim-access-key"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}

	got := buildAuthzRequestHeaders(config, testRequestInfo, requestHeaders, identity{State: identityUnresolved})

	if got.Get(headerAccessKey) != "" {
		t.Fatalf("a client-supplied identity assertion must never reach the server, got %q", got.Get(headerAccessKey))
	}
}

func TestBuildAuthzRequestHeadersForwardsMarkerWithoutBeingListed(t *testing.T) {
	config := testConfig(t)
	requestHeaders := [][2]string{{"x-gpustack-auth-cache", "signed.marker.value"}}

	got := buildAuthzRequestHeaders(config, testRequestInfo, requestHeaders, identity{State: identityUnresolved})

	if got.Get("X-Gpustack-Auth-Cache") != "signed.marker.value" {
		t.Error("the marker is the plugin's own protocol with the server and must not require an allowed_headers entry")
	}
}

func TestBuildAuthzRequestHeadersSetsForwardAuthMetadata(t *testing.T) {
	got := buildAuthzRequestHeaders(testConfig(t), testRequestInfo, nil, identity{State: identityUnresolved})

	for name, want := range map[string]string{
		headerOriginalMethod:   "POST",
		headerOriginalURI:      "/v1/chat/completions",
		headerXForwardedProto:  "https",
		headerXForwardedMethod: "POST",
		headerXForwardedURI:    "/v1/chat/completions",
		headerXForwardedHost:   "gateway.example.com",
	} {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want)
		}
	}
}

func TestResolveIdentity(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	expired := now.Unix() - 1
	live := now.Unix() + 3600

	base := func() PluginConfig {
		return PluginConfig{
			LocalAuth: LocalAuth{
				Enabled: true,
				Keys: map[string]keyEntry{
					"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7, Exp: &live},
					"b7d2e91045ac6f38": {Digest: vectorDigest, UserID: 9},
					"aaaaaaaaaaaaaaaa": {Digest: vectorDigest, UserID: 1, Exp: &expired},
					"bbbbbbbbbbbbbbbb": {Digest: "blake2b$salt$hash", UserID: 2},
				},
			},
		}
	}
	bearer := func(key string) [][2]string {
		return [][2]string{{"authorization", "Bearer " + key}}
	}

	cases := []struct {
		name         string
		config       func() PluginConfig
		headers      [][2]string
		wantState    identityState
		wantKey      string
		wantUser     int64
		wantUnusable bool
		why          string
	}{
		{
			name: "tier 1 hit", config: base,
			headers:   bearer("gpustack_3192253c1f4a9b7e_" + vectorSecret),
			wantState: identityResolved, wantKey: "3192253c1f4a9b7e", wantUser: 7,
		},
		{
			name: "tier 1 hit with no expiry", config: base,
			headers:   bearer("gpustack_b7d2e91045ac6f38_" + vectorSecret),
			wantState: identityResolved, wantKey: "b7d2e91045ac6f38", wantUser: 9,
		},
		{
			name: "known key, wrong secret", config: base,
			headers:   bearer("gpustack_3192253c1f4a9b7e_ffffffffffffffffffffffffffffffff"),
			wantState: identityRejected,
			why:       "the table held everything needed to decide, so no server round trip is warranted",
		},
		{
			name: "unknown access key", config: base,
			headers:   bearer("gpustack_ffffffffffffffff_" + vectorSecret),
			wantState: identityUnresolved,
			why:       "absence may just mean the config push has not landed yet",
		},
		{
			name: "expired entry defers rather than rejects", config: base,
			headers:   bearer("gpustack_aaaaaaaaaaaaaaaa_" + vectorSecret),
			wantState: identityUnresolved,
			why:       "expiry is judged against the gateway clock, so the server stays authoritative",
		},
		{
			name: "unreadable digest defers", config: base,
			headers:      bearer("gpustack_bbbbbbbbbbbbbbbb_" + vectorSecret),
			wantState:    identityUnresolved,
			wantUnusable: true,
			why:          "a digest this build cannot check says nothing about the credential",
		},
		{
			name: "custom key shape", config: base,
			headers:   bearer("sk-some-custom-key"),
			wantState: identityUnresolved,
		},
		{
			name: "no credential", config: base,
			headers:   [][2]string{{"accept", "*/*"}},
			wantState: identityUnresolved,
		},
		{
			name: "local auth disabled",
			config: func() PluginConfig {
				c := base()
				c.LocalAuth.Enabled = false
				return c
			},
			headers:   bearer("gpustack_3192253c1f4a9b7e_" + vectorSecret),
			wantState: identityUnresolved,
			why:       "with local_auth off the plugin must behave exactly as it does today",
		},
		{
			name: "empty key table",
			config: func() PluginConfig {
				return PluginConfig{LocalAuth: LocalAuth{Enabled: true}}
			},
			headers:   bearer("gpustack_3192253c1f4a9b7e_" + vectorSecret),
			wantState: identityUnresolved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveIdentity(tc.config(), tc.headers, now)
			if got.State != tc.wantState {
				t.Fatalf("state = %v, want %v. %s", got.State, tc.wantState, tc.why)
			}
			if got.DigestUnusable != tc.wantUnusable {
				t.Errorf("DigestUnusable = %v, want %v", got.DigestUnusable, tc.wantUnusable)
			}
			if tc.wantState == identityResolved {
				if got.AccessKey != tc.wantKey || got.UserID != tc.wantUser {
					t.Errorf("got (%q, %d), want (%q, %d)", got.AccessKey, got.UserID, tc.wantKey, tc.wantUser)
				}
			}
		})
	}
}

// The authorization call must never carry a client-supplied caller identity
// either: the server would have no way to tell it from one this plugin resolved.
func TestBuildAuthzRequestHeadersDropsClientSuppliedConsumer(t *testing.T) {
	config := testConfig(t)
	config.Authz.AllowedHeaders = mustHeaderMatcher(t, `[{"prefix":"x-"}]`)

	got := buildAuthzRequestHeaders(config, testRequestInfo, [][2]string{
		{"x-mse-consumer", "admin.gpustack-1"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}, identity{State: identityUnresolved})

	if got.Get(headerConsumer) != "" {
		t.Fatalf("a client-supplied consumer reached the authorization call: %q", got.Get(headerConsumer))
	}
}

// HTTP/2 lets a client split one logical Cookie across several header fields.
// Collapsing them to the last would hand the server a fragment -- enough to
// fail cookie_auth on a session that is perfectly valid.
func TestBuildAuthzRequestHeadersPreservesDuplicates(t *testing.T) {
	config := testConfig(t)
	got := buildAuthzRequestHeaders(config, testRequestInfo, [][2]string{
		{"cookie", "a=1"},
		{"cookie", "b=2"},
		{"x-forwarded-for", "203.0.113.1"},
		{"x-forwarded-for", "198.51.100.7"},
	}, identity{State: identityUnresolved})

	if values := got.Values("Cookie"); len(values) != 2 {
		t.Errorf("Cookie = %v, want both values forwarded", values)
	}
	if values := got.Values("X-Forwarded-For"); len(values) != 2 {
		t.Errorf("X-Forwarded-For = %v, want both values forwarded", values)
	}
}

// Duplicates must not survive where one authoritative value is meant: form B
// deletes by name, and a static headers_to_add entry replaces rather than
// appends.
func TestAuthoritativeHeadersCollapseDuplicates(t *testing.T) {
	config := testConfig(t)

	stripped := buildAuthzRequestHeaders(config, testRequestInfo, [][2]string{
		{"cookie", "a=1"},
		{"cookie", "b=2"},
	}, identity{State: identityResolved, AccessKey: "3192253c1f4a9b7e", UserID: 7})
	if values := stripped.Values("Cookie"); len(values) != 0 {
		t.Errorf("form B left %v behind", values)
	}

	overridden := buildAuthzRequestHeaders(config, testRequestInfo, [][2]string{
		{"x-gpustack-auth-token", "client-supplied"},
	}, identity{State: identityUnresolved})
	if values := overridden.Values("X-Gpustack-Auth-Token"); len(values) != 1 || values[0] != "derived-token" {
		t.Errorf("gateway token = %v, want only the configured value", values)
	}
}

// The server's authenticate_request is an if/elif chain -- basic, then cookie,
// then bearer / x-api-key -- so a cookie or basic credential means the bearer
// is never reached there. Deciding from it here would answer a different
// question than the one being stood in for, in three ways that are all silent.
func TestAServerFirstCredentialSuspendsLocalAuthentication(t *testing.T) {
	config := PluginConfig{
		LocalAuth: LocalAuth{
			Enabled: true,
			Keys: map[string]keyEntry{
				"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7},
			},
		},
	}
	good := "gpustack_3192253c1f4a9b7e_" + vectorSecret
	bad := "gpustack_3192253c1f4a9b7e_ffffffffffffffffffffffffffffffff"

	for name, tc := range map[string]struct {
		credential string
		extra      [2]string
		want       identityState
	}{
		// Without a second carrier the table did hold everything needed, and
		// both verdicts stand.
		"bearer alone, good secret": {good, [2]string{}, identityResolved},
		"bearer alone, bad secret":  {bad, [2]string{}, identityRejected},
		// A valid session beside a mistyped key authenticates on the server;
		// rejecting here would 401 a request the platform would have served.
		"cookie, bad secret": {bad, [2]string{"cookie", "gpustack_session=x"}, identityUnresolved},
		// And beside a valid key the server authenticates *as the session*, so
		// the consumer names the user and the key's scope does not apply.
		// Asserting the key here would diverge on both.
		"cookie, good secret": {good, [2]string{"cookie", "gpustack_session=x"}, identityUnresolved},
		"basic, good secret":  {good, [2]string{"authorization", "Basic dXNlcjpwdw=="}, identityUnresolved},
		// The scheme is case-insensitive, and it is now matched over a slice of
		// the header rather than a lower-cased copy of the whole of it.
		"basic in lower case": {good, [2]string{"authorization", "basic dXNlcjpwdw=="}, identityUnresolved},
	} {
		t.Run(name, func(t *testing.T) {
			headers := [][2]string{{"authorization", "Bearer " + tc.credential}}
			if tc.extra[0] != "" {
				// A basic credential occupies Authorization itself, so it
				// replaces the bearer rather than joining it.
				if tc.extra[0] == "authorization" {
					headers = [][2]string{tc.extra}
				} else {
					headers = append(headers, tc.extra)
				}
			}

			if got := resolveIdentityTiers(config, headers, time.Unix(1700000000, 0)).State; got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
		})
	}
}
