package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

const (
	defaultStatusOnError = http.StatusForbidden
	defaultTimeoutMillis = 1000
	defaultServicePort   = 80

	// endpointModeForwardAuth is the only supported mode, so the field never
	// needs to be written. Upstream ext-auth also offers `envoy` mode (proxy
	// the original method and path under a prefix); GPUStack has only ever used
	// forward_auth against /token-auth, and carrying the other mode would mean
	// carrying a second request-shaping path that nothing exercises.
	//
	// An explicit `envoy` is rejected rather than ignored. Ignoring it would
	// let a config copied from upstream shape requests as forward_auth without
	// saying so, pointing authorization calls at the wrong endpoint -- under
	// FAIL_OPEN that degrades to unauthenticated traffic rather than an error.
	endpointModeForwardAuth = "forward_auth"
)

// PluginConfig is the merged view a request sees: the global block from
// defaultConfig with the matched rule's fields laid over it.
type PluginConfig struct {
	// RouteMatchRegexes is the safety gate: the plugin acts on a request only
	// when Envoy's `route_name` matches one of these.
	//
	// It has to be a plugin-level check rather than a rule, because the SDK's
	// matcher falls back to the global config when nothing matches
	// (RuleMatcher.GetMatchConfig) -- so "no rule matched" cannot by itself mean
	// "not ours". Anything in defaultConfig otherwise reaches every route on the
	// gateway, including other tenants sharing it.
	//
	// Empty matches nothing, which makes an unconfigured plugin inert rather
	// than one that starts authorizing traffic it was never meant to see.
	// Patterns are not anchored implicitly; write `^` if that is what is meant.
	RouteMatchRegexes []*regexp.Regexp

	// AccessPolicy is "public" only on routes that carry a dedicated rule for
	// it. Absent everywhere else, which is what makes a missing rule fail
	// towards "authorize normally".
	AccessPolicy string

	LocalAuth       LocalAuth
	Authz           Authz
	AuthCache       AuthCache
	UpstreamRequest UpstreamRequest

	// StatusOnError is the status used when the authorization service is
	// unreachable or answers 5xx. Top-level rather than nested under Authz,
	// matching upstream's ExtAuthConfig.StatusOnError.
	StatusOnError uint32

	// FailureModeAllow lets **any** request through when the authorization
	// service fails, authenticated or not. Defaults to false, i.e. fail closed --
	// the behaviour GPUStack has today, since it has never set the upstream flag
	// either. On an internet-facing gateway this turns an outage into an open
	// inference proxy, so prefer FailureModeAllowAuthenticated unless that is
	// genuinely what is wanted.
	FailureModeAllow bool

	// FailureModeAllowAuthenticated is the narrow form: allow the request only
	// when the plugin resolved the caller locally. It keeps a server outage from
	// interrupting legitimate traffic without opening the models to anyone who
	// asks, which is the distinction blanket fail-open cannot make.
	//
	// Authorization is still skipped for the duration, so a key valid for one
	// model can reach another until the server returns.
	FailureModeAllowAuthenticated bool
}

// LocalAuth holds the authentication state pushed down to the gateway.
//
// Its two tables are disjoint and serve different jobs. Keys is indexed by
// access_key and both authenticates a credential and answers revocation and
// expiry. Refs is indexed by api_keys.id and is a validity index only -- it
// cannot verify a credential, only confirm that an identity obtained elsewhere
// is still live. Which table a key lands in is decided by a single predicate on
// the server: it has a digest, or it does not.
type LocalAuth struct {
	Enabled bool
	Keys    map[string]keyEntry
	Refs    map[string]refEntry
}

// Authz is the authorization call: endpoint, and which headers cross it in
// each direction. Structurally upstream's `http_service`.
type Authz struct {
	Client        wrapper.HttpClient
	RequestMethod string
	Path          string
	TimeoutMillis uint32

	AllowedHeaders         matcher
	HeadersToAdd           map[string]string
	AllowedUpstreamHeaders matcher
	AllowedClientHeaders   matcher
}

// UpstreamRequest holds rewrites applied to the request heading for the model.
//
// Separate from Authz because they apply whether or not an authorization call
// happened: on a public route no call is made, so nothing under
// `authorization_response` is reachable, yet the request still has to be
// scrubbed before it leaves.
type UpstreamRequest struct {
	// HeadersToRemove are stripped from the upstream request, lower-cased.
	//
	// This replaces the `cookie: dummy=dummy` trick. ext-auth can only *set*
	// response headers, never delete them, so the server overwrote the client's
	// cookie with a junk value to keep a session credential from reaching the
	// model. A wasm plugin can remove the header outright, which achieves the
	// same thing without a magic constant shared across two codebases and
	// without handing the model a nonsense cookie.
	HeadersToRemove []string
}

