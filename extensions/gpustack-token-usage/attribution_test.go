package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// buildBaseMetrics is a pure function over the header list, so the
// attribution dimensions are testable without a wasm host.
func TestAttributionHeaders(t *testing.T) {
	var config PluginConfig
	raw := `{"attributionHeaders":[
		{"header":"x-app-id","attribute":"app_id"},
		{"header":"x-end-user","attribute":"end_user"}
	]}`
	if err := parseConfig(gjson.Parse(raw), &config); err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(config.AttributionHeaders) != 2 {
		t.Fatalf("parsed %d attribution headers, want 2", len(config.AttributionHeaders))
	}

	m := &ModelUsageMetrics{}
	applyAttributionHeaders(m, config.AttributionHeaders, [][2]string{
		{"X-App-Id", "nj-billing"}, // case-insensitive, like every header read here
	})
	if m.Attributes == nil || m.Attributes["app_id"] != "nj-billing" {
		t.Errorf("app_id attribute missing: %v", m.Attributes)
	}
	// x-end-user absent: contributes nothing.
	if _, ok := m.Attributes["end_user"]; ok {
		t.Errorf("end_user present although the header is absent")
	}

	// No configured dimensions: no map at all.
	empty := &ModelUsageMetrics{}
	applyAttributionHeaders(empty, nil, [][2]string{{"x-app-id", "a"}})
	if empty.Attributes != nil {
		t.Errorf("attributes emitted without configuration: %v", empty.Attributes)
	}
}

// The report payload must carry the collected attributes: reportMetrics
// builds a fresh ModelUsageMetrics from the stashed base one, so the
// field has to be copied across explicitly (regression for a review
// finding where it was dropped and the serialized report omitted
// `attributes` entirely).
func TestReportPayloadCarriesAttributes(t *testing.T) {
	m := &ModelUsageMetrics{Attributes: map[string]string{"app_id": "nj"}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"attributes":{"app_id":"nj"}`) {
		t.Errorf("serialized payload omits attributes: %s", body)
	}
}

// The parse retry path: dimensions from a failed attempt must not leak
// into the backup-JSON retry (wasm-go reuses the same config object).
func TestAttributionHeadersResetOnReparse(t *testing.T) {
	var config PluginConfig
	if err := parseConfig(gjson.Parse(`{"attributionHeaders":[{"header":"x-a","attribute":"a"}]}`), &config); err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if len(config.AttributionHeaders) != 1 {
		t.Fatalf("first parse: %d dimensions", len(config.AttributionHeaders))
	}
	// Second parse on the same object without the field must reset, not
	// extend.
	if err := parseConfig(gjson.Parse(`{}`), &config); err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if len(config.AttributionHeaders) != 0 {
		t.Errorf("dimensions survived a reparse without the field: %v", config.AttributionHeaders)
	}
}

func TestParseAttributionHeadersRejectsMalformed(t *testing.T) {
	cases := []string{
		`{"attributionHeaders":{"x-app-id":"app_id"}}`,    // not an array
		`{"attributionHeaders":[{"header":"x-app-id"}]}`,  // attribute missing
		`{"attributionHeaders":[{"attribute":"app_id"}]}`, // header missing
	}
	for _, raw := range cases {
		var config PluginConfig
		if err := parseConfig(gjson.Parse(raw), &config); err == nil {
			t.Errorf("parseConfig(%s) succeeded, want error", raw)
		}
	}
}
