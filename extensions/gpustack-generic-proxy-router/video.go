package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	wire "github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Self-routing job IDs.
//
// An asynchronous job's native ID only exists on the instance (or behind the
// provider) that created it, and the OpenAI SDK's follow-up calls carry
// nothing but that ID: no model, no custom header. So the gateway makes the
// ID self-describing when the create response passes through --
//
//	gpustack_<route-id>-<instance-id>_<native-id>     an instance target
//	gpustack_<route-id>-provider-<id>_<native-id>     a provider target
//
// Underscore separates the three fields; hyphen joins the parts inside the
// target field. The route id is what names the model in GPUStack -- the same
// key `/model/proxy/{id}` resolves through aliasNameMapping -- and what
// follows it names the target within that route, in the spelling of the
// GPUStack cluster: `model-<model>-<instance>` / `provider-<id>`. What a
// cluster name means as a target is targetPatterns (wire.TargetPatternsKey),
// read by this plugin and gpustack-lb alike.
//
// On the way back in, videoRegexes extracts the two halves, aliasNameMapping
// turns the route id into the model name the route match and ext-auth need,
// the path is rewritten to the native ID for the upstream, and the target
// goes to gpustack-lb's finisher (wire.FilterStateVideoPin) to be routed to
// the one instance that can answer. Nothing is registered anywhere: the ID
// is the state.
const (
	videoIDPrefix    = "gpustack_"
	videoIDDivider   = "_"
	videoIDTargetSep = "-"

	// videoCreatePath is the OpenAI-compatible create endpoint. The create
	// request is an ordinary body-driven model request (the model is in the
	// body), so it has no regex -- only the response rewrite needs to know
	// it is a create. Exact match, so /v1/videos/sync (binary body) is not
	// mistaken for it.
	videoCreatePath = "/v1/videos"

	// videoResponseBodyLimit caps what is buffered of a video object
	// response (create / status / delete). A video object is a few hundred
	// bytes; the cap only exists so a mislabelled response cannot make the
	// plugin buffer a video.
	videoResponseBodyLimit = 16 * 1024 * 1024 // 16 MiB

	ctxKeyVideoID            = "gpustack_grp_video_id"
	ctxKeyVideoCreate        = "gpustack_grp_video_create"
	ctxKeyVideoRewritePrefix = "gpustack_grp_video_rewrite_prefix"

	// videoOriginalPathHeader carries the client's original path across an
	// internal-redirect re-entry. Filter state is recreated per pass and the
	// live :path is by then the upstream-facing one (the self-routing prefix
	// stripped, possibly a provider base path prepended by ai-proxy), so
	// neither the pin nor "this was a create" can be re-derived from it.
	// Request headers ARE replayed by Envoy on a redirect, which is the
	// same property gpustack-lb's tried-clusters header relies on. The value
	// is only ever fed back through videoRegexes + aliasNameMapping, never
	// trusted as routing input on its own.
	videoOriginalPathHeader = "x-gpustack-video-path"
)

// VideoRegex is one videoRegexes entry: a request-path pattern whose named
// groups are the two halves of a self-routing ID, and the upstream path
// template.
type VideoRegex struct {
	Pattern *regexp.Regexp
	Path    string
}

// videoID is the parsed self-routing ID.
type videoID struct {
	Route  string
	Target string
	Origin string
}

// prefix is what the native ID is wrapped with.
func (v videoID) prefix() string {
	return videoIDPrefix + v.Route + videoIDTargetSep + v.Target + videoIDDivider
}

var (
	// the capture groups every videoRegexes pattern must name.
	videoGroups = []string{"target", "origin"}
	// the placeholders a path template may use.
	videoPlaceholder = regexp.MustCompile(`\{([a-z]+)\}`)

	videoLeadingDigits = regexp.MustCompile(`^[0-9]+`)
)

// splitVideoTarget splits the target half of an ID into the route id and the
// target within the route (`7`, or `provider-5`): the route is what precedes
// the first hyphen. Neither half's shape is checked here -- what a target
// looks like is targetPatterns' business (wire.TargetPatternsKey), and on a
// pin the join is a whole-string comparison against the candidates, so an
// unknown one is a miss, never a different instance.
func splitVideoTarget(target string) (route, within string, ok bool) {
	route, within, ok = strings.Cut(target, videoIDTargetSep)
	if !ok || route == "" || within == "" {
		return "", "", false
	}
	return route, within, true
}

