package main

import "testing"

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
