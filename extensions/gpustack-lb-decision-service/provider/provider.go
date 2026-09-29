// Package provider defines the decision-service abstraction for the
// gpustack-lb-decision-service plugin. The plugin is a load-balancing
// capability on the gpustack-lb pattern: it asks a Jev-compatible (TypeSafe
// System One) service — the "jev" system-1 model — to pick the backend model
// for a task, then appends a confidence-weighted rank entry to the shared
// gpustack_lb_ranks filter state; the gpustack-lb finisher consumes the rank
// entries and writes x-higress-target-cluster (cluster_header routing).
//
// Protocol reference (jevcompat SPEC 0.1):
// https://github.com/mandu5/jevcompat/blob/main/SPEC.md
package provider

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	// TypeSystemone is the provider id of the generic Jev-compatible
	// (TypeSafe System One wire contract) decision service — self-deployed
	// or any third-party Jev server.
	TypeSystemone = "systemone"

	// TypeTypesafe is the provider id of the TypeSafe-hosted flavour; it
	// inherits the whole wire behaviour from TypeSystemone.
	TypeTypesafe = "typesafe"

	// ModelJevLatest is the official SDKs' default model alias; a
	// Jev-compatible server MUST accept it (spec request.model-alias).
	ModelJevLatest = "jev-latest"
)

// DecisionProvider is the Jev-compatible service the router consults.
type DecisionProvider interface {
	// Name returns the provider id (e.g. "systemone", "typesafe").
	Name() string
	// DecisionPath is the upstream path decision requests are sent to.
	DecisionPath() string
	// DecisionHost is the :authority of the decision callout.
	DecisionHost() string
	// DecisionCluster is the Envoy cluster the callout is dispatched to.
	DecisionCluster() string
	// DecisionModel is the optional `model` field of the decision request
	// ("" omits it — the server's default engine answers).
	DecisionModel() string
	// AuthHeaders returns headers (auth, ...) for the decision callout.
	AuthHeaders(hs http.Header)
	// ValidateDecisionRequest validates the composed decision request body.
	ValidateDecisionRequest(body []byte) error
}

// ProviderConfig configures the decision service (one per route; matchRule
// overrides replace it wholesale, ai-proxy `providers` catalogue style).
type ProviderConfig struct {
	Id       string `json:"id"`       // referenced by activeProviderId
	Type     string `json:"type"`     // provider type, e.g. "typesafe"
	Endpoint string `json:"endpoint"` // scheme://host[:port] of the jev service
	APIToken string `json:"apiToken"` // optional bearer token
	// Cluster is the Envoy cluster the decision callout is dispatched to
	// (proxy-wasm DispatchHttpCall needs a cluster, not a URL).
	Cluster string `json:"cluster"`
	// Model optionally sets the decision request's `model` field (spec
	// request.model-alias: "jev-latest" MUST be accepted; servers MAY
	// accept other names to pick an engine profile). Empty omits the
	// field — the server then uses its default engine.
	Model string `json:"model"`
}

// CreateProvider builds the decision provider for cfg.Type. Two provider
// types ship: `systemone` (the generic Jev-compatible service — the base
// implementation) and `typesafe` (the TypeSafe-hosted flavour, inheriting
// everything from systemone). The wire contract (POST /v1/systemone,
// jevcompat SPEC) is shared, but there is deliberately no hosted default:
// the callout ships task bodies to the service, so every provider must
// name its target via an explicit `endpoint` — the endpoint also supplies
// the callout's :authority, so a cluster-only configuration (empty
// authority, invalid callout) is rejected; `cluster` is an optional
// override for the derived name.
func CreateProvider(cfg *ProviderConfig) (DecisionProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("provider config is nil")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("provider %q needs an explicit endpoint — the decision callout sends task bodies to this service and uses the endpoint host as :authority, so no hosted default applies (cluster alone is not enough)", orType(cfg.Type))
	}
	switch cfg.Type {
	case "", TypeSystemone:
		return NewSystemone(cfg), nil
	case TypeTypesafe:
		return NewTypesafe(cfg), nil
	default:
		// Unknown types still ride the shared wire contract, via the
		// generic systemone implementation (catalogue-forward compatible:
		// a new flavour works before it gets a dedicated type here).
		return NewSystemone(cfg), nil
	}
}

// orType renders the provider type for error messages.
func orType(t string) string {
	if t == "" {
		return TypeSystemone
	}
	return t
}

// stripScheme turns scheme://host[:port][/path][?query] into host[:port]:
// any trailing path or query is dropped, so :authority and the derived
// cluster name never carry it.
func stripScheme(endpoint string) string {
	host := strings.TrimPrefix(endpoint, "http://")
	host = strings.TrimPrefix(host, "https://")
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	return host
}
