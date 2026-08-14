package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
)

// The verification cache remembers which identity a credential turned out to
// be, for credentials this gateway cannot verify itself: custom keys in a
// deployment that keeps them out of the `keys` table.
//
// It answers "who is this", never "may they". A hit still goes to the server
// for authorization while the server is up; what it removes is the
// authentication half, and with it the reason such a key could not survive an
// outage. Because the identity is resolved, the ordinary
// failure_mode_allow_authenticated path applies -- no separate decision replay
// is needed.
//
// This is the plugin's only use of shared data (markers are stateless JWTs), so
// it is the whole of its shared-data footprint. That footprint is conditional:
// it exists only where `refs` is non-empty, which is only where
// GATEWAY_AUTH_ALLOW_CUSTOM_KEYS is off. In the default configuration custom
// keys carry a digest and resolve at tier 1, `refs` is empty, and both halves
// below short-circuit before touching shared data -- the cache is inert and
// nothing is stored. It earns its keep only in the switch-off deployment, where
// it is what lets a custom key stay served through a server outage without its
// hash ever being published to the CR.
//
// Its survival guarantee is partial, and worth stating: only a credential seen
// (and vouched for) before an outage has a warm entry. One first presented
// during the outage is a miss, resolves to nothing, and is refused -- unlike a
// tier-1 key, which is always local.
//
// Unlike a decision cache this is written once per credential rather than once
// per request, read only on the paths that reach tier 2 (a key with a digest
// resolves at tier 1 and never touches shared data), and invalidated by `refs`
// membership rather than by time.
//
// Growth is the cost of that. Shared data has no TTL and cannot be enumerated
// or deleted (only overwritten), so an entry for a credential never presented
// again cannot be reclaimed until the pod restarts. The bound is the number of
// distinct valid switch-off custom credentials a pod sees in its lifetime --
// not per-request, and zero in the default configuration -- and the live
// working set within that is itself bounded by the `refs` byte budget in the
// CR. evictVerification claws back only the one case the ABI permits: an entry
// whose key was revoked and is still being presented.

// verificationKeyPrefix namespaces the shared-data keys.
//
// `vm_id` is never set anywhere in the higress -> istio -> envoy chain, so
// shared data is one namespace per Envoy process, shared with every other wasm
// plugin running in it. There is no configuration escape: `buildVMConfig` in
// istiod sets only Runtime, Code and EnvironmentVariables, and the WasmPlugin
// API exposes no `vm_id` at all.
//
// Entries are therefore unauthenticated, and deliberately so. Authenticating
// them -- a MAC over the value, or deriving the key with `auth_cache.signing_key`
// so it cannot be computed -- would defend against a co-resident plugin writing
// an entry that names someone else. But every way of becoming that co-resident
// plugin already carries something stronger:
//
//   - installing a WasmPlugin needs write access to the gateway namespace,
//     which comes with read access to this plugin's own CR, and
//     `auth_cache.signing_key` is in it -- markers can then be forged outright;
//   - substituting or compromising a module that is already installed gives
//     code execution inside the sandbox, and any filter on the chain can read
//     every request's headers, i.e. harvest API keys in plaintext.
//
// A MAC would be hardening against an attacker who has already won by a
// shorter route, so the cost of the check buys nothing. What it would still
// catch is narrow: a legitimately installed plugin with a bug that lets an
// attacker steer both the key and the value it writes.
const verificationKeyPrefix = "gpustack-ext-auth:v:"

// verificationCacheKey indexes an entry by the credential's digest.
//
// Never by the plaintext. What is being avoided is residency: the credential is
// already in the request headers for the life of the request, but a cache would
// hold it for as long as the process runs. SHA-256 does not collide, so a hit is
// trustworthy without a second check.
func verificationCacheKey(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return verificationKeyPrefix + hex.EncodeToString(sum[:])
}

// encodeVerification renders an entry as `<ref>|<consumer>`.
//
// The ref leads and the split takes the first separator only, so a consumer
// containing a pipe cannot corrupt it. A ref containing one would be truncated
// and then fail its refs lookup, which fails safe.
func encodeVerification(ref, consumer string) []byte {
	return []byte(ref + "|" + consumer)
}

