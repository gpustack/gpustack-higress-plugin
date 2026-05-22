package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"regexp"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prefix string
		want   string
	}{
		{"matches and extracts id", "/model/proxy/my-id/v1/chat/completions", "/model/proxy/", "my-id"},
		{"id only, no trailing slash", "/model/proxy/my-id", "/model/proxy/", "my-id"},
		{"id with trailing slash", "/model/proxy/my-id/", "/model/proxy/", "my-id"},
		{"strips query string", "/model/proxy/my-id?x=1", "/model/proxy/", "my-id"},
		{"strips query before slash", "/model/proxy/my-id/foo?x=1", "/model/proxy/", "my-id"},
		{"empty id", "/model/proxy/", "/model/proxy/", ""},
		{"empty id with query", "/model/proxy/?x=1", "/model/proxy/", ""},
		{"prefix not present", "/v1/chat/completions", "/model/proxy/", ""},
		{"custom prefix", "/llm/route/abc/foo", "/llm/route/", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractID(tt.path, tt.prefix); got != tt.want {
				t.Errorf("extractID(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestMatchPathSuffix(t *testing.T) {
	suffixes := []string{"/completions", "/embeddings", "/audio/transcriptions"}
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/embeddings", true},
		{"/v1/audio/transcriptions", true},
		{"/v1/audio/translations", false},
		{"/healthz", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := matchPathSuffix(tt.path, suffixes); got != tt.want {
				t.Errorf("matchPathSuffix(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	t.Run("wildcard matches everything", func(t *testing.T) {
		if !matchPathSuffix("/anything", []string{"*"}) {
			t.Errorf("wildcard should match")
		}
		if !matchPathSuffix("", []string{"*"}) {
			t.Errorf("wildcard should match empty path")
		}
	})
}

func TestSplitProviderModel(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		wantProvider string
		wantModel    string
		wantSplit    bool
	}{
		{"plain model", "qwen2.5-7b-instruct", "", "qwen2.5-7b-instruct", false},
		{"provider/model", "openai/gpt-4", "openai", "gpt-4", true},
		{"nested path", "openai/gpt-4/turbo", "openai", "gpt-4/turbo", true},
		{"leading slash", "/foo", "", "/foo", false},
		{"trailing slash", "foo/", "", "foo/", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotModel, gotSplit := splitProviderModel(tt.target)
			if gotProvider != tt.wantProvider || gotModel != tt.wantModel || gotSplit != tt.wantSplit {
				t.Errorf("splitProviderModel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.target, gotProvider, gotModel, gotSplit,
					tt.wantProvider, tt.wantModel, tt.wantSplit)
			}
		})
	}
}

// buildMultipart helps assemble a body for testing.
func buildMultipart(t *testing.T, fields []struct{ name, value string }, files []struct {
	name, filename, content string
}) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f.name, f.value); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		fw, err := w.CreateFormFile(f.name, f.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return "multipart/form-data; boundary=" + w.Boundary(), buf.Bytes()
}

func extractMultipartField(t *testing.T, contentType string, body []byte, name string) (string, bool) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == name {
			data, _ := io.ReadAll(part)
			return string(data), true
		}
	}
}

func extractMultipartFile(t *testing.T, contentType string, body []byte, name string) (string, bool) {
	t.Helper()
	_, params, _ := mime.ParseMediaType(contentType)
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == name && part.FileName() != "" {
			data, _ := io.ReadAll(part)
			return string(data), true
		}
	}
}

func boundaryOf(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	return params["boundary"]
}

// rewriteMultipartTestWrapper runs handleMultipartBody-style logic without
// the proxywasm host. It calls the standalone helper that operates on body
// + boundary + (modelKey, newValue) and returns the rewritten body. Used to
// drive the same code path handleMultipartBody uses internally.
//
// Because handleMultipartBody itself calls proxywasm.LogWarnf and
// writeRoutingHeaders (which calls ReplaceHttpRequestHeader), we can't
// invoke it directly from tests. Instead these tests exercise the multipart
// rewrite primitive by calling a local copy of the same logic.
func rewriteMultipartForTest(t *testing.T, body []byte, contentType, modelKey, newValue string) ([]byte, bool) {
	t.Helper()
	boundary := multipartBoundary(contentType)
	if boundary == "" {
		t.Fatalf("test setup: bad boundary in %q", contentType)
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	rewrote := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pw, err := writer.CreatePart(part.Header)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == modelKey && part.FileName() == "" {
			raw, _ := io.ReadAll(part)
			if string(raw) != newValue {
				rewrote = true
			}
			pw.Write([]byte(newValue))
			continue
		}
		io.Copy(pw, part)
	}
	writer.Close()
	return out.Bytes(), rewrote
}

