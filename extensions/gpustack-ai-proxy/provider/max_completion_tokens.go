// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Normalization of the deprecated Chat Completions `max_tokens` parameter to
// `max_completion_tokens` for model families that, per the upstream API spec,
// reject it. Currently wired into the openai provider (see openai.go); any
// other provider facing the same restriction calls the same helpers with its
// own model prefix list. See gpustack/gpustack#6505.

package provider

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openaiMaxCompletionTokenModelPrefixes lists the OpenAI model families that,
// per the OpenAI Chat Completions API spec, only accept max_completion_tokens
// and reject the deprecated max_tokens parameter with
// "Unsupported parameter: 'max_tokens' is not supported with this model. Use
// 'max_completion_tokens' instead." (the o-series and gpt-5+ reasoning models).
// Keep in sync with the GPUStack server's _get_model_output_token_dict in
// gpustack/routes/model_provider.py. See gpustack/gpustack#6505.
var openaiMaxCompletionTokenModelPrefixes = []string{"gpt-5", "o1", "o3", "o4"}

// requiresMaxCompletionTokens reports whether the model belongs to one of the
// given families that reject the deprecated max_tokens parameter. The name is
// normalized the same way as the GPUStack server's _get_model_output_token_dict:
// lowercased, and only the segment after the last "/" is considered, so e.g.
// "openai/gpt-5.6-luna" is judged by "gpt-5.6-luna".
func requiresMaxCompletionTokens(model string, modelPrefixes []string) bool {
	name := strings.ToLower(model)
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	for _, prefix := range modelPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// rewriteMaxTokensToMaxCompletionTokens rewrites the deprecated max_tokens
// parameter to max_completion_tokens when the outbound request targets a model
// that, per the upstream API spec, rejects max_tokens. Clients legitimately
// keep sending max_tokens — it remains the documented parameter for legacy chat
// models, which is why the proxy normalizes per-model instead of pushing the
// distinction to every caller (gpustack/gpustack#6505).
//
// modelPrefixes carries the provider's restricted model families (see
// openaiMaxCompletionTokenModelPrefixes for the OpenAI one). The body is edited
// with raw sjson field writes (no unmarshal) to keep the per-request cost
// constant. If max_completion_tokens is already set (or max_tokens is JSON
// null), max_tokens is dropped instead, since forwarding both is guaranteed to
// be rejected upstream.
func rewriteMaxTokensToMaxCompletionTokens(body []byte, modelPrefixes []string) ([]byte, error) {
	model := gjson.GetBytes(body, "model")
	if !requiresMaxCompletionTokens(model.String(), modelPrefixes) {
		return body, nil
	}
	// One pass over the top-level object instead of one GetBytes per token
	// parameter: max_tokens and max_completion_tokens typically sit after the
	// (large) messages array or are absent entirely, so each GetBytes is a
	// near-full-body scan. gjson is lazy — Result.Get re-scans t.Raw on every
	// call — so "parse once, query many" via Result.Get does not exist; a
	// single ForEach walk is the only way to pay for the body once, and it
	// costs exactly one full-body copy (ParseBytes; gjson has no zero-copy
	// single-pass entry, GetBytes(body, "@this") allocates the same). The
	// parse happens only after the model matched a restricted family, so the
	// legacy-model common path pays nothing. First occurrence per key wins,
	// matching GetBytes semantics on (pathological) duplicate keys. Measured
	// numbers in max_completion_tokens_bench_test.go.
	var maxTokens, maxCompletionTokens gjson.Result
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "max_tokens":
			if !maxTokens.Exists() {
				maxTokens = value
			}
		case "max_completion_tokens":
			if !maxCompletionTokens.Exists() {
				maxCompletionTokens = value
			}
		}
		return true
	})
	if !maxTokens.Exists() {
		return body, nil
	}
	if maxTokens.Type == gjson.Null || maxCompletionTokens.Exists() {
		return sjson.DeleteBytes(body, "max_tokens")
	}
	body, err := sjson.SetRawBytes(body, "max_completion_tokens", []byte(maxTokens.Raw))
	if err != nil {
		return nil, err
	}
	return sjson.DeleteBytes(body, "max_tokens")
}
