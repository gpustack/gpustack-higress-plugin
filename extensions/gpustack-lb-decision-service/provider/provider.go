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
	// DecisionClusters returns every Envoy cluster the callout may be
	// dispatched to (configured clusters first, else the endpoint-derived
	// default) — in configured order, no selection applied.
	DecisionClusters() []string
	// DecisionCluster is the cluster THIS callout attempt is dispatched
	// to: when several clusters are configured (e.g. one per GPUStack
	// model instance) one is picked uniformly at random, spreading the
	// decision traffic across the replicas; a single configured or
	// endpoint-derived cluster returns deterministically.
	DecisionCluster() string
	// DecisionModel is the optional `model` field of the decision request
	// ("" omits it — the server's default engine answers).
	DecisionModel() string
	// DecisionHeaders returns extra headers for a callout dispatched to
	// the given cluster. Today that is the clusterNameHeader entry: the
	// cluster's service name (segment 4 of the outbound|<port>||<service>
	// form) under the configured header name, derived per attempt so it
	// follows the randomly picked cluster. nil when no header is
	// configured or the cluster is not in the outbound form.
	DecisionHeaders(cluster string) [][2]string
	// AuthHeaders returns headers (auth, ...) for the decision callout.
	// apiToken is the credential to send this attempt — "" sends none. It
	// comes from EffectiveAPITokens(); passing it per call (instead of
	// reading cfg directly) is what lets the plugin retry the callout with
	// the fallback tokens on auth/quota failures (401/403/429).
	AuthHeaders(hs http.Header, apiToken string)
	// ValidateDecisionRequest validates the composed decision request body.
	ValidateDecisionRequest(body []byte) error
}

// ProviderConfig configures the decision service (one per route; matchRule
// overrides replace it wholesale, ai-proxy `providers` catalogue style).
type ProviderConfig struct {
	Id       string `json:"id"`       // referenced by activeProviderId
	Type     string `json:"type"`     // provider type, e.g. "typesafe"
	Endpoint string `json:"endpoint"` // scheme://host[:port] of the jev service
	APIToken string `json:"apiToken"` // optional bearer token (single-token form)
	// APITokens is the failover token list (spec auth.bearer): the first
	// token serves the traffic; when the decision service answers 401/403/
	// 429 (invalid/exhausted key) the callout is retried with the next
	// one. When set it takes precedence over the legacy single apiToken.
	APITokens []string `json:"apiTokens"`
	// Cluster is the Envoy cluster the decision callout is dispatched to
	// (proxy-wasm DispatchHttpCall needs a cluster, not a URL).
	Cluster string `json:"cluster"`
	// Clusters is the multi-cluster form: several Envoy clusters the
	// decision callout may be dispatched to, one picked uniformly at
	// random per callout. This is how a decision service with several
	// replicas — e.g. a GPUStack model instance set, every instance
	// reachable through its own cluster — serves as the provider: list
	// the instance clusters and the decision traffic spreads across
	// them. When set it takes precedence over the legacy single
	// `cluster` (mirroring apiTokens-over-apiToken).
	Clusters []string `json:"clusters"`
	// Model optionally sets the decision request's `model` field (spec
	// request.model-alias: "jev-latest" MUST be accepted; servers MAY
	// accept other names to pick an engine profile). Empty omits the
	// field — the server then uses its default engine.
	Model string `json:"model"`
	// ClusterNameHeader names a header that carries the chosen cluster's
	// service name — segment 4 of the outbound|<port>||<service> form —
	// on every callout, the same value gpustack-set-header-pre-route
	// writes for routed traffic from the matched cluster. An entrypoint
	// shared by several members (replicas behind one proxy routing by
	// this header) then selects the member the picked cluster names;
	// deriving the value per attempt is what lets it follow the random
	// cluster pick. Empty disables it.
	ClusterNameHeader string `json:"clusterNameHeader"`
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

// EffectiveAPITokens returns the credential list to try, in order. An
// EXPLICIT `apiTokens` field always wins — even an empty (or all-empty)
// list means "anonymous", never a silent fallback to the legacy single
// `apiToken` (an operator writing apiTokens: [] is retiring the keys, not
// asking for the deprecated field to resurrect). Only when the field is
// absent (nil) does the legacy `apiToken` apply; nil APITokens vs empty
// APITokens is exactly the presence signal the parser preserves.
func (c *ProviderConfig) EffectiveAPITokens() []string {
	if c.APITokens != nil {
		var tokens []string
		for _, t := range c.APITokens {
			if t != "" {
				tokens = append(tokens, t)
			}
		}
		return tokens
	}
	if c.APIToken != "" {
		return []string{c.APIToken}
	}
	return nil
}

// EffectiveClusters returns the Envoy cluster list the decision callout may
// be dispatched to, in configured order. An EXPLICIT `clusters` field always
// wins — empty entries are dropped, and an explicit all-empty (or empty)
// list means "no configured cluster" (the endpoint-derived default then
// applies), never a silent fallback to the legacy single `cluster` (an
// operator writing clusters: [] is retiring the explicit cluster, not asking
// the deprecated field to resurrect). Only when the field is absent (nil)
// does the legacy `cluster` apply; nil Clusters vs empty Clusters is exactly
// the presence signal the parser preserves — the same contract as
// EffectiveAPITokens.
func (c *ProviderConfig) EffectiveClusters() []string {
	if c.Clusters != nil {
		var clusters []string
		for _, cl := range c.Clusters {
			if cl != "" {
				clusters = append(clusters, cl)
			}
		}
		return clusters
	}
	if c.Cluster != "" {
		return []string{c.Cluster}
	}
	return nil
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
