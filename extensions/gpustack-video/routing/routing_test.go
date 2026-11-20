package routing

import (
	"strings"
	"testing"
)

func TestTaskPathsDoNotMatchCollectionsOrOtherVideoOperations(t *testing.T) {
	id := "video_" + strings.Repeat("a", 32)
	tests := []struct{ method, path, operation string }{
		{"POST", "/v1/videos", "create"}, {"GET", "/v1/videos/" + id, "status"},
		{"GET", "/v1/videos/" + id + "/content?variant=video", "content"},
		{"GET", "/v1/videos", ""}, {"DELETE", "/v1/videos/" + id, "delete"},
		{"DELETE", "/v1/videos/" + id + "/content", ""},
		{"GET", "/v1/videos/native-id", ""}, {"POST", "/v1/videos/sync", ""},
		{"GET", "/v1/videos/" + id + "/../content", ""},
	}
	for _, test := range tests {
		op, _ := Operation(test.method, test.path)
		if op != test.operation {
			t.Errorf("%s %s => %s", test.method, test.path, op)
		}
	}
}

func TestModelProxyAliasesCannotBypassTaskBinding(t *testing.T) {
	id := "video_" + strings.Repeat("a", 32)
	cases := []struct {
		path, want, model string
		video             bool
	}{
		{"/model/proxy/9/v1/videos", "/v1/videos", "published-wan", true},
		{"/model/proxy/9/v1/videos/" + id + "/content?variant=video", "/v1/videos/" + id + "/content?variant=video", "published-wan", true},
		{"/model/proxy/v1/videos", "/v1/videos", "", true},
		{"/MODEL/PROXY/9/V1/VIDEOS/", "/v1/videos", "published-wan", true},
		{"/v1/videos/", "/v1/videos", "", true},
		{"/model/proxy/9/v1/videos/native-id", "/v1/videos/native-id", "published-wan", true},
		{"/V1/VIDEOS", "/v1/videos", "", true},
		{"/V1/VIDEOS/" + id + "/CONTENT", "/v1/videos/" + id + "/content", "", true},
		{"/model/proxy/9/v1/videos/sync", "/model/proxy/9/v1/videos/sync", "", false},
		{"/model/proxy/9/v1/chat/completions", "/model/proxy/9/v1/chat/completions", "", false},
	}
	for _, c := range cases {
		path, model, video := CanonicalPath(c.path, "/model/proxy/", map[string]string{"9": "published-wan"})
		if path != c.want || model != c.model || video != c.video {
			t.Errorf("%s => %q %q %v", c.path, path, model, video)
		}
	}
}
