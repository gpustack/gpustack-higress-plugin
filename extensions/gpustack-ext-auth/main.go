// gpustack-ext-auth authenticates GPUStack API keys at the gateway and defers
// authorization to the GPUStack server.
//
// It plays the role upstream Higress ext-auth plays today, but splits the two
// halves of /token-auth apart. Authentication is a pure function of the
// credential over a small, slowly-changing table, so it is pushed down here.
// Authorization spans key scope, user RBAC and route policy with no single
// invalidation signal, so it stays on the server and is asked afresh per
// request. Written against the upstream plugin rather than forked from it --
// the request/response header contract is upstream's, the rest is not.

package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

const pluginName = "gpustack-ext-auth"

// accessPolicyPublic is the one policy value the plugin understands. Every
// other policy is the server's business and never reaches this config.
const accessPolicyPublic = "public"

// anonymousConsumer is what the server records for a request it could not
// authenticate. Must stay byte-identical to the literal in routes/token.py,
// since it lands in the access log next to consumers the server produced.
const anonymousConsumer = "none"

// Response-code detail suffixes. They surface in the access log as
// `via_wasm::...gpustack-ext-auth.<detail>`, which is the only way to tell
// which branch rejected a request after the fact.
const (
	detailUnauthorized = "gpustack-ext-auth.unauthorized"
	detailBadSecret    = "gpustack-ext-auth.local_secret_mismatch"
	detailNotReady     = "gpustack-ext-auth.authz_not_configured"
)

// nonEmptyRejectionBody is substituted when a rejection would otherwise carry
// no body at all. Envoy's local-reply path treats headers-plus-end_stream with
// a nil body as degenerate under some filter chains: the reply is queued and
// never flushed, and the client sees a bare disconnect (bytes_sent=0,
// response_flags=DC) instead of the status we chose. Always send something.
var nonEmptyRejectionBody = []byte(`{"error":{"message":"Invalid authentication credentials","type":"invalid_request_error","code":401}}`)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		// Two-level config: defaultConfig carries shared state, each rule lays
		// its own fields over it. Upstream uses the flat ParseConfig, which has
		// no merge step and so cannot express "global key table, per-route
		// policy".
		wrapper.ParseOverrideConfig(parseGlobalConfig, parseOverrideRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
	)
}

// identityState is how far local authentication got.
type identityState int

const (
	// identityUnresolved means the plugin could not name the caller. Not a
	// rejection: the credential travels with the authorization call and the
	// server does both jobs, exactly as it does today.
	identityUnresolved identityState = iota
	// identityResolved means the credential verified against the local key
	// table, so the call can assert an identity instead of forwarding a secret.
	identityResolved
	// identityRejected means the key is known and the secret is wrong. The
	// table held everything needed to decide, so deciding here keeps a
	// credential-stuffing client from turning into server load.
	identityRejected
)

// identitySource records which tier named the caller. Carried for logging: on a
// fallback pass the marker is the only tier that can succeed, so "which tier"
// is the first thing worth knowing when one misbehaves.
type identitySource int

const (
	sourceNone identitySource = iota
	sourceMarker
	sourceKeys
	sourceCache
	sourceAnonymous
)

func (s identitySource) String() string {
	switch s {
	case sourceMarker:
		return "marker"
	case sourceKeys:
		return "key table"
	case sourceCache:
		return "verification cache"
	case sourceAnonymous:
		return "no credential"
	}
	return "unresolved"
}

type identity struct {
	State  identityState
	Source identitySource

	// Exactly one of AccessKey / Ref is set on a resolved identity: an access
	// key answers from `keys`, a ref from `refs`.
	AccessKey string
	Ref       string
	UserID    int64

	// Anonymous marks a request that carries no credential at all on a public
	// route. It is a resolved identity in the sense that matters -- the server's
	// answer for it is fixed, so there is nothing to ask -- but it names nobody,
	// so it must never be asserted to the server and must never be honoured off
	// a public route.
	Anonymous bool

	// Consumer is set only when the identity came from a marker, which carries
	// the value the server itself produced. Everywhere else the consumer is the
	// server's to supply on the authorization response.
	Consumer string

	// MarkerRejected records that a marker header was present but produced no
	// identity -- a bad signature, a model mismatch, or an entry no longer in
	// the tables. Reported rather than logged in place, for the same reason as
	// DigestUnusable. Worth surfacing because the silent fallthrough otherwise
	// hides a signing-key mismatch between the server and the gateway, or
	// between two gateway pods.
	MarkerRejected bool

	// DigestUnusable records that a key entry matched but carried a digest
	// this build cannot check. Reported rather than logged in place so that
	// resolveIdentity stays free of host calls: wasm-go's log package
	// dereferences a package-level logger that only SetCtx installs, so a
	// single log line inside the pure core would make it unreachable from a
	// unit test. Worth surfacing because it means either a corrupted column or
	// a server that has moved to a digest algorithm this gateway predates.
	DigestUnusable bool
}

