---
title: GPUStack Rate Limit
keywords: [AI Gateway, GPUStack, Rate Limiting, Token Rate Limiting]
description: Redis-backed multi-dimensional sliding-window rate limiter for request-count and AI token-count quotas.
---

## Function Description

`gpustack-rate-limit` is a Redis-backed multi-dimensional limiter that supports
both **request-count** and **AI token-count** accounting at the same time,
organized as one or more `limit_combinations`. Each combination declares:

- a set of **match rules** (attributes extracted from HTTP header / query param /
  cookie / real client IP / consumer), and
- any combination of:
  - **`query_limits`** -- rolling-window request-count quota.
  - **`token_limits`** -- rolling-window AI token-count quota.
  - **`token_quota`** -- calendar-aligned AI token quota (per calendar day /
    month / year, anchored to a configurable timezone).

Rolling-window limits use a sliding window of fixed duration. Calendar-aligned
quotas reset on the natural calendar boundary (e.g. 1st of every month at
00:00 in the configured timezone) and are intended for monthly / yearly token
allowances that line up with billing cycles.

Request-count limits are checked and recorded atomically at the request phase
(check-and-add). Token-count limits (both `token_limits` and `token_quota`)
are:

1. **Checked** at the request phase: a request is rejected early if the token
   quota for the matched combination is already exhausted.
2. **Recorded** at the response phase: once the stream finishes, the real
   `total_tokens` of the response is added to the counter.

For token accounting the plugin reads `total_tokens` from the Envoy filter-state
key `ai_log`, which is published by upstream AI plugins (Higress `ai-statistics`
or the companion `gpustack-token-usage`). The plugin deliberately does not parse
the SSE body itself, to avoid duplicating the parsing already performed upstream.

## Runtime Properties

- Plugin execution phase: `UNSPECIFIED_PHASE`
- Plugin execution priority: `600`

The priority must be **lower** than that of the plugin which publishes `ai_log`
(e.g. `ai-statistics` has priority `200` in `CUSTOM` phase). In the response-phase
filter order this plugin therefore runs **after** the AI statistics plugin, so
that `ai_log` has already been written to the filter state by the time
`onHttpStreamDone` reads it.

This matches the runtime properties of Higress's `ai-token-ratelimit` plugin.

## Configuration

### Top-level fields

| Field                      | Type                | Required | Default              | Description                                                                                                                                      |
| -------------------------- | ------------------- | -------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `rule_name`                | string              | Yes      | -                    | Rate-limit rule name; used as the Redis key prefix. Should be unique across rule-level overrides that need independent counters.                 |
| `limit_combinations`       | array of object     | Yes\*    | -                    | List of rate-limit combinations. Required at rule-level. Optional at global-level when the global config only provides base fields (e.g. Redis). |
| `rejected_code`            | int                 | No       | `429`                | HTTP status code returned when a request is rejected. Must fall in `[100, 599]`.                                                                 |
| `rejected_msg`             | string              | No       | `Too many requests`  | Response body returned when a request is rejected. Must be non-empty (empty values fall back to the default).                                    |
| `show_limit_quota_header`  | bool                | No       | `false`              | When true, the rejected response carries the header `x-ratelimit-limited-key` whose value is the Redis key that triggered the rejection.         |
| `enable_on_path_suffix`    | array of string     | No       | AI endpoints\*\*     | Only enforce the limit when `:path` ends with one of these suffixes (after stripping the query string). `"*"` matches any path.                  |
| `enable_on_path_prefix`    | array of string     | No       | `["/model/proxy"]`   | Only enforce the limit when `:path` starts with one of these prefixes. An empty string `""` matches any path.                                    |
| `timezone`                 | string              | No       | `UTC`                | IANA timezone name (e.g. `Asia/Shanghai`) used to anchor every calendar-aligned `token_quota` boundary in this config. Deployment-wide.          |
| `redis`                    | object              | Yes      | -                    | Redis connection settings.                                                                                                                       |

