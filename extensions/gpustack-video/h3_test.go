package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func capabilities(profile, task string) Capabilities {
	return Capabilities{Profile: profile, Tasks: []string{task}, DefaultTask: task, DefaultSize: "768x1344", MediaInputs: map[string]MediaRules{
		"t2va": {}, "fl2va": {Keyframes: true, Image: 2, MaxItems: 2}, "ref2va": {Image: 3, Video: 3, Audio: 3, MaxItems: 12, RequiresVisual: true},
	}}
}

func TestH3TextShapeAndBlankReferences(t *testing.T) {
	for _, profile := range []string{"sglang_h3", "vllm_omni_h3"} {
		t.Run(profile, func(t *testing.T) {
			req := VideoRequest{Values: map[string]any{"model": "h3", "size": "720x1280", "seconds": "4", "image_reference": "   "}}
			if err := adaptH3(&req, capabilities(profile, "t2va")); err != nil {
				t.Fatal(err)
			}
			if _, exists := req.Values["image_reference"]; exists {
				t.Fatal("empty reference retained")
			}
			if profile == "sglang_h3" {
				target := req.Values["target"].(map[string]any)
				if req.Values["task"] != "t2va" || target["short_edge"] != 720 || target["aspect_ratio"] != "9:16" || target["duration_seconds"] != float64(4) {
					t.Fatalf("wrong target: %+v", req.Values)
				}
			} else {
				extra := req.Values["extra_params"].(map[string]any)
				if extra["task"] != "t2va" || extra["aspect_ratio"] != "9:16" || extra["duration"] != float64(4) || !req.Multipart {
					t.Fatalf("wrong vllm parameters: %+v", req.Values)
				}
			}
		})
	}
}

func TestH3UploadedFirstAndLastFrames(t *testing.T) {
	for _, profile := range []string{"sglang_h3", "vllm_omni_h3"} {
		t.Run(profile, func(t *testing.T) {
			req := VideoRequest{Multipart: true, Values: map[string]any{"model": "h3", "media_inputs": []any{
				map[string]any{"type": "image", "role": "keyframe", "frame_index": float64(0), "upload_field": "first"},
				map[string]any{"type": "image", "role": "keyframe", "frame_index": float64(-1), "uri": "https://example.com/last.png"},
			}, "first": "first.png"}, Files: map[string][]FilePart{"first": {{Filename: "first.png", ContentType: "image/png", Data: []byte("image-bytes")}}}}
			if err := adaptH3(&req, capabilities(profile, "fl2va")); err != nil {
				t.Fatal(err)
			}
			if len(req.Files) != 0 || req.Values["media_inputs"] != nil {
				t.Fatal("common media inputs were forwarded to the engine")
			}
			if profile == "sglang_h3" {
				cond := req.Values["conditions"].([]any)
				if !strings.HasPrefix(cond[0].(map[string]any)["uri"].(string), "data:image/png;base64,") || req.Values["target"].(map[string]any)["aspect_ratio"] != "auto" {
					t.Fatal("invalid SGLang condition")
				}
			} else {
				extra := req.Values["extra_params"].(map[string]any)
				if !reflect.DeepEqual(extra["frame_indices"], []int{0, -1}) || len(req.Values["image_reference"].([]any)) != 2 {
					t.Fatal("invalid vLLM references")
				}
			}
			data, ct, err := req.Encode()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseRequest(data, ct)
			if err != nil || parsed.Values["model"] != "h3" {
				t.Fatal("failed to encode adapted multipart")
			}
		})
	}
}

