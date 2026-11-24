// Package routing defines video operations and their model routing contract.
package routing

import (
	"net/url"
	"regexp"
	"strings"
)

var nativeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func Operation(method, rawPath string) (string, string) {
	path, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.TrimSuffix(path.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "" || parts[1] != "v1" || parts[2] != "videos" {
		return "", ""
	}
	if method == "POST" {
		if len(parts) == 3 {
			return "create", ""
		}
		if len(parts) == 4 && parts[3] == "sync" {
			return "sync", ""
		}
	}
	if (method != "GET" && method != "DELETE") || len(parts) < 4 || parts[3] == "sync" || !nativeID.MatchString(parts[3]) {
		return "", ""
	}
	if len(parts) == 4 {
		if method == "DELETE" {
			return "delete", parts[3]
		}
		return "status", parts[3]
	}
	if method == "GET" && len(parts) == 5 && parts[4] == "content" {
		return "content", parts[3]
	}
	return "", ""
}

// CanonicalPath sends model-proxy aliases through the same video routes.
func CanonicalPath(rawPath, prefix string, aliases map[string]string) (string, string, bool) {
	parsed, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return rawPath, "", false
	}
	candidate := parsed.Path
	model := ""
	if strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(prefix)) {
		rest := candidate[len(prefix):]
		if strings.HasPrefix(strings.ToLower(rest), "v1/videos") {
			candidate = "/" + rest
		} else {
			id, tail, ok := strings.Cut(rest, "/")
			if !ok {
				return rawPath, "", false
			}
			candidate = "/" + tail
			model = aliases[id]
		}
	}
	parts := strings.Split(strings.TrimSuffix(candidate, "/"), "/")
	if len(parts) < 3 || parts[0] != "" || !strings.EqualFold(parts[1], "v1") || !strings.EqualFold(parts[2], "videos") {
		return rawPath, "", false
	}
	if len(parts) == 4 && strings.EqualFold(parts[3], "sync") {
		parts[3] = "sync"
	}
	parts[1] = "v1"
	parts[2] = "videos"
	if len(parts) == 5 && strings.EqualFold(parts[4], "content") {
		parts[4] = "content"
	}
	parsed.Path = strings.Join(parts, "/")
	parsed.RawPath = ""
	return parsed.String(), model, true
}
