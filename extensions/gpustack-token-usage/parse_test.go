package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"testing"
)

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

func TestParseConsumerHeader(t *testing.T) {
	cases := []struct {
		input     string
		wantUID   *int64
		wantAK    *string
	}{
		{"", nil, nil},
		{"none", nil, nil},
		{"None", nil, nil},
		{"NONE", nil, nil},
		{"gpustack-42", int64Ptr(42), nil},
		{"mykey.gpustack-42", int64Ptr(42), strPtr("mykey")},
		{"a.b.gpustack-42", int64Ptr(42), strPtr("a.b")},
		{".gpustack-42", int64Ptr(42), nil},
		{"mykey", nil, strPtr("mykey")},
		{"gpustack-abc", nil, nil}, // non-numeric id → both nil (invalid, won't be generated)
		{"gpustack-", nil, nil},    // empty id → both nil (invalid, won't be generated)
	}

	for _, c := range cases {
		uid, ak := parseConsumerHeader(c.input)

		if (uid == nil) != (c.wantUID == nil) || (uid != nil && *uid != *c.wantUID) {
			t.Errorf("parseConsumerHeader(%q) userID = %v, want %v", c.input, uid, c.wantUID)
		}
		if (ak == nil) != (c.wantAK == nil) || (ak != nil && *ak != *c.wantAK) {
			t.Errorf("parseConsumerHeader(%q) accessKey = %v, want %v", c.input, ak, c.wantAK)
		}
	}
}

func TestParseClusterName(t *testing.T) {
	cases := []struct {
		input          string
		wantModelID    *int64
		wantProviderID *int64
	}{
		// Envoy outbound format
		{"outbound|80||model-1-2.static", int64Ptr(1), nil},
		{"outbound|80||model-99-0.static", int64Ptr(99), nil},
		{"outbound|80||model-1-2.dns", int64Ptr(1), nil},
		{"outbound|80||provider-5.static", nil, int64Ptr(5)},
		{"outbound|80||provider-100.dns", nil, int64Ptr(100)},
		// Bare names (fallback, no pipe-separated prefix)
		{"model-1-2.static", int64Ptr(1), nil},
		{"provider-5.static", nil, int64Ptr(5)},
		// Invalid / unrelated
		{"", nil, nil},
		{"outbound|80||other-service.static", nil, nil},
		{"outbound|80||model-abc-2.static", nil, nil},  // non-numeric model id
		{"outbound|80||model-1.static", nil, nil},      // missing instance-id
		{"outbound|80||provider-5-x.static", nil, nil}, // extra dash
		{"outbound|80||provider-abc.static", nil, nil}, // non-numeric provider id
	}

	for _, c := range cases {
		mid, pid := parseClusterName(c.input)

		if (mid == nil) != (c.wantModelID == nil) || (mid != nil && *mid != *c.wantModelID) {
			t.Errorf("parseRouteName(%q) modelID = %v, want %v", c.input, mid, c.wantModelID)
		}
		if (pid == nil) != (c.wantProviderID == nil) || (pid != nil && *pid != *c.wantProviderID) {
			t.Errorf("parseRouteName(%q) providerID = %v, want %v", c.input, pid, c.wantProviderID)
		}
	}
}

func buildMultipartBody(t *testing.T, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, value := range fields {
		fw, err := w.CreateFormField(name)
		if err != nil {
			t.Fatalf("CreateFormField(%q): %v", name, err)
		}
		fmt.Fprint(fw, value)
	}
	w.Close()
	return buf.Bytes(), fmt.Sprintf("multipart/form-data; boundary=%s", w.Boundary())
}

func TestExtractModelFromMultipart(t *testing.T) {
	cases := []struct {
		name        string
		buildBody   func(t *testing.T) ([]byte, string)
		wantModel   string
	}{
		{
			name: "model field present",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": "gpt-4o", "input": "hello"})
			},
			wantModel: "gpt-4o",
		},
		{
			name: "model field with surrounding whitespace",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": "  whisper-1  "})
			},
			wantModel: "whisper-1",
		},
		{
			name: "model field absent",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"input": "hello"})
			},
			wantModel: "",
		},
		{
			name: "model field empty",
			buildBody: func(t *testing.T) ([]byte, string) {
				return buildMultipartBody(t, map[string]string{"model": ""})
			},
			wantModel: "",
		},
		{
			name: "invalid content-type (no boundary)",
			buildBody: func(t *testing.T) ([]byte, string) {
				body, _ := buildMultipartBody(t, map[string]string{"model": "m"})
				return body, "multipart/form-data"
			},
			wantModel: "",
		},
		{
			name: "body mismatches boundary",
			buildBody: func(t *testing.T) ([]byte, string) {
				return []byte("--wrongboundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt-4\r\n--wrongboundary--"), "multipart/form-data; boundary=correctboundary"
			},
			wantModel: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, ct := c.buildBody(t)
			got := extractModelFromMultipart(body, ct)
			if got != c.wantModel {
				t.Errorf("got %q, want %q", got, c.wantModel)
			}
		})
	}
}