func (c *PluginConfig) videoEnabled() bool { return len(c.videoRegexes) > 0 }

// parseVideoConfig reads videoRegexes and videoIdRewriteOnRouteNamePrefix.
// Both empty disables the whole video branch; a partial configuration is an
// error rather than a silent no-op, because the failure would otherwise be
// a client holding an ID that cannot be polled, with nothing in the log to
// say why. The route-id -> model-name table is aliasNameMapping, shared with
// path-driven mode, so a video route has to carry it.
func parseVideoConfig(j gjson.Result, config *PluginConfig) error {
	rules := j.Get("videoRegexes")
	if rules.Exists() && !rules.IsArray() {
		return errors.New("videoRegexes must be an array")
	}
	for _, rule := range rules.Array() {
		patternStr := rule.Get("regex").String()
		path := rule.Get("path").String()
		compiled, err := regexp.Compile(patternStr)
		if err != nil {
			return fmt.Errorf("videoRegexes: invalid regex %q: %w", patternStr, err)
		}
		names := map[string]bool{}
		for _, n := range compiled.SubexpNames() {
			names[n] = true
		}
		for _, g := range videoGroups {
			if !names[g] {
				return fmt.Errorf("videoRegexes: regex %q must name a capture group (?P<%s>...)", patternStr, g)
			}
		}
		if path == "" {
			return fmt.Errorf("videoRegexes: regex %q has an empty path", patternStr)
		}
		for _, m := range videoPlaceholder.FindAllStringSubmatch(path, -1) {
			known := false
			for _, g := range videoGroups {
				if m[1] == g {
					known = true
				}
			}
			if !known {
				return fmt.Errorf("videoRegexes: path %q uses unknown placeholder {%s}", path, m[1])
			}
		}
		config.videoRegexes = append(config.videoRegexes, VideoRegex{Pattern: compiled, Path: path})
	}

	prefixes := j.Get("videoIdRewriteOnRouteNamePrefix")
	if prefixes.Exists() && !prefixes.IsArray() {
		return errors.New("videoIdRewriteOnRouteNamePrefix must be an array")
	}
	for _, p := range prefixes.Array() {
		if s := p.String(); s != "" {
			config.videoRouteNamePrefixes = append(config.videoRouteNamePrefixes, s)
		}
	}

	switch {
	case len(config.videoRegexes) > 0 && len(config.videoRouteNamePrefixes) == 0:
		return errors.New("videoIdRewriteOnRouteNamePrefix is required when videoRegexes is set")
	case len(config.videoRegexes) == 0 && len(config.videoRouteNamePrefixes) > 0:
		return errors.New("videoIdRewriteOnRouteNamePrefix requires videoRegexes")
	}
	// aliasNameMapping is deliberately NOT required: it starts out empty on
	// a fresh gateway (it is filled per route), and a follow-up whose route
	// is not mapped is already answered 404 at request time.
	return nil
}

// matchVideoPath returns the parsed ID and the rendered upstream path for the
// first videoRegexes entry that matches. Only GET and DELETE follow-ups are
// routed here: a POST to such a path is not a job operation this plugin
// understands, and it is left exactly as the client sent it.
func matchVideoPath(regexes []VideoRegex, method, path string) (videoID, string, bool) {
	if method != "GET" && method != "DELETE" {
		return videoID{}, "", false
	}
	for _, r := range regexes {
		m := r.Pattern.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		captures := map[string]string{}
		for i, name := range r.Pattern.SubexpNames() {
			captures[name] = m[i]
		}
		route, target, ok := splitVideoTarget(captures["target"])
		if !ok {
			continue
		}
		v := videoID{Route: route, Target: target, Origin: captures["origin"]}
		return v, strings.NewReplacer("{target}", captures["target"], "{origin}", v.Origin).Replace(r.Path), true
	}
	return videoID{}, "", false
}

