// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Structural validation of tool / tool_calls pairing on the Chat Completions
// request path. See gpustack/gpustack#6210.

package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unsafe"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/config"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/provider"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Roles recognised by the pairing walk. Any other role (system, user, developer,
// the legacy "function" role, ...) is treated as "some other role" by rule 2.
const (
	pairingRoleAssistant = "assistant"
	pairingRoleTool      = "tool"
)

// Client-facing rejection messages. These deliberately mirror the wording that
// strict OpenAI-compatible backends (vLLM and friends) already emit for the same
// malformed request, so a caller that switched to GPUStack keeps matching on the
// string it was already matching on.
const (
	// Covers rule 1 (orphan tool message) and rule 3 (a tool_call_id answered
	// twice) -- from the caller's point of view both are "this tool message does
	// not answer a preceding tool call".
	toolPairingOrphanMessage = "Messages with role 'tool' must be a response to a preceding message with 'tool_calls'"
	// Covers rule 2, both the dangling case (the conversation ends with calls
	// unanswered) and the interrupted case (another role shows up first).
	toolPairingUnansweredMessage = "An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'."
	// Covers rule 4. The upstream providers we compared against do not have a
	// settled wording for this one, so we state it plainly.
	toolPairingDuplicateCallIDMessage = "An assistant message with 'tool_calls' must not reuse a 'tool_calls[].id'."
	// An assistant tool call whose `id` is absent, empty, or not a JSON string.
	toolPairingInvalidCallIDMessage = "Each entry of 'tool_calls' must carry a non-empty string 'id'."
)

// toolPairingRule names which structural rule a request tripped. It is emitted
// as a metric label and in the rejection log line, so operators can tell an
// orphan tool message (usually a truncated client-side history) apart from
// dangling tool_calls (usually a client that dropped a tool result) without
// reading request bodies.
type toolPairingRule string

const (
	toolPairingRuleOrphanToolMessage    toolPairingRule = "orphan_tool_message"
	toolPairingRuleUnansweredToolCalls  toolPairingRule = "unanswered_tool_calls"
	toolPairingRuleDuplicateToolCallID  toolPairingRule = "duplicate_tool_call_id"
	toolPairingRuleDuplicateToolCallsID toolPairingRule = "duplicate_tool_calls_id"
	toolPairingRuleInvalidToolCallID    toolPairingRule = "invalid_tool_call_id"
	toolPairingRuleInvalidToolCallsID   toolPairingRule = "invalid_tool_calls_id"
)

// toolPairingViolation describes a single structural failure. Message goes to
// the client verbatim; Detail is logged only, because it names message indexes
// and ids that we do not want to hand back to an unauthenticated caller.
type toolPairingViolation struct {
	Rule    toolPairingRule
	Message string
	Detail  string
}

