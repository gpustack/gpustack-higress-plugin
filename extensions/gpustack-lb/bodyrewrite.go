package main

// Model-name rewriting in the request body, along both the JSON and multipart
// paths.
//
// **Both modes share this one implementation**; the only difference is the
// resolve callback passed in:
//
//	context mode   resolve = modelMapping's existing resolution
//	               (exact -> prefix -> default -> unchanged)
//	finisher mode  resolve = always the chosen candidate's modelName
//
// Before the merge this existed twice, once in each plugin, 112 lines
// duplicated verbatim -- and the multipart walk happens to be the fiddliest
// code in the whole design. Once the two copies diverge, the only place it
// shows up is endpoints like /v1/audio/transcriptions.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	mtJSON      = "application/json"
	mtMultipart = "multipart/form-data"
)

// resolver maps the model name already in the body to the rewrite target.
// Returning an empty string, or the same value, means no rewrite.
type resolver func(old string) string

func rewriteBody(config Config, body []byte, contentType string, resolve resolver) types.Action {
	if baseMediaType(contentType) == mtMultipart {
		return rewriteMultipart(config, body, contentType, resolve)
	}
	return rewriteJSON(config, body, resolve)
}

// rewriteResult is what the pure planners return: what the model was, what it
// should become, and the new body when one has to be written.
//
// newBody is nil when nothing should be replaced. header is what to write to
// modelToHeader, which is **not** the same as "the body changed" -- both paths
// deliberately set the header even when the body is left alone.
type rewriteResult struct {
	oldModel string
	newModel string
	newBody  []byte
	header   string
	setHeadr bool
}

// planJSONRewrite is the pure core of the JSON path.
//
// Split out for the usual reason: rewriteJSON makes host ABI calls, which panic
// outside a wasm host, so the decision logic could not otherwise be covered.
// This is the fiddliest code in the design (see the file header), so it is also
// the code that most needs covering.
//
// err is returned for a body that is not JSON at all; the caller logs and
// passes the body through.
func planJSONRewrite(modelKey string, body []byte, resolve resolver) (rewriteResult, error) {
	if !json.Valid(body) {
		return rewriteResult{}, errInvalidJSON
	}
	res := rewriteResult{oldModel: gjson.GetBytes(body, modelKey).String()}
	res.newModel = resolve(res.oldModel)
	// The header is set from the resolved value even when the body is not
	// touched, which is upstream model-mapper's behaviour.
	res.header, res.setHeadr = res.newModel, true

	if res.newModel == "" || res.newModel == res.oldModel {
		return res, nil
	}
	newBody, err := sjson.SetBytes(body, modelKey, res.newModel)
	if err != nil {
		return res, err
	}
	res.newBody = newBody
	return res, nil
}

var errInvalidJSON = errors.New("invalid json body")

func rewriteJSON(config Config, body []byte, resolve resolver) types.Action {
	res, err := planJSONRewrite(config.modelKey, body, resolve)
	if err != nil {
		if errors.Is(err, errInvalidJSON) {
			proxywasm.LogError(pluginName + ": invalid json body")
		} else {
			proxywasm.LogErrorf("%s: failed to update model: %v", pluginName, err)
		}
		return types.ActionContinue
	}
	if config.modelToHeader != "" && res.setHeadr {
		_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, res.header)
	}
	if res.newBody == nil {
		return types.ActionContinue
	}
	_ = proxywasm.ReplaceHttpRequestBody(res.newBody)
	proxywasm.LogInfof("%s: model rewritten, before: %s, after: %s", pluginName, res.oldModel, res.newModel)
	return types.ActionContinue
}

// rewriteMultipart covers endpoints like /v1/audio/transcriptions and
// /v1/images/edits. Upstream higress model-mapper only handles JSON, so those
// routes 404 whenever the alias differs from the actual deployment name
// (gpustack/gpustack#4617).
//
// The body is fully buffered, so the parts can be scanned in any order and
// file-before-model is fine; non-target parts are written back verbatim. Any
// error at any step abandons the rewrite and passes the body through unchanged
// -- corrupting the body is far worse than not rewriting it.
// planMultipartRewrite is the pure core of the multipart path.
//
// Every failure returns an error, and every error means "pass the body through
// unchanged" -- corrupting a body is far worse than not rewriting it. Splitting
// it out is what makes the part walk testable at all: the original made host
// calls at each of those failure points, and a host ABI call panics outside a
// wasm host.
func planMultipartRewrite(modelKey, contentType string, body []byte, resolve resolver) (rewriteResult, error) {
	boundary := multipartBoundary(contentType)
	if boundary == "" {
		return rewriteResult{}, errors.New("multipart boundary missing/unparseable")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	out.Grow(len(body))
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		return rewriteResult{}, err
	}

	// Resolve once as if there were no model field, so that even when the body
	// has no modelKey form part the header still gets the same value --
	// consistent with the JSON path.
	res := rewriteResult{}
	res.newModel = resolve("")
	foundModel := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rewriteResult{}, err
		}
		pw, err := writer.CreatePart(part.Header)
		if err != nil {
			return rewriteResult{}, err
		}

		// Only a non-file form field whose name equals modelKey is a rewrite
		// target. A binary upload that happens to be named "model" must be
		// passed through as a file, untouched.
		if !foundModel && part.FormName() == modelKey && part.FileName() == "" {
			raw, err := io.ReadAll(part)
			if err != nil {
				return rewriteResult{}, err
			}
			res.oldModel = string(raw)
			res.newModel = resolve(res.oldModel)
			if res.newModel == "" {
				res.newModel = res.oldModel
			}
			if _, err := pw.Write([]byte(res.newModel)); err != nil {
				return rewriteResult{}, err
			}
			foundModel = true
			continue
		}
		if _, err := io.Copy(pw, part); err != nil {
			return rewriteResult{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return rewriteResult{}, err
	}

	res.header, res.setHeadr = res.newModel, true
	// Do not invent a model form part when there was none -- adding a part to a
	// multipart body is far more intrusive than adding a field to JSON.
	if foundModel && res.newModel != res.oldModel {
		res.newBody = out.Bytes()
	}
	return res, nil
}

func rewriteMultipart(config Config, body []byte, contentType string, resolve resolver) types.Action {
	res, err := planMultipartRewrite(config.modelKey, contentType, body, resolve)
	if err != nil {
		proxywasm.LogWarnf("%s: multipart rewrite failed (%v); body unchanged", pluginName, err)
		return types.ActionContinue
	}
	if config.modelToHeader != "" && res.setHeadr {
		_ = proxywasm.ReplaceHttpRequestHeader(config.modelToHeader, res.header)
	}
	if res.newBody != nil {
		_ = proxywasm.ReplaceHttpRequestBody(res.newBody)
		proxywasm.LogInfof("%s: model rewritten, before: %s, after: %s", pluginName, res.oldModel, res.newModel)
	}
	return types.ActionContinue
}

func baseMediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}

func multipartBoundary(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["boundary"]
}
