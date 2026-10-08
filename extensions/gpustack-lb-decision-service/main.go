// gpustack-lb-decision-service is a Jev-based routing decision stage for the
// gpustack-lb framework. It sits in the LB band after the context role has
// published the candidate set: for a task request it asks a Jev-compatible
// decision service (the "jev" system-1 model) which candidate model should
// serve the task, and appends a confidence-weighted rank entry to the shared
// FilterStateRanks array. The gpustack-lb finisher (AUTHN/325, further down
// the same chain) combines all rank entries by weighted sum and writes
// `x-higress-target-cluster` — this plugin never writes the header itself.
//
// Integration with the framework:
//   - candidates are NOT configured here; they are read from the filter
//     state key the gpustack-lb context role publishes (wire.FilterStateCandidates)
//   - a weighted candidate set (business traffic split) is left untouched,
//     like every capability plugin
//   - when the decision fails or the verdict is unknown, no rank entry is
//     published and the request simply continues — the finisher routes it
//     with the remaining opinions (least-load, session-affinity) as usual
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb-decision-service/provider"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-lb/wire"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName = "gpustack-lb-decision-service"

	// headerFallbackFrom marks an Envoy internal redirect; such requests
	// are not routed again.
	headerFallbackFrom = "x-higress-fallback-from"

	modelSelectionQuestionID = "model_selection"

	defaultDecisionTimeoutMs = 3000
	defaultMaxStateBytes     = 64 * 1024
	defaultMaxBodyBytes      = 100 * 1024 * 1024

	// defaultTokenCooldownMs is how long a token that failed with
	// 401/403/429 is skipped before being tried again (ai-proxy's
	// cooldownDuration recovery path, minus its health checks): without
	// it, a long-dead primary key would cost every request one wasted
	// callout round-trip on the data-plane critical path.
	defaultTokenCooldownMs = 30000

	// tokenUnavailableSinceKeyPrefix namespaces the cooldown shared-data
	// key per decision provider, so tenants with different providers
	// never trip each other's cooldowns.
	tokenUnavailableSinceKeyPrefix = pluginName + "-token-unavailable-since-"

	// defaultRankWeight makes the model-selection verdict dominant in the
	// finisher's weighted sum while leaving instance-level tie-breaking to
	// the other capability opinions.
	defaultRankWeight = 10

	// defaultModelSelectionInstructions is used when the route enables
	// modelSelection without writing its own instructions.
	defaultModelSelectionInstructions = "Which candidate model is the most suitable to execute this task? Consider the reasoning depth required, code/math complexity, and the tolerance for errors."

	// calloutBodyReadCap bounds the decision response read: the jev answer
	// is small and only answers.model_selection is needed.
	calloutBodyReadCap = 64 * 1024

	ctxArmed = "ai_route_proxy_armed"
)

// ModelSelectionConfig describes the injected `model_selection` question.
// Both fields are optional: the option list always comes from the published
// candidates at runtime; Criteria entries describe specific models, and any
// candidate without an entry falls back to a name-only option.
type ModelSelectionConfig struct {
	// Instructions is the choice question's `instructions` field. Defaults
	// to defaultModelSelectionInstructions.
	Instructions string
	// Criteria maps candidate model name -> capability description.
	Criteria map[string]string
}

// PluginConfig is the wasm plugin configuration.
type PluginConfig struct {
	// providerConfigs is the decision-services catalogue (each entry needs
	// an explicit endpoint — there is no hosted default; an empty
	// catalogue leaves the feature inert until a matchRule provides one).
	providerConfigs []*provider.ProviderConfig
	// activeProviderId selects the decision service in effect.
	activeProviderId string
	// decision is the resolved decision service.
	decision provider.DecisionProvider
	// decisionTokens is the resolved credential list of the active
	// decision provider: tried in order, with the callout retried on
	// 401/403/429 (invalid/quota-exhausted key). Empty = anonymous.
	decisionTokens []string
	// tokenStateKey is the shared-data key recording when each token last
	// failed (cooldown state), namespaced per provider.
	tokenStateKey string
	// tokenCooldownMs: a token that failed with 401/403/429 is skipped
	// for this long before being tried again; 0 disables cooldown.
	tokenCooldownMs int64
	// decisionModel overrides the provider's decision-engine model for
	// this config scope (set globally or per route in matchRules); empty
	// falls back to the provider's `model`, then to omitting the field.
	decisionModel string
	// modelSelection, when configured (per route), drives the jev decision.
	modelSelection *ModelSelectionConfig
	// modelSelectionWarning is set when modelSelection was configured but
	// skipped (e.g. < 2 criteria); logged at request time.
	modelSelectionWarning string
	// decisionTimeoutMs bounds the jev callout.
	decisionTimeoutMs uint32
	// maxStateBytes caps the task body shipped as `state`.
	maxStateBytes int
	// rankWeight is this plugin's vote multiplier in the finisher's
	// weighted-sum combination. The default (10) makes the model decision
	// dominant over soft opinions like least-load, while those still break
	// ties between instances of the same model.
	rankWeight float64
	// maxBodyBytes caps the buffered task body (Envoy buffer limit).
	maxBodyBytes uint32
}

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseOverrideConfig(parseGlobalConfig, parseOverrideRuleConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		// WithRebuildAfterRequests is deliberately not set: requests inside
		// the rebuild window get a 503, and this plugin sits on the LB
		// decision path — matching the context/finisher and the other
		// capability plugins. It holds no cross-request state (bodies are
		// buffered per request and released with the stream), so there is
		// no source of memory growth to reclaim with a rebuild.
	)
}

