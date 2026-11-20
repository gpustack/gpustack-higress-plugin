// systemone.go serves the Jev decision contract (jevcompat SPEC 0.1,
// POST /v1/systemone) as an inference upstream, mirroring the generic
// Systemone provider of the gpustack-lb-decision-service plugin: a
// gpustack-lb-typesafe ModelProvider becomes a `type: systemone` ai-proxy
// catalogue entry whose endpoint/token/fields carry over unchanged.
package provider

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// defaultSystemoneDomain is the hosted TypeSafe API used when the entry
	// carries no endpoint.
	defaultSystemoneDomain = "api.typesafe.ai"

	// systemoneImagesOptInHeader is the hosted TypeSafe API's explicit
	// opt-in for media-bearing requests: one without it answers 400 before
	// the decision runs.
	systemoneImagesOptInHeader = "TypeSafe-Danger-Zone-Images"
)

type systemoneProviderInitializer struct{}

func (m *systemoneProviderInitializer) ValidateConfig(config *ProviderConfig) error {
	// A decision service may run anonymously (self-hosted without auth) or
	// behind apiTokens; both shapes are valid.
	switch config.systemoneDangerZoneHeader {
	case "", "auto", "true", "false":
	default:
		return fmt.Errorf("invalid typesafeDangerZoneHeader %q: want auto, true, or false", config.systemoneDangerZoneHeader)
	}
	return nil
}

func (m *systemoneProviderInitializer) DefaultCapabilities() map[string]string {
	return map[string]string{
		string(ApiNameSystemone): PathSystemone,
		string(ApiNameModels):    PathOpenAIModels,
	}
}

func (m *systemoneProviderInitializer) CreateProvider(config ProviderConfig) (Provider, error) {
	config.setDefaultCapabilities(m.DefaultCapabilities())
	return &systemoneProvider{
		config:       config,
		contextCache: createContextCache(&config),
	}, nil
}

type systemoneProvider struct {
	config       ProviderConfig
	contextCache *contextCache
}

func (m *systemoneProvider) GetProviderType() string {
	return providerTypeSystemone
}

// GetApiName maps the decision contract's own paths. It is consulted in the
// original protocol, which is how GPUStack deploys these entries: without it
// the framework treats every path as unrecognized and never reads the body,
// which would disable the normalization below.
func (m *systemoneProvider) GetApiName(path string) ApiName {
	if strings.HasSuffix(path, PathSystemone) {
		return ApiNameSystemone
	}
	if strings.HasSuffix(path, PathOpenAIModels) {
		return ApiNameModels
	}
	return ""
}

func (m *systemoneProvider) OnRequestHeaders(ctx wrapper.HttpContext, apiName ApiName) error {
	m.config.handleRequestHeaders(m, ctx, apiName)
	return nil
}

// OnRequestBody runs the dialect normalization directly rather than through
// ProviderConfig.handleRequestBody: entries deploy with protocol "original"
// (no protocol translation — the decision contract is the client's own), and
// that helper returns early for original protocols.
func (m *systemoneProvider) OnRequestBody(ctx wrapper.HttpContext, apiName ApiName, body []byte) (types.Action, error) {
	if apiName != ApiNameSystemone && apiName != ApiNameModels {
		return types.ActionContinue, errUnsupportedApiName
	}
	if apiName == ApiNameSystemone {
		newBody, err := m.transformSystemoneBody(body)
		if err != nil {
			return types.ActionContinue, err
		}
		m.ensureImagesOptIn(newBody)
		return types.ActionContinue, replaceRequestBody(newBody)
	}
	// /v1/models needs no body rewriting; pass it through untouched.
	return types.ActionContinue, nil
}

// systemoneBodyHasMedia reports whether the decision request carries images
// in either spelling.
func systemoneBodyHasMedia(body []byte) bool {
	return gjson.GetBytes(body, "assets").Exists() || gjson.GetBytes(body, "images").Exists()
}

