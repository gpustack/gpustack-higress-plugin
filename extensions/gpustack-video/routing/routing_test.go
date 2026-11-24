package routing

import "testing"

func TestVideoOperationsUseNativeIDs(t *testing.T) {
	for _, test := range []struct{ method, path, op, id string }{
		{"POST", "/v1/videos", "create", ""},
		{"GET", "/v1/videos/native-id", "status", "native-id"},
		{"GET", "/v1/videos/123_abc/content?variant=video", "content", "123_abc"},
		{"DELETE", "/v1/videos/native-id", "delete", "native-id"},
		{"GET", "/v1/videos", "", ""},
		{"POST", "/v1/videos/sync", "sync", ""},
		{"GET", "/v1/videos/sync", "", ""},
		{"DELETE", "/v1/videos/sync", "", ""},
		{"DELETE", "/v1/videos/native-id/content", "", ""},
		{"GET", "/v1/videos/id.with.dots", "", ""},
	} {
		if op, id := Operation(test.method, test.path); op != test.op || id != test.id {
			t.Errorf("%s %s returned %q %q", test.method, test.path, op, id)
		}
	}
}

func TestModelProxyAliasesKeepTheSameNativeOperation(t *testing.T) {
	path, model, ok := CanonicalPath("/MODEL/PROXY/9/V1/VIDEOS/native-id/CONTENT?variant=video", "/model/proxy/", map[string]string{"9": "org/wan"})
	if !ok || model != "org/wan" || path != "/v1/videos/native-id/content?variant=video" {
		t.Fatalf("unexpected canonical request: %q, %q, %v", path, model, ok)
	}
}
