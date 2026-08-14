# gpustack-ext-auth

Authenticates GPUStack API keys at the gateway and defers authorization to the
GPUStack server. Replaces upstream `ext-auth` on GPUStack inference routes; the
request/response header contract is upstream's, the configuration shape is not.

Rationale and trade-offs live in the GPUStack design doc
`designs/API-Key认证简化设计.md`; implementation hazards live in the code
comments. This file documents configuration.

## Status

Implemented: the authorization call, the route gate, marker verification
and minting, local key authentication, the verification cache, identity
assertion, local allow on public routes, and a narrow fail-open for
authenticated callers.

The verification cache needs the server to return `X-GPUStack-Key-Ref` on the
authorization response. Until it does, credentials without a digest keep going
to the server on every request, as they do today.

## Placement

Shared state goes in `defaultConfig`; per-route overrides go in the CR's
`spec.matchRules`, keyed by ingress name.

The two are mutually exclusive with a hand-written `defaultConfig._rules_`: a
non-empty `matchRules` *overwrites* `_rules_` wholesale
(`convertIstioWasmPlugin`, `pkg/ingress/config/ingress_config.go`). That matters
because `matchRules` can only express `ingress` / `service` / `domain` — nothing
emits `_match_route_prefix_` — so a catch-all rule cannot live there. It does not
need to: `route_match_regexes` is a plugin-level check, which frees `matchRules`
for the only thing that needs a rule at all, declaring a route public.

## Example

```yaml
spec:
  defaultConfig:
    route_match_regexes: ["^<namespace>/ai-route-route-"]

    local_auth:
      enabled: true
      keys:
        "3192253c1f4a9b7e":
          exp: 1790000000 # Unix seconds; null or absent means never
          digest: "s128$<salt>$<hash>"
          user_id: 7
      refs:
        "58": { exp: null } # keyed by api_keys.id

    authz:
      endpoint:
        path: /token-auth
        request_method: GET
        service_name: <gpustack server registry>
        service_port: 80
      timeout: 30000
      authorization_request:
        allowed_headers:
          - { exact: X-Real-IP }
          - { exact: X-Forwarded-For }
          - { exact: x-higress-llm-model }
          - { exact: x-api-key }
          - { exact: cookie }
          - { exact: X-GPUStack-Auth-Token }
        headers_to_add:
          X-GPUStack-Auth-Token: <derived gateway token>

    upstream_request:
      headers_to_remove: [cookie]

    auth_cache:
      header: x-gpustack-auth-cache
      signing_key: <64 hex chars>

    status_on_error: 403
    failure_mode_allow_authenticated: false

  # One entry per public route. Non-public routes need none: they fall back to
  # defaultConfig, which the route gate has already claimed.
  matchRules:
    - ingress:
        - <namespace>/ai-route-route-42.internal
        - <namespace>/ai-route-route-42.fallback.internal
      config:
        access_policy: public
```

## Fields

| Field | Meaning |
| --- | --- |
| `local_auth.enabled` | Master switch for local authentication. Off ⇒ every request forwards its credential to the server, as before. |
| `local_auth.keys` | `access_key` → `{exp, digest, user_id}`. Drives authentication, expiry and revocation. |
| `local_auth.refs` | `api_keys.id` → `{exp}`. Validity index only; cannot verify a credential, but gates and invalidates the verification cache. |
| `authz.endpoint` | `/token-auth` location. `service_name` and `path` are required. |
| `authz.endpoint_mode` | Optional; `forward_auth` is the only accepted value. An explicit `envoy` is rejected at parse time. |
| `authz.timeout` | Milliseconds, default 1000. |
| `authz.authorization_request.allowed_headers` | Client headers forwarded to the authorization call. Matchers: `exact`, `prefix`, `suffix`, `contains`, `regex`. Names always compared case-insensitively. |
| `authz.authorization_request.headers_to_add` | Static headers added to the authorization call. |
| `authz.authorization_response.allowed_client_headers` | Optional; filters headers relayed back on a rejection. |
| `upstream_request.headers_to_remove` | Headers stripped from the request going to the model. |
| `auth_cache.header` | Name of the signed marker header. |
| `auth_cache.signing_key` | HMAC key for the marker. Absent ⇒ the plugin does not touch markers. |
| `status_on_error` | Status returned when the authorization service fails. Default 403. |
| `failure_mode_allow` | Allow **everything** when the authorization service fails, credentialed or not. Default false. |
| `failure_mode_allow_authenticated` | Allow only callers the plugin resolved locally when the service fails. Default false. |
| `route_match_regexes` | Route names this plugin owns. Empty or absent matches nothing. Not anchored implicitly. |
| `matchRules[].config.access_policy` | `public` on public routes only. |