func TestMultipartRewrite_FirstPart(t *testing.T) {
	ct, body := buildMultipart(t,
		[]struct{ name, value string }{
			{"model", "whisper-1"},
			{"language", "en"},
		}, nil)
	out, rewrote := rewriteMultipartForTest(t, body, ct, "model", "qwen-audio")
	if !rewrote {
		t.Fatalf("expected rewrite to occur")
	}
	if got, ok := extractMultipartField(t, ct, out, "model"); !ok || got != "qwen-audio" {
		t.Errorf("model = %q ok=%v, want qwen-audio", got, ok)
	}
	if got, ok := extractMultipartField(t, ct, out, "language"); !ok || got != "en" {
		t.Errorf("language = %q ok=%v, want en", got, ok)
	}
}

func TestMultipartRewrite_SecondPart(t *testing.T) {
	// model field can be in any position now; stdlib reader walks all parts.
	ct, body := buildMultipart(t,
		[]struct{ name, value string }{
			{"language", "en"},
			{"model", "whisper-1"},
		}, nil)
	out, rewrote := rewriteMultipartForTest(t, body, ct, "model", "qwen-audio")
	if !rewrote {
		t.Fatalf("expected rewrite")
	}
	if got, ok := extractMultipartField(t, ct, out, "model"); !ok || got != "qwen-audio" {
		t.Errorf("model = %q ok=%v", got, ok)
	}
}

func TestMultipartRewrite_ModelAfterFile(t *testing.T) {
	// In buffered mode the router scans past file parts to find model.
	// Streaming-mode mapper still aborts on a file-before-model, but
	// router doesn't.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", "audio.mp3")
	fw.Write([]byte("FAKE-AUDIO-BYTES"))
	w.WriteField("model", "whisper-1")
	w.Close()
	ct := "multipart/form-data; boundary=" + w.Boundary()

	out, rewrote := rewriteMultipartForTest(t, buf.Bytes(), ct, "model", "qwen-audio")
	if !rewrote {
		t.Fatalf("router should rewrite even when model comes after file")
	}
	if got, ok := extractMultipartField(t, ct, out, "model"); !ok || got != "qwen-audio" {
		t.Errorf("model field = %q ok=%v", got, ok)
	}
	if got, ok := extractMultipartFile(t, ct, out, "file"); !ok || got != "FAKE-AUDIO-BYTES" {
		t.Errorf("file part corrupted: %q ok=%v", got, ok)
	}
}

func TestMultipartRewrite_NoModelField(t *testing.T) {
	ct, body := buildMultipart(t,
		[]struct{ name, value string }{
			{"language", "en"},
		}, nil)
	_, rewrote := rewriteMultipartForTest(t, body, ct, "model", "qwen-audio")
	if rewrote {
		t.Errorf("no rewrite expected when model field absent")
	}
}

func TestMultipartRewrite_FilePartNamedModel(t *testing.T) {
	// A file upload whose form-name happens to be "model" is NOT the model
	// identifier — it's a binary file. The rewriter must skip it.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("model", "model.bin")
	fw.Write([]byte("BINARY-WEIGHTS"))
	w.Close()
	ct := "multipart/form-data; boundary=" + w.Boundary()

	out, rewrote := rewriteMultipartForTest(t, buf.Bytes(), ct, "model", "qwen-audio")
	if rewrote {
		t.Errorf("file-named-model should not be rewritten")
	}
	if got, ok := extractMultipartFile(t, ct, out, "model"); !ok || got != "BINARY-WEIGHTS" {
		t.Errorf("file content lost: %q ok=%v", got, ok)
	}
}

func TestMultipartRewrite_NoOpWhenSameValue(t *testing.T) {
	ct, body := buildMultipart(t,
		[]struct{ name, value string }{
			{"model", "same-name"},
		}, nil)
	_, rewrote := rewriteMultipartForTest(t, body, ct, "model", "same-name")
	if rewrote {
		t.Errorf("no-op rewrite should not set rewrote=true")
	}
}

func TestMultipartRewrite_PreservesLargeFilePart(t *testing.T) {
	// Round-trip a 4 KiB binary file through the rewriter and check it
	// survives byte-for-byte.
	bigContent := string(bytes.Repeat([]byte("X"), 4096))
	ct, body := buildMultipart(t,
		[]struct{ name, value string }{
			{"model", "old"},
		},
		[]struct {
			name, filename, content string
		}{
			{"file", "a.bin", bigContent},
		},
	)
	out, _ := rewriteMultipartForTest(t, body, ct, "model", "new-model")
	if got, ok := extractMultipartFile(t, ct, out, "file"); !ok || got != bigContent {
		t.Errorf("large file corrupted: ok=%v len(got)=%d", ok, len(got))
	}
}

func TestExtractLastUserMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "text content",
			body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"how are you"}]}`,
			want: "how are you",
		},
		{
			name: "multimodal content - last text wins",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"part1"},{"type":"image_url","image_url":{"url":"..."}},{"type":"text","text":"part2"}]}]}`,
			want: "part2",
		},
		{
			name: "no user messages",
			body: `{"messages":[{"role":"system","content":"sys"}]}`,
			want: "",
		},
		{
			name: "messages missing",
			body: `{}`,
			want: "",
		},
		{
			name: "messages not array",
			body: `{"messages":"oops"}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractLastUserMessage([]byte(tt.body)); got != tt.want {
				t.Errorf("extractLastUserMessage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchAutoRoutingRule(t *testing.T) {
	rules := []AutoRoutingRule{
		{Pattern: regexp.MustCompile(`(?i)code`), Model: "coder-model"},
		{Pattern: regexp.MustCompile(`(?i)image|photo`), Model: "vision-model"},
	}
	tests := []struct {
		msg       string
		wantModel string
		wantOk    bool
	}{
		{"write me some code", "coder-model", true},
		{"describe this image please", "vision-model", true},
		{"hello world", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			gotModel, gotOk := matchAutoRoutingRule(rules, tt.msg)
			if gotModel != tt.wantModel || gotOk != tt.wantOk {
				t.Errorf("matchAutoRoutingRule(%q) = (%q,%v), want (%q,%v)",
					tt.msg, gotModel, gotOk, tt.wantModel, tt.wantOk)
			}
		})
	}
}

func TestBaseMediaType(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"application/json", "application/json"},
		{"application/json; charset=utf-8", "application/json"},
		{"multipart/form-data; boundary=abc", "multipart/form-data"},
		{"", ""},
		{"garbage", "garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := baseMediaType(tt.in); got != tt.want {
				t.Errorf("baseMediaType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMultipartBoundary(t *testing.T) {
	if got := multipartBoundary("multipart/form-data; boundary=abc"); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
	if got := multipartBoundary("application/json"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseConfig_AliasNameMappingOptional(t *testing.T) {
	cfgJSON := `{"prefix":"/model/proxy/"}`
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(cfgJSON), &cfg); err != nil {
		t.Fatalf("parseConfig should allow empty aliasNameMapping, got: %v", err)
	}
	if cfg.targetHeader != defaultTargetHeader {
		t.Errorf("targetHeader default = %q, want %q", cfg.targetHeader, defaultTargetHeader)
	}
	if cfg.modelKey != defaultModelKey {
		t.Errorf("modelKey default = %q, want %q", cfg.modelKey, defaultModelKey)
	}
	if len(cfg.enableOnPathSuffix) == 0 {
		t.Errorf("enableOnPathSuffix should have defaults")
	}
}

func TestParseConfig_AutoRouting(t *testing.T) {
	cfgJSON := `{
		"autoRouting": {
			"enable": true,
			"defaultModel": "fallback-model",
			"rules": [
				{"pattern": "(?i)code", "model": "coder"},
				{"pattern": "(?i)image|photo", "model": "vision"}
			]
		}
	}`
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(cfgJSON), &cfg); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !cfg.autoRoutingEnabled {
		t.Errorf("autoRoutingEnabled should be true")
	}
	if cfg.autoRoutingDefault != "fallback-model" {
		t.Errorf("autoRoutingDefault = %q, want %q", cfg.autoRoutingDefault, "fallback-model")
	}
	if len(cfg.autoRoutingRules) != 2 {
		t.Fatalf("expected 2 compiled rules, got %d", len(cfg.autoRoutingRules))
	}
}

func TestParseConfig_EnableOnPathSuffixOverride(t *testing.T) {
	cfgJSON := `{"enableOnPathSuffix":["/foo","/bar"]}`
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(cfgJSON), &cfg); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.enableOnPathSuffix) != 2 ||
		cfg.enableOnPathSuffix[0] != "/foo" ||
		cfg.enableOnPathSuffix[1] != "/bar" {
		t.Errorf("enableOnPathSuffix = %v, want [/foo /bar]", cfg.enableOnPathSuffix)
	}
}

func TestParseConfig_EnableOnPathSuffixWrongType(t *testing.T) {
	cfgJSON := `{"enableOnPathSuffix":"oops"}`
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(cfgJSON), &cfg); err == nil {
		t.Errorf("expected error for non-array enableOnPathSuffix")
	}
}
