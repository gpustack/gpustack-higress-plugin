# gpustack-generic-proxy-router Plugin

## Introduction

This plugin replaces and extends Higress's built-in `model-router`. It operates in
two complementary modes within a single request lifecycle:

- **Path-driven** (gpustack-specific). When the URL matches the configured
  `prefix` (e.g. `/model/proxy/{id}/...`), `{id}` is looked up in
  `aliasNameMapping` and the resolved model name is written into the
  **routing headers only**. The body is not read or modified. Body-level
  rewrites for the path-driven case are delegated to `gpustack-model-mapper`
  (single responsibility, mirrors higress's router/mapper split). `{id}` may
  also be `{route-id}-{instance-id}` to name the instance — see
  [Naming the instance](#naming-the-instance-modelproxyroute-idtarget).
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

## Position in the filter chain

Deployed as `AUTHN/900` — the **first** filter of the GPUStack chain
(model-router's original slot, per gpustack core `gateway/__init__.py`).
The request-side order (Higress sorts by phase bucket, then descending
priority within the bucket):

```text
AUTHN        900  this plugin (gpustack-model-router CR)  writes x-higress-llm-model
AUTHN        810  gpustack-header-transformer             strips untrusted identity headers
UNSPECIFIED  400  gpustack-token-usage                    usage reporting
AUTHN        360  gpustack-ext-auth                       authn, injects x-mse-consumer
AUTHN        350  gpustack-ip-acl                         per-consumer IP ACL
AUTHN        340  gpustack-lb (context)                   publishes the candidate set
AUTHN        336  gpustack-lb-session-affinity            ┐
AUTHN        333  gpustack-lb-prefix (enterprise)         │ capability band: each
AUTHN        330  gpustack-lb-least-load                  │ appends a scored opinion
AUTHN        328  gpustack-lb-decision-service            ┘
AUTHN        325  gpustack-lb (finisher)                  writes x-higress-target-cluster
UNSPECIFIED  100  gpustack-ai-proxy                       provider protocol adaptation
AUTHN         90  gpustack-set-header-pre-route           last AUTHN hop, pre-route headers
```

(AUTHN as a bucket runs before the router; UNSPECIFIED after it. Cross-bucket
order therefore does not follow the raw priority numbers.)

## Request tagging (planned)

This plugin is the **entry-classification point** of the chain, and is where
request tagging lives. Tagging turns "what is this request" into a shared
filter-state contract so downstream plugins stop re-implementing matchers:

- rules evaluate request attributes (headers, model name, and — because this
  plugin already buffers the body on `enableOnPathSuffix` paths — exact body
  length) and set tags into the filter state key `gpustack_request_tags`
  (`map[string]string`);
- when a rule needs the body, the tag is set in this plugin's body phase —
  which is safe for every consumer below, because this plugin pauses header
  iteration while buffering, so nothing downstream (810 and below) has run
  its header callback yet;
- consumers reference tags, not raw headers: LB `groups` match on
  `tags` for deterministic routing (#9/#10), `gpustack-token-usage`
  attributes usage by tag (#6738, gpustack issue "header-based usage
  attribution");
- tags are plugin-private filter state, **not** request headers: headers can
  be forged by clients and leak to upstreams;
- fail-open: a rule that does not match sets no tag, and every consumer
  behaves exactly as it does today.

**Implemented**: the measurement half of this design ships today — on every
body-driven path this plugin writes the final body length (post-rewrite)
into the filter state under `gpustack_request_context.lengthBytes`, which
`gpustack-lb`'s `groups` `contextLength` matching reads (with a
`Content-Length` fallback on paths this plugin never buffered). Header
matching in `groups` reads the live request headers at 340 and needs
nothing from this plugin; the tag map remains unbuilt until a value exists
that equals no header (see `designs/request-tagging-and-route-rules.md`).

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
| `videoRegexes` | []object | No | - | Self-routing video job IDs. Each entry is `{regex, path}`; `regex` must name the capture groups `target` and `origin`, `path` is the upstream path template. Presence enables the video branch. See [Self-routing video job IDs](#self-routing-video-job-ids). |
| `videoIdRewriteOnRouteNamePrefix` | []string | with `videoRegexes` | - | The response `id` is wrapped only when Envoy's `route_name` (namespace stripped) starts with one of these; the digits that follow are the route id. |

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

## Naming the instance: `/model/proxy/{route-id}-{target}/...`

With `aliasRegex`, the path-driven alias accepts a second spelling, with
which the client names the target for an ordinary request:

```yaml
aliasRegex: '^(?P<route>[^/]+?)(?:-(?P<target>[0-9]+|provider-[0-9]+))?$'
```

```text
/model/proxy/2-7/v1/chat/completions          instance 7 of route 2
/model/proxy/2-provider-5/v1/chat/completions provider 5 of route 2
```

The regex is matched whole against the `{id}` segment. Its `route` group
is looked up in `aliasNameMapping` exactly as a plain alias is; an optional
`target` group is the instance or provider the client names within that
route. Without `aliasRegex`, `{id}` is the alias — the behaviour before the
option existed.

- **The route group is lazy on purpose.** A suffix that is not a target
  shape is absorbed, so `qwen-alias` stays one alias and
  `/model/proxy/qwen-alias-7/...` is alias `qwen-alias`, instance 7. The
  flip side: `2-blue` (not a target shape) is looked up as the alias
  `2-blue`, misses, and falls through to the body's `model` — with a WARN
  in the log.
- The path outranks the body's `model`, and is rewritten to the canonical
  form.
- The target goes to `gpustack-lb` as a **select-mode** pin: the instance
  is honoured, but the request still goes through the in-flight reserve,
  so naming an instance is not a way around `maxRunningRequests` (full
  means 429, not a move to another instance). A target that is not a
  candidate of the route is a `404` with `x-gpustack-lb-reject: pin_miss`.
- The pin is restored on an internal-redirect re-entry, so a retry lands
  on the named instance again.

### `targetPatterns`

What a target is, and which cluster service names realise it, is
configuration. A rule is a **pair of regexes whose capture groups
correspond by position**:

```yaml
targetPatterns:            # unset: exactly these two
  - target: '^(\d+)$'
    cluster: 'model-\d+-(\d+)'
  - target: '^provider-(\d+)$'
    cluster: 'provider-(\d+)'
```

```text
target "2"            ->  the candidate whose service name is model-<model>-2
service name model-42-2  ->  the target "2", written into the wrapped ID
target "provider-5"   ->  the candidate whose service name is provider-5
```

- What is **outside** a group is a regex and takes no part in the mapping
  (`model-\d+-`: any model). What is **inside** a group is the mapped
  value, and the n-th group of `target` is the n-th group of `cluster`.
- Both plugins read a rule the same way, through one function: a service
  name is matched against `cluster`, its groups are interleaved into
  `target`'s literals, and the result is the target. The router does it
  for the cluster that served a create (the target goes into the ID), the
  LB for every candidate (the result is compared with the pin). Nothing is
  substituted into a pattern, so no pattern can be wider than the target
  it was built for.
- The two plugins must carry the same list — otherwise every wrapped ID is
  a pin miss. A custom list replaces the defaults rather than adding to
  them.
- Both sides are matched as a whole. The group counts must be equal, and
  for the target to be rebuildable `target` must be a plain sequence of
  literals and capture groups (`^provider-(\d+)$`); anything else, and
  the older `$n` template spelling, is rejected at load time. A target
  must not contain `_` (the ID's divider); hyphens are fine, the ID splits
  on the first one.
- **The `target` regexes of a list must not overlap.** A target is claimed
  by the *first* rule whose `target` matches it, so a target two rules
  claim would be wrapped by one rule and matched by another. The same
  regex twice is rejected at load time; any other overlap makes the router
  leave the ID native rather than wrap it for the wrong owner.

## Self-routing video job IDs

An asynchronous job's native ID only exists on the instance that created
it, and the OpenAI SDK's follow-up calls (`videos.retrieve`,
`videos.download_content`, `videos.delete`) carry nothing but that ID — no
model, no custom header. So this plugin makes the ID self-describing when
the create response passes through, and routes the follow-ups by it:

```text
gpustack_<route-id>-<instance-id>_<native-id>      gpustack_2-7_video_abc
gpustack_<route-id>-provider-<id>_<native-id>      gpustack_2-provider-5_video_abc
```

Underscore separates the three fields; hyphen joins the parts inside the
target field. **The route id is what names the model** — the same key
`/model/proxy/{id}` resolves through `aliasNameMapping`, which the video
branch reuses — and what follows it names the target within that route, in
the spelling of the GPUStack cluster (`model-<model>-<instance>` /
`provider-<id>`). The literal `provider-` is what tells the two target kinds
apart, so there is no kind field.

```yaml
aliasNameMapping:
  "2": "sora"
videoRegexes:
  - regex: '^/v1/videos/gpustack_(?P<target>[^_]+)_(?P<origin>[^/]+)$'
    path:   '/v1/videos/{origin}'
  - regex: '^/v1/videos/gpustack_(?P<target>[^_]+)_(?P<origin>[^/]+)/content$'
    path:   '/v1/videos/{origin}/content'
videoIdRewriteOnRouteNamePrefix:
  - "ai-route-route-"
```

Both fields are **deployment-level** and belong in `defaultConfig`, next to
`aliasNameMapping`: nothing in them is per-route (the regexes match a fixed
prefix, the route-name prefixes are the deployment's naming convention), so
there is nothing a `matchRules` entry could usefully narrow. Which routes take
part is decided by the data: a route is a video route when its id is a key of
`aliasNameMapping` and its name carries the configured prefix. With both
fields absent the video branch never runs and the plugin behaves exactly as
before. A partial configuration fails `parseConfig`: `videoRegexes` without
the prefix list, or the prefix list without `videoRegexes`; a `regex` missing
one of the two named groups; a `path` with an unknown `{placeholder}`.
`aliasNameMapping` is **not** required — it starts out empty on a fresh
gateway and is filled per route, so requiring it would make the factory
defaults a config error on a new install; a follow-up whose route is not
mapped is answered `404` at request time instead. The half-open state would hand clients an ID that
cannot be polled, with nothing in the log to say why.

Write `origin` as `[^/]+`, not `.+`: a native ID never contains `/`, and
`.+` would let the first rule swallow `.../content` and make the second one
unreachable.

| Request | What happens |
| --- | --- |
| `POST /v1/videos` | Ordinary body-driven (the model is in the body). Additionally marked as a create, and taken out of every replay path — see below. Response: top-level `id` wrapped. |
| `GET /v1/videos/gpustack_2-7_<id>` | target `2-7` → route `2` → `aliasNameMapping["2"]` → `x-higress-llm-model`; `:path` rewritten to `/v1/videos/<id>` (query preserved); pin `7` published for `gpustack-lb`. Response: `id` wrapped again. |
| `GET /v1/videos/gpustack_2-provider-5_<id>` | Same, with pin `provider-5`. |
| `GET /model/proxy/2/v1/videos/gpustack_2-7_<id>` | The path-driven alias prefix (`prefix` + one segment) is stripped before the regexes match, so one regex set serves both spellings. The alias is dropped, not resolved: the ID's route id decides the model. Upstream path: `/v1/videos/<id>`. |
| `DELETE /v1/videos/gpustack_2-7_<id>` | Same as the status request. |
| `GET /v1/videos/gpustack_2-7_<id>/content` | Same; the response (a byte stream) is never buffered. |
| `POST /v1/videos/sync` | Not touched: an ordinary model request with a binary response. |

**Why the model is resolved here.** The route match and `gpustack-ext-auth`
both need `x-higress-llm-model`, and this plugin is the first filter of the
chain (AUTHN/900): the header it writes is what the model route is
re-matched against. `gpustack-lb` (AUTHN/340) sits after the route match
and cannot supply it. A follow-up whose route id is not in
`aliasNameMapping` (route deleted, or an ID that was never ours) is answered
`404` on the spot — without the model it could not be matched to a model
route at all. The target must be `<digits>` or `provider-<digits>`; anything
else is not treated as a self-routing ID.

**Why the pin is filter state, not a header.** `gpustack-lb`'s finisher
reads `gpustack_video_pin` ([wire](../gpustack-lb/wire/wire.go), just the
target) and routes to the named candidate without re-selecting; see that
plugin's README. A request header would be forgeable, and a forged pin would
let a caller pick its own instance on every LB route.

**Response rewrite is gated on `route_name`.** The wrap happens only when
all of these hold: the request was a create or a self-routing follow-up,
the response is from upstream, 2xx, `application/json`, **and** Envoy's
`route_name` (after the last `/`, so a namespace qualifier is ignored)
starts with one of `videoIdRewriteOnRouteNamePrefix` — the digits that
follow are the route id (`ai-route-route-12.internal` → `12`). Only the
top-level string `id` is replaced; an error envelope or a body without one is
left as is. For a create the target comes from the `cluster_name` that served
the request (`model-<model>-<instance>.static` → `<instance>`,
`provider-<id>.static` → `provider-<id>`, the naming contract token-usage
already depends on); the model in that name is deliberately not carried, a
route id and a model id are different numbers. An unrecognised cluster
leaves the response untouched. At most 16 MiB of a response is buffered, and
only for requests that passed every gate. When a response is armed for
rewriting, its `Content-Length` is removed in the response-header phase:
Envoy does not refresh it when a wasm filter swaps the body, so the wrapped
body would reach the client truncated to the native length (measured), and
by the body phase the header is already on the wire. The response is then
framed chunked.

**Cascade (edge → cluster).** The cluster gateway wraps first
(`gpustack_2-7_…`); the edge sees a `provider-*` cluster and wraps again
(`gpustack_9-provider-5_gpustack_2-7_…`). The edge resolves the outer route
id to the remote cluster and forwards the path with only its own prefix
removed; the serving gateway strips the inner one. No gateway needs to know
another cluster's routes or instances.

**A create is never replayed.** An async create that reached the engine
may already have produced a job; a second attempt would submit it twice.
So a create gets `x-envoy-max-retries: 0` and loses `x-envoy-retry-on`,
`x-envoy-retry-grpc-on` and `x-envoy-hedge-on-per-try-timeout`; a
re-entry pass (one carrying `x-higress-fallback-from`) of a create is
answered `502` here instead of being re-selected; and `gpustack-ai-proxy`
skips token failover and `retryOnFailure` for video operations. The cost is
deliberate: a create whose response was lost is reported as a failure.

**A create is recognised early, and a re-entry is recognised by its original
path.** `POST /v1/videos`, `/v1/videos/` and `/model/proxy/{id}/v1/videos`
are all armed as creates before any early return (alias hit, body not
capable, suffix miss), with the alias prefix and trailing slash normalised
away so the request reaches the body-driven branch and the route's path
match. On a redirect re-entry the live `:path` is already the upstream-facing
one (native ID, possibly a provider base path prepended by ai-proxy), so it
cannot tell a create from anything else: the first pass therefore leaves the
client's original path in `x-gpustack-video-path`, which Envoy replays on the
redirect. A create is answered 502 from it; a pinned follow-up is routed
again from it, restoring the pin so the retry reaches the owner. The header
is cleared on every first pass, so a client-seeded value never acts as one,
and its value is only ever re-run through `videoRegexes` and
`aliasNameMapping`, never trusted on its own.

**Prerequisites on the controller side.**

- `/v1/videos` and `/v1/videos/sync` must be in the route's
  `enableOnPathSuffix`; the create's model comes from the body.
- The model ingress must match `/v1/videos` and `/v1/videos/*` together
  with the model header — the follow-up path is rewritten before the
  route match, so either spelling matches.
- The follow-up needs an LB route: `gpustack-lb` can only pin to a
  candidate it was given. On a route without `candidates` the pin is
  ignored and the request goes wherever `weighted_clusters` sends it.
- Only `GET` and `DELETE` are routed through `videoRegexes`. Any other
  method on such a path is passed on untouched.

### Scope and assumptions

- **Provider credentials.** The ID names a provider, not the token a job
  was created under. The assumption is that every `apiTokens` entry of a
  provider that serves video can access the provider's jobs -- tokens of
  one account -- so `gpustack-ai-proxy` may pick any of them for a
  follow-up. Tokens of different accounts behind one provider is not a
  supported configuration.
- **A gateway in front.** Wrapping IDs is for the gateway that owns the
  GPUStack routes, once. An entry point in front of it (a load balancer,
  a plain ingress, a Higress without `videoRegexes`) must only forward:
  it neither wraps nor leaves the replay marker (`x-gpustack-video-path`),
  so nothing of this plugin's state reaches the GPUStack gateway from
  outside. Chaining two gateways that **both** wrap IDs is not a supported
  topology -- the inner one would read the outer one's marker on a retry.