func TestH3KeyframesRespectExplicitCanvas(t *testing.T) {
	for _, profile := range []string{"sglang_h3", "vllm_omni_h3"} {
		for _, test := range []struct {
			size  string
			ratio string
			edge  int
		}{
			{"1344x768", "16:9", 768},
			{"768x768", "1:1", 768},
			{"960x544", "16:9", 544},
		} {
			t.Run(profile+"/"+test.size, func(t *testing.T) {
				req := VideoRequest{Values: map[string]any{
					"size": test.size,
					"media_inputs": []any{map[string]any{
						"type": "image", "role": "keyframe", "frame_index": float64(0),
						"uri": "https://example.com/first.png",
					}},
				}}
				if err := adaptH3(&req, capabilities(profile, "fl2va")); err != nil {
					t.Fatal(err)
				}
				if req.Values["size"] != test.size {
					t.Fatal("selected canvas size changed")
				}
				if profile == "sglang_h3" {
					target := req.Values["target"].(map[string]any)
					if target["short_edge"] != test.edge || target["aspect_ratio"] != test.ratio {
						t.Fatalf("selected canvas was not applied: %+v", target)
					}
				} else if req.Values["extra_params"].(map[string]any)["aspect_ratio"] != test.ratio {
					t.Fatal("selected aspect ratio was not applied")
				}
			})
		}
	}
}

func TestH3RejectsInvalidTaskReferencesAndNumbers(t *testing.T) {
	cases := []map[string]any{
		{"task": true}, {"task": float64(1)}, {"task": "t2va", "image_reference": "https://example.com/a.png"},
		{"seconds": "4 seconds"}, {"seconds": 4.5}, {"seconds": float64(16)},
		{"media_inputs": []any{map[string]any{"type": "audio", "role": "reference", "uri": "https://example.com/a.wav"}}},
	}
	for i, values := range cases {
		task := "t2va"
		if i == len(cases)-1 {
			task = "ref2va"
		}
		req := VideoRequest{Values: values}
		if adaptH3(&req, capabilities("sglang_h3", task)) == nil {
			data, _ := json.Marshal(values)
			t.Fatalf("invalid parameters accepted: %s", data)
		}
	}
}

func TestH3KeyframeIndexMustBeANumberInStructuredJSON(t *testing.T) {
	req := VideoRequest{Values: map[string]any{"media_inputs": []any{map[string]any{"type": "image", "role": "keyframe", "frame_index": "0", "uri": "https://example.com/a.png"}}}}
	if adaptH3(&req, capabilities("sglang_h3", "fl2va")) == nil {
		t.Fatal("string frame index accepted")
	}
}

func TestNativeTargetsFrameCountsAndReferencesArePreserved(t *testing.T) {
	req := VideoRequest{Values: map[string]any{"num_frames": float64(124), "fps": float64(24), "extra_params": map[string]any{"task": "t2va", "aspect_ratio": "16:9"}}}
	if err := adaptH3(&req, capabilities("vllm_omni_h3", "t2va")); err != nil {
		t.Fatal(err)
	}
	if _, exists := req.Values["extra_params"].(map[string]any)["duration"]; exists {
		t.Fatal("native frame count overridden")
	}
	req = VideoRequest{Values: map[string]any{"input_reference": "native-file", "seconds": float64(5)}}
	if err := adaptH3(&req, capabilities("vllm_omni_h3", "fl2va")); err != nil {
		t.Fatal(err)
	}
	if _, exists := req.Values["extra_params"].(map[string]any)["task"]; exists {
		t.Fatal("native task inference overridden")
	}
	target := map[string]any{"short_edge": float64(768), "aspect_ratio": "auto"}
	req = VideoRequest{Values: map[string]any{"task": "ref2va", "target": target, "conditions": []any{map[string]any{"type": "audio", "role": "reference", "uri": "https://example.com/a.wav"}}}}
	if err := adaptH3(&req, capabilities("sglang_h3", "ref2va")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Values["target"], target) {
		t.Fatal("native target changed")
	}
}

func TestBlankReferencesInEitherExtensionNeverSelectReferenceTask(t *testing.T) {
	for _, profile := range []string{"sglang_h3", "vllm_omni_h3"} {
		for _, blank := range []any{nil, "", " \t\n"} {
			for _, extension := range []bool{false, true} {
				values := map[string]any{"seconds": float64(5), "size": "768x1344"}
				fields := map[string]any{}
				for _, name := range referenceFields {
					fields[name] = blank
				}
				if extension {
					values["extra_params"] = fields
				} else {
					for k, v := range fields {
						values[k] = v
					}
				}
				req := VideoRequest{Values: values}
				if err := adaptH3(&req, capabilities(profile, "t2va")); err != nil {
					t.Fatal(err)
				}
				native := req.Values
				if profile == "vllm_omni_h3" {
					native = values["extra_params"].(map[string]any)
				}
				if native["task"] != "t2va" {
					t.Fatal("empty reference changed task")
				}
				for _, name := range referenceFields {
					if native[name] != nil || values[name] != nil {
						t.Fatal("empty reference retained")
					}
				}
			}
		}
	}
}