\* Global-level configuration is allowed to omit `limit_combinations` -- in that
case it only supplies the base fields (Redis / `rejected_code` / `rejected_msg`)
and the actual rate-limit rules are declared in route/host/service rule-level
overrides. Rule-level parsing always runs full validation.

\*\* The default `enable_on_path_suffix` list targets common AI inference
endpoints so that the plugin is a no-op for health checks, doc pages and other
infrastructure traffic. Together with the default `enable_on_path_prefix`, a
request is subject to rate limiting only when the path matches at least one
suffix or prefix.

Default values:

```yaml
enable_on_path_suffix:
  - /completions
  - /embeddings
  - /images/generations
  - /audio/speech
  - /fine_tuning/jobs
  - /moderations
  - /image-synthesis
  - /video-synthesis
  - /rerank
  - /messages
  - /responses
enable_on_path_prefix:
  - /model/proxy
```

To disable path filtering entirely (apply the limits to every request), set:

```yaml
enable_on_path_suffix: ["*"]
```

An explicitly provided empty array `[]` opts out of that specific filter while
keeping the other one; the overall enable decision is the OR of both lists.

### `limit_combinations` fields

| Field           | Type             | Required | Default | Description                                                                                                 |
| --------------- | ---------------- | -------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `name`          | string           | Yes      | -       | Combination name; must be unique within `limit_combinations`. Used in the Redis key, logs, and metrics.     |
| `match`         | array of object  | Yes      | -       | List of dimension match rules. **Every rule must hit** for the combination to be activated.                 |
| `query_limits`  | object           | No\*     | -       | Rolling-window request-count quota (see [RateQuota](#ratequota-fields)). Triggers check-and-add at the request phase.      |
| `token_limits`  | object           | No\*     | -       | Rolling-window AI token-count quota. Triggers check at the request phase and add at the response phase.                    |
| `token_quota`   | object           | No\*     | -       | Calendar-aligned AI token quota (see [QuotaSpec](#quotaspec-fields)). Same check / add lifecycle as `token_limits`.         |

\* At least one of `query_limits` / `token_limits` / `token_quota` must be configured per combination.

### `match` rule fields (dimension)

| Field     | Type                                           | Required | Default | Description                                                                                                                                                              |
| --------- | ---------------------------------------------- | -------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `source`  | enum: `param`, `header`, `cookie`, `ip`, `consumer` | Yes      | -       | Where the attribute is extracted from.                                                                                                                                   |
| `name`    | string                                         | See right | - | The source-specific identifier. Required for `header` / `param` / `cookie` / `ip`; ignored for `consumer` (fixed to `x-mse-consumer`). For `ip`, the value of `name` is the header that carries the real client IP (e.g. `x-real-ip`). |
| `value`   | string                                         | Yes      | -       | Match pattern. See [Match value semantics](#match-value-semantics).                                                                                                      |

### Match value semantics

The `value` field is a string; its match strategy is inferred by the plugin in
this order:

1. `value == "*"` -> **wildcard**: every non-empty input matches.
2. `source == ip` -> **IP or CIDR**: the value is parsed as an IP (treated as
   `/32` for IPv4 or `/128` for IPv6) or a CIDR network; the extracted value is
   parsed as an IP and checked for membership.
3. `value` starts with `regexp_capture:` -> **regexp with capture-group key
   extraction** (RE2). The pattern must contain at least one capturing group
   `(...)`. On hit, the **first capture group** is used as the Redis key
   fragment instead of the full extracted value, so two requests whose
   extracted values differ only in a non-captured portion share the same
   bucket. Use non-capturing groups `(?:...)` for parts you only want to
   match but not include in the key. See the example below.
4. `value` starts with `regexp:` -> **regexp** (RE2): everything after the
   `regexp:` prefix is compiled as the pattern. The whole extracted value
   is used as the key fragment (different inputs land in different buckets
   even when both match).
5. Otherwise -> **exact** (case-sensitive) string match.

#### Example: cross-prefix bucket aggregation with `regexp_capture`

In a deployment where `x-mse-consumer` may take either of two shapes, the
API form `<access_key>.gpustack-<user-id>` and the UI form
`gpustack-<user-id>`, you typically want both to count against the **same
per-user bucket**. Plain `regexp:` cannot do this -- it would keep the full
header value as the key, so the two shapes end up in different buckets even
when they refer to the same user.

`regexp_capture:` solves it:

```yaml
match:
  - source: consumer
    value: 'regexp_capture:^(?:[^.]+\.)?(gpustack-.+)$'
```

- `(?:[^.]+\.)?` -- optional non-capturing access_key prefix
- `(gpustack-.+)` -- the captured user-id segment used as the key fragment

Both `ak-x.gpustack-1` and `gpustack-1` map to the same key fragment
`consumer=gpustack-1`, so they share the bucket
`<rule>|<combo>|consumer=gpustack-1|...`.

### RateQuota fields

Exactly one of the following window fields should be set. The first non-nil one
in the order `per_second -> per_minute -> per_hour -> per_day -> per_custom`
takes effect.

| Field                   | Type | Required | Default | Description                                                                  |
| ----------------------- | ---- | -------- | ------- | ---------------------------------------------------------------------------- |
| `per_second`            | int  | No\*     | -       | Allowed count per second.                                                    |
| `per_minute`            | int  | No\*     | -       | Allowed count per minute.                                                    |
| `per_hour`              | int  | No\*     | -       | Allowed count per hour.                                                      |
| `per_day`               | int  | No\*     | -       | Allowed count per day.                                                       |
| `per_custom`            | int  | No\*     | -       | Allowed count per custom window; must be used together with `custom_window_seconds`. |
| `custom_window_seconds` | int  | Paired   | -       | Size of the custom window in seconds.                                        |

\* At least one window field must be set; otherwise the quota is treated as
"not configured" and the combination silently skips that kind of limit.

### QuotaSpec fields

A calendar-aligned token quota. Exactly one of `each_day` / `each_month` /
`each_year` must be set; the int value is the token limit for that period.
The period boundary is anchored to the deployment-wide top-level `timezone`
field -- e.g. `each_month` resets on the 1st of every month at 00:00 in
that timezone.

| Field        | Type | Required | Default | Description                                                                          |
| ------------ | ---- | -------- | ------- | ------------------------------------------------------------------------------------ |
| `each_day`   | int  | No\*     | -       | Tokens allowed per calendar day (00:00 - 24:00 in the deployment timezone).          |
| `each_month` | int  | No\*     | -       | Tokens allowed per calendar month (resets on the 1st at 00:00 in the deployment timezone). |
| `each_year`  | int  | No\*     | -       | Tokens allowed per calendar year (resets on Jan 1 at 00:00 in the deployment timezone).    |

\* Exactly one of `each_day` / `each_month` / `each_year` must be set.

The timezone is intentionally **not** a per-spec field: a deployment almost
always has one timezone, so declaring it once at the top level avoids
per-spec repetition and prevents accidentally mismatched boundaries across
combinations.

#### Why no `query_quota`?

The plugin intentionally does not offer a calendar-aligned **request-count**
quota. The rolling-window `query_limits` already covers the typical
"N requests per day / hour / minute" anti-abuse and fairness use cases. AI
billing is almost always token-based, so a calendar quota is only meaningful
for tokens.

#### Calendar quota internals

The Redis key for `token_quota` uses a stable label (e.g. `t:each_month`)
instead of a window-second number, so the key does not change within a
period. The deployment timezone is **not** encoded into the key -- it is
a deployment-wide constant, so changing it reinterprets every existing key
under the new boundary (which is the user-intended behaviour).

The plugin computes a dynamic `now - period_start` window at both the
request and response phases and passes it to the lua script. The lua
script treats this as a larger / shorter sliding window. A long stream
that crosses the period boundary records its tokens into the period in
which the response **ends**.

The Redis EXPIRE TTL is **not** derived from the window. For calendar
quotas, the dynamic window is near-zero at period start (e.g. 1s at
00:00:01 on the 1st of the month), and using `window * 2` for TTL would
evict the key within seconds, prematurely resetting the quota. The
plugin sets TTL to `period_end - now + 300s` (5-minute grace), so the
key always lives until the period ends regardless of how early in the
period the first write happens. Rolling-window combos still use
`window * 2` as before.

### Redis fields

| Field          | Type   | Required | Default                                   | Description                                                                                          |
| -------------- | ------ | -------- | ----------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `service_name` | string | Yes      | -                                         | Redis service FQDN. For static DNS services the name ends with `.static`.                            |
| `service_port` | int    | No       | `6379`, or `80` for names ending `.static` | Redis service port.                                                                                  |
| `username`     | string | No       | -                                         | Redis username (Redis 6.0+ with ACL enabled).                                                        |
| `password`     | string | No       | -                                         | Redis password.                                                                                      |
| `timeout`      | int    | No       | `1000`                                    | Per-command timeout in milliseconds.                                                                 |
| `database`     | int    | No       | `0`                                       | Redis database index.                                                                                |

## Redis Key Layout

Every active combination maps to one ZSET key per limit kind. The key is
built as:

```text
<rule_name>|<combo.Name>|<dim1>|<dim2>|...|<kind>:<period>
```

- `<kind>` is `q` for `query_limits` and `t` for `token_limits` / `token_quota`.
- `<period>` is:
  - For rolling limits (`query_limits` / `token_limits`): the window size
    in seconds, e.g. `60s`, `3600s`.
  - For calendar quotas (`token_quota`): the stable period label, e.g.
    `each_month`, `each_year`. The deployment timezone is **not** encoded
    into the key -- it is a top-level constant.
- Each dimension fragment is `<source>:<name>=<value>` (or `<source>=<value>`
  when `name` is empty / for `consumer`).
- ZSET member is `<request-uuid>|<count>`: `1` for request-count limits,
  `total_tokens` for token-count limits and token quotas. ZSET score is the
  epoch second.

Example keys:

```text
myrule|premium-gpt4|header:x-api-key=premium-abc-123|param:model=gpt-4|q:60s
myrule|premium-gpt4|header:x-api-key=premium-abc-123|param:model=gpt-4|t:60s
myrule|premium-gpt4|consumer=tenant-a|t:each_month
myrule|premium-gpt4|consumer=tenant-a|t:each_year
```

### Reconciliation with external systems (e.g. GPUStack)

Because `token_quota` keys use a stable per-period label, an external
accounting system can read the same Redis ZSET to monitor or reconcile
current-period usage. Hot-path writes are owned by the plugin (`check_and_add`
for `query_limits`, `check` + `add` for `token_limits` / `token_quota`); an
external system should only write when **bootstrapping a tenant**,
**recovering after Redis data loss**, or **applying manual adjustments** --
never on every request, otherwise the count will be doubled.

When recovering after data loss, a single ZSET member is sufficient to
restore the period total -- the lua script just sums the `|<count>` suffixes
across all members, regardless of how many members there are. Use a member
ID prefix that does not collide with the plugin's UUIDs (e.g. `recovery-`)
so the two sources stay distinguishable, and pick a score within the current
period (typically `now - 1` to avoid the period-boundary edge).

If Redis is unreachable when a request arrives, the plugin **fails open** and
allows the request through (a warning is logged). This is the existing
behaviour for `query_limits` / `token_limits` and applies to `token_quota`
too -- the plugin never blocks traffic because of its own infrastructure
failure.

## Configuration Examples

### Example 1: Per-consumer request-count limit

```yaml
rule_name: per-consumer-rule
limit_combinations:
  - name: per-consumer
    match:
      - source: consumer
        value: "*"
    query_limits:
      per_minute: 60
redis:
  service_name: redis.static
```

### Example 2: Token limit for premium API keys on specific models

```yaml
rule_name: premium-ai-rule
limit_combinations:
  - name: premium-gpt4
    match:
      - source: header
        name: x-api-key
        value: "regexp:^premium-.+$"
      - source: param
        name: model
        value: "gpt-4"
    query_limits:
      per_minute: 100
    token_limits:
      per_minute: 200000
redis:
  service_name: redis.static
```

### Example 3: CIDR-based limit on internal network

```yaml
rule_name: internal-net-rule
limit_combinations:
  - name: internal-10-8
    match:
      - source: ip
        name: x-real-ip
        value: "10.0.0.0/8"
    query_limits:
      per_second: 1000
redis:
  service_name: redis.static
```

### Example 4: Custom window and cookie-based limit

```yaml
rule_name: session-rule
limit_combinations:
  - name: session
    match:
      - source: cookie
        name: sid
        value: "*"
    query_limits:
      per_custom: 500
      custom_window_seconds: 30   # 500 requests per 30 seconds
redis:
  service_name: redis.static
```

### Example 5: Monthly token quota per consumer

A 1M-token monthly allowance per consumer, anchored to UTC. Combined with a
rolling-minute rate limit so a single tenant cannot burn through the month
in seconds.

```yaml
rule_name: monthly-token-budget
timezone: UTC                   # deployment-wide; default if omitted
limit_combinations:
  - name: per-consumer-monthly
    match:
      - source: consumer
        value: "*"
    token_limits:
      per_minute: 200000          # rolling burst guard
    token_quota:
      each_month: 1000000         # 1M tokens / calendar month
redis:
  service_name: redis.static
```

For a deployment whose billing cycle aligns with a non-UTC timezone, set the
top-level `timezone` accordingly (e.g. `Asia/Shanghai`). All `token_quota`
boundaries in this config will use that timezone.

### Example 6: Global + route-level override (aggregation semantics)

`defaultConfig` and each `matchRules[].config` express two different scopes
and are **aggregated**, not "replaced when matched":

- Scalar fields (`rule_name`, `timezone`, `rejected_code`, `redis`, ...) and
  the path-filter lists follow classic "inherit from default, override if
  redeclared" semantics.
- **`limit_combinations`** declared at the rule-level are **appended** onto
  those declared at the default level. Every request that matches a rule
  applies BOTH sets of combos, checked atomically in a single Redis Eval.

This lets you declare deployment-wide buckets (e.g. a per-consumer monthly
quota that is the same regardless of model) **once** at the default level,
and route-specific buckets (e.g. a gpt-4 sub-cap) at each ingress without
having to repeat the deployment-wide combos in every route.

`Validate` enforces unique combo names across the merged list, so a route
**cannot** silently shadow a default-level combo: if you need to relax a
default-level combo for a specific route, give the route's combo a distinct
name; if you really need to replace one, declare it only at the route
level and not at the default level.

#### Default-level config -- shared layer

Declares Redis, timezone, and the cross-route per-consumer combo that every
route should apply:

```yaml
rule_name: ai-budget
timezone: UTC
rejected_code: 429
rejected_msg: "Too Many Requests"
show_limit_quota_header: true
redis:
  service_name: redis.static

# Layer 1: applied to every request that matches ANY route below.
limit_combinations:
  - name: per-consumer
    match:
      - source: consumer
        value: "*"
    query_limits:
      per_minute: 60
    token_limits:
      per_minute: 100000
    token_quota:
      each_month: 5000000      # 5M tokens / month per consumer (cross-model)
```

#### Route-level overrides -- model-specific layer

Each model ingress only declares its own model-specific sub-cap. The
deployment-wide `per-consumer` combo above is appended automatically at
runtime, so each request runs both checks.

```yaml
_rules_:
  - _match_route_:
      - llm-gpt-4
    limit_combinations:
      - name: model-gpt-4      # combo name must be globally unique
        match:
          - source: consumer
            value: "*"
        query_limits:
          per_minute: 30
        token_limits:
          per_minute: 50000
        token_quota:
          each_month: 1000000  # 1M gpt-4 tokens / month per consumer

  - _match_route_:
      - llm-gpt-3-5
    limit_combinations:
      - name: model-gpt-3-5
        match:
          - source: consumer
            value: "*"
        token_quota:
          each_month: 10000000

  - _match_route_:
      - llm-claude-3
    limit_combinations:
      - name: model-claude-3
        match:
          - source: consumer
            value: "*"
        token_quota:
          each_month: 2000000
```

#### Resulting Redis keys for one user `alice`

A request through the `llm-gpt-4` route runs **both** layers in one Redis
Eval:

```text
ai-budget|per-consumer|consumer=ak-x.gpustack-alice|q:60s
ai-budget|per-consumer|consumer=ak-x.gpustack-alice|t:60s
ai-budget|per-consumer|consumer=ak-x.gpustack-alice|t:each_month
ai-budget|model-gpt-4|consumer=ak-x.gpustack-alice|q:60s
ai-budget|model-gpt-4|consumer=ak-x.gpustack-alice|t:60s
ai-budget|model-gpt-4|consumer=ak-x.gpustack-alice|t:each_month
```

A request through `llm-gpt-3-5` shares the same `per-consumer` keys (Layer
1 is identical because the combo originates from the default-level config)
but uses its own `model-gpt-3-5` keys. So alice's monthly cross-model 5M
budget is shared between routes, while each model has its own sub-cap.

## Deployment Example

The plugin relies on Redis; deploy Redis first:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  labels:
    app: redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
        - name: redis
          image: redis
          ports:
            - containerPort: 6379
---
apiVersion: v1
kind: Service
metadata:
  name: redis
  labels:
    app: redis
spec:
  ports:
    - port: 6379
      targetPort: 6379
  selector:
    app: redis
```

Token-count limits depend on an AI statistics plugin (either Higress
`ai-statistics` or the companion `gpustack-token-usage`) that writes the
`ai_log` filter state. Deploy it together with this plugin -- the `phase`
and `priority` above guarantee the correct execution order.

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: gpustack-rate-limit
  namespace: higress-system
spec:
  defaultConfig:
    rule_name: default-rule
    limit_combinations:
      - name: per-consumer
        match:
          - source: consumer
            value: "*"
        query_limits:
          per_minute: 60
        token_limits:
          per_minute: 200000
    redis:
      service_name: redis.dns
      service_port: 6379
  url: oci://<your-registry>/plugins/gpustack-rate-limit:<version>
  phase: UNSPECIFIED_PHASE
  priority: 600
```

## Behavior Notes

- **Fail-open**: when Redis is unreachable or the Lua script returns an error,
  the request is allowed through and a warning is logged. The plugin never
  blocks traffic because of its own infrastructure failure.
- **Token limits already at quota**: rejected at the request phase before the
  upstream is touched. Using check-only at the request phase and add-only at
  the response phase means concurrent requests can occasionally exceed the
  quota by a small amount; this is the same trade-off every production token
  limiter makes.
- **Cross-plugin ordering**: if you deploy both `ai-statistics` (or
  `gpustack-token-usage`) and `gpustack-rate-limit`, make sure the statistics
  plugin publishes `ai_log` before this plugin's response-phase hook runs. The
  default phase/priority above respects that ordering against upstream
  `ai-statistics`.
- **VM rebuild**: the plugin triggers a VM rebuild every 1000 requests or when
  the VM memory reaches 200 MiB to guard against long-running memory-leak
  accumulation.
- **Non-empty response body is required**: under Higress's AI-route filter chain,
  a local reply with an empty body is queued by Envoy but never flushed to the
  downstream client (access log shows `response_code=429` / `bytes_sent=0` /
  `response_flags=DC`). `rejected_msg` therefore falls back to
  `Too many requests` when left unset, so the reply always has a body.
