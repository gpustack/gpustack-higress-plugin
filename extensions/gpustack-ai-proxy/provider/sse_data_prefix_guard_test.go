// GPUStack-local test (no upstream counterpart): guards the `data:` prefix
// check in the SSE event loops of the streaming providers.
//
// A framed SSE event may legally carry non-data fields (event:, id:, comments).
// The upstream loops stripped a fixed 6-byte prefix from every line of length
// >= 6 and fed the remainder to json.Unmarshal, so those fields became error
// log spam and, for a line that happened to parse, silently wrong behavior
// (flagged by gemini-code-assist on PR #53 for gemini, dify, minimax, spark
// and triton). Two providers are exercised end-to-end here; the other three
// carry the identical mechanical guard.

package provider

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiStreamingSkipsNonDataLines(t *testing.T) {
	p := &geminiProvider{config: ProviderConfig{}}
	ctx := newMapCtx()

	// One framed event carrying non-data fields around the data field.
	chunk := []byte("event: message\n" +
		"id: 42\n" +
		": keep-alive comment\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}` +
		"\n\n")

	out, err := p.OnStreamingResponseBody(ctx, ApiNameChatCompletion, chunk, true)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// Exactly one converted chunk: the non-data fields must contribute nothing.
	assert.Equal(t, 1, strings.Count(string(out), "data: "))
	assert.Contains(t, string(out), "hi")
}

func TestSparkStreamingSkipsNonDataLines(t *testing.T) {
	p := &sparkProvider{config: ProviderConfig{}}
	ctx := newMapCtx()

	chunk := []byte("event: add\n" +
		`data: {"id":"1","code":0,"choices":[{"delta":{"role":"assistant","content":"hi"},"index":0}]}` +
		"\n" +
		"data: [DONE]\n" +
		"\n")

	out, err := p.OnStreamingResponseBody(ctx, ApiNameChatCompletion, chunk, true)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	assert.Equal(t, 1, strings.Count(string(out), "data: "))
	assert.Contains(t, string(out), "hi")
}
