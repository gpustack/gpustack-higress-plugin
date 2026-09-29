package provider

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// Port used when the endpoint carries no explicit port and the cluster is
// derived from the host (Higress DNS-service cluster naming).
const defaultSystemonePort = "443"

// Systemone is the GENERIC Jev-compatible decision provider (the wire
// contract of jevcompat SPEC 0.1, POST /v1/systemone): the "jev" system-1
// decision engine, self-deployed or any Jev-compatible server. The router
// sends it {state: task, questions: {model_selection: ...}} and reads
// answers.model_selection to score the candidates. It is also the base the
// hosted-flavour provider (typesafe) inherits from.
type Systemone struct {
	cfg *ProviderConfig
}

// NewSystemone creates the generic Jev-compatible decision provider.
func NewSystemone(cfg *ProviderConfig) *Systemone {
	return &Systemone{cfg: cfg}
}

func (s *Systemone) Name() string { return TypeSystemone }

// DecisionPath is the canonical Jev endpoint the callout posts to —
// /v1/systemone, the path every Jev-compatible server serves (the
// /evaluate alias of the original system-one server is deliberately not
// used: /v1/systemone is the jevcompat spec's MUST endpoint).
func (s *Systemone) DecisionPath() string { return "/v1/systemone" }

// DecisionHost strips the scheme from the configured endpoint for the
// callout's :authority header.
func (s *Systemone) DecisionHost() string { return stripScheme(s.cfg.Endpoint) }

// DecisionCluster is the Envoy cluster the callout is dispatched to — the
// configured cluster wins; otherwise it is derived from the endpoint host
// following Higress's DNS-service cluster naming (outbound|<port>||<host>).
// The default port follows the endpoint scheme: 443 for https:// (or no
// scheme), 80 for http://.
func (s *Systemone) DecisionCluster() string {
	if s.cfg.Cluster != "" {
		return s.cfg.Cluster
	}
	defaultPort := defaultSystemonePort
	if strings.HasPrefix(s.cfg.Endpoint, "http://") {
		defaultPort = "80"
	}
	host, port := splitHostPort(s.DecisionHost(), defaultPort)
	return "outbound|" + port + "||" + host
}

// DecisionModel returns the configured decision-engine model ("" = omit the
// field, server default).
func (s *Systemone) DecisionModel() string { return s.cfg.Model }

// splitHostPort splits host[:port]; port defaults when absent.
func splitHostPort(hostport, defaultPort string) (string, string) {
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, defaultPort
}

// AuthHeaders injects the optional bearer token (spec auth.bearer).
func (s *Systemone) AuthHeaders(hs http.Header) {
	if s.cfg.APIToken != "" {
		hs.Set("Authorization", "Bearer "+s.cfg.APIToken)
	}
}

// ValidateDecisionRequest checks the composed decision request against the
// jevcompat §3 contract: state present, questions a non-empty object, the
// injected model_selection question a valid choice.
func (s *Systemone) ValidateDecisionRequest(body []byte) error {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return &ValidationError{Loc: []string{"body"}, Msg: "decision request is not valid JSON", Type: "value_error"}
	}
	// Parse once and query the result: every gjson.GetBytes call re-parses
	// the raw bytes from scratch, which is wasteful on this hot path.
	res := gjson.ParseBytes(body)
	if !res.IsObject() {
		return &ValidationError{Loc: []string{"body"}, Msg: "decision request must be a JSON object", Type: "value_error"}
	}
	if !res.Get("state").Exists() {
		return &ValidationError{Loc: []string{"body", "state"}, Msg: "field required", Type: "value_error"}
	}
	questions := res.Get("questions")
	if !questions.Exists() || !questions.IsObject() || len(questions.Map()) == 0 {
		return &ValidationError{Loc: []string{"body", "questions"}, Msg: "must be a non-empty map of questions", Type: "value_error"}
	}
	for id, q := range questions.Map() {
		if q.Get("type").String() != "choice" {
			return &ValidationError{Loc: []string{"body", "questions", id, "type"},
				Msg: "decision questions must be of type choice", Type: "value_error"}
		}
		if n := len(q.Get("criteria").Map()); n < 2 {
			return &ValidationError{Loc: []string{"body", "questions", id, "criteria"},
				Msg: "a choice needs at least 2 options", Type: "value_error"}
		}
	}
	return nil
}

// ValidationError is the jevcompat errors.validation-shape failure form.
type ValidationError struct {
	Loc  []string
	Msg  string
	Type string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.Loc, ".") + ": " + e.Msg
}
