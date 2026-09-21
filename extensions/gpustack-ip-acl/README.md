# gpustack-ip-acl Plugin

## Introduction

Per-consumer source-IP blacklist/whitelist enforcement at the gateway, mirroring the GPUStack Enterprise API key IP ACL semantics.

The **consumer** is the gateway's authenticated caller identity (the `x-mse-consumer` header — injected by `gpustack-ext-auth` at AUTHN/360 in the GPUStack deployment; Higress's built-in key-auth in stock deployments). In GPUStack each API key maps to a consumer, so per-consumer rules give the same granularity as the enterprise per-API-key IP ACL.

- **deniedCidrs** — blacklist, evaluated first and always takes precedence.
- **allowedCidrs** — whitelist; when non-empty, the caller's IP must fall inside one of these ranges.
- A consumer with no rule (or a request without a consumer header) is unrestricted.
- Entries accept CIDR notation (`192.168.1.0/24`, `2001:db8::/32`) or a single host (`203.0.113.7`).
- Rejected requests get a `403 Forbidden` JSON local reply.

## Configuration

```yaml
realIPHeader: "x-forwarded-for"  # optional, trusted header holding the real client IP; falls back to the downstream source address
consumers:
  alice: # per-consumer rules
    allowedCidrs:
      - "192.168.1.0/24"
    deniedCidrs:
      - "10.0.0.0/8"
  bob:
    deniedCidrs:
      - "203.0.113.0/24"
  "gpustack-*": # wildcard: matches consumers by prefix
    allowedCidrs:
      - "10.0.0.0/8"
  "*-ci": # wildcard: matches consumers by suffix
    deniedCidrs:
      - "198.51.100.0/24"
  "*": # catch-all default for all other consumers
    deniedCidrs:
      - "192.0.2.0/24"
```

### Wildcard consumer keys

A key may contain a single `*`:

- `gpustack-*` — prefix match
- `*-ci` — suffix match
- `*` — catch-all default rule for every consumer

Resolution order: **exact names always win**, then wildcard rules in **declaration order** (first match wins) — put narrower patterns before broader ones, and `*` last. Keys with no `*` or with a `*` in the middle / multiple `*` are treated as literal exact names.

**⚠️ Only set `realIPHeader` when a trusted proxy in front of the gateway OVERWRITES the header** (not appends to it). The first value of the list is used; with `X-Forwarded-For` and no trusted overwriting proxy, a client that reaches the gateway directly can spoof its source IP and bypass the ACL. When unset, the plugin uses Envoy's downstream source address, which cannot be spoofed.

### Ingress / route-level overrides (matchRule)

The plugin supports Higress `matchRules`. A matched rule inherits `defaultConfig` and then applies **per-key overrides**: a consumer (exact name or wildcard) redeclared in the rule replaces its global rule entirely on that route; consumers not mentioned keep their global rules. `realIPHeader` is likewise inherited unless overridden. Override parsing never mutates the global config.

```yaml
matchRules:
  - ingress:
      - ai-route-admin.internal
    config:
      consumers:
        platform-team: # tightened on this route only
          allowedCidrs: ["10.8.1.0/24"]
        "*-ci": {} # unrestrict ci consumers here (empty redeclaration)
```

## Notes

- Enforcement is fail-closed: an unparseable client IP is rejected.
- A request in both an allowed and a denied range is **denied** — use this to carve holes out of broad allow ranges.
- **This plugin must run after the consumer header is injected.** In the GPUStack deployment `x-mse-consumer` is written by `gpustack-ext-auth` at **AUTHN/360** (gpustack core `gateway/ext_auth.py`), not by Higress key-auth. Use the same phase with a priority just below it (the example uses **AUTHN/350**). A priority above 360 makes the ACL silently no-op (header absent → every consumer unrestricted).
- **Broken configuration fails closed.** wasm-go's RuleMatcher drops a global parse error when matchRules exist (leaving a zero config = fail-open), and returning the error without matchRules removes the filter entirely. Instead, any invalid `defaultConfig` or matchRule config is replaced by a **deny-all** rule (every consumer rejected with 403) plus a CRITICAL log — a typo stops traffic instead of widening access. Consumers without a `x-mse-consumer` header (unauthenticated routes) remain unrestricted, as the ACL is consumer-keyed.
- **Phase buckets, not raw priorities, decide cross-plugin order.** AUTHN runs before UNSPECIFIED: `rate-limit` (UNSPECIFIED/600) and `token-usage` (UNSPECIFIED/400) execute *after* this plugin despite their larger priority numbers — rejected requests are neither quota-counted nor metered.