// AuthCache names the header carrying the signed "already authenticated"
// marker, and holds the key used to sign and check it. The marker is how a
// request survives an internal redirect: on the fallback pass ai-proxy has
// already replaced Authorization with the provider credential, so no
// credential-based check can succeed and only the marker can re-establish who
// the caller is.
type AuthCache struct {
	Header string

	// SigningKey is the HMAC key, used as the literal bytes of the configured
	// string -- the hex is NOT decoded first. That matches PyJWT, which encodes
	// a str key as UTF-8, so a value derived server-side with
	// `hmac(jwt_secret_key, b"gateway-auth-cache").hexdigest()` interoperates.
	// Decoding it here would produce a different key and reject every marker,
	// silently: the same trap as the digest salt in credential.go.
	//
	// Absent is a supported state. The plugin then leaves markers strictly
	// alone -- forwarding and relaying whatever the server minted -- rather
	// than replacing them with ones the server cannot read.
	SigningKey []byte
}

// hasMarkerHeader reports whether a marker header name is configured.
func (a AuthCache) hasMarkerHeader() bool { return a.Header != "" }

// canSign reports whether the plugin may mint and check its own markers.
func (a AuthCache) canSign() bool { return a.Header != "" && len(a.SigningKey) > 0 }

// parseGlobalConfig reads the defaultConfig block.
//
// Every field is optional here. The global block legitimately carries only
// shared state (keys, endpoint, timeouts) while the decision to act at all
// lives in the rules, so an incomplete global config is normal rather than an
// error -- rejecting it would leave the matcher with a zero-value global and
// silently disable the plugin on every route.
func parseGlobalConfig(raw gjson.Result, config *PluginConfig) error {
	return parseInto(raw, config)
}

// parseOverrideRuleConfig layers one rule's fields onto the global config.
//
// `*config = global` copies the struct, which shares the Keys and Refs map
// headers with global. That is safe only because nothing ever writes to those
// maps after parsing: parseInto always builds a fresh map when a rule
// redeclares local_auth, and never mutates an inherited one in place. Keep that
// invariant -- an in-place insert here would corrupt the global config seen by
// every other rule, and the corruption would be a stale authentication table,
// which is the worst thing in this plugin to get wrong.
func parseOverrideRuleConfig(raw gjson.Result, global PluginConfig, config *PluginConfig) error {
	*config = global
	return parseInto(raw, config)
}

// parseInto applies whatever raw declares onto config, leaving everything else
// as inherited. Called for both the global block and each rule, so "absent
// means inherit" falls out of every field being guarded by an Exists() check.
func parseInto(raw gjson.Result, config *PluginConfig) error {
	if v := raw.Get("route_match_regexes"); v.IsArray() {
		compiled := make([]*regexp.Regexp, 0, len(v.Array()))
		for i, entry := range v.Array() {
			pattern := entry.String()
			if pattern == "" {
				continue
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("route_match_regexes[%d]: %w", i, err)
			}
			compiled = append(compiled, re)
		}
		config.RouteMatchRegexes = compiled
	}
	if v := raw.Get("access_policy"); v.Exists() {
		config.AccessPolicy = strings.ToLower(v.String())
	}
	if v := raw.Get("status_on_error"); v.Exists() {
		config.StatusOnError = uint32(v.Uint())
	}
	if config.StatusOnError == 0 {
		config.StatusOnError = defaultStatusOnError
	}
	if v := raw.Get("failure_mode_allow"); v.Exists() {
		config.FailureModeAllow = v.Bool()
	}
	if v := raw.Get("failure_mode_allow_authenticated"); v.Exists() {
		config.FailureModeAllowAuthenticated = v.Bool()
	}
	if v := raw.Get("auth_cache"); v.IsObject() {
		if h := v.Get("header"); h.Exists() {
			config.AuthCache.Header = strings.ToLower(h.String())
		}
		if k := v.Get("signing_key"); k.Exists() {
			config.AuthCache.SigningKey = []byte(k.String())
		}
	}
	if v := raw.Get("upstream_request"); v.IsObject() {
		if h := v.Get("headers_to_remove"); h.IsArray() {
			// Rebuilt rather than appended to, so a rule can narrow the list
			// and cannot mutate the global slice it was copied from.
			names := make([]string, 0, len(h.Array()))
			for _, entry := range h.Array() {
				if name := strings.ToLower(strings.TrimSpace(entry.String())); name != "" {
					names = append(names, name)
				}
			}
			config.UpstreamRequest.HeadersToRemove = names
		}
	}
	if v := raw.Get("local_auth"); v.IsObject() {
		if err := parseLocalAuth(v, &config.LocalAuth); err != nil {
			return fmt.Errorf("local_auth: %w", err)
		}
	}
	if v := raw.Get("authz"); v.IsObject() {
		if err := parseAuthz(v, &config.Authz); err != nil {
			return fmt.Errorf("authz: %w", err)
		}
	}
	return nil
}

