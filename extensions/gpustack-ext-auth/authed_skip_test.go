package main

import (
	"testing"
	"time"
)

// authedSkipConfig is an authed route whose one key carries the flag. The
// marker vector's key is reused so a fallback pass can be exercised against the
// same entry.
func authedSkipConfig() PluginConfig {
	config := markerTestConfig()
	config.AccessPolicy = accessPolicyAuthed
	config.LocalAuth.Keys = map[string]keyEntry{
		"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7, Unrestricted: true},
	}
	return config
}

func authedSkip(config PluginConfig, id identity) (string, bool) {
	return localSkipConsumer(config, id, "my-org/qwen3-8b", markerNow())
}

func unrestrictedTier1() identity {
	return identity{State: identityResolved, Source: sourceKeys,
		AccessKey: "3192253c1f4a9b7e", UserID: 7}
}

// The whole point of the feature: an authenticated caller whose key restricts
// nothing needs no server round trip on an authed route, because there the
// server's verdict is settled by the key entry the gateway already holds.
func TestAuthedSkipAllowsAnUnrestrictedKey(t *testing.T) {
	consumer, ok := authedSkip(authedSkipConfig(), unrestrictedTier1())
	if !ok {
		t.Fatal("an unrestricted key on an authed route must skip the authorization call")
	}
	// Byte-identical to the server's join, as on the public path: ai-statistics
	// writes it into the access log beside consumers the server produced.
	if want := "3192253c1f4a9b7e.gpustack-7"; consumer != want {
		t.Errorf("consumer = %q, want %q", consumer, want)
	}
}

// The flag is the only thing standing between "the caller is authenticated" and
// "the caller is authorized". Without it the key may name a model list or lack
// inference scope, neither of which this config carries.
func TestAuthedSkipDeclines(t *testing.T) {
	expired := int64(vectorMarkerExp - 3600)

	cases := []struct {
		name   string
		config func() PluginConfig
		id     identity
		model  string
		why    string
	}{
		{
			name: "key is not flagged",
			config: func() PluginConfig {
				c := authedSkipConfig()
				c.LocalAuth.Keys = map[string]keyEntry{
					"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7},
				}
				return c
			},
			id:  unrestrictedTier1(),
			why: "an unflagged key may carry allowed_model_names or lack inference scope",
		},
		{
			name: "key is gone from the table",
			config: func() PluginConfig {
				c := authedSkipConfig()
				c.LocalAuth.Keys = map[string]keyEntry{}
				return c
			},
			id:  unrestrictedTier1(),
			why: "revocation travels as absence, and tiers 0 and 2 name a caller without reading its entry",
		},
		{
			name: "entry has expired",
			config: func() PluginConfig {
				c := authedSkipConfig()
				c.LocalAuth.Keys = map[string]keyEntry{
					"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7, Unrestricted: true, Exp: &expired},
				}
				return c
			},
			id:  unrestrictedTier1(),
			why: "a flag on a dead entry is still a dead entry",
		},
		{
			name:   "identity unresolved",
			config: authedSkipConfig,
			id:     identity{State: identityUnresolved},
			why:    "there is no key to read the flag from, and the credential still has to be authenticated",
		},
		{
			name:   "identity names nobody",
			config: authedSkipConfig,
			id:     identity{State: identityResolved, Anonymous: true},
			why:    "an authed route has no anonymous callers; treating one as authenticated would be the worst kind of hit",
		},
		{
			name:   "model header absent",
			config: authedSkipConfig,
			id:     unrestrictedTier1(),
			model:  "-",
			why:    "the route policy stands in for the server's model check only because the header selected the route",
		},
		{
			name: "policy is neither public nor authed",
			config: func() PluginConfig {
				c := authedSkipConfig()
				c.AccessPolicy = "allowed_principals"
				return c
			},
			id:  unrestrictedTier1(),
			why: "per-principal grants are not published to the gateway",
		},
		{
			name: "policy is a value this build does not know",
			config: func() PluginConfig {
				c := authedSkipConfig()
				c.AccessPolicy = "authenticated"
				return c
			},
			id:  unrestrictedTier1(),
			why: "an unrecognised policy must cost a round trip, never a verdict",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := "my-org/qwen3-8b"
			if tc.model == "-" {
				model = ""
			}
			if consumer, ok := localSkipConsumer(tc.config(), tc.id, model, markerNow()); ok {
				t.Errorf("skipped with consumer %q, but should have called the server. %s", consumer, tc.why)
			}
		})
	}
}

// Absent access_policy is the state every route without a dedicated rule is in.
// The authed skip must not leak into it through the global block, which carries
// the key table and therefore the flags.
func TestAuthedSkipNeedsADedicatedRule(t *testing.T) {
	config := authedSkipConfig()
	config.AccessPolicy = ""
	if _, ok := authedSkip(config, unrestrictedTier1()); ok {
		t.Error("a route with no policy rule skipped the authorization call")
	}
}