// routeVideo handles a self-routing follow-up: resolve the model for the
// route match, rewrite the path to the native ID, and publish the pin. The
// second return is false when the request is not one.
func routeVideo(ctx wrapper.HttpContext, config PluginConfig, method, cleanPath, rawPath string) (types.Action, bool) {
	// /model/proxy/{id}/v1/videos/... carries the same ID behind the
	// path-driven alias prefix. The ID is self-describing, so the alias is
	// dropped rather than resolved: one regex set serves both spellings.
	matchPath, viaAlias := stripVideoProxyAlias(cleanPath, config.prefix)
	v, rendered, ok := matchVideoPath(config.videoRegexes, method, matchPath)
	if !ok {
		return types.ActionContinue, false
	}
	proxywasm.LogInfof("%s: video follow-up route=%s target=%s (viaAlias=%v)", pluginName, v.Route, v.Target, viaAlias)

	model := config.aliasNameMapping[v.Route]
	if model == "" {
		// Without the model the request cannot be matched to a model route
		// at all (the route is gone, or the ID was never ours). Answer here
		// rather than let it fall to the catch-all.
		return respondLocal(ctx, 404, "unknown video route"), true
	}
	writeRoutingHeaders(config, model)
	// Remember the client's spelling -- query included, so a follow-up
	// that is retried on a redirect re-entry asks for the same thing --
	// for the re-entry handled by videoReentry.
	_ = proxywasm.ReplaceHttpRequestHeader(videoOriginalPathHeader, rawPath)

	upstreamPath := rendered
	if idx := strings.Index(rawPath, "?"); idx != -1 {
		upstreamPath += rawPath[idx:]
	}
	if err := proxywasm.ReplaceHttpRequestHeader(":path", upstreamPath); err != nil {
		proxywasm.LogWarnf("%s: failed to rewrite :path for video follow-up: %v", pluginName, err)
	}

	publishVideoPin(wire.VideoPin{Target: v.Target, Mode: wire.PinModeJob})
	ctx.SetContext(ctxKeyVideoID, v)
	ctx.DontReadRequestBody()
	return types.ActionContinue, true
}

// routePinnedProxy handles the path-driven form
// /model/proxy/{route-id}-{target}/..., with which a client names the
// instance for an ordinary request ({route-id}-provider-{id} for a
// provider). The caller has already split the alias into route and target
// (resolveAlias). The route half selects the model exactly like a plain
// alias does -- the path outranks the body's `model` -- and the target half
// is left for gpustack-lb as a PinModeSelect pin: the instance is honoured,
// but the request still goes through the in-flight reserve, so naming an
// instance is not a way around maxRunningRequests.
//
// False when the route is not in aliasNameMapping, in which case the caller
// applies the ordinary alias handling.
func routePinnedProxy(ctx wrapper.HttpContext, config PluginConfig, route, target, cleanPath, canonicalPath, rawPath string) (types.Action, bool) {
	upstream := pinnedProxyPath(cleanPath, canonicalPath, config.prefix)
	if _, ok := pinProxy(ctx, config, route, target, upstream, true, rawPath); !ok {
		return types.ActionContinue, false
	}
	ctx.DontReadRequestBody()
	return types.ActionContinue, true
}

// pinProxy is what a client-selected request needs written on every pass,
// the first one and each redirect re-entry alike: the routing headers for
// the route's model, the select pin for gpustack-lb, and the original-path
// marker the re-entry reads back. False when the route is not in
// aliasNameMapping.
//
// The :path is rewritten only on the first pass. On a re-entry the request
// is replayed as the earlier filters left it -- ai-proxy may have turned
// /v1/messages into /v1/chat/completions together with the body -- and
// putting the client's path back would make it convert an already
// converted body a second time. The first pass's canonical path is still
// in the replayed headers.
func pinProxy(ctx wrapper.HttpContext, config PluginConfig, route, target, upstreamPath string, rewritePath bool, rawPath string) (string, bool) {
	model := config.aliasNameMapping[route]
	if model == "" {
		return "", false
	}
	proxywasm.LogInfof("%s: pinned proxy route=%s target=%s", pluginName, route, target)
	writeRoutingHeaders(config, model)

	if rewritePath {
		if idx := strings.Index(rawPath, "?"); idx != -1 {
			upstreamPath += rawPath[idx:]
		}
		if err := proxywasm.ReplaceHttpRequestHeader(":path", upstreamPath); err != nil {
			proxywasm.LogWarnf("%s: failed to rewrite :path for pinned proxy: %v", pluginName, err)
		}
	}
	publishVideoPin(wire.VideoPin{Target: target, Mode: wire.PinModeSelect})
	// The original path is left for videoReentry: the pin lives in filter
	// state, which does not survive an internal redirect, and without it
	// the retry would be re-selected onto another instance.
	_ = proxywasm.ReplaceHttpRequestHeader(videoOriginalPathHeader, rawPath)
	return model, true
}