// requestInfo is the request metadata the authorization call needs, lifted out
// of the proxy context so header building stays a pure function.
type requestInfo struct {
	Method string
	Path   string
	Host   string
	Scheme string
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	ctx.DontReadRequestBody()

	// The safety gate. Routes outside this list belong to someone else -- other
	// tenants on a shared gateway, the control-plane mirror ingress -- and must
	// pass through untouched.
	// Held rather than read twice: the debug log below wants it too, and its
	// arguments are evaluated whether or not the level is enabled, so calling
	// again there would cost a host call on every authorized request.
	routeName := currentRouteName()
	if !matchesRoute(config.RouteMatchRegexes, routeName) {
		return types.ActionContinue
	}

	// The plugin rewrites headers that other filters may route on, and the
	// authorization dispatch is async, so pin the route before either happens.
	ctx.DisableReroute()

	// Drop any client-supplied caller identity before anything else can read it.
	// Every path below either replaces this header with an authoritative value
	// or does not touch it, so without this a client that sends its own
	// x-mse-consumer would have it survive to the model on any path that leaves
	// it unset -- and that value feeds access-log attribution and the
	// consumer dimension of rate limiting. Today the server answers with a
	// consumer on every 200, so the gap never opens in practice; removing it
	// here means correctness does not depend on that staying true.
	_ = proxywasm.RemoveHttpRequestHeader(headerConsumer)

	if !config.Authz.ready() {
		log.Errorf("%s: authz endpoint is not configured; failing closed", pluginName)
		_ = sendResponse(config.StatusOnError, detailNotReady, nil, nonEmptyRejectionBody)
		return types.ActionPause
	}

	requestHeaders, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		log.Errorf("%s: failed to read request headers: %v", pluginName, err)
		_ = sendResponse(config.StatusOnError, detailNotReady, nil, nonEmptyRejectionBody)
		return types.ActionPause
	}

	// Read once and threaded: the redirect pass both verifies a marker and
	// mints a fresh one, so resolving it per use would double the host calls.
	conn := downstreamConnection()
	id := resolveIdentity(config, requestHeaders, conn, time.Now())
	if id.MarkerRejected {
		log.Debugf("%s: an auth marker was present but did not verify; falling back to the credential",
			pluginName)
	}
	if id.DigestUnusable {
		log.Warnf("%s: unusable secret_key_digest for access key %s; deferring to the server",
			pluginName, id.AccessKey)
	}
	if id.State == identityRejected {
		if config.AccessPolicy == accessPolicyPublic {
			// Never be stricter than the route's own policy. On a public route
			// authentication is not a precondition -- the server lets an
			// unrecognised credential through and simply records the consumer as
			// 'none' -- so rejecting here would break a caller whose key merely
			// has a typo. The reason for rejecting locally does not apply either:
			// it exists to keep credential stuffing off the server, and the same
			// client can reach this model carrying no credential at all.
			id = identity{State: identityUnresolved}
		} else {
			// A synchronous local reply must be followed by ActionPause
			// (HeaderStopIteration), not HeaderStopAllIterationAndWatermark: the
			// latter is the async-wait shape and parks the response under a
			// watermark waiting for a resume that will never come.
			_ = sendResponse(http.StatusUnauthorized, detailBadSecret, nil, nonEmptyRejectionBody)
			return types.ActionPause
		}
	}

	if consumer, ok := publicSkipConsumer(config, id); ok {
		return skipAuthz(config, id, consumer, extractHeader(requestHeaders, headerLLMModel), conn)
	}

	// Debug rather than Info: this fires on every request that does call the
	// server, where wasm-go already logs the call itself. The skip path logs at
	// the same level for symmetry -- see skipAuthz, where it is the only trace a
	// request leaves in the plugin's own logs.
	log.Debugf("%s: authorizing %s via the server (%s) on %s from %s",
		pluginName, describeCaller(id), describeForm(id), routeName, conn)

	info := requestInfo{
		Method: ctx.Method(),
		Path:   ctx.Path(),
		Host:   ctx.Host(),
		Scheme: ctx.Scheme(),
	}
	return dispatchAuthz(config, info, requestHeaders, id, conn)
}

// downstreamConnection identifies the client connection this request arrived
// on, as `<ip>:<port>`, empty when the property cannot be read.
//
// Not a header: it is the peer address of the socket, so a client can neither
// set it nor choose it. That is the whole reason a marker can be bound to it.
//
// Per-connection rather than per-process, which rules out the obvious
// alternative: Envoy's connection id is a counter that restarts at zero in
// every gateway pod, and a marker signed on one pod verifies on any other, so
// two pods hand out colliding ids by construction.
func downstreamConnection() string {
	address, err := proxywasm.GetProperty([]string{"source", "address"})
	if err != nil {
		return ""
	}
	return string(address)
}

