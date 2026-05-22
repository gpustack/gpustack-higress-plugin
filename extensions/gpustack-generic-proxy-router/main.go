package main

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"regexp"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName          = "gpustack-generic-proxy-router"
	defaultPrefix       = "/model/proxy/"
	defaultTargetHeader = "x-higress-llm-model"
	defaultModelKey     = "model"
	autoModelPrefix     = "higress/auto"
	defaultMaxBodyBytes = 100 * 1024 * 1024 // 100 MiB

	mtJSON      = "application/json"
	mtMultipart = "multipart/form-data"

	// Only body-driven mode runs through the body callback. Path-driven
	// is pure header projection and finishes in onHttpRequestHeaders;
	// body rewrite for the path-driven case is intentionally delegated to
	// gpustack-model-mapper (single-responsibility, mirrors higress's
	// router/mapper split).
	ctxKeyBodyDriven  = "gpustack_grp_body_driven"
	ctxKeyContentType = "gpustack_grp_content_type"
)

// AutoRoutingRule defines a regex-based routing rule for `model: higress/auto`
// (mirrors higress model-router's rule shape).
type AutoRoutingRule struct {
	Pattern *regexp.Regexp
	Model   string
}

type PluginConfig struct {
	prefix             string
	targetHeader       string
	modelKey           string
	addProviderHeader  string
	modelToHeader      string
	enableOnPathSuffix []string
	aliasNameMapping   map[string]string
	autoRoutingEnabled bool
	autoRoutingDefault string
	autoRoutingRules   []AutoRoutingRule
	// maxBodyBytes caps how many bytes envoy will buffer of the request body
	// before returning 413. The body is buffered (not streamed) because the
	// wasm-go wrapper's streaming path resumes header iteration after each
	// chunk's ActionContinue (plugin_wrapper.go:1031), which breaks
	// route re-matching against `x-higress-llm-model`. Pick this value to
	// bound the worst-case memory footprint per concurrent request (envoy
	// keeps one copy in its buffer, the wasm linear memory keeps another
	// when we read it out — so peak ≈ 2× maxBodyBytes per request).
	maxBodyBytes uint32
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		// IMPORTANT: ProcessRequestBody (buffered), NOT
		// ProcessStreamingRequestBody. The wasm-go wrapper returns
		// ActionContinue per chunk in streaming mode, which resumes the
		// envoy header iteration after the first chunk — by the time we
		// finally write `x-higress-llm-model` in a later chunk callback,
		// envoy has already matched the route against the original
		// headers (no `x-higress-llm-model`), so the AI-route ingress
		// never picks the request up. The buffered variant returns
		// ActionPause for every non-final chunk and the callback's
		// return value only at end-of-stream, which is the contract
		// model-router (the plugin we replace) was designed for.
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.WithRebuildAfterRequests[PluginConfig](1000),
		wrapper.WithRebuildMaxMemBytes[PluginConfig](200*1024*1024),
	)
}

