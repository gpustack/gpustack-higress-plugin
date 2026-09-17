# gpustack-lb

The **single binary** of the LB plugin framework; `mode` in its config decides
which role it plays:

```text
mode: context   AUTHN/795  read config -> filter -> publish the candidate set
                           into filter state; on non-LB routes it degrades to
                           model-mapper's existing behaviour
  ↓ the capability band (session-affinity 780 / prefix 760 / least-load 740)
    each append a ranking opinion
mode: finisher  AUTHN/700  read the rankings -> pick a candidate -> write
                           x-higress-target-cluster -> buffer the body to
                           rewrite the model name -> book-keeping in
                           onStreamDone
```

**Two CRs are structurally necessary**, two binaries are not: the capability
plugins have to run between "publish the candidates" and "make the decision",
and one filter instance cannot be in two places in the chain at once. Merging
the binaries did remove two hand-maintained copies of the wire contract, along
with 112 verbatim-duplicated lines of multipart walking -- which happens to be
the most tedious code in the whole design, and once two copies of it diverge
the only place it shows is an endpoint like `/v1/audio/transcriptions`.

`finisher` is the **only** role on this chain that writes the cluster header,
the **only** one that buffers the body, and the **only** one that writes shared
state.

Design reference: the LB framework design doc §2, §4, §6, §7.

## Prerequisite

A route with LB enabled must have an EnvoyFilter turning on `cluster_header`
routing:

```yaml
patch:
  operation: MERGE
  value:
    route: { cluster_header: x-higress-target-cluster }
```

⚠️ **`candidates` and this patch must be rolled out together and withdrawn
together.** With the former but not the latter, the plugin rewrites the model
name for the candidate it picked while Envoy still rolls its own dice over
`weighted_clusters` -- and when candidates such as `gpt-4o` and `gpt-5` sit side
by side on the same cluster, the rewritten model name and the instance the
request actually reaches disagree, **completely silently**.

### `x-higress-fallback-from` must not be accepted from clients

Both roles treat this header as proof that the request is an internal redirect:
context publishes no candidates and the finisher strips
`x-higress-target-cluster` and skips selection. It is an ordinary request
header, so a client that simply sets it gets LB bypassed -- and because
`cluster_header` has displaced `weighted_clusters` on this route, no header
means no cluster, which is the 503 that does not flush. The request then hangs
until the client times out.

Strip it at the listener so it can only ever come from inside the mesh:

```yaml
# HttpConnectionManager
internal_only_headers:
  - x-higress-fallback-from
```

The plugin cannot defend itself here: the whole point of the header is to be
indistinguishable from one Envoy set itself, so there is nothing for the wasm
filter to check. Deployments that do not use Higress's AI fallback at all can
strip the header unconditionally instead.

## Configuration