// ensureSystemoneImagesOptIn adds the hosted API's media opt-in header
// according to the entry's typesafeDangerZoneHeader setting:
//
//	auto (default) — inject when the request carries images (either
//	                 spelling) and the client did not send one; a
//	                 dialect-agnostic client composing an assets-only
//	                 request may not know the header, and the hosted API
//	                 rejects a media-bearing request without it;
//	true            — ensure the header regardless of media presence;
//	false           — never inject (opt-out), the client's own header
//	                 (or absence) passes through untouched.
//
// Self-hosted Jev servers ignore the extra header, so no endpoint
// distinction is needed. In every mode an explicitly present header is
// never overridden — the client's choice stands, including an explicit
// opt-out.
func (m *systemoneProvider) ensureImagesOptIn(body []byte) {
	switch m.config.systemoneDangerZoneHeader {
	case "false":
		return
	case "true":
	default: // "", "auto"
		if !systemoneBodyHasMedia(body) {
			return
		}
	}
	if _, err := proxywasm.GetHttpRequestHeader(systemoneImagesOptInHeader); err == nil {
		return
	}
	_ = proxywasm.ReplaceHttpRequestHeader(systemoneImagesOptInHeader, "true")
}

// transformSystemoneBody normalizes a both-fields image request and defaults
// the decision-engine model:
//
//   - The hosted TypeSafe schema reads images only through the top-level
//     assets map, so a request also carrying a top-level images array is
//     rejected with api_usage_error. Clients that cannot know the dialect
//     (Playground's undeclared fallback, generic SDKs) send the same images
//     in both spellings with every reference pointing at the assets
//     placeholders — dropping the redundant array loses nothing. Only the
//     redundant-spelling removal happens here; translating one spelling into
//     the other would have to invent references and is deliberately not done.
//   - A request without a model falls back to the configured default, the
//     same rule the decision-service plugin applies to its callouts.
func (m *systemoneProvider) transformSystemoneBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		// Nothing to normalize; without this guard a default model would
		// fabricate a {"model":"..."} body out of an empty request.
		return body, nil
	}
	if gjson.GetBytes(body, "assets").Exists() && gjson.GetBytes(body, "images").Exists() {
		var err error
		if body, err = sjson.DeleteBytes(body, "images"); err != nil {
			return nil, err
		}
	}
	if !gjson.GetBytes(body, "model").Exists() && m.config.systemoneModel != "" {
		var err error
		if body, err = sjson.SetBytes(body, "model", m.config.systemoneModel); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func (m *systemoneProvider) TransformRequestHeaders(ctx wrapper.HttpContext, apiName ApiName, headers http.Header) {
	// The decision contract is served where the client sent it: /v1/systemone
	// and /v1/models pass through with no path rewrite. The Host header must
	// be the endpoint's own authority — a Cloudflare-fronted Jev upstream
	// answers a mismatched Host with 421 Misdirected Request.
	host := systemoneEndpointHost(m.config.systemoneEndpoint)
	if host == "" {
		host = defaultSystemoneDomain
	}
	util.OverwriteRequestHostHeader(headers, host)
	if len(m.config.apiTokens) > 0 {
		// Guard against an empty in-use token (e.g. all tokens temporarily
		// disabled by failover) and an already-prefixed credential, so the
		// upstream never sees a malformed "Bearer " or "Bearer Bearer ...".
		token := m.config.GetApiTokenInUse(ctx)
		if token != "" {
			if !strings.HasPrefix(token, "Bearer ") {
				token = "Bearer " + token
			}
			util.OverwriteRequestAuthorizationHeader(headers, token)
		}
	}
	headers.Del("Content-Length")
}

// dropRedundantSystemoneAssets keeps the top-level images array and drops
// the assets map when both are present — the reverse-direction counterpart
// of the systemone provider's normalization, for images-spelling engines
// (upstream vLLM, whose extra=forbid schema rejects the assets field)
// behind openai/vllm-type entries. As with the other direction, only the
// redundant spelling is removed: the array carries the same images the
// client duplicated into both fields.
func dropRedundantSystemoneAssets(body []byte) ([]byte, error) {
	if gjson.GetBytes(body, "assets").Exists() && gjson.GetBytes(body, "images").Exists() {
		return sjson.DeleteBytes(body, "assets")
	}
	return body, nil
}

// systemoneEndpointHost reduces a configured endpoint to its authority
// (host[:port]); an empty input yields an empty authority.
func systemoneEndpointHost(endpoint string) string {
	authority := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	return strings.TrimSuffix(authority, "/")
}
