package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
)

// The verification cache remembers which identity a credential turned out to
// be, for credentials this gateway cannot verify itself -- custom keys, and
// generated keys whose digest has not been backfilled yet.
//
// It answers "who is this", never "may they". A hit still goes to the server
// for authorization while the server is up; what it removes is the
// authentication half, and with it the reason a custom key could not survive an
// outage. Because the identity is resolved, the ordinary
// failure_mode_allow_authenticated path applies -- no separate decision replay
// is needed.
//
// Unlike a decision cache this is written once per credential rather than once
// per request, read only on the paths that reach tier 2 (a key with a digest
// resolves at tier 1 and never touches shared data), and invalidated by `refs`
// membership rather than by time.

// verificationKeyPrefix namespaces the shared-data keys.
//
// `vm_id` is never set anywhere in the higress -> istio -> envoy chain, so
// shared data is one namespace per Envoy process, shared with every other wasm
// plugin running in it.
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

// lookupVerification is a variable so tests can exercise tier 2 without a
// proxy-wasm host.
var lookupVerification = func(credential string) (ref, consumer string, ok bool) {
	raw, _, err := proxywasm.GetSharedData(verificationCacheKey(credential))
	if err != nil || len(raw) == 0 {
		return "", "", false
	}
	return decodeVerification(raw)
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
	if err := proxywasm.SetSharedData(
		verificationCacheKey(credential), encodeVerification(ref, consumer), 0); err != nil {
		log.Warnf("%s: failed to cache the identity for ref %s: %v", pluginName, ref, err)
	}
}

// resolveFromVerificationCache is tier 2.
//
// The entry is trusted for the identity it names, then that identity is checked
// against `refs` by the caller exactly as a marker-derived one is -- which is
// what makes revocation take effect without any cache-invalidation step.
func resolveFromVerificationCache(config PluginConfig, requestHeaders [][2]string) (identity, bool) {
	// Nothing to validate a hit against, so a lookup could only produce an
	// identity that has to be discarded. Also keeps the host call off every
	// request in deployments that have no refs at all.
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
	return identity{
		State:    identityResolved,
		Source:   sourceCache,
		Ref:      ref,
		Consumer: consumer,
	}, true
}