// currentRouteName reads Envoy's route_name property, empty when there is none
// (which no configured pattern should match).
func currentRouteName() string {
	name, err := proxywasm.GetProperty([]string{"route_name"})
	if err != nil {
		return ""
	}
	return string(name)
}

// matchesRoute reports whether this route is one the plugin owns.
//
// An empty list matches nothing on purpose: the failure mode of a missing
// config should be a plugin that does nothing, not one that starts
// authenticating traffic it was never pointed at.
func matchesRoute(patterns []*regexp.Regexp, routeName string) bool {
	if routeName == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern.MatchString(routeName) {
			return true
		}
	}
	return false
}

// publicSkipConsumer decides whether this request can be allowed without asking
// the server, and what consumer to record if so.
//
// On a public route the server evaluates no policy at all: scope,
// allowed_model_names and RBAC are skipped entirely, and even an unrecognised
// credential is let through. The response is therefore a constant function of
// the identity, and every bit of it is something the gateway already knows. The
// call is not redundant only while the server is down -- it is redundant
// always.
//
// `access_policy: public` is the whole control surface. There is deliberately no
// second toggle: the field means exactly "you may allow here" to this plugin and
// nothing else, so a config that declared a route public but then declined to
// act on it would only be a way to carry a fact nobody reads. Whether a route
// gets the field at all is the reconciler's call, per route.
//
// The limit is consumer fidelity, not safety. A `ref:` identity cannot be
// rendered locally because a custom key's consumer embeds its access key, which
// the `refs` table does not carry; guessing would corrupt the caller attribution
// in the access log, so those go to the server as usual. Sending them is never
// less strict -- the server would allow them too.
func publicSkipConsumer(config PluginConfig, id identity) (string, bool) {
	if config.AccessPolicy != accessPolicyPublic {
		return "", false
	}
	return localConsumer(id)
}

// localConsumer renders the consumer for an identity the gateway named itself,
// empty when it cannot be known without asking the server.
func localConsumer(id identity) (string, bool) {
	if id.State != identityResolved {
		return "", false
	}
	// Nobody to name, and the server would not have named anyone either.
	if id.Anonymous {
		return anonymousConsumer, true
	}
	// A marker or a cache entry replays the consumer the server itself produced,
	// so it is exact by construction and works for both identity forms.
	if id.Consumer != "" {
		return id.Consumer, true
	}
	if id.AccessKey == "" || id.UserID <= 0 {
		return "", false
	}
	// Byte-identical to the server's own join. A generated key's access_key is
	// never absent, so the branch where the server omits that part cannot arise
	// here.
	return id.AccessKey + ".gpustack-" + strconv.FormatInt(id.UserID, 10), true
}

// skipAuthz allows the request locally, taking over the header rewrites that a
// 200 from the authorization service would otherwise have produced.
//
// Those rewrites are not incidental. Stripping `cookie` is what stops the
// client's own session cookie reaching the model -- on the authorized path that
// happens because /token-auth answers with a dummy value, so a skip that did not
// reproduce it would silently start forwarding a credential that never used to
// travel this far.
func skipAuthz(config PluginConfig, id identity, consumer, model, conn string) types.Action {
	// A public route makes no server call, so nothing else in the request path
	// logs anything: without this the plugin is indistinguishable from not being
	// in the chain at all. The always-on signal is X-Mse-Consumer reaching the
	// access log via ai-statistics; this line is for telling *which* tier
	// resolved the caller when that needs diagnosing.
	log.Debugf("%s: allowing %s locally on a public route, consumer %q",
		pluginName, describeCaller(id), consumer)
	applyUpstreamRewrites(config, consumer)
	// The server never runs on this path, so it can never mint the marker the
	// fallback pass needs. If the plugin cannot sign either, the redirect has
	// nothing to authenticate with.
	applyMarker(config, id, model, consumer, conn, time.Now())
	return types.ActionContinue
}

// applyUpstreamRewrites finishes the request before it leaves for the model:
// stamp who is calling, then strip whatever must not travel further.
//
// The consumer header name is hard-coded rather than configurable, matching
// every other plugin in the ecosystem -- basic-auth, key-auth and jwt-auth write
// the literal, ai-statistics, ai-quota, ai-security-guard, cluster-key-rate-limit
// and ai-token-ratelimit all read a constant. Making it settable here would only
// allow a value nothing downstream reads, which would silently misattribute
// access logs and consumer-dimension rate limits rather than fail.
//
// Removal runs last so it also clears anything allowed_upstream_headers just
// copied in -- notably a `cookie: dummy=dummy` from a server that still sends
// one.
func applyUpstreamRewrites(config PluginConfig, consumer string) {
	if consumer != "" {
		_ = proxywasm.ReplaceHttpRequestHeader(headerConsumer, consumer)
	}
	for _, name := range config.UpstreamRequest.HeadersToRemove {
		_ = proxywasm.RemoveHttpRequestHeader(name)
	}
}

