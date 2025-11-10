package main

import (
	"encoding/json"
	"testing"
)

func TestProcessDataWithToken(t *testing.T) {

	// 构造一个简单的 JSON 响应体
	origin := `data: {"id":"chatcmpl-2c66674e-719d-4be4-a0f1-d7cdbd76df30","object":"chat.completion.chunk","created":1762490796,"model":"qwen3-0.6b","choices":[],"usage":{"prompt_tokens":132,"total_tokens":311,"completion_tokens":179}}`
	ttft := int64(123)
	tpot := float64(45.4555)
	tps := float64(6.6677)

	result := process_data_with_token([]byte(origin), ttft, tpot, tps)

	var m map[string]interface{}
	err := json.Unmarshal(result, &m)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	usage, ok := m["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage field not found or not a map")
	}

	if usage["time_to_first_token_ms"] != float64(ttft) {
		t.Errorf("time_to_first_token_ms want %d, got %v", ttft, usage["time_to_first_token_ms"])
	}
	if usage["time_per_output_token_ms"] != float64(45.46) {
		t.Errorf("time_per_output_token_ms want %f, got %v", tpot, usage["time_per_output_token_ms"])
	}
	if usage["tokens_per_second"] != 6.67 {
		t.Errorf("tokens_per_second want %f, got %v", tps, usage["tokens_per_second"])
	}
}
