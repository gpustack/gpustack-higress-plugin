// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Configuration for the tool / tool_calls pairing check. See gpustack/gpustack#6210.

package config

import "fmt"

// ToolCallValidationMode selects what the plugin does about the structural
// tool / tool_calls pairing check on Chat Completions requests.
//
// A string enum rather than a boolean even though there are only two values
// today: `toolCallValidation: strict` is self-describing in a WasmPlugin
// manifest in a way `enforceToolCallValidation: true` is not, and adding a
// third mode later (a log-and-forward `warn`, say) stays a non-breaking config
// change.
type ToolCallValidationMode string

const (
	// ToolCallValidationOff skips the check entirely -- no walk, no cost.
	ToolCallValidationOff ToolCallValidationMode = "off"
	// ToolCallValidationStrict rejects a malformed request with 400. A
	// permissive upstream answering a dangling tool_calls request with a
	// fabricated tool result is a silent correctness failure the caller cannot
	// detect, so a deployment that wants that safety net asks for this.
	ToolCallValidationStrict ToolCallValidationMode = "strict"

	// ToolCallValidationDefault is what an unset (or unrecognised) value
	// resolves to.
	//
	// Off. This plugin is a vendored fork of upstream ai-proxy and ships to
	// every GPUStack deployment; a default that rejected traffic would make the
	// fork unilaterally turn requests that work today into 400s fleet-wide, to
	// serve the deployments that actually asked for the check. Opting in is
	// their single config line either way.
	ToolCallValidationDefault = ToolCallValidationOff
)

// normalizeToolCallValidationMode canonicalises the configured spelling and
// falls back to the default on an unrecognised one, reporting the fallback so
// the caller can log it. The empty string is left alone so
// GetToolCallValidationMode can tell "unset" (inherit / default) from an
// explicit value.
//
// An unknown value is deliberately NOT a config error. wasm-go's rule_matcher
// captures the error returned by parseGlobalConfig and then *drops* it whenever
// any matchRule exists (see RuleMatcher.ParseRuleConfig): m.globalConfig is left
// at its zero value and every matchRule inherits it. GPUStack's reconciler puts
// `providers` only in defaultConfig and `{"activeProviderId": ...}` in each
// matchRule, so a zeroed global means no provider resolves and the whole plugin
// silently no-ops on every AI request. Taking the proxy down over a typo in an
// unrelated field is far worse than running the check in its default mode.
//
// The warning is returned rather than logged here so this package stays free of
// wasm-go's log, whose package-level Log is nil until the host installs it --
// logging from a config path would panic every plain `go test`.
func normalizeToolCallValidationMode(raw string) (mode ToolCallValidationMode, warning string) {
	mode = ToolCallValidationMode(raw)
	switch mode {
	case "", ToolCallValidationOff, ToolCallValidationStrict:
		return mode, ""
	default:
		return ToolCallValidationDefault, fmt.Sprintf(
			"invalid toolCallValidation %q, expected one of: off, strict; falling back to %q",
			raw, string(ToolCallValidationDefault))
	}
}
