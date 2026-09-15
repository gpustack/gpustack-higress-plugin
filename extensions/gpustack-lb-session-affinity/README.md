# gpustack-lb-session-affinity

A **capability plugin** of the LB framework: it sends the requests of one
session consistently to the same instance.

```text
795  gpustack-lb (mode: context)   publishes the candidate set into filter state
780  this plugin                   reads the set -> rendezvous ranking ->
                                   appends one opinion
740  gpustack-lb-least-load        least load (when deployed)
700  gpustack-lb (mode: finisher)  L1-normalises + weighted sum -> picks a
                                   candidate -> writes the cluster header
```

It **only states an opinion, it does not decide**: it gives every candidate a
score, and the finisher decides by combining all the entries by weighted sum.

Design reference: the LB framework design doc §3.1, §4.2, §7.1, §8.3.

Priority within the capability band **carries no meaning** -- the entries are
combined by weighted sum, so ordering does not affect the result. To change how
much say this plugin has, change its `weight`.

## Configuration

```yaml
config:
  sessionKeys:                      # ordered chain; the first to yield a value wins
    - header: session_id
    - header: x-client-request-id
    - bodyKey: prompt_cache_key
  weight: 1                         # this entry's vote multiplier in the weighted sum
  enableOnPathSuffix: ["/responses", "/messages"]   # body sources only; this is the default
```

| Field | Default | Description |
| --- | --- | --- |
| `sessionKeys` | **required** | Ordered chain of candidates. Each entry gives exactly one of `header` or `bodyKey` |
| `sessionKeys[].header` | — | Request header name, **case-insensitive** (lowercased at parse time; Envoy's headers are all lowercase) |
| `sessionKeys[].bodyKey` | — | A **gjson path** (`prompt_cache_key`, `metadata.user_id`, `a.0.b` all work). Only string-typed values are accepted |
| `enableOnPathSuffix` | `["/responses","/messages"]` | Constrains body sources only. `"*"` allows everything |
| `maxBodyBytes` | 100 MiB | Decoder buffer limit applied before this plugin buffers a body source. **Must match `gpustack-lb`'s `maxBodyBytes`** — see below |
| `weight` | `1` | This entry's vote multiplier in the weighted sum, published with the opinion. `<= 0` is treated as 1 |

Missing, an empty array, or an entry giving both sources all make `parseConfig`
**fail** rather than quietly do nothing: Higress applies `defaultConfig` to
routes not listed in `matchRules` as well, and a config that does nothing is
very hard to tell apart from a config that is wrong.

⚠️ **`sessionKeys` is not inherited by matchRules**, unlike `weight` and
`enableOnPathSuffix`. Which key identifies a session is a per-route fact, and
it is mandatory, so there is no "inherit an empty chain" ambiguity to resolve.
The consequence is that a `sessionKeys` in `defaultConfig` covers only the
routes **absent** from `matchRules` -- a matchRule that omits it does not fall
back to the global chain, it fails to parse. Pick one of the two shapes:

| Shape | How |
| --- | --- |
| Per route | `defaultConfigDisable: true`, and every matchRule declares its own chain |
| Deployment-wide | One chain in `defaultConfig`, and no `matchRules` at all |

### Why a chain rather than a single source

One gateway serves several client types, and **they each mark sessions with a
different field**:

| Client / protocol | Field | Nature |
| --- | --- | --- |
| Codex | `session_id` (header) | Native routing signal |
| Codex | `x-client-request-id` (header) | Its companion |
| OpenAI | `prompt_cache_key` (body) | **A cache routing key** -- its declared purpose is "route to the same cache", which overlaps with what this plugin does |
| OpenAI | `previous_response_id` (body) | Real session continuation, requires `store`. **Measured: llama.cpp returns a hard 400**, see below |
| Anthropic / Claude Code | `metadata.user_id` (body) | Officially abuse detection; in practice used to carry a session UUID. **User-level, not session-level** |
| Anthropic | `cache_control` | A cache marker, not a key; takes no part in routing |

If only one source could be configured, the other client types would **silently
get no stickiness**.

Order is precedence, the same convention as the array order of
`gpustack_lb_ranks` in the framework.

> ⚠️ **`metadata.user_id` is user-level**: using it as the affinity key pins all
> of one user's sessions to the same instance. Still a net win for cache hits,
> but coarser for balancing. Commented out by default in the example.
>
> **`prompt_cache_key` deserves a note of its own**: it is effectively the
> client doing prefix affinity for you -- the enterprise prefix plugin has to
> tokenize and hash the real prefix, whereas this field is the client telling you
> directly "I share a prefix with the previous turn". Configuring it gets the
> community edition a large share of that benefit with zero tokenization and no
> extra dependency.

### Header sources take precedence over body sources as a group

Regardless of their relative position in `sessionKeys`. Reading a header is
free, whereas every body source requires stopping iteration to buffer the body;
strictly following array order, buffering the body first and only then
discovering that a header was available all along, is a pure loss.

**Order among the headers, and among the body keys, still follows the array
strictly.** With header sources only, iteration is never stopped and no body is
ever buffered.

**There is no `hash` algorithm switch.** The design once specified
`hash: rendezvous`, but that field has exactly one correct value -- `hash % N`
reshuffles every session whenever the candidate set changes, and the candidate
set changes precisely when stickiness matters most (scaling, passive health
ejection). A knob with only one right answer is an entry point for
misconfiguration, the same reasoning that removed `selection` in §4.2.

## Output: exponential decay by HRW rank

**What is published is not the raw HRW hash value**, but `1, 0.5, 0.25, …` by
rank.

That step is required under a weighted sum. HRW hash values are **purely
ordinal** and approximately uniform on [0,1) -- the sticky owner might draw 0.95
on one session and 0.51 on the next, which makes "how strong is the stickiness"
**random per session** and entirely uncontrollable. Under the earlier
"first to state an opinion wins" model that did not matter (only the argmax was
read); under a weighted sum it is fatal.

With rank decay instead:

- The owner's advantage is **independent of the candidate count** (always twice
  the second choice), unlike a linear rank which flattens out as candidates are
  added.
