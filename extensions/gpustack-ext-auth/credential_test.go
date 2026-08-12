package main

import (
	"testing"
	"time"
)

// Produced by the server's own construction, so a change on either side breaks
// this rather than breaking authentication in production:
//
//	salt   = "4f3c2a1b9e8d7c6b5a4938271605f4e3"
//	secret = "c11c75ed6334ea9505da4ad9c11c75ed"
//	hashlib.sha256(salt.encode() + secret.encode()).hexdigest()
//
// The salt contributes its hex *text*, not the 16 bytes that text encodes.
// Decoding it first is the obvious-looking mistake and yields a digest that
// never matches.
const (
	vectorSalt   = "4f3c2a1b9e8d7c6b5a4938271605f4e3"
	vectorSecret = "c11c75ed6334ea9505da4ad9c11c75ed"
	vectorDigest = "sha256$4f3c2a1b9e8d7c6b5a4938271605f4e3$7ca1547a67b46a04ee5dc4ff669ae460680a8f82e1714a679c086cc401b5748f"
)

// The truncated form of that same value, which is what the server puts in the
// config:
//
//	base64url(bytes.fromhex(hash)[:16])
//
// Same salt, same hashed input, same hash -- only the last 16 bytes of the
// expected value are dropped and the remainder re-encoded. Nothing is
// recomputed, which is why an existing key can be shortened without its
// plaintext.
const truncatedVectorDigest = "s128$4f3c2a1b9e8d7c6b5a4938271605f4e3$fKFUeme0agTuXcT_ZprkYA"

func TestVerifyTruncatedSecretKeyDigestMatchesServerVector(t *testing.T) {
	match, usable := verifySecretKeyDigest(truncatedVectorDigest, vectorSecret)
	if !usable {
		t.Fatalf("truncated digest reported unusable; the value format diverged from the server")
	}
	if !match {
		t.Fatalf("truncated digest did not verify against the secret it was built from")
	}
}

// Both constructions have to keep working at once: a row carries whichever the
// server wrote when the key was created, and existing rows are never rewritten
// -- the plaintext needed to do so only passes through the server on the paths
// this plugin has taken over.
func TestBothDigestConstructionsCoexist(t *testing.T) {
	for name, digest := range map[string]string{
		"hex":       vectorDigest,
		"truncated": truncatedVectorDigest,
	} {
		if match, usable := verifySecretKeyDigest(digest, vectorSecret); !match || !usable {
			t.Errorf("%s: match=%v usable=%v, want both true", name, match, usable)
		}
		if match, _ := verifySecretKeyDigest(digest, "0000000000000000c11c75ed6334ea95"); match {
			t.Errorf("%s: a wrong secret verified", name)
		}
	}
}

// A hash written at the wrong length for the algorithm it is filed under means
// the server and this build disagree about the construction. That must never
// verify -- truncation is the whole point of the compact form, so a prefix
// comparison would silently accept a shorter hash than the name promises.
//
// It must also come back *unusable* rather than merely unequal. Every case here
// is a value the construction could not have produced, which makes it bad column
// data: the server is the one that can still authenticate the key, so the
// request goes there. Reporting a mismatch instead would spend a permanent 401
// on what may well be a valid key -- and on the padded-base64 line, on every key
// at once.
func TestWrongLengthDigestIsUnusableNotAMismatch(t *testing.T) {
	padded := "s128$" + vectorSalt + "$fKFUeme0agTuXcT_ZprkYA=="
	fullHashUnderCompactName := "s128$" + vectorSalt + "$7ca1547a67b46a04ee5dc4ff669ae460680a8f82e1714a679c086cc401b5748f"
	truncatedHashUnderHexName := "sha256$" + vectorSalt + "$fKFUeme0agTuXcT_ZprkYA"

	for name, digest := range map[string]string{
		"padded base64":                 padded,
		"full hash under compact name":  fullHashUnderCompactName,
		"truncated hash under hex name": truncatedHashUnderHexName,
		"over-long":                     truncatedVectorDigest + "extra",
		"empty hash":                    "s128$" + vectorSalt + "$",
	} {
		match, usable := verifySecretKeyDigest(digest, vectorSecret)
		if match {
			t.Errorf("%s: verified against the secret it was not built for", name)
		}
		if usable {
			t.Errorf("%s: reported usable, so a valid key would be rejected instead of sent to the server", name)
		}
	}
}

// Verification runs once per authenticated request, inside a wasm VM where the
// garbage it makes is more expensive than the same garbage on the host. Both
// functions are written to keep the salt, the hash and its encoding on the
// stack, which is not something a reader would infer from the code -- the
// hand-rolled `$` scan in particular reads like a strings.Split that someone
// forgot to simplify. This is what says otherwise, and what fails if anyone
// does simplify it.
//
// A digest that carries an over-long secret is deliberately not covered: that
// path falls back to the heap on purpose, since the secret comes from the
// client and is not bounded.
func TestDigestVerificationDoesNotAllocate(t *testing.T) {
	for name, digest := range map[string]string{
		"hex":       vectorDigest,
		"truncated": truncatedVectorDigest,
	} {
		allocs := testing.AllocsPerRun(100, func() {
			verifySecretKeyDigest(digest, vectorSecret)
		})
		if allocs != 0 {
			t.Errorf("%s: %v allocations per verification, want 0", name, allocs)
		}
	}
}