func parseConfig(json gjson.Result, config *PluginConfig) error {
	config.prefix = json.Get("prefix").String()
	if config.prefix == "" {
		config.prefix = defaultPrefix
	}
	if !strings.HasSuffix(config.prefix, "/") {
		config.prefix += "/"
	}

	config.targetHeader = json.Get("targetHeader").String()
	if config.targetHeader == "" {
		config.targetHeader = defaultTargetHeader
	}

	config.modelKey = json.Get("modelKey").String()
	if config.modelKey == "" {
		config.modelKey = defaultModelKey
	}

	config.addProviderHeader = json.Get("addProviderHeader").String()
	config.modelToHeader = json.Get("modelToHeader").String()

	// maxBodyBytes is the envoy decoder buffer limit. Default 100 MiB to
	// cover the largest realistic AI request (a 25 MiB Whisper audio upload
	// with ample headroom). The host call takes uint32 so the JSON value is
	// also treated as uint32 — anything over 4 GiB is silently clamped to
	// the uint32 max (operators with such requests have bigger problems).
	if mbb := json.Get("maxBodyBytes"); mbb.Exists() {
		v := mbb.Int()
		if v <= 0 {
			return errors.New("maxBodyBytes must be a positive integer")
		}
		if v > int64(^uint32(0)) {
			v = int64(^uint32(0))
		}
		config.maxBodyBytes = uint32(v)
	} else {
		config.maxBodyBytes = defaultMaxBodyBytes
	}

	enableOnPathSuffix := json.Get("enableOnPathSuffix")
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

	config.aliasNameMapping = make(map[string]string)
	for k, v := range json.Get("aliasNameMapping").Map() {
		if k == "" {
			continue
		}
		config.aliasNameMapping[k] = v.String()
	}

	if autoRouting := json.Get("autoRouting"); autoRouting.Exists() {
		config.autoRoutingEnabled = autoRouting.Get("enable").Bool()
		config.autoRoutingDefault = autoRouting.Get("defaultModel").String()
		for _, rule := range autoRouting.Get("rules").Array() {
			patternStr := rule.Get("pattern").String()
			model := rule.Get("model").String()
			if patternStr == "" || model == "" {
				proxywasm.LogWarnf("%s: skipping invalid autoRouting rule: pattern=%q model=%q",
					pluginName, patternStr, model)
				continue
			}
			compiled, err := regexp.Compile(patternStr)
			if err != nil {
				proxywasm.LogWarnf("%s: failed to compile autoRouting pattern %q: %v",
					pluginName, patternStr, err)
				continue
			}
			config.autoRoutingRules = append(config.autoRoutingRules, AutoRoutingRule{
				Pattern: compiled,
				Model:   model,
			})
		}
	}
	// NOTE: a "config loaded" log line would be ideal here, but LogInfof
	// requires a live wasm host (panics from `go test`). The same config
	// shape is emitted at the head of every onHttpRequestHeaders so the
	// first request gives the same visibility without breaking tests.
	return nil
}

// extractID returns the {id} segment of path after the configured prefix.
// Returns an empty string if path does not match the prefix or the id is empty.
func extractID(path, prefix string) string {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id, _, _ := strings.Cut(path[len(prefix):], "/")
	return id
}

