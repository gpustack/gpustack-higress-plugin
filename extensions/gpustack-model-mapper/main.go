// gpustack-model-mapper is a fork of higress's wasm-go model-mapper
// (plugins/wasm-go/extensions/model-mapper) with two enhancements:
//
//  1. multipart/form-data support — the upstream plugin only handles
//     application/json bodies, which leaves /v1/audio/transcriptions and
//     /v1/images/edits broken when a route alias differs from the actual
//     deployed model name (gpustack/gpustack#4617).
//
//  2. maxBodyBytes is configurable — upstream hardcodes 100 MiB. Operators
//     can tighten or widen this to bound wasm memory per concurrent request.
//
// The JSON code path, config schema (`modelMapping`, `modelKey`,
// `modelToHeader`, `enableOnPathSuffix`), default `enableOnPathSuffix`
// values, alphabetical prefix-iteration ordering, and resolution priority
// (exact → first matching prefix → defaultModel → original) are preserved
// verbatim from higress model-mapper to keep this a true drop-in
// replacement. See diff comments inline where behavior was extended.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"sort"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName = "gpustack-model-mapper"

	// DefaultMaxBodyBytes matches higress model-mapper's hardcoded
	// constant. Operators can override via the `maxBodyBytes` config.
	DefaultMaxBodyBytes = 100 * 1024 * 1024 // 100 MiB

	mtJSON      = "application/json"
	mtMultipart = "multipart/form-data"

	ctxKeyContentType = "gpustack_mm_content_type"
)

// ModelMapping mirrors higress model-mapper. Prefix is the YAML key with
// the trailing "*" stripped.
type ModelMapping struct {
	Prefix string
	Target string
}

// Config has the same shape as higress model-mapper plus `maxBodyBytes`.
type Config struct {
	modelKey           string
	exactModelMapping  map[string]string
	prefixModelMapping []ModelMapping
	defaultModel       string
	enableOnPathSuffix []string
	modelToHeader      string
	// maxBodyBytes is the envoy decoder buffer limit. Bodies larger than
	// this get 413 before this plugin's body callback runs. Optional;
	// default 100 MiB (same as upstream higress model-mapper). Peak wasm
	// memory per concurrent request ≈ 2× maxBodyBytes (envoy decoder
	// buffer + wasm linear memory each hold one copy during rewrite).
	maxBodyBytes uint32
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.WithRebuildAfterRequests[Config](1000),
		wrapper.WithRebuildMaxMemBytes[Config](200*1024*1024),
	)
}

