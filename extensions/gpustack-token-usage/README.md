# gpustack-token-usage Plugin

## Overview

`gpustack-token-usage` is a Higress Proxy-Wasm plugin that:

1. Injects token usage timing statistics (`time_to_first_token_ms`, `time_per_output_token_ms`, `tokens_per_second`) into AI API responses
2. Reports usage metrics to a configurable HTTP endpoint for routes matching the GPUStack naming convention (`model-<id>-<instance-id>` or `provider-<id>`)
3. Injects the real client IP and a pre-shared trust token into upstream requests destined for GPUStack-trusted clusters (the same trust token is also attached to outbound metrics-report POSTs, so the GPUStack backend validates it identically in both contexts)

It supports both streaming (SSE) and non-streaming responses, OpenAI-compatible and Anthropic-compatible APIs, and multipart/form-data requests (e.g. TTS/STT).

## Configuration

```yaml
# Optional: path suffixes that trigger metrics tracking and response body reading
# Defaults: /chat/completions, /completions, /responses, /messages
enableOnPathSuffix:
  - "/chat/completions"
  - "/messages"

# Optional: path suffixes that also inject stream_options.include_usage (OpenAI-compatible APIs only)
# Defaults: /chat/completions, /completions
# Note: /messages (Anthropic) and /responses (OpenAI Responses API) are excluded by default as they include usage natively
enableUsageOnPathSuffix:
  - "/chat/completions"
  - "/completions"

# Optional: metrics reporting endpoint
endpoint:
  service_name: gpustack-server.gpustack.svc.cluster.local  # K8s FQDN
  service_port: 80
  path: /v2/usage/gateway-metrics
  timeout_ms: 5000  # Optional, default 5000ms

# Optional: header that receives the real client IP. Injected into upstream
# requests only when cluster_name matches a trust regexp (see below).
realIPHeader: x-gpustack-real-ip

# Optional: static headers used in TWO places:
#   1. Injected into upstream LLM requests (same trust gate as realIPHeader).
#   2. Attached to every metrics-report POST sent to `endpoint`.
# A single shared pre-shared token works for both because the GPUStack
# backend validates them identically.
header_add:
  x-gpustack-internal-token: "shared-secret"

# Optional: extra regular expressions matched against the cluster_name FQDN.
# Added to the built-in defaults; does not replace them.
additionalClusterNameRegexps:
  - "^my-internal-svc(\\.|$)"
```

Recommended priority: `400`  
Recommended phase: `UNSPECIFIED_PHASE`

The plugin runs at `priority: 400` so it lands after `model-mapper` (cluster_name is finalised) and before `ext-auth` (the auth service sees the trust headers). It does not publish the `ai_log` filter state, so its priority is independent of `gpustack-rate-limit`'s token accounting.

## Trust header injection

For traffic destined to **GPUStack-trusted upstreams**, the plugin injects `realIPHeader` and every entry of `header_add` into the request. Both are written with **Replace** semantics so a client-supplied value cannot co-exist with the gateway-injected one.

### Filter ordering requirement

Trust-header injection runs in the request-headers phase and reads the Envoy `cluster_name` property to decide whether the upstream is trusted. `cluster_name` is populated only after the route has been resolved, so this plugin **must** run after the filter that resolves the route — in Higress that is `model-router` / `model-mapper`. The recommended priority of `400` puts this plugin downstream of `model-mapper` (priority `900`) and satisfies the requirement.

If the property is empty when this plugin runs (no upstream resolved yet, or an unrecognised flow), trust headers are not injected — the plugin fail-closes so the gateway-issued token cannot leak through a misordered filter chain.

### Trusted cluster matching

The plugin reads the `cluster_name` property (Envoy form `outbound|<port>|<subset>|<fqdn>`), extracts the FQDN, and matches it against:

| Pattern                       | Matches                                                              |
| ----------------------------- | -------------------------------------------------------------------- |
| `^gpustack(-\|\.\|$)`         | `gpustack`, `gpustack-server`, `gpustack-*.<ns>.svc.cluster.local`…  |
| `^model-\d+-\d+(\.\|$)`       | GPUStack model instances (`model-<id>-<instance>[.suffix]`)          |
| `^provider-\d+(\.\|$)`        | GPUStack providers (`provider-<id>[.suffix]`)                        |

Headers are injected **only** when the FQDN matches one of these patterns or one of the `additionalClusterNameRegexps`. Every other upstream is passed through untouched — the trust token never leaks to third-party LLM providers reached via the proxy.

### Threat model

Within a Kubernetes cluster the gpustack-server HTTP port is typically reachable from any pod in the cluster. A malicious in-cluster workload could therefore bypass Higress and send forged `x-gpustack-real-ip` headers directly to gpustack-server. By pairing the IP header with a pre-shared token (`header_add: x-gpustack-internal-token: <secret>`) and validating the token on the server side, gpustack-server can distinguish requests that actually came through Higress from forged direct hits.

The pre-shared token is a long-lived static secret — anyone with read access to *both* the WasmPlugin config *and* the server-side validator config can forge requests. The token still raises the bar relative to plain network reachability and is sufficient when access to request-level traces / logs is kept narrower than access to configuration.

