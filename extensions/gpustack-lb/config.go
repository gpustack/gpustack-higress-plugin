package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/tidwall/gjson"
)

const (
	modeContext  = "context"
	modeFinisher = "finisher"

	DefaultMaxBodyBytes = 100 * 1024 * 1024 // 100 MiB, same as model-mapper

	defaultUnhealthyThreshold = 3
	defaultCooldownMs         = 10000
	defaultMaxInflightAgeMs   = 10 * 60 * 1000 // 10 min
	defaultRejectStatus       = 503
	defaultRejectMessage      = "no healthy model instance available"

	// failOpen defaults to on. Passive health judges by response.flags, and
	// those can go red for every candidate at once for reasons that have
	// nothing to do with the instances themselves: a misconfigured cluster
	// name, network jitter between gateway and engine, engines restarting for
	// an upgrade. Taking the whole route out of service in those cases is far
	// worse than "send it and probably fail" -- the former is a guaranteed
	// 100% failure, while the latter at least lets a recovering instance be
	// probed by the first real request (§6.3 relies on exactly that).
	defaultFailOpen = true

	// defaultRankWeight: one vote when a capability plugin published no weight
	// (or published <= 0).
	defaultRankWeight = 1.0
)

// Config holds **nothing per-route** in the finisher role -- the health
// thresholds, reject message and body parameters are all deployment-level.
// "Does this route do LB" exists in exactly one place (whether the context
// role has candidates), and a finisher that finds no candidate set simply
// passes the request through.
//
// So the finisher only needs defaultConfig and **does not need matchRules**,
// which rules out the "the two CRs' matchRules drifted apart" class of
// misconfiguration.
type Config struct {
	// mode decides which role this instance plays; see the main.go header.
	mode string

	// -- Passive health (§6). The finisher is the only writer of health state.
	unhealthyThreshold int64
	cooldownMs         int64
	// rampMs is the decay window for the recovery penalty; it defaults to
	// cooldownMs.
	//
	// Both it and cooldownMs are configured **only here**: at ejection time
	// both end instants are stamped into the shared state, so the context role
	// can derive the penalty and probation from the state alone and the windows
	// never need to be configured twice.
	rampMs int64

	// maxInflightAgeMs: the finisher is the only writer of in-flight counts, so
	// it is also responsible for pruning leaked entries.
	maxInflightAgeMs int64

	// -- The reject path (§7.3).
	//
	// rejectMessage is kept alongside the rendered body because the body
	// **embeds the status code**, so inheriting one of the two across a
	// matchRule means the body has to be rebuilt from the other. Without the
	// message on hand there would be nothing to rebuild it from; see
	// parseOverrideConfig.
	rejectStatus  int64
	rejectMessage string
	rejectBody    []byte

	// -- Specific to mode: context.
	lbMode     bool
	candidates []candidateSpec
	// modelMappers holds model-name mappings grouped by targetId. The keys use
	// model-mapper's existing syntax (exact / "prefix*" / "*"). The publisher
	// resolves the model name the client sent into the rewrite target and
	// fills it into the candidate's ModelName -- so the finisher needs no
	// mapping logic at all.
	modelMappers map[string]map[string]string

	// failOpen is a pointer, where nil means unset. Not fastidiousness: it
	// distinguishes "not configured" from "explicitly set to false", and the
	// two must behave differently under matchRules inheritance -- see
	// parseOverrideConfig. Always read it through shouldFailOpen().
	failOpen *bool

	// -- model-mapper's existing behaviour on non-LB routes (mode: context).
	exactModelMapping  map[string]string
	prefixModelMapping []ModelMapping
	defaultModel       string

	// -- Body-handling parameters, shared by both modes (§2.1).
	modelKey           string
	enableOnPathSuffix []string
	maxBodyBytes       uint32
	// modelToHeader defaults to x-higress-llm-model-final, verbatim from
	// upstream model-mapper -- that is part of being a drop-in replacement, so
	// do not change it to "write nothing by default".
	//
	// Checked: **no plugin in this repo or upstream higress reads it**, so
	// today it is a write-only header. Keeping it costs nothing, whereas
	// removing it would introduce an invisible behavioural difference from
	// upstream on non-LB routes. There is currently no way to configure it
	// off.
	modelToHeader string
}