func parseConfig(j gjson.Result, config *Config) error {
	config.modelKey = j.Get("modelKey").String()
	if config.modelKey == "" {
		config.modelKey = "model"
	}

	config.modelToHeader = j.Get("modelToHeader").String()
	if config.modelToHeader == "" {
		config.modelToHeader = "x-higress-llm-model-final"
	}

	// gpustack enhancement: maxBodyBytes is configurable.
	if mbb := j.Get("maxBodyBytes"); mbb.Exists() {
		v := mbb.Int()
		if v <= 0 {
			return errors.New("maxBodyBytes must be a positive integer")
		}
		if v > int64(^uint32(0)) {
			v = int64(^uint32(0))
		}
		config.maxBodyBytes = uint32(v)
	} else {
		config.maxBodyBytes = DefaultMaxBodyBytes
	}

	modelMapping := j.Get("modelMapping")
	if modelMapping.Exists() && !modelMapping.IsObject() {
		return errors.New("modelMapping must be an object")
	}

	config.exactModelMapping = make(map[string]string)
	config.prefixModelMapping = make([]ModelMapping, 0)

	// Verbatim from higress: replicate C++ nlohmann::json's alphabetical
	// key iteration so the first-matching-prefix priority is
	// deterministic and matches upstream behavior.
	type mappingEntry struct{ key, value string }
	var entries []mappingEntry
	modelMapping.ForEach(func(key, value gjson.Result) bool {
		entries = append(entries, mappingEntry{
			key:   key.String(),
			value: value.String(),
		})
		return true
	})
	sort.Slice(entries, func(i, k int) bool {
		return entries[i].key < entries[k].key
	})

	for _, entry := range entries {
		switch {
		case entry.key == "*":
			config.defaultModel = entry.value
		case strings.HasSuffix(entry.key, "*"):
			config.prefixModelMapping = append(config.prefixModelMapping, ModelMapping{
				Prefix: strings.TrimSuffix(entry.key, "*"),
				Target: entry.value,
			})
		default:
			config.exactModelMapping[entry.key] = entry.value
		}
	}

	enableOnPathSuffix := j.Get("enableOnPathSuffix")
	if enableOnPathSuffix.Exists() {
		if !enableOnPathSuffix.IsArray() {
			return errors.New("enableOnPathSuffix must be an array")
		}
		for _, item := range enableOnPathSuffix.Array() {
			if s := item.String(); s != "" {
				config.enableOnPathSuffix = append(config.enableOnPathSuffix, s)
			}
		}
	} else {
		// Verbatim from higress, PLUS the multipart-form endpoints
		// (`/audio/transcriptions`, `/audio/translations`, `/images/edits`)
		// which the upstream plugin omits because its JSON-only body
		// handler can't process them anyway. We can.
		config.enableOnPathSuffix = []string{
			"/completions",
			"/embeddings",
			"/images/generations",
			"/images/edits",
			"/audio/speech",
			"/audio/transcriptions",
			"/audio/translations",
			"/fine_tuning/jobs",
			"/moderations",
			"/image-synthesis",
			"/video-synthesis",
			"/rerank",
			"/messages",
			"/responses",
		}
	}

	return nil
}

// resolveModel implements higress model-mapper's resolution order:
// exact match → first matching prefix (entries iterated in the order
// established by parseConfig's alphabetical sort) → defaultModel → original.
//
// Pure function (no proxywasm host calls) so tests can call it directly.
func resolveModel(config Config, oldModel string) string {
	newModel := config.defaultModel
	if newModel == "" {
		newModel = oldModel
	}
	if target, ok := config.exactModelMapping[oldModel]; ok {
		return target
	}
	for _, mapping := range config.prefixModelMapping {
		if strings.HasPrefix(oldModel, mapping.Prefix) {
			return mapping.Target
		}
	}
	return newModel
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}

	matched := false
	for _, suffix := range config.enableOnPathSuffix {
		if suffix == "*" || strings.HasSuffix(path, suffix) {
			matched = true
			break
		}
	}

	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	mediaType := baseMediaType(contentType)
	bodyCapable := (mediaType == mtJSON || mediaType == mtMultipart) && ctx.HasRequestBody()

	if !matched || !bodyCapable {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}

	// Verbatim from higress: disable re-route because this plugin only
	// rewrites body and the routing decision has already been made by a
	// preceding plugin (model-router / gpustack-generic-proxy-router).
	ctx.DisableReroute()
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(config.maxBodyBytes)
	ctx.SetContext(ctxKeyContentType, contentType)

	return types.HeaderStopIteration
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	if len(body) == 0 {
		return types.ActionContinue
	}
	contentType, _ := ctx.GetContext(ctxKeyContentType).(string)
	if baseMediaType(contentType) == mtMultipart {
		return handleMultipartBody(config, body, contentType)
	}
	return handleJSONBody(config, body)
}

// handleJSONBody is verbatim higress model-mapper's onHttpRequestBody
// (after the json.Valid check), with the host-call site routed through
// proxywasm directly instead of wasm-go's log package — same observable
// behavior, just avoiding an extra dependency.
func handleJSONBody(config Config, body []byte) types.Action {
	if !json.Valid(body) {
		proxywasm.LogError(pluginName + ": invalid json body")
		return types.ActionContinue
	}

	oldModel := gjson.GetBytes(body, config.modelKey).String()
	newModel := resolveModel(config, oldModel)

	// Header write is unconditional in higress upstream (even when
	// oldModel and newModel are empty). Preserved here for parity.
	_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, newModel)
	proxywasm.LogDebugf("%s: set header %s: %s", pluginName, config.modelToHeader, newModel)

	if newModel != "" && newModel != oldModel {
		newBody, err := sjson.SetBytes(body, config.modelKey, newModel)
		if err != nil {
			proxywasm.LogErrorf("%s: failed to update model: %v", pluginName, err)
			return types.ActionContinue
		}
		_ = proxywasm.ReplaceHttpRequestBody(newBody)
		proxywasm.LogInfof("%s: model mapped, before: %s, after: %s", pluginName, oldModel, newModel)
	}
	return types.ActionContinue
}

