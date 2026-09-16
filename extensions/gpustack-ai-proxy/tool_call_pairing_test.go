// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Covers the six verification cases listed in gpustack/gpustack#6210: the four
// malformed shapes must be flagged, and the two controls must pass untouched.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/config"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/provider"
)

func TestValidateToolCallPairing(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRule toolPairingRule
		wantMsg  string
	}{
		// --- the four malformed requests from the issue ---
		{
			name: "orphan_tool_message",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"tool","tool_call_id":"call_1","content":"22C"}
			]}`,
			wantRule: toolPairingRuleOrphanToolMessage,
			wantMsg:  toolPairingOrphanMessage,
		},
		{
			name: "dangling_tool_calls",
			body: `{"model":"m","messages":[
				{"role":"user","content":"weather in Shanghai?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_weather_001","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}
				]}
			]}`,
			wantRule: toolPairingRuleUnansweredToolCalls,
			wantMsg:  toolPairingUnansweredMessage,
		},
		{
			name: "duplicate_tool_call_id",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"a"},
				{"role":"tool","tool_call_id":"call_1","content":"b"}
			]}`,
			wantRule: toolPairingRuleDuplicateToolCallID,
			wantMsg:  toolPairingOrphanMessage,
		},
		{
			name: "interrupted_by_another_role",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"user","content":"never mind"},
				{"role":"tool","tool_call_id":"call_1","content":"a"}
			]}`,
			wantRule: toolPairingRuleUnansweredToolCalls,
			wantMsg:  toolPairingUnansweredMessage,
		},

		// --- the two controls ---
		{
			name: "well_formed_tool_chain",
			body: `{"model":"m","messages":[
				{"role":"system","content":"be brief"},
				{"role":"user","content":"weather in Shanghai?"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"22C"},
				{"role":"assistant","content":"It is 22C."},
				{"role":"user","content":"and tomorrow?"}
			]}`,
		},
		{
			name: `plain_request_with_stop`,
			body: `{"model":"m","stop":["\n\n"],"messages":[{"role":"user","content":"hi"}]}`,
		},

		// --- additional shapes worth pinning ---
		{
			name: "parallel_calls_answered_out_of_order",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_b","content":"b"},
				{"role":"tool","tool_call_id":"call_a","content":"a"}
			]}`,
		},
		{
			name: "two_turns_each_with_tool_calls",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"a"},
				{"role":"assistant","content":"ok"},
				{"role":"user","content":"again"},
				{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_2","content":"b"}
			]}`,
		},
		{
			name: "partially_answered_parallel_calls",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_a","content":"a"}
			]}`,
			wantRule: toolPairingRuleUnansweredToolCalls,
			wantMsg:  toolPairingUnansweredMessage,
		},
		{
			name: "duplicate_id_within_one_tool_calls_array",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"call_1","type":"function","function":{"name":"g","arguments":"{}"}}
				]}
			]}`,
			wantRule: toolPairingRuleDuplicateToolCallsID,
			wantMsg:  toolPairingDuplicateCallIDMessage,
		},
		{
			name: "id_reused_across_turns",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"a"},
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"b"}
			]}`,
			wantRule: toolPairingRuleDuplicateToolCallsID,
			wantMsg:  toolPairingDuplicateCallIDMessage,
		},
		// gjson's String() renders a missing field, a null and a number all as
		// something that compares equal across the two sides, so an unpaired
		// pair of "empty" ids would otherwise match each other and pass.
		{
			name: "tool_message_without_tool_call_id",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","content":"a"}
			]}`,
			wantRule: toolPairingRuleInvalidToolCallsID,
			wantMsg:  toolPairingInvalidCallIDMessage,
		},
		{
			name: "tool_call_id_is_null",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":null,"content":"a"}
			]}`,
			wantRule: toolPairingRuleInvalidToolCallID,
			wantMsg:  toolPairingOrphanMessage,
		},
		{
			name: "tool_call_id_is_empty_string",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"","content":"a"}
			]}`,
			wantRule: toolPairingRuleInvalidToolCallID,
			wantMsg:  toolPairingOrphanMessage,
		},
		{
			// OpenAI types both sides as strings. A numeric id would otherwise
			// be coerced to "123" on both sides and pair up.
			name: "numeric_ids_are_rejected",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":123,"type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":123,"content":"a"}
			]}`,
			wantRule: toolPairingRuleInvalidToolCallsID,
			wantMsg:  toolPairingInvalidCallIDMessage,
		},
		{
			name: "tool_calls_entry_without_id",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[
					{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}},
					{"type":"function","function":{"name":"g","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_a","content":"a"}
			]}`,
			wantRule: toolPairingRuleInvalidToolCallsID,
			wantMsg:  toolPairingInvalidCallIDMessage,
		},
		{
			name: "assistant_with_empty_tool_calls_array",
			body: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"hello","tool_calls":[]}
			]}`,
		},
		// Fail-open shapes: nothing to validate, never a rejection.
		{name: "no_messages_field", body: `{"model":"m","input":"hi"}`},
		{name: "messages_not_an_array", body: `{"model":"m","messages":"hi"}`},
		{name: "not_json_at_all", body: `not json`},
		{name: "empty_body", body: ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateToolCallPairing([]byte(tt.body))
			if tt.wantRule == "" {
				if got != nil {
					t.Fatalf("expected no violation, got rule=%s detail=%s", got.Rule, got.Detail)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected violation %s, got none", tt.wantRule)
			}
			if got.Rule != tt.wantRule {
				t.Errorf("rule = %s, want %s (detail: %s)", got.Rule, tt.wantRule, got.Detail)
			}
			if got.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", got.Message, tt.wantMsg)
			}
			if got.Detail == "" {
				t.Error("detail must not be empty: it is the only operator-facing context")
			}
		})
	}
}