func TestVerifySecretKeyDigestMatchesServerVector(t *testing.T) {
	match, usable := verifySecretKeyDigest(vectorDigest, vectorSecret)
	if !usable {
		t.Fatalf("digest reported unusable; the value format diverged from the server")
	}
	if !match {
		t.Fatalf("digest did not verify against the secret it was built from")
	}
}

func TestVerifySecretKeyDigest(t *testing.T) {
	cases := []struct {
		name        string
		digest      string
		secret      string
		wantMatch   bool
		wantUsable  bool
		explanation string
	}{
		{
			name: "wrong secret", digest: vectorDigest, secret: "0000000000000000c11c75ed6334ea95",
			wantMatch: false, wantUsable: true,
			explanation: "a usable digest that says no is the one case we may reject on",
		},
		{
			name: "empty digest", digest: "", secret: vectorSecret,
			wantMatch: false, wantUsable: false,
		},
		{
			name: "unknown algorithm", digest: "blake2b$" + vectorSalt + "$deadbeef", secret: vectorSecret,
			wantMatch: false, wantUsable: false,
			explanation: "an algorithm a future server introduces must not lock this gateway out",
		},
		{
			name: "too few fields", digest: "sha256$" + vectorSalt, secret: vectorSecret,
			wantMatch: false, wantUsable: false,
		},
		{
			name: "empty salt", digest: "sha256$$deadbeef", secret: vectorSecret,
			wantMatch: false, wantUsable: false,
		},
		{
			name: "empty hash", digest: "sha256$" + vectorSalt + "$", secret: vectorSecret,
			wantMatch: false, wantUsable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, usable := verifySecretKeyDigest(tc.digest, tc.secret)
			if match != tc.wantMatch || usable != tc.wantUsable {
				t.Errorf("got (match=%v, usable=%v), want (match=%v, usable=%v). %s",
					match, usable, tc.wantMatch, tc.wantUsable, tc.explanation)
			}
		})
	}
}

func TestParseAPIKey(t *testing.T) {
	cases := []struct {
		name          string
		key           string
		wantAccessKey string
		wantSecretKey string
		wantOK        bool
	}{
		{
			name: "standard", key: "gpustack_3192253c1f4a9b7e_" + vectorSecret,
			wantAccessKey: "3192253c1f4a9b7e", wantSecretKey: vectorSecret, wantOK: true,
		},
		{
			name: "secret containing underscores", key: "gpustack_3192253c1f4a9b7e_a_b_c",
			wantAccessKey: "3192253c1f4a9b7e", wantSecretKey: "a_b_c", wantOK: true,
		},
		{name: "no prefix", key: "sk-abcdef", wantOK: false},
		{name: "prefix only", key: "gpustack_", wantOK: false},
		{name: "two parts", key: "gpustack_abcdef", wantOK: false},
		{name: "empty", key: "", wantOK: false},
		{
			name: "prefix must be followed by underscore", key: "gpustackfoo_a_b",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accessKey, secretKey, ok := parseAPIKey(tc.key)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if accessKey != tc.wantAccessKey || secretKey != tc.wantSecretKey {
				t.Errorf("got (%q, %q), want (%q, %q)", accessKey, secretKey, tc.wantAccessKey, tc.wantSecretKey)
			}
		})
	}
}

func TestExtractCredential(t *testing.T) {
	cases := []struct {
		name    string
		headers [][2]string
		want    string
	}{
		{
			name:    "bearer",
			headers: [][2]string{{"authorization", "Bearer gpustack_ak_sk"}},
			want:    "gpustack_ak_sk",
		},
		{
			name:    "bearer is case insensitive",
			headers: [][2]string{{"authorization", "bEaReR gpustack_ak_sk"}},
			want:    "gpustack_ak_sk",
		},
		{
			name:    "x-api-key",
			headers: [][2]string{{"x-api-key", "gpustack_ak_sk"}},
			want:    "gpustack_ak_sk",
		},
		{
			name: "authorization wins over x-api-key",
			headers: [][2]string{
				{"authorization", "Bearer from-authorization"},
				{"x-api-key", "from-x-api-key"},
			},
			want: "from-authorization",
		},
		{
			name:    "basic auth yields nothing",
			headers: [][2]string{{"authorization", "Basic dXNlcjpwYXNz"}},
			want:    "",
		},
		{
			name:    "cookie is not a local credential carrier",
			headers: [][2]string{{"cookie", "session=abc"}},
			want:    "",
		},
		{name: "none", headers: [][2]string{{"accept", "*/*"}}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCredential(tc.headers); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyEntryExpired(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	past := now.Unix() - 1
	future := now.Unix() + 1
	exact := now.Unix()

	if (keyEntry{}).expired(now) {
		t.Error("a nil exp must mean never expires")
	}
	if !(keyEntry{Exp: &past}).expired(now) {
		t.Error("an exp in the past must be expired")
	}
	if (keyEntry{Exp: &future}).expired(now) {
		t.Error("an exp in the future must not be expired")
	}
	if !(keyEntry{Exp: &exact}).expired(now) {
		t.Error("exp is exclusive: a key is dead at the instant it expires")
	}
}
