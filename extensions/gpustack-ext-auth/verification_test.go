package main

import (
	"strings"
	"testing"
	"time"
)

// stubVerificationCache swaps the host-backed lookup for an in-memory one and
// restores it afterwards, so tier 2 can be exercised without a proxy-wasm host.
func stubVerificationCache(t *testing.T, entries map[string][2]string) {
	t.Helper()
	original := lookupVerification
	lookupVerification = func(credential string) (string, string, bool) {
		entry, ok := entries[credential]
		if !ok {
			return "", "", false
		}
		return entry[0], entry[1], true
	}
	t.Cleanup(func() { lookupVerification = original })
}

func TestVerificationCacheKey(t *testing.T) {
	key := verificationCacheKey("gpustack_3192253c1f4a9b7e_" + vectorSecret)

	if !strings.HasPrefix(key, "gpustack-ext-auth:") {
		t.Errorf("key %q is not namespaced; shared data is one namespace per Envoy process", key)
	}
	// The credential must not survive in the key. It is already in the request
	// headers for the life of the request; a cache would hold it for the life of
	// the process.
	if strings.Contains(key, vectorSecret) {
		t.Error("the key embeds the plaintext credential")
	}
	if key == verificationCacheKey("gpustack_3192253c1f4a9b7e_ffffffffffffffffffffffffffffffff") {
		t.Error("two credentials produced the same key")
	}
}

func TestVerificationRoundTrip(t *testing.T) {
	for _, tc := range []struct{ ref, consumer string }{
		{"58", "3192253c1f4a9b7e.gpustack-7"},
		{"58", "none"},
		{"1", "consumer|with|pipes"},
	} {
		ref, consumer, ok := decodeVerification(encodeVerification(tc.ref, tc.consumer))
		if !ok || ref != tc.ref || consumer != tc.consumer {
			t.Errorf("got (%q, %q, %v), want (%q, %q, true)", ref, consumer, ok, tc.ref, tc.consumer)
		}
	}
}

func TestDecodeVerificationRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "no-separator", "|consumer"} {
		if _, _, ok := decodeVerification([]byte(raw)); ok {
			t.Errorf("%q was accepted as a cache entry", raw)
		}
	}
}

func tier2Config() PluginConfig {
	return PluginConfig{
		LocalAuth: LocalAuth{
			Enabled: true,
			Keys:    map[string]keyEntry{"3192253c1f4a9b7e": {Digest: vectorDigest, UserID: 7}},
			Refs:    map[string]refEntry{"58": {}},
		},
	}
}

const customCredential = "sk-a-user-supplied-key"

func TestTier2ResolvesACustomKey(t *testing.T) {
	stubVerificationCache(t, map[string][2]string{
		customCredential: {"58", "abc123.gpustack-9"},
	})

	id := resolveIdentity(tier2Config(), [][2]string{
		{"authorization", "Bearer " + customCredential},
	}, vectorMarkerConn, time.Unix(1_790_000_000, 0))

	if id.State != identityResolved {
		t.Fatalf("state = %v, want resolved", id.State)
	}
	if id.Ref != "58" || id.Consumer != "abc123.gpustack-9" || id.Source != sourceCache {
		t.Errorf("id = %+v", id)
	}
}

// The point of tier 2: once the server has named a custom key, an outage no
// longer takes it down. Authorization reuses the existing flag -- no separate
// decision replay.
func TestTier2SurvivesAnOutage(t *testing.T) {
	stubVerificationCache(t, map[string][2]string{
		customCredential: {"58", "abc123.gpustack-9"},
	})
	config := tier2Config()
	config.FailureModeAllowAuthenticated = true

	id := resolveIdentity(config, [][2]string{
		{"authorization", "Bearer " + customCredential},
	}, vectorMarkerConn, time.Unix(1_790_000_000, 0))

	if !failureModeAllows(config, id, 503) {
		t.Error("a cached custom key was rejected while the server was unreachable")
	}
}

// stubVerificationWrites records host writes (store and evict) so tests can
// assert on them without a proxy-wasm host. Returns the recorder.
func stubVerificationWrites(t *testing.T) *map[string][]byte {
	t.Helper()
	writes := map[string][]byte{}
	original := writeVerification
	writeVerification = func(key string, value []byte) error {
		writes[key] = value
		return nil
	}
	t.Cleanup(func() { writeVerification = original })
	return &writes
}

