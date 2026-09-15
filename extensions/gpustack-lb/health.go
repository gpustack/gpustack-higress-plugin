package main

import (
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// Envoy's legacy response-flag bitmap; the bit number is the ordinal of the
// CoreResponseFlag enum (envoy/stream_info/stream_info.h). **Only the flags we
// actually use are listed.** When adding one, count it off against that header
// rather than copying a number out of any documentation.
const (
	flagNoHealthyUpstream             = 1 << 1  // UH
	flagUpstreamConnectionFailure     = 1 << 5  // UF
	flagUpstreamConnectionTermination = 1 << 6  // UC
	flagNoClusterFound                = 1 << 24 // NC
)

// connectivityFailureMask covers the failures that never reached the
// application.
//
// UT (upstream timeout) is deliberately **excluded**: a long LLM request that
// times out may simply be slow, and since the AI route's own timeout is 0s
// that flag most likely comes from a cluster-level timeout. Counting it would
// eject instances during perfectly normal long generations. Revisit with data.
//
// Upstream 5xx is also **excluded** -- that is an application-level error, the
// instance is alive, and ejecting it would be collateral damage. 5xx does not
// appear in the response flags at all, so looking only at flags already rules
// it out.
const connectivityFailureMask = flagNoHealthyUpstream |
	flagUpstreamConnectionFailure |
	flagUpstreamConnectionTermination |
	flagNoClusterFound

// isConnectivityFailure reads the response flags to decide whether this
// failure should count against health.
//
// The local 503 Envoy synthesises when it cannot connect still traverses the
// encoder filter chain, and onHttpStreamDone (the log phase) fires either way,
// so a value is always available here.
//
// ⚠️ One class of failure is undetectable: **connected but never answers**
// (engine OOM, deadlock, still loading, or a worker proxy that accepted the
// connection but cannot reach the engine). In that case neither flags nor
// code_details carry anything until the route timeout -- and the AI route's
// timeout is 0s. This is a known blind spot; do not assume passive marking
// covers it.
func isConnectivityFailure() bool {
	data, err := proxywasm.GetProperty([]string{"response", "flags"})
	if err != nil || len(data) == 0 {
		return false
	}
	return leUint(data)&connectivityFailureMask != 0
}

// leUint decodes the little-endian integer a property returns. Both
// response.flags and response.code come back as raw integers rather than
// decimal text.
func leUint(data []byte) uint64 {
	var v uint64
	for i := len(data) - 1; i >= 0; i-- {
		v = v<<8 | uint64(data[i])
	}
	return v
}
