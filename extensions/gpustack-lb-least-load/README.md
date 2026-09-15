# gpustack-lb-least-load

A **capability plugin** of the LB framework: it sends each request to the
instance with the lowest current load.

```text
795  gpustack-lb (mode: context)        publishes the candidate set (inflight /
                                        penalty already included)
780  gpustack-lb-session-affinity       session stickiness (when deployed)
760  gpustack-lb-prefix                 prefix affinity (enterprise edition)
740  this plugin                        reads the set -> scores load -> appends
                                        one opinion
700  gpustack-lb (mode: finisher)       L1-normalises + weighted sum -> picks a
                                        candidate -> writes the cluster header
```

Design reference: the LB framework design doc §3.1, §4.2, §5, §7.1, §8.3.

Priority within the capability band **carries no meaning** -- the entries are
combined by weighted sum, so who writes first does not change the result. To
change how much say this plugin has, change its `weight`, not its priority.

## Configuration

```yaml
config:
  enabled: true     # defaults to true
  weight: 1         # defaults to 1
```

| Field | Default | Description |
| --- | --- | --- |
| `enabled` | `true` | Per-route switch. matchRules inherit `defaultConfig` when silent |
| `weight` | `1` | This entry's vote multiplier in the weighted sum, published with the opinion. `<= 0` is treated as 1 |

Both shapes are expressible:

```yaml
# on globally, off for a few
defaultConfig: {}
matchRules:
  - ingress: [ai-route-route-9.internal]
    config: { enabled: false }

# off globally, on for a few
defaultConfig: { enabled: false }
matchRules:
  - ingress: [ai-route-route-2.internal]
    config: { enabled: true }
```

The latter can also be had with `defaultConfigDisable: true` on the CR, but that
is a **deployment-level** switch (the plugin does not run at all on routes not
listed in matchRules) whereas `enabled` is a **config-level** one -- a single
matchRule can list many ingresses, and flipping one boolean reviews more easily
than adding and removing entries. The two stack; either one off means off.

⚠️ `enabled` is a `*bool` rather than a `bool` in Config: when the global config
fails to parse, rule_matcher **swallows the error and leaves `globalConfig` at
its zero value**. A bare bool's zero value is `false`, so one unrelated typo
would silently degrade every route to round-robin -- no error, only the kind of
"hmm, this looks unbalanced" symptom you notice long after the fact.