// resolveIdentity names the caller without leaving the gateway.
//
// Tier 0 is the marker, checked first because it is the only tier that can
// succeed on a fallback pass -- by then ai-proxy has replaced Authorization, so
// tier 1 necessarily misses. Tier 1 is a lookup in the local key table plus one
// fast hash.
//
// Everything that is not a confident match falls through to identityUnresolved
// rather than to a rejection. An expired entry, an unreadable digest, an access
// key the table has never heard of -- each of those is a statement about the
// gateway's copy of the world, not about the credential, and the server is the
// one that gets to be authoritative. The single exception is a known access key
// whose secret does not verify, which is a statement about the credential.
func resolveIdentity(config PluginConfig, requestHeaders [][2]string, conn string, now time.Time) identity {
	id := resolveIdentityTiers(config, requestHeaders, conn, now)
	if id.Source != sourceMarker && config.AuthCache.canSign() &&
		extractHeader(requestHeaders, config.AuthCache.Header) != "" {
		id.MarkerRejected = true
	}
	return id
}

func resolveIdentityTiers(config PluginConfig, requestHeaders [][2]string, conn string, now time.Time) identity {
	if !config.LocalAuth.Enabled {
		return identity{State: identityUnresolved}
	}
	if id, ok := resolveFromMarker(config, requestHeaders, conn, now); ok {
		return id
	}
	// Tiers 1 and 2 read the API key, and the server would not have. Its
	// authenticate_request is an if/elif chain -- basic, then cookie, then
	// bearer / x-api-key -- so whenever one of the first two is present the
	// bearer is never reached, and answering from it here would answer a
	// different question than the one being stood in for.
	//
	// Tier 0 above is exempt: a marker records a decision the server already
	// made, or a public-route skip, and the fallback pass that reads it is an
	// internal redirect of the very same request. Tier 3 is unreachable anyway,
	// since it requires that no credential at all be present.
	if hasServerFirstCredential(requestHeaders) {
		return identity{State: identityUnresolved}
	}
	if id, decided := resolveFromKeys(config, requestHeaders, now); decided {
		return id
	}
	if id, ok := resolveFromVerificationCache(config, requestHeaders); ok {
		// Same re-check a marker gets: the entry names an identity, `refs` says
		// whether it is still one.
		if identityStillValid(config, id.AccessKey, id.Ref, now) {
			return id
		}
	}
	if id, ok := resolveAnonymous(config, requestHeaders); ok {
		return id
	}
	return identity{State: identityUnresolved}
}

// hasServerFirstCredential reports whether the request carries a credential the
// server would consult before the API key, making anything this plugin decides
// from that key a different answer than the server's.
//
// Three ways it diverges, all from the same cause, and all silent:
//
//   - a valid session cookie beside a mistyped bearer authenticates on the
//     server and would be rejected here;
//   - a valid session cookie beside a valid bearer authenticates *as the
//     session* there, so the consumer names the user rather than the key and
//     the key's scope and allowed_model_names do not apply -- while here the
//     key would be asserted, with both;
//   - an expired session cookie beside a valid bearer is a 401 on the server,
//     because the cookie branch is taken and returns nobody rather than falling
//     through, and would be allowed here.
//
// Only cookie and basic qualify. x-api-key sits in the same branch as the
// bearer, so tier 1 has already had its say about it.
func hasServerFirstCredential(requestHeaders [][2]string) bool {
	if extractHeader(requestHeaders, headerCookie) != "" {
		return true
	}
	authorization := extractHeader(requestHeaders, headerAuthorization)
	// Folded over the sliced prefix rather than the whole header: a bearer JWT
	// runs to kilobytes, and lower-casing one on every request would allocate a
	// copy of it to read six bytes.
	const basic = "basic "
	return len(authorization) >= len(basic) && strings.EqualFold(authorization[:len(basic)], basic)
}

// resolveFromKeys is tier 1: parse the credential, look the access key up, and
// check one fast hash. decided is false when this tier has nothing to say and
// the next one should run.
func resolveFromKeys(config PluginConfig, requestHeaders [][2]string, now time.Time) (id identity, decided bool) {
	if len(config.LocalAuth.Keys) == 0 {
		return identity{}, false
	}
	credential := extractCredential(requestHeaders)
	if credential == "" {
		return identity{}, false
	}
	accessKey, secretKey, ok := parseAPIKey(credential)
	if !ok {
		return identity{}, false
	}
	entry, found := config.LocalAuth.Keys[accessKey]
	if !found || entry.expired(now) {
		return identity{}, false
	}
	match, usable := verifySecretKeyDigest(entry.Digest, secretKey)
	if !usable {
		return identity{State: identityUnresolved, AccessKey: accessKey, DigestUnusable: true}, true
	}
	if !match {
		return identity{State: identityRejected}, true
	}
	return identity{State: identityResolved, Source: sourceKeys, AccessKey: accessKey, UserID: entry.UserID}, true
}

