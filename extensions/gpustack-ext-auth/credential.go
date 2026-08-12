package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"time"
)

// apiKeyPrefix is the literal that opens every GPUStack API key:
// `gpustack_<access_key>_<secret_key>`. Mirrors API_KEY_PREFIX in
// gpustack/security.py.
const apiKeyPrefix = "gpustack"

// digestAlgorithm is the only `secret_key_digest` construction this build can
// check. The algorithm name is the value's own prefix (the convention argon2
// uses for `hashed_secret_key`), so introducing a second construction means
// adding a prefix here rather than versioning the column.
const digestAlgorithm = "sha256"

// parseAPIKey splits a credential of the form `gpustack_<ak>_<sk>`.
//
// Deliberately identical to is_valid_format in gpustack/security.py, including
// the split limit: the secret is everything after the second underscore, so a
// secret containing underscores survives. Anything that does not fit the shape
// is not ours to classify -- the caller treats it as an unresolved identity and
// lets the server decide (it may be a custom key, a legacy UUID token, or a
// credential for some other scheme entirely).
//
// No length validation, on purpose. A user-supplied custom key can occupy a
// 16-hex-character access key, so length cannot distinguish generated keys from
// custom ones; eligibility is the server's call, expressed by whether the key
// appears in the `keys` table at all.
func parseAPIKey(key string) (accessKey, secretKey string, ok bool) {
	if !strings.HasPrefix(key, apiKeyPrefix+"_") {
		return "", "", false
	}
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// extractCredential pulls the client credential out of the request headers.
//
// Only the two carriers that can hold an API key are considered. Cookie and
// basic credentials are intentionally absent: neither has an API key behind it,
// so neither can ever be resolved locally -- they fall through to the server,
// which holds the JWT secret and the password hashes.
func extractCredential(headers [][2]string) string {
	if authorization := extractHeader(headers, headerAuthorization); authorization != "" {
		const bearer = "bearer "
		if len(authorization) > len(bearer) && strings.EqualFold(authorization[:len(bearer)], bearer) {
			return strings.TrimSpace(authorization[len(bearer):])
		}
		return ""
	}
	return extractHeader(headers, headerXAPIKey)
}

// parseSecretKeyDigest splits a stored `sha256$<salt>$<hash>` value.
//
// ok is false for a missing, malformed or unknown-algorithm value. That is not
// the same as a mismatch: it says the digest tells us nothing, so the caller
// must fall through to the server rather than reject. Rejecting on an unusable
// digest would lock out a valid key on bad column data and would make adding a
// digest algorithm a breaking change for older gateways.
func parseSecretKeyDigest(digest string) (salt, expected string, ok bool) {
	parts := strings.Split(digest, "$")
	if len(parts) != 3 {
		return "", "", false
	}
	algorithm, salt, expected := parts[0], parts[1], parts[2]
	if algorithm != digestAlgorithm || salt == "" || expected == "" {
		return "", "", false
	}
	return salt, expected, true
}

// verifySecretKeyDigest checks a plaintext secret against a stored digest.
//
// The hashed input is the salt's own characters followed by the secret, with no
// separator -- byte-for-byte what _digest_hash in gpustack/security.py does.
// Note the salt contributes its *hex text*, not the bytes that text encodes;
// decoding it here would produce a different hash and silently reject every
// key.
//
// usable distinguishes "this digest says no" from "this digest says nothing";
// only the former is grounds for rejecting the request.
func verifySecretKeyDigest(digest, secretKey string) (match, usable bool) {
	salt, expected, ok := parseSecretKeyDigest(digest)
	if !ok {
		return false, false
	}
	sum := sha256.Sum256(append([]byte(salt), secretKey...))
	actual := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1, true
}

// keyEntry is one row of the `keys` table: everything needed to authenticate a
// generated key locally and to rebuild its consumer string without asking the
// server.
type keyEntry struct {
	// Exp is a Unix epoch second, nil for "never expires". An integer rather
	// than RFC 3339 so the check is a comparison, not a date parse in wasm.
	Exp *int64 `json:"exp"`
	// Digest is the `sha256$<salt>$<hash>` verifier. A key without one does not
	// belong in this table -- it belongs in refs.
	Digest string `json:"digest"`
	// UserID backs the locally rebuilt consumer string. Cheaper than storing
	// the assembled consumer on the largest structure in the config.
	UserID int64 `json:"user_id"`
}

// expired reports whether the entry's lifetime has run out as of now.
//
// The clock is the gateway's, not the server's. Skew between them shifts the
// expiry instant by the same amount -- acceptable because skew is orders of
// magnitude below any key's lifetime, but it is a dependency this plugin did
// not previously have.
func (e keyEntry) expired(now time.Time) bool {
	return e.Exp != nil && now.Unix() >= *e.Exp
}

// refEntry is one row of the `refs` table: a validity index only. It cannot
// authenticate anything -- it confirms that an identity obtained some other way
// is still live. Keyed by api_keys.id, so unlike keyEntry it carries no
// user_id: a custom key's consumer embeds its access_key, which this table does
// not have.
type refEntry struct {
	Exp *int64 `json:"exp"`
}

func (e refEntry) expired(now time.Time) bool {
	return e.Exp != nil && now.Unix() >= *e.Exp
}
