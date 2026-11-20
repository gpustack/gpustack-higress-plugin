package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type MediaRules struct {
	Keyframes      bool `json:"keyframes"`
	Image          int  `json:"image"`
	Video          int  `json:"video"`
	Audio          int  `json:"audio"`
	MaxItems       int  `json:"max_items"`
	RequiresVisual bool `json:"requires_visual"`
}

type Capabilities struct {
	Profile     string                `json:"request_profile"`
	Tasks       []string              `json:"tasks"`
	DefaultTask string                `json:"default_task"`
	DefaultSize string                `json:"default_size"`
	MediaInputs map[string]MediaRules `json:"media_inputs"`
}

var canvasPattern = regexp.MustCompile(`^[1-9][0-9]*x[1-9][0-9]*$`)

var referenceFields = []string{"input_reference", "input_references", "reference_url", "image_reference", "video_reference", "audio_reference"}

func extras(values map[string]any) (map[string]any, error) {
	left, err := object(values["extra_body"])
	if err != nil {
		return nil, err
	}
	right, err := object(values["extra_params"])
	if err != nil {
		return nil, err
	}
	delete(values, "extra_body")
	delete(values, "extra_params")
	for name, value := range right {
		if original, ok := left[name]; ok && !reflect.DeepEqual(original, value) {
			return nil, fmt.Errorf("Conflicting video parameter: %s", name)
		}
		left[name] = value
	}
	if _, ok := left["model"]; ok {
		return nil, errors.New("model must be a top-level video field")
	}
	if _, ok := left["prompt"]; ok {
		return nil, errors.New("prompt must be a top-level video field")
	}
	for _, name := range []string{"task", "target", "conditions", "aspect_ratio", "duration", "duration_seconds"} {
		value, ok := values[name]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && (name == "target" || name == "conditions") {
			if decodeJSON([]byte(text), &value) != nil {
				return nil, fmt.Errorf("%s must be valid JSON", name)
			}
		}
		if original, ok := left[name]; ok && !reflect.DeepEqual(original, value) {
			return nil, fmt.Errorf("Conflicting video parameter: %s", name)
		}
		left[name] = value
		delete(values, name)
	}
	for _, fields := range []map[string]any{values, left} {
		for _, name := range referenceFields {
			value := fields[name]
			text, isText := value.(string)
			if value == nil || isText && strings.TrimSpace(text) == "" {
				delete(fields, name)
			}
		}
	}
	return left, nil
}

func mediaURI(entry map[string]any, request *VideoRequest, kind string, consumed map[string]bool) (string, error) {
	if field, exists := entry["upload_field"]; exists {
		name, ok := field.(string)
		files := request.Files[name]
		if !ok || consumed[name] || len(files) != 1 || entry["uri"] != nil || len(files[0].Data) == 0 {
			return "", errors.New("Each reference must identify a distinct nonempty upload")
		}
		file := files[0]
		contentType := file.ContentType
		if contentType == "" || contentType == "application/octet-stream" {
			contentType = mime.TypeByExtension(filepath.Ext(file.Filename))
		}
		if !strings.HasPrefix(contentType, kind+"/") {
			return "", errors.New("Invalid reference content type")
		}
		consumed[name] = true
		delete(request.Values, name)
		delete(request.Files, name)
		return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(file.Data), nil
	}
	uri, ok := entry["uri"].(string)
	parsed, err := url.Parse(uri)
	if !ok || err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("Reference URLs must use HTTP(S) without credentials")
	}
	return uri, nil
}