// parseGlobalConfig parses defaultConfig. Everything is optional except the
// per-route modelSelection: the typesafe decision service works with zero
// configuration (sensible defaults), and the candidates come from the
// gpustack-lb context role at runtime, not from this plugin's config.
func parseGlobalConfig(json gjson.Result, config *PluginConfig) error {
	return parseConfigInto(json, config, true)
}

// parseOverrideRuleConfig merges a matchRule config over the global one —
// typically the rule carries only modelSelection (per-route question).
func parseOverrideRuleConfig(json gjson.Result, global PluginConfig, pluginConfig *PluginConfig) error {
	*pluginConfig = global
	return parseConfigInto(json, pluginConfig, false)
}

func parseConfigInto(json gjson.Result, config *PluginConfig, fromGlobal bool) error {
	// ---- decision services (optional; explicit target required) ----
	providers := json.Get("providers")
	pj := json.Get("provider")
	if providers.IsArray() {
		config.providerConfigs = nil
		for _, entry := range providers.Array() {
			config.providerConfigs = append(config.providerConfigs, parseProviderConfig(entry))
		}
	} else if pj.IsObject() {
		// Single-provider form (global or matchRule override).
		config.providerConfigs = []*provider.ProviderConfig{parseProviderConfig(pj)}
	}
	if activeID := json.Get("activeProviderId").String(); activeID != "" {
		config.activeProviderId = activeID
	}
	// A matchRule that overrides the provider catalogue replaces it
	// wholesale, so an INHERITED global activeProviderId no longer applies —
	// drop it instead of failing the rule on a dangling reference. A
	// rule-level activeProviderId (set just above) is explicit intent and
	// must survive: only clear what the rule did not provide itself.
	// The copied global `decision` is dropped too: a rule override that
	// ends up with no resolvable provider (e.g. providers: []) must leave
	// the feature inert, NOT keep routing task bodies to the global
	// decision service.
	if !fromGlobal && !json.Get("activeProviderId").Exists() && (providers.IsArray() || pj.IsObject()) {
		config.activeProviderId = ""
	}
	if !fromGlobal && (providers.IsArray() || pj.IsObject()) {
		config.decision = nil
		// The inherited credential list (and its cooldown state key)
		// belongs to the dropped global provider; keep it consistent
		// with decision == nil.
		config.decisionTokens = nil
		config.tokenStateKey = ""
	}
	var provCfg *provider.ProviderConfig
	for _, pc := range config.providerConfigs {
		if pc.Id == config.activeProviderId {
			provCfg = pc
			break
		}
	}
	if provCfg == nil && config.activeProviderId != "" {
		return fmt.Errorf("activeProviderId %q not found in providers", config.activeProviderId)
	}
	if provCfg == nil && len(config.providerConfigs) == 1 {
		provCfg = config.providerConfigs[0]
	}
	if provCfg == nil {
		// No decision service configured at all (typical for an empty
		// global defaultConfig with everything set per route): the feature
		// stays inert — config.decision == nil is handled at request time.
		// There is deliberately NO hosted default: the callout ships task
		// bodies to the service, so the target must be configured
		// explicitly (privacy).
	} else {
		dec, err := provider.CreateProvider(provCfg)
		if err != nil {
			return err
		}
		config.decision = dec
		config.decisionTokens = provCfg.EffectiveAPITokens()
		// Cooldown state is namespaced per provider ENTRY. id alone is not
		// a collision-free identity (it is optional — two id-less
		// systemone providers with different endpoints would share a map,
		// so a failure on one route could suppress a shared token on the
		// other), so the key folds in every identity-bearing field:
		// type, id, endpoint and cluster.
		config.tokenStateKey = tokenUnavailableSinceKeyPrefix + provCfg.Type + "-" + provCfg.Id + "-" + provCfg.Endpoint + "-" + provCfg.Cluster
		if fromGlobal {
			// A global config (re)load means the token list may have
			// changed: reset stale cooldown state, matching ai-proxy's
			// resetSharedData on config update. Rule overrides inherit
			// the global key and must NOT reset it mid-flight.
			resetSharedDataSafe(config.tokenStateKey)
		}
	}

	// ---- modelSelection (per route) ----
	// An object with a required `criteria` map (model name -> capability
	// description): the descriptions are what jev reasons over, so
	// name-only options are deliberately not supported — a bare `true`
	// would degrade decision quality below usefulness. `instructions` is
	// optional (default text applies). Criteria keys that match no
	// published candidate are inert; candidates without a description
	// fall back to a name-only option (better than disabling the route).
	if ms := json.Get("modelSelection"); ms.Exists() {
		if !ms.IsObject() {
			return fmt.Errorf("modelSelection must be an object with a criteria map; name-only selection is not supported")
		}
		if !ms.Get("criteria").Exists() {
			return fmt.Errorf("modelSelection.criteria is required (model name -> capability description); name-only selection is not supported")
		}
		if criteria := ms.Get("criteria"); !criteria.IsObject() {
			return fmt.Errorf("modelSelection.criteria must be a JSON object (model name -> capability description), got %s", criteria.Type)
		}
		mc := &ModelSelectionConfig{
			Instructions: defaultModelSelectionInstructions,
			Criteria:     map[string]string{},
		}
		if inst := ms.Get("instructions"); inst.Exists() && inst.String() != "" {
			mc.Instructions = inst.String()
		}
		for name, desc := range ms.Get("criteria").Map() {
			mc.Criteria[name] = desc.String()
		}
		if len(mc.Criteria) < 2 {
			// A single-model route has nothing to select: skip the
			// feature (no opinion published, nothing sent to jev)
			// instead of failing the whole config. The warning is
			// recorded and logged at request time (proxywasm logging
			// is unavailable during config parsing in unit tests).
			config.modelSelection = nil
			config.modelSelectionWarning = fmt.Sprintf(
				"modelSelection.criteria has %d entries (< 2); nothing to select — skipping model selection for this config",
				len(mc.Criteria))
		} else {
			config.modelSelection = mc
		}
	}

	// ---- decisionModel (route-level engine override) ----
	if m := json.Get("decisionModel"); m.Exists() {
		config.decisionModel = m.String()
	}

	// ---- decision knobs ----
	if v := json.Get("decisionTimeoutMs").Uint(); v > 0 {
		config.decisionTimeoutMs = uint32(v)
	} else if fromGlobal {
		config.decisionTimeoutMs = defaultDecisionTimeoutMs
	}
	if v := json.Get("maxStateBytes").Uint(); v > 0 {
		config.maxStateBytes = int(v)
	} else if fromGlobal {
		config.maxStateBytes = defaultMaxStateBytes
	}
	if v := json.Get("rankWeight"); v.Exists() {
		w := v.Float()
		if w <= 0 {
			return fmt.Errorf("rankWeight must be > 0 (got %v); a non-positive vote weight would invert or mute the model-selection opinion in the finisher's weighted sum", w)
		}
		config.rankWeight = w
	} else if fromGlobal {
		config.rankWeight = defaultRankWeight
	}
	if v := json.Get("maxBodyBytes").Uint(); v > 0 {
		config.maxBodyBytes = uint32(v)
	} else if fromGlobal {
		config.maxBodyBytes = defaultMaxBodyBytes
	}
	// tokenCooldownMs (ai-proxy cooldownDuration semantics): a 401/403/429
	// token is skipped this long before being retried. 0 disables the
	// cooldown; the default keeps a long-dead key from costing every
	// request one wasted callout.
	if json.Get("tokenCooldownMs").Exists() {
		config.tokenCooldownMs = json.Get("tokenCooldownMs").Int()
	} else if fromGlobal {
		config.tokenCooldownMs = defaultTokenCooldownMs
	}
	return nil
}