`local_auth`, `authz`, `auth_cache`, `upstream_request` may all be overridden per
rule; a rule inherits whatever it does not redeclare. Redeclaring `keys` /
`refs` / `headers_to_remove` replaces the collection rather than merging.

### `route_match_regexes` is the safety gate

The SDK's rule matcher falls back to the global config when no rule matches, so
"no rule matched" cannot by itself mean "not ours" — everything in
`defaultConfig` would otherwise reach every route on the gateway, other tenants
included. The gate is therefore a plugin-level check against Envoy's
`route_name`, evaluated before anything else.

Empty or absent matches **nothing**: an unconfigured plugin is inert rather than
one that starts authenticating traffic it was never pointed at. Setting it to
`[]` is also how to switch the plugin off without removing the CR.

Patterns are not anchored implicitly. `"^<namespace>/ai-route-route-"` is the
usual shape; the namespace prefix is present only when the ingresses live
outside the gateway's own namespace.

`access_policy` must still appear only on a rule, never in `defaultConfig` —
there it would declare every route public.

### How a caller is named

Four tiers, first hit wins. Every one of them re-checks the identity against
`keys` / `refs` before use, which is why revocation needs no cache-invalidation
step of its own — removing the entry is the whole of it.

| Tier | Source | Covers |
| --- | --- | --- |
| 0 | Signed marker on the request | Any identity, on a redirect pass where `Authorization` has already been replaced |
| 1 | `keys` lookup + one `sha256` | Any key that has a digest. The access key comes from the credential: read from it for `gpustack_<ak>_<sk>`, recomputed by hashing the whole string for a custom key, exactly as `get_key_pair` does |
| 2 | Verification cache in shared data | Keys without a digest — after the server has named them once |
| 3 | No credential at all, public route | Anonymous access |

Anything still unnamed goes to the server with its credential, as before.

### A marker is only honoured on the connection it was minted on

Nothing strips a client-supplied `x-gpustack-auth-cache`, and a marker travels
on the **upstream** request — so whoever receives those requests, whether a
worker, an APM or a third-party provider, is handed a fresh bearer for the
caller with every one of them.

What keeps that from being a standing ability to act as that caller is the
`conn` claim: the downstream `<ip>:<port>` the marker was minted on. An internal
redirect runs on that very connection — the client is still waiting on it — so
the pass the marker exists for still resolves, while anyone replaying it is on
a connection of their own and does not.

The value comes from Envoy's `source.address` rather than from a header, so a
client can neither set it nor choose it. Envoy's connection id would be the more
obvious choice and is the wrong one: it is a counter that restarts at zero in
every gateway pod, while a marker signed on one pod verifies on any other, so
two pods hand out colliding ids by construction.

Either side of the comparison being empty is a refusal, not a match. Comparing
two unreadable values would pass, which would disable the check exactly when the
property is unavailable; for the same reason a marker is not minted at all when
the address cannot be read.

The same binding covers the marker the **server** mints. When the plugin cannot
name a caller it forwards the request, and the server signs a marker of its own
with a key this plugin cannot verify — so on the fallback pass the plugin
forwards that marker rather than resolving it locally, and the server is what
checks it. The server has no way to read `source.address`, so the plugin sends
it as `x-gpustack-downstream-conn` on every authorization call: the server binds
its marker to the value it received at mint time and refuses it unless the same
value arrives on the pass that presents it. The header is set from
`source.address` and overwrites any client-supplied copy, so it names the
connection the client is on, not one it asked for. Without this the server's
marker — which carries no such claim of its own — would be replayable from any
connection, the exact gap this plugin's own binding closes for the markers it
signs.

### A cookie or basic credential suspends tiers 1 and 2

The server's `authenticate_request` is an if/elif chain — basic, then cookie,
then bearer / x-api-key — so whenever one of the first two is present the bearer
is **never reached** there. Naming the caller from that bearer here would answer
a different question than the one this plugin stands in for, so tiers 1 and 2 do
not run and the request goes to the server with its credentials.

Three ways it would otherwise diverge, all silent:

