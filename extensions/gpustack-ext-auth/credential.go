package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

// customKeyHashBytes is the digest size gpustack's custom_key_hash uses.
const customKeyHashBytes = 16

// apiKeyPrefix is the literal that opens every GPUStack API key:
// `gpustack_<access_key>_<secret_key>`. Mirrors API_KEY_PREFIX in
// gpustack/security.py.
const apiKeyPrefix = "gpustack"

// The `secret_key_digest` constructions this build can check. The algorithm
// name is the value's own prefix (the convention argon2 uses for
// `hashed_secret_key`), so a new construction is a new prefix rather than a
// version column.
//
// The two differ only in how the expected hash is written down; the hashed
// input is identical, so digestTruncated is derived from a stored digest
// rather than replacing it -- the server keeps the long form in its database
// and shortens it on the way into this config.
//
// That matters because this table ships inside a WasmPlugin CR, and a CR lives
// in etcd under a ~1.5 MiB object limit: 60 characters against 104 is the
// difference between roughly 6000 and 8600 keys authenticating here instead of
// at the server. Deriving rather than restating also means existing keys
// shrink on the next reconcile, with no migration -- the plaintext needed to
// rewrite a stored digest stops reaching the server once its key is
// authenticated here.
//
// The prefix is what keeps a rollback safe. An older gateway reading a value
// it cannot name falls through to the server; one that recognised `sha256` and
// then compared a truncated hash against a full one would reject outright,
// which is a 401 rather than a slower path.
const (
	// `sha256$<32 hex salt>$<64 hex hash>` -- as the server stores it.
	digestAlgorithm = "sha256"
	// `s128$<salt>$<b64url hash>`: the same salt text, and the same hash
	// truncated to its leading 16 bytes and base64url-encoded without padding.
	//
	// 128 bits is not a compromise, it is the matching length: the secret this
	// verifies is 128 bits of CSPRNG output, so finding a second preimage
	// (2^128) costs exactly what guessing the secret outright costs. The
	// discarded half was protecting nothing.
	//
	// Deliberately not configurable. The length is pinned to the secret's own
	// entropy, so exposing it would turn a constant that has to be reasoned
	// about into a knob that can be set to 64 -- which still works, silently,
	// at 2^64.
	digestTruncated = "s128"
)

// truncatedDigestBytes is the length behind digestTruncated's name; the two
// must agree, since the name is what tells a reader how the value was built.
const truncatedDigestBytes = 16

// parseAPIKey derives the pair a credential is indexed and verified by.
//
// Deliberately identical to get_key_pair in gpustack/security.py, both branches
// of it. A credential shaped `gpustack_<ak>_<sk>` splits on the first two
// underscores, so a secret containing underscores survives. Anything else is a
// custom key, whose access key the server derives by hashing the whole string --
// the credential *is* the secret in that case, and there is no embedded access
// key to read.
//
// The second branch is what lets a custom key be authenticated here at all. Its
// absence would not be visible as an error: the lookup would simply never find
// an entry, and every request carrying such a key would go to the server --
// including while the server is the thing that is down, which is the case this
// plugin exists for.
//
// No length validation, on purpose. A user-supplied custom key can occupy a
// 16-hex-character access key, so length cannot distinguish generated keys from
// custom ones; eligibility is the server's call, expressed by whether the key
// appears in the `keys` table at all.
func parseAPIKey(key string) (accessKey, secretKey string) {
	if strings.HasPrefix(key, apiKeyPrefix+"_") {
		if parts := strings.SplitN(key, "_", 3); len(parts) == 3 {
			return parts[1], parts[2]
		}
	}
	return customKeyHash(key), key
}

