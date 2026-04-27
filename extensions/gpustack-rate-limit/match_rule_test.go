package main

import "testing"

func TestSourceIsValid(t *testing.T) {
	cases := []struct {
		in   Source
		want bool
	}{
		{SourceParam, true},
		{SourceHeader, true},
		{SourceCookie, true},
		{SourceIP, true},
		{SourceConsumer, true},
		{"", false},
		{"unknown", false},
		{Source("HEADER"), false}, // enum comparison is case sensitive
	}
	for _, c := range cases {
		if got := c.in.IsValid(); got != c.want {
			t.Errorf("Source(%q).IsValid() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMatchRuleCompile(t *testing.T) {
	cases := []struct {
		name     string
		rule     MatchRule
		wantErr  bool
		wantKind matchKind
	}{
		{"wildcard", MatchRule{Source: SourceHeader, Value: "*"}, false, kindWildcard},
		{"exact", MatchRule{Source: SourceHeader, Value: "abc"}, false, kindExact},
		{"regexp ok", MatchRule{Source: SourceHeader, Value: "regexp:^a.*"}, false, kindRegexp},
		{"regexp invalid", MatchRule{Source: SourceHeader, Value: "regexp:[["}, true, 0},
		{"regexp_capture ok", MatchRule{Source: SourceConsumer, Value: "regexp_capture:^(.+)$"}, false, kindRegexpCapture},
		{"regexp_capture no group", MatchRule{Source: SourceConsumer, Value: "regexp_capture:^foo$"}, true, 0},
		{"regexp_capture invalid", MatchRule{Source: SourceConsumer, Value: "regexp_capture:[["}, true, 0},
		{"regexp_capture multi groups uses first", MatchRule{Source: SourceConsumer, Value: "regexp_capture:^(a)(b)$"}, false, kindRegexpCapture},
		{"ip cidr", MatchRule{Source: SourceIP, Value: "10.0.0.0/8"}, false, kindIPOrCIDR},
		{"ip single v4", MatchRule{Source: SourceIP, Value: "10.0.0.1"}, false, kindIPOrCIDR},
		{"ip single v6", MatchRule{Source: SourceIP, Value: "::1"}, false, kindIPOrCIDR},
		{"ip invalid", MatchRule{Source: SourceIP, Value: "nope"}, true, 0},
		{"wildcard beats ip", MatchRule{Source: SourceIP, Value: "*"}, false, kindWildcard},
		{"bad source", MatchRule{Source: "XYZ", Value: "a"}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.rule
			err := r.Compile()
			if (err != nil) != c.wantErr {
				t.Fatalf("Compile() err=%v, wantErr=%v", err, c.wantErr)
			}
			if err == nil && r.kind != c.wantKind {
				t.Errorf("kind=%d, want %d", r.kind, c.wantKind)
			}
		})
	}
}

func TestMatchRuleMatch(t *testing.T) {
	pathHeaders := [][2]string{{":path", "/chat?model=gpt-4&k=1"}}
	cookieHeaders := [][2]string{{"cookie", `sid="abc-def"; other=1`}}
	headerHeaders := [][2]string{{"X-Api-Key", "premium-123"}}
	consumerHeaders := [][2]string{{"x-mse-consumer", "svc-a"}}
	ipHeaders := [][2]string{{"x-real-ip", "10.0.0.5"}}

	cases := []struct {
		name    string
		rule    MatchRule
		headers [][2]string
		want    *string
	}{
		{
			"header exact hit (case-insensitive lookup)",
			mustCompile(t, MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "premium-123"}),
			headerHeaders,
			strPtr("premium-123"),
		},
		{
			"header regexp hit",
			mustCompile(t, MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "regexp:^premium-"}),
			headerHeaders,
			strPtr("premium-123"),
		},
		{
			"header wildcard hit",
			mustCompile(t, MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "*"}),
			headerHeaders,
			strPtr("premium-123"),
		},
		{
			"header absent",
			mustCompile(t, MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "premium-123"}),
			nil,
			nil,
		},
		{
			"header present but miss",
			mustCompile(t, MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "premium-999"}),
			headerHeaders,
			nil,
		},
		{
			"param exact hit",
			mustCompile(t, MatchRule{Source: SourceParam, Name: "model", Value: "gpt-4"}),
			pathHeaders,
			strPtr("gpt-4"),
		},
		{
			"param missing",
			mustCompile(t, MatchRule{Source: SourceParam, Name: "missing", Value: "*"}),
			pathHeaders,
			nil,
		},
		{
			"cookie quoted value",
			mustCompile(t, MatchRule{Source: SourceCookie, Name: "sid", Value: "abc-def"}),
			cookieHeaders,
			strPtr("abc-def"),
		},
		{
			"consumer wildcard",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: "*"}),
			consumerHeaders,
			strPtr("svc-a"),
		},
		{
			"ip cidr hit",
			mustCompile(t, MatchRule{Source: SourceIP, Name: "x-real-ip", Value: "10.0.0.0/8"}),
			ipHeaders,
			strPtr("10.0.0.5"),
		},
		{
			"ip cidr miss",
			mustCompile(t, MatchRule{Source: SourceIP, Name: "x-real-ip", Value: "192.168.0.0/16"}),
			ipHeaders,
			nil,
		},
		{
			"uncompiled rule returns nil",
			MatchRule{Source: SourceHeader, Name: "x-api-key", Value: "premium-123"}, // not compiled
			headerHeaders,
			nil,
		},
		// regexp_capture cases: API and UI shapes both reduce to gpustack-1
		{
			"regexp_capture API form returns captured user-id",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-.+)$`}),
			[][2]string{{"x-mse-consumer", "ak-x.gpustack-1"}},
			strPtr("gpustack-1"),
		},
		{
			"regexp_capture UI form returns the same captured user-id",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-.+)$`}),
			[][2]string{{"x-mse-consumer", "gpustack-1"}},
			strPtr("gpustack-1"),
		},
		{
			"regexp_capture miss returns nil",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-.+)$`}),
			[][2]string{{"x-mse-consumer", "not-a-gpustack-consumer"}},
			nil,
		},
		// regexp_capture pinned to a specific user-id (example.yml usage):
		// the captured fragment is the literal "gpustack-1", and consumers for
		// other users must miss so they don't accidentally share user-1's bucket.
		{
			"regexp_capture user-pinned API form for user 1",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-1)$`}),
			[][2]string{{"x-mse-consumer", "ak-x.gpustack-1"}},
			strPtr("gpustack-1"),
		},
		{
			"regexp_capture user-pinned UI form for user 1",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-1)$`}),
			[][2]string{{"x-mse-consumer", "gpustack-1"}},
			strPtr("gpustack-1"),
		},
		{
			"regexp_capture user-pinned rejects different user (API form)",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-1)$`}),
			[][2]string{{"x-mse-consumer", "ak-x.gpustack-2"}},
			nil,
		},
		{
			"regexp_capture user-pinned rejects different user (UI form)",
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-1)$`}),
			[][2]string{{"x-mse-consumer", "gpustack-2"}},
			nil,
		},
		{
			"regexp_capture user-pinned end-anchor rejects user-12",
			// Without the trailing $ anchor "gpustack-1" would match the prefix
			// of "gpustack-12" -- the $ is what keeps user-1 isolated from user-12.
			mustCompile(t, MatchRule{Source: SourceConsumer, Value: `regexp_capture:^(?:[^.]+\.)?(gpustack-1)$`}),
			[][2]string{{"x-mse-consumer", "gpustack-12"}},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.rule.Match(c.headers)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("Match() = %v, want %v", got, c.want)
			}
			if got != nil && *got != *c.want {
				t.Errorf("Match() = %q, want %q", *got, *c.want)
			}
		})
	}
}

func TestFindHeader(t *testing.T) {
	h := [][2]string{
		{"Content-Type", "application/json"},
		{"X-Api-Key", "abc"},
	}
	cases := []struct {
		name    string
		find    string
		wantVal string
		wantOK  bool
	}{
		{"exact case", "X-Api-Key", "abc", true},
		{"all lower", "x-api-key", "abc", true},
		{"all upper", "X-API-KEY", "abc", true},
		{"missing", "missing", "", false},
		{"empty name", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok := findHeader(h, c.find)
			if v != c.wantVal || ok != c.wantOK {
				t.Errorf("findHeader(%q) = (%q,%v), want (%q,%v)", c.find, v, ok, c.wantVal, c.wantOK)
			}
		})
	}
}

func TestExtractParam(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		param   string
		wantVal string
		wantOK  bool
	}{
		{"no query", "/chat", "model", "", false},
		{"single param", "/chat?model=gpt-4", "model", "gpt-4", true},
		{"missing param", "/chat?model=gpt-4", "k", "", false},
		{"multi params", "/chat?model=gpt-4&k=1", "k", "1", true},
		{"empty value present", "/chat?model=&k=1", "model", "", true},
		{"empty name", "/chat?model=gpt-4", "", "", false},
		{"no :path header", "", "model", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h [][2]string
			if c.path != "" {
				h = [][2]string{{":path", c.path}}
			}
			v, ok := extractParam(h, c.param)
			if v != c.wantVal || ok != c.wantOK {
				t.Errorf("extractParam(%q,%q) = (%q,%v), want (%q,%v)", c.path, c.param, v, ok, c.wantVal, c.wantOK)
			}
		})
	}
}

func TestExtractCookie(t *testing.T) {
	cases := []struct {
		name    string
		cookie  string
		key     string
		wantVal string
		wantOK  bool
	}{
		{"single", "sid=abc", "sid", "abc", true},
		{"multiple", "sid=abc; other=1", "other", "1", true},
		{"quoted value", `sid="abc-def"`, "sid", "abc-def", true},
		{"missing key", "sid=abc", "missing", "", false},
		{"empty key", "sid=abc", "", "", false},
		{"no cookie header", "", "sid", "", false},
		{"skips non-kv fragment", "foo; sid=abc", "sid", "abc", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h [][2]string
			if c.cookie != "" {
				h = [][2]string{{"cookie", c.cookie}}
			}
			v, ok := extractCookie(h, c.key)
			if v != c.wantVal || ok != c.wantOK {
				t.Errorf("extractCookie(%q,%q) = (%q,%v), want (%q,%v)", c.cookie, c.key, v, ok, c.wantVal, c.wantOK)
			}
		})
	}
}

func TestMatchRuleKeyPart(t *testing.T) {
	cases := []struct {
		name string
		rule MatchRule
		val  string
		want string
	}{
		{"header with name", MatchRule{Source: SourceHeader, Name: "x-api-key"}, "abc", "header:x-api-key=abc"},
		{"param with name", MatchRule{Source: SourceParam, Name: "model"}, "gpt-4", "param:model=gpt-4"},
		{"consumer ignores name", MatchRule{Source: SourceConsumer, Name: "ignored"}, "svc-a", "consumer=svc-a"},
		{"ip with empty name", MatchRule{Source: SourceIP, Name: ""}, "1.2.3.4", "ip=1.2.3.4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.rule.KeyPart(c.val)
			if got != c.want {
				t.Errorf("KeyPart() = %q, want %q", got, c.want)
			}
		})
	}
}
