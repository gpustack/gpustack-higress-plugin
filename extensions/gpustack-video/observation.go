package main

import (
	"encoding/json"
	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-video/routing"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
)

func persistObservation(ctx wrapper.HttpContext, config Config, binding Binding, native string, job map[string]any, encoded []byte) types.Action {
	path := controlPrefix + "/" + binding.ID + "/observed"
	payload := map[string]any{"native_id": native, "destination": binding.Destination, "job": job}
	if config.role == "cluster" {
		var grant RelayGrant
		if !getState(routing.RelayState, &grant) {
			return responseError(503, "Video routing is unavailable")
		}
		path = "/v2/video-tasks/relay/observed"
		payload["task_id"], payload["ticket"] = grant.ID, grant.Ticket
	}
	err := call(config, "POST", path, payload, func(status int, body []byte) {
		if status != 200 {
			responseError(503, "Video status could not be saved")
		} else {
			encoded = committedResponse(body, binding, encoded)
			_ = proxywasm.RemoveHttpResponseHeader("content-length")
			_ = proxywasm.ReplaceHttpResponseBody(encoded)
		}
		proxywasm.ResumeHttpResponse()
	})
	if err != nil {
		return responseError(503, "Video status could not be saved")
	}
	return types.ActionPause
}

// The control plane may have observed a newer terminal state under its row lock.
func committedResponse(body []byte, binding Binding, fallback []byte) []byte {
	var result struct {
		Response map[string]any `json:"response"`
	}
	if json.Unmarshal(body, &result) == nil && result.Response["id"] == binding.ID {
		encoded, err := json.Marshal(publicJob(result.Response, binding))
		if err == nil {
			return encoded
		}
	}
	return fallback
}

func publicJob(job map[string]any, binding Binding) map[string]any {
	result := map[string]any{"id": binding.ID, "model": binding.Model, "object": "video"}
	for _, key := range []string{"status", "progress", "seconds", "size", "fps", "num_frames", "duration_s", "expires_at", "created_at", "prompt", "backend", "file_state", "record_only_delete", "requested_size", "requested_seconds"} {
		if value, ok := job[key]; ok {
			result[key] = value
		}
	}
	if job["error"] != nil {
		message, code := "Video generation failed. Check the model instance logs for details.", "video_generation_failed"
		if error, ok := job["error"].(map[string]any); ok && error["code"] == "video_runtime_lost" {
			code = "video_runtime_lost"
			message = "The original model instance was deleted or restarted; this video task cannot continue."
		}
		result["error"] = map[string]any{"code": code, "message": message}
	}
	return result
}
