package main

import (
	"errors"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
)

// The shared state has exactly one writer (the finisher role) and one reader
// (the context role), and this file is the seam between them and wherever that
// state actually lives.
//
// Two backends implement it:
//
//	sharedDataStore  proxy-wasm shared data. The default, zero dependencies.
//	                 Scope is **one Envoy process**, so several gateway replicas
//	                 each keep their own count and their own health verdict.
//	redisStore       opt-in via the `redis` config block. Scope is the whole
//	                 deployment, at the price of one blocking round-trip on the
//	                 request path (see redis_store.go).
//
// The seam is deliberately **not** the three shared-data keys themselves: the
// redis backend does not store an inflight timestamp array at all (it uses a
// sorted set, which prunes and counts without a read-modify-write), and a
// health update there is one atomic script rather than a CAS loop. What the two
// genuinely have in common is this handful of operations.

// clusterState is everything the context role needs to know about one cluster
// in order to filter and score it. It is keyed by **cluster name, not by
// Candidate.Key()**: several candidates can share a cluster and differ only in
// targetId, and in-flight load and health describe the backend, which they
// genuinely share.
type clusterState struct {
	// Inflight is the number of requests in flight that have not aged out.
	Inflight int64
	Health   healthState
}

// outcome is what the finisher learned about the request when it ended.
//
// outcomeIgnored exists for `kind: provider`: the in-flight entry still has to
// be released, but a provider's single DNS cluster has many endpoints behind
// it, so one failed request must not eject the whole thing.
type outcome int

const (
	outcomeIgnored outcome = iota
	outcomeSuccess
	outcomeFailure
)

// doneRecord is the whole of what the finisher hands the store when a request
// ends. The **threshold is already resolved** (tightened to 1 during
// probation), so no backend has to know what probation means -- and neither one
// can resolve it differently from the other.
type doneRecord struct {
	Cluster string
	// Token is what AddInflight returned. Empty means nothing was written and
	// there is nothing to release.
	Token   string
	Outcome outcome

	NowMs    int64
	MaxAgeMs int64

	// Threshold, CooldownMs and RampMs are only read on the failure path.
	Threshold  int64
	CooldownMs int64
	RampMs     int64
}

type stateStore interface {
	// LoadStates fetches the state of every cluster and then invokes done
	// **exactly once**, with a map keyed by cluster name. A cluster missing
	// from the map has no state yet, which is the same thing as zero in-flight
	// requests and a clean health record.
	//
	// The returned action has to be returned from the filter handler
	// unmodified. A backend that pauses the request owns the matching resume;
	// callers must not resume on their own.
	//
	// ⚠️ done may be invoked **after** LoadStates returns (that is the whole
	// point of the async backend), so anything it needs must be captured by the
	// closure rather than read afterwards.
	LoadStates(clusters []string, nowMs, maxAgeMs int64, done func(map[string]clusterState)) types.Action

	// AddInflight records one in-flight request and returns the token that
	// identifies it, to be handed back in doneRecord.Token.
	//
	// An empty token means **nothing was written** (the backend gave up, or the
	// dispatch failed). The finisher passes it back anyway; every backend
	// treats it as "nothing to release", which keeps the paired call from
	// spending a write on a record that never existed.
	AddInflight(cluster string, nowMs, maxAgeMs int64) string

	// Done releases the in-flight entry and records the health outcome. The two
	// are one operation rather than two because they always happen together, in
	// the same hook, for the same cluster -- and on the redis backend that lets
	// them share a single round-trip.
	Done(rec doneRecord)
}

// redisConfigured reports whether this config location declares a redis block.
//
// ⚠️ **IsObject(), not Exists().** gjson's contract makes Exists() true for an
// explicit `redis: null` literal, which would send an empty settings object
// into the parser and fail the whole config with "service_name must not be
// empty". rule_matcher swallows that error and leaves the global config
// zero-valued, so every matchRule would then inherit empty path filters and the
// plugin would silently no-op every request -- the exact failure
// gpustack-rate-limit documents for its own `redis` field. `redis: null` is a
// reasonable way to write "none"; treat it as absent.
//
// Everything that is neither an object nor null is rejected by buildStateStore
// before this is consulted, so "not an object" here always means "absent".
func redisConfigured(j gjson.Result) bool {
	return j.Get("redis").IsObject()
}

// buildStateStore picks the backend for this config location. No redis block
// means shared data, which is the zero-dependency default.
//
// **A malformed value is an error, not "absent".** `redis: []`, `redis: "yes"`
// and `redis: 1` are none of them an object, so reading them as "no redis
// configured" would silently drop the deployment back to per-process shared
// data -- an operator would believe fleet-wide state is on while every gateway
// replica quietly kept its own counts, and nothing anywhere would say so. This
// is the same rule parseLB applies to candidates and modelMappers, for the same
// reason; it was inconsistent to leave this one field out.
func buildStateStore(j gjson.Result) (stateStore, error) {
	r := j.Get("redis")
	if r.Exists() && r.Type != gjson.Null && !r.IsObject() {
		return nil, errors.New("redis must be an object")
	}
	if !r.IsObject() {
		return sharedDataStore{}, nil
	}
	settings, err := parseRedisSettings(r)
	if err != nil {
		return nil, err
	}
	return newRedisStore(settings)
}

// candidateClusters is the de-duplicated cluster list for a candidate set, in
// config order.
//
// De-duplication is not an optimisation: gpustack's several-model-names-on-one-
// cluster case puts the same cluster in the candidate list more than once, and
// on the redis backend a repeated cluster would mean the same key appearing
// twice in one KEYS array -- extra work per request, and a needless second copy
// of the same answer.
func candidateClusters(candidates []candidateSpec) []string {
	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, dup := seen[c.Cluster]; dup {
			continue
		}
		seen[c.Cluster] = struct{}{}
		out = append(out, c.Cluster)
	}
	return out
}