// parseLocalAuth rebuilds the key tables from scratch whenever the block is
// present. Rebuilding rather than merging is deliberate: a partially inherited
// authentication table would mean a key deleted by an override stays live, and
// there is no use case for a rule extending the global key set.
//
// `keys` / `refs` are read with IsObject() rather than Exists(): gjson reports
// an explicit `null` literal as existing (Type is Null but Raw is the string
// "null"), which would take the branch and then iterate nothing, quietly
// emptying an inherited table.
func parseLocalAuth(raw gjson.Result, local *LocalAuth) error {
	if v := raw.Get("enabled"); v.Exists() {
		local.Enabled = v.Bool()
	}
	if v := raw.Get("keys"); v.IsObject() {
		keys := make(map[string]keyEntry)
		var err error
		v.ForEach(func(k, entry gjson.Result) bool {
			accessKey := k.String()
			if accessKey == "" {
				// The legacy cluster token is the row with an empty access key.
				// It never receives a digest, so it can never legitimately be
				// here; refusing it keeps a malformed config from creating a
				// key that matches a credential with no access key at all.
				err = errors.New("keys: empty access_key")
				return false
			}
			digest := entry.Get("digest").String()
			if digest == "" {
				err = fmt.Errorf("keys[%s]: missing digest", accessKey)
				return false
			}
			keys[accessKey] = keyEntry{
				Exp:    optionalUnixSeconds(entry.Get("exp")),
				Digest: digest,
				UserID: entry.Get("user_id").Int(),
			}
			return true
		})
		if err != nil {
			return err
		}
		local.Keys = keys
	}
	if v := raw.Get("refs"); v.IsObject() {
		refs := make(map[string]refEntry)
		v.ForEach(func(k, entry gjson.Result) bool {
			refs[k.String()] = refEntry{Exp: optionalUnixSeconds(entry.Get("exp"))}
			return true
		})
		local.Refs = refs
	}
	return nil
}

// optionalUnixSeconds maps an absent or null `exp` to "never expires".
func optionalUnixSeconds(v gjson.Result) *int64 {
	if !v.Exists() || v.Type == gjson.Null {
		return nil
	}
	seconds := v.Int()
	return &seconds
}

func parseAuthz(raw gjson.Result, authz *Authz) error {
	if v := raw.Get("endpoint_mode"); v.Exists() && v.String() != endpointModeForwardAuth {
		return fmt.Errorf("endpoint_mode %q is not supported, only %q", v.String(), endpointModeForwardAuth)
	}
	if v := raw.Get("endpoint"); v.IsObject() {
		if err := parseEndpoint(v, authz); err != nil {
			return err
		}
	}
	if v := raw.Get("timeout"); v.Exists() {
		authz.TimeoutMillis = uint32(v.Uint())
	}
	if authz.TimeoutMillis == 0 {
		authz.TimeoutMillis = defaultTimeoutMillis
	}

	if v := raw.Get("authorization_request"); v.IsObject() {
		if h := v.Get("allowed_headers"); h.Exists() {
			m, err := buildHeaderMatcher(h.Array())
			if err != nil {
				return fmt.Errorf("authorization_request.allowed_headers: %w", err)
			}
			authz.AllowedHeaders = m
		}
		if h := v.Get("headers_to_add"); h.IsObject() {
			add := make(map[string]string)
			h.ForEach(func(k, val gjson.Result) bool {
				add[k.String()] = val.String()
				return true
			})
			authz.HeadersToAdd = add
		}
	}

	if v := raw.Get("authorization_response"); v.IsObject() {
		if h := v.Get("allowed_upstream_headers"); h.Exists() {
			m, err := buildHeaderMatcher(h.Array())
			if err != nil {
				return fmt.Errorf("authorization_response.allowed_upstream_headers: %w", err)
			}
			authz.AllowedUpstreamHeaders = m
		}
		if h := v.Get("allowed_client_headers"); h.Exists() {
			m, err := buildHeaderMatcher(h.Array())
			if err != nil {
				return fmt.Errorf("authorization_response.allowed_client_headers: %w", err)
			}
			authz.AllowedClientHeaders = m
		}
	}
	return nil
}

func parseEndpoint(raw gjson.Result, authz *Authz) error {
	serviceName := raw.Get("service_name").String()
	if serviceName == "" {
		return errors.New("endpoint.service_name must not be empty")
	}
	servicePort := raw.Get("service_port").Int()
	if servicePort == 0 {
		servicePort = defaultServicePort
	}
	authz.Client = wrapper.NewClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: servicePort,
		Host: raw.Get("service_host").String(),
	})

	path := raw.Get("path").String()
	if path == "" {
		return errors.New("endpoint.path must not be empty")
	}
	authz.Path = path

	if v := raw.Get("request_method"); v.Exists() {
		authz.RequestMethod = strings.ToUpper(v.String())
	} else {
		authz.RequestMethod = http.MethodGet
	}
	return nil
}

// ready reports whether the authorization call can actually be made. A rule can
// switch the plugin on without the global block having supplied an endpoint;
// treating that as "not ready" and failing closed beats dispatching to a nil
// client.
func (a Authz) ready() bool {
	return a.Client != nil && a.Path != ""
}