// matchPathSuffix reports whether `path` (query already stripped) ends with
// any of the configured suffixes. The wildcard "*" matches everything.
func matchPathSuffix(path string, suffixes []string) bool {
	for _, s := range suffixes {
		if s == "*" || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// splitProviderModel returns (provider, model, ok) when target is "provider/model".
// Returns ("", target, false) otherwise.
func splitProviderModel(target string) (string, string, bool) {
	i := strings.Index(target, "/")
	if i <= 0 || i == len(target)-1 {
		return "", target, false
	}
	return target[:i], target[i+1:], true
}

func baseMediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}

// multipartBoundary returns the boundary parameter from a multipart
// content-type, or "" if it can't be parsed.
func multipartBoundary(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["boundary"]
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		proxywasm.LogWarnf("%s: failed to read :path header: %v — passing through", pluginName, err)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	cleanPath := path
	if idx := strings.Index(cleanPath, "?"); idx != -1 {
		cleanPath = cleanPath[:idx]
	}

	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	mediaType := baseMediaType(contentType)
	hasBody := ctx.HasRequestBody()
	bodyCapable := (mediaType == mtJSON || mediaType == mtMultipart) && hasBody

	proxywasm.LogInfof("%s: onRequestHeaders path=%q contentType=%q mediaType=%q hasBody=%v prefix=%q aliasCount=%d",
		pluginName, cleanPath, contentType, mediaType, hasBody, config.prefix, len(config.aliasNameMapping))

	// Path-driven branch (priority: a configured `/model/proxy/{id}/` URL is
	// the operator's strongest intent signal, outranks body's `model`).
	// On a hit we ONLY write the routing headers and return — body is not
	// read or modified. Rationale: body-level model rewrite is the
	// responsibility of gpustack-model-mapper, mirroring higress's
	// router/mapper split. Multi-model upstreams that need the body's
	// `model` field rewritten to the alias target should chain mapper
	// after this plugin.
	//
	// Miss (id present in URL but not in aliasNameMapping) deliberately
	// falls through to the body-driven branch below: the body's `model`
	// field is a valid backup, and falling through keeps requests flowing
	// while mapping config is being rolled out — the WARN log surfaces the
	// miss for operators to fix.
	if id := extractID(cleanPath, config.prefix); id != "" {
		if target := config.aliasNameMapping[id]; target != "" {
			proxywasm.LogInfof("%s: path-driven HIT id=%q → %q (headers only)", pluginName, id, target)
			writeRoutingHeaders(config, target)
			ctx.DontReadRequestBody()
			return types.ActionContinue
		}
		proxywasm.LogWarnf("%s: path-driven miss id=%q not in aliasNameMapping (size=%d); falling through to body-driven",
			pluginName, id, len(config.aliasNameMapping))
	}

	if !bodyCapable {
		proxywasm.LogInfof("%s: body-driven skip — body not capable (mediaType=%q hasBody=%v); passing through",
			pluginName, mediaType, hasBody)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if !matchPathSuffix(cleanPath, config.enableOnPathSuffix) {
		proxywasm.LogInfof("%s: body-driven skip — path %q does not match enableOnPathSuffix (%d entries); passing through",
			pluginName, cleanPath, len(config.enableOnPathSuffix))
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	armBodyProcessing(ctx, contentType, config.maxBodyBytes)
	proxywasm.LogInfof("%s: body-driven activated: mediaType=%q", pluginName, mediaType)
	return types.HeaderStopIteration
}

// armBodyProcessing primes ctx + headers for the buffered body callback.
// Only the body-driven branch calls this — path-driven hit short-circuits
// after writing routing headers and never enters the body callback.
//
// IMPORTANT: this plugin is a model-router replacement, so it must NOT call
// ctx.DisableReroute() — envoy needs to re-match the route AFTER we write
// `x-higress-llm-model` so the AI-route ingress (which matches on that
// header) can pick the right upstream cluster. wasm-go's DisableReroute()
// sets the proxy property `clear_route_cache=off`, which freezes envoy on
// whatever route was matched at initial header parsing (typically a default
// fallback route, since the routing header doesn't exist yet at that
// point). Setting the header without re-routing yields HTTP 500 / wrong
// upstream.
//
// model-mapper-style plugins that run AFTER routing (and only rewrite body
// for upstream visibility) SHOULD DisableReroute — see gpustack-model-mapper.
func armBodyProcessing(ctx wrapper.HttpContext, contentType string, maxBodyBytes uint32) {
	proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(maxBodyBytes)
	ctx.SetContext(ctxKeyBodyDriven, true)
	ctx.SetContext(ctxKeyContentType, contentType)
}

// writeRoutingHeaders writes targetHeader / addProviderHeader / modelToHeader
// based on `target` and returns the value that should be substituted into the
// request body (the post-split model half).
func writeRoutingHeaders(config PluginConfig, target string) string {
	if err := proxywasm.ReplaceHttpRequestHeader(config.targetHeader, target); err != nil {
		proxywasm.LogWarnf("%s: failed to set header %s: %v", pluginName, config.targetHeader, err)
	} else {
		proxywasm.LogInfof("%s: header set %s=%q", pluginName, config.targetHeader, target)
	}
	bodyModel := target
	if config.addProviderHeader != "" {
		if provider, model, split := splitProviderModel(target); split {
			if err := proxywasm.ReplaceHttpRequestHeader(config.addProviderHeader, provider); err != nil {
				proxywasm.LogWarnf("%s: failed to set provider header %s: %v", pluginName, config.addProviderHeader, err)
			} else {
				proxywasm.LogInfof("%s: header set %s=%q (split from %q)", pluginName, config.addProviderHeader, provider, target)
			}
			bodyModel = model
		}
	}
	if config.modelToHeader != "" {
		if err := proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, bodyModel); err != nil {
			proxywasm.LogWarnf("%s: failed to set header %s: %v", pluginName, config.modelToHeader, err)
		} else {
			proxywasm.LogInfof("%s: header set %s=%q", pluginName, config.modelToHeader, bodyModel)
		}
	}
	return bodyModel
}

// onHttpRequestBody is the buffered body callback. wasm-go invokes us once,
// at end-of-stream, with the complete request body. Only body-driven
// requests reach this point (path-driven hits short-circuit with
// DontReadRequestBody in the header phase). We dispatch by media type,
// read the body's `model` field, optionally rewrite it, and return
// ActionContinue — which resumes envoy's header iteration and triggers
// route re-matching against the `x-higress-llm-model` header we wrote.
func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	if armed, _ := ctx.GetContext(ctxKeyBodyDriven).(bool); !armed {
		// Body callback fired without header-phase arming — should not
		// happen, but bail safely.
		proxywasm.LogDebugf("%s: body callback fired without body-driven arming; len=%d", pluginName, len(body))
		return types.ActionContinue
	}
	contentType, _ := ctx.GetContext(ctxKeyContentType).(string)
	mediaType := baseMediaType(contentType)
	proxywasm.LogInfof("%s: body assembled: mediaType=%s len=%d modelKey=%q",
		pluginName, mediaType, len(body), config.modelKey)

	var out []byte
	if mediaType == mtMultipart {
		out = handleMultipartBody(config, body, contentType)
	} else {
		out = processBodyDrivenJSON(body, config)
	}
	if out != nil && !bytes.Equal(out, body) {
		if err := proxywasm.ReplaceHttpRequestBody(out); err != nil {
			proxywasm.LogWarnf("%s: ReplaceHttpRequestBody failed: %v", pluginName, err)
		}
	}
	return types.ActionContinue
}

// handleMultipartBody walks the multipart body via the stdlib
// mime/multipart reader. The buffered-body model means we have everything
// in memory, so we can scan past file parts and locate the `model`
// form-field regardless of where the client placed it. Non-target parts
// (including file uploads) are passed through to the writer unchanged.
//
// Body-driven only: the value found in the `model` form-field drives the
// header writes (and the optional provider/model split). Multipart never
// triggers autoRouting — no `messages` array to match against.
func handleMultipartBody(config PluginConfig, body []byte, contentType string) []byte {
	boundary := multipartBoundary(contentType)
	if boundary == "" {
		proxywasm.LogWarnf("%s: multipart boundary missing/unparseable; emitting body unchanged", pluginName)
		return body
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	out.Grow(len(body))
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		proxywasm.LogWarnf("%s: SetBoundary failed: %v; emitting body unchanged", pluginName, err)
		return body
	}

	rewrote := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			proxywasm.LogWarnf("%s: multipart parse failed (%v); emitting body unchanged", pluginName, err)
			return body
		}
		pw, err := writer.CreatePart(part.Header)
		if err != nil {
			proxywasm.LogWarnf("%s: multipart create-part failed (%v); emitting body unchanged", pluginName, err)
			return body
		}

		// Only the non-file part whose form name matches modelKey is the
		// routing input. A binary file upload that happens to be named
		// "model" must be treated as a file (passed through unchanged).
		if part.FormName() == config.modelKey && part.FileName() == "" {
			rawVal, err := io.ReadAll(part)
			if err != nil {
				proxywasm.LogWarnf("%s: read model part failed (%v); emitting body unchanged", pluginName, err)
				return body
			}
			original := string(rawVal)
			newValue := writeRoutingHeaders(config, original)
			if newValue == "" {
				newValue = original
			}
			if _, err := pw.Write([]byte(newValue)); err != nil {
				proxywasm.LogWarnf("%s: write model part failed (%v); emitting body unchanged", pluginName, err)
				return body
			}
			if newValue != original {
				rewrote = true
				proxywasm.LogInfof("%s: multipart field %q rewritten: %q → %q",
					pluginName, config.modelKey, original, newValue)
			} else {
				proxywasm.LogInfof("%s: multipart field %q found, no rewrite", pluginName, config.modelKey)
			}
			continue
		}

		if _, err := io.Copy(pw, part); err != nil {
			proxywasm.LogWarnf("%s: copy part failed (%v); emitting body unchanged", pluginName, err)
			return body
		}
	}
	if err := writer.Close(); err != nil {
		proxywasm.LogWarnf("%s: writer close failed (%v); emitting body unchanged", pluginName, err)
		return body
	}

	if !rewrote {
		// Nothing changed — return the original body so envoy doesn't see
		// a byte-level diff (e.g. header case canonicalisation by the
		// stdlib writer) when there was no semantic change.
		return body
	}
	return out.Bytes()
}