### Secret rotation

Static-token rotation requires a brief overlap window:

1. Update gpustack-server's validator to accept `<old-secret>` **and** `<new-secret>`.
2. Update this plugin's `header_add` to emit `<new-secret>`.
3. After all in-flight requests have drained (usually seconds), remove `<old-secret>` from the validator.

Both ends must be able to be reconfigured without restart for this to be non-disruptive.

## Token Usage Injection

For requests whose path matches `enableOnPathSuffix`, the plugin injects additional fields into the `usage` object of the response:

| Field | Description |
| --- | --- |
| `time_to_first_token_ms` | Milliseconds from request start to first response token (streaming only) |
| `time_per_output_token_ms` | Average milliseconds per output token (streaming only) |
| `tokens_per_second` | Output token throughput |

**Streaming example** — original usage chunk:

```text
data: {"usage": {"prompt_tokens": 50, "completion_tokens": 100, "total_tokens": 150}}
```

After processing:

```text
data: {"usage": {"prompt_tokens": 50, "completion_tokens": 100, "total_tokens": 150,
       "time_to_first_token_ms": 123, "time_per_output_token_ms": 45.46, "tokens_per_second": 6.67}}
```

**Non-streaming** — `tokens_per_second` is injected into `usage.tokens_per_second` in the JSON response body.

## Metrics Reporting

When `endpoint` is configured, the plugin POSTs a JSON payload to the endpoint at the end of every response whose cluster name matches the GPUStack pattern. Requests that do not match are silently skipped.

### Cluster Name Format

The cluster name is read from the Envoy property `cluster_name`. Envoy encodes it as `outbound|<port>|<subset>|<fqdn>`. GPUStack sets the FQDN to one of:

| FQDN pattern | Meaning |
| --- | --- |
| `model-<model_id>-<instance_id>.static` | Request routed to a specific model instance |
| `provider-<provider_id>.static` | Request routed via a provider |

Full cluster name examples: `outbound|80||model-1-2.static`, `outbound|80||provider-5.static`.

### Consumer Identity

User ID and access key are read from the `x-mse-consumer` request header, which is set by the Higress auth plugin. The header value has the format:

```text
[<access_key>.]gpustack-<user_id>
```

| Header value | `user_id` | `access_key` |
| --- | --- | --- |
| `mykey.gpustack-42` | 42 | `mykey` |
| `gpustack-42` | 42 | — |
| `mykey` | — | `mykey` |
| `none` | — | — |

### Payload

```json
{
  "model": "qwen3-0.6b",
  "input_token": 50,
  "output_token": 100,
  "total_token": 150,
  "input_cached_token": 10,
  "request_count": 1,
  "completed": true,
  "output_chunk_count": 87,
  "request_content_bytes": 2048,
  "started_at": 1746518400123,
  "completed_at": 1746518402456,
  "model_id": 3,
  "model_route_id": 1,
  "user_id": 42,
  "access_key": "mykey"
}
```

#### Field reference

| Field | Always present | Meaning |
| --- | --- | --- |
| `model` | yes | Model name from the request (or upstream usage payload as fallback). |
| `input_token` / `output_token` / `total_token` / `input_cached_token` | yes (may be `0`) | Token counts from the upstream usage chunk. `0` does not necessarily mean the request was free — see `completed` below. |
| `request_count` | yes (always `1`) | Reserved for future per-batch reporting. |
| `completed` | yes | `true` iff the response reached its normal terminus — independent of whether any tokens were emitted. Set when the body callback observes `endOfStream=true` (covers SSE streams and non-streaming JSON), or when the body-skip fast path (TTS/STT/image) observed a 2xx upstream status. `false` indicates a mid-stream client disconnect or upstream reset. Crucially: a TTS request that completes normally has all token fields `0` but `completed: true`; an LLM stream cut mid-flight also has token fields `0` but `completed: false`. |
| `output_chunk_count` | yes (may be `0`) | Number of streaming delta chunks observed with non-empty content. Always populated; useful for output-token estimation when `completed=false` and for calibrating the `chunks → tokens` ratio when `completed=true`. |
| `request_content_bytes` | yes (may be `0`) | Sum of `messages[].content`, `input[].content`, and top-level `system` text-block byte lengths from the request. Excludes images / audio / file blocks. `0` for unrecognized request shapes (e.g. multipart/form-data). |
| `started_at` / `completed_at` | yes | UnixMilli wall-clock stamps at request entry (after path/cluster filtering) and at report dispatch. Both are emitted because request-rate accounting attributes events at start (e.g. QPS / `QueryLimits`) while token-rate accounting attributes events at completion (e.g. `TokenLimits` / calendar `TokenQuota`); a stream that crosses a calendar boundary lands in the period it ends in. |
| `model_id` / `provider_id` | mutually exclusive | Derived from the Envoy cluster name (`outbound\|<port>\|\|model-<id>-<instance>` or `provider-<id>`). |
| `model_route_id` | when matched | Derived from the Envoy `route_name` property; formats `ai-route-route-<id>.internal` and `ai-route-route-<id>.fallback.internal` (suffix optional). |
| `user_id` / `access_key` | when present | Parsed from the `x-mse-consumer` header. |

