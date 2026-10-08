# gpustack-lb-decision-service

A Jev-based routing decision stage for the **gpustack-lb** framework. It sits in the LB band after the context role has published the candidate set: for a task request it asks a Jev-compatible decision service (the "jev" system-1 model) which candidate model should serve the task, then appends a confidence-weighted rank entry to the shared `gpustack_lb_ranks` filter-state array. The **gpustack-lb finisher** (AUTHN/325, further down the same chain) combines all rank entries by weighted sum and writes `x-higress-target-cluster` — this plugin never writes the header itself.

## How it works

```
Client          LB band                                  jev decision service
  |   ext-auth(360) / ip-acl(350) / model-router(900)          |
  |                 |                                            |
  |        gpustack-lb context (340) publishes candidates       |
  |                 |                                            |
  |        capability plugins (335/333/330)                     |
  |                 |                                            |
  |          lb-decision-service (328, this plugin)                  |
  |                 | 1. read filter state gpustack_lb_candidates
  |                 | 2. buffer body, compose:                   |
  |                 |    {state: task, questions:                |
  |                 |     {model_selection: choice over          |
  |                 |      the candidates' models}}             |
  |                 |------------------------------------------->|
  |                 |        200 {answers: {model_selection:    |
  |                 |               {choice: "gpt-6-luna"}}}    |
  |                 |<-------------------------------------------|
  |                 | 3. verdict -> appendRank(gpustack_lb_ranks,      |
  |                 |    Weight = rankWeight x confidence,           |
  |                 |    Scores = p(model) per instance); resume     |
  |        finisher (325) weighted-sum -> x-higress-target-cluster    |
  |                 |-----------> selected backend cluster      |
```

Framework integration:

- **Multi-tenant decision services**: `providers` is a catalogue; each tenant's route picks its entry via `activeProviderId` in `matchRules`, so every tenant uses its own Jev API key. Two provider types ship: **`systemone`** (the generic Jev-compatible service — any self-deployed or third-party jev server; the default when `type` is omitted) and **`typesafe`** (the TypeSafe-hosted flavour, inheriting systemone's entire wire behaviour). The wire contract (`POST /v1/systemone`, jevcompat SPEC) is shared, so other type names also ride the generic systemone implementation. **There is no hosted default**: the callout ships task bodies to the decision service, so every provider must configure an explicit `endpoint` (the endpoint host is the callout's :authority; `cluster` is an optional override of the derived cluster name); with no provider configured the feature stays inert.
- **Candidates are not configured here** — they are read from the filter state key the gpustack-lb context role publishes (`wire.FilterStateCandidates`, the shared contract package, imported via a local replace like the other capability plugins).
- **How the verdict associates with load balancing** (this is the key association): `modelSelection.criteria` keys are MODEL names matching the published candidates' `ModelName`. The plugin consumes the **full choice answer** — `probabilities` (Σ=1) and `confidence`, not just the argmax — and appends a `wire.RankEntry` to the shared `FilterStateRanks` array:
  - **Score shape** = the probability distribution mapped onto candidate INSTANCES (keyed by `Candidate.Key()`): **every instance of a model receives `p(model)` itself, not `p/n`**. Splitting p across instances would let the replica count invert the model-level ordering (2 replicas at p=0.6 losing to 1 replica at p=0.4); with per-instance p, the finisher's shared L1 normalisation (`total = Weight × score / Σ p·n`) keeps the model ordering equal to p's ordering under any replica asymmetry, while instances of the same model stay exactly tied (instance choice remains with least-load / session-affinity).
  - **Confidence** modulates the entry **Weight**, not the scores: the finisher L1-normalises every entry (`total = Weight × score/Σscores`), so scaling the scores themselves to sum to confidence would be erased by that normalisation. `Weight = rankWeight × confidence` achieves the intent — the vote's effective strength is proportional to confidence: a high-confidence verdict dominates routing, a low-confidence one yields influence to the other opinions (least-load, session-affinity).
  - The **gpustack-lb finisher** (AUTHN/325) combines all rank entries by weighted sum and picks the instance; the plugin never writes `x-higress-target-cluster` itself, so there is no conflict.
- **Weighted sets are left alone** (business traffic split), exactly like every capability plugin.
- **Degrade, never fail closed**: no published candidates, no configured question, callout error/timeout, or an unknown verdict → no rank entry is published and the finisher routes as usual. There is no defaultModel to configure. On a successful verdict the plugin logs the **per-instance scores and confidence** at DEBUG level (`model-selection verdict: choice=… confidence=… effectiveWeight=… scores=…`).
- **Internal redirects** (`x-higress-fallback-from`) are not re-routed; strip the header at the listener so clients cannot forge it.
- **Position**: `phase: AUTHN, priority: 328` — after the capability plugins (335/333/330), before the gpustack-lb finisher (325).

## The decision request

```json
{
  "state": "<the task body, capped by maxStateBytes>",
  "questions": {
    "model_selection": {
      "type": "choice",
      "instructions": "Pick the most suitable model for this task…",
      "criteria": {
        "gpt-6-luna": "strongest reasoner, highest cost",
        "claude-opus-5.5": "strong reasoner for code",
        "deepseek-v4-flash": "fast and cheap"
      }
    }
  }
}
```

The **option list is the published candidate set's models**; `modelSelection.criteria` (a per-route config in `matchRules`) supplies the capability descriptions, and any candidate without an entry falls back to a name-only option (`null` description, spec `choice.null-description`). The routing decision consumes the answer's **full probability distribution and confidence** — per-instance scores of `p(model)` plus `Weight = rankWeight × confidence` — not just the `choice` argmax.

## Configuration UX

`modelSelection` requires an explicit `criteria` map (model name → capability description) — **name-only selection is deliberately not supported**: the descriptions are what jev reasons over, and options without them lose too much precision to be useful. The option list itself still comes from the published candidates (GPUStack ModelRouteTargets via the gpustack-lb context role), so the config never enumerates options — it annotates them:

| Situation | Behaviour |
| --- | --- |
| `modelSelection` absent | Feature off; passthrough |
| `modelSelection: true` / `{}` (no criteria) | **Config error** — criteria is required |
| `criteria` with < 2 entries | Feature skipped with a **WARN log** (no opinion published, nothing sent to jev) — a single-model route has nothing to select |
| criteria key matches a candidate's `ModelName` | Option carries the description |
| criteria key matches no candidate (typo/drift) | Inert — never appears in the question, no error |
| candidate has no criteria entry | Falls back to a name-only option (better than disabling the route) |
| `instructions` omitted | A sensible default applies |

To discover the model names to describe, list the route's ModelRouteTargets with the gpustack CLI / control-plane API — the same source gpustack-lb builds its candidate sets from.

## Configuration

| Field | Default | Description |
| --- | --- | --- |
| `modelSelection` | per route | The choice question: `{instructions, criteria: {model: description}}` — the only required config |
| `provider` / `providers` | required | The decision service (`{endpoint, apiToken?, apiTokens?, cluster?, model?}`); there is **no hosted default** — the callout ships task bodies to this service, so the endpoint must be explicit (a cluster alone is rejected: it leaves :authority empty). Without a provider the feature stays inert |
| `provider.apiTokens` | optional | **Canonical** credential field (use it even for a single key): ordered failover list — the first key serves the traffic; when the decision service answers **401/403/429** (invalid or quota-exhausted key) the callout is retried with the next key — the request stays paused throughout, and after the last key fails the plugin degrades as usual (no rank entry). Service-side failures (5xx, timeouts) do not trigger a key retry |
| `provider.apiToken` | deprecated | Legacy single-token form (kept for backward compatibility with existing configs). Ignored when `apiTokens` is set; new configs — and controllers generating config (GPUStack maps a provider's `api_tokens` here) — should always emit `apiTokens` |
| `tokenCooldownMs` | optional (default 30000) | A key that failed with 401/403/429 is skipped for this window before being tried again (shared across worker VMs, namespaced per provider entry — ai-proxy's `cooldownDuration` recovery path without its health checks), so a long-dead primary key costs no wasted callout per request. `0` disables the cooldown; when ALL keys are cooling the full list is still tried (fail-open) |
| `decisionModel` | empty | Route-level override of the decision engine (wins over the provider's `model`); set globally or per route in `matchRules` — empty inherits the provider's value |
| `decisionTimeoutMs` | `3000` | Bound on the jev callout |
| `maxStateBytes` | `65536` | Cap on the task body shipped as `state` |
| `rankWeight` | `10` | This plugin's vote multiplier in the finisher's weighted sum (must be > 0) |
| `maxBodyBytes` | `104857600` | Envoy buffer cap for the task body |

See [example.yaml](example.yaml) for the full manifest, including the prerequisites (gpustack-lb context role on the route, cluster_header EnvoyFilter, fallback-header stripping).

## Build / Test

```bash
make -C extensions test PLUGIN_NAME=gpustack-lb-decision-service
make -C extensions build PLUGIN_NAME=gpustack-lb-decision-service
```

## McpBridge / service registry configuration

The decision callout (`DispatchHttpCall`) targets an **Envoy cluster**, not a URL. Clusters for external services come from Higress's `McpBridge` (the `default` one in `higress-system`; the UI entry is "服务来源 / Service Sources"). Declare one registry entry per decision service:

```yaml
apiVersion: networking.higress.io/v1
kind: McpBridge
metadata:
  name: default
  namespace: higress-system
spec:
  registries:
    # The hosted TypeSafe Jev API — DNS type (an explicit provider entry;
    # no hosted default applies).
    - type: dns
      name: typesafe-jev
      domain: api.typesafe.ai
      port: 443
      protocol: HTTPS
    # A self-hosted tenant jev service with a stable IP — static type.
    - type: static
      name: jev-tenant-a
      domain: "10.0.0.1:8010,10.0.0.2:8010"
      port: 8010
    # A self-hosted tenant jev service by domain — DNS type.
    - type: dns
      name: jev-tenant-b
      domain: jev.tenant-b.internal
      port: 8010
```

Each entry generates an Envoy cluster named **`outbound|<port>||<registry-name>.<static|dns>`**. Wire that name into the provider's `cluster` field:

| McpBridge entry | Generated cluster | Provider config |
| --- | --- | --- |
| `name: typesafe-jev`, dns, 443 | `outbound\|443\|\|typesafe-jev.dns` | `provider: {endpoint: https://api.typesafe.ai, cluster: "outbound\|443\|\|typesafe-jev.dns"}` |
| `name: jev-tenant-a`, static, 8010 | `outbound\|8010\|\|jev-tenant-a.static` | `endpoint: http://…, cluster: "outbound\|8010\|\|jev-tenant-a.static"` |
| `name: jev-tenant-b`, dns, 8010 | `outbound\|8010\|\|jev-tenant-b.dns` | `endpoint: http://…, cluster: "outbound\|8010\|\|jev-tenant-b.dns"` |

⚠️ **The endpoint-derived cluster is only a convenience default.** The plugin derives `outbound|<port>||<host>` from `endpoint` when `cluster` is absent, which matches the generated name **only when the registry name equals the endpoint host**. Whenever the jev service is registered via McpBridge (the normal case), set `cluster` explicitly to the generated `name.static`/`name.dns` form — including for the hosted typesafe API.

⚠️ **Data egress**: every decision callout ships the (capped) task body to the configured decision service. There is deliberately no hosted default — the target must be configured explicitly, so request data never leaves the cluster by accident.

The model-serving backends (the candidates) are likewise reached through their own clusters; those are provisioned by GPUStack / gpustack-lb when it builds the candidate set from ModelRouteTargets, not by this plugin.

## References for controller integration

This README alone is **not sufficient** to build an upper-layer controller — it documents this plugin's slice, while a controller must provision the whole band. Read in this order:

1. **`extensions/gpustack-lb/wire/wire.go`** — the authoritative cross-binary contract: `Candidate` field semantics (`Weight` pointer presence = weighted-mode switch, `ModelName` = the context-resolved rewrite target, `Key()` = `cluster\x00targetId`), `RankEntry` combination maths (per-entry L1 normalisation, weighted sum), filter-state key names. README summaries must never be copied into codegen; import this package instead.
2. **`extensions/gpustack-lb/README.md` + `example.yaml`** — the schema a controller actually generates: the context role's `candidates` + `modelMappers` (built from ModelRouteTargets), the finisher's selection/book-keeping, and the AUTHN band ordering (340 context → 335/333/330 capabilities → 328 this plugin → 325 finisher).
3. **`designs/gpustack-lb-decision-service-modelselection-lb-example.md`** — a worked end-to-end example (single external model, multi-target backends, per-prompt score/weight arithmetic).
4. **This README** — this plugin's config schema and behaviour (the last mile: `modelSelection.criteria` keys must equal the candidates' `ModelName`).

Controller integration tip: `modelSelection.criteria` keys are derivable from the ModelRoute's `target.model` fields — a controller can auto-generate the criteria skeleton (keys + empty descriptions) and leave only the capability descriptions for human authoring. The prerequisites (cluster_header EnvoyFilter, `x-higress-fallback-from` listener stripping) must be provisioned together with the band, not just with this plugin.