// processBodyDrivenJSON mirrors higress model-router's handleJsonBody but
// keeps the gpustack convention: targetHeader always carries the *unsplit*
// model value, addProviderHeader / modelToHeader carry the post-split halves
// when configured.
func processBodyDrivenJSON(body []byte, config PluginConfig) []byte {
	if len(body) == 0 {
		proxywasm.LogInfof("%s: body-driven JSON skipped — empty body", pluginName)
		return body
	}
	if !gjson.ValidBytes(body) {
		proxywasm.LogWarnf("%s: body-driven JSON skipped — invalid JSON (len=%d)", pluginName, len(body))
		return body
	}
	original := gjson.GetBytes(body, config.modelKey).String()
	if original == "" {
		proxywasm.LogInfof("%s: body-driven JSON skipped — modelKey %q missing or empty in body", pluginName, config.modelKey)
		return body
	}
	proxywasm.LogInfof("%s: body-driven JSON model read: %q (modelKey=%q)", pluginName, original, config.modelKey)

	resolved := original
	if config.autoRoutingEnabled && original == autoModelPrefix {
		matched := ""
		userMessage := extractLastUserMessage(body)
		if userMessage != "" {
			if m, ok := matchAutoRoutingRule(config.autoRoutingRules, userMessage); ok {
				matched = m
				proxywasm.LogInfof("%s: autoRouting matched rule, last user message=%q → %q",
					pluginName, userMessage, matched)
			}
		}
		if matched == "" {
			matched = config.autoRoutingDefault
			if matched != "" {
				proxywasm.LogInfof("%s: autoRouting fell back to defaultModel=%q", pluginName, matched)
			}
		}
		if matched == "" {
			proxywasm.LogWarnf("%s: autoRouting hit but no rule matched and no defaultModel configured", pluginName)
			return body
		}
		resolved = matched
	}

	bodyModel := writeRoutingHeaders(config, resolved)
	if bodyModel == original {
		proxywasm.LogInfof("%s: body-driven JSON no rewrite needed (resolved=%q == original)", pluginName, bodyModel)
		return body
	}
	out, err := sjson.SetBytes(body, config.modelKey, bodyModel)
	if err != nil {
		proxywasm.LogWarnf("%s: rewrite json body failed: %v", pluginName, err)
		return body
	}
	proxywasm.LogInfof("%s: body-driven JSON model rewritten: %q → %q", pluginName, original, bodyModel)
	return out
}

// extractLastUserMessage returns the text content of the last `role:"user"`
// message in `body.messages`. Multimodal contents (array of {type,text}) are
// flattened by picking the last `text` entry — mirrors model-router.
func extractLastUserMessage(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var last string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		if content.IsArray() {
			for _, item := range content.Array() {
				if item.Get("type").String() == "text" {
					last = item.Get("text").String()
				}
			}
		} else {
			last = content.String()
		}
	}
	return last
}

func matchAutoRoutingRule(rules []AutoRoutingRule, message string) (string, bool) {
	for _, rule := range rules {
		if rule.Pattern.MatchString(message) {
			return rule.Model, true
		}
	}
	return "", false
}

