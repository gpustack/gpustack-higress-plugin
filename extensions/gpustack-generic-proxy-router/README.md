# gpustack-generic-proxy-router Plugin

## Introduction

This plugin replaces and extends Higress's built-in `model-router`. It operates in
two complementary modes within a single request lifecycle:

- **Path-driven** (gpustack-specific). When the URL matches the configured
  `prefix` (e.g. `/model/proxy/{id}/...`), `{id}` is looked up in
  `aliasNameMapping` and the resolved model name is written into the
  **routing headers only**. The body is not read or modified. Body-level
  rewrites for the path-driven case are delegated to `gpustack-model-mapper`
  (single responsibility, mirrors higress's router/mapper split).
- **Body-driven** (model-router parity). When the path does not match
  `prefix` but does match `enableOnPathSuffix` (default: the standard OpenAI
  AI endpoints), the `model` field is read from the request body and
  projected into headers, with optional `provider/model` splitting and
  optional `higress/auto` auto-routing. This is a drop-in replacement for
  `model-router`. Both `application/json` and `multipart/form-data` bodies
  are supported (the latter is necessary for `/v1/audio/transcriptions`-
  style uploads).

Body-driven processing uses `ProcessRequestBody` (buffered, not streaming).
The wasm-go streaming wrapper returns `ActionContinue` on every chunk,
which resumes envoy's header iteration before we've written
`x-higress-llm-model` — the AI-route ingress match would then always lose
against the original headers. Buffered restores the `ActionPause` /
`ActionContinue` contract that route re-matching relies on. See
[Memory considerations](#memory-considerations) for the cost trade-off.

> **You must disable Higress's built-in `model-router` when this plugin is
> active**, otherwise both will write `x-higress-llm-model` and the result
> depends on relative `priority`.

## Configuration

| Name | Type | Required | Default | Description |
| ---- | ---- | -------- | ------- | ----------- |
| `prefix` | string | No | `/model/proxy/` | Path prefix for the path-driven mode. A trailing `/` is appended if missing. |
| `targetHeader` | string | No | `x-higress-llm-model` | Header to receive the **unsplit** model value (full target in path-driven mode; full body `model` value in body-driven mode). Replaces any existing value. |
| `modelKey` | string | No | `model` | JSON field name / multipart form-name that holds the model identifier in the request body. Will be rewritten when path-driven, or when body-driven `provider/model` split / auto-routing applies. |
| `addProviderHeader` | string | No | - | When set AND the resolved target is `"<provider>/<model>"`, the provider half is written to this header. |
| `modelToHeader` | string | No | - | When set, the model half (after the optional provider split) is also written to this header. |
| `enableOnPathSuffix` | []string | No | OpenAI suffixes (see below) | Gate for **body-driven** mode. Use `"*"` to match every path. |
| `aliasNameMapping` | map[string]string | No | - | Path-driven mapping from `{id}` to the target model name (plain model or `provider/model`). Omit to disable path-driven mode entirely. |
| `autoRouting` | object | No | - | Body-driven `higress/auto` regex routing — see below. JSON bodies only. |
| `maxBodyBytes` | int | No | `104857600` (100 MiB) | Envoy decoder buffer limit. Requests with body larger than this get 413 before the plugin runs. See [Memory considerations](#memory-considerations). |

Default `enableOnPathSuffix`:

```text
/completions /embeddings /images/generations /audio/speech
/audio/transcriptions /audio/translations /fine_tuning/jobs /moderations
/image-synthesis /video-synthesis /rerank /messages /responses
```

`autoRouting` shape:

```yaml
autoRouting:
  enable: true
  defaultModel: "fallback-model"     # used when no rule matched
  rules:
    - pattern: "(?i)\\bcode\\b"      # RE2 regex
      model: "qwen2.5-coder-7b"
    - pattern: "(?i)\\b(image|photo)\\b"
      model: "qwen-vl"
```

When the request body has `"model":"higress/auto"`, the last `role:user`
message content is matched against `rules` in order; first match wins.
Multimodal `content` arrays (`[{type:text,text:...}, {type:image_url,...}]`)
are flattened by picking the last `text` entry. If no rule matches,
`defaultModel` is used; if that is also empty the body is left untouched.
Auto-routing is intentionally not applied to multipart bodies — they have
no `messages` array.

### Plugin Config

```yaml
prefix: "/model/proxy/"
targetHeader: "x-higress-llm-model"
modelKey: "model"
addProviderHeader: "x-higress-llm-provider"
modelToHeader: "x-higress-llm-model-final"
aliasNameMapping:
  qwen-alias: "qwen2.5-7b-instruct"
  openai-alias: "openai/gpt-4"
  whisper-alias: "whisper-1"
autoRouting:
  enable: true
  defaultModel: "qwen2.5-7b-instruct"
  rules:
    - pattern: "(?i)\\bcode\\b"
      model: "qwen2.5-coder-7b"
```

Example outcomes with the config above:

- **Path-driven** `POST /model/proxy/qwen-alias/v1/chat/completions` with JSON
  `{"model":"foo"}` →
  - header `x-higress-llm-model: qwen2.5-7b-instruct`
  - header `x-higress-llm-model-final: qwen2.5-7b-instruct`
  - body **unchanged** (`{"model":"foo"}`). If upstream needs the body's
    `model` field rewritten to `qwen2.5-7b-instruct`, chain
    `gpustack-model-mapper` after this plugin and configure the mapping
    table there.
- **Path-driven + split** `POST /model/proxy/openai-alias/v1/chat/completions`
  with JSON `{"model":"foo"}` →
  - header `x-higress-llm-model: openai/gpt-4` (the *full* target)
  - header `x-higress-llm-provider: openai`
  - header `x-higress-llm-model-final: gpt-4`
  - body **unchanged**
- **Path-driven multipart** `POST /model/proxy/whisper-alias/v1/audio/transcriptions`
  with `model=whisper-foo, file=<bytes>` →
  - header `x-higress-llm-model: whisper-1`
  - body **unchanged** — `model` form-field still `whisper-foo`, `file`
    flows through. The body is never read or buffered, so even large
    audio uploads do not enter wasm memory in path-driven mode.
- **Body-driven** `POST /v1/chat/completions` (no `/model/proxy/` prefix) with
  JSON `{"model":"openai/gpt-4o"}` →
  - header `x-higress-llm-model: openai/gpt-4o` (unsplit original)
  - header `x-higress-llm-provider: openai`
  - header `x-higress-llm-model-final: gpt-4o`
  - body `{"model":"gpt-4o"}` (provider/model split rewrites body)
- **Body-driven autoRouting** `POST /v1/chat/completions` with JSON
  `{"model":"higress/auto","messages":[{"role":"user","content":"write some code"}]}` →
  - header `x-higress-llm-model: qwen2.5-coder-7b`
  - header `x-higress-llm-model-final: qwen2.5-coder-7b`
  - body `{"model":"qwen2.5-coder-7b",...}`
- **Path matched but no body model field** (body-driven mode, e.g.
  `/v1/embeddings` without `model`) → request passes through untouched.
- **Path doesn't match `enableOnPathSuffix`** (e.g. `/healthz`) → body never
  read; request passes through.

### WasmPlugin manifest

A full deployable example lives in [`example.yaml`](./example.yaml).

## Behavior

### Path-driven mode

- The path is matched against `prefix`; the first segment after `prefix`
  (delimited by `/` or end-of-path) is the alias id. Query strings are
  stripped first.
- If `{id}` is empty or not in `aliasNameMapping`, **the plugin falls
  through to body-driven mode** (it does **not** short-circuit). A WARN
  log surfaces the miss for operators to diagnose.
- On a hit, **only the routing headers are written**; the request body is
  not read and not modified. This mirrors higress `model-router`'s
  restraint — body-level model rewrite is the responsibility of
  `gpustack-model-mapper`, which can be chained after this plugin.
- Because path-driven hit never reads the body, it incurs **zero**
  buffered-body memory regardless of upload size — large multipart audio
  uploads stream through.
- **Operator note for multi-model upstreams**: if the upstream selects
  the model from the body's `model` field (vLLM, sglang, OpenAI-compatible
  multi-tenant serving), pair this plugin with `gpustack-model-mapper`
  to translate the body. Single-model upstreams (one model per route)
  don't need mapper.

### Body-driven mode

- Triggered when path doesn't match `prefix` (or `{id}` isn't mapped) **and**
  the path matches `enableOnPathSuffix` **and** the body is JSON or
  multipart-form.
- Reads `modelKey` from the body. Empty / missing → no-op.
- For JSON bodies, when `model == "higress/auto"` and `autoRouting.enable`,
  auto-routing applies (regex over last user message → matched model →
  `defaultModel` fallback).
- `targetHeader` always receives the **unsplit** value (matching path-driven
  semantics). If `addProviderHeader` is set AND the value contains `/`, it
  is split: provider → `addProviderHeader`, model half → `modelToHeader`
  and body `modelKey`.

### General

- `content-length` is removed whenever the body will be read (body-driven
  branch only), since the rewritten body may have a different size and
  Envoy will re-chunk it.
- **Path-driven hit** is header-only — no body read, no body rewrite, no
  buffer cost.
- **Body-driven** uses **buffered** body processing (not streaming). This
  is not a free choice: in higress's `wasm-go` wrapper the streaming
  variant returns `ActionContinue` on every chunk
  ([plugin_wrapper.go:1031](https://github.com/higress-group/wasm-go/blob/master/pkg/wrapper/plugin_wrapper.go#L1031)),
  which resumes envoy's header iteration before we've had a chance to
  write `x-higress-llm-model`. The buffered path returns `ActionPause`
  until end-of-stream, then `ActionContinue` after the callback finishes
  — which is what lets envoy re-match the route against the header we
  just wrote.
- The `aliasNameMapping` value, the body's `modelKey` value, and an
  auto-routing match may all be plain or `"<provider>/<model>"`. Splitting
  is gated on `addProviderHeader` being configured.
- In body-driven mode, multipart bodies are parsed with stdlib
  `mime/multipart`. Non-target parts (including binary file uploads) are
  passed through to the writer; only the `model` form-field's value is
  rewritten when a provider/model split applies.

### Memory considerations

Memory cost depends on which branch handles the request:

- **Path-driven hit**: ~0 buffered body. The body streams through envoy
  without ever being read or copied into wasm. Large multipart uploads
  (Whisper audio, image edits) cost nothing extra here.
- **Body-driven** (and **path-driven miss** → fall-through): body is
  fully buffered. Peak memory per concurrent request is roughly
  **2× `maxBodyBytes`** (envoy holds one copy in its decoder buffer, the
  wasm linear memory holds another when the callback reads it out).

Defaults:

- `maxBodyBytes`: 100 MiB
- VM rebuild threshold: 200 MiB (`WithRebuildMaxMemBytes`)

Recommended tuning for **body-driven** workloads (path-driven is unaffected):

| Workload | Recommended `maxBodyBytes` |
| --- | --- |
| Chat-only (`/v1/chat/completions`, embeddings) | 1–4 MiB |
| Mixed (chat + small image gen) | 16–32 MiB |
| Whisper transcription / image editing (multipart with media) | 64–100 MiB |
| Default catch-all | 100 MiB |

A request whose body exceeds `maxBodyBytes` gets HTTP 413 (Payload Too
Large) from envoy before this plugin even runs — so tighter is generally
safer. If you don't deploy any multipart upload routes through this
plugin, drop the limit aggressively.

Sizing example: at 25 MiB Whisper audio uploads with 50 concurrent
in-flight requests, peak per pod ≈ `25 × 2 × 50` = ~2.5 GiB just for
in-flight wasm+envoy buffers. Make sure your envoy/wasm pod memory
request matches this ceiling.
