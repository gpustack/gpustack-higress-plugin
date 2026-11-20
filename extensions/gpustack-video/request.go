package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
)

type FilePart struct {
	Filename    string
	ContentType string
	Data        []byte
}

type VideoRequest struct {
	Values    map[string]any
	Files     map[string][]FilePart
	Multipart bool
}

func parseRequest(body []byte, contentType string) (VideoRequest, error) {
	r := VideoRequest{Values: map[string]any{}, Files: map[string][]FilePart{}}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return r, err
	}
	if mediaType == "application/json" {
		err = decodeJSON(body, &r.Values)
		if err == nil && r.Values == nil {
			err = errors.New("video request must be an object")
		}
		return r, err
	}
	if mediaType != "multipart/form-data" {
		return r, errors.New("unsupported video content type")
	}
	r.Multipart = true
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return r, err
		}
		name := part.FormName()
		if name == "" {
			return r, errors.New("missing form field name")
		}
		if filename := part.FileName(); filename != "" {
			r.Files[name] = append(r.Files[name], FilePart{filename, part.Header.Get("Content-Type"), data})
			r.Values[name] = name
		} else {
			r.Values[name] = string(data)
		}
	}
}

func (r VideoRequest) Encode() ([]byte, string, error) {
	if !r.Multipart {
		data, err := json.Marshal(r.Values)
		return data, "application/json", err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range r.Values {
		if value == nil {
			continue
		}
		if files := r.Files[name]; len(files) > 0 {
			for _, file := range files {
				header := textproto.MIMEHeader{}
				header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": file.Filename}))
				if file.ContentType != "" {
					header.Set("Content-Type", file.ContentType)
				}
				part, err := writer.CreatePart(header)
				if err != nil {
					return nil, "", err
				}
				if _, err = part.Write(file.Data); err != nil {
					return nil, "", err
				}
			}
			continue
		}
		text, ok := value.(string)
		if !ok {
			data, err := json.Marshal(value)
			if err != nil {
				return nil, "", err
			}
			text = string(data)
		}
		if err := writer.WriteField(name, text); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func object(value any) (map[string]any, error) {
	if value == nil || value == "" {
		return map[string]any{}, nil
	}
	if text, ok := value.(string); ok {
		if err := decodeJSON([]byte(text), &value); err != nil {
			return nil, errors.New("Video extension must be a JSON object")
		}
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Video extension must be a JSON object")
	}
	return result, nil
}

func truthy(value any) bool {
	if number, ok := value.(json.Number); ok {
		v, err := number.Float64()
		return err == nil && v == 1
	}
	if text, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "yes", "on":
			return true
		}
		return false
	}
	return value == true || value == float64(1)
}

func scalar(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case json.Number:
		return v.Float64()
	case int:
		return float64(v), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	return 0, errors.New("Invalid numeric video parameter")
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("invalid trailing JSON data")
	}
	return nil
}