func lowerMedia(request *VideoRequest, extra map[string]any, caps Capabilities) (bool, error) {
	value, exists := request.Values["media_inputs"]
	if !exists {
		return false, nil
	}
	delete(request.Values, "media_inputs")
	if text, ok := value.(string); ok {
		if decodeJSON([]byte(text), &value) != nil {
			return false, errors.New("media_inputs must be a JSON array")
		}
	}
	entries, ok := value.([]any)
	if !ok {
		return false, errors.New("media_inputs must be a JSON array")
	}
	task, _ := extra["task"].(string)
	if task == "" {
		task = caps.DefaultTask
	}
	rules, ok := caps.MediaInputs[task]
	if !ok || len(entries) == 0 || len(entries) > rules.MaxItems {
		return false, errors.New("Provide compatible reference inputs for this video task")
	}
	for _, name := range referenceFields {
		if request.Values[name] != nil || extra[name] != nil {
			return false, errors.New("Do not combine media_inputs with native reference parameters")
		}
	}
	if items, ok := extra["conditions"].([]any); ok && len(items) > 0 {
		return false, errors.New("Do not combine media_inputs with native conditions")
	}
	conditions := []any{}
	indices := []int{}
	counts := map[string]int{}
	consumed := map[string]bool{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return false, errors.New("Invalid reference media entry")
		}
		kind, _ := entry["type"].(string)
		role, _ := entry["role"].(string)
		limits := map[string]int{"image": rules.Image, "video": rules.Video, "audio": rules.Audio}
		condition := map[string]any{"type": kind, "role": role}
		if rules.Keyframes {
			index, numeric := entry["frame_index"].(float64)
			if number, ok := entry["frame_index"].(json.Number); ok {
				n, err := number.Int64()
				index = float64(n)
				numeric = err == nil
			}
			if kind != "image" || role != "keyframe" || !numeric || index != 0 && index != -1 {
				return false, errors.New("FL2VA requires first/last image keyframes")
			}
			indices = append(indices, int(index))
			condition["frame_index"] = int(index)
		} else if limits[kind] < 1 || role != "reference" {
			return false, errors.New("Unsupported reference media type or role")
		}
		uri, err := mediaURI(entry, request, kind, consumed)
		if err != nil {
			return false, err
		}
		condition["uri"] = uri
		conditions = append(conditions, condition)
		counts[kind]++
		if !rules.Keyframes && counts[kind] > limits[kind] {
			return false, errors.New("Too many reference inputs of one type")
		}
	}
	if rules.Keyframes && !reflect.DeepEqual(indices, []int{0}) && !reflect.DeepEqual(indices, []int{-1}) && !reflect.DeepEqual(indices, []int{0, -1}) {
		return false, errors.New("Keyframes must be ordered first, last without duplicates")
	}
	if rules.RequiresVisual && counts["image"]+counts["video"] == 0 {
		return false, errors.New("Provide at least one reference image or video")
	}
	extra["task"] = task
	if caps.Profile == "sglang_h3" {
		extra["conditions"] = conditions
	} else {
		for _, kind := range []string{"image", "video", "audio"} {
			references := []any{}
			for _, raw := range conditions {
				condition := raw.(map[string]any)
				if condition["type"] == kind {
					references = append(references, map[string]any{kind + "_url": condition["uri"]})
				}
			}
			if len(references) > 0 {
				request.Values[kind+"_reference"] = references
			}
		}
		if rules.Keyframes {
			if _, exists := extra["frame_indices"]; exists {
				return false, errors.New("Do not combine media_inputs with frame_indices")
			}
			extra["frame_indices"] = indices
		}
	}
	return true, nil
}

func canvas(value any) (int, string, error) {
	size, ok := value.(string)
	parts := strings.Split(size, "x")
	if !ok || len(parts) != 2 || !canvasPattern.MatchString(size) {
		return 0, "", errors.New("size must use positive WIDTHxHEIGHT dimensions")
	}
	width, errW := strconv.Atoi(parts[0])
	height, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil || width < 1 || height < 1 {
		return 0, "", errors.New("size must use positive WIDTHxHEIGHT dimensions")
	}
	best := math.Inf(1)
	ratio := ""
	delta := math.Inf(1)
	for _, candidate := range []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"} {
		p := strings.Split(candidate, ":")
		x, _ := strconv.ParseFloat(p[0], 64)
		y, _ := strconv.ParseFloat(p[1], 64)
		if distance := math.Abs(float64(width)/float64(height) - x/y); distance < best {
			best = distance
			ratio = candidate
			delta = math.Min(math.Abs(float64(width)-float64(height)*x/y), math.Abs(float64(height)-float64(width)*y/x))
		}
	}
	if delta > 32 {
		return 0, "", errors.New("MiniMax H3 size must use a supported aspect ratio")
	}
	return min(width, height), ratio, nil
}

