# gpustack-generic-proxy-router Plugin

## Introduction

This plugin extracts a model alias from the request path (e.g. `/model/proxy/{id}/...`),
maps it through a configured `aliasNameMapping` table, and writes the mapped value into
a request header so downstream Higress AI routing can target the correct model.

It is intended to run **after** Higress built-in `model-router` (which parses the
`model` field from the request body into `x-higress-llm-model` at
`UNSPECIFIED_PHASE` / `priority=900`) so this plugin can override that header
with the path-derived model name. The recommended slot is
`phase: UNSPECIFIED_PHASE` with `priority: 800` — below `model-router` (900) so
it overrides, and above limit plugins such as `gpustack-rate-limit` /
`ai-token-ratelimit` (600) so they see the final model name.

## Configuration

| Name               | Type              | Required | Default               | Description                                                                 |
| ------------------ | ----------------- | -------- | --------------------- | --------------------------------------------------------------------------- |
| `prefix`           | string            | No       | `/model/proxy/`       | Path prefix preceding the alias id. A trailing `/` is appended if missing.  |
| `targetHeader`     | string            | No       | `x-higress-llm-model` | Header to receive the mapped model name (existing value is replaced).       |
| `aliasNameMapping` | map[string]string | Yes      | -                     | Mapping from `{id}` extracted from the path to the actual model name.       |

### Plugin Config

```yaml
prefix: "/model/proxy/"
targetHeader: "x-higress-llm-model"
aliasNameMapping:
  qwen-alias: "qwen2.5-7b-instruct"
  llama-alias: "llama-3.1-8b-instruct"
```

With this config, a request to `/model/proxy/qwen-alias/v1/chat/completions` will have
the request header `x-higress-llm-model: qwen2.5-7b-instruct` injected before
downstream AI routing / rate-limiting reads it.

### WasmPlugin manifest

A full deployable example lives in [`example.yaml`](./example.yaml). Key points:

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: gpustack-generic-proxy-router
  namespace: higress-system
spec:
  phase: UNSPECIFIED_PHASE
  priority: 800            # < 900 so it runs AFTER model-router and overrides
                           # x-higress-llm-model; > 600 so rate-limit sees the
                           # final value.
  url: oci://<your-registry>/gpustack-generic-proxy-router:1.0.0
  defaultConfig:
    prefix: "/model/proxy/"
    targetHeader: "x-higress-llm-model"
    aliasNameMapping:
      qwen-alias: "qwen2.5-7b-instruct"
      llama-alias: "llama-3.1-8b-instruct"
```

## Behavior

- Path is matched against `prefix` as an exact prefix; the first segment after the prefix
  (delimited by `/` or end-of-path) is taken as the alias id. Query strings are stripped
  before matching.
- If the path does not match `prefix`, or the extracted id is empty, or the id is not
  present in `aliasNameMapping`, the request passes through untouched.
- When a mapping is found, the target header is **replaced** (not appended) so a
  client-supplied value cannot bypass the mapping.
