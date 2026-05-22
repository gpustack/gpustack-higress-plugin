# gpustack-model-mapper Plugin

## Introduction

A **fork of Higress's wasm-go `model-mapper`** with two enhancements:

1. **`multipart/form-data` support** — the upstream plugin only handles
   `application/json` bodies, which leaves STT (`/v1/audio/transcriptions`)
   and image-editing (`/v1/images/edits`) routes broken when a route alias
   differs from the actual deployed model name. Closes
   [gpustack/gpustack#4617](https://github.com/gpustack/gpustack/issues/4617).
2. **Configurable `maxBodyBytes`** — upstream hardcodes 100 MiB. Operators
   can tighten this knob to bound wasm memory per concurrent request.

Everything else — the `modelMapping` schema, default `enableOnPathSuffix`,
resolution priority (exact → first matching prefix → defaultModel → original),
alphabetical prefix-iteration ordering, header / body write semantics — is
preserved verbatim from upstream so this is a true drop-in replacement.

> **Disable Higress's built-in `model-mapper`** when this plugin is active —
> otherwise both will try to rewrite the body and the outcome depends on
> filter priority.

## Configuration

| Name | Type | Required | Default | Description |
| ---- | ---- | -------- | ------- | ----------- |
| `modelKey` | string | No | `model` | JSON field / multipart form-name that holds the model identifier. |
| `modelToHeader` | string | No | `x-higress-llm-model-final` | Header receiving the post-mapping model name. Set to `""` to disable. |
| `modelMapping` | map[string]string | No | - | Mapping table (see below). Key syntax: `"foo"` exact, `"foo*"` prefix, `"*"` default. |
| `enableOnPathSuffix` | []string | No | OpenAI suffixes | Paths that trigger mapping. Use `["*"]` to match every path. |
| `maxBodyBytes` | int | No | `104857600` (100 MiB) | Envoy decoder buffer limit. Requests with body larger than this get 413 before the plugin runs. See [Memory considerations](#memory-considerations). |

### `modelMapping` key syntax

```yaml
modelMapping:
  my-stt-route: whisper-large-v3       # exact
  my-image-route: dall-e-3             # exact
  legacy-*: qwen2.5-7b-instruct        # prefix (key ends with *)
  legacy-coder-*: qwen2.5-coder-7b     # see "prefix priority" note
  "*": fallback-model                  # default (used when nothing else matches)
```

Resolution order (verbatim from higress upstream):

1. Exact match in `modelMapping`.
2. First matching prefix entry. **Important**: prefix entries are sorted
   by key alphabetically at config-parse time and the first matching
   entry wins — so `"legacy-*"` outranks `"legacy-coder-*"` for an input
   like `legacy-coder-v1`, even though the latter is a more specific
   prefix. This matches upstream model-mapper's C++ behavior.
3. `"*"` default if configured.
4. Otherwise the request body is left unchanged.

### Default `enableOnPathSuffix`

```text
/completions /embeddings /images/generations /images/edits /audio/speech
/audio/transcriptions /audio/translations /fine_tuning/jobs /moderations
/image-synthesis /video-synthesis /rerank /messages /responses
```

This list is **higress's default plus three multipart endpoints** that
upstream omits because its JSON-only handler can't process them:
`/audio/transcriptions`, `/audio/translations`, `/images/edits`.

### Plugin config example

```yaml
modelKey: "model"
modelToHeader: "x-higress-llm-model-final"
maxBodyBytes: 67108864    # 64 MiB — enough for Whisper audio uploads
modelMapping:
  my-stt-route: whisper-large-v3
  my-image-route: dall-e-3
  legacy-*: qwen2.5-7b-instruct
```

Outcomes:

- `POST /v1/chat/completions` JSON `{"model":"my-stt-route",...}` →
  body becomes `{"model":"whisper-large-v3",...}`, header
  `x-higress-llm-model-final: whisper-large-v3`.
- `POST /v1/audio/transcriptions` multipart with `model=my-stt-route,
  file=<25MiB audio>` → `model` form-field rewritten to
  `whisper-large-v3`, file part passes through unchanged. Order of
  fields does not matter — file before model is fine.
- Same multipart but `model=unknown-name` → mapping returns `unknown-name`
  unchanged (no rule matched, no default); body unchanged, header set to
  `unknown-name`.

### WasmPlugin manifest

See [`example.yaml`](./example.yaml) for a deployable example with priority
guidance and the "disable higress model-mapper" reminder.

## Behavior

- Request paths that don't match `enableOnPathSuffix` are skipped without
  reading the body.
- Content-types other than `application/json` or `multipart/form-data` are
  skipped without reading the body.
- When the body is read, `content-length` is removed (Envoy will re-chunk)
  and route re-evaluation is disabled (mapper runs after routing has been
  decided; we don't want our header writes to trigger a re-match).
- **JSON bodies** are parsed via `gjson`, the model identifier is read from
  the configured `modelKey` field, resolved through the mapping table,
  and written back via `sjson` if it changed. Body is not modified when
  the resolved name equals the original (verbatim from upstream).
- **Multipart bodies** are parsed via the stdlib `mime/multipart` reader.
  The first non-file form-field whose name matches `modelKey` is the
  mapping input; binary file uploads and other form-fields pass through
  to the writer unchanged regardless of their position in the form. Body
  is not modified when the resolved name equals the original.
- If `modelKey` is missing from the body, header is still written using
  the default-model resolution (matches upstream).
- A multipart body with **no** matching form-field is passed through
  unchanged — no header write, no body modification.

## Memory considerations

Mapper uses **buffered** body processing (not streaming) to keep
behaviour parity with upstream `model-mapper`. This trades memory for
correctness: clients sending `model` AFTER a large file in multipart form
are handled correctly without any "model must be in first N bytes"
restriction. Peak wasm memory per concurrent request ≈ **2× `maxBodyBytes`**
(envoy's decoder buffer + wasm linear memory each hold one copy during
rewrite).

Defaults:

- `maxBodyBytes`: 100 MiB
- VM rebuild threshold: 200 MiB (`WithRebuildMaxMemBytes`)

Recommended tuning by workload:

| Workload | Recommended `maxBodyBytes` |
| --- | --- |
| Chat-only (JSON `/v1/chat/completions`, embeddings) | 1–4 MiB |
| Mixed (chat + small image gen) | 16–32 MiB |
| Whisper transcription / image editing (multipart with media) | 64–100 MiB |
| Default catch-all | 100 MiB |

A request whose body exceeds `maxBodyBytes` gets HTTP 413 from envoy
before the plugin runs.

Sizing example: at 25 MiB Whisper uploads with 50 concurrent in-flight
requests, peak per pod ≈ `25 × 2 × 50` = ~2.5 GiB for in-flight wasm +
envoy buffers. Provision pod memory accordingly.