func decodeVerification(raw []byte) (ref, consumer string, ok bool) {
	// One conversion, then substrings of it: slicing a string shares the backing
	// array, where converting each half separately would copy both again.
	entry := string(raw)
	separator := strings.IndexByte(entry, '|')
	if separator <= 0 {
		return "", "", false
	}
	return entry[:separator], entry[separator+1:], true
}

// lookupVerification and writeVerification are variables so tests can exercise
// tier 2 -- reads, writes and evictions -- without a proxy-wasm host. Both host
// calls in this file go through them.
var lookupVerification = func(credential string) (ref, consumer string, ok bool) {
	raw, _, err := proxywasm.GetSharedData(verificationCacheKey(credential))
	if err != nil || len(raw) == 0 {
		return "", "", false
	}
	return decodeVerification(raw)
}

var writeVerification = func(key string, value []byte) error {
	return proxywasm.SetSharedData(key, value, 0)
}

// storeVerification records what the server said a credential was.
//
// Only identities that can be looked back up are cached. Writing one that is
// absent from `refs` would make the credential slower than not caching it at
// all -- every request would hit, fail the re-check, and go to the server
// anyway. The case this guards is a key created moments ago whose config push
// has not landed: it simply keeps going to the server until it has, then starts
// hitting, with no "works once then breaks" window.
func storeVerification(config PluginConfig, credential, ref, consumer string) {
	if !config.LocalAuth.Enabled || credential == "" || ref == "" || consumer == "" {
		return
	}
	if _, live := config.LocalAuth.Refs[ref]; !live {
		return
	}
	if err := writeVerification(
		verificationCacheKey(credential), encodeVerification(ref, consumer)); err != nil {
		log.Warnf("%s: failed to cache the identity for ref %s: %v", pluginName, ref, err)
	}
}

// evictVerification tombstones a cache entry whose identity `refs` no longer
// vouches for.
//
// Called on a failed re-check, where the credential is in hand and the entry is
// known dead -- the one reclamation this ABI allows, since shared data cannot
// be enumerated to sweep later. The value is cleared rather than the entry
// deleted (there is no delete, and an empty value is what lookupVerification
// reads as a miss); the key string itself stays, so this reclaims the value of
// a revoked-and-still-presented entry, not its footprint entirely. Once cleared
// it is not re-cached: the next presentation is a miss, goes to the server, and
// the server refuses a revoked key rather than returning a ref.
func evictVerification(credential string) {
	if credential == "" {
		return
	}
	if err := writeVerification(verificationCacheKey(credential), nil); err != nil {
		log.Warnf("%s: failed to evict a stale verification entry: %v", pluginName, err)
	}
}

// resolveFromVerificationCache is tier 2.
//
// The entry is trusted for the identity it names, then that identity is checked
// against `refs` here -- exactly as the marker path checks its own -- which is
// what makes revocation take effect without any time-based invalidation. A
// failed re-check evicts the dead entry and falls through to unresolved,
// leaving the server authoritative.
func resolveFromVerificationCache(config PluginConfig, requestHeaders [][2]string, now time.Time) (identity, bool) {
	// Nothing to validate a hit against, so a lookup could only produce an
	// identity that has to be discarded. Also keeps the host call off every
	// request in deployments that have no refs at all -- i.e. every deployment
	// that has not turned GATEWAY_AUTH_ALLOW_CUSTOM_KEYS off.
	if len(config.LocalAuth.Refs) == 0 {
		return identity{}, false
	}
	credential := extractCredential(requestHeaders)
	if credential == "" {
		return identity{}, false
	}
	ref, consumer, ok := lookupVerification(credential)
	if !ok || ref == "" || consumer == "" {
		return identity{}, false
	}
	// A cache entry names a ref, never an access key -- the credential behind it
	// is unverifiable here, which is why it went to refs rather than keys.
	if !identityStillValid(config, "", ref, now) {
		evictVerification(credential)
		return identity{}, false
	}
	return identity{
		State:    identityResolved,
		Source:   sourceCache,
		Ref:      ref,
		Consumer: consumer,
	}, true
}