// resolveAnonymous recognises a request that carries no credential at all on a
// public route.
//
// This is the one case where "the server would have allowed it" is knowable
// without asking. The ambiguity that keeps other unresolved credentials going to
// the server is that /token-auth tries basic, then cookie, then bearer /
// x-api-key -- a bad bearer alongside a valid session cookie still
// authenticates, and the consumer would be a real identity rather than `none`.
// With none of them present there is nothing left for the server to succeed
// with, so its answer is fixed: allow, consumer `none`.
//
// It matters because anonymous access is the defining use of a public model, and
// without this it would be the one kind of public traffic that still needs a
// live server.
func resolveAnonymous(config PluginConfig, requestHeaders [][2]string) (identity, bool) {
	if config.AccessPolicy != accessPolicyPublic {
		return identity{}, false
	}
	for _, name := range credentialHeaders {
		if extractHeader(requestHeaders, name) != "" {
			return identity{}, false
		}
	}
	return identity{State: identityResolved, Source: sourceAnonymous, Anonymous: true}, true
}

// resolveFromMarker is tier 0: the identity this plugin (or the server) already
// established on an earlier pass of the same request.
//
// The model binding is what stops a marker being reused as a general bearer
// token. It holds on a fallback pass because a fallback route serves the same
// `x-higress-llm-model` and only swaps the upstream registry -- that invariant
// is load-bearing, so a mismatch is treated as no marker at all.
func resolveFromMarker(config PluginConfig, requestHeaders [][2]string, conn string, now time.Time) (identity, bool) {
	if !config.AuthCache.canSign() {
		return identity{}, false
	}
	raw := extractHeader(requestHeaders, config.AuthCache.Header)
	if raw == "" {
		return identity{}, false
	}
	claims, ok := verifyMarker(config.AuthCache.SigningKey, raw, now)
	if !ok {
		return identity{}, false
	}
	if claims.Model == "" || claims.Model != extractHeader(requestHeaders, headerLLMModel) {
		return identity{}, false
	}
	// The marker is only meant to be read on the pass after the one that minted
	// it, and an internal redirect runs on the very same client connection.
	// Anyone else presenting the marker is on a connection of their own, so
	// requiring the two to match is what makes it useless away from the request
	// it belongs to.
	//
	// It has to be checked, because nothing strips a client-supplied copy of
	// this header and the marker travels on the upstream request: whoever
	// receives those requests -- a worker, an APM, a third-party provider --
	// gets a fresh bearer for the caller with every one. Binding it to the
	// connection is what stops that from being a standing ability to act as
	// them.
	//
	// Either side empty is a refusal rather than a match: a comparison between
	// two unreadable values would pass, which would turn the check into a
	// no-op precisely when the property is unavailable.
	if claims.Conn == "" || claims.Conn != conn {
		return identity{}, false
	}
	// An anonymous marker is only meaningful where anonymity is allowed. Without
	// this guard one minted on a public route could be replayed against a
	// non-public route serving the same model -- the model binding would hold --
	// and the plugin would report a resolved identity that names nobody.
	if claims.ID == identityAnonymousID {
		if config.AccessPolicy != accessPolicyPublic {
			return identity{}, false
		}
		return identity{
			State:     identityResolved,
			Source:    sourceMarker,
			Anonymous: true,
			Consumer:  claims.Consumer,
		}, true
	}

	// A server-minted marker carries no `id`, only a consumer. That is enough
	// for the server to answer with but not for the gateway to re-assert an
	// identity, so treat it as unresolved and let it be forwarded instead.
	accessKey, ref, ok := splitNormalizedID(claims.ID)
	if !ok {
		return identity{}, false
	}
	// Re-check the identity against the current tables. A marker is valid for
	// five minutes, so without this a key revoked and pushed out of `keys` would
	// keep working until its last marker expired -- and on a public route, where
	// nothing else calls the server, absence from `keys` is the *only* thing
	// revocation has to travel on. Falling through to unresolved rather than
	// rejecting keeps the usual rule: the server stays authoritative.
	if !identityStillValid(config, accessKey, ref, now) {
		return identity{}, false
	}
	return identity{
		State:     identityResolved,
		Source:    sourceMarker,
		AccessKey: accessKey,
		Ref:       ref,
		Consumer:  claims.Consumer,
	}, true
}

// identityStillValid confirms an identity obtained without a table lookup is
// still live: present in whichever table answers for its form, and not expired.
//
// This is what lets revocation work with no cache-invalidation step of its own.
// Nothing has to reach into a marker or a cache entry and delete it; removing
// the key from `keys` / `refs` is enough, because every identity that did not
// come from a lookup is checked against them before it is used. `refs` exists
// for exactly this: it cannot verify a credential, only confirm that an
// identity obtained some other way has not been withdrawn.
//
// An identity in neither table is not live here. That is the honest answer even
// when the tables are merely stale -- the caller falls through to the server,
// which is authoritative.
func identityStillValid(config PluginConfig, accessKey, ref string, now time.Time) bool {
	if accessKey != "" {
		entry, ok := config.LocalAuth.Keys[accessKey]
		return ok && !entry.expired(now)
	}
	entry, ok := config.LocalAuth.Refs[ref]
	return ok && !entry.expired(now)
}

