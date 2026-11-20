package main

import (
	"net/url"
	"path"
	"strings"
)

// Snapshot preserves submitted parameters, not the engine's inferred defaults.
// Media bytes and credentials never enter the metadata control service.
func (r VideoRequest) Snapshot(model string) map[string]any {
	parameters := make(map[string]any, len(r.Values))
	for name, value := range r.Values {
		if credentialField(name) {
			continue
		}
		if files := r.Files[name]; len(files) > 0 {
			uploads := make([]map[string]any, 0, len(files))
			for _, file := range files {
				uploads = append(uploads, map[string]any{"filename": path.Base(strings.ReplaceAll(file.Filename, "\\", "/")), "content_type": file.ContentType, "size": len(file.Data)})
			}
			parameters[name] = map[string]any{"uploads": uploads}
			continue
		}
		if name == "prompt" {
			parameters[name] = value
			continue
		}
		parameters[name] = snapshotParameter(name, value)
	}
	parameters["model"] = model
	return parameters
}

func snapshotParameter(name string, value any) any {
	switch name {
	case "extra_params", "extra_body", "extra_json", "media_inputs", "image_reference", "video_reference", "audio_reference", "conditions", "target", "sampling_params":
		if text, ok := value.(string); ok {
			var decoded any
			if decodeJSON([]byte(text), &decoded) == nil {
				return snapshotValue(decoded)
			}
		}
	}
	return snapshotValue(value)
}

func credentialField(name string) bool {
	name = strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(name))
	switch name {
	case "authorization", "proxyauthorization", "cookie", "apikey", "apitoken", "apitokens", "token", "accesstoken", "refreshtoken", "password", "secret", "secretkey", "accesskey", "accesskeyid", "secretaccesskey", "signature", "credential", "credentials", "environment", "env", "filepath", "filepaths", "storagepath", "storagekey", "outputpath":
		return true
	}
	return false
}

func snapshotValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for name, item := range v {
			if !credentialField(name) {
				if name == "prompt" {
					result[name] = item
				} else {
					result[name] = snapshotValue(item)
				}
			}
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = snapshotValue(item)
		}
		return result
	case string:
		trimmed := strings.TrimSpace(v)
		if strings.HasPrefix(strings.ToLower(trimmed), "http://") || strings.HasPrefix(strings.ToLower(trimmed), "https://") {
			parsed, err := url.Parse(trimmed)
			if err != nil {
				return "[reference URL omitted]"
			}
			parsed.User, parsed.RawQuery, parsed.Fragment, parsed.RawFragment = nil, "", "", ""
			parsed.ForceQuery = false
			return parsed.String()
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
			return "[inline media omitted]"
		}
	}
	return value
}