`weight` uses a bare `float64`: 0 is not a meaningful value ("do not
participate" is expressed with `enabled`), so the zero value and "unset" mean
the same thing and no pointer is needed.

## Scoring

```go
score = 1 / (1 + Inflight + Penalty)
```

**The two terms can simply be added**, because the publisher has already
converted the penalty into the same unit as the in-flight count:

```text
Penalty = decay fraction × (max in-flight across the set + 1)
```

So this plugin does not need to know `rampMs`, and does not need to read any
shared state. P₀ adapts to how busy the cluster is -- a recovering candidate
ranks behind everyone at t=0, then returns linearly to normal.

Walk through a worked example. A at 12 in flight, B at 2, C just released from
cooldown (0 in flight, decay fraction 0.6, P₀ = 13):

| | Inflight | Penalty | Effective load | score |
| --- | --- | --- | --- | --- |
| A | 12 | 0 | 12 | 0.0769 |
| B | 2 | 0 | 2 | **0.3333** ← wins |
| C | 0 | 0.6×13 = 7.8 | 7.8 | 0.1136 |

C has the lowest in-flight count yet ranks between A and B -- without the
penalty it would be saturated the instant it came back. C only overtakes B once
the penalty has decayed to 0.15 (`Penalty` = 1.95), which is 85% of the way
through the ramp window.

The busier the cluster, the later the crossover (P₀ tracks maxLoad): a busy
cluster (max 12 in flight) waits until 8500ms, an idle one (max 3) is back at
6000ms. This is intentional -- **the penalty delays a candidate's return in
proportion to the harm an immediate return would do.** To get capacity back
faster, lower the finisher's `rampMs`.

### This transform carries meaning; do not casually swap it

The finisher **L1-normalises** each entry (dividing by that entry's own sum, so
each casts one vote) and then accumulates by weight. The **shape** of `f(load)`
therefore directly decides how concentrated this entry's vote is:

| Load comparison | Raw score ratio | Meaning |
| --- | --- | --- |
| 0 → 1 | `1.0 / 0.5` = **2×** | Idle to one in flight is a big deal |
| 100 → 101 | `0.00990 / 0.00980` = **1.01×** | One more out of a hundred is noise |

`1/(1+load)` thus encodes diminishing marginal sensitivity: when the candidates
are close this entry votes evenly (near neutral, letting stickiness and the
others speak), and only when they are far apart does it vote sharply. That is
exactly why "stickiness holds through a trivial load gap and gives way to a real
one" works at all.

⚠️ **Do not replace it with `-load` or `(max-load)/max` on the grounds that
"the argmax is the same".** That reasoning only held under the earlier "first to
state an opinion wins" model and does not apply at all under a weighted sum. To
tune "how much say this entry has", use `weight`, which is the knob designed for
exactly that.

## There are no scoring parameters, and that is a conclusion, not a gap

| What one might configure | Its better home |
| --- | --- |
| What counts as "load" | `Inflight + Penalty` is a **definition**, not a preference; a knob would just permit a wrong definition |
| Penalty decay window | The finisher's `rampMs` -- it is the writer that does the ejecting, and the window is stamped into shared state at that moment |
| Capacity ceiling | The candidate's `maxRunningRequests`; that is a **topology fact**, in the same class as `cluster` |
| Tie handling | Random, to avoid a thundering herd. A correctness requirement, not a matter of taste |
| The scoring transform | See above -- it carries meaning; to tune influence, use `weight` |

**Configuration follows the fact, not the consumer.** So this plugin is a pure
function of the published candidate set -- no cross-request state, no CAS, no
leak handling to write.

## Three things it does not do

- **Never calls `ctx.DisableReroute()`.** That writes the request-level property
  `clear_route_cache=off` (it takes effect across plugins), and the finisher's
  `x-higress-target-cluster` write depends on the route being re-evaluated.
  Calling it makes the override silently ineffective.
- **Never reads shared data.** Load and penalty were already computed by the
  publisher and come down with the candidate set (§7.1). Reading them again
  would return the same numbers plus N host calls -- and the publisher's read
  is **unavoidable** anyway (the ejection filter needs it), so reusing it costs
  nothing at the margin.
- **Never touches the body at all**, so it never stops iteration; it does not
  even register `ProcessRequestBody`.

## It does not break ties itself

On an idle cluster every candidate is at zero in flight, which is the **normal
case rather than an edge case**. Emitting exactly equal scores (after L1 each
gets 1/N, the same constant for everyone, automatically neutral) leaves the
reservoir sampling to the finisher's `bestTotal` -- that is already implemented
and tested, and randomising again here would be a second implementation of it,
which would eventually disagree.

Equal loads must produce **bit-for-bit equal** float64 values, or the reservoir
sampling never triggers at all. `TestEqualLoadsTieExactly` pins this down.

## Two defects that cannot be fixed (they belong in the UX docs)

1. **In flight ≠ the GPU is decoding**: a request that has been streaming for
   two minutes and one that was just issued carry the same weight.
2. **Traffic the engine receives from elsewhere is invisible** (other gateway
   replicas, direct connections, other clients).

Accepting them rests on this: the baseline being compared against is
round-robin / weighted random, and least-outstanding really is markedly better
than round-robin for a workload with LLM's variance in duration. **Do not expect
it to be equivalent to engine metrics.**

### Phase two: how engine metrics get wired in

The agreed shape (design §11): `lb-metrics` **does not score and does not join
the request chain**. It only registers a tick to scrape the engine's `/metrics`
→ writes shared data → **context reads it and publishes it onto the
`Candidate`**, exactly the same path the in-flight count takes. This plugin
still does not read shared data.

Switching criteria is a branch inside this plugin; there is **no new
metrics-aware capability plugin**:

```go
load := float64(c.Inflight) + c.Penalty
if c.EngineRunning != nil {                                  // metrics available
    load = float64(*c.EngineRunning+*c.EngineWaiting) + c.Penalty
}
```

Why a separate plugin was rejected: it answers the **same question** as this one
(who is least loaded), just with a better sensor. If both took part in the sum,
the load dimension would get two votes -- tuning "how much load matters" would
mean editing two plugins' `weight`, and the two would trade off against each
other as metrics went fresh and stale. Swapping the sensor for the same
capability is an implementation detail and should not become a second voter.

Two accompanying rules: **all or nothing** (local in-flight and the engine's
`running + waiting` are in different units, so if any candidate lacks metrics
the whole set falls back to local counting); and **freshness is judged in
context** (stale means the field is not published, which reduces this plugin's
rule to "is the field there").

## Known limits

`maxRunningRequests` is **not published downstream**, so this plugin **cannot
normalise by capacity** -- a small card with 8 concurrency at 4 in flight and a
big card with 32 at 4 in flight score identically. Least-outstanding
self-corrects for differences in **speed** (a fast card drains quicker, so its
in-flight count is naturally lower and it receives more), but it does not
correct for differences in **capacity**. When that really matters, give the
candidates a `weight` (which switches to the weighted dice roll).

## Tests

```bash
make -C extensions test PLUGIN_NAME=gpustack-lb-least-load
```

They cover monotonicity and range, `weight`'s default and inheritance, the
penalty and in-flight count being additive in the same unit, bit-for-bit
equality of ties, floor protection against absurd inputs (the candidate set is
written by another binary, and `load = -1` would divide by zero), the design
doc's worked example, the `weight` early exit, the four inheritance combinations
of `enabled`, and a zero-valued global not silently disabling the plugin.

They are all pure logic tests -- this plugin has no decision logic that needs a
host call to test, which follows directly from "a capability plugin is a pure
function of the candidate set".
