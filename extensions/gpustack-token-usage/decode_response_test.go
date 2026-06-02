package main

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/tidwall/gjson"
)

// gzipBytes is a test helper that gzip-compresses src.
func gzipBytes(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(src); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// embeddingsBody mimics an OpenAI /v1/embeddings response: a large data array
// followed by the usage block at the tail. The size guarantees that, in
// production, Envoy would split it across multiple streaming chunks — the
// scenario the non-streaming buffer path exists to handle.
func embeddingsBody() []byte {
	var vec bytes.Buffer
	vec.WriteString(`{"id":"embd-1","object":"list","model":"qwen3-vl-embedding-2b","data":[{"object":"embedding","index":0,"embedding":[`)
	for i := 0; i < 4096; i++ {
		if i > 0 {
			vec.WriteByte(',')
		}
		vec.WriteString("0.0123456789")
	}
	vec.WriteString(`]}],"usage":{"prompt_tokens":38,"total_tokens":38,"completion_tokens":0,"prompt_tokens_details":null}}`)
	return vec.Bytes()
}

const testMaxBytes = 10 * 1024 * 1024

func TestDecodeResponseBody(t *testing.T) {
	plain := embeddingsBody()

	cases := []struct {
		name     string
		body     []byte
		encoding string
		maxBytes int
		wantErr  bool
		wantBody []byte
	}{
		{name: "identity empty", body: plain, encoding: "", maxBytes: testMaxBytes, wantBody: plain},
		{name: "identity literal", body: plain, encoding: "identity", maxBytes: testMaxBytes, wantBody: plain},
		{name: "gzip", body: gzipBytes(t, plain), encoding: "gzip", maxBytes: testMaxBytes, wantBody: plain},
		{name: "gzip case-insensitive", body: gzipBytes(t, plain), encoding: "GZIP", maxBytes: testMaxBytes, wantBody: plain},
		{name: "gzip with spaces", body: gzipBytes(t, plain), encoding: " gzip ", maxBytes: testMaxBytes, wantBody: plain},
		{name: "unsupported brotli", body: plain, encoding: "br", maxBytes: testMaxBytes, wantErr: true},
		{name: "corrupt gzip", body: []byte("not gzip at all"), encoding: "gzip", maxBytes: testMaxBytes, wantErr: true},
		// Zip-bomb guard: a body that decodes past the cap is rejected.
		{name: "gzip over cap", body: gzipBytes(t, plain), encoding: "gzip", maxBytes: 64, wantErr: true},
		// Exactly-at-cap still succeeds (cap == decoded length).
		{name: "gzip exactly at cap", body: gzipBytes(t, plain), encoding: "gzip", maxBytes: len(plain), wantBody: plain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeResponseBody(tc.body, tc.encoding, tc.maxBytes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.wantBody) {
				t.Fatalf("decoded body mismatch")
			}
			// The decoded body must be parseable and carry the tail usage.
			if total := gjson.GetBytes(got, "usage.total_tokens").Int(); total != 38 {
				t.Fatalf("usage.total_tokens want 38, got %d", total)
			}
		})
	}
}