// validateToolCallPairing walks an OpenAI Chat Completions request body and
// reports the first structural tool / tool_calls pairing violation, or nil when
// the message list is well formed.
//
// The four rules, from gpustack/gpustack#6210:
//
//  1. every role:"tool" message's tool_call_id matches an id in a *preceding*
//     assistant.tool_calls[];
//  2. an assistant message carrying tool_calls is followed immediately by one
//     tool message per call -- no other role may appear until all are answered;
//  3. a tool_call_id does not repeat within a request;
//  4. an id does not repeat within an assistant.tool_calls array.
//
// This is purely structural: it never repairs the message list and never makes
// a business-semantic judgement. A wrong repair is worse than no repair, since
// the gateway cannot know what the missing tool result should have said.
//
// Rule 4 is enforced request-wide rather than per-array. That is stricter than
// the literal wording, but it is what rule 3 already implies: if two assistant
// turns issue the same id, the tool messages answering them necessarily repeat
// a tool_call_id, which rule 3 rejects anyway. Enforcing it at the point the id
// is *issued* produces the more actionable error of the two.
//
// The function is pure (gjson only, no host calls) so it is unit-testable
// without a proxy-wasm host.
func validateToolCallPairing(body []byte) *toolPairingViolation {
	messages := parseBodyNoCopy(body).Get("messages")
	if !messages.IsArray() {
		// Not an OpenAI-shaped chat request (or not JSON at all). Nothing to
		// validate -- fail open and let the upstream decide.
		return nil
	}

	// pending is the set of ids issued by the most recent assistant tool_calls
	// that have not been answered yet; pendingOrder records them in issue order
	// purely so the diagnostic below is deterministic. A set rather than a slice
	// because matching and removal have to stay O(1): a single assistant message
	// may legitimately carry many parallel calls, and the request body limit is
	// 100 MiB, so a linear scan-and-shift per tool result would be quadratic in
	// an attacker-chosen k and could pin an Envoy worker.
	pending := make(map[string]struct{})
	var pendingOrder []string
	// answered / issued are request-wide so rules 3 and 4 survive multi-turn
	// histories, not just the current tool-call window.
	answered := make(map[string]struct{})
	issued := make(map[string]struct{})

	for i, msg := range messages.Array() {
		role := msg.Get("role").String()

		if role == pairingRoleTool {
			id, ok := nonEmptyJSONString(msg.Get("tool_call_id"))
			if !ok {
				// gjson's String() would render a missing field, a null, and a
				// numeric id all as something matchable ("" or "123"), so an
				// assistant call with no id followed by a tool message with no
				// tool_call_id would pair up and pass. Require a real string.
				return &toolPairingViolation{
					Rule:    toolPairingRuleInvalidToolCallID,
					Message: toolPairingOrphanMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_call_id is missing or not a non-empty string", i),
				}
			}
			if _, dup := answered[id]; dup {
				return &toolPairingViolation{
					Rule:    toolPairingRuleDuplicateToolCallID,
					Message: toolPairingOrphanMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_call_id %q is answered more than once", i, id),
				}
			}
			if _, open := pending[id]; !open {
				return &toolPairingViolation{
					Rule:    toolPairingRuleOrphanToolMessage,
					Message: toolPairingOrphanMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_call_id %q matches no preceding assistant tool_calls[].id", i, id),
				}
			}
			delete(pending, id)
			answered[id] = struct{}{}
			continue
		}

		// Any non-tool message closes the window opened by a preceding assistant
		// tool_calls. Reaching here with pending ids left is rule 2's
		// "interrupted by another role" case.
		if len(pending) > 0 {
			return &toolPairingViolation{
				Rule:    toolPairingRuleUnansweredToolCalls,
				Message: toolPairingUnansweredMessage,
				Detail: fmt.Sprintf("messages[%d]: role %q appears before tool_call_id(s) [%s] were answered",
					i, role, joinPending(pending, pendingOrder)),
			}
		}
		pendingOrder = pendingOrder[:0]

		if role != pairingRoleAssistant {
			continue
		}
		toolCalls := msg.Get("tool_calls")
		if !toolCalls.IsArray() {
			continue
		}
		for _, call := range toolCalls.Array() {
			id, ok := nonEmptyJSONString(call.Get("id"))
			if !ok {
				return &toolPairingViolation{
					Rule:    toolPairingRuleInvalidToolCallsID,
					Message: toolPairingInvalidCallIDMessage,
					Detail:  fmt.Sprintf("messages[%d]: a tool_calls[].id is missing or not a non-empty string", i),
				}
			}
			if _, dup := issued[id]; dup {
				return &toolPairingViolation{
					Rule:    toolPairingRuleDuplicateToolCallsID,
					Message: toolPairingDuplicateCallIDMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_calls[].id %q is issued more than once", i, id),
				}
			}
			issued[id] = struct{}{}
			pending[id] = struct{}{}
			pendingOrder = append(pendingOrder, id)
		}
	}

	if len(pending) > 0 {
		return &toolPairingViolation{
			Rule:    toolPairingRuleUnansweredToolCalls,
			Message: toolPairingUnansweredMessage,
			Detail: fmt.Sprintf("request ends with unanswered tool_call_id(s) [%s]",
				joinPending(pending, pendingOrder)),
		}
	}
	return nil
}

// nonEmptyJSONString accepts a gjson value only when it really is a non-empty
// JSON string. OpenAI types both `tool_calls[].id` and `tool_call_id` as
// strings, and Result.String() would otherwise coerce a null, a missing field
// or a number into something that compares equal across the two sides.
func nonEmptyJSONString(v gjson.Result) (string, bool) {
	if v.Type != gjson.String || v.Str == "" {
		return "", false
	}
	return v.Str, true
}

// joinPending renders the still-unanswered ids in the order they were issued.
// Only ever called on the rejection path, so the O(k) filter is free.
//
// Each id is quoted rather than appended raw. `nonEmptyJSONString` hands back
// the *unescaped* JSON value, so `{"id": "a\nb"}` arrives as a Go string with a
// real newline in it; joined raw into Detail and then logged with %s, a caller
// could inject whole forged WARN lines into a line-oriented log pipeline.
// strconv.Quote renders control characters as escapes, which is what every
// other diagnostic in this file already gets from %q.
func joinPending(pending map[string]struct{}, order []string) string {
	remaining := make([]string, 0, len(pending))
	for _, id := range order {
		if _, ok := pending[id]; ok {
			remaining = append(remaining, strconv.Quote(id))
		}
	}
	return strings.Join(remaining, ", ")
}