// normalizedID renders the identity in the form the marker and the verification
// cache both index by. Empty when nothing was resolved.
func (i identity) normalizedID() string {
	switch {
	case i.Anonymous:
		return identityAnonymousID
	case i.AccessKey != "":
		return identityPrefixAccessKey + i.AccessKey
	case i.Ref != "":
		return identityPrefixRef + i.Ref
	}
	return ""
}

func dispatchAuthz(config PluginConfig, info requestInfo, requestHeaders [][2]string, id identity, conn string) types.Action {
	authzHeaders := buildAuthzRequestHeaders(config, info, requestHeaders, id, conn)
	model := extractHeader(requestHeaders, headerLLMModel)
	credential := extractCredential(requestHeaders)

	err := config.Authz.Client.Call(
		config.Authz.RequestMethod,
		config.Authz.Path,
		reconvertHeaders(authzHeaders),
		nil,
		func(statusCode int, responseHeaders http.Header, responseBody []byte) {
			if statusCode != http.StatusOK {
				// This callback is the one that paused the request, so it is
				// the one that owes the resume.
				if handleAuthzFailure(config, id, model, conn, statusCode, responseHeaders, responseBody) {
					proxywasm.ResumeHttpRequest()
				}
				return
			}
			applyUpstreamHeaders(config, responseHeaders)
			consumer := responseHeaders.Get(headerConsumer)
			applyUpstreamRewrites(config, consumer)

			// The server has just named a caller this gateway could not. Take
			// the id it hands back so the request can mint a marker for its own
			// fallback pass, and remember it so the next request skips the
			// authentication half entirely.
			if id.State != identityResolved {
				if ref := responseHeaders.Get(headerKeyRef); ref != "" {
					id.Ref = ref
					storeVerification(config, credential, ref, consumer)
				}
			}

			applyMarker(config, id, model, consumer, conn, time.Now())
			proxywasm.ResumeHttpRequest()
		},
		config.Authz.TimeoutMillis,
	)
	if err != nil {
		log.Errorf("%s: failed to dispatch authorization call: %v", pluginName, err)
		// A dispatch error and a 5xx from the server are the same event to the
		// failure-mode policy, so route both through it. What differs is the
		// resume: nothing was ever paused on this path, so resuming here would
		// be a resume against a request that is still iterating. Return an
		// action instead.
		if handleAuthzFailure(config, id, model, conn, http.StatusInternalServerError, nil, nil) {
			return types.ActionContinue
		}
		return types.ActionPause
	}
	return types.HeaderStopAllIterationAndWatermark
}

// buildAuthzRequestHeaders assembles the headers for the authorization call.
//
// Two shapes come out of here. With an unresolved identity the credential is
// forwarded and the server authenticates it, which is what happens today. With
// a resolved identity the plugin asserts the access key and strips every
// credential header, so the server can skip authentication and price only the
// policy evaluation.
//
// The stripping is the part that is easy to leave out and hard to notice: it is
// not enough to add the assertion header, because the credential would still be
// sitting there and the server would authenticate it anyway, quietly turning
// every request back into the shape the split was meant to avoid. Note also
// that upstream forwards Authorization unconditionally -- outside the
// allowed_headers matcher entirely -- so leaving it in place is the default
// behaviour rather than something a config has to ask for.
func buildAuthzRequestHeaders(config PluginConfig, info requestInfo, requestHeaders [][2]string, id identity, conn string) http.Header {
	authzHeaders := http.Header{}

	// Add, not Set: HTTP/2 lets a client split one logical Cookie across several
	// header fields, and collapsing them to the last one would hand the server a
	// fragment -- enough to fail cookie_auth on a session that is perfectly
	// valid. Every later step here deletes by name, so duplicates cannot survive
	// where a single authoritative value is meant.
	for _, header := range requestHeaders {
		if matchesHeader(config.Authz.AllowedHeaders, header[0]) {
			authzHeaders.Add(header[0], header[1])
		}
	}
	for key, value := range config.Authz.HeadersToAdd {
		authzHeaders.Set(key, value)
	}

	// The marker rides along without being declared in allowed_headers. It is
	// this plugin's own protocol with the server rather than a client header
	// the operator chose to expose, so requiring it in the CR would only create
	// a way to misconfigure it.
	if config.AuthCache.hasMarkerHeader() {
		if marker := extractHeader(requestHeaders, config.AuthCache.Header); marker != "" {
			authzHeaders.Set(config.AuthCache.Header, marker)
		}
	}

	// The connection this request is on, for the server to bind its marker to.
	// Set unconditionally, and with Set so any client-supplied copy is
	// overwritten: the whole guarantee is that this value names the connection
	// the client is actually on, not one it asked to be. An empty conn (no
	// source.address) is sent as empty, which the server treats as unbindable
	// and refuses to short-circuit on -- the same posture the plugin takes for
	// its own marker when the address is missing.
	authzHeaders.Set(headerDownstreamConn, conn)

	// Never relay a client-supplied assertion. The transformer plugin strips
	// both at priority 810 before this plugin runs at 360, so reaching this
	// line with one present should be impossible -- which is exactly why it is
	// worth two lines to guarantee, since the failure mode is a client naming
	// any identity it likes.
	authzHeaders.Del(headerAccessKey)
	authzHeaders.Del(headerKeyRef)
	// The consumer travels the other way -- the server produces it, never reads
	// it -- so a copy here could only have come from the client. The request
	// phase already removes it, but repeating it keeps this function correct
	// independent of the order its caller does things in.
	authzHeaders.Del(headerConsumer)

	// Anonymity is not an identity to assert: telling the server authentication
	// is done while naming nobody would have it skip the check and evaluate
	// policy for a caller that does not exist. Unreachable today -- an anonymous
	// identity always takes the local-allow path -- but the failure would be
	// severe enough to be worth closing here too.
	if id.State == identityResolved && !id.Anonymous {
		for _, name := range credentialHeaders {
			authzHeaders.Del(name)
		}
		if id.AccessKey != "" {
			authzHeaders.Set(headerAccessKey, id.AccessKey)
		}
		if id.Ref != "" {
			authzHeaders.Set(headerKeyRef, id.Ref)
		}
	} else if authorization := extractHeader(requestHeaders, headerAuthorization); authorization != "" {
		authzHeaders.Set(headerAuthorization, authorization)
	}

	authzHeaders.Set(headerOriginalMethod, info.Method)
	authzHeaders.Set(headerOriginalURI, info.Path)
	authzHeaders.Set(headerXForwardedProto, info.Scheme)
	authzHeaders.Set(headerXForwardedMethod, info.Method)
	authzHeaders.Set(headerXForwardedURI, info.Path)
	authzHeaders.Set(headerXForwardedHost, info.Host)

	return authzHeaders
}