// customKeyHash mirrors custom_key_hash in gpustack/security.py: blake2b with a
// 16-byte digest, rendered as hex.
//
// Unkeyed and fast, which is the server's choice rather than this plugin's --
// what matters here is only that the two agree byte for byte, since this value
// is the key the `keys` table is indexed by.
func customKeyHash(key string) string {
	sum, err := blake2b.New(customKeyHashBytes, nil)
	if err != nil {
		return ""
	}
	sum.Write([]byte(key))
	return hex.EncodeToString(sum.Sum(nil))
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

// encodedHashLen is how long the expected hash is written under a given
// construction, and the only thing about the value this build checks beyond its
// algorithm name.
//
// Length is a property of the construction, not of the secret, so a value that
// gets it wrong was not produced by the construction it claims -- which makes it
// bad column data rather than a wrong secret. See parseSecretKeyDigest for why
// the distinction decides between a 401 and a slower path.
func encodedHashLen(algorithm string) int {
	if algorithm == digestTruncated {
		return base64.RawURLEncoding.EncodedLen(truncatedDigestBytes)
	}
	return hex.EncodedLen(sha256.Size)
}

// parseSecretKeyDigest splits a stored digest into its algorithm, salt and
// expected hash.
//
// ok is false for a missing, malformed or unknown-algorithm value. That is not
// the same as a mismatch: it says the digest tells us nothing, so the caller
// must fall through to the server rather than reject. Rejecting on an unusable
// digest would lock out a valid key on bad column data and would make adding a
// digest algorithm a breaking change for older gateways.
//
// The hash's length is checked here for that reason, and not left to the
// comparison in verifySecretKeyDigest. A hash of the wrong length can never
// equal ours, so leaving it out would still reject the request -- but it would
// reject it as a wrong secret, permanently, instead of asking the server. The
// hex form was close to self-protecting here (there is one way to spell
// `hexdigest()`); the truncated form is not, because it adds an encoding step on
// the server side, and padded-vs-raw base64url is exactly the variant that gets
// picked wrong once and locks out every key that carries it.
// Cut by hand rather than with strings.Split, which would allocate a slice on
// every authenticated request to hand back three strings that are already
// substrings of the input. The three fields returned here share the caller's
// backing array; nothing is copied. TestDigestVerificationDoesNotAllocate
// exists to keep a later simplification back to Split from going unnoticed.
func parseSecretKeyDigest(digest string) (algorithm, salt, expected string, ok bool) {
	sep := strings.IndexByte(digest, '$')
	if sep < 0 {
		return "", "", "", false
	}
	algorithm, rest := digest[:sep], digest[sep+1:]
	sep = strings.IndexByte(rest, '$')
	if sep < 0 {
		return "", "", "", false
	}
	salt, expected = rest[:sep], rest[sep+1:]
	// A third separator means this is not a three-field value, which Split's
	// len(parts) != 3 used to catch. A salt or hash that has grown a `$` is
	// not something to guess at.
	if strings.IndexByte(expected, '$') >= 0 {
		return "", "", "", false
	}
	if salt == "" {
		return "", "", "", false
	}
	if algorithm != digestAlgorithm && algorithm != digestTruncated {
		return "", "", "", false
	}
	if len(expected) != encodedHashLen(algorithm) {
		return "", "", "", false
	}
	return algorithm, salt, expected, true
}

// verifySecretKeyDigest checks a plaintext secret against a stored digest.
//
// The hashed input is the salt's own characters followed by the secret, with no
// separator -- byte-for-byte what _digest_hash in gpustack/security.py does,
// and identical for both constructions. Note the salt contributes its *encoded
// text*, not the bytes that text encodes; decoding it here would produce a
// different hash and silently reject every key.
//
// usable distinguishes "this digest says no" from "this digest says nothing";
// only the former is grounds for rejecting the request.
//
// Both buffers are sized so the whole check stays on the stack: this runs once
// per authenticated request, and a garbage-collected allocation is worth more
// inside a wasm VM than the same one on the host.
func verifySecretKeyDigest(digest, secretKey string) (match, usable bool) {
	algorithm, salt, expected, ok := parseSecretKeyDigest(digest)
	if !ok {
		return false, false
	}

	// A generated key is a 32-character salt and a 32-character secret, so the
	// fixed buffer covers every value this table can hold. The secret still
	// arrives from the client and parseAPIKey deliberately does not bound its
	// length, so an over-long one has to remain answerable -- it just answers
	// from the heap, and answers no.
	const hashInputBuf = 128
	var stack [hashInputBuf]byte
	var input []byte
	if len(salt)+len(secretKey) <= hashInputBuf {
		n := copy(stack[:], salt)
		n += copy(stack[n:], secretKey)
		input = stack[:n]
	} else {
		input = append([]byte(salt), secretKey...)
	}
	sum := sha256.Sum256(input)

	// 2*sha256.Size is the hex form, the longer of the two encodings; the
	// truncated form needs 22 of those bytes. encodedHashLen is what says how
	// much of the buffer is live, so the two cannot drift apart.
	var encoded [2 * sha256.Size]byte
	if algorithm == digestTruncated {
		base64.RawURLEncoding.Encode(encoded[:], sum[:truncatedDigestBytes])
	} else {
		hex.Encode(encoded[:], sum[:])
	}
	actual := encoded[:encodedHashLen(algorithm)]

	return subtle.ConstantTimeCompare(actual, []byte(expected)) == 1, true
}

// keyEntry is one row of the `keys` table: everything needed to authenticate a
// generated key locally and to rebuild its consumer string without asking the
// server.
type keyEntry struct {
	// Exp is a Unix epoch second, nil for "never expires". An integer rather
	// than RFC 3339 so the check is a comparison, not a date parse in wasm.
	Exp *int64 `json:"exp"`
	// Digest is the `<algorithm>$<salt>$<hash>` verifier. A key without one
	// does not belong in this table -- it belongs in refs.
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
