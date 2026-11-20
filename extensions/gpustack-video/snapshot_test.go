package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestSnapshotPreservesParametersAndUploadDescriptors(t *testing.T) {
	request, err := parseRequest([]byte(`{"model":"mapped-model","prompt":"test","seed":9007199254740993,"extra_params":{"task":"fl2va","num_inference_steps":30}}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	request.Files["first"] = []FilePart{{Filename: "frame.png", ContentType: "image/png", Data: []byte("image-bytes")}}
	request.Values["first"] = "first"
	parameters := request.Snapshot("published-model")
	data, err := json.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	if parameters["model"] != "published-model" || !strings.Contains(string(data), "9007199254740993") || strings.Contains(string(data), "image-bytes") {
		t.Fatalf("incomplete or unsafe snapshot: %s", data)
	}
	upload := parameters["first"].(map[string]any)["uploads"].([]map[string]any)[0]
	if upload["filename"] != "frame.png" || upload["size"] != len("image-bytes") {
		t.Fatal(upload)
	}
	if request.Values["model"] != "mapped-model" {
		t.Fatal("snapshot changed the inference request")
	}
}

func TestRequestSnapshotOmitsInlineMediaAndNestedCredentials(t *testing.T) {
	request := VideoRequest{Values: map[string]any{
		"prompt": "test", "api_key": "private-key",
		"extra_params": `{"task":"ref2va","authorization":"Bearer secret","refs":[{"image_url":"data:image/png;base64,private-image"}]}`,
	}, Files: map[string][]FilePart{}}
	data, err := json.Marshal(request.Snapshot("published-model"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-key", "Bearer secret", "private-image"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot retained credentials or bytes: %s", data)
		}
	}
	if !strings.Contains(string(data), "ref2va") || request.Values["api_key"] != "private-key" {
		t.Fatal("snapshot lost parameters or modified the request")
	}
}

func TestSnapshotKeepsJSONTextPromptsAsText(t *testing.T) {
	prompt := `{"scene":"rain"}`
	r := VideoRequest{Values: map[string]any{"prompt": prompt}}
	if r.Snapshot("wan")["prompt"] != prompt {
		t.Fatal("JSON-shaped prompt was converted into structured parameters")
	}
}

func TestSnapshotRemovesURLSecretsAndUploadPathsWithoutChangingInference(t *testing.T) {
	r := VideoRequest{Values: map[string]any{"prompt": "https://own:prompt@example.test/?keep=me", "image_reference": map[string]any{"image_url": "https://user:password@media.example/frame?signature=private#private"}, "first": "first"}, Files: map[string][]FilePart{"first": {{Filename: "/host/private/frame.png", Data: []byte("image")}}}}
	snapshot := r.Snapshot("wan")
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "/host/") {
		t.Fatal(string(encoded))
	}
	if snapshot["prompt"] != r.Values["prompt"] {
		t.Fatal("prompt changed")
	}
	if r.Values["image_reference"].(map[string]any)["image_url"] != "https://user:password@media.example/frame?signature=private#private" {
		t.Fatal("inference input changed")
	}
}

func TestPublicResponseOmitsEngineExceptionAndPrivateFields(t *testing.T) {
	job := map[string]any{"id": "native", "status": "failed", "file_path": "/host/private/file", "error": map[string]any{"message": "internal /secret/model token=secret"}}
	encoded, _ := json.Marshal(publicJob(job, Binding{ID: "video_public", Model: "wan"}))
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "/host/") || strings.Contains(string(encoded), "native") {
		t.Fatal(string(encoded))
	}
}
