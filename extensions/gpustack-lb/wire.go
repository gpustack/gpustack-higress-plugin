package main

// This plugin's binding to the LB framework's on-the-wire format.
//
// **The cross-binary part of the contract is not here** -- it lives in ./wire.
// Capability plugins (least-load, session-affinity, and the enterprise prefix
// plugin) are separately compiled, separately shipped wasm binaries and must
// import the same definition. Hand-mirroring the contract into each plugin
// eventually produces the silent mismatch where one side adds a field and the
// other does not follow.
//
// Only two things stay here: aliases for the types in wire (so the rest of
// this package can keep writing Candidate instead of wire.Candidate), and the
// private shapes that **do not** cross a binary boundary.

import (
	"fmt"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
)

// Aliases rather than redefinitions: an alias *is* the same type, so what
// publishCandidates writes is exactly what the capability plugins import, and
// a field mismatch is a compile error rather than a runtime surprise.
type (
	Candidate    = wire.Candidate
	CandidateSet = wire.CandidateSet
	RankEntry    = wire.RankEntry
)

const (
	FilterStateCandidates = wire.FilterStateCandidates
	FilterStateRanks      = wire.FilterStateRanks

	// FilterStateRequestContext: request measurements (body length) written
	// by generic-proxy-router upstream in the chain; see wire.go.
	FilterStateRequestContext = wire.FilterStateRequestContext

	// FilterStateVideoPin: the deterministic instance pin written by
	// generic-proxy-router for job follow-ups; see wire.go.
	FilterStateVideoPin = wire.FilterStateVideoPin

	KindInstance = wire.KindInstance
	KindProvider = wire.KindProvider
)

// RequestContext is the value stored under FilterStateRequestContext.
type RequestContext = wire.RequestContext

// VideoPin is the value stored under FilterStateVideoPin.
type VideoPin = wire.VideoPin

// Pin modes, aliased for the same reason as the types above.
const (
	PinModeJob    = wire.PinModeJob
	PinModeSelect = wire.PinModeSelect
)

// SharedInflightPrefix / SharedHealthPrefix + <full cluster name>.
//
// **Deliberately not in wire**: the finisher role is the only writer and the
// context role is the only reader, and both live in this binary. Putting them
// in the cross-binary contract would only invite capability plugins to read
// shared data directly, which is what §7.1 avoids -- filtering is applied by
// the publisher, and capability plugins should only see the already-filtered
// candidate set (load and penalty are published on the Candidate already).
const (
	SharedInflightPrefix = "gpustack_lb_inflight_"
	SharedHealthPrefix   = "gpustack_lb_health_"
)

// candidateSpec is the **config-parse-time** candidate.
//
// maxRunningRequests is duplicated onto wire.Candidate at publish time (the
// finisher reads the candidate set from filter state and needs the cap for its
// reserve walk), so this field is the parse-time input to that copy rather
// than the sole owner of the number.
type candidateSpec struct {
	wire.Candidate
	maxRunningRequests int64

	// groups is the candidate's membership: a group narrows to the
	// candidates listing its own name. Absent means "in no group", which every
	// group excludes. It is config-parse-time only -- the finisher never
	// narrows, so it does not ride the wire.
	groups []string
}

// checkServiceKeys rejects candidates whose service names collide. The
// tried-clusters header records the service name alone (see serviceKey), so two
// DIFFERENT clusters answering to one name could not be told apart: excluding
// one would silently drop the other's endpoints. gpustack's renderer cannot
// produce that -- a registry name encodes the instance identity and the port
// comes from that same instance, so one service name means one cluster -- which
// is why this is a check rather than a per-candidate fallback: it only ever
// catches a hand-written config, and refusing is more useful than guessing.
func checkServiceKeys(cands []candidateSpec) error {
	seen := make(map[string]string, len(cands))
	for _, c := range cands {
		k := serviceKey(c.Cluster)
		if prev, ok := seen[k]; ok && prev != c.Cluster {
			return fmt.Errorf(
				"candidates %q and %q share service name %q but are different clusters; the tried-clusters header cannot distinguish them",
				prev, c.Cluster, k)
		}
		seen[k] = c.Cluster
	}
	return nil
}

// serviceKey strips the Envoy cluster prefix, leaving the service name:
// outbound|80||mock-a.ns.svc -> mock-a.ns.svc. Only that exact shape is
// stripped -- direction outbound, empty subset. A subset cluster
// (outbound|80|v1|svc) or a custom four-part name (custom|http|v1|svc) keeps
// its full name: it is a different cluster from the plain one, and collapsing
// the two would either trip checkServiceKeys or exclude the wrong endpoints.
func serviceKey(cluster string) string {
	parts := strings.SplitN(cluster, "|", 4)
	if len(parts) != 4 || parts[0] != "outbound" || parts[2] != "" || parts[3] == "" {
		return cluster
	}
	return parts[3]
}

// healthState is the value stored under SharedHealthPrefix.
type healthState struct {
	// Fails is the consecutive failure count (reset to zero on success).
	Fails int64 `json:"f,omitempty"`
	// EjectedUntil is the millisecond timestamp when the cooldown ends; 0
	// means never ejected.
	EjectedUntil int64 `json:"e,omitempty"`
	// RampUntil is the moment the recovery penalty has decayed to zero.
	//
	// Both timestamps are **stamped into the state by the finisher at the
	// moment of ejection**, rather than leaving the reader to recompute them
	// from the configured windows. That keeps cooldownMs / rampMs in exactly
	// one place, and as a bonus fixes the window **per occurrence** -- editing
	// the config later cannot reinterpret an in-progress cooldown as a
	// different length.
	RampUntil int64 `json:"r,omitempty"`
}

// The decision:
//
//	now <  EjectedUntil              -> ejected
//	EjectedUntil <= now < RampUntil  -> recovering: carries a penalty, and the
//	                                    failure threshold tightens to 1
//	now >= RampUntil                 -> normal

// inflightState stores the start time of every in-flight request rather than a
// counter.
//
// A counter cannot be lazily pruned, and the decrement path is where this kind
// of feature fails (early-terminated streams, client disconnects, upstream
// timeouts). Miss one decrement and the counter only ever grows, eventually
// starving that instance for good. Storing timestamps is what allows expired
// entries to be pruned on the request path.
type inflightState struct {
	Starts []int64 `json:"s,omitempty"`
}
