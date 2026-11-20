package main

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func deletionControl(config Config, binding Binding, suffix string, keepTask bool) (string, map[string]any, error) {
	id := strings.TrimPrefix(binding.Path, "/v1/videos/")
	if !nativeID.MatchString(id) {
		return "", nil, errors.New("invalid video deletion binding")
	}
	path := controlPrefix + "/" + binding.ID + "/" + suffix
	payload := map[string]any{"native_id": id, "destination": binding.Destination, "keep_task": keepTask}
	if config.role == "cluster" {
		var grant RelayGrant
		if !getState(routing.RelayState, &grant) || grant.ID != binding.ID {
			return "", nil, errors.New("missing video deletion grant")
		}
		path = "/v2/video-tasks/relay/" + suffix
		payload["task_id"] = grant.ID
		payload["ticket"] = grant.Ticket
	}
	return path, payload, nil
}

func startDeletion(ctx wrapper.HttpContext, config Config, binding Binding) types.Action {
	path, payload, err := deletionControl(config, binding, "delete-started", ctx.GetBoolContext("video_keep_task", false))
	if err != nil {
		return fail(503, "Video task cannot be deleted")
	}
	err = call(config, "POST", path, payload, func(status int, body []byte) {
		if status != 200 {
			fail(503, "Video deletion could not be prepared")
			return
		}
		proxywasm.ResumeHttpRequest()
	})
	if err != nil {
		return fail(503, "Video deletion could not be prepared")
	}
	return types.ActionPause
}

func onDeletionResponse(ctx wrapper.HttpContext, config Config, binding Binding, job map[string]any) types.Action {
	id, ok := job["id"].(string)
	keepTask := ctx.GetBoolContext("video_keep_task", false)
	confirmed := job["deleted"] == true || job["status"] == "deleted" || (binding.Forward && keepTask && (job["status"] == "cancelled" || job["status"] == "completed" || job["status"] == "failed" || job["status"] == "expired"))
	if !ok || !confirmed {
		return responseError(503, "Video deletion was not confirmed")
	}
	path, payload := "", map[string]any{}
	if binding.Forward {
		if id != binding.ID {
			return responseError(503, "Invalid video deletion response")
		}
		path = controlPrefix + "/" + binding.ID + "/deletion-confirmed"
		if keepTask {
			path += "?keep_task=true"
		}
	} else {
		if binding.Path != "/v1/videos/"+id {
			return responseError(503, "Invalid video deletion response")
		}
		var err error
		path, payload, err = deletionControl(config, binding, "deleted", keepTask)
		if err != nil {
			return responseError(503, "Video deletion could not be confirmed")
		}
	}
	err := call(config, "POST", path, payload, func(status int, body []byte) {
		if status != 200 {
			responseError(503, "Video deletion could not be confirmed")
			proxywasm.ResumeHttpResponse()
			return
		}
		var result map[string]any
		if json.Unmarshal(body, &result) != nil || result["id"] != binding.ID || (keepTask && !terminalResult(result)) || (!keepTask && result["deleted"] != true) {
			responseError(503, "Invalid video deletion confirmation")
			proxywasm.ResumeHttpResponse()
			return
		}
		encoded, _ := json.Marshal(result)
		_ = proxywasm.ReplaceHttpResponseBody(encoded)
		proxywasm.ResumeHttpResponse()
	})
	if err != nil {
		return responseError(503, "Video deletion could not be confirmed")
	}
	return types.ActionPause
}

func terminalResult(job map[string]any) bool {
	switch job["status"] {
	case "cancelled", "completed", "failed", "expired":
		return true
	}
	return false
}
