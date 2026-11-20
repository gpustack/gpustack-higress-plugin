// Package routing defines the async video path and trusted header contract.
package routing

import (
	"net/url"
	"regexp"
	"strings"
)

const BootstrapHeader = "x-gpustack-video-bootstrap"
const ClusterHeader = "x-gpustack-video-cluster"
const RelayHeader = "x-gpustack-video-delegation"
const RelayState = "gpustack_video_delegation"

var PublicID = regexp.MustCompile(`^video_[0-9a-f]{32}$`)
var legacyID = regexp.MustCompile(`^video_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

func Operation(method, rawPath string) (string, string) {
	path, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.TrimSuffix(path.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "" || parts[1] != "v1" || parts[2] != "videos" {
		return "", ""
	}
	if method == "POST" && len(parts) == 3 {
		return "create", ""
	}
	if (method != "GET" && method != "DELETE") || len(parts) < 4 || len(parts[3]) > 4096 || !(PublicID.MatchString(parts[3]) || legacyID.MatchString(parts[3])) {
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

// CanonicalPath makes async video aliases enter the same protected task routes.
// Synchronous video keeps its existing model-proxy path and filters.
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
		return rawPath, "", false
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