// Revocation needs no cache-invalidation step: dropping the ref from `refs` is
// enough, because a hit is always re-checked against it. And a hit whose ref is
// gone evicts its own dead entry -- the one reclamation the shared-data ABI
// allows.
func TestTier2HitIsRecheckedAgainstRefs(t *testing.T) {
	stubVerificationCache(t, map[string][2]string{
		customCredential: {"58", "abc123.gpustack-9"},
	})
	writes := stubVerificationWrites(t)
	credential := [][2]string{{"authorization", "Bearer " + customCredential}}
	now := time.Unix(1_790_000_000, 0)

	config := tier2Config()
	if resolveIdentity(config, credential, vectorMarkerConn, now).State != identityResolved {
		t.Fatal("precondition: a live ref must resolve")
	}

	// Refs empty: the lookup is skipped entirely, so there is nothing to evict.
	config.LocalAuth.Refs = map[string]refEntry{}
	if id := resolveIdentity(config, credential, vectorMarkerConn, now); id.State != identityUnresolved {
		t.Error("a revoked ref still resolved from the cache")
	}
	if len(*writes) != 0 {
		t.Errorf("empty refs must not reach the host, got writes %v", *writes)
	}

	// Ref present but expired: the entry is dead, and presenting it evicts it.
	expired := now.Unix() - 1
	config.LocalAuth.Refs = map[string]refEntry{"58": {Exp: &expired}}
	if id := resolveIdentity(config, credential, vectorMarkerConn, now); id.State != identityUnresolved {
		t.Error("an expired ref still resolved from the cache")
	}
	key := verificationCacheKey(customCredential)
	if value, ok := (*writes)[key]; !ok || len(value) != 0 {
		t.Errorf("a dead entry must be tombstoned to empty, got (%q, present=%v)", value, ok)
	}
}

// Tier 1 answers first, so a key with a digest never pays a shared-data lookup.
func TestTier1TakesPrecedenceOverTheCache(t *testing.T) {
	consulted := false
	original := lookupVerification
	lookupVerification = func(string) (string, string, bool) {
		consulted = true
		return "", "", false
	}
	t.Cleanup(func() { lookupVerification = original })

	id := resolveIdentity(tier2Config(), [][2]string{
		{"authorization", "Bearer gpustack_3192253c1f4a9b7e_" + vectorSecret},
	}, vectorMarkerConn, time.Unix(1_790_000_000, 0))

	if id.Source != sourceKeys {
		t.Errorf("source = %v, want the key table", id.Source)
	}
	if consulted {
		t.Error("a digest-bearing key reached the verification cache")
	}
}

// With no refs there is nothing a hit could be validated against, so the host
// must not be touched at all.
func TestEmptyRefsSkipsTheLookup(t *testing.T) {
	consulted := false
	original := lookupVerification
	lookupVerification = func(string) (string, string, bool) {
		consulted = true
		return "58", "c", true
	}
	t.Cleanup(func() { lookupVerification = original })

	config := tier2Config()
	config.LocalAuth.Refs = map[string]refEntry{}
	resolveIdentity(config, [][2]string{{"authorization", "Bearer " + customCredential}},
		vectorMarkerConn, time.Unix(1_790_000_000, 0))

	if consulted {
		t.Error("the cache was consulted with no refs to validate against")
	}
}

// A freshly created key whose config push has not landed must not be cached:
// it would hit, fail the re-check, and go to the server anyway -- slower than
// not caching, and it would look like "works once then breaks".
func TestStoreVerificationRequiresAKnownRef(t *testing.T) {
	config := tier2Config()

	// Reaching a host call would panic here, so returning is the assertion.
	storeVerification(config, customCredential, "999", "abc123.gpustack-9")
	storeVerification(config, customCredential, "", "abc123.gpustack-9")
	storeVerification(config, customCredential, "58", "")
	storeVerification(config, "", "58", "abc123.gpustack-9")

	disabled := config
	disabled.LocalAuth.Enabled = false
	storeVerification(disabled, customCredential, "58", "abc123.gpustack-9")
}

// The positive case: a live ref is written under the credential's digest key,
// as `<ref>|<consumer>`.
func TestStoreVerificationWritesALiveRef(t *testing.T) {
	writes := stubVerificationWrites(t)

	storeVerification(tier2Config(), customCredential, "58", "abc123.gpustack-9")

	key := verificationCacheKey(customCredential)
	if got := string((*writes)[key]); got != "58|abc123.gpustack-9" {
		t.Errorf("stored %q, want %q", got, "58|abc123.gpustack-9")
	}
}
