// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.

package config

import (
	"testing"

	"github.com/tidwall/gjson"
)

const minimalProvider = `"provider":{"type":"generic","genericHost":"http://127.0.0.1:8080","apiTokens":["t"]}`

func TestToolCallValidationMode_FromJson(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		want        ToolCallValidationMode
		wantWarning bool
	}{
		{
			// Off -- the fork must not change upstream ai-proxy's observable
			// behaviour for deployments that never asked for the check. See
			// ToolCallValidationDefault.
			name: "unset_defaults_to_off",
			json: `{` + minimalProvider + `}`,
			want: ToolCallValidationOff,
		},
		{
			name: "explicit_strict",
			json: `{"toolCallValidation":"strict",` + minimalProvider + `}`,
			want: ToolCallValidationStrict,
		},
		{
			name: "explicit_off",
			json: `{"toolCallValidation":"off",` + minimalProvider + `}`,
			want: ToolCallValidationOff,
		},
		{
			// `warn` was considered and dropped; it must not silently behave
			// like strict if someone copies it out of an old draft.
			name:        "removed_warn_mode_falls_back_to_default",
			json:        `{"toolCallValidation":"warn",` + minimalProvider + `}`,
			want:        ToolCallValidationDefault,
			wantWarning: true,
		},
		{
			// Parsed before the legacy `provider` branch returns early, so the
			// legacy config shape must still honour it.
			name: "honoured_alongside_new_style_providers_list",
			json: `{"toolCallValidation":"off","providers":[{"id":"p","type":"generic","genericHost":"http://127.0.0.1:8080","apiTokens":["t"]}],"activeProviderId":"p"}`,
			want: ToolCallValidationOff,
		},
		{
			// An unknown value must NOT fail Validate: wasm-go drops the
			// global-config error whenever matchRules exist, which would leave
			// every rule with a zeroed global (no providers) and silently
			// no-op the whole plugin. Log loudly, fall back to the default.
			name:        "unknown_value_falls_back_to_default",
			json:        `{"toolCallValidation":"of",` + minimalProvider + `}`,
			want:        ToolCallValidationDefault,
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c PluginConfig
			c.FromJson(gjson.Parse(tt.json))
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
			if got := c.GetToolCallValidationMode(); got != tt.want {
				t.Errorf("GetToolCallValidationMode() = %q, want %q", got, tt.want)
			}
			if gotWarning := c.ToolCallValidationWarning() != ""; gotWarning != tt.wantWarning {
				t.Errorf("ToolCallValidationWarning() = %q, want non-empty: %v",
					c.ToolCallValidationWarning(), tt.wantWarning)
			}
		})
	}
}

// A matchRule override that does not mention toolCallValidation must inherit
// the global value; one that does mention it must win.
// The global is set to strict here precisely because it is NOT the default:
// otherwise "inherited" and "fell back to the default" are indistinguishable.
func TestToolCallValidationMode_OverrideInheritance(t *testing.T) {
	var global PluginConfig
	global.FromJson(gjson.Parse(`{"toolCallValidation":"strict",` + minimalProvider + `}`))

	inherited := global
	inherited.FromJson(gjson.Parse(`{` + minimalProvider + `}`))
	if got := inherited.GetToolCallValidationMode(); got != ToolCallValidationStrict {
		t.Errorf("silent override: got %q, want inherited %q", got, ToolCallValidationStrict)
	}

	overridden := global
	overridden.FromJson(gjson.Parse(`{"toolCallValidation":"off",` + minimalProvider + `}`))
	if got := overridden.GetToolCallValidationMode(); got != ToolCallValidationOff {
		t.Errorf("explicit override: got %q, want %q", got, ToolCallValidationOff)
	}

	if got := global.GetToolCallValidationMode(); got != ToolCallValidationStrict {
		t.Errorf("global was mutated by an override: got %q, want %q", got, ToolCallValidationStrict)
	}
}
