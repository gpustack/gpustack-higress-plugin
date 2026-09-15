package main

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// defaultEnabled: installing this CR already means you want it, so it defaults
// to on.
//
// Turning it off for one route can be written two ways, depending on which
// shape you want:
//
//	on globally, off for a few:  defaultConfig: {}              + enabled: false in the matchRule
//	off globally, on for a few:  defaultConfig: {enabled:false} + enabled: true  in the matchRule
//
// The latter can also be had with defaultConfigDisable: true on the CR, but
// that is a **deployment-level** switch (the plugin does not run at all on
// routes not listed in matchRules) whereas enabled is a **config-level** one --
// a single matchRule can list many ingresses, and flipping one boolean reviews
// more easily than adding and removing entries. The two stack; either one off
// means off.
const defaultEnabled = true

// defaultWeight: this entry's vote multiplier, 1 by default.
//
// After the finisher's L1 normalisation every entry casts a total of one vote,
// and weight is the multiplier on it. 0 is not a meaningful value ("do not
// participate" is expressed with enabled), so this is a bare float64 rather
// than a pointer (§9 rule 5).
const defaultWeight = 1.0

// Config has just one switch and one weight.
//
// **The scoring function itself has no parameters at all.** That is a
// conclusion, not a gap -- going through the things one might want to
// configure, every one of them has a better home:
//
//	what counts as "load"   Inflight + Penalty is a definition, not a preference;
//	                        a knob here would just permit a wrong definition
//	penalty decay window    lives in the finisher (rampMs), which is the writer
//	                        that does the ejecting
//	capacity ceiling        lives on the candidate (maxRunningRequests); that is
//	                        a topology fact
//	tie handling            random, to avoid a thundering herd; a correctness
//	                        requirement, not a matter of taste
//	the scoring transform   it carries meaning (see score.go) and should not be
//	                        changed from config -- to tune "how much say this
//	                        entry has", use weight, which is the knob designed
//	                        for exactly that
//
// So this plugin is a pure function of the published candidate set, and weight
// only affects its share of the finisher's weighted sum, not the function
// itself.
type Config struct {
	// weight is this entry's vote multiplier in the weighted sum, published
	// along with the ranking opinion.
	//
	// It is the **only config that affects the scoring outcome**: the scoring
	// function itself still has no parameters.
	weight float64

	// enabled is a pointer, nil = unset. This is not fastidiousness: it is
	// there to tell "not configured" apart from "explicitly configured false"
	// -- the two must behave differently under matchRules inheritance, see the
	// comment on parseOverrideConfig. Always read it through shouldEnable().
	enabled *bool
}

// shouldEnable is the only read entry point. nil (unset) is treated as the
// default, on.
func (c Config) shouldEnable() bool {
	if c.enabled == nil {
		return defaultEnabled
	}
	return *c.enabled
}

func parseConfig(j gjson.Result, config *Config) error {
	// **Reset the destination first.** rule_matcher re-runs the parse on the
	// *same* &rule.config when the first attempt errors, retrying with a backup
	// JSON (rule_matcher.go:251 then :265). enabled is written only when the
	// field is present, so a previous `enabled: false` would survive into a
	// retry whose JSON omits it -- and then neither the default nor the
	// inherited global value can take effect, because parseOverrideConfig only
	// inherits when the pointer is nil. The route stays off with nothing saying
	// why.
	*config = Config{}

	// **Strict boolean.** gjson's Bool() coerces: a string that is not
	// "true"/"false", the number 0, and a null literal all come back as false.
	// Here false is a meaningful setting rather than a default, so a typo like
	// `enabled: "yes"` would not fall back to the default -- it would read as a
	// deliberate disable, silently dropping the load opinion and leaving the
	// route on round-robin. That is the same symptom the pointer on enabled
	// exists to prevent, arriving by a different route.
	//
	// null is treated as unset rather than rejected, matching how `candidates`
	// handles it: gjson reports Exists() for a null literal.
	if e := j.Get("enabled"); e.Exists() && e.Type != gjson.Null {
		if e.Type != gjson.True && e.Type != gjson.False {
			return fmt.Errorf("enabled must be a boolean, got %s", e.Raw)
		}
		v := e.Type == gjson.True
		config.enabled = &v
	}
	config.weight = defaultWeight
	if w := j.Get("weight"); w.Exists() && w.Float() > 0 {
		config.weight = w.Float()
	}
	return nil
}

// parseOverrideConfig lets matchRules inherit enabled from defaultConfig.
//
// ⚠️ When the global config fails to parse, rule_matcher **swallows the error
// and leaves m.globalConfig at its zero value** (rule_matcher.go:198-206), so
// the global handed in here is zero-valued. That is exactly why enabled has to
// be a pointer: a zero-valued global has enabled == nil rather than false, the
// nil is inherited as-is, and the reader treats it as the default true. With a
// bare bool, one unrelated typo in the global config would silently turn load
// awareness off on **every route** -- degrading to round-robin with no error
// anywhere, only the kind of "hmm, this looks unbalanced" symptom you notice
// long after the fact.
//
// There is no wholesale `*config = global` copy here; every rule is parsed from
// scratch and only enabled is copied across. Config has just that one field, so
// a wholesale copy would be equivalent today; writing the copy out explicitly
// is what keeps a future field from being inherited by default
// (gpustack-rate-limit hit the slice aliasing trap, see CLAUDE.md).
func parseOverrideConfig(j gjson.Result, global Config, config *Config) error {
	if err := parseConfig(j, config); err != nil {
		return err
	}
	if config.enabled == nil {
		config.enabled = global.enabled
	}
	// With no explicit weight, inherit the global value (when global is
	// zero-valued, parseConfig has already filled in the default of 1).
	if !j.Get("weight").Exists() && global.weight > 0 {
		config.weight = global.weight
	}
	return nil
}