func TestMixedReferencesAndUploadBytesKeepTheirOrderAndContent(t *testing.T) {
	for _, profile := range []string{"sglang_h3", "vllm_omni_h3"} {
		entries := []any{}
		for _, kind := range []string{"image", "video", "audio", "image"} {
			entries = append(entries, map[string]any{"type": kind, "role": "reference", "uri": "https://example.com/" + kind})
		}
		req := VideoRequest{Values: map[string]any{"media_inputs": entries, "seconds": float64(5)}}
		if err := adaptH3(&req, capabilities(profile, "ref2va")); err != nil {
			t.Fatal(err)
		}
		if profile == "sglang_h3" {
			if !reflect.DeepEqual(req.Values["conditions"], entries) {
				t.Fatal("mixed reference order changed")
			}
		} else if len(req.Values["image_reference"].([]any)) != 2 {
			t.Fatal("references lost")
		}
		data := []byte(strings.Repeat("x", 2*1024*1024))
		req = VideoRequest{Multipart: true, Values: map[string]any{"task": "fl2va", "input_reference": "upload"}, Files: map[string][]FilePart{"input_reference": {{Filename: "frame, 1.png", ContentType: "image/png", Data: data}}}}
		if err := adaptH3(&req, capabilities(profile, "fl2va")); err != nil {
			t.Fatal(err)
		}
		body, ct, err := req.Encode()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseRequest(body, ct)
		if err != nil {
			t.Fatal(err)
		}
		file := parsed.Files["input_reference"][0]
		if file.Filename != "frame, 1.png" || file.ContentType != "image/png" || string(file.Data) != string(data) {
			t.Fatal("upload modified or UI size limit applied by the gateway")
		}
	}
}

func TestInvalidExtensionsMediaAndDurationsAreRejected(t *testing.T) {
	bad := []map[string]any{
		{"extra_body": "[1]"}, {"extra_body": "null"}, {"extra_body": "{"},
		{"task": "t2va", "extra_body": `{"task":"fl2va"}`},
		{"size": "+720x1280"}, {"seconds": true}, {"seconds": "NaN"}, {"seconds": float64(3)},
		{"media_inputs": []any{}},
		{"media_inputs": []any{map[string]any{"type": "image", "role": "reference", "uri": "file:///secret.png"}}},
		{"media_inputs": []any{map[string]any{"type": "image", "role": "reference", "uri": "https://user:secret@example.com/a.png"}}},
	}
	for i, values := range bad {
		task := "t2va"
		if i >= 8 {
			task = "ref2va"
		}
		req := VideoRequest{Values: values}
		if err := adaptH3(&req, capabilities("sglang_h3", task)); err == nil {
			t.Fatalf("invalid case %d accepted", i)
		}
	}
	for _, indices := range [][]float64{{0, 0}, {-1, 0}, {1}} {
		entries := []any{}
		for _, index := range indices {
			entries = append(entries, map[string]any{"type": "image", "role": "keyframe", "frame_index": index, "uri": "https://example.com/a.png"})
		}
		req := VideoRequest{Values: map[string]any{"media_inputs": entries}}
		if adaptH3(&req, capabilities("sglang_h3", "fl2va")) == nil {
			t.Fatal("invalid keyframe order accepted")
		}
	}
}

func TestModelRewriteKeepsLargeJSONIntegersExact(t *testing.T) {
	req, err := parseRequest([]byte(`{"model":"public-route","seed":9007199254740993,"extra_params":{"custom_counter":9223372036854775807}}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	req.Values["model"] = "engine-model"
	if err := adaptH3(&req, capabilities("vllm_omni_h3", "t2va")); err != nil {
		t.Fatal(err)
	}
	encoded, _, err := req.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "9007199254740993") || !strings.Contains(string(encoded), "9223372036854775807") {
		t.Fatal("integer request parameters lost precision")
	}
}
