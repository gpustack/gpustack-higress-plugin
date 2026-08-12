package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// The marker is an HS256 JWT carrying the caller's identity across an Envoy
// internal redirect. It exists because ai-proxy replaces `Authorization` with
// the provider credential before the request goes upstream: on the fallback
// pass the client's key is simply gone, so no credential-based check can
// succeed and the marker is the only surviving statement of who is calling.
//
// It is a bearer credential, so it is deliberately narrow: bound to one model,
// valid for five minutes, and never seen by a client (it rides the upstream
// request, not the response).

// jwtHeaderB64 is the encoded `{"alg":"HS256","typ":"JWT"}`, precomputed
// because it never varies.
//
// Verification requires this exact prefix rather than decoding the header and
// reading `alg`. Dispatching on a token's own algorithm claim is how `alg:none`
// forgeries get in, and here that would hand an attacker the one credential
// that bypasses tier 1 on the fallback pass.
const jwtHeaderB64 = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

// markerTTL matches the server's hard-coded five minutes. Not configurable, on
// purpose: the only thing raising it buys is a longer replay window on a bearer
// credential, and a redirect follows its original request within milliseconds.
const markerTTL = 5 * time.Minute

// Normalized identity prefixes, as used by the marker's `id` claim and by the
// verification cache. The prefix says which table answers a lookup: `keys` for
// ak, `refs` for ref.
const (
	identityPrefixAccessKey = "ak:"
	identityPrefixRef       = "ref:"

	// identityAnonymousID names a caller that presented no credential at all.
	// Unprefixed because it has no payload: it is a whole identity, not a
	// reference to one. Honouring it is confined to public routes -- see
	// resolveFromMarker.
	identityAnonymousID = "anon"
)

// markerClaims is the payload's `data` object.
//
// `id` is what makes a plugin-minted marker useful to the plugin: the server's
// own marker carries only a consumer, which is enough for the server to answer
// with but not enough for the gateway to re-assert an identity. There is no
// `token` claim -- the server's has one holding the upstream credential, which
// ai-proxy now holds statically, and which this plugin must never possess.
type markerClaims struct {
	ID       string `json:"id"`
	Consumer string `json:"consumer"`
	Model    string `json:"model"`
}

type markerPayload struct {
	Data markerClaims `json:"data"`
	Exp  int64        `json:"exp"`
}

// signMarker produces `<header>.<payload>.<signature>`, base64url without
// padding, byte-compatible with what PyJWT emits for the same claims.
func signMarker(key []byte, claims markerClaims, expiresAt time.Time) (string, error) {
	payload, err := json.Marshal(markerPayload{Data: claims, Exp: expiresAt.Unix()})
	if err != nil {
		return "", err
	}
	signingInput := jwtHeaderB64 + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(markerSignature(key, signingInput)), nil
}

// verifyMarker checks a marker and returns its claims.
//
// The signature is computed over the received substring, never over a
// re-serialised payload: Go and Python order and space JSON differently, so
// re-encoding before hashing would reject every token the server ever signed.
// The signature is also checked before the payload is parsed, so malformed
// claims cannot be reached without the key.
func verifyMarker(key []byte, token string, now time.Time) (markerClaims, bool) {
	lastDot := strings.LastIndexByte(token, '.')
	if lastDot < 0 {
		return markerClaims{}, false
	}
	signingInput, signatureB64 := token[:lastDot], token[lastDot+1:]
	if !strings.HasPrefix(signingInput, jwtHeaderB64+".") {
		return markerClaims{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureB64)
	if err != nil {
		return markerClaims{}, false
	}
	if !hmac.Equal(signature, markerSignature(key, signingInput)) {
		return markerClaims{}, false
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(signingInput[len(jwtHeaderB64)+1:])
	if err != nil {
		return markerClaims{}, false
	}
	var payload markerPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return markerClaims{}, false
	}
	// A missing exp is treated as expired rather than as "never expires": an
	// unbounded marker is an unbounded bearer credential.
	if payload.Exp == 0 || now.Unix() >= payload.Exp {
		return markerClaims{}, false
	}
	return payload.Data, true
}

func markerSignature(key []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

// splitNormalizedID takes an `id` claim apart into whichever half it holds.
// Exactly one of the two is non-empty on success.
func splitNormalizedID(id string) (accessKey, ref string, ok bool) {
	switch {
	case strings.HasPrefix(id, identityPrefixAccessKey):
		accessKey = strings.TrimPrefix(id, identityPrefixAccessKey)
		return accessKey, "", accessKey != ""
	case strings.HasPrefix(id, identityPrefixRef):
		ref = strings.TrimPrefix(id, identityPrefixRef)
		return "", ref, ref != ""
	}
	return "", "", false
}
