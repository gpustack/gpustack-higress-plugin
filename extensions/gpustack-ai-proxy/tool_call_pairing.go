// GPUStack-local file: no upstream counterpart in the Higress ai-proxy plugin.
// Structural validation of tool / tool_calls pairing on the Chat Completions
// request path. See gpustack/gpustack#6210.

package main

import (
	"fmt"
	"net/http"
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

	// pending holds the ids issued by the most recent assistant tool_calls that
	// have not been answered yet. Order does not matter: OpenAI clients are free
	// to return parallel tool results in any order.
	var pending []string
	// answered / issued are request-wide so rules 3 and 4 survive multi-turn
	// histories, not just the current tool-call window.
	answered := make(map[string]struct{})
	issued := make(map[string]struct{})

	for i, msg := range messages.Array() {
		role := msg.Get("role").String()

		if role == pairingRoleTool {
			id := msg.Get("tool_call_id").String()
			if _, dup := answered[id]; dup {
				return &toolPairingViolation{
					Rule:    toolPairingRuleDuplicateToolCallID,
					Message: toolPairingOrphanMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_call_id %q is answered more than once", i, id),
				}
			}
			pos := indexOfString(pending, id)
			if pos < 0 {
				return &toolPairingViolation{
					Rule:    toolPairingRuleOrphanToolMessage,
					Message: toolPairingOrphanMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_call_id %q matches no preceding assistant tool_calls[].id", i, id),
				}
			}
			pending = append(pending[:pos], pending[pos+1:]...)
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
					i, role, strings.Join(pending, ", ")),
			}
		}

		if role != pairingRoleAssistant {
			continue
		}
		toolCalls := msg.Get("tool_calls")
		if !toolCalls.IsArray() {
			continue
		}
		for _, call := range toolCalls.Array() {
			id := call.Get("id").String()
			if _, dup := issued[id]; dup {
				return &toolPairingViolation{
					Rule:    toolPairingRuleDuplicateToolCallsID,
					Message: toolPairingDuplicateCallIDMessage,
					Detail:  fmt.Sprintf("messages[%d]: tool_calls[].id %q is issued more than once", i, id),
				}
			}
			issued[id] = struct{}{}
			pending = append(pending, id)
		}
	}

	if len(pending) > 0 {
		return &toolPairingViolation{
			Rule:    toolPairingRuleUnansweredToolCalls,
			Message: toolPairingUnansweredMessage,
			Detail: fmt.Sprintf("request ends with unanswered tool_call_id(s) [%s]",
				strings.Join(pending, ", ")),
		}
	}
	return nil
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

func indexOfString(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
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

// enforceToolCallPairing runs the pairing check for the current request and
// reports whether it already sent a local reply (in which case the caller must
// stop the filter chain).
//
// It runs on the *inbound* body, before ReplaceByCustomSettings and before the
// provider's own TransformRequestBody, so the rejection describes what the
// client actually sent.
func enforceToolCallPairing(ctx wrapper.HttpContext, pluginConfig config.PluginConfig, apiName provider.ApiName, body []byte) bool {
	// Off is the default, so this is the branch almost every deployment takes:
	// keep it first and ahead of every other check so an unconfigured plugin
	// pays nothing at all.
	if pluginConfig.GetToolCallValidationMode() != config.ToolCallValidationStrict {
		return false
	}
	// Chat Completions only. The Anthropic Messages surface expresses the same
	// relationship with tool_use / tool_result content blocks rather than a
	// tool role, and is out of scope here.
	if apiName != provider.ApiNameChatCompletion {
		return false
	}
	// A Claude request that main.go rewrote onto /v1/chat/completions carries
	// apiName == ApiNameChatCompletion but a Claude-shaped body, which this
	// walk would find no tool messages in. Skip it explicitly rather than
	// relying on that accident.
	if needsClaudeResponseConversion(ctx) {
		return false
	}

	violation := validateToolCallPairing(body)
	if violation == nil {
		return false
	}

	log.Warnf("[%s] rejecting request: tool_calls pairing check failed: rule=%s, %s",
		pluginName, violation.Rule, violation.Detail)
	emitToolCallPairingRejected(gjson.GetBytes(body, "model").String(), violation.Rule)

	_ = proxywasm.SendHttpResponseWithDetail(
		http.StatusBadRequest,
		"ai-proxy.tool_call_pairing_invalid",
		util.CreateHeaders(util.HeaderContentType, util.MimeTypeApplicationJson),
		toolPairingErrorBody(violation.Message),
		-1,
	)
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
