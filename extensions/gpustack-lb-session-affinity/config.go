package main

import (
	"errors"
	"mime"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/tidwall/gjson"
)

// defaultBodyKeySuffixes: by default, body sources are only looked up on these
// endpoints.
//
// Every body source requires buffering the body, and the finisher buffers it
// again, so a path gate is mandatory (design §15). These are the endpoints
// where the agent clients actually carry a session key: the Responses API
// (prompt_cache_key / previous_response_id) and Anthropic messages
// (metadata.user_id).
//
// **chat/completions is deliberately excluded**: it is the hottest path, and
// there is currently no known standard session key on it. Anyone using a
// custom field there has to add it themselves.
//
// This returns a fresh slice rather than sharing a package-level variable:
// assigning the package slice into every Config would let any future append
// cross-contaminate configs.
func defaultBodyKeySuffixes() []string { return []string{"/responses", "/messages"} }

// defaultWeight is this entry's vote multiplier, 1 by default.
//
// After the finisher L1-normalises, each entry casts exactly one vote, and
// weight is the multiplier -- weight: 2 means this entry casts two votes. Zero
// is not a meaningful value ("do not participate" is expressed by not
// installing the plugin, or by defaultConfigDisable), so this is a bare
// float64 rather than a pointer (§9 rule 5).
const defaultWeight = 1.0

const (
	sourceHeader = "header"
	sourceBody   = "bodyKey"
)

// defaultMaxBodyBytes mirrors gpustack-lb's DefaultMaxBodyBytes.
//
// The two have to agree, and the value is duplicated rather than imported
// because it is a **deployment setting, not a wire contract** -- putting it in
// the shared wire package would imply the finisher reads it, which it does not.
// See the comment in onHttpRequestHeaders for why this plugin has to set a
// buffer limit at all.
const defaultMaxBodyBytes uint32 = 100 * 1024 * 1024

// keySource is one link in the chain.
type keySource struct {
	kind string // sourceHeader | sourceBody
	name string // header name, or gjson path
}

func (k keySource) isHeader() bool { return k.kind == sourceHeader }

// Config is about one thing only: where the session key comes from.
//
// **There is no hash-algorithm switch.** Such a field would have exactly one
// correct value: hash % N reshuffles every session whenever the candidate set
// changes, and the set changes at precisely the moments stickiness matters
// most (scaling, passive-health ejection). A knob that can only be set one way
// is just an entry point for misconfiguration -- the same reasoning that
// removed the selection enum in §4.2.
type Config struct {
	// sessionKeys is an **ordered** chain; the first source that yields a
	// value wins.
	//
	// Why a chain rather than a single source: one gateway serves several
	// client types and they mark sessions with different fields -- Codex uses
	// a header, Claude Code uses metadata inside the body. With only one
	// source configurable, the other kind of client silently gets no
	// stickiness.
	//
	// Order is priority, with one exception: header sources are consulted
	// before body sources as a group, because reading a header is free (see
	// onHttpRequestHeaders).
	sessionKeys []keySource

	// enableOnPathSuffix only matters for body sources -- header sources never
	// touch the body, so there is no cost to save.
	enableOnPathSuffix []string

	// weight is this entry's vote multiplier in the weighted sum, published
	// alongside the opinion.
	weight float64

	// maxBodyBytes is the decoder buffer limit applied before this plugin stops
	// iteration to buffer a body source. It must match gpustack-lb's
	// maxBodyBytes; see onHttpRequestHeaders.
	maxBodyBytes uint32
}

// hasBodySource: with no body source, iteration never has to stop to buffer
// the body.
func (c Config) hasBodySource() bool {
	for _, k := range c.sessionKeys {
		if !k.isHeader() {
			return true
		}
	}
	return false
}