func parseProviderConfig(pj gjson.Result) *provider.ProviderConfig {
	pc := &provider.ProviderConfig{
		Id:       pj.Get("id").String(),
		Type:     pj.Get("type").String(),
		Endpoint: pj.Get("endpoint").String(),
		APIToken: pj.Get("apiToken").String(),
		Cluster:  pj.Get("cluster").String(),
		Model:    pj.Get("model").String(),
	}
	if ts := pj.Get("apiTokens"); ts.IsArray() {
		// Always materialise the slice when the field is present —
		// even empty — so EffectiveAPITokens can distinguish "explicit
		// empty list = anonymous" from "field absent = legacy apiToken".
		pc.APITokens = []string{}
		for _, t := range ts.Array() {
			if s := t.String(); s != "" {
				pc.APITokens = append(pc.APITokens, s)
			}
		}
	}
	if pc.Type == "" {
		pc.Type = provider.TypeSystemone
	}
	return pc
}

// readCandidates reads the candidate set published by the gpustack-lb
// context role from the filter state (the shared wire contract).
func readCandidates() ([]wire.Candidate, bool) {
	data, err := proxywasm.GetProperty([]string{wire.FilterStateCandidates})
	if err != nil || len(data) == 0 {
		return nil, false
	}
	var set wire.CandidateSet
	if err := json.Unmarshal(data, &set); err != nil {
		proxywasm.LogWarnf("%s: unparseable candidate set: %v", pluginName, err)
		return nil, false
	}
	return set.Candidates, true
}

