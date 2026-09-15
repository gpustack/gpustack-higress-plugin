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

import "github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"

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

	KindInstance = wire.KindInstance
	KindProvider = wire.KindProvider
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

// candidateSpec is the **config-parse-time** candidate: the wire contract plus
// a concurrency cap.
//
// maxRunningRequests stays out of wire.Candidate because it is an input to the
// filter, and the filter is applied by the publisher before publishing.
// Downstream never sees over-cap candidates, so carrying the number would only
// invite capability plugins to re-implement the filter -- and worse, it does
// not exist in the JSON at all, so they would always read 0 and conclude
// "no cap".
type candidateSpec struct {
	wire.Candidate
	maxRunningRequests int64
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