func duration(values map[string]any) (float64, error) {
	value, ok := values["seconds"]
	if !ok {
		value = float64(4)
	}
	n, err := scalar(value)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 4 || n > 15 || math.Trunc(n) != n {
		return 0, errors.New("seconds must be between 4 and 15 in whole seconds for MiniMax H3")
	}
	return n, nil
}

func adaptH3(request *VideoRequest, caps Capabilities) error {
	if caps.Profile != "sglang_h3" && caps.Profile != "vllm_omni_h3" {
		return errors.New("Unsupported video request profile")
	}
	values := request.Values
	extra, err := extras(values)
	if err != nil {
		return err
	}
	commonMedia, err := lowerMedia(request, extra, caps)
	if err != nil {
		return err
	}
	task, explicit := extra["task"].(string)
	if value, exists := extra["task"]; exists && (!explicit || strings.TrimSpace(task) == "") {
		return fmt.Errorf("Invalid video task: %v", value)
	}
	hasReference := false
	for _, name := range referenceFields {
		hasReference = hasReference || values[name] != nil || extra[name] != nil
	}
	if conditions, ok := extra["conditions"].([]any); ok {
		hasReference = hasReference || len(conditions) > 0
	}
	if !explicit && hasReference {
		if caps.Profile == "vllm_omni_h3" {
			values["extra_params"] = extra
			request.Multipart = true
			return nil
		}
		return errors.New("MiniMax H3 references require an explicit task and model-specific reference parameters")
	}
	if !explicit {
		task = caps.DefaultTask
	}
	allowed := false
	for _, candidate := range caps.Tasks {
		allowed = allowed || task == candidate
	}
	if !allowed {
		return errors.New("Video task is not supported by this deployment")
	}
	if task == "t2va" && hasReference {
		return errors.New("MiniMax H3 t2va accepts text only; remove reference inputs")
	}
	extra["task"] = task
	if task == "t2va" || commonMedia {
		target, err := object(extra["target"])
		if err != nil {
			return err
		}
		if caps.Profile == "sglang_h3" {
			if target["short_edge"] == nil || target["aspect_ratio"] == nil {
				size := values["size"]
				explicitSize := size != nil && size != ""
				if size == nil || size == "" {
					size = caps.DefaultSize
				}
				short, ratio, err := canvas(size)
				if err != nil {
					return err
				}
				if target["short_edge"] == nil {
					target["short_edge"] = short
				}
				if target["aspect_ratio"] == nil {
					if task == "fl2va" && !explicitSize {
						ratio = "auto"
					}
					target["aspect_ratio"] = ratio
				}
			}
			if target["duration_seconds"] == nil {
				n, err := duration(values)
				if err != nil {
					return err
				}
				target["duration_seconds"] = n
			}
			extra["target"] = target
			if task == "t2va" && extra["conditions"] == nil {
				extra["conditions"] = []any{}
			}
		} else {
			if target["aspect_ratio"] == nil && extra["aspect_ratio"] == nil {
				size := values["size"]
				if size == nil || size == "" {
					size = caps.DefaultSize
				}
				_, ratio, err := canvas(size)
				if err != nil {
					return err
				}
				extra["aspect_ratio"] = ratio
			}
			if extra["duration"] == nil && extra["duration_seconds"] == nil && target["duration_seconds"] == nil && (values["seconds"] != nil || values["num_frames"] == nil) {
				n, err := duration(values)
				if err != nil {
					return err
				}
				extra["duration"] = n
			}
		}
	}
	if caps.Profile == "sglang_h3" {
		for key, value := range extra {
			if _, exists := values[key]; !exists {
				values[key] = value
			}
		}
	} else {
		values["extra_params"] = extra
		request.Multipart = true
	}
	return nil
}