The two roles share one schema and each reads only its own part. **Every knob
appears in exactly one role**, so there is no setting that has to be kept in
sync across both — with exactly one exception, the optional `redis` block, for
a structural reason spelled out under [The state backend](#the-state-backend).

### mode: context

```yaml
defaultConfig:
  mode: context          # context is the default; written out so as not to
                         # depend on the default
  health:
    failOpen: true       # the global config location; matchRules inherit it
matchRules:
  - ingress: [ai-route-route-2.internal]
    config:
      mode: context
      candidates:
        - cluster: "outbound|80||model-2-12.static"
          targetId: "2"
          kind: instance
          # weight: 10           # the presence of the field is the mode
                                 # switch, see below
          # maxRunningRequests: 8
      modelMappers:
        "2":
          "gpt-*": "qwen3-0.6b"
      # health.failOpen is inherited from defaultConfig; only write it to turn
      # it off for this route alone:
      # health:
      #   failOpen: false
```

| Field | Default | Description |
| --- | --- | --- |
| `candidates` | none | **Its presence decides whether this route does LB.** Empty or missing = degrade to model-mapper behaviour |
| `candidates[].weight` | absent | **The presence of the field is the criterion switch.** A pointer rather than "treat 0 as absent": `weight: 0` is meaningful -- a canary dialled down to 0% is still healthy and usable, its share is just zero |
| `candidates[].targetId` | empty | Looks up the `modelMappers` group, **and is part of the candidate's scoring identity** — see below. Several candidates may share one cluster and differ only here |
| `candidates[].kind` | `instance` | `provider` must be given explicitly; it cannot be inferred from "there is only one candidate" -- a single-instance self-hosted model also has only one |
| `candidates[].maxRunningRequests` | 0 (unlimited) | An input to the filter, **not published downstream**: what downstream sees already excludes anything over the limit. A **soft** cap — see below |
| `modelMappers` | none | Model name mappings grouped by `targetId`; keys within a group use model-mapper syntax (exact / `prefix*` / `*`) |
| `health.failOpen` | `true` | When every candidate has been ejected, pass through rather than reject. With a misconfigured path or a fleet-wide engine restart, it is better to send the request and have it fail than to take the whole route out of service. **Can be set globally in `defaultConfig` and overridden by matchRules** |
| `modelMapping` etc. | — | The existing model-mapper config for non-LB routes; upstream semantics are followed verbatim |

Under `health`, `mode: context` **recognises only `failOpen`**. The threshold,
cooldown and recovery windows are all configured on the finisher -- at the
moment of ejection the finisher stamps both end timestamps into shared state,
and context only reads them, so it never needs to know the window lengths.

#### `maxRunningRequests` is a soft cap

The check reads a snapshot in `context` (795) while the increment happens in the
finisher (700), so requests on different worker threads can all see
`inflight < maxRunningRequests`, all be published, and all be routed — the cap
can be overshot by up to the number of admissions in that window.

Closing that would take an atomic check-and-reserve plus a release on every exit
path, which is exactly the machinery §6.3 rejected for half-open health: miss
one release and the instance is wedged for good. A bounded overshoot is the
better trade, and exact enforcement is `gpustack-rate-limit`'s job.

#### What matchRules inherit

`mode` and `health.failOpen` are **deployment-level** knobs: put them in
`defaultConfig` and matchRules inherit them when silent, override them when not.
Nothing else is inherited -- `candidates` / `modelMappers` / `modelMapping` are
per-route by definition, and inheriting them is dangerous rather than
convenient: a `candidates` in defaultConfig would turn every route not listed in
matchRules into an LB route, and a `modelMapping` there would silently rewrite
model names on all of them.

The implementation has **no** wholesale `*config = global` copy; every rule is
parsed from scratch and only those two fields are copied across. That
incidentally sidesteps the slice aliasing trap `gpustack-rate-limit` hit (when
an inherited slice has spare capacity, `append` writes back through into the
global config -- see CLAUDE.md).

⚠️ `failOpen` is a `*bool` rather than a `bool` in Config, because when the
global config fails to parse rule_matcher **swallows the error and leaves
`globalConfig` at its zero value**. A bare bool's zero value is `false`, so one
unrelated typo in the global config would silently flip every route to
fail-closed -- from "pass through" to "503" when all candidates are ejected,
with no error anywhere. A pointer's zero value is `nil`, which the reader treats
as the default `true`.

### mode: finisher

```yaml
defaultConfig:
  mode: finisher
  health:
    unhealthyThreshold: 3
    cooldownMs: 10000
    # rampMs defaults to cooldownMs
  maxInflightAgeMs: 600000
  reject:
    status: 503
    message: "no healthy model instance available"
```

| Field | Default | Description |
| --- | --- | --- |
| `health.unhealthyThreshold` | 3 | Consecutive-failure threshold during normal operation. **Tightened to 1 during recovery** |
| `health.cooldownMs` | 10000 | How long a candidate stays ejected |
| `health.rampMs` | = `cooldownMs` | Decay window for the recovery-period load penalty |
| `maxInflightAgeMs` | 600000 | In-flight entries older than this are treated as leaked and lazily pruned |
| `reject.status` | 503 | Must be 400–599. A 2xx/3xx is not a rejection: `200` returns a success carrying an error envelope, and `204` is defined to have no body, so Envoy drops the body that has to be non-empty to flush at all |
| `reject.message` | `no healthy model instance available` | **The fallback message must not be removed**; an empty body keeps the response from flushing |
| `modelToHeader` | `x-higress-llm-model-final` | Upstream's default, kept. Measured: no plugin in this repo or in upstream higress's whole plugin set reads this header |

The finisher **needs no matchRules**: its config is entirely deployment-level,
and "does this route do LB" exists in exactly one place, on context. With no
candidate set to read it passes the request through untouched -- so there is no
way for the two CRs' matchRules to fall out of sync.

## The state backend

Two pieces of state outlive a single request: the **in-flight count** and the
**passive health verdict**, both per cluster. The finisher writes them, context
reads them and publishes the result with the candidate set, and capability
plugins never touch them at all.

Where they live is configurable:

| Backend | When | Scope |
| --- | --- | --- |
| proxy-wasm shared data | default, no `redis` block | **One Envoy process** |
| Redis | a `redis` block is present | The whole deployment |

Shared data is the zero-dependency default and is enough for a single-replica
gateway that does not mind per-thread accounting. Note what its scope really
is: Envoy runs **one wasm VM per worker thread** and their linear memories are
disjoint, so even one replica already keeps several independent in-flight
counts and health verdicts. A candidate's published `inflight` is the count on
*this* thread, not the instance's real load.

Redis makes both facts deployment-wide. Least-load sees the true aggregate,
`maxRunningRequests` becomes a fleet-wide soft cap rather than a per-process
one, and one replica ejecting a dead instance ejects it for every replica.

```yaml
redis:
  service_name: redis.static
  # service_port omitted on purpose -- see the note under the table
  username: ""
  password: ""
  timeout: 1000
  database: 0
  key_prefix: gpustack_lb
```

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `service_name` | string | Yes | - | Redis service FQDN. For static DNS services the name ends with `.static` |
| `service_port` | int | No | `6379`, or `80` for names ending `.static` | Redis service port. Must be in `[0, 65535]` |
| `username` | string | No | - | Redis username (Redis 6.0+ with ACL enabled) |
| `password` | string | No | - | Redis password |
| `timeout` | int | No | `1000` | Per-command timeout in milliseconds |
| `database` | int | No | `0` | Redis database index |
| `key_prefix` | string | No | `gpustack_lb` | Isolates deployments sharing one Redis. Must not contain braces — it *is* the hash tag, see below |

Field names are snake_case, matching `gpustack-rate-limit` and every other
Higress redis block, rather than this plugin's own camelCase: an operator
configuring redis is copying a block they already have.

⚠️ **Leave `service_port` out for a `.static` name.** That suffix is a Higress
static-DNS registration whose listener port is **80** whatever the backend
speaks, which is what the default resolves to for those names — writing the
familiar `6379` there connects to the wrong port. Spell the port out only for a
`.dns` / `.svc` style service, where the default is 6379.

A `redis` value that is neither an object nor `null` — `redis: []`,
`redis: "yes"`, `redis: 1` — is a **config error**, not "no redis". Reading it
as absent would silently drop the deployment back to per-process shared data
while the operator believed fleet-wide state was on, which is the same rule
`candidates` and `modelMappers` already follow.

### It has to be configured on both CRs

This is the one knob that appears twice, and it is structural rather than an
oversight: the two roles are two separate WasmPlugin resources, so each one
needs its own client. There is no channel through which the context CR could
hand the finisher CR a redis connection.

⚠️ **Configuring it on one side only fails silently.** Context would read a
store nobody writes and the finisher would write one nobody reads, so every
candidate looks idle and healthy forever: selection degrades to the weighted
dice roll / round-robin, and nothing is ever ejected. No error, no log —
the request path works, it just stops being load-aware. Roll the block out to
both CRs together, and take it away from both together.

Within one CR, `redis` is inherited by matchRules exactly like the other
deployment-level knobs — put it in `defaultConfig` and the rules pick it up,
**including the already-initialised client**, so there is no per-rule `Init`.

### Redis key layout

```text
{<key_prefix>}:inflight:<cluster>   ZSET   member=<token>  score=<start ms>
{<key_prefix>}:health:<cluster>     HASH   f=<consecutive fails>
                                           e=<ejected-until ms>
                                           r=<ramp-until ms>
```

The prefix is wrapped in a **Redis Cluster hash tag** on purpose. The read
batches every candidate of a route into one multi-key script — that batching is
the whole reason the read costs one round-trip instead of 2N — and a cross-slot
script is rejected outright under Redis Cluster. Braces pin every key this
plugin writes to a single slot. The sharding given up is worth nothing here:
the key count is bounded by the number of model instances and the values are
tiny.

The in-flight sorted set replaces the shared-data backend's timestamp array
wholesale. `ZADD` / `ZREM` / `ZREMRANGEBYSCORE` need no read-modify-write, so
there is no CAS loop, no retry budget, and none of the "the very first write to
a new key bypasses CAS" window that `state.go` has to document. The health
update is one atomic `HINCRBY`, which is strictly better than the CAS loop it
replaces: a lost reset there let *non-consecutive* failures accumulate into an
ejection.

Inspecting a bucket by hand:

```bash
redis-cli ZRANGE '{gpustack_lb}:inflight:outbound|80||model-2-12.static' 0 -1 WITHSCORES
redis-cli HGETALL '{gpustack_lb}:health:outbound|80||model-2-12.static'
```

### What it costs

The context role has to **wait** for the read, so an LB request carries one
blocking round-trip: it returns `HeaderStopAllIterationAndWatermark` at 795 and
resumes from the redis callback. That is the same shape Higress's own
`prefix_cache` uses and the one `gpustack-rate-limit` already runs in
production; against an LLM request measured in seconds, an in-cluster
round-trip well under a millisecond is noise.

The two writes are **fire-and-forget** (`+1` at selection, and one script at
`onHttpStreamDone` that releases the entry and records the health outcome
together) and never block anything.

Only the read is batched across candidates. The writes concern exactly one
cluster each, so they are single-key by nature.

### Failure is fail-open

A dispatch error, a redis error reply, a timeout and a malformed reply all
resolve to the same thing: **no state**. Every candidate is published with zero
in-flight and a clean health record, so selection degrades to what it would
have done before any of this existed.

Decoding is all-or-nothing rather than best-effort on purpose. A partial map
would be worse than an empty one — the candidates that did decode would carry
real in-flight counts while the rest carried zero, and least-load would steer
everything at whichever half failed.

⚠️ The honest cost: **for the duration of a Redis outage nothing is ejected
either**, so the black-hole effect passive health exists to prevent can come
back. Taking every LB route out of service because a cache is down is the worse
of the two.

## Why the model name rewrite lives in the finisher

The rewrite is driven by **which candidate was selected**, not by what name the
client sent. When `gpt-4o` and `gpt-5` sit side by side as two candidates on the
same route and the same cluster, the rewrite target is the only thing that
distinguishes them -- and `modelMapping`'s domain does not contain that
information at all.

So this is not a matter of the wrong ordering but of the **wrong criterion**,
and moving model-mapper further down the chain would not fix it.

The mapping is nevertheless **resolved** in context: it finds the `modelMappers`
group for the candidate's `targetId`, looks the target up against the model name
the client sent (the `x-higress-llm-model` header), and fills it into the
candidate's `modelName`. The finisher therefore needs no mapping logic at all,
and the per-request filter state payload does not grow with the mapping table.

`modelMappers` and `candidates` are separate sections because they change at
different rates: adding a model alias should not touch `candidates`, and
adjusting a weight should not touch the mapping table.

⚠️ "First matching prefix" within a group is **not "longest match"**. With both
`"legacy-*"` and `"legacy-coder-*"` present, `legacy-coder-7b` matches the
former -- because `'*'(0x2A) < 'c'`, so `legacy-*` sorts first. This is existing
higress upstream behaviour and the tests pin it down; making longer prefixes win
is only possible by changing the keys themselves.

## Selection logic

```text
candidates carry a weight  ->  hash(x-request-id) % totalWeight dice roll,
                               every scoring opinion ignored
candidates carry none      ->  the capability plugins' opinions are
                               L1-normalised, summed by weight, highest total
                               wins
                               ↓ no capability plugin installed at all
                               built-in round-robin
```

**The criterion is decided by whether the candidates themselves carry a
`weight`**; there is no separate switch field. "Does this route have a business
split" is already encoded in the candidates' `weight`, and adding an enum would
be a second representation of the same fact -- the two would eventually
disagree.

### Candidate identity is `(cluster, targetId)`, not the cluster

gpustack serves several model names on one route from the **same cluster**,
separated only by `targetId` — and since the rewrite target is resolved per
`targetId`, those candidates carry different `modelName` values while sharing a
cluster. So `wire.Candidate.Key()` is what every capability plugin keys its
scores map by, and `combineRanks` looks up.

Keying by cluster alone collapsed them three ways at once:

| | Effect |
| --- | --- |
| Map | The later candidate **overwrote** the earlier, so the scores map had fewer entries than the candidate set |
| Ranking | The duplicate still **consumed a decay step**, pushing every candidate below it one rank down — the distortion hit the whole set, not just the pair |
| L1 sum | `combineRanks` iterates *candidates*, so the shared score was **counted twice**, giving that cluster double the aggregate vote |

And the two then tied exactly, so `bestTotal`'s tie-break chose between them at
random — meaning one sticky session flipped its model rewrite from request to
request, the exact opposite of what session affinity is for.

The cluster stays the right key for **shared state** (in-flight counts, passive
health): those describe the backend, and candidates sharing a cluster genuinely
share one backend. Only the scoring identity needed splitting.

⚠️ **A capability plugin that still keys by cluster does not fail loudly** — it
silently reverts to the behaviour above. This matters most for the separately
shipped enterprise prefix plugin, which has to be rebuilt against the updated
`wire` package.

Two candidates sharing *both* fields are genuinely indistinguishable and are
rejected at config load.

### The weighted sum

```text
total(c) = Σ_entry ( Weight_entry × Scores_entry[c] / Σ_c' Scores_entry[c'] )
```

`c` here is a candidate identified by `Key()`, not a cluster.

The combination model matches llm-d's scorers. Three things happen in this
plugin and are deliberately not delegated to the capability plugins:

1. **L1 normalisation** -- doing it here means a plugin that normalises badly
   cannot dominate the other entries just by writing large numbers. For a
   separately shipped enterprise plugin, this is the only place it can be
   enforced.
2. **The weight default** (`<= 0` becomes 1).
3. **Negative scores treated as 0** (L1 is meaningless over negatives).

An entry with no valid score (it abstained, everything was zero, or the
candidates it scored are all gone) is **skipped whole**, not spread evenly --
spreading would hand a free bonus to the subset in its map, when it actually
said nothing.

**Scoring only a subset is legal**; candidates absent from the map score 0 in
that entry. Prefix affinity is exactly that shape.

**The weight is published by the plugin itself** (`RankEntry.Weight`) rather
than configured here. That is the one divergence from llm-d, motivated by the
distribution boundary: the enterprise prefix plugin is compiled and shipped
separately, so this plugin cannot possibly know in advance what weight to give a
plugin it has never seen. Letting the plugin carry its own weight keeps
"install a new capability plugin" equal to "install one CR". It also means
**priority among the capability plugins carries no meaning** -- a sum is
insensitive to order.

### Why L1 and not min-max

min-max unconditionally maps the best candidate to 1.0 and the worst to 0.0, so
**a one-request difference votes exactly as hard as a thousand-request
difference**. Measured, for two candidates at load 100 vs 101:

| | min-max | L1 |
| --- | --- | --- |
| Load scores | `1.0000 / 0.0000` | `0.5025 / 0.4975` |
| Combined with stickiness (both weights 1) | **stickiness is flipped** | **stickiness holds** |

At load 1000 vs 10 both give way to load. Only L1 can express *how far apart*
the candidates are, and that is the premise on which the weights can be
interpreted at all.

The combination logic is split out as the pure function `combineRanks`, because
it is the one place in this mechanism that is easy to get numerically wrong and
cannot be covered by cluster testing alone.

### Determinism

The candidate order is fixed by context, sorted by full cluster name, and is
**not re-sorted** here. As long as the sort key is stable, the same candidates
plus the same weights always produce the same interval split; otherwise removing
an instance and adding it back would redistribute all the traffic.

`rrCursor` stays in **per-VM memory** rather than shared data: every worker
thread round-robins evenly over the candidate set, and the union of several
independent even rotations is still even -- all that is lost is the global
ordering, which round-robin never promised anyway. Putting it in shared data
would cost a CAS per request for zero benefit. Its initial value is
`rand.Uint64()` rather than 0 -- Envoy's own `RoundRobinLoadBalancer` also uses
a random seed, and its comment gives the reason: several load balancers on one
host marching in step can skew the request distribution badly enough to
overwhelm a backend.

## Book-keeping

`onHttpStreamDone` does two things at once (they already share the same hook,
and on the redis backend they share a single round-trip):

**The in-flight count** is stored per request rather than as a counter — an
array of start timestamps on shared data, a sorted set scored by start time on
redis. A counter cannot be lazily pruned, and the decrement path is the main
failure source for this kind of feature (a stream ending early, a client
disconnecting, an upstream timing out) -- miss one decrement and the counter
only ever grows, eventually **starving that instance for good**. Entries older
than `maxInflightAgeMs` are treated as leaked; the reader applies that horizon
as well as the writer, because a candidate that gets filtered out stops being
selected and therefore stops being pruned.

Selection hands back a **token** identifying the entry, which `onHttpStreamDone`
gives back to release exactly that one. An empty token means nothing was
written, and the release is skipped rather than spending a write on a record
that never existed.

**Passive health** reads `response.flags`, and only failures that did not reach
the application count:

| flag | Counted? | Reason |
| --- | --- | --- |
| UH / UF / UC / NC | ✅ | Never reached the application; points at an unavailable instance |
| Upstream 5xx | ❌ | An application-level error; the instance is alive and ejecting it would be collateral damage (5xx does not appear in flags at all) |
| UT upstream timeout | ❌ | The AI route's route timeout is 0s, so this flag most likely comes from a cluster-level timeout; counting it would eject instances during legitimately long generations |
| DC client disconnect | ❌ | Unrelated to instance health |

⚠️ **One case is undetectable**: the connection is established but nothing ever
comes back (engine OOM / deadlock / still loading, or a worker proxy that
accepts the connection but cannot reach the engine). Nothing appears in flags or
in `code_details`, and the route timeout is 0s. This is a known blind spot.

## Recovery: cooldown plus a load penalty, not half-open

When the cooldown expires the candidate goes straight back, and **the first real
request is the probe** -- no active probing needed.

"Half-open: allow exactly one in flight" was rejected for two reasons: it needs
a cross-thread CAS test-and-set plus a reliable release path (miss one release
and the instance is wedged for good); and **capacity collapses during a group
recovery** -- instances usually fail together and their cooldowns expire
together, which would pin the whole route's concurrency to the number of
candidates.

Penalty decay is used instead (computed by context from shared state and
published with the candidate set), together with an asymmetric failure threshold
(tightened to 1 during recovery). The real cost of a single instance recovering
is about **one request per cooldown cycle**.

## The rejection path

When the candidate set is empty the plugin **must answer itself** and cannot
rely on Envoy to report the error. Measured: on this route, the 503 Envoy
generates for a missing cluster **does not flush to the client**, and the
request hangs for the full 25.0s until the client times out; under the same
config our own reject returns **a 503 with a JSON body in 0.002s**. Removing the
cluster header is only cleanup.

Three hard requirements (learned the hard way in `gpustack-rate-limit`):

1. Call `DisableReroute()` before rejecting -- this is the **only** place it may
   be called
2. Return `ActionPause` after `SendHttpResponseWithDetail`, not `ActionContinue`
   (the body gets dropped) and not `HeaderStopAllIterationAndWatermark` (it gets
   stuck under watermark waiting for a resume that never comes)
3. The body must be non-empty and valid JSON

## Hard constraints

- **`DisableReroute()` must not be called in context mode**: it prevents the
  route from being re-evaluated after the header changes, and the finisher's
  `x-higress-target-cluster` write depends on exactly that. Calling it makes the
  override silently ineffective -- the request keeps using `weighted_clusters`
  with no error anywhere. On the non-LB legacy path it **must** be called
  (upstream model-mapper is followed verbatim).
- **`WithRebuildAfterRequests` is not set**: requests inside the rebuild window
  get a 503 and both roles sit on the LB decision path; also the round-robin
  counter lives in per-VM memory and a rebuild would reset it.
- **A config reload must not reset shared data.** There is deliberately no reset
  function in `state.go` -- measured on a cluster, any configuration change
  causes **every** plugin's extension config to be re-applied, not just the one
  that changed. Copying ai-proxy's `resetSharedData()` would wipe health and
  in-flight state on every scale-out (a dead instance that was just ejected
  returns to the candidate set immediately, plus a thundering herd), and
  scaling is precisely when load most needs to be sensed correctly. The same
  applies to the redis keys, which is why nothing ever deletes them wholesale --
  they carry TTLs and expire on their own.
- **The redis read must stay read-only.** It counts live in-flight members with
  a `ZCOUNT` lower bound instead of pruning the expired ones first. Pruning
  there would be a write on the read path, on every request, for every
  candidate, from the role that is supposed to be a reader. The finisher prunes
  on both of its writes instead, so expired members are cleaned up by the very
  traffic that could create them.
- **The redis callback owns the resume.** It is the only path that resumes the
  request, and the wrapper invokes it exactly once per successful dispatch --
  including when redis errored or timed out, which arrive as a resp error value
  rather than as a missing callback. A failed *dispatch* never pauses in the
  first place. There is no branch on which the request stays parked.

## Tests

```bash
make -C extensions test PLUGIN_NAME=gpustack-lb
```

They cover the weighted intervals' determinism / distribution / zero weights /
order sensitivity, round-robin rotation, tie randomisation, mode normalisation,
candidate ordering and weight presence, `modelMappers` resolution precedence,
config defaults, the rejection body, in-flight pruning, and the flags mask
(which failures count toward health is easy to change wrongly on intuition).

`buildCandidateSet` **became a pure function** when the state moved behind the
store — it takes the loaded state instead of reading it — so the whole filter
is now covered directly: ejection, the concurrency cap, the adaptive penalty
curve (including that P₀ ignores an ejected instance's pile of leaked
entries), fail-open re-admission, and that candidates sharing a cluster share
its state while resolving their own rewrite target. It is the same code on both
backends, so those tests pin redis behaviour too.

On the redis side the pure surface is the config parse, the key layout, the
TTL floors, the leak cutoff shared by reader and writer, and `decodeLoadReply`
— the one place the lua script's wire contract is interpreted, tested against a
hand-built RESP reply so the contract holds without a redis or a wasm host.

`normalizeMode` and `decodeLoadReply` are both split out of their I/O shells as
pure functions for the same reason: host ABI calls (including `LogWarnf`) panic
outside a wasm host, so decision logic buried next to one cannot be unit-tested.
`decodeLoadReply` therefore returns an error for its caller to log rather than
logging itself. For the same reason `selectScored` reads filter state and is out
of unit-test scope.