// The fallback pass is where this matters most: ai-proxy has replaced
// Authorization by then, so only the marker can name the caller -- and if the
// skip did not hold there, an authed route would still need a live server on
// exactly the pass that runs when something is already failing.
func TestAuthedSkipSurvivesFallbackPass(t *testing.T) {
	config := authedSkipConfig()
	fallbackHeaders := [][2]string{
		{"x-gpustack-auth-cache", vectorMarkerToken},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
		{"authorization", "Bearer gpustack_sysaksysaksysak_" + vectorSecret},
	}

	id := resolveIdentity(config, fallbackHeaders, vectorMarkerConn, markerNow())
	if id.State != identityResolved || id.Source != sourceMarker {
		t.Fatalf("tier 0 failed on the fallback pass: %+v", id)
	}
	consumer, ok := authedSkip(config, id)
	if !ok {
		t.Fatal("a marker-resolved identity on an authed route must skip when its key is unrestricted")
	}
	if consumer != vectorMarkerClaims.Consumer {
		t.Errorf("consumer = %q, want the marker's %q", consumer, vectorMarkerClaims.Consumer)
	}
}

// A marker records who the caller is, never what they may reach. Withdrawing
// the flag has to take effect on the next request even though the marker in
// hand was minted while it was still set -- otherwise narrowing a key would
// wait out its markers, and on a skipped route nothing else calls the server.
func TestAuthedSkipRereadsTheFlagOnAMarkerPass(t *testing.T) {
	config := authedSkipConfig()
	config.LocalAuth.Keys = map[string]keyEntry{
		"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7},
	}
	headers := [][2]string{
		{"x-gpustack-auth-cache", vectorMarkerToken},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}

	id := resolveIdentity(config, headers, vectorMarkerConn, markerNow())
	if id.State != identityResolved {
		t.Fatalf("precondition: the marker still names the caller, got %+v", id)
	}
	if _, ok := authedSkip(config, id); ok {
		t.Error("a marker minted under the flag kept skipping after the flag was withdrawn")
	}
}

// A refs identity carries no access key, so it can only be named by a marker or
// a cache hit -- and both are resolved identities that reach this decision. The
// flag therefore has to be readable from refs as well, or a deployment with
// GATEWAY_AUTH_ALLOW_CUSTOM_KEYS off would silently lose the skip.
func TestAuthedSkipReadsTheFlagFromRefs(t *testing.T) {
	config := authedSkipConfig()
	config.LocalAuth.Refs = map[string]refEntry{"58": {Unrestricted: true}}
	id := identity{State: identityResolved, Source: sourceCache,
		Ref: "58", Consumer: "custom-consumer.gpustack-9"}

	consumer, ok := authedSkip(config, id)
	if !ok || consumer != "custom-consumer.gpustack-9" {
		t.Errorf("ok=%v consumer=%q; an unrestricted refs identity must skip and replay its consumer", ok, consumer)
	}

	config.LocalAuth.Refs = map[string]refEntry{"58": {}}
	if _, ok := authedSkip(config, id); ok {
		t.Error("an unflagged refs entry skipped the authorization call")
	}
}

// A resolved identity with neither an access key nor a ref names nothing the
// tables can be asked about. It cannot arise today, but the lookup must not
// answer true by falling off the end of it.
func TestUnrestrictedNeedsSomethingToLookUp(t *testing.T) {
	config := authedSkipConfig()
	if config.LocalAuth.unrestricted(identity{State: identityResolved}, markerNow()) {
		t.Error("an identity with no access key and no ref was reported unrestricted")
	}
}

// The tier-1 verdict is unchanged by any of this: a known key with a wrong
// secret is still a local rejection on an authed route, flag or no flag.
func TestAuthedRouteStillRejectsABadSecretLocally(t *testing.T) {
	badSecret := [][2]string{
		{"authorization", "Bearer gpustack_3192253c1f4a9b7e_ffffffffffffffffffffffffffffffff"},
		{"x-higress-llm-model", "my-org/qwen3-8b"},
	}
	if id := resolveIdentity(authedSkipConfig(), badSecret, vectorMarkerConn, markerNow()); id.State != identityRejected {
		t.Fatalf("state = %v, want rejected", id.State)
	}
}

// The skip has to mint a marker for the same reason the public one does: the
// server never runs, so nothing else can sign the statement the fallback pass
// reads.
func TestAuthedSkipMintsAMarker(t *testing.T) {
	claims, ok := markerClaimsFor(authedSkipConfig(), unrestrictedTier1(),
		"my-org/qwen3-8b", "3192253c1f4a9b7e.gpustack-7", vectorMarkerConn)
	if !ok {
		t.Fatal("an authed local allow must still mint a marker")
	}
	if claims.ID != "ak:3192253c1f4a9b7e" {
		t.Errorf("marker id = %q", claims.ID)
	}
}

// An entry that expires between two requests must stop qualifying on the second
// one, which is only true because the flag is read against the request's clock
// rather than cached at resolution time.
func TestUnrestrictedIsAnsweredAgainstTheRequestClock(t *testing.T) {
	exp := int64(vectorMarkerExp)
	config := authedSkipConfig()
	config.LocalAuth.Keys = map[string]keyEntry{
		"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7, Unrestricted: true, Exp: &exp},
	}

	if !config.LocalAuth.unrestricted(unrestrictedTier1(), time.Unix(exp-1, 0)) {
		t.Error("a live entry was reported restricted a second before its expiry")
	}
	if config.LocalAuth.unrestricted(unrestrictedTier1(), time.Unix(exp, 0)) {
		t.Error("an expired entry was still reported unrestricted")
	}
}