// parseSessionKeys is a pure function so it can be unit-tested. The rest of
// parseConfig makes no host calls either, but splitting the parsing out is
// still worth it: the chain's resolution rules are the one thing in this
// plugin that is easy to misconfigure.
//
// Each link must set **exactly one** of header or bodyKey. Setting both is an
// ambiguous config and is an error rather than a guess: each link in the chain
// is an independent hop, and guessing wrong would make the chain's actual
// order disagree with what the config says -- and order is the entire meaning
// of this field.
func parseSessionKeys(j gjson.Result) ([]keySource, error) {
	arr := j.Get("sessionKeys")
	if !arr.Exists() {
		return nil, errors.New("sessionKeys must be set")
	}
	if !arr.IsArray() {
		return nil, errors.New("sessionKeys must be an array")
	}

	var out []keySource
	var parseErr error
	arr.ForEach(func(_, item gjson.Result) bool {
		h := strings.TrimSpace(item.Get(sourceHeader).String())
		b := strings.TrimSpace(item.Get(sourceBody).String())
		switch {
		case h != "" && b != "":
			parseErr = errors.New("each sessionKeys entry must set exactly one of 'header' or 'bodyKey', not both")
			return false
		case h != "":
			// Header names are case-insensitive and normalised to lower case:
			// Envoy's headers are all lower case, and a config that writes
			// Session-Id and then silently fails to match is very hard to
			// diagnose.
			out = append(out, keySource{kind: sourceHeader, name: strings.ToLower(h)})
		case b != "":
			out = append(out, keySource{kind: sourceBody, name: b})
		default:
			parseErr = errors.New("each sessionKeys entry must set one of 'header' or 'bodyKey'")
			return false
		}
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if len(out) == 0 {
		// An empty array is the same as not installing the plugin. Error out
		// rather than silently doing nothing: Higress's defaultConfig also
		// applies to routes absent from matchRules, and a config that does
		// nothing is very hard to tell apart from one that is simply wrong.
		return nil, errors.New("sessionKeys must not be empty")
	}
	return out, nil
}

func parseConfig(j gjson.Result, config *Config) error {
	// Reset the destination first. rule_matcher re-runs the parse on the *same*
	// &rule.config when the first attempt errors (rule_matcher.go:251 then
	// :265), so leftovers from the failed attempt are still in the struct.
	// Every field below happens to be written unconditionally today, so this
	// changes nothing right now -- it is here so that adding a field written
	// only when present does not quietly reintroduce the problem.
	*config = Config{}

	keys, err := parseSessionKeys(j)
	if err != nil {
		return err
	}
	config.sessionKeys = keys

	config.weight = defaultWeight
	if w := j.Get("weight"); w.Exists() && w.Float() > 0 {
		config.weight = w.Float()
	}

	config.maxBodyBytes = defaultMaxBodyBytes
	if mbb := j.Get("maxBodyBytes"); mbb.Exists() {
		v := mbb.Int()
		if v <= 0 {
			return errors.New("maxBodyBytes must be a positive integer")
		}
		if v > int64(^uint32(0)) {
			v = int64(^uint32(0))
		}
		config.maxBodyBytes = uint32(v)
	}

	config.enableOnPathSuffix = nil
	suffixes := j.Get("enableOnPathSuffix")
	if suffixes.Exists() {
		if !suffixes.IsArray() {
			return errors.New("enableOnPathSuffix must be an array")
		}
		for _, item := range suffixes.Array() {
			if s := item.String(); s != "" {
				config.enableOnPathSuffix = append(config.enableOnPathSuffix, s)
			}
		}
	} else {
		config.enableOnPathSuffix = defaultBodyKeySuffixes()
	}

	return nil
}

// matchPathSuffix is a pure function so it can be unit-tested (the caller
// needs a host call to read :path).
func matchPathSuffix(path string, suffixes []string) bool {
	for _, s := range suffixes {
		if s == "*" || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// bodyLookupApplies decides whether this request is worth buffering a body
// for.
//
// Two gates: the path suffix and the content type. If neither matches,
// iteration does not stop -- this plugin runs before the finisher, and every
// extra stop is another full copy of the body into wasm linear memory.
func bodyLookupApplies(config Config) bool {
	path, err := proxywasm.GetHttpRequestHeader(":path")
	if err != nil {
		return false
	}
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	if !matchPathSuffix(path, config.enableOnPathSuffix) {
		return false
	}

	// Only JSON can be read with gjson. Multipart endpoints such as
	// /audio/transcriptions will not carry a session key.
	ct, err := proxywasm.GetHttpRequestHeader("content-type")
	if err != nil {
		return false
	}
	return isJSONMediaType(ct)
}

// isJSONMediaType parses the media type instead of prefix-matching it.
//
// A prefix match also accepts `application/jsonx` and friends, and the cost is
// not just a wasted check: this plugin would then stop iteration and buffer a
// whole body it cannot parse as JSON, and raise the decoder buffer limit to do
// it. Parameters are still fine -- `application/json; charset=utf-8` is the
// common real-world form.
//
// Pure, so it is unit-testable; the caller needs a host call to read the
// header. This mirrors gpustack-lb's baseMediaType, which gates the finisher's
// buffering on the same requests -- the two must agree or one will buffer for a
// request the other passes through.
func isJSONMediaType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(mt)) == "application/json"
}

// parseOverrideConfig lets matchRules inherit the **deployment-level** config
// from defaultConfig.
//
// Without it, `weight` and `enableOnPathSuffix` set in defaultConfig would be
// silently reset to the defaults by every matchRule -- and those two are
// precisely deployment-level (the first is how much say this entry has, the
// second is the path gate for body sources), so configuring them globally is
// the natural thing to do.
//
// `sessionKeys` is **not** inherited: which key to read is a per-route fact,
// and it is mandatory anyway -- parseConfig errors out when it is missing, so
// there is no "inherit an empty chain" ambiguity.
//
// ⚠️ When the global config fails to parse, rule_matcher swallows the error and
// leaves globalConfig zero-valued, so every field has to check that the global
// side holds a valid value first.
func parseOverrideConfig(j gjson.Result, global Config, config *Config) error {
	if err := parseConfig(j, config); err != nil {
		return err
	}
	if !j.Get("weight").Exists() && global.weight > 0 {
		config.weight = global.weight
	}
	if !j.Get("maxBodyBytes").Exists() && global.maxBodyBytes > 0 {
		config.maxBodyBytes = global.maxBodyBytes
	}
	if !j.Get("enableOnPathSuffix").Exists() && len(global.enableOnPathSuffix) > 0 {
		// Copy rather than share the slice header.
		config.enableOnPathSuffix = append([]string(nil), global.enableOnPathSuffix...)
	}
	return nil
}