// normalizeMode is a pure function so it can be unit-tested -- the warning in
// parseConfig goes through the host ABI, which panics outside a wasm host, so
// the decision logic must not be buried inside it.
//
// Both the default and an unrecognised value fall back to context, which is the
// fail-safe choice: a context role with no candidates does nothing, equivalent
// to today's empty model-mapper config, whereas defaulting to finisher would
// put a plugin on every route trying to write a cluster header and do
// book-keeping.
func normalizeMode(raw string) (mode string, known bool) {
	switch raw {
	case modeFinisher:
		return modeFinisher, true
	case modeContext, "":
		return modeContext, true
	default:
		return modeContext, false
	}
}

func parseConfig(j gjson.Result, config *Config) error {
	// **Reset the destination first.** rule_matcher re-runs the parse on the
	// *same* &rule.config: when the first attempt returns an error it retries
	// with a backup JSON (rule_matcher.go:251 then :265), so whatever the
	// failed attempt managed to write is still sitting in the struct. Every
	// field this parser writes only conditionally would survive into the retry
	// -- lbMode, candidates, modelMappers, defaultModel and failOpen are all in
	// that class, and all of them change routing. A concrete case: attempt one
	// sets defaultModel from `modelMapping: {"*": "foo"}` and then fails on a
	// malformed enableOnPathSuffix; the backup JSON has no modelMapping, so
	// defaultModel stays "foo" and every request on the route is silently
	// rewritten to it.
	//
	// Zeroing the whole struct rather than resetting fields one by one is
	// deliberate: a per-field list is only correct until the next field is
	// added, and the failure mode of forgetting one is silent.
	*config = Config{}

	// mode defaults to context, the fail-safe choice: see normalizeMode.
	mode, known := normalizeMode(j.Get("mode").String())
	if !known {
		proxywasm.LogWarnf("%s: unknown mode %q, falling back to %q",
			pluginName, j.Get("mode").String(), modeContext)
	}
	config.mode = mode

	if err := parseLegacy(j, config); err != nil {
		return err
	}
	if err := parseLB(j, config); err != nil {
		return err
	}

	config.unhealthyThreshold = j.Get("health.unhealthyThreshold").Int()
	if config.unhealthyThreshold <= 0 {
		config.unhealthyThreshold = defaultUnhealthyThreshold
	}
	config.cooldownMs = j.Get("health.cooldownMs").Int()
	if config.cooldownMs <= 0 {
		config.cooldownMs = defaultCooldownMs
	}
	config.rampMs = j.Get("health.rampMs").Int()
	if config.rampMs <= 0 {
		config.rampMs = config.cooldownMs
	}

	// failOpen is parsed outside parseLB: parseLB returns early when there are
	// no candidates, and the global config location (defaultConfig) is
	// precisely the one without any.
	failOpen, failOpenSet, err := boolField(j, "health.failOpen")
	if err != nil {
		return err
	}
	if failOpenSet {
		config.failOpen = &failOpen
	}

	config.maxInflightAgeMs = j.Get("maxInflightAgeMs").Int()
	if config.maxInflightAgeMs <= 0 {
		config.maxInflightAgeMs = defaultMaxInflightAgeMs
	}

	status, ok := normalizeRejectStatus(j.Get("reject.status").Int())
	if !ok {
		proxywasm.LogWarnf("%s: reject.status %d out of range, using %d",
			pluginName, j.Get("reject.status").Int(), defaultRejectStatus)
	}
	config.rejectStatus = status
	config.rejectMessage = rejectMessage(j)
	config.rejectBody = buildRejectBody(config.rejectMessage, config.rejectStatus)

	return nil
}

// rejectMessage returns the reject message, falling back to the default when
// empty.
//
// **The fallback message must not be removed**: an empty body makes Envoy's
// local-reply path treat the response as degenerate -- it queues it but never
// flushes (§7.3).
func rejectMessage(j gjson.Result) string {
	if msg := j.Get("reject.message").String(); msg != "" {
		return msg
	}
	return defaultRejectMessage
}