// pinnedProxyPath is the upstream path of a pinned-proxy request: the alias
// stripped, unless an earlier step already normalised the path -- a create
// loses its trailing slash there (/model/proxy/2-7/v1/videos/?a=1 ->
// /v1/videos) -- in which case that result is kept. Rebuilding it from the
// raw path would bring the slash back and miss every /videos suffix match
// downstream.
func pinnedProxyPath(cleanPath, canonicalPath, prefix string) string {
	if canonicalPath != cleanPath {
		return canonicalPath
	}
	return stripAliasPath(cleanPath, prefix)
}

// stripAliasPath drops the alias prefix and the one segment after it,
// leaving the canonical path: /model/proxy/2-7/v1/x -> /v1/x. A path with
// nothing after the alias yields "/".
func stripAliasPath(path, prefix string) string {
	_, tail, _ := strings.Cut(path[len(prefix):], "/")
	return "/" + tail
}

func publishVideoPin(pin wire.VideoPin) {
	data, err := json.Marshal(pin)
	if err != nil {
		return
	}
	if err := proxywasm.SetProperty([]string{wire.FilterStateVideoPin}, data); err != nil {
		proxywasm.LogWarnf("%s: publish video pin failed: %v", pluginName, err)
	}
}

func isVideoCreate(method, cleanPath string) bool {
	return method == "POST" && strings.TrimSuffix(cleanPath, "/") == videoCreatePath
}

// normalizeVideoCreatePath maps a create request's path onto the canonical
// /v1/videos: the path-driven alias prefix and one segment are dropped and a
// trailing slash removed (/model/proxy/2/v1/videos/?x -> /v1/videos). False
// when the result is not a create.
func normalizeVideoCreatePath(cleanPath, prefix string) (string, bool) {
	matchPath, _ := stripVideoProxyAlias(cleanPath, prefix)
	matchPath = strings.TrimSuffix(matchPath, "/")
	if matchPath != videoCreatePath {
		return cleanPath, false
	}
	return matchPath, true
}

// stripVideoProxyAlias removes the path-driven alias prefix and the one
// segment after it (/model/proxy/99/v1/videos/x -> /v1/videos/x). False when
// the path does not start with the prefix or has no segment after it, in
// which case the caller matches the path as it is.
func stripVideoProxyAlias(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return path, false
	}
	id, tail, ok := strings.Cut(path[len(prefix):], "/")
	if !ok || id == "" {
		return path, false
	}
	return "/" + tail, true
}

// videoReentry handles a redirect re-entry of a request that carries the
// original-path marker the first pass left. The live :path cannot be used
// for this: it has been rewritten (native ID, alias stripped), possibly
// prefixed by ai-proxy, which is exactly how a create would slip past the
// replay guard and how a pin would be lost.
//
//   - a video create is answered 502: it must never be submitted twice;
//   - a video follow-up is routed again from the original path, which
//     restores the model header, the native path and the pin, so the retry
//     reaches the owner instead of a re-selected instance. Both need the
//     video branch to be enabled;
//   - a client-selected request (/model/proxy/{route}-{target}/...) gets
//     its select pin restored whatever the video config: it does not
//     depend on it. The replayed :path is left alone -- see pinProxy. It
//     then returns "not handled" on purpose, so the caller's generic
//     re-entry handling still runs -- in particular the length-only body
//     pass that re-measures a body the first pass rewrote, which
//     gpustack-lb's length-based groups need.
//
// False when the request is none of these.
func videoReentry(ctx wrapper.HttpContext, config PluginConfig, method string) (types.Action, bool) {
	original, err := proxywasm.GetHttpRequestHeader(videoOriginalPathHeader)
	if err != nil || original == "" {
		return types.ActionContinue, false
	}
	original = strings.TrimSpace(original)
	clean := original
	if idx := strings.Index(original, "?"); idx != -1 {
		clean = original[:idx]
	}
	if config.videoEnabled() {
		if matchPath, _ := stripVideoProxyAlias(clean, config.prefix); isVideoCreate(method, matchPath) {
			return blockVideoReplay(ctx), true
		}
		if action, handled := routeVideo(ctx, config, method, clean, original); handled {
			proxywasm.LogInfof("%s: video follow-up re-entry, pin restored", pluginName)
			return action, true
		}
	}
	if id := extractID(clean, config.prefix); id != "" {
		route, target, matched := config.resolveAlias(id)
		if matched && target != "" {
			if _, ok := pinProxy(ctx, config, route, target, "", false, original); ok {
				proxywasm.LogInfof("%s: pinned proxy re-entry, pin restored", pluginName)
			}
		}
	}
	return types.ActionContinue, false
}