- The ranking information is **still preserved**, so once the owner is ejected
  the second choice remains clearly better than the third -- the session stays
  stable after the transfer instead of bouncing per request. A binary `{1,0}`
  would lose that.

The finisher L1-normalises afterwards, so there is no need to divide by the sum
here; with three candidates the votes actually cast are `0.571 / 0.286 / 0.143`.

## Why rendezvous (HRW)

The property: removing a candidate reassigns **only the sessions that lived on
it**, and nothing else moves. Under `hash % N` roughly (N-1)/N of the sessions
get shuffled.

That property is pinned by `TestRemovalOnlyDisturbsItsOwn` /
`TestAdditionOnlyTakesItsShare` in [affinity_test.go](affinity_test.go) -- 5000
sessions, where the only ones allowed to move are the share belonging to the
candidate that was removed, and everything else must see **zero disturbance**.

The chance of two HRW scores tying is about 2⁻⁵³, but when it does happen there
has to be a deterministic tiebreak (the candidate key), or the same key would
rank differently on different workers and stickiness would fail outright.

### Ranking is over candidates, not clusters

The hash input and the scores map are both keyed by `wire.Candidate.Key()` —
`(cluster, targetId)` — not by the cluster name. gpustack serves several model
names on one route from the same cluster, separated only by `targetId`, and
each of those candidates carries its own rewrite target.

Ranking by cluster collapsed them: the later candidate overwrote the earlier in
the map, the duplicate still consumed a decay step (pushing every candidate
below it one rank down), and the finisher then found an exact tie and picked a
model at **random, per request, for one session**. See `gpustack-lb`'s README
for the full arithmetic.

### Two details of the hash

**The session key is length-prefixed** before the candidate key is appended,
rather than separated by a delimiter. A single delimiter was enough while the
second half was a bare cluster name, but the candidate key now contains its own
NUL separator, so `("a", "b\0c")` and `("a\0b", "c")` would feed the same byte
stream — and session keys are user-controlled (a JSON body field can carry a
`\u0000` escape), which makes that reachable rather than theoretical. A length
prefix is unambiguous whatever either half contains.

**A MurmurHash3 finalizer (`mix64`) is layered on top of FNV-1a.** This is not
preventive fastidiousness: FNV-1a avalanches weakly, and our cluster names
differ only in the last character or two (`model-2-10` / `model-2-11`), so their
high bits are strongly correlated -- and HRW compares nothing but which score is
highest. Measured maximum bin deviation (20000 keys):

| Candidates | FNV-1a alone | With `mix64` |
| --- | --- | --- |
| 3 | 6.2% | 0.7% |
| 5 | 8.4% | 2.3% |
| 16 | **14.1%** | 6.6% |
| 32 | **23.2%** | 10.1% |

With the mixer the numbers are essentially sampling noise. `TestDistributionIsEven`
pins this down with **16** candidates -- at 8 both are within the noise (3.2% vs
4.1%), the test passes with the mixer removed, and it guards nothing.

**The raw hash takes the top 53 bits and divides by 2⁵³**, not
`float64(x)/2^64`: a float64 mantissa is only 53 bits, so a direct conversion
would discard the low 11 bits and manufacture fake ties -- which the rank
ordering depends on.

## Three things it does not do

Each corresponds to a silent failure mode:

- **Never calls `ctx.DisableReroute()`.** That writes the request-level property
  `clear_route_cache=off` (it takes effect across plugins, it is not a flag on
  this context), and the finisher's `x-higress-target-cluster` write depends on
  the route being re-evaluated. Calling it makes the override silently
  ineffective: the request keeps using `weighted_clusters` with no error
  anywhere.
- **Never reads shared data.** Load and health were already filtered by the
  publisher and come down with the candidate set (§7.1). Reading them again
  would return the same numbers plus a host call.
- **Never calls `ctx.SetRequestBodyBufferLimit()`.** That also writes a
  request-level property; this plugin runs before the finisher, so lowering it
  would make Envoy truncate a large body with a 413 at this point in the chain.

## The five cases where it abstains

Abstaining is not an error: this entry casts no vote and the total is decided by
the others (least-load, or the built-in round-robin when there are none).

| Case | Reason |
| --- | --- |
| No candidate set | LB is not enabled on this route, or this is the fallback redirect pass (where the publisher deliberately publishes nothing) |
| Candidates carry a `weight` | This route has a business split and the finisher will ignore every scoring opinion (§4.2). Continuing is pure waste, and it would leave an opinion in the logs that never took effect |
| **No session key** | See below |
| No entry in the chain yielded a value | See below |
| Path or `content-type` does not match, for a body source | See below |

**"No session key means no opinion" is the most important line in this plugin.**
The overwhelming majority of requests (stateless `chat/completions`, the first
request of a session) have no key. Hashing the empty string for those would give
them all the same ranking and pin them to one instance -- load balancing would
fail outright, and silently.

## The cost of body sources

⚠️ **`maxBodyBytes` has to be raised here, not left to the finisher.** This
plugin stops iteration at 780 and `gpustack-lb` only raises the decoder buffer
limit at 700, so by then Envoy has already buffered under the route default —
1 MiB in stock Envoy, and Higress deployments run values as low as 32 KiB. A
`/v1/messages` body carrying images would take a 413, which would mean
*installing this plugin changes which requests succeed*. The default therefore
matches `gpustack-lb`'s, and the two have to be configured together; setting
this one **lower** than the finisher's reintroduces the 413.


A body source has to buffer the body, and so does the finisher. The two **do not
coexist** (the finisher's `decodeHeaders` only starts after this plugin's
`decodeData` returns Continue), so peak memory does not double; the cost is one
extra full copy into wasm linear memory per request plus one extra JSON parse.

`enableOnPathSuffix` defaults to just `/responses` and `/messages` to confine
that cost to the endpoints that can actually carry a session key. It
**deliberately excludes `/chat/completions`** -- the hottest path, with no known
standard session key on it today. **Do not change it to `"*"` either**, which
amounts to paying the cost on every request.

Writing the chain with header sources only avoids the cost entirely (iteration
is never stopped when `hasBodySource()` is false).

### ⚠️ `previous_response_id` is unusable on llama.cpp

Measured (2026-09-14, design doc appendix A.7): llama.cpp serves `/v1/responses`
and mints `resp_*` ids, but returns a **hard 400** for `previous_response_id`:

```json
{"error":{"code":400,"message":"llama.cpp does not support 'previous_response_id'.","type":"invalid_request_error"}}
```

A real id and a fabricated one get exactly the same message, which shows it is
rejected during parameter validation without ever consulting storage. So:

- That field **never appears in a successful request**, and stickiness neither
  helps nor hurts it;
- The premise that "stickiness would fix Responses API continuation" **does not
  hold** on this backend -- whichever instance it lands on, the answer is the
  same 400.

The recommended chain therefore **omits** `previous_response_id` in favour of
`prompt_cache_key`, which requires the engine to store nothing.

**vLLM is untested** -- its Responses API implementation differs from
llama.cpp's, so enabling `bodyKey` on that kind of route requires confirming
against the version actually deployed.

## Tests

```bash
make -C extensions test PLUGIN_NAME=gpustack-lb-session-affinity
```

They cover HRW's four properties (same key picks the same candidate, order
independence, removal disturbs only its own share, addition takes only its own
share), distribution evenness (the mixer's guard), the separator, the output
being rank decay rather than the raw hash, the owner's advantage being
independent of the candidate count, rank determinism, the raw hash's 53-bit
precision, `weight`'s default and guards, the candidate chain's order
preservation and validation, header name lowercasing (while the body path is
**not** lowercased -- gjson paths are case-sensitive), and the type checking and
nested paths of session key extraction.

`parseSessionKeys` / `matchPathSuffix` are pure functions split out of the paths
that need host calls -- host ABI calls panic outside a wasm host, so the
decision logic could not be unit-tested while buried in there.
