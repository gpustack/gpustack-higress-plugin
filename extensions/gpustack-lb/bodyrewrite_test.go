package main

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// These cover the **pure planners**. rewriteJSON / rewriteMultipart themselves
// make host ABI calls, which panic outside a wasm host; the part walk and the
// JSON edit are split out precisely so the fiddly half can be tested.
//
// This is the code the design calls the most tedious in the whole plugin, and
// it is shared by both roles -- the context role's modelMapping resolution and
// the finisher's "always the chosen candidate" resolver differ only in the
// callback passed in.

func toModel(target string) resolver {
	return func(string) string { return target }
}

// -- JSON.

func TestPlanJSONRewrite(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		modelKey string
		resolve  resolver
		wantOld  string
		wantNew  string
		wantBody string // "" means the body must be left alone
	}{
		{
			name: "rewrites the model field", body: `{"model":"gpt","stream":true}`,
			modelKey: "model", resolve: toModel("qwen3-32b"),
			wantOld: "gpt", wantNew: "qwen3-32b",
			wantBody: `{"model":"qwen3-32b","stream":true}`,
		},
		{
			name: "same value is not a rewrite", body: `{"model":"gpt"}`,
			modelKey: "model", resolve: toModel("gpt"),
			wantOld: "gpt", wantNew: "gpt",
		},
		{
			// A by-pass candidate carries no modelName; the body must survive
			// byte-for-byte.
			name: "empty target is a by-pass", body: `{"model":"gpt"}`,
			modelKey: "model", resolve: toModel(""),
			wantOld: "gpt", wantNew: "",
		},
		{
			// The finisher's resolver ignores the old value, so a body with no
			// model field still gets one.
			name: "adds the field when absent", body: `{"stream":true}`,
			modelKey: "model", resolve: toModel("qwen3-32b"),
			wantOld: "", wantNew: "qwen3-32b",
			wantBody: `{"stream":true,"model":"qwen3-32b"}`,
		},
		{
			name: "honours a custom modelKey", body: `{"the_model":"gpt"}`,
			modelKey: "the_model", resolve: toModel("qwen3-32b"),
			wantOld: "gpt", wantNew: "qwen3-32b",
			wantBody: `{"the_model":"qwen3-32b"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := planJSONRewrite(tt.modelKey, []byte(tt.body), tt.resolve)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.oldModel != tt.wantOld || res.newModel != tt.wantNew {
				t.Errorf("old/new = %q/%q, want %q/%q", res.oldModel, res.newModel, tt.wantOld, tt.wantNew)
			}
			// The header is written from the resolved value even when the body
			// is untouched -- upstream model-mapper's behaviour.
			if !res.setHeadr || res.header != tt.wantNew {
				t.Errorf("header = %q (set=%v), want %q", res.header, res.setHeadr, tt.wantNew)
			}
			if tt.wantBody == "" {
				if res.newBody != nil {
					t.Errorf("body should be left alone, got %s", res.newBody)
				}
				return
			}
			if string(res.newBody) != tt.wantBody {
				t.Errorf("body = %s, want %s", res.newBody, tt.wantBody)
			}
		})
	}
}

func TestPlanJSONRewriteRejectsNonJSON(t *testing.T) {
	if _, err := planJSONRewrite("model", []byte("not json at all"), toModel("x")); err == nil {
		t.Error("a non-JSON body must return an error so the caller passes it through")
	}
}

// -- Multipart.

type part struct {
	name     string
	fileName string // non-empty makes it a file part
	content  string
}

func buildMultipart(t *testing.T, parts []part) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var (
			fw  interface{ Write([]byte) (int, error) }
			err error
		)
		if p.fileName != "" {
			fw, err = w.CreateFormFile(p.name, p.fileName)
		} else {
			fw, err = w.CreateFormField(p.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(p.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func readParts(t *testing.T, body []byte, contentType string) []part {
	t.Helper()
	r := multipart.NewReader(bytes.NewReader(body), multipartBoundary(contentType))
	form, err := r.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("result is not parseable multipart: %v", err)
	}
	var out []part
	for name, vals := range form.Value {
		for _, v := range vals {
			out = append(out, part{name: name, content: v})
		}
	}
	for name, fhs := range form.File {
		for _, fh := range fhs {
			out = append(out, part{name: name, fileName: fh.Filename})
		}
	}
	return out
}

func findPart(parts []part, name string) (part, bool) {
	for _, p := range parts {
		if p.name == name {
			return p, true
		}
	}
	return part{}, false
}

// The body is fully buffered, so parts can be scanned in any order -- a file
// arriving before the model field is the normal shape for
// /v1/audio/transcriptions, and it must still be rewritten.
func TestPlanMultipartRewriteFileBeforeModel(t *testing.T) {
	body, ct := buildMultipart(t, []part{
		{name: "file", fileName: "audio.mp3", content: "BINARY-AUDIO-DATA"},
		{name: "model", content: "whisper"},
		{name: "language", content: "en"},
	})

	res, err := planMultipartRewrite("model", ct, body, toModel("whisper-large-v3"))
	if err != nil {
		t.Fatal(err)
	}
	if res.oldModel != "whisper" || res.newModel != "whisper-large-v3" {
		t.Fatalf("old/new = %q/%q", res.oldModel, res.newModel)
	}
	if res.newBody == nil {
		t.Fatal("expected a rewritten body")
	}

	got := readParts(t, res.newBody, ct)
	if m, ok := findPart(got, "model"); !ok || m.content != "whisper-large-v3" {
		t.Errorf("model part = %+v, want whisper-large-v3", m)
	}
	// Every other part has to survive untouched, file included.
	if f, ok := findPart(got, "file"); !ok || f.fileName != "audio.mp3" {
		t.Errorf("file part lost or renamed: %+v", f)
	}
	if l, ok := findPart(got, "language"); !ok || l.content != "en" {
		t.Errorf("language part = %+v, want en", l)
	}
	if !bytes.Contains(res.newBody, []byte("BINARY-AUDIO-DATA")) {
		t.Error("file content did not survive the rewrite")
	}
	// The boundary must be preserved, or the content-type header would no
	// longer describe the body.
	if b := multipartBoundary(ct); !bytes.Contains(res.newBody, []byte(b)) {
		t.Errorf("boundary %q missing from the rewritten body", b)
	}
}

// With no model form part there is nothing to rewrite, and a part must not be
// invented -- adding one to a multipart body is far more intrusive than adding
// a field to JSON. The header is still set.
func TestPlanMultipartRewriteNoModelPart(t *testing.T) {
	body, ct := buildMultipart(t, []part{
		{name: "file", fileName: "audio.mp3", content: "BINARY"},
		{name: "language", content: "en"},
	})

	res, err := planMultipartRewrite("model", ct, body, toModel("whisper-large-v3"))
	if err != nil {
		t.Fatal(err)
	}
	if res.newBody != nil {
		t.Errorf("no model part means no body rewrite, got %s", res.newBody)
	}
	if !res.setHeadr || res.header != "whisper-large-v3" {
		t.Errorf("header = %q (set=%v), want the resolved value anyway", res.header, res.setHeadr)
	}
}

// A binary upload that happens to be named "model" is a file, not the model
// field, and must pass through untouched.
func TestPlanMultipartRewriteIgnoresFileNamedModel(t *testing.T) {
	body, ct := buildMultipart(t, []part{
		{name: "model", fileName: "model.bin", content: "WEIGHTS"},
	})

	res, err := planMultipartRewrite("model", ct, body, toModel("qwen3-32b"))
	if err != nil {
		t.Fatal(err)
	}
	if res.newBody != nil {
		t.Errorf("a file part named model must not be rewritten, got %s", res.newBody)
	}
	if res.oldModel != "" {
		t.Errorf("oldModel = %q, want empty (no form field was found)", res.oldModel)
	}
}

// Only the first model form field is taken, so a second one cannot quietly
// override the decision.
func TestPlanMultipartRewriteUsesFirstModelField(t *testing.T) {
	body, ct := buildMultipart(t, []part{
		{name: "model", content: "first"},
		{name: "model", content: "second"},
	})

	captured := []string{}
	res, err := planMultipartRewrite("model", ct, body, func(old string) string {
		captured = append(captured, old)
		return "resolved"
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.oldModel != "first" {
		t.Errorf("oldModel = %q, want first", res.oldModel)
	}
	// Resolved once for the "no model field" default, then once for the first
	// field -- never for the second.
	for _, c := range captured {
		if c == "second" {
			t.Error("the second model field must not be resolved")
		}
	}
}

// An unparseable boundary means the body is passed through, never mangled.
func TestPlanMultipartRewriteRejectsBadBoundary(t *testing.T) {
	body, _ := buildMultipart(t, []part{{name: "model", content: "gpt"}})
	for _, ct := range []string{"multipart/form-data", "", "not a media type"} {
		if _, err := planMultipartRewrite("model", ct, body, toModel("x")); err == nil {
			t.Errorf("content-type %q should fail rather than rewrite", ct)
		}
	}
}

// -- Media type helpers, shared by the buffering decision in both roles.

func TestBaseMediaType(t *testing.T) {
	tests := map[string]string{
		"application/json":                  mtJSON,
		"application/json; charset=utf-8":   mtJSON,
		"APPLICATION/JSON":                  mtJSON,
		"multipart/form-data; boundary=xyz": mtMultipart,
		// A prefix match would call this JSON; parsing does not.
		"application/jsonx": "application/jsonx",
	}
	for in, want := range tests {
		if got := baseMediaType(in); got != want {
			t.Errorf("baseMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A by-pass on the finisher side means modelName is empty, and rewriteBody is
// never reached. This pins the dispatch itself: multipart goes to the multipart
// planner, everything else to the JSON one.
func TestRewriteDispatchByMediaType(t *testing.T) {
	if baseMediaType("multipart/form-data; boundary=xyz") != mtMultipart {
		t.Error("multipart must dispatch to the multipart planner")
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", ""} {
		if baseMediaType(ct) == mtMultipart {
			t.Errorf("%q must not dispatch to multipart", ct)
		}
	}
}

// gjson/sjson round trip: the rewritten body has to stay parseable, including
// when the model value needs escaping.
func TestPlanJSONRewriteKeepsBodyValid(t *testing.T) {
	body := `{"model":"gpt","messages":[{"role":"user","content":"hi \"there\""}]}`
	res, err := planJSONRewrite("model", []byte(body), toModel(`we"ird`))
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(res.newBody) {
		t.Fatalf("rewritten body is not valid JSON: %s", res.newBody)
	}
	if got := gjson.GetBytes(res.newBody, "model").String(); got != `we"ird` {
		t.Errorf("model = %q, want %q", got, `we"ird`)
	}
	if got := gjson.GetBytes(res.newBody, "messages.0.content").String(); !strings.Contains(got, `"there"`) {
		t.Errorf("unrelated content was damaged: %q", got)
	}
}
