// Header plumbing shared by the request-building and response-applying halves
// of the authorization call.
//
// reconvertHeaders / extractHeader / sendResponse are derived from the Higress
// ext-auth plugin's `util` package.
// Upstream: https://github.com/alibaba/higress/blob/c8b82797c51a97faca46e2ae12990453f5026802/plugins/wasm-go/extensions/ext-auth/util/utils.go
// Keep this attribution when editing.

package main

import (
	"net/http"
	"sort"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
)

// Request headers this plugin reads or writes. Lower-case: Envoy normalises
// header names on the way in, and every comparison below assumes it.
const (
	headerAuthorization = "authorization"
	headerXAPIKey       = "x-api-key"
	headerCookie        = "cookie"

	// headerLLMModel names the model the request is for. Injected upstream of
	// this plugin; the marker is bound to its value.
	headerLLMModel = "x-higress-llm-model"

	// headerConsumer is the caller identity the authorization service returns
	// and ai-statistics writes into the access log.
	headerConsumer = "x-mse-consumer"

	// headerKeyRef asserts a `refs`-table identity, the counterpart of
	// headerAccessKey for credentials the plugin cannot verify itself. The
	// server also returns it on a form-A response so the plugin has something
	// to index a resolved identity by.
	headerKeyRef = "x-gpustack-key-ref"

	// headerAccessKey carries the identity the plugin resolved locally, so the
	// server can skip authentication and evaluate policy only (design form B).
	//
	// It must be unforgeable from outside: the transformer plugin strips any
	// client-supplied copy at priority 810, this plugin re-injects a trusted
	// value at 360, and the server only honours it when the gateway token
	// validates. Adding it here without the matching strip rule would let any
	// client claim any access key.
	headerAccessKey = "x-gpustack-access-key"

	// headerDownstreamConn carries this request's client connection address to
	// the authorization call, so the server can bind the marker it mints to it
	// and refuse that marker on any other connection.
	//
	// The plugin is the only party that can read `source.address`, so the
	// server never derives this -- it embeds whatever value arrives here at
	// mint time and compares against whatever arrives on the pass that presents
	// the marker. Both values come from this plugin, set fresh on every call
	// from `source.address`, which is why a client-supplied copy is worthless:
	// it is overwritten with the connection the client is actually on. This
	// closes the gap that the plugin's own marker binding cannot reach -- the
	// server's marker carries no such claim, the plugin cannot verify it
	// locally and forwards it, and without this the server accepts it on any
	// connection. Not declared in allowed_headers for the same reason the
	// marker is not: it is protocol between this plugin and the server, not a
	// client header an operator opted into.
	headerDownstreamConn = "x-gpustack-downstream-conn"
)

// credentialHeaders are the request headers that carry an end-user credential.
// Form B strips all of them from the authorization call: having asserted an
// identity, forwarding the credential too would make the server authenticate
// it anyway, which is exactly the work form B exists to avoid.
var credentialHeaders = []string{headerAuthorization, headerXAPIKey, headerCookie}

// Forward-auth headers, set when endpoint_mode is forward_auth.
const (
	headerOriginalMethod   = "x-original-method"
	headerOriginalURI      = "x-original-uri"
	headerXForwardedProto  = "x-forwarded-proto"
	headerXForwardedMethod = "x-forwarded-method"
	headerXForwardedURI    = "x-forwarded-uri"
	headerXForwardedHost   = "x-forwarded-host"
)

// reconvertHeaders flattens an http.Header into the [][2]string shape the
// wasm host call expects, sorted for a stable request line ordering.
func reconvertHeaders(headers http.Header) [][2]string {
	var ret [][2]string
	if headers == nil {
		return ret
	}
	for k, vs := range headers {
		for _, v := range vs {
			ret = append(ret, [2]string{k, v})
		}
	}
	sort.SliceStable(ret, func(i, j int) bool {
		return ret[i][0] < ret[j][0]
	})
	return ret
}

// extractHeader returns the first value for headerKey, which must be
// lower-case. Empty string when absent.
func extractHeader(headers [][2]string, headerKey string) string {
	for _, header := range headers {
		if strings.EqualFold(header[0], headerKey) {
			return strings.TrimSpace(header[1])
		}
	}
	return ""
}

func sendResponse(statusCode uint32, detail string, headers http.Header, body []byte) error {
	return proxywasm.SendHttpResponseWithDetail(statusCode, detail, reconvertHeaders(headers), body, -1)
}