// normalizeRejectStatus is a pure function -- the warning in parseConfig goes
// through the host ABI, which panics outside a wasm host, so the decision
// logic must not be buried inside it (§9 rule 6).
//
// **The range has to be enforced**: reject.go converts this to uint32, and
// 4294967496 truncates to exactly **200** -- a rejection that should have been
// fail-closed turns into a success response, completely silently.
//
// The floor is 400, not 100, and that is the same argument applied to the
// configured value rather than to an overflowed one. A reject path answering
// 2xx or 3xx is not a rejection: `reject.status: 200` hands the caller a
// success carrying an error envelope, and 204 is defined to have no body at
// all, so Envoy drops the body that §7.3 requires to be non-empty. Neither has
// a legitimate use here -- this status exists solely to refuse a request.
//
// ok is false when the supplied value is out of range (0, meaning unset, is
// not out of range -- that is the normal default path).
func normalizeRejectStatus(raw int64) (status int64, ok bool) {
	if raw == 0 {
		return defaultRejectStatus, true
	}
	if raw < 400 || raw > 599 {
		return defaultRejectStatus, false
	}
	return raw, true
}

// boolField reads a **strict** JSON boolean.
//
// gjson's Bool() coerces: a string that is not "true"/"false", the number 0,
// and a null literal all come back as false. For a field where false is a
// meaningful setting rather than a default, that turns a typo into a deliberate
// choice -- `health.failOpen: "oops"` silently flips a route from fail-open to
// fail-closed, which is the exact distinction the pointer on failOpen exists to
// preserve.
//
// A null literal is reported as **unset** rather than rejected: gjson's
// Exists() is true for null (the trap gpustack-rate-limit documents for its
// `redis` field), and `failOpen: null` reads naturally as "not configured".
//
// Fields whose malformed value merely falls back to a default -- the health
// windows, maxInflightAgeMs, reject.status -- do not need this: coercing them
// to 0 lands on the default path, which is already the safe answer.
func boolField(j gjson.Result, path string) (val bool, set bool, err error) {
	r := j.Get(path)
	if !r.Exists() || r.Type == gjson.Null {
		return false, false, nil
	}
	if r.Type != gjson.True && r.Type != gjson.False {
		return false, false, fmt.Errorf("%s must be a boolean, got %s", path, r.Raw)
	}
	return r.Type == gjson.True, true, nil
}

// intField reads a JSON number that has to be a whole number.
//
// gjson's Int() **truncates** rather than failing: 1.5 -> 1, and 0.5 / 0.9 /
// -0.5 all -> 0. Truncating toward zero is the dangerous direction for every
// field here, because 0 is a meaningful value in all of them -- a fractional
// weight below 1 lands on "explicitly weighted zero", which pins the candidate
// unreachable and drags the whole set into weighted mode, and a fractional
// maxRunningRequests lands on "unlimited".
//
// Truncation also slips past a plain negative check: -0.5 becomes 0, which is
// not < 0, so the value has to be rejected on this side of the conversion.
//
// Integral floats are accepted (2.0, 1e3): the value is what matters, not how
// the YAML happened to render it.
func intField(r gjson.Result, what string) (int64, error) {
	if r.Type != gjson.Number {
		return 0, fmt.Errorf("%s must be a number, got %s", what, r.Raw)
	}
	if r.Num != math.Trunc(r.Num) {
		return 0, fmt.Errorf("%s must be a whole number, got %s", what, r.Raw)
	}
	return r.Int(), nil
}

// shouldFailOpen is the only read path. nil (unset) is treated as on.
func (c Config) shouldFailOpen() bool {
	if c.failOpen == nil {
		return defaultFailOpen
	}
	return *c.failOpen
}