// handleMultipartBody is the gpustack-specific enhancement: the same
// resolution logic as handleJSONBody, but the model identifier is read
// from / written to the `modelKey` form-field of a multipart body.
//
// Because the body is fully buffered (ProcessRequestBody, not streaming),
// we can scan all parts in any order — file-before-model is fine. Files
// and any other non-target parts are passed through to the multipart
// writer unchanged.
func handleMultipartBody(config Config, body []byte, contentType string) types.Action {
	boundary := multipartBoundary(contentType)
	if boundary == "" {
		proxywasm.LogWarnf("%s: multipart boundary missing/unparseable; body unchanged", pluginName)
		return types.ActionContinue
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	out.Grow(len(body))
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		proxywasm.LogWarnf("%s: SetBoundary failed: %v; body unchanged", pluginName, err)
		return types.ActionContinue
	}

	// Initialise newModel with the "no model field" resolution result
	// (defaultModel if configured, otherwise ""). If the body has no
	// modelKey form-field we still write the header to this value —
	// matches the JSON path's behaviour (higress upstream parity).
	oldModel := ""
	newModel := resolveModel(config, oldModel)
	foundModel := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			proxywasm.LogWarnf("%s: multipart parse failed (%v); body unchanged", pluginName, err)
			return types.ActionContinue
		}
		pw, err := writer.CreatePart(part.Header)
		if err != nil {
			proxywasm.LogWarnf("%s: multipart create-part failed (%v); body unchanged", pluginName, err)
			return types.ActionContinue
		}

		// Only a non-file form-field whose name matches modelKey is the
		// mapping input. A binary file upload whose form-name happens to
		// be "model" must be treated as a file (passed through).
		if !foundModel && part.FormName() == config.modelKey && part.FileName() == "" {
			raw, err := io.ReadAll(part)
			if err != nil {
				proxywasm.LogWarnf("%s: read model part failed (%v); body unchanged", pluginName, err)
				return types.ActionContinue
			}
			oldModel = string(raw)
			newModel = resolveModel(config, oldModel)
			if newModel == "" {
				newModel = oldModel
			}
			if _, err := pw.Write([]byte(newModel)); err != nil {
				proxywasm.LogWarnf("%s: write model part failed (%v); body unchanged", pluginName, err)
				return types.ActionContinue
			}
			foundModel = true
			continue
		}
		if _, err := io.Copy(pw, part); err != nil {
			proxywasm.LogWarnf("%s: copy part failed (%v); body unchanged", pluginName, err)
			return types.ActionContinue
		}
	}
	if err := writer.Close(); err != nil {
		proxywasm.LogWarnf("%s: writer close failed (%v); body unchanged", pluginName, err)
		return types.ActionContinue
	}

	// Header write is unconditional after a successful full-body parse —
	// even when the body had no modelKey form-field. This matches the JSON
	// path (handleJSONBody) and higress upstream.
	_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, newModel)
	proxywasm.LogDebugf("%s: set header %s: %s", pluginName, config.modelToHeader, newModel)

	// Body rewrite only when we found a model part AND the resolution
	// produced a different value. Without a model part we don't inject one
	// — adding parts to a multipart body is more intrusive than the JSON
	// case where sjson harmlessly inserts a top-level field.
	if foundModel && newModel != oldModel {
		_ = proxywasm.ReplaceHttpRequestBody(out.Bytes())
		proxywasm.LogInfof("%s: model mapped, before: %s, after: %s", pluginName, oldModel, newModel)
	}
	return types.ActionContinue
}

func baseMediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}

func multipartBoundary(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["boundary"]
}
