package main

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

var resolvedCaller = identity{State: identityResolved, AccessKey: "3192253c1f4a9b7e", UserID: 7}

// Both flags are off by default, so an unreachable server rejects -- the
// behaviour GPUStack ships today, which must not drift by accident.
func TestFailureModeDefaultsToClosed(t *testing.T) {
	var config PluginConfig
	for _, statusCode := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if failureModeAllows(config, resolvedCaller, statusCode) {
			t.Errorf("status %d was allowed through with both flags unset", statusCode)
		}
	}
}

func TestFailureModeAllowsOnlyServiceFailures(t *testing.T) {
	config := PluginConfig{FailureModeAllow: true}

	cases := []struct {
		statusCode int
		want       bool
		why        string
	}{
		{http.StatusInternalServerError, true, "the service failed, which is what the flag is for"},
		{http.StatusServiceUnavailable, true, "same"},
		{http.StatusUnauthorized, false, "a real verdict is never waived by the failure-mode policy"},
		{http.StatusForbidden, false, "same"},
		{http.StatusTooManyRequests, false, "same"},
	}
	for _, tc := range cases {
		if got := failureModeAllows(config, resolvedCaller, tc.statusCode); got != tc.want {
			t.Errorf("status %d: allowed = %v, want %v. %s", tc.statusCode, got, tc.want, tc.why)
		}
	}
}

func TestStatusForFailure(t *testing.T) {
	config := PluginConfig{StatusOnError: http.StatusForbidden}

	cases := []struct {
		statusCode int
		want       uint32
		why        string
	}{
		{http.StatusInternalServerError, http.StatusForbidden, "the service's own failure is not leaked to the client as a 5xx"},
		{http.StatusBadGateway, http.StatusForbidden, "same"},
		{http.StatusUnauthorized, http.StatusUnauthorized, "a real verdict is relayed unchanged"},
		{http.StatusForbidden, http.StatusForbidden, "same"},
	}
	for _, tc := range cases {
		if got := statusForFailure(config, tc.statusCode); got != tc.want {
			t.Errorf("status %d: mapped to %d, want %d. %s", tc.statusCode, got, tc.want, tc.why)
		}
	}
}

// The narrow form is the difference between "an outage must not interrupt
// legitimate traffic" and "an outage opens the models to anyone who asks".
func TestFailureModeAllowAuthenticated(t *testing.T) {
	config := PluginConfig{FailureModeAllowAuthenticated: true}

	cases := []struct {
		name string
		id   identity
		want bool
		why  string
	}{
		{name: "tier 1 identity", id: resolvedCaller, want: true},
		{
			name: "marker identity", want: true,
			id: identity{State: identityResolved, Ref: "58", Consumer: "c.gpustack-9"},
		},
		{
			name: "unresolved", id: identity{State: identityUnresolved}, want: false,
			why: "an uncredentialed or unknown caller is exactly what this form excludes",
		},
		{
			name: "anonymous", want: false,
			id:  identity{State: identityResolved, Anonymous: true},
			why: "anonymity is a resolved identity that names nobody",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureModeAllows(config, tc.id, http.StatusServiceUnavailable); got != tc.want {
				t.Errorf("allowed = %v, want %v. %s", got, tc.want, tc.why)
			}
		})
	}

	if failureModeAllows(config, resolvedCaller, http.StatusForbidden) {
		t.Error("a real verdict was waived")
	}
}

// The blanket form is a superset and must not be narrowed by the other flag.
func TestBlanketFailureModeCoversUnresolvedCallers(t *testing.T) {
	config := PluginConfig{FailureModeAllow: true, FailureModeAllowAuthenticated: true}
	if !failureModeAllows(config, identity{State: identityUnresolved}, http.StatusServiceUnavailable) {
		t.Error("failure_mode_allow must still allow everything when both are set")
	}
}

func TestFailureModeFlagsParse(t *testing.T) {
	var config PluginConfig
	if err := parseGlobalConfig(gjson.Parse(`{"failure_mode_allow_authenticated": true}`), &config); err != nil {
		t.Fatalf("parseGlobalConfig: %v", err)
	}
	if !config.FailureModeAllowAuthenticated || config.FailureModeAllow {
		t.Errorf("parsed %+v; the two flags must be independent", config)
	}
}

// Letting a request through is not just a verdict, it is a set of rewrites the
// request still needs. The one that matters is stripping the client's cookie:
// skipping it sends a GPUStack session credential to the model, and through
// ai-proxy to a third-party provider.
func TestFailOpenStillDerivesTheConsumer(t *testing.T) {
	cases := []struct {
		name string
		id   identity
		want string
		why  string
	}{
		{
			name: "tier 1", want: "3192253c1f4a9b7e.gpustack-7",
			id:  resolvedCaller,
			why: "the gateway named this caller, so the access log should say who it was",
		},
		{
			name: "replayed from a marker", want: "abc.gpustack-9",
			id: identity{State: identityResolved, Ref: "58", Consumer: "abc.gpustack-9"},
		},
		{
			name: "unresolved", want: "",
			id:  identity{State: identityUnresolved},
			why: "nothing is known, so the header is left absent rather than guessed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := localConsumer(tc.id)
			if got != tc.want {
				t.Errorf("consumer = %q, want %q. %s", got, tc.want, tc.why)
			}
		})
	}
}

// The marker is what carries a fail-open request through its own redirect: by
// the fallback pass Authorization has been replaced, so without one the caller
// cannot be named and failure_mode_allow_authenticated would reject it --
// during the very outage the marker exists to survive.
func TestFailOpenMintsAMarker(t *testing.T) {
	config := PluginConfig{
		LocalAuth: LocalAuth{Enabled: true},
		AuthCache: AuthCache{Header: "x-gpustack-auth-cache", SigningKey: []byte(vectorMarkerKey)},
	}

	claims, ok := markerClaimsFor(config, resolvedCaller, "my-org/qwen3-8b", "3192253c1f4a9b7e.gpustack-7", vectorMarkerConn)
	if !ok {
		t.Fatal("a fail-open allow must still be able to mint a marker")
	}
	if claims.ID != "ak:3192253c1f4a9b7e" {
		t.Errorf("marker id = %q", claims.ID)
	}

	// With nothing to name there is nothing to put in a marker either.
	if _, ok := markerClaimsFor(config, identity{State: identityUnresolved}, "m", "", vectorMarkerConn); ok {
		t.Error("minted a marker for a caller that was never named")
	}
}