// applyUpstreamHeaders copies the authorized response's headers onto the
// request heading for the model.
func applyUpstreamHeaders(config PluginConfig, responseHeaders http.Header) {
	for name, values := range responseHeaders {
		if len(values) == 0 {
			continue
		}
		if matchesHeader(config.Authz.AllowedUpstreamHeaders, name) || isMarkerHeader(config, name) {
			_ = proxywasm.ReplaceHttpRequestHeader(name, values[0])
		}
	}
}

// applyMarker replaces the marker on the upstream request with one this plugin
// signed, so the fallback pass can re-establish the identity locally instead of
// depending on the server being reachable.
//
// Overwriting rather than appending is the point: the server's own marker
// carries a consumer but no identity, which the gateway cannot act on.
// warnedMissingConnection keeps the warning in applyMarker to one line per VM.
var warnedMissingConnection bool

func applyMarker(config PluginConfig, id identity, model, consumer, conn string, now time.Time) {
	claims, ok := markerClaimsFor(config, id, model, consumer, conn)
	if !ok {
		// One non-mint is worth reporting: an unreadable downstream address
		// disables markers entirely, and the cost lands on the pass this
		// plugin exists to serve -- a redirect on a public route has no server
		// to fall back to. Every other reason here is a deliberate non-mint.
		//
		// Once per VM. Per-worker duplication is acceptable; a line per request
		// is not.
		if conn == "" && !warnedMissingConnection {
			warnedMissingConnection = true
			log.Warnf("%s: no downstream address available, so auth markers are "+
				"disabled and a fallback pass has to ask the server", pluginName)
		}
		return
	}
	token, err := signMarker(config.AuthCache.SigningKey, claims, now.Add(markerTTL))
	if err != nil {
		log.Errorf("%s: failed to sign auth marker: %v", pluginName, err)
		return
	}
	_ = proxywasm.ReplaceHttpRequestHeader(config.AuthCache.Header, token)
}

// markerClaimsFor decides whether this request can carry a plugin-minted marker
// and what goes in it.
//
// All three claims are required. Without an identity the fallback pass would
// get a consumer it cannot act on -- worse than nothing, because replacing the
// server's marker with one the server cannot read would break the very pass the
// marker exists for. So when anything is missing, leave the server's marker
// alone and let the existing path handle it.
func markerClaimsFor(config PluginConfig, id identity, model, consumer, conn string) (markerClaims, bool) {
	if !config.LocalAuth.Enabled || !config.AuthCache.canSign() {
		return markerClaims{}, false
	}
	normalized := id.normalizedID()
	if normalized == "" || model == "" || consumer == "" {
		return markerClaims{}, false
	}
	// No marker at all rather than one bound to nothing: an empty value would
	// be refused on the way back in, so minting it would only cost a signature
	// and leave a bearer lying on the upstream request that no pass can use.
	// The redirect then falls back to asking the server, which is a degradation
	// and not a hole.
	//
	// The warning for it lives in applyMarker, which keeps this a pure function
	// the tests can call without a host.
	if conn == "" {
		return markerClaims{}, false
	}
	return markerClaims{ID: normalized, Consumer: consumer, Model: model, Conn: conn}, true
}