// parseOverrideConfig lets matchRules inherit the **deployment-level** config
// from defaultConfig. Topology (`candidates` / `modelMappers` /
// `modelMapping`) is never inherited.
//
// The dividing line is whether a field describes the deployment or the
// topology:
//
//   - **Deployment-level** fields are configured once in defaultConfig. The
//     health windows, the reject body and the body-handling parameters are all
//     in this class -- none of them depend on which route is involved.
//   - **Topology is per-route by definition**, and inheriting it is dangerous
//     rather than convenient: candidates in defaultConfig would turn every
//     route not listed in matchRules into an LB route, and modelMapping would
//     silently rewrite model names on all of them.
//
// **No wholesale `*config = global` copy**; every field is copied explicitly.
// A wholesale copy makes both sides share slice headers, and an `append` onto
// an inherited slice with spare capacity writes through into the global config
// (gpustack-rate-limit was burned by exactly this, see CLAUDE.md). Copying
// field by field also turns "what is not inherited" into an explicit list.
//
// ⚠️ When the global config fails to parse, rule_matcher **swallows the error
// and leaves m.globalConfig zero-valued** (rule_matcher.go:198-206), so the
// global handed in here is the zero value. Every copy therefore has to check
// that the global side holds a valid value first, and must not copy
// unconditionally -- otherwise one typo in an unrelated global field would
// zero this field on every route. failOpen is a pointer for exactly this
// moment: a zero-valued global's failOpen is nil rather than false, so the
// inherited value stays nil and the read path applies the default of true.
func parseOverrideConfig(j gjson.Result, global Config, config *Config) error {
	if err := parseConfig(j, config); err != nil {
		return err
	}

	// -- Role and fail-open posture.
	if !j.Get("mode").Exists() && global.mode != "" {
		config.mode = global.mode
	}
	if config.failOpen == nil {
		config.failOpen = global.failOpen
	}

	// -- The three passive-health windows. Only the finisher reads them, but
	// the config location is deployment-level.
	inheritInt64(j, "health.unhealthyThreshold", &config.unhealthyThreshold, global.unhealthyThreshold)
	inheritInt64(j, "health.cooldownMs", &config.cooldownMs, global.cooldownMs)
	// rampMs defaults to cooldownMs, so it has to be inherited together with
	// cooldownMs to stay coherent: when the global sets cooldownMs: 60000 and
	// the rule sets no rampMs, the rule should get 60000 -- not the value
	// parseConfig derived from the **rule's own** cooldownMs.
	if !j.Get("health.rampMs").Exists() {
		if global.rampMs > 0 {
			config.rampMs = global.rampMs
		} else {
			config.rampMs = config.cooldownMs
		}
	}
	inheritInt64(j, "maxInflightAgeMs", &config.maxInflightAgeMs, global.maxInflightAgeMs)

	// -- The reject body. **The rendered body embeds the status code**, so it
	// is only safe to reuse the global body when status and message are *both*
	// inherited. A rule that overrides only reject.status would otherwise ship
	// the global body: the response goes out as 429 while its JSON says
	// "code": 503, and the client is told two different things.
	statusSet := j.Get("reject.status").Exists()
	messageSet := j.Get("reject.message").Exists()
	if !statusSet && global.rejectStatus > 0 {
		config.rejectStatus = global.rejectStatus
	}
	if !messageSet && global.rejectMessage != "" {
		config.rejectMessage = global.rejectMessage
	}
	switch {
	case !statusSet && !messageSet && len(global.rejectBody) > 0:
		// Neither was overridden, so the global body already agrees with the
		// inherited status. Reuse it rather than re-rendering identical JSON.
		config.rejectBody = global.rejectBody
	default:
		config.rejectBody = buildRejectBody(config.rejectMessage, config.rejectStatus)
	}

	// -- Body-handling parameters, shared by both modes.
	if !j.Get("modelKey").Exists() && global.modelKey != "" {
		config.modelKey = global.modelKey
	}
	if !j.Get("modelToHeader").Exists() && global.modelToHeader != "" {
		config.modelToHeader = global.modelToHeader
	}
	if !j.Get("maxBodyBytes").Exists() && global.maxBodyBytes > 0 {
		config.maxBodyBytes = global.maxBodyBytes
	}
	if !j.Get("enableOnPathSuffix").Exists() && len(global.enableOnPathSuffix) > 0 {
		// Copy rather than share the slice header: if the inherited slice has
		// spare capacity, any future append would write through into the
		// global config.
		config.enableOnPathSuffix = append([]string(nil), global.enableOnPathSuffix...)
	}

	return nil
}

