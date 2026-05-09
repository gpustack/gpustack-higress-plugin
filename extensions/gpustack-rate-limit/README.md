---
title: GPUStack Rate Limit
keywords: [AI Gateway, GPUStack, Rate Limiting, Token Rate Limiting]
description: Multi-dimensional sliding-window rate limiter for request-count and AI token-count quotas. Supports Redis-backed cluster-wide limiting and a built-in local (per-instance) fallback mode.
---

## Function Description

`gpustack-rate-limit` is a multi-dimensional limiter that supports both
**request-count** and **AI token-count** accounting at the same time, organized
as one or more `limit_combinations`. Each combination declares:

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

The plugin supports two **backend modes** selected at configuration time:

- **Redis mode** (when `redis` is configured): counters are stored in a shared
  Redis instance, so limits are enforced cluster-wide across all Higress replicas.
- **Local mode** (when `redis` is omitted): counters are stored in proxy-wasm
  shared data, scoped to a single Envoy instance. Limits are enforced
  per-replica; a deployment with N replicas effectively allows N times the
  configured quota in aggregate. Local mode is suitable for single-node
  deployments, development/testing, and lightweight per-instance anti-abuse
  guards where a Redis dependency is not desired.

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
| `rejected_msg`             | string              | No       | `Too many requests`  | Human-readable error message. Wrapped into a JSON envelope; see [Rejection response shape](#rejection-response-shape). Must be non-empty.        |
| `show_limit_quota_header`  | bool                | No       | `false`              | When true, the rejected response carries the header `x-ratelimit-limited-key` whose value is the Redis key that triggered the rejection.         |
| `enable_on_path_suffix`    | array of string     | No       | AI endpoints\*\*     | Only enforce the limit when `:path` ends with one of these suffixes (after stripping the query string). `"*"` matches any path.                  |
| `enable_on_path_prefix`    | array of string     | No       | `["/model/proxy"]`   | Only enforce the limit when `:path` starts with one of these prefixes. An empty string `""` matches any path.                                    |
| `timezone`                 | string              | No       | `UTC`                | IANA timezone name (e.g. `Asia/Shanghai`) used to anchor every calendar-aligned `token_quota` boundary in this config. Deployment-wide.          |
| `redis`                    | object              | No       | -                    | Redis connection settings. When omitted the plugin runs in **local mode** (per-instance counters). See [Backend Modes](#backend-modes).          |

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

Any subset of the following window fields may be set on a single `RateQuota`.
**Each declared window is enforced as an independent rolling-window bucket
with its own Redis key**, so a request must satisfy every configured window.
The natural pattern is "burst guard + sustained guard" (e.g. `per_second` +
`per_minute`) or "burst + sustained + abuse cap" (`per_second` + `per_minute`
+ `per_day`).

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
Window fields with non-positive values (or `per_custom` without a positive
`custom_window_seconds`) are silently dropped and do not contribute a
bucket.

### QuotaSpec fields

A calendar-aligned token quota. Any subset of `each_day` / `each_month` /
`each_year` may be set; the int value is the token limit for that period.
**Each declared period is enforced as an independent calendar bucket with
its own Redis key**, so a request must satisfy every configured period.
A common pattern is `each_day` + `each_month` ("daily fairness floor and
monthly billing cap"). Period boundaries are anchored to the deployment-wide
top-level `timezone` field -- e.g. `each_month` resets on the 1st of every
month at 00:00 in that timezone.

| Field        | Type | Required | Default | Description                                                                          |
| ------------ | ---- | -------- | ------- | ------------------------------------------------------------------------------------ |
| `each_day`   | int  | No\*     | -       | Tokens allowed per calendar day (00:00 - 24:00 in the deployment timezone).          |
| `each_month` | int  | No\*     | -       | Tokens allowed per calendar month (resets on the 1st at 00:00 in the deployment timezone). |
| `each_year`  | int  | No\*     | -       | Tokens allowed per calendar year (resets on Jan 1 at 00:00 in the deployment timezone).    |

\* At least one of `each_day` / `each_month` / `each_year` must be set, and
every set value must be a positive integer.

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

### Rejection response shape

When a request is rejected, the plugin returns a local reply with
`content-type: application/json` and a body that follows the OpenAI-style
error envelope:

```json
{
  "error": {
    "message": "Too many requests",
    "type": "rate_limit_exceeded",
    "code": 429
  }
}
```

- `message` is `rejected_msg` (default `Too many requests`).
- `code` is `rejected_code` (default `429`).
- `type` is always `rate_limit_exceeded`.

The status code of the HTTP response is also `rejected_code`.

To override the body schema entirely, set `rejected_msg` to a JSON object
literal. It is detected by the leading `{` and `json.Valid`, and is sent
verbatim without further wrapping. The `content-type: application/json`
header still applies.

```yaml
rejected_msg: '{"error":{"message":"配额已用尽","type":"quota_exhausted","code":429,"upgrade_url":"https://example.com/billing"}}'
```

The JSON envelope is required because `gpustack-token-usage` runs at a higher
priority and parses the response body as SSE chunks for streaming requests
(`stream: true`). A non-JSON body would be buffered as an "incomplete chunk"
and silently replaced with an empty body downstream — the client would see
the rejection headers but no body at all. Keeping the body valid JSON makes
it pass through that pipeline unchanged.

## Metrics

The plugin emits three Prometheus counters via the proxy-wasm metric API.
They appear on the Envoy `/stats` admin endpoint and, when scraped by
Prometheus through Higress's existing stats sink, become standard
`envoy_*` series. Emission is always-on; the per-event cost is one
`sync.Map` lookup plus one host-side counter increment.

### Emitted counters

| Metric                               | Increment unit          | Increment timing                                                              |
| ------------------------------------ | ----------------------- | ----------------------------------------------------------------------------- |
| `gpustack_rate_limit_request_total`  | 1 per request           | Once per request that ran rate-limit checks; labelled with `result`           |
| `gpustack_rate_limit_rejected_total` | 1 per rejected request  | When lua / local backend short-circuits with LIMITED; tripping combo labelled |
| `gpustack_rate_limit_value_total`    | The lua `count` written | Per declared window/period bucket that the request wrote into. A combo with stacked windows (e.g. `per_second` + `per_minute`) writes once per window per request, so `sum by (kind) (value_total)` is **not** request count when stacking is in use — pin the `period` label (or use `request_total` for request rate). |

`sum by (route) (gpustack_rate_limit_rejected_total)` always equals
`gpustack_rate_limit_request_total{result="limited"}` for the same `route`,
because lua's pass-1 short-circuit guarantees each rejected request
attributes to exactly one `(combo, kind, period, bucket)` tuple.

### Stat name layout

Stat names follow the Higress AI plugin convention so the existing
bootstrap stats_tags extractors fire automatically:

```text
route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.<metric>.rule.<rule>.combo.<combo>.kind.<kind>.period.<period>.bucket.<bucket>
route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.gpustack_rate_limit_request_total.rule.<rule>.result.<result>
```

| Slot       | Source                                                                                  |
| ---------- | --------------------------------------------------------------------------------------- |
| `route`    | Envoy property `route_name` (`none` when unset)                                         |
| `upstream` | Envoy property `cluster_name` (the AI upstream)                                         |
| `model`    | Request header `x-higress-llm-model` (set by Higress AI route / pre-route plugin)       |
| `consumer` | Request header `x-mse-consumer` (Higress consumer-identity header)                      |
| `metric`   | One of the three counter names above                                                    |
| `rule`     | Top-level `rule_name`                                                                   |
| `combo`    | The matched `limit_combinations[].name`                                                 |
| `kind`     | `query` / `token_rolling` / `token_calendar`                                            |
| `period`   | Window/period suffix. For rolling kinds: `1s` / `60s` / `3600s` / `86400s` / `<custom>s`. For `token_calendar`: `each_day` / `each_month` / `each_year`. Disambiguates per-window emissions when one combo declares multiple windows on the same kind. |
| `bucket`   | Sanitised dimension fragment (past `<rule>\|<combo>` in the Redis key)                  |
| `result`   | `passed` / `limited` (only on `request_total`)                                          |

Sanitisation: any byte outside `[a-zA-Z0-9_.-]` becomes `_`. Empty values
become `none`. **Dots are deliberately preserved in label values** so that
Higress's bootstrap stats_tags regex (which uses `((.*?)\.)` patterns)
backtracks correctly across dot-containing values like `qwen3-0.6b` or
`ai-route-route-1.internal`, surfacing them in the resulting Prometheus
label values verbatim. After Envoy converts the remaining `.` to `_` in
the metric name (the parts that were *not* extracted as labels), every
series ends up in the form
`route_<route>_upstream_<cluster>_model_<m>_consumer_<c>_metric_gpustack_rate_limit_*_total_...`.

### Default Prometheus appearance (no extra config)

Higress's [`envoy_bootstrap.json`](https://github.com/alibaba/higress/blob/main/istio/istio/tools/packaging/common/envoy_bootstrap.json)
ships built-in `stats_tags` regex extractors that match the leading
`route.<route>.upstream.<cluster>.` prefix and turn it into proper
Prometheus labels. With **zero** deployment changes:

```text
route_upstream_model_qwen3_0_6b_consumer_alice_metric_gpustack_rate_limit_rejected_total_rule_model_route_1_combo_1_default_kind_query_period_60s_bucket_header_x-higress-llm-model_qwen3-0_6b{
  ai_route="ai-route-route-1.internal",
  ai_cluster="outbound|80||model-1.static"
} 2
```

Two of the four AI slots (`ai_route`, `ai_cluster`) are extracted as
Prometheus labels for free. The remaining slots (`model`, `consumer`,
`rule`, `combo`, `kind`, `period`, `bucket`, `result`) live inside the
metric name string. To grep just our metrics:

```bash
curl -s localhost:15020/stats/prometheus | grep '_metric_gpustack_rate_limit_'
```

### Recovering the rest as Prometheus labels via `metric_relabel_configs`

The lowest-friction way to flatten the remaining slots into proper
Prometheus labels is at scrape time -- no Envoy restart, hot-reloadable
via SIGHUP. Add this to the `scrape_config` that targets the Higress
gateway:

```yaml
scrape_configs:
  - job_name: higress-gateway
    # ...your existing scrape settings (kubernetes_sd / static_configs / etc)...
    metric_relabel_configs:
      # rejected_total / value_total: 6 extra labels (rule/combo/kind/period/bucket + model/consumer)
      # The period anchor uses an enumerated capture (\d+s|each_day|each_month|each_year)
      # so the bucket-end '_(.+)$' cannot accidentally swallow it.
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: model
        replacement: '$1'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: consumer
        replacement: '$2'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: rule
        replacement: '$4'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: combo
        replacement: '$5'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: kind
        replacement: '$6'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: period
        replacement: '$7'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: bucket
        replacement: '$8'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_(rejected|value)_total_rule_(.+?)_combo_(.+?)_kind_(query|token_rolling|token_calendar)_period_(\d+s|each_day|each_month|each_year)_bucket_(.+)$'
        action: replace
        target_label: __name__
        replacement: 'gpustack_rate_limit_$3_total'

      # request_total: 4 extra labels (rule/result + model/consumer)
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_request_total_rule_(.+?)_result_(passed|limited)$'
        action: replace
        target_label: model
        replacement: '$1'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_request_total_rule_(.+?)_result_(passed|limited)$'
        action: replace
        target_label: consumer
        replacement: '$2'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_request_total_rule_(.+?)_result_(passed|limited)$'
        action: replace
        target_label: rule
        replacement: '$3'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_request_total_rule_(.+?)_result_(passed|limited)$'
        action: replace
        target_label: result
        replacement: '$4'
      - source_labels: [__name__]
        regex: '^route_upstream_model_(.+?)_consumer_(.+?)_metric_gpustack_rate_limit_request_total_rule_(.+?)_result_(passed|limited)$'
        action: replace
        target_label: __name__
        replacement: 'gpustack_rate_limit_request_total'
```

After scrape Prometheus sees:

```text
gpustack_rate_limit_rejected_total{
  ai_route="ai-route-route-1.internal", ai_cluster="outbound|80||model-1.static",
  model="qwen3-0_6b", consumer="alice", rule="model-route-1",
  combo="1-default", kind="query", period="60s", bucket="header_x-higress-llm-model_qwen3-0_6b"
} 2
```

#### ⚠ Keyword-collision caveat

The `metric_relabel` regex anchors on the literal substrings `_route_`,
`_upstream_`, `_model_`, `_consumer_`, `_metric_`, `_rule_`, `_combo_`,
`_kind_`, `_period_`, `_bucket_`, `_result_`. Because the underlying
separator (`.` → `_` after Envoy's Prometheus formatter) is identical to
the byte that can appear *inside* a value, **values containing one of
these substrings will silently mis-parse**. Concrete examples that break:

- A `combo` named `1_kind_premium` — `_kind_` appears inside the combo
  value, so `_kind_(query|token_rolling|token_calendar)` will lock onto
  the wrong position.
- A `bucket` whose dimension fragment contains `consumer_` (e.g. a
  custom header named `x-internal-consumer-id`) — the `_consumer_`
  anchor near the start of the metric name and inside the bucket value
  collide.

This is **not unique to our plugin** -- it is a fundamental limitation of
embedding labels in Envoy stat names: Envoy's Prometheus formatter
collapses `.` and `-` to `_`, so the dot-separator that is unambiguous in
the internal stat name disappears at scrape time. The same caveat applies
to ai-statistics, ai-token-ratelimit, and any other Higress AI plugin
that has not been promoted into `envoy_bootstrap.json`'s stats_tags list.

**Practical mitigations**:

- Avoid using the literal substrings `route`, `upstream`, `model`,
  `consumer`, `metric`, `rule`, `combo`, `kind`, `period`, `bucket`,
  `result` as segments of `rule_name` / `combo.name` / match values.
  Underscored variants (e.g. `gpu_routing` is fine, `gpu_route_id` is
  risky). The `period` slot's enumerated regex anchor
  (`\d+s|each_day|each_month|each_year`) makes it safer than the open
  `(.+?)` slots — a value matching the enumeration but appearing inside
  another field can still mis-parse, but typical config values don't.
- For the long-term clean solution, use an Istio EnvoyFilter to extend
  `bootstrap.stats_config.stats_tags` (the same mechanism Higress uses
  for `ai_route` / `ai_cluster` / `ai_model`); the regex there matches
  the `.`-separated internal stat name where the separator is guaranteed
  not to appear in values. We have not bundled this manifest yet —
  meaningful operator usage of it requires paired Prometheus + Grafana
  dashboards which we plan to ship together as a follow-up.

### Bucket cardinality control

`bucket` is the sanitised dimension fragment (e.g. `consumer_alice` or
`consumer_alice_header_x-higress-llm-model_qwen3-0_6b`). Its cardinality is
controlled entirely by how match values are written:

| Match value form                        | Bucket count                                        |
| --------------------------------------- | --------------------------------------------------- |
| `value: "*"` (wildcard)                 | 1                                                   |
| `value: "alice"` (exact)                | 1                                                   |
| `value: regexp_capture:^...(grp)...$`   | N (one per unique capture-group result)             |
| `value: regexp:.*`                      | One per unique extracted header value (⚠ unbounded) |
| `source: ip` without grouping           | One per unique client IP (⚠ unbounded)              |

For high-cardinality dimensions either use `regexp_capture:` to bucket
many inputs together, or accept the cardinality cost.

The bucket label maps 1:1 back to the Redis key (Redis mode) by reversing
the sanitisation. Operators can take a noisy `bucket` value from
Prometheus, reconstruct the Redis key, and `redis-cli ZRANGE` to inspect
the actual sliding-window contents.

### Example queries (assumes `metric_relabel_configs` applied)

```promql
# Rejection rate by combo/kind/period (top-level dashboard).
sum by (rule, combo, kind, period) (rate(gpustack_rate_limit_rejected_total[5m]))

# Per-window breakdown for a stacked rate combo (e.g. q:1s vs q:60s vs q:86400s
# for a single combo with per_second + per_minute + per_day all set).
sum by (period) (rate(gpustack_rate_limit_rejected_total{combo="stacked", kind="query"}[5m]))

# Top 10 noisiest buckets right now (drill-down).
topk(10, rate(gpustack_rate_limit_rejected_total[5m]))

# Token consumption rate by route. Pin a single (kind, period) tuple --
# value_total writes once per declared bucket, so an unpinned
# kind=~"token_.*" sum double-counts across token_rolling and token_calendar
# (and across stacked windows within either). The shortest token_rolling
# window is the most granular signal; pick whichever is configured.
sum by (ai_route) (rate(gpustack_rate_limit_value_total{kind="token_rolling", period="60s"}[5m]))

# Token consumption per consumer per model (the AI billing view).
# When token_quota declares both each_day and each_month, pick the period
# explicitly to avoid double-counting the same total_tokens twice (the lua
# script writes the same count to each period's ZSET).
sum by (consumer, model) (rate(gpustack_rate_limit_value_total{kind="token_calendar", period="each_month"}[5m]))

# Tokens consumed in current calendar month for a single consumer
# (counter is monotonic across periods; pick a window covering the period).
increase(gpustack_rate_limit_value_total{kind="token_calendar", period="each_month", consumer="alice"}[31d])

# Pass-through rate (per-request basis, not per-combo).
sum(rate(gpustack_rate_limit_request_total{result="passed"}[5m]))
  /
sum(rate(gpustack_rate_limit_request_total[5m]))
```

For `kind=token_calendar`, note that the underlying lua script's window
boundary rotates at the configured `timezone`, but Prometheus counters are
monotonic — you query by absolute time range, not by the plugin's notion
of a calendar period. Use a recording rule pinned to the period start if
you need a "current period only" gauge.

## Backend Modes

### Redis mode (cluster-wide)

When the top-level `redis` field is present the plugin stores all counters in
Redis using sorted sets (ZSETs) and a Lua script that atomically checks and
records every active combination in a single round-trip. Limits are enforced
**globally across all Higress replicas** sharing that Redis instance, making
Redis mode the correct choice for production deployments where accurate
cluster-wide quotas are required.

See the [Redis fields](#redis-fields) section for connection options.

### Local mode (per-instance)

When `redis` is omitted the plugin falls back to local mode: counters are kept
in proxy-wasm shared data, which is scoped to the individual Envoy process. The
same sliding-window algorithm is used, with CAS-based (compare-and-swap)
optimistic locking for thread safety across worker threads within one instance.

**Important constraint**: because each replica has its own independent counter,
a cluster with N replicas effectively allows up to N times the configured quota
in aggregate. Local mode is therefore appropriate when:

- Running a **single Higress replica** (the quotas are exact).
- Using the limits as **lightweight per-instance anti-abuse guards** where
  occasional over-admission across replicas is acceptable.
- **Development or testing** where a Redis dependency is not desired.

For production clusters where accurate cross-replica quotas matter (e.g. monthly
token budgets tied to billing), always configure `redis`.

#### Local mode limitations

| Capability | Redis mode | Local mode |
| --- | --- | --- |
| Cluster-wide quota enforcement | Yes | No — per-replica only |
| `token_quota` calendar alignment | Exact across all replicas | Each replica resets independently; the aggregate reset is still wall-clock-aligned but each instance's budget is independent |
| Cross-key atomicity (multi-combination) | Yes (single Lua Eval) | Best-effort — brief over-admission possible at quota boundary under high concurrency |
| External reconciliation / monitoring | Via Redis ZSET | Not available |

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

### Example 1: Local mode (no Redis)

Omit the `redis` field entirely to run in local (per-instance) mode. All other
configuration is identical to Redis mode. Suitable for single-node deployments
or when a Redis dependency is not desired.

```yaml
rule_name: local-rule
limit_combinations:
  - name: per-consumer
    match:
      - source: consumer
        value: "*"
    query_limits:
      per_minute: 60
    token_limits:
      per_minute: 200000
```

### Example 2: Per-consumer request-count limit (Redis)

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

### Example 3: Token limit for premium API keys on specific models

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

### Example 4: CIDR-based limit on internal network

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

### Example 5: Custom window and cookie-based limit

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

### Example 6: Monthly token quota per consumer

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

### Example 6a: Stacked windows on a single combo

A single `query_limits` (or `token_limits`) may declare multiple rolling
windows at once -- every declared window is its own bucket and the request
must satisfy them all. A single `token_quota` works the same way for
calendar periods. Use this when you want a layered cap on the same
dimension match without duplicating the `match` block.

```yaml
rule_name: stacked-budget
timezone: UTC
limit_combinations:
  - name: per-consumer
    match:
      - source: consumer
        value: "*"
    # Burst guard + sustained guard + abuse cap, all on the same consumer.
    query_limits:
      per_second: 20
      per_minute: 600
      per_day:    50000
    # Tight rolling token cap and a longer-window safety net.
    token_limits:
      per_minute: 200000
      per_hour:   5000000
    # Daily fairness floor and monthly billing cap, both per consumer.
    token_quota:
      each_day:   500000
      each_month: 5000000
redis:
  service_name: redis.static
```

Each window/period contributes a separate Redis key (so the keys above are
`q:1s`, `q:60s`, `q:86400s`, `t:60s`, `t:3600s`, `t:each_day`, `t:each_month`
under the same `<rule>|<combo>|<dim>` prefix). Operators can configure them
independently and `redis-cli ZRANGE` each one in isolation.

### Example 7: Global + route-level override (aggregation semantics)

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

### Local mode (no Redis required)

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
  url: oci://<your-registry>/plugins/gpustack-rate-limit:<version>
  phase: UNSPECIFIED_PHASE
  priority: 600
```

### Redis-backed deployment

Deploy Redis first:

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

- **Fail-open**: infrastructure failures never block traffic. In Redis mode,
  when Redis is unreachable or the Lua script returns an error the request is
  allowed through and a warning is logged. In local mode, shared-data read/write
  errors are likewise logged and the request continues.
- **Backend selection**: the `redis` field selects the backend at config-parse
  time. When absent the plugin logs `"using local (per-instance) rate limiting
  mode"` at INFO level on startup. There is no runtime fallback from Redis to
  local -- if `redis` is configured but the connection fails, the plugin
  fails-open on each request rather than silently switching to local counters.
- **Local mode per-instance semantics**: each Higress replica enforces its
  own independent counters. The effective cluster-wide quota is
  `configured_limit × number_of_replicas`. Do not use local mode for accurate
  cluster-wide billing or quota enforcement in multi-replica deployments.
- **Token limits already at quota**: rejected at the request phase before the
  upstream is touched. Using check-only at the request phase and add-only at
  the response phase means concurrent requests can occasionally exceed the
  quota by a small amount; this is the same trade-off every production token
  limiter makes (applies to both Redis and local modes).
- **Cross-plugin ordering**: if you deploy both `ai-statistics` (or
  `gpustack-token-usage`) and `gpustack-rate-limit`, make sure the statistics
  plugin publishes `ai_log` before this plugin's response-phase hook runs. The
  default phase/priority above respects that ordering against upstream
  `ai-statistics`.
- **VM rebuild**: the plugin triggers a VM rebuild every 1000 requests or when
  the VM memory reaches 200 MiB to guard against long-running memory-leak
  accumulation.
- **Rejection body is always JSON**: see [Rejection response shape](#rejection-response-shape).
  Two coupled hazards make this non-negotiable: (1) an empty body local reply
  is queued by Envoy but never flushed under Higress's AI-route filter chain
  (access log: `response_code=429` / `bytes_sent=0` / `response_flags=DC`),
  and (2) when the request had `stream: true`, `gpustack-token-usage` runs
  at higher priority and parses every response chunk through `json.Valid`;
  non-JSON chunks are buffered as "incomplete SSE" and silently replaced with
  an empty body. `rejected_msg` therefore falls back to `Too many requests`
  when unset, and the body is wrapped into a JSON envelope before being sent.
