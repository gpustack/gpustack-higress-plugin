package main

import (
	"regexp"
	"testing"

	"github.com/tidwall/gjson"
)

const (
	// "\\d" is written for the JSON layer: gjson unescapes it to the regex \d.
	testVideoRegex    = `^/v1/videos/gpustack_(?P<target>[^_]+)_(?P<origin>[^/]+)$`
	testVideoContent  = `^/v1/videos/gpustack_(?P<target>[^_]+)_(?P<origin>[^/]+)/content$`
	testVideoPrefixes = `"videoIdRewriteOnRouteNamePrefix":["ai-route-route-"]`
	testVideoAliases  = `"aliasNameMapping":{"2":"sora","3":"openai/veo"}`
	testVideoConfig   = `{"videoRegexes":[{"regex":"` + testVideoRegex + `","path":"/v1/videos/{origin}"},` +
		`{"regex":"` + testVideoContent + `","path":"/v1/videos/{origin}/content"}],` +
		testVideoAliases + `,` + testVideoPrefixes + `}`
)

func parseTestVideoConfig(t *testing.T) PluginConfig {
	t.Helper()
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(testVideoConfig), &cfg); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

func TestParseConfig_Video(t *testing.T) {
	cfg := parseTestVideoConfig(t)
	if !cfg.videoEnabled() || len(cfg.videoRegexes) != 2 {
		t.Fatalf("video not enabled properly: %v", cfg.videoRegexes)
	}
	if len(cfg.videoRouteNamePrefixes) != 1 || cfg.videoRouteNamePrefixes[0] != "ai-route-route-" {
		t.Errorf("videoRouteNamePrefixes = %v", cfg.videoRouteNamePrefixes)
	}
}

func TestParseConfig_VideoDisabledByDefault(t *testing.T) {
	var cfg PluginConfig
	if err := parseConfig(gjson.Parse(`{"prefix":"/model/proxy/"}`), &cfg); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.videoEnabled() {
		t.Error("video must be disabled without videoRegexes")
	}
}