// inheritInt64 copies the global value only when the rule did not set the
// field and the global value is valid.
func inheritInt64(j gjson.Result, path string, dst *int64, globalVal int64) {
	if !j.Get(path).Exists() && globalVal > 0 {
		*dst = globalVal
	}
}

// defaultPathSuffixes is higress's original list plus the multipart endpoints
// (/audio/transcriptions, /audio/translations, /images/edits). Upstream leaves
// those out because it only handles JSON bodies; we handle them.
func defaultPathSuffixes() []string {
	return []string{
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

// ModelMapping follows higress model-mapper: Prefix is the key with its
// trailing "*" removed.
type ModelMapping struct {
	Prefix string
	Target string
}

// parseLegacy follows higress model-mapper's config semantics verbatim, so
// that non-LB routes behave exactly as they do today.
func parseLegacy(j gjson.Result, config *Config) error {
	config.modelKey = j.Get("modelKey").String()
	if config.modelKey == "" {
		config.modelKey = "model"
	}

	config.modelToHeader = j.Get("modelToHeader").String()
	if config.modelToHeader == "" {
		config.modelToHeader = "x-higress-llm-model-final"
	}

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

	// The two maps are allocated rather than merely reset: the rest of this
	// function assigns into them. Everything else is already zeroed by
	// parseConfig, which wipes the whole struct before parsing -- see the note
	// there on rule_matcher's retry.
	config.exactModelMapping = make(map[string]string)
	config.prefixModelMapping = make([]ModelMapping, 0)

	// Verbatim from higress: reproduce C++ nlohmann::json's lexicographic key
	// iteration, so that "first matching prefix" is a deterministic priority
	// and agrees with upstream.
	type mappingEntry struct{ key, value string }
	var entries []mappingEntry
	modelMapping.ForEach(func(key, value gjson.Result) bool {
		entries = append(entries, mappingEntry{key: key.String(), value: value.String()})
		return true
	})
	sort.Slice(entries, func(i, k int) bool { return entries[i].key < entries[k].key })

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
		config.enableOnPathSuffix = defaultPathSuffixes()
	}

	return nil
}

func parseLB(j gjson.Result, config *Config) error {
	cands := j.Get("candidates")
	// The **presence** of candidates is what decides whether this route does LB
	// at all, so a malformed value must not be read as "absent". `candidates: {}`
	// or a typo would otherwise drop the route into legacy model-mapper handling
	// with no error anywhere -- LB silently off on a route that was configured
	// for it, which is the single most consequential thing that can go wrong
	// quietly here.
	//
	// An explicit JSON null is treated as absent rather than rejected: gjson's
	// Exists() is true for a null literal (the same trap gpustack-rate-limit
	// documents for its `redis` field), and `candidates: null` is a reasonable
	// way to write "none".
	if cands.Exists() && cands.Type != gjson.Null && !cands.IsArray() {
		return errors.New("candidates must be an array")
	}
	if !cands.Exists() || !cands.IsArray() || len(cands.Array()) == 0 {
		return nil
	}

	// totalWeight is accumulated only to reject a set whose weights overflow
	// int64. selectWeighted has a silent backstop for the same case, but this is
	// the place that can actually say so.
	var totalWeight int64
	for _, c := range cands.Array() {
		cluster := c.Get("cluster").String()
		if cluster == "" {
			return errors.New("candidates[].cluster must not be empty")
		}
		// kind has to be given explicitly for a provider; it cannot be inferred
		// from "there is only one candidate", because a single-instance
		// self-hosted model also has one candidate and also has no engine
		// metrics. The selection behaviour happens to be identical, but the
		// health marking must differ (a provider's DNS cluster has many
		// endpoints, so ejecting all of it is an over-reaction).
		//
		// An unrecognised value is an **error, not a silent fallback**. Folding
		// everything that is not "provider" into "instance" means a typo like
		// `kind: provder` turns passive health ejection back on for a provider
		// -- the one case this field exists to prevent -- with nothing in the
		// logs to say so.
		kind := c.Get("kind").String()
		switch kind {
		case "":
			kind = KindInstance
		case KindInstance, KindProvider:
		default:
			return fmt.Errorf("candidates[].kind %q must be %q or %q",
				kind, KindInstance, KindProvider)
		}
		// 0 means unlimited, so this field cannot lean on a positivity check the
		// way maxBodyBytes does -- every other way of being wrong has to be
		// rejected explicitly. The filter tests `maxRun > 0`, so anything that
		// lands on 0 silently removes the concurrency cap from exactly the
		// candidate an operator was trying to cap:
		//
		//	maxRunningRequests: -1       negative, gjson returns it as-is
		//	maxRunningRequests: "oops"   gjson's Int() renders a non-number as 0
		var maxRunning int64
		if mr := c.Get("maxRunningRequests"); mr.Exists() && mr.Type != gjson.Null {
			v, err := intField(mr, fmt.Sprintf("candidates[].maxRunningRequests (cluster %q)", cluster))
			if err != nil {
				return err
			}
			if v < 0 {
				return fmt.Errorf("candidates[].maxRunningRequests must not be negative, got %d for cluster %q",
					v, cluster)
			}
			maxRunning = v
		}
		cand := candidateSpec{
			Candidate: Candidate{
				Cluster:  cluster,
				TargetID: c.Get("targetId").String(),
				Kind:     kind,
			},
			maxRunningRequests: maxRunning,
			// ModelName is not set here: it is resolved per request from
			// modelMappers against **the model name the client sent** (see
			// resolveCandidateModel). Hard-coding a modelName on the candidate
			// cannot express "one target serving several models" -- in that case
			// the rewrite target depends on which one the client asked for, not
			// on which candidate LB picked.
		}
		// **The presence of the field is the mode switch**, so use Exists()
		// rather than testing for zero: weight: 0 is meaningful (a canary
		// dialled to 0% -- the candidate stays in the set with a zero share).
		//
		// A negative weight is rejected rather than clamped. selectWeighted
		// skips non-positive weights when summing, so `weight: -1` would keep
		// the candidate permanently unselectable while still forcing the whole
		// set into weighted mode -- which looks exactly like "the canary is
		// configured but gets no traffic", with no error to explain it.
		// Rejecting also keeps the weighted total non-negative, which is what
		// the overflow guard in selectWeighted relies on.
		if w := c.Get("weight"); w.Exists() && w.Type != gjson.Null {
			// Type-checked for the same reason as maxRunningRequests: gjson
			// coerces a non-number to 0 and truncates a fractional one toward
			// zero, and 0 is a **meaningful** weight (a canary at 0%). So
			// neither `weight: "oops"` nor `weight: 0.5` would fall back to a
			// default -- both would mark the candidate explicitly
			// weighted-zero, making it unreachable, and drag the whole set into
			// weighted mode so every scoring opinion is discarded too.
			v, err := intField(w, fmt.Sprintf("candidates[].weight (cluster %q)", cluster))
			if err != nil {
				return err
			}
			if v < 0 {
				return fmt.Errorf("candidates[].weight must not be negative, got %d for cluster %q", v, cluster)
			}
			if totalWeight > math.MaxInt64-v {
				return errors.New("candidates[].weight values sum beyond int64; use proportional values, not absolute request counts")
			}
			totalWeight += v
			cand.Weight = &v
		}
		config.candidates = append(config.candidates, cand)
	}

	// (cluster, targetId) is the candidate's identity everywhere downstream --
	// it is what wire.Candidate.Key() returns and what every capability
	// plugin's score map is keyed by. Two candidates sharing **both** are
	// therefore genuinely indistinguishable, and would reintroduce the score
	// collapse that splitting the key was meant to fix. Several candidates on
	// one cluster with *different* targetIds stay legal: that is gpustack's
	// several-model-names-on-one-cluster case.
	seen := make(map[string]struct{}, len(config.candidates))
	for _, c := range config.candidates {
		k := c.Key()
		if _, dup := seen[k]; dup {
			return fmt.Errorf("candidates has two entries with cluster %q and targetId %q; "+
				"they would be indistinguishable to every capability plugin", c.Cluster, c.TargetID)
		}
		seen[k] = struct{}{}
	}

	// The candidates are sorted before the weighted intervals are built. As
	// long as the sort key is stable, the same candidates plus the same weights
	// always yield the same interval split; otherwise removing an instance and
	// adding it back would redistribute all the traffic. The finisher relies on
	// this order, so sort here before publishing.
	//
	// **The key has to be (cluster, targetId), not the cluster alone.** Several
	// candidates may share a cluster, sort.Slice is not stable, and weighted
	// intervals are order-sensitive (TestSelectWeightedIsOrderSensitive pins
	// that down) -- so a cluster-only comparator leaves the relative order of
	// same-cluster candidates unspecified, and one reparse can move a given
	// request id onto a different model target.
	sort.Slice(config.candidates, func(i, k int) bool {
		if config.candidates[i].Cluster != config.candidates[k].Cluster {
			return config.candidates[i].Cluster < config.candidates[k].Cluster
		}
		return config.candidates[i].TargetID < config.candidates[k].TargetID
	})

	// modelMappers is grouped by targetId, and within a group uses
	// model-mapper's key syntax (exact / "prefix*" / "*"). No matching group
	// means that target never rewrites.
	// Same rule as candidates: a malformed value must not be read as "absent".
	// Silently skipping a mis-typed modelMappers, or a mis-typed group inside
	// it, means the candidates it covers stop rewriting their model name --
	// requests then reach the instance asking for a model it does not serve,
	// with nothing in the config saying why.
	mm := j.Get("modelMappers")
	if mm.Exists() && mm.Type != gjson.Null && !mm.IsObject() {
		return errors.New("modelMappers must be an object")
	}
	if mm.IsObject() {
		config.modelMappers = make(map[string]map[string]string)
		var groupErr error
		mm.ForEach(func(target, table gjson.Result) bool {
			if !table.IsObject() {
				groupErr = fmt.Errorf("modelMappers[%q] must be an object", target.String())
				return false
			}
			group := make(map[string]string)
			table.ForEach(func(k, v gjson.Result) bool {
				group[k.String()] = v.String()
				return true
			})
			config.modelMappers[target.String()] = group
			return true
		})
		if groupErr != nil {
			return groupErr
		}
	}

	config.lbMode = true

	return nil
}

// resolveModel implements higress model-mapper's resolution order:
// exact match -> first matching prefix (the order fixed by parseLegacy's
// lexicographic sort) -> defaultModel -> unchanged.
//
// A pure function (no host calls), so it is directly unit-testable.
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

// resolveCandidateModel resolves the model name the client sent into a rewrite
// target, using the mapping group of the candidate's target. No matching group,
// or no match within the group, returns an empty string (no rewrite).
//
// Resolution within a group matches model-mapper: exact -> first matching
// prefix (**lexicographic iteration**) -> "*" -> no rewrite.
//
// ⚠️ "First matching prefix" is not "longest match". With both `"legacy-*"`
// and `"legacy-coder-*"` present, `legacy-coder-7b` hits the former, because
// '*'(0x2A) < 'c' puts `legacy-*` first lexicographically. This is upstream
// higress's existing behaviour and is pinned by a test; to make a longer
// prefix win, the key itself has to change.
func resolveCandidateModel(config Config, targetID, clientModel string) string {
	group, ok := config.modelMappers[targetID]
	if !ok || len(group) == 0 {
		return ""
	}
	if v, ok := group[clientModel]; ok {
		return v
	}
	keys := make([]string, 0, len(group))
	for k := range group {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "*" || !strings.HasSuffix(k, "*") {
			continue
		}
		if strings.HasPrefix(clientModel, strings.TrimSuffix(k, "*")) {
			return group[k]
		}
	}
	if v, ok := group["*"]; ok {
		return v
	}
	return ""
}