// parseBodyNoCopy hands the request body to gjson without copying it.
//
// gjson.GetBytes copies Result.Raw out of the []byte so the Result cannot
// alias a slice the caller might mutate. For a whole `messages` array that is
// a copy of essentially the entire request body -- ~1.2 MB on a long Claude
// Code history, allocated and collected on every request. That is exactly the
// per-request memory pressure gpustack/gpustack#6217 is about, and ai-proxy's
// own fast path (defaultTransformRequestBody) only ever asks gjson for tiny
// scalars, so the copy would be new cost this check introduced.
//
// Contract: the returned Result, and every string derived from it, aliases
// `body`. They must not outlive it and `body` must not be mutated while they
// are in use. validateToolCallPairing satisfies both -- the ids it keeps live
// only in function-local maps and slices, the violation strings it returns are
// built with fmt.Sprintf/strings.Join (which copy), and the caller does not
// touch `body` until the walk has returned.
func parseBodyNoCopy(body []byte) gjson.Result {
	if len(body) == 0 {
		return gjson.Result{}
	}
	return gjson.Parse(unsafe.String(&body[0], len(body)))
}

// logToolCallValidationWarning surfaces a toolCallValidation value the config
// package could not recognise. It lives here rather than in the config package
// because wasm-go's package-level Log is nil until the host installs it, so
// logging from a config path would panic every plain `go test`.
func logToolCallValidationWarning(pluginConfig *config.PluginConfig) {
	if warning := pluginConfig.ToolCallValidationWarning(); warning != "" {
		log.Errorf("[%s] %s", pluginName, warning)
	}
}

// toolCallPairingDecision answers "should this request be rejected, and why"
// without touching the host, so every gate is unit-testable without a
// proxy-wasm runtime. enforceToolCallPairing is then the thin I/O shell around
// it, and the only untested code is the local reply itself.
//
// claudeConverted comes from needsClaudeResponseConversion: a Claude request
// that main.go rewrote onto /v1/chat/completions carries
// apiName == ApiNameChatCompletion but a Claude-shaped body, which this walk
// would find no tool messages in. Skip it explicitly rather than relying on
// that accident.
func toolCallPairingDecision(mode config.ToolCallValidationMode, apiName provider.ApiName, claudeConverted bool, body []byte) *toolPairingViolation {
	// Off is the default, so this is the branch almost every deployment takes:
	// keep it first and ahead of every other check so an unconfigured plugin
	// pays nothing at all.
	if mode != config.ToolCallValidationStrict {
		return nil
	}
	// Chat Completions only. The Anthropic Messages surface expresses the same
	// relationship with tool_use / tool_result content blocks rather than a
	// tool role, and is out of scope here.
	if apiName != provider.ApiNameChatCompletion {
		return nil
	}
	if claudeConverted {
		return nil
	}
	return validateToolCallPairing(body)
}

// enforceToolCallPairing runs the pairing check for the current request and
// reports whether it already sent a local reply (in which case the caller must
// stop the filter chain).
//
// It runs on the *inbound* body, before ReplaceByCustomSettings and before the
// provider's own TransformRequestBody, so the rejection describes what the
// client actually sent.
//
// Ordering note: everything that can wait runs *after*
// SendHttpResponseWithDetail, because this repo's local-reply paths want the
// window before the send kept short (see the rate-limit rejection hazards in
// CLAUDE.md). The one exception is the consumer header: it is read before the
// send because attributing a bad request back to an API key is the point of the
// log line, and a header read is not guaranteed to still resolve once the local
// reply has been queued. That leaves exactly one hostcall in the window --
// fewer than ai-proxy's own 413 path, which reads and parses Content-Length
// before sending.
func enforceToolCallPairing(ctx wrapper.HttpContext, pluginConfig config.PluginConfig, apiName provider.ApiName, body []byte) bool {
	violation := toolCallPairingDecision(
		pluginConfig.GetToolCallValidationMode(), apiName, needsClaudeResponseConversion(ctx), body)
	if violation == nil {
		return false
	}

	consumer, _ := proxywasm.GetHttpRequestHeader(headerConsumer)

	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusBadRequest,
		"ai-proxy.tool_call_pairing_invalid",
		util.CreateHeaders(util.HeaderContentType, util.MimeTypeApplicationJson),
		toolPairingErrorBody(violation.Message),
		-1,
	)

	// The model and consumer live on the log line rather than on the metric:
	// both are request-controlled, and a label a caller can pick freely would
	// let cheap rejected requests grow Envoy's stat registry and the
	// process-global counter cache without bound. See emitToolCallPairingRejected.
	log.Warnf("[%s] rejected request: tool_calls pairing check failed: rule=%s, model=%q, consumer=%q, %s",
		pluginName, violation.Rule, gjson.GetBytes(body, "model").String(), consumer, violation.Detail)
	emitToolCallPairingRejected(violation.Rule)
	return true
}

// toolPairingErrorBody renders the OpenAI-style error envelope. Built with
// sjson so the message -- which embeds quotes -- is escaped rather than
// hand-formatted into the JSON.
func toolPairingErrorBody(message string) []byte {
	body, err := sjson.SetBytes(
		[]byte(`{"error":{"type":"invalid_request_error","code":"invalid_request_error"}}`),
		"error.message", message)
	if err != nil {
		return []byte(`{"error":{"message":"invalid tool_calls pairing","type":"invalid_request_error","code":"invalid_request_error"}}`)
	}
	return body
}