func TestParseConfig_VideoErrors(t *testing.T) {
	rule := func(regex, path string) string {
		return `{"regex":"` + regex + `","path":"` + path + `"}`
	}
	tests := []struct {
		name string
		json string
	}{
		{"videoRegexes not an array", `{"videoRegexes":"x"}`},
		{"invalid regex", `{"videoRegexes":[` + rule(`(`, `/v1/videos/{origin}`) + `],` + testVideoAliases + `,` + testVideoPrefixes + `}`},
		{"missing origin group", `{"videoRegexes":[` + rule(`^/v1/videos/gpustack_(?P<target>[^_]+)$`, `/v1/videos/x`) + `],` + testVideoAliases + `,` + testVideoPrefixes + `}`},
		{"missing target group", `{"videoRegexes":[` + rule(`^/v1/videos/gpustack_([^_]+)_(?P<origin>[^/]+)$`, `/v1/videos/{origin}`) + `],` + testVideoAliases + `,` + testVideoPrefixes + `}`},
		{"empty path", `{"videoRegexes":[` + rule(testVideoRegex, ``) + `],` + testVideoAliases + `,` + testVideoPrefixes + `}`},
		{"unknown placeholder", `{"videoRegexes":[` + rule(testVideoRegex, `/v1/videos/{origin}/{route}`) + `],` + testVideoAliases + `,` + testVideoPrefixes + `}`},
		{"regexes without route prefix", `{"videoRegexes":[` + rule(testVideoRegex, `/v1/videos/{origin}`) + `],` + testVideoAliases + `}`},
		{"route prefix without regexes", `{` + testVideoPrefixes + `}`},
		{"route prefix not an array", `{"videoRegexes":[` + rule(testVideoRegex, `/v1/videos/{origin}`) + `],` + testVideoAliases + `,"videoIdRewriteOnRouteNamePrefix":"ai-route-route-"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg PluginConfig
			if err := parseConfig(gjson.Parse(tt.json), &cfg); err == nil {
				t.Errorf("expected error for %s, json=%s", tt.name, tt.json)
			}
		})
	}
}

// The ID's target half is split on the first hyphen and nothing more: what a
// valid target looks like is targetPatterns' business, and an unknown one is
// a pin miss downstream (its route is unmapped, or no candidate yields it).
func TestSplitVideoTarget(t *testing.T) {
	tests := []struct {
		target, route, within string
		ok                    bool
	}{
		{"2-7", "2", "7", true},
		{"2-provider-5", "2", "provider-5", true},
		{"2-7-x", "2", "7-x", true},
		{"sora-7", "sora", "7", true},
		// no hyphen, or an empty half
		{"2-", "2", "", false},
		{"2", "", "", false},
		{"-7", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		route, within, ok := splitVideoTarget(tt.target)
		if ok != tt.ok || (ok && (route != tt.route || within != tt.within)) {
			t.Errorf("splitVideoTarget(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.target, route, within, ok, tt.route, tt.within, tt.ok)
		}
	}
}

func TestMatchVideoPath(t *testing.T) {
	cfg := parseTestVideoConfig(t)
	tests := []struct {
		name       string
		method     string
		path       string
		wantOK     bool
		wantRoute  string
		wantTarget string
		wantOrigin string
		wantPath   string
	}{
		{"instance status", "GET", "/v1/videos/gpustack_2-7_video_abc-123", true, "2", "7", "video_abc-123", "/v1/videos/video_abc-123"},
		{"provider content", "GET", "/v1/videos/gpustack_2-provider-5_vid/content", true, "2", "provider-5", "vid", "/v1/videos/vid/content"},
		{"delete", "DELETE", "/v1/videos/gpustack_2-7_vid", true, "2", "7", "vid", "/v1/videos/vid"},
		{"content suffix is not swallowed", "GET", "/v1/videos/gpustack_2-7_vid/contents", false, "", "", "", ""},
		{"post is not routed", "POST", "/v1/videos/gpustack_2-7_vid", false, "", "", "", ""},
		{"remix is not routed", "POST", "/v1/videos/gpustack_2-7_vid/remix", false, "", "", "", ""},
		{"native id", "GET", "/v1/videos/gpustack-video_abc", false, "", "", "", ""},
		{"create", "POST", "/v1/videos", false, "", "", "", ""},
		{"sync", "POST", "/v1/videos/sync", false, "", "", "", ""},
		{"non-video", "GET", "/v1/chat/completions", false, "", "", "", ""},
		// The target half is not shape-checked any more: an unknown route
		// parses, and routeVideo answers it 404 (unknown video route).
		{"unknown route", "GET", "/v1/videos/gpustack_x-7_vid", true, "x", "7", "vid", "/v1/videos/vid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, path, ok := matchVideoPath(cfg.videoRegexes, tt.method, tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if v.Route != tt.wantRoute || v.Target != tt.wantTarget || v.Origin != tt.wantOrigin {
				t.Errorf("got %q/%q/%q, want %q/%q/%q", v.Route, v.Target, v.Origin, tt.wantRoute, tt.wantTarget, tt.wantOrigin)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

// The regex captures the native ID whole; it must come back verbatim when
// spliced into the upstream path, whatever characters the engine used --
// hyphens, dots, dollar signs among them.
func TestMatchVideoPath_DoesNotReinterpretOrigin(t *testing.T) {
	cfg := parseTestVideoConfig(t)
	v, path, ok := matchVideoPath(cfg.videoRegexes, "GET", "/v1/videos/gpustack_1-2_$1-a.b~c-x")
	if !ok || v.Origin != "$1-a.b~c-x" || path != "/v1/videos/$1-a.b~c-x" {
		t.Fatalf("origin = %q path = %q (ok=%v)", v.Origin, path, ok)
	}
}

func TestStripVideoProxyAlias(t *testing.T) {
	tests := []struct {
		path, want string
		ok         bool
	}{
		{"/model/proxy/99/v1/videos/gpustack_99-1_x", "/v1/videos/gpustack_99-1_x", true},
		{"/model/proxy/99/v1/videos/gpustack_99-1_x/content", "/v1/videos/gpustack_99-1_x/content", true},
		{"/v1/videos/gpustack_99-1_x", "/v1/videos/gpustack_99-1_x", false},
		{"/model/proxy/99", "/model/proxy/99", false},
		{"/model/proxy/", "/model/proxy/", false},
	}
	for _, tt := range tests {
		got, ok := stripVideoProxyAlias(tt.path, "/model/proxy/")
		if ok != tt.ok || got != tt.want {
			t.Errorf("stripVideoProxyAlias(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

func TestIsVideoCreate(t *testing.T) {
	tests := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/videos", true},
		{"POST", "/v1/videos/", true},
		{"POST", "/v1/videos/sync", false},
		{"GET", "/v1/videos", false},
		{"POST", "/v1/videos/gpustack_2-7_x", false},
	}
	for _, tt := range tests {
		if got := isVideoCreate(tt.method, tt.path); got != tt.want {
			t.Errorf("isVideoCreate(%s %s) = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}

// resolveAlias is what makes "{route}-{target}" explicit: without aliasRegex
// the whole segment is the alias, with it the regex decides -- including
// that a hyphenated alias stays one alias.
func TestResolveAlias(t *testing.T) {
	const recommended = `^(?P<route>[^/]+?)(?:-(?P<target>[0-9]+|provider-[0-9]+))?$`
	pinned := PluginConfig{aliasPattern: regexp.MustCompile(recommended)}
	plain := PluginConfig{}
	// an unanchored regex is anchored when compiled (parseTargetConfig);
	// a substring must never resolve
	loose := PluginConfig{aliasPattern: regexp.MustCompile(`^(?:(?P<route>[0-9]+)(?:-(?P<target>[0-9]+))?)?$`)}
	tests := []struct {
		name              string
		cfg               *PluginConfig
		id, route, target string
		ok                bool
	}{
		// no aliasRegex: exactly the previous behaviour, never a target
		{"plain alias", &plain, "qwen-alias", "qwen-alias", "", true},
		{"plain would-be pin", &plain, "2-7", "2-7", "", true},
		// aliasRegex
		{"alias", &pinned, "qwen-alias", "qwen-alias", "", true},
		{"instance", &pinned, "2-7", "2", "7", true},
		{"provider", &pinned, "2-provider-5", "2", "provider-5", true},
		{"hyphenated alias, instance", &pinned, "qwen-alias-7", "qwen-alias", "7", true},
		// the lazy route group absorbs a suffix that is not a target shape:
		// the whole segment is then an (unmapped) alias, as before
		{"not a target shape", &pinned, "2-blue", "2-blue", "", true},
		{"empty", &pinned, "", "", "", false},
		{"digits then extra text", &loose, "12-extra", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, target, ok := tt.cfg.resolveAlias(tt.id)
			if ok != tt.ok || (ok && (route != tt.route || target != tt.target)) {
				t.Errorf("resolveAlias(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.id, route, target, ok, tt.route, tt.target, tt.ok)
			}
		})
	}
}

// rule_matcher re-runs the parse on the same destination after a failure,
// with a backup JSON. Nothing of the rejected config may survive into it.
func TestParseConfigResetsDestination(t *testing.T) {
	var cfg PluginConfig
	full := `{"aliasRegex":"(?P<route>[0-9]+)","targetPatterns":[{"target":"^(\\d+)$","cluster":"model-\\d+-(\\d+)"}],` +
		testVideoAliases + `,"videoRegexes":[{"regex":"` + testVideoRegex + `","path":"/v1/videos/{origin}"}],` + testVideoPrefixes + `}`
	if err := parseConfig(gjson.Parse(full), &cfg); err != nil {
		t.Fatal(err)
	}
	// the rejected attempt: valid video settings, then an invalid rule
	bad := `{"aliasRegex":"(?P<route>[0-9]+)","targetPatterns":[{"target":"^\\d+$","cluster":"x"}],` + testVideoAliases + `,"videoRegexes":[{"regex":"` + testVideoRegex + `","path":"/v1/videos/{origin}"}],` + testVideoPrefixes + `}`
	if err := parseConfig(gjson.Parse(bad), &cfg); err == nil {
		t.Fatal("expected the invalid rule to fail")
	}
	// the retry with a backup that omits all of it
	if err := parseConfig(gjson.Parse(testVideoAliases), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.aliasPattern != nil {
		t.Error("aliasRegex of the rejected config survived")
	}
	if cfg.videoEnabled() || len(cfg.videoRegexes) != 0 || len(cfg.videoRouteNamePrefixes) != 0 {
		t.Errorf("video settings of the rejected config survived: %d regexes, %d prefixes", len(cfg.videoRegexes), len(cfg.videoRouteNamePrefixes))
	}
	if len(cfg.targetRules) == 0 || cfg.targetRules.TargetOf("model-42-7") != "7" {
		t.Error("targetPatterns of the rejected config survived instead of the defaults")
	}
}

func TestParseConfigAliasRegex(t *testing.T) {
	tests := []struct {
		name, json, want string
		wantErr          bool
	}{
		{"absent", `{}`, "", false},
		{"set", `{"aliasRegex":"(?P<route>[0-9]+)"}`, `^(?:(?P<route>[0-9]+))$`, false},
		{"anchored by the operator too", `{"aliasRegex":"^(?P<route>[0-9]+)$"}`, `^(?:^(?P<route>[0-9]+)$)$`, false},
		{"no route group", `{"aliasRegex":"(?P<target>[0-9]+)"}`, "", true},
		{"invalid regex", `{"aliasRegex":"("}`, "", true},
		{"target patterns not an array", `{"targetPatterns":"x"}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg PluginConfig
			err := parseConfig(gjson.Parse(tt.json), &cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConfig err = %v, wantErr %v", err, tt.wantErr)
			}
			got := ""
			if cfg.aliasPattern != nil {
				got = cfg.aliasPattern.String()
			}
			if got != tt.want {
				t.Fatalf("aliasRegex = %q, want %q", got, tt.want)
			}
			if !tt.wantErr && len(cfg.targetRules) == 0 {
				t.Fatal("target patterns not defaulted")
			}
		})
	}
}