// armVideoCreate marks a create request so the response phase knows to wrap
// the ID, and takes the request out of every replay path: an async create
// that reached the engine may already have produced a job, and a second
// attempt -- Envoy retry, or an internal-redirect re-selection -- would
// submit it twice. The response of a lost attempt cannot be recovered, so
// the honest outcome is one failure, not two jobs.
//
// originalPath is the path BEFORE any normalisation, query included: it is
// what videoReentry reads back on a redirect pass.
func armVideoCreate(ctx wrapper.HttpContext, originalPath string) {
	ctx.SetContext(ctxKeyVideoCreate, true)
	_ = proxywasm.ReplaceHttpRequestHeader(videoOriginalPathHeader, originalPath)
	_ = proxywasm.ReplaceHttpRequestHeader("x-envoy-max-retries", "0")
	for _, h := range []string{"x-envoy-retry-on", "x-envoy-retry-grpc-on", "x-envoy-hedge-on-per-try-timeout"} {
		_ = proxywasm.RemoveHttpRequestHeader(h)
	}
}

// blockVideoReplay answers the re-entry pass of a create. Runs before the
// generic fallback handling, which would otherwise let the request through
// to a re-selection.
func blockVideoReplay(ctx wrapper.HttpContext) types.Action {
	proxywasm.LogWarnf("%s: refusing to replay a video create on a fallback pass", pluginName)
	return respondLocal(ctx, 502, "video creation failed upstream and was not retried, to avoid creating the job twice")
}

// respondLocal answers the request from the gateway. Same delivery rules as
// gpustack-lb's rejections (learned on this filter chain): DisableReroute
// first, ActionPause after, non-empty JSON body.
func respondLocal(ctx wrapper.HttpContext, status uint32, message string) types.Action {
	ctx.DisableReroute()
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "video_gateway_error", "code": status,
	}})
	if err := proxywasm.SendHttpResponse(status, [][2]string{{"content-type", "application/json"}}, body, -1); err != nil {
		proxywasm.LogErrorf("%s: SendHttpResponse failed: %v", pluginName, err)
	}
	return types.ActionPause
}

