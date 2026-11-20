package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeleteConfirmsNativeResponseBeforeRemovingPublicBinding(t *testing.T) {
	for _, response := range []string{`{"id":"native-task","deleted":true}`, `{"id":"native-task","status":"deleted"}`} {
		t.Run(response, func(t *testing.T) {
			h := host(t, "transport")
			id := "video_" + strings.Repeat("a", 32)
			binding := Binding{ID: id, Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
			data, _ := json.Marshal(binding)
			_ = h.SetProperty([]string{bindingState}, data)
			_ = h.SetProperty([]string{callerState}, []byte(`{"authorization":"Bearer user"}`))
			_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
			c := h.InitializeHttpContext()
			h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "DELETE"}, {":path", "/v1/videos/" + id}, {"x-envoy-max-retries", "5"}}, true)
			calls := h.GetCalloutAttributesFromContext(c)
			if len(calls) != 1 || !strings.HasSuffix(header(calls[0].Headers, ":path"), "/delete-started") {
				t.Fatal("engine deletion was not registered first")
			}
			if header(h.GetCurrentRequestHeaders(c), ":path") != "/v1/videos/native-task" || header(h.GetCurrentRequestHeaders(c), "x-envoy-max-retries") != "0" {
				t.Fatal("deletion lost its native path or retry boundary")
			}
			h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{}`))
			h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
			h.CallOnResponseBody(c, []byte(response), true)
			calls = h.GetCalloutAttributesFromContext(c)
			if len(calls) != 1 || !strings.HasSuffix(header(calls[0].Headers, ":path"), "/deleted") {
				t.Fatal("missing durable deletion confirmation")
			}
			h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"id":"`+id+`","deleted":true,"record_deleted":true,"file_cleanup":"unconfirmed"}`))
			var result map[string]any
			if json.Unmarshal(h.GetCurrentResponseBody(c), &result) != nil || result["id"] != id || result["deleted"] != true {
				t.Fatalf("incorrect public deletion response: %s", h.GetCurrentResponseBody(c))
			}
		})
	}
}

func TestDeleteResolverRequestsADeletionScopedGrant(t *testing.T) {
	h := host(t, "resolve")
	id := "video_" + strings.Repeat("a", 32)
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "DELETE"}, {":path", "/v1/videos/" + id}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || header(calls[0].Headers, ":path") != controlPrefix+"/"+id+"/resolve?operation=delete" {
		t.Fatal("delete reused read-only routing authorization")
	}
}

func TestCancellationKeepsPublicTaskAndStripsRetentionFlagBeforeEngine(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{callerState}, []byte(`{"authorization":"Bearer user"}`))
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "DELETE"}, {":path", "/v1/videos/" + id + "?keep_task=true"}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"keep_task":true`) {
		t.Fatal("cancel intent not retained")
	}
	if header(h.GetCurrentRequestHeaders(c), ":path") != "/v1/videos/native-task" {
		t.Fatal("GPUStack flag leaked to native engine")
	}
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{}`))
	h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
	h.CallOnResponseBody(c, []byte(`{"id":"native-task","deleted":true}`), true)
	calls = h.GetCalloutAttributesFromContext(c)
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `"keep_task":true`) {
		t.Fatal("cancel confirmation lost retention")
	}
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"id":"`+id+`","status":"cancelled","record_only_delete":true,"file_cleanup":"retained"}`))
	var result map[string]any
	if json.Unmarshal(h.GetCurrentResponseBody(c), &result) != nil || result["status"] != "cancelled" || result["record_only_delete"] != true || result["deleted"] != nil {
		t.Fatal("cancellation removed public record")
	}
}

func TestCancellationUsesCommittedCompletionInsteadOfInventingCancelled(t *testing.T) {
	h := host(t, "transport")
	id := "video_" + strings.Repeat("a", 32)
	binding := Binding{ID: id, Cluster: "outbound|80||model-2-4.static", Destination: "model-2-4.static", Path: "/v1/videos/native-task"}
	data, _ := json.Marshal(binding)
	_ = h.SetProperty([]string{bindingState}, data)
	_ = h.SetProperty([]string{callerState}, []byte(`{"authorization":"Bearer user"}`))
	_ = h.SetProperty([]string{"cluster_name"}, []byte(binding.Cluster))
	c := h.InitializeHttpContext()
	h.CallOnRequestHeaders(c, [][2]string{{":authority", "test"}, {":method", "DELETE"}, {":path", "/v1/videos/" + id + "?keep_task=true"}}, true)
	calls := h.GetCalloutAttributesFromContext(c)
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{}`))
	h.CallOnResponseHeaders(c, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
	h.CallOnResponseBody(c, []byte(`{"id":"native-task","deleted":true}`), true)
	calls = h.GetCalloutAttributesFromContext(c)
	h.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"id":"`+id+`","status":"completed","file_state":"missing","file_cleanup":"deleted"}`))
	var result map[string]any
	_ = json.Unmarshal(h.GetCurrentResponseBody(c), &result)
	if result["status"] != "completed" || result["file_cleanup"] != "deleted" {
		t.Fatal(result)
	}
}