// describeCaller renders an identity for a log line, never including a
// credential -- an access key is already public in the consumer string, a secret
// never is.
func describeCaller(id identity) string {
	if normalized := id.normalizedID(); normalized != "" {
		return normalized + " (via " + id.Source.String() + ")"
	}
	return "an unresolved caller"
}

// describeForm names which shape the authorization call will take, which is the
// single most useful thing to know when local authentication is misbehaving.
func describeForm(id identity) string {
	if id.State == identityResolved {
		return "asserting the identity"
	}
	return "forwarding the credential"
}

// isMarkerHeader mirrors the unconditional forwarding done on the request side,
// so the marker survives to the upstream request and therefore through an
// internal redirect.
func isMarkerHeader(config PluginConfig, name string) bool {
	return config.AuthCache.hasMarkerHeader() && http.CanonicalHeaderKey(name) == http.CanonicalHeaderKey(config.AuthCache.Header)
}

// failureModeAllows reports whether the failure-mode policy lets a failed
// authorization through.
//
// Only the service's own failures qualify. A 401 or 403 is a real verdict and is
// never waived; 5xx is the only signal available for "no verdict came back",
// since Envoy synthesises 503 when it cannot connect and a timeout surfaces as
// 502, with nothing in the callback to tell either from a genuine upstream 5xx.
//
// Two widths, both off by default so the shipped behaviour stays fail closed:
// the authenticated form allows callers the plugin named itself, which keeps an
// outage from interrupting legitimate traffic; the blanket form allows everyone,
// including requests carrying no credential at all.
func failureModeAllows(config PluginConfig, id identity, statusCode int) bool {
	if statusCode < http.StatusInternalServerError {
		return false
	}
	if config.FailureModeAllow {
		return true
	}
	// Anonymous is a resolved identity that names nobody, so it must not count
	// as authenticated here. It only ever arises on public routes, which never
	// reach this call, but the two concepts are worth keeping apart.
	return config.FailureModeAllowAuthenticated &&
		id.State == identityResolved && !id.Anonymous
}

// statusForFailure maps an authorization failure onto the status the client
// sees. The service's own failures are reported as status_on_error rather than
// leaked as a 5xx; a real verdict is relayed unchanged.
func statusForFailure(config PluginConfig, statusCode int) uint32 {
	if statusCode >= http.StatusInternalServerError {
		return config.StatusOnError
	}
	return uint32(statusCode)
}

// handleAuthzFailure applies the failure-mode policy to a non-200 result,
// sending the client response when the verdict is to reject.
//
// It reports whether the request may proceed but deliberately does not resume
// it: only the async callback ever paused the request, so only the async
// callback may resume, and folding that decision in here would mean resuming a
// request that is still iterating on the synchronous dispatch-error path.
func handleAuthzFailure(config PluginConfig, id identity, model, conn string, statusCode int,
	responseHeaders http.Header, responseBody []byte) (allowed bool) {
	if failureModeAllows(config, id, statusCode) {
		// Letting a request through means finishing it like any other allowing
		// path, not just returning true. Skipping this would send the client's
		// own cookie to the model -- and through ai-proxy to a third-party
		// provider -- which is the one thing headers_to_remove exists to
		// prevent, and it would leave the consumer unset in the access log.
		consumer, _ := localConsumer(id)
		applyUpstreamRewrites(config, consumer)
		// Same rule as everywhere else: no server 200, so the plugin owes the
		// marker. Without it the redirect pass of this very request would fail
		// to name the caller and be rejected -- during the outage the marker is
		// there to survive.
		applyMarker(config, id, model, consumer, conn, time.Now())

		// Worth a line: it only fires while the authorization service is down,
		// and it is the one outcome where a request was served without a
		// verdict. Upstream signals this by adding a header to the upstream
		// request instead, which here would travel to a model worker that does
		// not read it -- and, through ai-proxy, to a third-party provider.
		log.Warnf("%s: authorization service failed (%d); allowing %s without a verdict",
			pluginName, statusCode, describeCaller(id))
		return true
	}

	clientHeaders := responseHeaders
	if config.Authz.AllowedClientHeaders != nil {
		clientHeaders = http.Header{}
		for name, values := range responseHeaders {
			if len(values) > 0 && matchesHeader(config.Authz.AllowedClientHeaders, name) {
				clientHeaders.Set(name, values[0])
			}
		}
	}

	body := responseBody
	if len(body) == 0 {
		body = nonEmptyRejectionBody
	}
	_ = sendResponse(statusForFailure(config, statusCode), detailUnauthorized, clientHeaders, body)
	return false
}