// onHttpResponseHeaders decides whether the response body must be buffered
// to wrap the video ID. Every condition has to hold; anything else returns
// before buffering, so non-video responses cost nothing.
func onHttpResponseHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	if !config.videoEnabled() {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	_, isCreate := ctx.GetContext(ctxKeyVideoCreate).(bool)
	v, isFollowUp := ctx.GetContext(ctxKeyVideoID).(videoID)
	if !isCreate && !isFollowUp {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	if !wrapper.IsResponseFromUpstream() {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	status, _ := proxywasm.GetHttpResponseHeader(":status")
	contentType, _ := proxywasm.GetHttpResponseHeader("content-type")
	if !is2xx(status) || baseMediaType(contentType) != mtJSON {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	// Only a route of ours is rewritten: the route name is Envoy's, not the
	// client's, and gpustack names the routes it renders.
	routeName, err := proxywasm.GetProperty([]string{"route_name"})
	routeID, ours := routeIDFromName(string(routeName), config.videoRouteNamePrefixes)
	if err != nil || !ours {
		proxywasm.LogInfof("%s: video response left as-is: route %q is not one of ours", pluginName, string(routeName))
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}

	prefix := ""
	if isFollowUp {
		// The request carried the ID, so both halves are known.
		prefix = v.prefix()
	} else {
		// A create: the ID is native. The route is the one that served the
		// request; the target is whichever of its instances (or provider)
		// the route picked.
		cluster, _ := proxywasm.GetProperty([]string{"cluster_name"})
		prefix = config.videoCreatePrefix(routeID, string(cluster))
	}
	if prefix == "" {
		ctx.DontReadResponseBody()
		return types.ActionContinue
	}
	// Envoy keeps the upstream's Content-Length when a wasm filter swaps the
	// body (measured on a live gateway: the full wrapped body went out,
	// bytes_sent=105, under content-length: 90, so every client truncated it
	// mid-JSON). The header has to go in the RESPONSE-HEADER phase, before
	// this callback returns and Envoy forwards the headers: by the body
	// phase it is already on the wire, and both replacing and removing it
	// there were verified to have no effect. Without it Envoy frames the
	// response itself. A response that turns out to have no `id` is then
	// forwarded unchanged, merely chunked.
	_ = proxywasm.RemoveHttpResponseHeader("content-length")
	ctx.SetContext(ctxKeyVideoRewritePrefix, prefix)
	ctx.BufferResponseBody()
	ctx.SetResponseBodyBufferLimit(videoResponseBodyLimit)
	return types.ActionContinue
}

func onHttpResponseBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	prefix, ok := ctx.GetContext(ctxKeyVideoRewritePrefix).(string)
	if !ok || prefix == "" {
		return types.ActionContinue
	}
	out, rewritten := wrapVideoID(body, prefix)
	if !rewritten {
		return types.ActionContinue
	}
	if err := proxywasm.ReplaceHttpResponseBody(out); err != nil {
		proxywasm.LogWarnf("%s: ReplaceHttpResponseBody failed: %v", pluginName, err)
		return types.ActionContinue
	}
	proxywasm.LogInfof("%s: video id wrapped with prefix %s", pluginName, prefix)
	return types.ActionContinue
}

// wrapVideoID replaces the top-level string `id` of a JSON body with the
// wrapped form. Only the top-level `id`: an error envelope, a body without
// one, or a non-string value is left untouched.
func wrapVideoID(body []byte, prefix string) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, false
	}
	id := gjson.GetBytes(body, "id")
	if id.Type != gjson.String || id.String() == "" {
		return nil, false
	}
	out, err := sjson.SetBytes(body, "id", prefix+id.String())
	if err != nil {
		return nil, false
	}
	return out, true
}

// routeIDFromName reports whether the route is one of ours and, if so, its
// id: the namespace qualifier ("gpustack-system/") is dropped, the name must
// start with one of the configured prefixes ("ai-route-route-"), and the id
// is the digits that follow ("ai-route-route-12.internal" -> "12"). Anything
// else is not ours, or not an id.
func routeIDFromName(routeName string, prefixes []string) (string, bool) {
	if idx := strings.LastIndexByte(routeName, '/'); idx != -1 {
		routeName = routeName[idx+1:]
	}
	for _, p := range prefixes {
		if !strings.HasPrefix(routeName, p) {
			continue
		}
		// Keep looking after a prefix hit without a leading id: with
		// overlapping prefixes ("ai-route-" and "ai-route-route-") the
		// later one may be the one that yields the id.
		if id := videoLeadingDigits.FindString(routeName[len(p):]); id != "" {
			return id, true
		}
	}
	return "", false
}

// videoCreatePrefix derives the wrap prefix of a create response from the
// route id and the cluster that served it. The target is what targetPatterns
// make of the cluster's service name -- by default `model-<model>-<instance>`
// yields the instance id and `provider-<id>` is kept whole, which is what
// gpustack-lb matches a pin against when a follow-up comes back. A cluster
// no rule knows yields "" and leaves the response untouched.
func (c *PluginConfig) videoCreatePrefix(routeID, cluster string) string {
	if routeID == "" {
		return ""
	}
	name := cluster
	if idx := strings.LastIndexByte(cluster, '|'); idx != -1 {
		name = cluster[idx+1:]
	}
	// The first dot, not the last: the resolution suffix may be several
	// labels (`static`, `dns`) and may be followed by a search domain.
	if idx := strings.IndexByte(name, '.'); idx != -1 {
		name = name[:idx]
	}
	target := c.targetRules.TargetOf(name)
	if target == "" {
		return ""
	}
	return (videoID{Route: routeID, Target: target}).prefix()
}

func is2xx(status string) bool {
	return len(status) == 3 && status[0] == '2'
}