`input_cached_token` aggregates cached prompt tokens from OpenAI/vLLM (`usage.prompt_tokens_details.cached_tokens`) and Anthropic (`cache_read_input_tokens`); cache-creation tokens are excluded because they are new tokens being written, not a hit.

The HTTP call is fire-and-forget (async via `DispatchHttpCall`); it does not block the response to the client.

### Reliability of usage data

The plugin guarantees that streaming requests under `enableUsageOnPathSuffix` have `stream_options.include_usage: true` injected into the request body, **regardless of what the client sent**. If the client originally sent `include_usage: false`, the plugin sniffs the upstream usage chunk for telemetry and then **strips that chunk from the response** before it reaches the client — the client's contract is preserved while the proxy keeps reliable usage data. This is documented as the "sniff-but-don't-leak" pattern and applies only to OpenAI-shape upstreams (Anthropic `/messages` emits usage as an inherent part of the SSE protocol and is not modified).

Even with the force-injection in place, a small fraction of requests will still report `completed: false`:

| Trigger | Effect |
| --- | --- |
| Client disconnects mid-stream | Envoy resets the upstream stream; `endOfStream=true` is never delivered to the body callback. |
| Upstream 5xx / connection reset before completion | Body never reaches its end; usage chunk also never emitted. |
| Request travels through a non-OpenAI-shape upstream (e.g. Anthropic) and is cut early | The plugin captures `input_tokens` from `message_start` greedily, so `input_token` is usually populated even on cancel; `output_token` may still be missing. |

For non-LLM endpoints (TTS / STT / image generation) the plugin reports `completed: true` whenever the upstream returns 2xx, even though `DontReadResponseBody()` was called. This trades off a corner case: if the upstream begins a 2xx response and then resets mid-body, `completed: true` is reported anyway — but for these endpoints the billable unit is the request itself, not body content, so this is the right default.

#### Local-reply responses are not reported

Requests that never reached an upstream — e.g. rejected by `gpustack-rate-limit` (429), `route_not_found`, or any other filter that calls `SendHttpResponseWithDetail` — are deliberately excluded from the metrics report. The proxy detects them via the Envoy `upstream.address` property: when no upstream connection was ever made, the property is empty and the report is skipped. (We don't use `response_code_details` because Higress's filter ordering puts token-usage before rate-limit in the response phase, and Envoy only finalizes `response_code_details` at stream destruction — i.e. after our `onHttpStreamDone` has already run. `upstream.address` is set the moment Envoy opens the upstream connection and is reliably populated by the time we read it.) The originating filter is responsible for its own observability (e.g. rate-limit emits `rejected_total` directly), so reporting these here would just duplicate the count and pollute "average tokens per request" aggregations with token=0 entries that never hit an LLM. Upstream-origin 4xx/5xx responses keep `upstream.address` populated and **are** reported (they represent real LLM-bound traffic, even if it failed).

**Downstream contract for `completed: false`**

The proxy never applies estimation ratios — those are content-type and tokenizer specific and belong in the billing service. Recommended fallback strategy on the consumer side:

| Field | Strategy when `completed: false` |
| --- | --- |
| `input_token` | If non-zero (Anthropic case), trust it. Otherwise apply a per-tenant, per-model byte→token ratio to `request_content_bytes` (calibrate from your own `completed: true` history; English text is roughly `bytes / 4`, Chinese is closer to `bytes / 2`, code is closer to `bytes / 3`). |
| `output_token` | Apply a per-model `chunks → tokens` ratio to `output_chunk_count` (OpenAI-style streams emit roughly one token per chunk; speculative decoding can emit several at once but the deviation is bounded). |
| `total_token` | Sum of the two estimates. |
| `input_cached_token` | Trust if non-zero; otherwise leave at `0`. |
| Billing policy | Some operators charge interrupted streams at 100% of the estimate (defends against cancel-to-evade abuse), some charge at 50%, some not at all. The proxy emits enough signal to support any of these — it does not pick one. |

For both axes (input and output), recording the calibration table from your own `completed: true` traffic is significantly more accurate than any universal ratio. The proxy also emits `output_chunk_count` on `completed: true` requests precisely so this calibration is possible without a separate telemetry pipeline.

## Notes

- **`enableOnPathSuffix`** controls which paths trigger metrics tracking (response body is read to extract token counts). Metrics reporting also requires the cluster name to match the GPUStack pattern.
- **`enableUsageOnPathSuffix`** controls which paths get `stream_options.include_usage: true` automatically injected into streaming request bodies. This is only needed for OpenAI-compatible APIs; Anthropic's `/messages` endpoint includes usage natively and is excluded from the default list.
- Multipart/form-data requests (e.g. TTS audio generation) are supported: the `model` field is extracted from the form, and the binary response body is not processed.
- If the response carries `Content-Type: application/json` despite `stream: true` in the request (e.g. an error response), the plugin treats it as non-streaming.