| Request | Server | Without this rule |
| --- | --- | --- |
| Valid cookie + mistyped bearer | Allowed, as the session | **401** |
| Valid cookie + valid bearer | Allowed as the session: consumer names the *user*, and the key's `scope` / `allowed_model_names` do not apply | Allowed as the *key*, with both |
| Expired cookie + valid bearer | **401** — the cookie branch is taken and returns nobody rather than falling through | Allowed |

Tier 0 is exempt. A marker records a decision the server already made, or a
public-route skip, and the fallback pass that reads it is an internal redirect
of the very same request. Tier 3 is unreachable here anyway, since it requires
that no credential at all be present.

The cost is confined to requests carrying both a session and an API key, which
is not a shape any first-party client produces: the console authenticates with
its cookie alone, and a copied code sample runs outside the browser with only
its key.

The verification cache is keyed by `sha256(credential)` and holds the
`api_keys.id` and consumer the server returned. It is written once per
credential rather than per request, read only on the paths that reach tier 2,
and invalidated by `refs` membership rather than by time. An identity absent
from `refs` is never cached: a key created moments ago simply keeps going to the
server until its config push lands, instead of hitting and failing a re-check.

Because a tier-2 hit is a *resolved* identity, a credential that only tier 2 can
name survives a server outage under `failure_mode_allow_authenticated` exactly as
a tier-1 one does — but only once the server has named it, and only in the Envoy
process that heard the answer. A custom key that carries a digest does not need
any of that: tier 1 recomputes its access key from the credential and resolves it
cold, which is what makes an outage survivable for a key nobody has used yet.

This is the plugin's only use of shared data — markers are stateless JWTs — and
its whole footprint is conditional. `refs` is non-empty only where
`GATEWAY_AUTH_ALLOW_CUSTOM_KEYS` is off; in the default configuration custom keys
carry a digest, resolve at tier 1, and both the read and the write short-circuit
before touching shared data. The cache is inert there and stores nothing.

Where it is live, shared data has no TTL, no delete and no way to enumerate keys,
so it cannot be swept. A tier-2 hit whose `refs` re-check fails is evicted on the
spot — the credential is in hand and the entry is known dead — by clearing its
value; the platform keeps the key string, so this reclaims a revoked-but-still-
presented entry, not one that is simply never seen again. That residual is bounded
by the distinct switch-off custom credentials a pod sees in its lifetime, with the
live working set inside it bounded by the `refs` byte budget, and is cleared on
restart. If that bound ever stops holding, an expiry stamp inside the value is the
mechanism to add, since the platform offers none.

### Headers the plugin handles itself

Do not declare these; the plugin manages them unconditionally and an entry can
only misconfigure them.

| Header | Handling |
| --- | --- |
| `X-Mse-Consumer` | Client copy removed on the way in, authoritative value stamped on the way out. Name is hard-coded, matching every other Higress plugin. |
| `x-gpustack-auth-cache` | Forwarded to the authorization call and relayed to the upstream request. |
| `X-GPUStack-Access-Key`, `X-GPUStack-Key-Ref` | Set when the plugin has resolved an identity; any client-supplied copy is dropped. |
| `Authorization`, `x-api-key`, `cookie` | Forwarded when the identity is unresolved, stripped when it is not. |

`authorization_response.allowed_upstream_headers` is not supported: every header
this plugin relays has a fixed meaning.

## What the reconciler must populate

Beyond `deleted_at IS NULL`, `keys` and `refs` must exclude:

- **SYSTEM principals.** `cluster.registration_token` is a structurally normal
  `gpustack_<ak>_<sk>` key and is digest-eligible, so the digest predicate will
  not filter it — filter on principal kind explicitly. It is also what ai-proxy
  puts in `Authorization` on every fallback pass, so a SYSTEM key in `keys`
  would make that pass resolve as the system identity.
- **Inactive owners.** The server rejects a deactivated user's key.

Whether a custom key — one whose secret its user supplied — carries a digest is
the server's decision, not a property of the key: it lands in `keys` when the
deployment allows custom keys to be authenticated here, and in `refs` otherwise.
Nothing in this plugin tests for it. An entry with a digest is verified, one
without is only checked for validity, and that is the whole of the rule.

### Digest constructions

Two are accepted, distinguished by the value's own prefix.

| Prefix | Layout | Length |
| --- | --- | --- |
| `sha256` | `sha256$<32 hex salt>$<64 hex hash>` — as the server stores it | 104 |
| `s128` | `s128$<same salt>$<b64url hash>`, hash truncated to its leading 16 bytes | 60 |