func TestPinnedProxyPath(t *testing.T) {
	const prefix = "/model/proxy/"
	raw := "/model/proxy/2-7/v1/videos/"
	// what onHttpRequestHeaders computed for the same request
	canonical, ok := normalizeVideoCreatePath(raw, prefix)
	if !ok || canonical != "/v1/videos" {
		t.Fatalf("normalizeVideoCreatePath = %q, %v", canonical, ok)
	}
	tests := []struct{ clean, canonical, want string }{
		// an armed create keeps its normalised path
		{raw, canonical, "/v1/videos"},
		// anything else is stripped here
		{"/model/proxy/2-7/v1/chat/completions", "/model/proxy/2-7/v1/chat/completions", "/v1/chat/completions"},
	}
	for _, tt := range tests {
		if got := pinnedProxyPath(tt.clean, tt.canonical, prefix); got != tt.want {
			t.Errorf("pinnedProxyPath(%q, %q) = %q, want %q", tt.clean, tt.canonical, got, tt.want)
		}
	}
}

func TestStripAliasPath(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/model/proxy/2-7/v1/chat/completions", "/v1/chat/completions"},
		{"/model/proxy/2/v1/videos", "/v1/videos"},
		{"/model/proxy/2-7", "/"},
	}
	for _, tt := range tests {
		if got := stripAliasPath(tt.path, "/model/proxy/"); got != tt.want {
			t.Errorf("stripAliasPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestNormalizeVideoCreatePath(t *testing.T) {
	tests := []struct {
		path, want string
		ok         bool
	}{
		{"/v1/videos", "/v1/videos", true},
		{"/v1/videos/", "/v1/videos", true},
		{"/model/proxy/2/v1/videos", "/v1/videos", true},
		{"/model/proxy/2/v1/videos/", "/v1/videos", true},
		{"/v1/videos/sync", "/v1/videos/sync", false},
		{"/model/proxy/2/v1/chat/completions", "/model/proxy/2/v1/chat/completions", false},
	}
	for _, tt := range tests {
		got, ok := normalizeVideoCreatePath(tt.path, "/model/proxy/")
		if ok != tt.ok || got != tt.want {
			t.Errorf("normalizeVideoCreatePath(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRouteIDFromName(t *testing.T) {
	prefixes := []string{"ai-route-route-"}
	tests := []struct {
		name     string
		wantID   string
		wantOurs bool
	}{
		{"ai-route-route-2.internal", "2", true},
		{"gpustack-system/ai-route-route-12.internal", "12", true},
		{"ai-route-route-2.fallback.internal", "2", true},
		{"ai-route-route-2", "2", true},
		{"other-route-2.internal", "", false},
		{"ai-route-route-x.internal", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		id, ours := routeIDFromName(tt.name, prefixes)
		if ours != tt.wantOurs || id != tt.wantID {
			t.Errorf("routeIDFromName(%q) = (%q, %v), want (%q, %v)", tt.name, id, ours, tt.wantID, tt.wantOurs)
		}
	}
	if _, ours := routeIDFromName("ai-route-route-2.internal", nil); ours {
		t.Error("no prefixes must match nothing")
	}
	// Overlapping prefixes: the first one matches but yields no id, the
	// second one must still be tried.
	if id, ours := routeIDFromName("ai-route-route-2.internal", []string{"ai-route-", "ai-route-route-"}); !ours || id != "2" {
		t.Errorf("overlapping prefixes: got (%q, %v), want (2, true)", id, ours)
	}
}

func TestVideoCreatePrefix(t *testing.T) {
	tests := []struct {
		routeID, cluster, want string
	}{
		{"2", "outbound|80||model-42-7.static", "gpustack_2-7_"},
		{"2", "outbound|80||model-42-7.dns", "gpustack_2-7_"},
		{"2", "outbound|80||provider-5.static", "gpustack_2-provider-5_"},
		// The resolution suffix may carry several labels and a search domain.
		{"2", "outbound|80||model-42-7.static.svc.cluster.local", "gpustack_2-7_"},
		{"2", "outbound|80||provider-5.dns", "gpustack_2-provider-5_"},
		// The route id is the route's; the model in the cluster name is
		// not carried -- a route id and a model id are different numbers.
		{"3", "outbound|80||model-42-7.static", "gpustack_3-7_"},
		{"2", "outbound|80||some-other-svc.static", ""},
		{"", "outbound|80||model-42-7.static", ""},
		{"2", "", ""},
	}
	for _, tt := range tests {
		cfg := PluginConfig{}
		if err := parseConfig(gjson.Parse(`{}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if got := cfg.videoCreatePrefix(tt.routeID, tt.cluster); got != tt.want {
			t.Errorf("videoCreatePrefix(%q, %q) = %q, want %q", tt.routeID, tt.cluster, got, tt.want)
		}
	}
}

// The target a cluster yields is targetPatterns' decision, and the router
// reads the rules through the same wire.TargetOf the LB does: the cluster
// regex is matched, the target is rebuilt from its groups.
func TestVideoCreatePrefixCustomPatterns(t *testing.T) {
	var cfg PluginConfig
	err := parseConfig(gjson.Parse(`{"targetPatterns":[{"target":"^([a-z]+)-node-(\\d+)$","cluster":"pool-\\d+-([a-z]+)-node-(\\d+)"}]}`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ cluster, want string }{
		// a custom list replaces the defaults, it does not add to them. A
		// target may contain hyphens: the ID splits on the first one only.
		{"outbound|80||pool-3-blue-node-9.static", "gpustack_2-blue-node-9_"},
		{"outbound|80||model-42-7.static", ""},
	}
	for _, tt := range tests {
		if got := cfg.videoCreatePrefix("2", tt.cluster); got != tt.want {
			t.Errorf("videoCreatePrefix(2, %q) = %q, want %q", tt.cluster, got, tt.want)
		}
	}
}

func TestWrapVideoID(t *testing.T) {
	out, ok := wrapVideoID([]byte(`{"id":"abc","status":"queued"}`), "gpustack_2-7_")
	if !ok {
		t.Fatal("expected rewrite")
	}
	if got := gjson.GetBytes(out, "id").String(); got != "gpustack_2-7_abc" {
		t.Errorf("id = %q", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "queued" {
		t.Errorf("status must be untouched, got %q", got)
	}

	// Cascade: the cluster gateway already wrapped, the edge wraps again.
	out, ok = wrapVideoID([]byte(`{"id":"gpustack_2-7_abc"}`), "gpustack_9-provider-5_")
	if !ok || gjson.GetBytes(out, "id").String() != "gpustack_9-provider-5_gpustack_2-7_abc" {
		t.Errorf("nested wrap failed: %s", out)
	}

	for name, body := range map[string]string{
		"not json":       `video-bytes`,
		"no id":          `{"status":"queued"}`,
		"non-string id":  `{"id":42}`,
		"error envelope": `{"error":{"message":"nope"}}`,
		"empty":          ``,
	} {
		if out, ok := wrapVideoID([]byte(body), "gpustack_2-7_"); ok {
			t.Errorf("%s: unexpected rewrite to %s", name, out)
		}
	}
}
