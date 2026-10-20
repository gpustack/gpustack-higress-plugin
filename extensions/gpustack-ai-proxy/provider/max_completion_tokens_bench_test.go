// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
//
// Cost of the max_tokens -> max_completion_tokens normalization lookup
// strategies considered in the review of gpustack/gpustack-higress-plugin#49:
// the shipped hybrid (one GetBytes for model + a single ForEach pass over
// ParseBytes(body), taken only after the model matched a restricted family),
// the original all-GetBytes variant, and the suggested ParseBytes + Result.Get
// variant. gjson is lazy — Result.Get re-scans t.Raw on every call — so "parse
// once, query many" via Result.Get does not exist; both single-pass entry
// points (ParseBytes, GetBytes(body, "@this")) cost one full-body copy. The
// numbers below pin the trade: the suggested variant eagerly parses before the
// model check and is ~50x slower with +64 KiB per op on the legacy
// short-circuit path (most traffic), while the shipped hybrid is ~2x faster
// than all-GetBytes on restricted-model lookups and end-to-end ~10% faster on
// the rewrite path. Bodies are shaped like real Chat Completions traffic
// (model first, then a large messages array, then the token parameters), so
// every key lookup past "model" is a near-full-body scan. The ratio between
// variants is the point; the legacy short-circuit case is included to show the
// common path pays ~one tiny scan.

package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func benchChatCompletionBody(model string, targetBytes int, maxTokens, maxCompletionTokens bool) []byte {
	const overhead = 128
	filler := strings.Repeat("x", targetBytes-overhead)
	var sb strings.Builder
	sb.Grow(targetBytes + 256)
	fmt.Fprintf(&sb, `{"model":%q,"messages":[{"role":"user","content":"%s"}]`, model, filler)
	if maxTokens {
		sb.WriteString(`,"max_tokens":1024`)
	}
	if maxCompletionTokens {
		sb.WriteString(`,"max_completion_tokens":2048`)
	}
	sb.WriteString(`}`)
	return []byte(sb.String())
}

// rewriteGetBytesVariant is the original implementation: one GetBytes per key.
func rewriteGetBytesVariant(body []byte, modelPrefixes []string) ([]byte, error) {
	model := gjson.GetBytes(body, "model").String()
	if !requiresMaxCompletionTokens(model, modelPrefixes) {
		return body, nil
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if !maxTokens.Exists() {
		return body, nil
	}
	if maxTokens.Type == gjson.Null || gjson.GetBytes(body, "max_completion_tokens").Exists() {
		return sjson.DeleteBytes(body, "max_tokens")
	}
	body, err := sjson.SetRawBytes(body, "max_completion_tokens", []byte(maxTokens.Raw))
	if err != nil {
		return nil, err
	}
	return sjson.DeleteBytes(body, "max_tokens")
}

// rewriteParseBytesGetVariant is the strategy suggested in the PR review:
// gjson.ParseBytes once, then Result.Get per key.
func rewriteParseBytesGetVariant(body []byte, modelPrefixes []string) ([]byte, error) {
	parsed := gjson.ParseBytes(body)
	model := parsed.Get("model").String()
	if !requiresMaxCompletionTokens(model, modelPrefixes) {
		return body, nil
	}
	maxTokens := parsed.Get("max_tokens")
	if !maxTokens.Exists() {
		return body, nil
	}
	if maxTokens.Type == gjson.Null || parsed.Get("max_completion_tokens").Exists() {
		return sjson.DeleteBytes(body, "max_tokens")
	}
	body, err := sjson.SetRawBytes(body, "max_completion_tokens", []byte(maxTokens.Raw))
	if err != nil {
		return nil, err
	}
	return sjson.DeleteBytes(body, "max_tokens")
}

// lookupOnly variants isolate the key-lookup cost from the sjson rewrite cost
// (SetRawBytes + DeleteBytes each rebuild the whole body and dominate the
// rewrite path).
func lookupGetBytesVariant(body []byte, prefixes []string) int {
	model := gjson.GetBytes(body, "model").String()
	if !requiresMaxCompletionTokens(model, prefixes) {
		return 0
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	if !maxTokens.Exists() {
		return 1
	}
	if maxTokens.Type == gjson.Null || gjson.GetBytes(body, "max_completion_tokens").Exists() {
		return 2
	}
	return 3
}

func lookupParseBytesGetVariant(body []byte, prefixes []string) int {
	parsed := gjson.ParseBytes(body)
	model := parsed.Get("model").String()
	if !requiresMaxCompletionTokens(model, prefixes) {
		return 0
	}
	maxTokens := parsed.Get("max_tokens")
	if !maxTokens.Exists() {
		return 1
	}
	if maxTokens.Type == gjson.Null || parsed.Get("max_completion_tokens").Exists() {
		return 2
	}
	return 3
}

func lookupForEachVariant(body []byte, prefixes []string) int {
	model := gjson.GetBytes(body, "model")
	if !requiresMaxCompletionTokens(model.String(), prefixes) {
		return 0
	}
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
		return 1
	}
	if maxTokens.Type == gjson.Null || maxCompletionTokens.Exists() {
		return 2
	}
	return 3
}

func BenchmarkMaxTokenLookupOnly(b *testing.B) {
	scenarios := []struct {
		name                string
		model               string
		maxTokens           bool
		maxCompletionTokens bool
	}{
		{"legacy_model_short_circuit", "gpt-4o", true, false},
		{"gpt5_with_max_tokens", "gpt-5.6-luna", true, false},
		{"gpt5_with_both", "gpt-5.6-luna", true, true},
	}
	variants := []struct {
		name string
		fn   func([]byte, []string) int
	}{
		{"getBytes_per_key", lookupGetBytesVariant},
		{"parseBytes_then_get", lookupParseBytesGetVariant},
		{"forEach_single_pass", lookupForEachVariant},
	}
	for _, sc := range scenarios {
		body := benchChatCompletionBody(sc.model, 64*1024, sc.maxTokens, sc.maxCompletionTokens)
		for _, v := range variants {
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				var sink int
				for i := 0; i < b.N; i++ {
					sink += v.fn(body, openaiMaxCompletionTokenModelPrefixes)
				}
				if sink == -1 {
					b.Fatal("unreachable")
				}
			})
		}
	}
}

func BenchmarkRewriteMaxTokensToMaxCompletionTokens(b *testing.B) {
	scenarios := []struct {
		name                string
		model               string
		maxTokens           bool
		maxCompletionTokens bool
	}{
		{"legacy_model_short_circuit", "gpt-4o", true, false},
		{"gpt5_with_max_tokens", "gpt-5.6-luna", true, false},
		{"gpt5_with_both", "gpt-5.6-luna", true, true},
	}
	variants := []struct {
		name string
		fn   func([]byte, []string) ([]byte, error)
	}{
		{"shipped_hybrid", rewriteMaxTokensToMaxCompletionTokens},
		{"getBytes_per_key", rewriteGetBytesVariant},
		{"parseBytes_then_get", rewriteParseBytesGetVariant},
	}
	for _, sc := range scenarios {
		body := benchChatCompletionBody(sc.model, 64*1024, sc.maxTokens, sc.maxCompletionTokens)
		for _, v := range variants {
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for i := 0; i < b.N; i++ {
					if _, err := v.fn(body, openaiMaxCompletionTokenModelPrefixes); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