// Detail goes straight into a WARN log line with %s, and the ids it embeds are
// the *unescaped* JSON values, so a caller could otherwise put a newline in a
// tool_calls[].id and forge whole log lines in a line-oriented pipeline. Every
// id-bearing diagnostic must escape.
func TestViolationDetailEscapesControlCharactersInIDs(t *testing.T) {
	// "call_a\n2026-09-17 WARN forged line" plus a tab and a quote.
	const evil = `call_a\n2026-09-17 WARN [forged]\tline\"`

	tests := []struct {
		name string
		body string
	}{
		{
			// Dangling -> the end-of-request branch, via joinPending.
			name: "unanswered_at_end_of_request",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":"` + evil + `","type":"function","function":{"name":"f","arguments":"{}"}}]}
			]}`,
		},
		{
			// Interrupted -> the mid-walk branch, also via joinPending.
			name: "unanswered_when_interrupted",
			body: `{"model":"m","messages":[
				{"role":"assistant","tool_calls":[{"id":"` + evil + `","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"user","content":"never mind"}
			]}`,
		},
		{
			// Orphan -> the %q path, covered here so all id-bearing
			// diagnostics are held to the same bar.
			name: "orphan_tool_message",
			body: `{"model":"m","messages":[
				{"role":"tool","tool_call_id":"` + evil + `","content":"a"}
			]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateToolCallPairing([]byte(tt.body))
			if got == nil {
				t.Fatal("expected a violation")
			}
			if strings.ContainsAny(got.Detail, "\n\r\t") {
				t.Errorf("Detail carries a raw control character, log injection is possible: %q", got.Detail)
			}
			// The id must still be legible to an operator, just escaped.
			if !strings.Contains(got.Detail, `call_a\n`) {
				t.Errorf("Detail lost the id entirely: %s", got.Detail)
			}
		})
	}
}

// The gates that decide whether the walk runs at all are as load-bearing as
// the walk itself: a regression in any of them either forwards the four
// malformed shapes or starts rejecting traffic that was meant to be exempt.
// toolCallPairingDecision exists as a pure seam so they can be covered without
// a proxy-wasm host; only the SendHttpResponseWithDetail call in
// enforceToolCallPairing is left untested.
func TestToolCallPairingDecision(t *testing.T) {
	malformed := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"tool","tool_call_id":"call_1","content":"a"}
	]}`)
	wellFormed := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	tests := []struct {
		name            string
		mode            config.ToolCallValidationMode
		apiName         provider.ApiName
		claudeConverted bool
		body            []byte
		wantReject      bool
	}{
		{
			name: "strict_rejects_a_malformed_chat_completion", mode: config.ToolCallValidationStrict,
			apiName: provider.ApiNameChatCompletion, body: malformed, wantReject: true,
		},
		{
			name: "strict_passes_a_well_formed_chat_completion", mode: config.ToolCallValidationStrict,
			apiName: provider.ApiNameChatCompletion, body: wellFormed,
		},
		{
			name: "off_never_looks_at_the_body", mode: config.ToolCallValidationOff,
			apiName: provider.ApiNameChatCompletion, body: malformed,
		},
		{
			// The zero value is what a config that never mentions the field
			// produces before GetToolCallValidationMode resolves it; it must
			// not be mistaken for strict.
			name: "unset_mode_never_looks_at_the_body", mode: "",
			apiName: provider.ApiNameChatCompletion, body: malformed,
		},
		{
			name: "embeddings_are_out_of_scope", mode: config.ToolCallValidationStrict,
			apiName: provider.ApiNameEmbeddings, body: malformed,
		},
		{
			name: "anthropic_messages_are_out_of_scope", mode: config.ToolCallValidationStrict,
			apiName: provider.ApiNameAnthropicMessages, body: malformed,
		},
		{
			// Rewritten onto the chat-completions path but still Claude-shaped.
			name: "converted_claude_request_is_skipped", mode: config.ToolCallValidationStrict,
			apiName: provider.ApiNameChatCompletion, claudeConverted: true, body: malformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolCallPairingDecision(tt.mode, tt.apiName, tt.claudeConverted, tt.body)
			if gotReject := got != nil; gotReject != tt.wantReject {
				t.Fatalf("toolCallPairingDecision() rejected = %v, want %v", gotReject, tt.wantReject)
			}
		})
	}
}

// The rejection body is what a client parses, so pin its exact shape.
func TestToolPairingErrorBody(t *testing.T) {
	body := toolPairingErrorBody(toolPairingOrphanMessage)

	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("rejection body is not valid JSON: %v (%s)", err, body)
	}
	if parsed.Error.Message != toolPairingOrphanMessage {
		t.Errorf("message = %q, want %q", parsed.Error.Message, toolPairingOrphanMessage)
	}
	if parsed.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", parsed.Error.Type)
	}
	if parsed.Error.Code != "invalid_request_error" {
		t.Errorf("code = %q, want invalid_request_error", parsed.Error.Code)
	}
	// The messages embed single quotes; make sure nothing mangled them.
	if !strings.Contains(string(body), `must be a response to a preceding message`) {
		t.Errorf("message was mangled during encoding: %s", body)
	}
}

func TestAiStatName(t *testing.T) {
	got := aiStatName("ai-route-route-1", "outbound|80||svc.local", "qwen3-0.6b", "",
		metricNameToolCallPairingRejected,
		[2]string{"rule", string(toolPairingRuleOrphanToolMessage)},
	)
	want := "route.ai-route-route-1.upstream.outbound_80__svc.local.model.qwen3-0.6b.consumer.none" +
		".metric.gpustack_ai_proxy_tool_call_pairing_rejected_total" +
		".rule.orphan_tool_message"
	if got != want {
		t.Errorf("aiStatName()\n got: %s\nwant: %s", got, want)
	}
}