// isWeighted mirrors the capability plugins' rule: a weighted set carries a
// business traffic split, which a model-based decision must not override.
func isWeighted(cands []wire.Candidate) bool {
	for i := range cands {
		if cands[i].Weight != nil {
			return true
		}
	}
	return false
}

// effectiveDecisionModel resolves which decision engine answers: a
// route-level `decisionModel` override wins, then the provider's `model`,
// then "" (omit the field — server default).
func effectiveDecisionModel(config PluginConfig) string {
	if config.decisionModel != "" {
		return config.decisionModel
	}
	if config.decision == nil {
		return ""
	}
	return config.decision.DecisionModel()
}

// onHttpRequestHeaders arms the router: internal redirects and weighted
// sets are left alone; otherwise the body is buffered for the decision.
func onHttpRequestHeaders(ctx wrapper.HttpContext, config PluginConfig) types.Action {
	if v, err := proxywasm.GetHttpRequestHeader(headerFallbackFrom); err == nil && v != "" {
		// Non-empty value only, matching the context role / finisher's
		// internal-redirect signal: an explicitly empty header is not a
		// redirect marker and must not bypass this decision stage.
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if config.modelSelection == nil {
		// No question configured on this route: nothing to ask jev. If it
		// was configured but skipped (bad shape), surface the warning.
		if config.modelSelectionWarning != "" {
			proxywasm.LogWarnf("%s: %s", pluginName, config.modelSelectionWarning)
		}
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if config.decision == nil {
		// A question is configured but no decision service is: never send
		// task bodies anywhere by implicit default — stay inert.
		proxywasm.LogWarnf("%s: modelSelection configured but no decision service (provider/providers) is; skipping model selection", pluginName)
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	cands, ok := readCandidates()
	if !ok || len(cands) == 0 || isWeighted(cands) {
		// No published candidates (non-LB route) or a business split: the
		// plugin degrades to a pass-through.
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	// Fewer than two unique candidate models: there is no model-selection
	// decision to make — skip the body buffering and the callout entirely
	// (buildDecisionRequest would reject the question anyway; exiting here
	// avoids paying for the buffered body and the WARN log per request).
	uniqueModels := 0
	seen := map[string]struct{}{}
	for i := range cands {
		if cands[i].ModelName == "" {
			continue
		}
		if _, dup := seen[cands[i].ModelName]; !dup {
			seen[cands[i].ModelName] = struct{}{}
			uniqueModels++
		}
	}
	if uniqueModels < 2 {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	if !ctx.HasRequestBody() {
		ctx.DontReadRequestBody()
		return types.ActionContinue
	}
	ctx.SetContext(ctxArmed, true)
	ctx.SetRequestBodyBufferLimit(config.maxBodyBytes)
	return types.HeaderStopIteration
}

// onHttpRequestBody composes the decision request, dispatches it to the jev
// service, and pauses until the verdict arrives.
func onHttpRequestBody(ctx wrapper.HttpContext, config PluginConfig, body []byte) types.Action {
	armed, _ := ctx.GetContext(ctxArmed).(bool)
	if !armed || len(body) == 0 {
		return types.ActionContinue
	}
	cands, ok := readCandidates()
	if !ok || len(cands) == 0 {
		return types.ActionContinue
	}

	decisionReq := buildDecisionRequest(body, config.modelSelection, cands, config.maxStateBytes, effectiveDecisionModel(config))
	if decisionReq == nil {
		proxywasm.LogWarnf("%s: failed to compose decision request", pluginName)
		return types.ActionContinue
	}
	if err := config.decision.ValidateDecisionRequest(decisionReq); err != nil {
		proxywasm.LogWarnf("%s: composed decision request invalid: %v", pluginName, err)
		return types.ActionContinue
	}

	calloutHeaders := [][2]string{
		{":method", "POST"},
		{":path", config.decision.DecisionPath()},
		{":authority", config.decision.DecisionHost()},
		{"content-type", "application/json"},
	}

	// Token failover: the provider's credential list is tried in order.
	// The first token serves the traffic; on an auth/quota failure
	// (401/403/429) the callout is retried with the next token (the
	// request stays paused throughout), and only after the last token
	// fails does the plugin degrade as usual (no rank entry published).
	// Tokens still in cooldown (recent 401/403/429) are skipped up front
	// so a long-dead key costs no wasted callout.
	tokens := activeTokens(config)
	if err := dispatchDecisionCallout(config, calloutHeaders, decisionReq, cands, tokens, 0); err != nil {
		// Degrade: no header written, the rest of the band routes normally.
		proxywasm.LogWarnf("%s: dispatch decision call failed: %v", pluginName, err)
		return types.ActionContinue
	}
	return types.ActionPause
}

// tokenFailureStatuses are the HTTP statuses treated as "this credential
// is unusable" for the decision callout: 401/403 (invalid or forbidden
// key) and 429 (quota/rate exhausted — the classic fallback-key case).
// Everything else (5xx, timeouts) is a service-side problem another key
// would hit too, so no token retry is spent on it.
var tokenFailureStatuses = map[string]bool{"401": true, "403": true, "429": true}

// dispatchDecisionCallout sends the decision request with tokens[idx] and,
// when that attempt fails on an auth/quota status and tokens remain,
// recurses with the next one. It returns the DispatchHttpCall error of the
// FIRST attempt only (the caller maps it to ActionContinue); a retry-level
// dispatch failure resumes the request here, since the stream is already
// paused by then.
func dispatchDecisionCallout(config PluginConfig, baseHeaders [][2]string, decisionReq []byte, cands []wire.Candidate, tokens []string, idx int) error {
	token := ""
	if idx < len(tokens) {
		token = tokens[idx]
	}
	// Copy the base headers: appending auth headers must not leak into the
	// shared slice later retries also build on.
	calloutHeaders := append([][2]string{}, baseHeaders...)
	if token != "" {
		hs := http.Header{}
		config.decision.AuthHeaders(hs, token)
		for k, vals := range hs {
			for _, v := range vals {
				calloutHeaders = append(calloutHeaders, [2]string{k, v})
			}
		}
	}
	_, err := proxywasm.DispatchHttpCall(
		config.decision.DecisionCluster(),
		calloutHeaders,
		decisionReq,
		nil,
		config.decisionTimeoutMs,
		func(numHeaders, bodySize, numTrailers int) {
			if status := calloutStatus(); tokenFailureStatuses[status] {
				markTokenUnavailable(config, token)
				if idx+1 < len(tokens) {
					proxywasm.LogWarnf("%s: decision service returned HTTP %s for token #%d; retrying with fallback token #%d of %d",
						pluginName, status, idx+1, idx+2, len(tokens))
					if err := dispatchDecisionCallout(config, baseHeaders, decisionReq, cands, tokens, idx+1); err != nil {
						proxywasm.LogWarnf("%s: retry dispatch failed: %v", pluginName, err)
						proxywasm.ResumeHttpRequest()
					}
					return
				}
			}
			applyVerdict(readAnswer(bodySize), cands, config.rankWeight)
		},
	)
	return err
}

// calloutStatus reads the :status of the decision callout response ("" when
// unavailable) so the failover logic can classify the failure without
// consuming the body.
func calloutStatus() string {
	if hs, err := proxywasm.GetHttpCallResponseHeaders(); err == nil {
		for _, h := range hs {
			if h[0] == ":status" {
				return h[1]
			}
		}
	}
	return ""
}

// activeTokens filters the provider's credential list down to tokens NOT in
// cooldown — i.e. keys that did not fail with 401/403/429 within the last
// tokenCooldownMs (ai-proxy's cooldownDuration recovery path, minus its
// health checks: the token simply becomes eligible again once the window
// passes and proves itself on the next real callout). Order is preserved.
// Fail-open on every degenerate case: no cooldown configured, no state key
// (single/anonymous provider), unreadable state, or ALL tokens cooling (a
// total outage must not turn into "no tokens" — the request would then skip
// the decision entirely instead of trying its luck with a recovering key).
func activeTokens(config PluginConfig) []string {
	if config.tokenCooldownMs <= 0 || config.tokenStateKey == "" || len(config.decisionTokens) <= 1 {
		return config.decisionTokens
	}
	state := readTokenState(config.tokenStateKey)
	live := filterCooledTokens(config.decisionTokens, state, currentTimeMillis(), config.tokenCooldownMs)
	if len(live) == 0 {
		proxywasm.LogWarnf("%s: all decision tokens in cooldown; trying them anyway (fail-open)", pluginName)
		return config.decisionTokens
	}
	return live
}

// filterCooledTokens is the pure core of the cooldown filter: tokens whose
// last recorded failure is still within cooldownMs of now are dropped,
// order is preserved.
func filterCooledTokens(tokens []string, state map[string]int64, now, cooldownMs int64) []string {
	var live []string
	for _, t := range tokens {
		if since, ok := state[t]; !ok || now-since >= cooldownMs {
			live = append(live, t)
		}
	}
	return live
}

// markTokenUnavailable records that token just failed a callout with
// 401/403/429, starting (or restarting) its cooldown window. The
// read-merge-write is guarded by the shared-data CAS: two VMs failing
// different keys simultaneously would otherwise each write a map holding
// only its own update and lose the other's cooldown (CAS 0 is an
// unconditional overwrite in the host), so the write retries on
// ErrorStatusCasMismatch the way ai-proxy's failover.go does.
func markTokenUnavailable(config PluginConfig, token string) {
	if token == "" || config.tokenCooldownMs <= 0 || config.tokenStateKey == "" {
		return
	}
	for attempt := 1; attempt <= tokenStateCasMaxRetries; attempt++ {
		state, cas := readTokenStateCAS(config.tokenStateKey)
		now := currentTimeMillis()
		state[token] = now
		// Prune entries that no longer belong: expired cooldowns and
		// tokens removed from the config, so the map never grows
		// unboundedly.
		configured := map[string]bool{}
		for _, t := range config.decisionTokens {
			configured[t] = true
		}
		for t, since := range state {
			if !configured[t] || now-since >= config.tokenCooldownMs {
				delete(state, t)
			}
		}
		data, err := json.Marshal(state)
		if err != nil {
			return
		}
		err = proxywasm.SetSharedData(config.tokenStateKey, data, cas)
		if err == nil {
			return
		}
		if !errors.Is(err, types.ErrorStatusCasMismatch) {
			proxywasm.LogWarnf("%s: failed to record token cooldown: %v", pluginName, err)
			return
		}
		// CAS mismatch: another worker wrote first — re-read, re-merge,
		// retry. Exhausting the retries only delays one cooldown entry.
	}
	proxywasm.LogWarnf("%s: token cooldown state kept racing after %d attempts; giving up this update", pluginName, tokenStateCasMaxRetries)
}

// tokenStateCasMaxRetries bounds the read-merge-write retry loop (same
// constant ai-proxy uses for its shared-data CAS loops).
const tokenStateCasMaxRetries = 10

// readTokenStateCAS reads the cooldown map together with its CAS token; a
// missing/unreadable state yields an empty map and the CAS the host handed
// back (0 for a fresh key, which the host treats as an unconditional set).
func readTokenStateCAS(key string) (map[string]int64, uint32) {
	data, cas, err := proxywasm.GetSharedData(key)
	if err != nil || len(data) == 0 {
		return map[string]int64{}, cas
	}
	var state map[string]int64
	if err := json.Unmarshal(data, &state); err != nil {
		return map[string]int64{}, cas
	}
	return state, cas
}

// readTokenState reads the cooldown map (token -> failed-at ms); any error
// or empty value yields nil (treated as "nothing in cooldown").
func readTokenState(key string) map[string]int64 {
	data, _, err := proxywasm.GetSharedData(key)
	if err != nil || len(data) == 0 {
		return nil
	}
	var state map[string]int64
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	return state
}

// resetSharedDataSafe clears a shared-data key, recovering from the SDK
// mock's panic when no proxy-wasm host is attached (unit tests): a failed
// reset only leaves a stale cooldown timestamp behind, never a wrong route.
func resetSharedDataSafe(key string) {
	defer func() { _ = recover() }()
	_ = proxywasm.SetSharedData(key, nil, 0)
}

// currentTimeMillis returns the current time in milliseconds for the
// cooldown windows. The higress proxy-wasm-go-sdk fork exports no time
// hostcall (no GetSystemTimeNanoSeconds / GetCurrentTimeNanoseconds), so
// time.Now() is the only source — and it is what the SDK itself
// (internal logTiming) and ai-proxy's production token-cooldown
// (failover.go) already rely on: higress wasm builds link a WASI clock.
// Keeping a single helper means switching to a hostcall later, should the
// SDK grow one, is a one-line change.
func currentTimeMillis() int64 {
	return time.Now().UnixMilli()
}

// applyVerdict turns the jev answer into a ranking opinion appended to the
// shared FilterStateRanks array — the framework-native association:
//
//   - criteria keys (and the answer's probabilities) are keyed by the
//     published candidates' ModelName
//   - the score SHAPE comes from the full probability distribution: every
//     instance of a model receives p(model) itself — NOT p/n. Splitting p
//     across instances would let the replica count invert the model-level
//     ordering (2 replicas at p=0.6 losing to 1 replica at p=0.4); with
//     per-instance p, the finisher's shared L1 normalisation keeps the
//     model ordering equal to p's ordering, while instances of the same
//     model stay exactly tied (instance choice remains with least-load /
//     session-affinity)
//   - CONFIDENCE modulates the vote's Weight, not the scores: the finisher
//     L1-normalises every entry (total = Weight × score/Σscores), so
//     scaling the scores themselves to sum to confidence would be erased
//     by that normalisation. Encoding it as Weight = rankWeight ×
//     confidence preserves the intent — sum(effect) = confidence: a
//     high-confidence verdict dominates the routing, a low-confidence one
//     yields influence to the other opinions (least-load, session)
//
// An answer that matches no published candidate appends nothing; the
// finisher routes as usual.
func applyVerdict(ans jevAnswer, cands []wire.Candidate, rankWeight float64) {
	if len(ans.Probabilities) == 0 {
		if ans.Choice == "" {
			proxywasm.LogWarnf("%s: empty answer; publishing no rank", pluginName)
		} else {
			proxywasm.LogWarnf("%s: answer %q carries no probabilities; publishing no rank", pluginName, ans.Choice)
		}
		proxywasm.ResumeHttpRequest()
		return
	}
	scores, matched := verdictScores(cands, ans.Probabilities)
	if !matched {
		proxywasm.LogWarnf("%s: answer matches no published candidate; publishing no rank", pluginName)
		proxywasm.ResumeHttpRequest()
		return
	}
	// Debug-level trace of the verdict: the per-instance scores (score
	// shape) and the confidence, i.e. exactly what this plugin contributes
	// to the finisher's weighted sum.
	proxywasm.LogDebugf("%s: model-selection verdict: choice=%q confidence=%.4f rankWeight=%v effectiveWeight=%.4f scores=%v",
		pluginName, ans.Choice, ans.Confidence, rankWeight, rankWeight*ans.Confidence, scores)
	appendRank(wire.RankEntry{
		Name:   pluginName,
		Weight: rankWeight * ans.Confidence,
		Scores: scores,
	})
	proxywasm.ResumeHttpRequest()
}

// appendRank appends this plugin's opinion to the shared ranking array.
// Read-modify-write is safe: the filter chain for a single request runs
// serially on one worker thread (same reasoning as the other capability
// plugins).
func appendRank(entry wire.RankEntry) {
	var ranks []wire.RankEntry
	if data, err := proxywasm.GetProperty([]string{wire.FilterStateRanks}); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &ranks)
	}
	ranks = append(ranks, entry)
	if data, err := json.Marshal(ranks); err == nil {
		_ = proxywasm.SetProperty([]string{wire.FilterStateRanks}, data)
	}
}

// jevAnswer is the parsed model_selection choice answer: the full
// probability distribution plus confidence, not just the argmax.
type jevAnswer struct {
	Choice        string
	Probabilities map[string]float64
	Confidence    float64
}

// readAnswer parses answers.model_selection from the callout response. A
// non-2xx status yields an empty answer: an error body must never be
// interpreted as a verdict (it would either misroute or, worse, look valid).
func readAnswer(bodySize int) jevAnswer {
	if hs, err := proxywasm.GetHttpCallResponseHeaders(); err == nil {
		for _, h := range hs {
			if h[0] == ":status" && (len(h[1]) < 2 || h[1][0] != '2') {
				proxywasm.LogWarnf("%s: decision service returned HTTP %s; publishing no rank", pluginName, h[1])
				return jevAnswer{}
			}
		}
	}
	size := bodySize
	if size <= 0 || size > calloutBodyReadCap {
		size = calloutBodyReadCap
	}
	body, err := proxywasm.GetHttpCallResponseBody(0, size)
	if err != nil || len(body) == 0 {
		return jevAnswer{}
	}
	a := gjson.GetBytes(body, "answers."+modelSelectionQuestionID)
	ans := jevAnswer{
		Choice:        a.Get("choice").String(),
		Probabilities: map[string]float64{},
	}
	// jevcompat: confidence is a number in [0, 1]. Missing, zero, negative
	// or out-of-range values are a malformed answer — publishing no rank
	// (fail-open) beats inventing confidence: defaulting to 1 would turn a
	// zero-confidence or broken response into a full-strength vote.
	conf := a.Get("confidence")
	if !conf.Exists() {
		proxywasm.LogWarnf("%s: answer carries no confidence; publishing no rank", pluginName)
		return jevAnswer{}
	}
	ans.Confidence = conf.Float()
	if ans.Confidence <= 0 || ans.Confidence > 1 {
		proxywasm.LogWarnf("%s: answer confidence %v outside (0, 1]; publishing no rank", pluginName, ans.Confidence)
		return jevAnswer{}
	}
	for opt, p := range a.Get("probabilities").Map() {
		ans.Probabilities[opt] = p.Float()
	}
	return ans
}

// pickCandidate reports whether any published candidate carries the model.
func pickCandidate(cands []wire.Candidate, model string) bool {
	if model == "" {
		return false
	}
	for i := range cands {
		if cands[i].ModelName == model {
			return true
		}
	}
	return false
}

// verdictScores maps the jev probability distribution onto candidate
// INSTANCES, keyed by Candidate.Key(): every instance of a model receives
// p(model) itself — NOT p/n. Splitting p across a model's instances would
// put the replica count into the model-level ordering (a 2-replica model
// with p=0.6 would lose to a 1-replica model with p=0.4 after the
// finisher's per-instance argmax); scoring each instance with p keeps the
// model ordering identical to p's ordering under any replica asymmetry,
// while instances of the same model remain exactly tied (instance-level
// choice stays with the other capability opinions). The finisher's
// per-entry L1 normalisation divides all instances by the shared
// Σ p(M)×n(M), which preserves that ordering.
// The second return reports whether any candidate matched at all.
func verdictScores(cands []wire.Candidate, probabilities map[string]float64) (map[string]float64, bool) {
	scores := map[string]float64{}
	matched := false
	for i := range cands {
		p, ok := probabilities[cands[i].ModelName]
		if !ok || p <= 0 {
			continue
		}
		scores[cands[i].Key()] = p
		matched = true
	}
	return scores, matched
}

// buildDecisionRequest composes the jev request: the task body becomes the
// state (capped by maxStateBytes) and the model_selection question's options
// are the published candidates' models, described by the configured criteria
// (falling back to the model name itself). decisionModel, when set, is sent
// as the request's optional `model` field (spec request.model-alias) to pick
// the decision engine on the server side.
func buildDecisionRequest(taskBody []byte, ms *ModelSelectionConfig, cands []wire.Candidate, maxStateBytes int, decisionModel string) []byte {
	if ms == nil || ms.Criteria == nil {
		// Defensive: callers always pass a parsed modelSelection, but a
		// nil would panic below — degrade instead.
		return nil
	}
	criteria := map[string]interface{}{}
	for i := range cands {
		model := cands[i].ModelName
		if model == "" {
			continue
		}
		if desc, ok := ms.Criteria[model]; ok && desc != "" {
			criteria[model] = desc
		} else {
			criteria[model] = nil
		}
	}
	if len(criteria) < 2 {
		return nil
	}
	state := taskBody
	if maxStateBytes > 0 && len(state) > maxStateBytes {
		state = truncateUTF8(state, maxStateBytes)
	}
	req, err := sjson.SetBytes(nil, "state", string(state))
	if err != nil {
		return nil
	}
	if decisionModel != "" {
		if req, err = sjson.SetBytes(req, "model", decisionModel); err != nil {
			return nil
		}
	}
	question := map[string]interface{}{
		"type":         "choice",
		"instructions": ms.Instructions,
		"criteria":     criteria,
	}
	req, err = sjson.SetBytes(req, "questions."+modelSelectionQuestionID, question)
	if err != nil {
		return nil
	}
	return req
}

// truncateUTF8 cuts b to at most max bytes, backing off over any trailing
// partial multi-byte sequence so the cut always lands on a valid UTF-8
// rune boundary (a mid-rune cut would hand the decision service malformed
// UTF-8 in `state`). DecodeLastRune inspects only the trailing bytes, so
// each backoff is O(1) and the loop runs at most 3 times (max rune size).
func truncateUTF8(b []byte, max int) []byte {
	if max < 0 {
		return nil
	}
	if max >= len(b) {
		return b
	}
	b = b[:max]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r == utf8.RuneError && size == 1 {
			b = b[:len(b)-1]
		} else {
			break
		}
	}
	return b
}
