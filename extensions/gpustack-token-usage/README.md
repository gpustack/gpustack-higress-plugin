# gpustack-token-usage Plugin

## Overview

`gpustack-token-usage` is a Higress Proxy-Wasm plugin that:

1. Injects token usage timing statistics (`time_to_first_token_ms`, `time_per_output_token_ms`, `tokens_per_second`) into AI API responses
2. Reports usage metrics to a configurable HTTP endpoint for routes matching the GPUStack naming convention (`model-<id>-<instance-id>` or `provider-<id>`)

It supports both streaming (SSE) and non-streaming responses, OpenAI-compatible and Anthropic-compatible APIs, and multipart/form-data requests (e.g. TTS/STT).

## Configuration

```yaml
# Optional: inject real client IP into a request header
realIPToHeader: "X-GPUStack-Real-IP"

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

# Optional: extra headers to attach to every metrics POST request
header_add:
  X-Internal-Token: "secret"
```

Recommended priority: `910`  
Recommended phase: `UNSPECIFIED_PHASE`

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
  "request_count": 1,
  "model_id": 3,
  "user_id": 42,
  "access_key": "mykey"
}
```

`model_id` and `provider_id` are mutually exclusive and derived from the route name. For TTS/STT or other non-LLM routes where no token usage is recorded, all token counts default to `0`.

The HTTP call is fire-and-forget (async via `DispatchHttpCall`); it does not block the response to the client.

## Notes

- **`enableOnPathSuffix`** controls which paths trigger metrics tracking (response body is read to extract token counts). Metrics reporting also requires the cluster name to match the GPUStack pattern.
- **`enableUsageOnPathSuffix`** controls which paths get `stream_options.include_usage: true` automatically injected into streaming request bodies. This is only needed for OpenAI-compatible APIs; Anthropic's `/messages` endpoint includes usage natively and is excluded from the default list.
- Multipart/form-data requests (e.g. TTS audio generation) are supported: the `model` field is extracted from the form, and the binary response body is not processed.
- If the response carries `Content-Type: application/json` despite `stream: true` in the request (e.g. an error response), the plugin treats it as non-streaming.