The second is **derived from** the first, not a separate hash: same salt, same
hashed input, same digest, with the trailing 16 bytes dropped and the remainder
re-encoded. So the server keeps the long form in its database and shortens it on
the way into this config, which means existing keys shrink on the next reconcile
— no migration, and none is possible anyway, since the plaintext needed to
rewrite a stored digest stops reaching the server once its key is authenticated
here.

The hashed input is the same for both: the salt's **text** followed by the
secret, no separator. The salt contributes its text, not the bytes that text
encodes — decoding it first is the obvious-looking mistake and yields a digest
that never matches.

Truncating to 128 bits is not a weakening. The secret being verified is 128 bits
of CSPRNG output, so a second preimage costs 2^128 — exactly what guessing the
secret outright costs. The length is pinned to that and is deliberately not
configurable: it is a constant to reason about, not a knob that can be set to 64
and keep working, silently, at 2^64.

Length matters because this table ships inside a WasmPlugin CR, and etcd caps an
object at ~1.5 MiB: 60 characters instead of 104 is the difference between
roughly 6000 and 8600 keys authenticating at the gateway rather than at the
server.

A prefix rather than a config field is also what keeps a rollback safe. An older
gateway reading a value it cannot name falls through to the server; one that
recognised `sha256` and then compared a truncated hash against a full one would
reject outright — a 401 rather than a slower path.

The base64url encoding is **unpadded** (22 characters, no trailing `=`), and the
hash's length is checked against the prefix before anything is compared. A value
whose hash is the wrong length for the construction it names — padded, full-length
under `s128`, truncated under `sha256` — is treated as *unusable* rather than as a
mismatch, so the request falls through to the server exactly as an unknown prefix
would. That distinction is the whole point: a wrong length means the writer and
this build disagree about the construction, and spending a permanent 401 on that
would take out every key written that way at once.

A public route's `matchRules` entry must list **both** ingress names, primary and
`.fallback`; `route_ingress_names_for_plugins` returns both. The route gate must
cover both too, which the shared `ai-route-route-` prefix already does. A route emitting
`access_policy: public` must also have `auth_cache.signing_key` configured,
otherwise its fallback pass goes back to needing a live server.

`auth_cache.signing_key` must be a dedicated derived key —
`hmac(jwt_secret_key, b"gateway-auth-cache").hexdigest()` — never
`jwt_secret_key` itself, which also signs user sessions, and never the gateway
token, which is transmitted on every request. The plugin HMACs with the literal
characters of the hex digest; it does not decode them.

## Failure behaviour

"Failure" means the authorization call returned status ≥ 500 — Envoy synthesises
503 when it cannot connect, and a timeout surfaces as 502. A 401 or 403 is a
real verdict and is never waived.

| Request during an outage | neither flag | `failure_mode_allow_authenticated` | `failure_mode_allow` |
| --- | --- | --- | --- |
| Identity resolved locally | Rejected | **Allowed** | Allowed |
| Known key, wrong secret | Rejected before the call | Rejected before the call | Rejected before the call |
| Unknown key, or no credential | Rejected | Rejected | **Allowed** |

An allowed request still gets the same rewrites as any other: the consumer is
stamped when the gateway knows it, the client's cookie is stripped, and a marker
is minted so the request survives its own redirect pass.

Both flags default off, so the shipped behaviour is fail closed. The
authenticated form keeps an outage from interrupting legitimate traffic;
authorization is skipped for its duration, so a key valid for one model can
reach another until the server returns. The blanket form additionally admits
callers carrying no credential at all, which on an internet-facing gateway turns
an outage into an open inference proxy.

Public routes are unaffected: they never call the server.

## Observability

A public route makes no server call, so nothing in the request path logs
anything by default — the plugin looks the same as if it were not in the chain.
Two signals exist:

- **Always on:** `X-Mse-Consumer` reaches the access log via ai-statistics. A
  correct consumer on a public-route request is proof the plugin ran and named
  the caller.
- **At `debug`:** one line per request naming which tier resolved the caller and
  whether the call was skipped, asserted or credential-forwarding.

Fail-open is logged at `warn` whenever it fires, since a request served without
a verdict is worth seeing without turning the level up. Rejections carry a
`via_wasm::...gpustack-ext-auth.<detail>` response-code detail in the access log.

## Testing

```bash
make -C extensions test PLUGIN_NAME=gpustack-ext-auth
make -C extensions build PLUGIN_NAME=gpustack-ext-auth
```
