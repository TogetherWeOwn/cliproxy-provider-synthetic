package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func classifyResponse(r pluginapi.HTTPResponse) error { return classifyFailure(r, time.Now()) }

type executor struct {
	kit.Passthrough
	quota *quotaProvider
}

func newExecutor(q *quotaProvider) executor {
	return executor{Passthrough: kit.Passthrough{Provider: provider, BuildRequest: buildRequest, PrepareHTTP: prepareHTTP, ClassifyError: classifyResponse}, quota: q}
}
func (e executor) forAuth(id string, storage []byte, meta map[string]any) kit.Passthrough {
	p := e.Passthrough
	digest := credentialDigest(storage, meta)
	p.ClassifyError = func(resp pluginapi.HTTPResponse) error {
		// Evaluated lazily: a header observation that lands during a long
		// stream is still fresh evidence when the 429 classifies.
		return classifyWithQuota(resp, time.Now(), e.quota.latest(id, digest))
	}
	return p
}

// stripHostPrefix removes one leading "<prefix>/" segment from a payload model
// when the host pinned this credential under a prefix. The host rewrites the
// payload model to the stripped execution model for registered plugin models,
// but raw HTTP execution and unregistered models can still arrive pinned, and
// without this the pinned shape "<acct>/synthetic/<id>" fails the provider
// prefix check below. The host pins the auth prefix verbatim
// ("<prefix>/<id>"), so any non-empty prefix strips verbatim. Slashed
// prefixes can no longer come from ParseAuth (rejected with invalid_auth),
// but if one ever reaches metadata it still pinned verbatim.
func stripHostPrefix(model string, meta map[string]any) string {
	prefix, _ := meta["prefix"].(string)
	if prefix = strings.TrimSpace(prefix); prefix == "" {
		return model
	}
	return strings.TrimPrefix(model, prefix+"/")
}

func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	// Execute is non-streaming regardless of an inconsistent payload stream flag.
	var input struct{ Model string }
	_ = json.Unmarshal(r.Payload, &input)
	if !strings.HasPrefix(stripHostPrefix(input.Model, r.AuthMetadata), modelPrefix+"infinite:") {
		return e.forAuth(r.AuthID, r.StorageJSON, r.AuthMetadata).Execute(ctx, r)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Stream = true
	stream, err := e.forAuth(r.AuthID, r.StorageJSON, r.AuthMetadata).ExecuteStream(streamCtx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	payload, err := aggregateSSE(streamCtx, stream.Chunks)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	headers := stream.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	for _, h := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		headers.Del(h)
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: headers}, nil
}

func (e executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	return e.forAuth(r.AuthID, r.StorageJSON, r.AuthMetadata).ExecuteStream(ctx, r)
}

func (e executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	var mode struct {
		Model  string
		Stream bool
	}
	_ = json.Unmarshal(r.Body, &mode)
	if strings.HasPrefix(stripHostPrefix(mode.Model, r.Metadata), modelPrefix+"infinite:") && !mode.Stream {
		out, err := e.Execute(ctx, pluginapi.ExecutorRequest{AuthID: r.AuthID, Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata, HTTPClient: r.HTTPClient})
		if err != nil {
			return pluginapi.ExecutorHTTPResponse{}, err
		}
		return pluginapi.ExecutorHTTPResponse{StatusCode: http.StatusOK, Headers: out.Headers, Body: out.Payload}, nil
	}
	return e.forAuth(r.AuthID, r.StorageJSON, r.Metadata).HttpRequest(ctx, r)
}

func buildRequest(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
	key, err := kit.APIKey(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &body) != nil || body == nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_request", Message: "payload must be a JSON object", HTTPStatus: 400}
	}
	var model string
	if err := json.Unmarshal(body["model"], &model); err != nil {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected a synthetic/ model", HTTPStatus: 400}
	}
	if model = stripHostPrefix(model, r.AuthMetadata); !strings.HasPrefix(model, modelPrefix) || len(model) == len(modelPrefix) {
		return pluginapi.HTTPRequest{}, &pluginabi.Error{Code: "invalid_model", Message: "expected a synthetic/ model", HTTPStatus: 400}
	}
	model = strings.TrimPrefix(model, modelPrefix)
	body["model"], _ = json.Marshal(model)
	if r.Stream || strings.HasPrefix(model, "infinite:") {
		body["stream"] = json.RawMessage("true")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: selectedBase(upstreamBase) + "/openai/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, Body: raw}, nil
}

func prepareHTTP(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
	// Raw HTTP execution cannot inject destinations, credentials or extra headers.
	return buildRequest(ctx, pluginapi.ExecutorRequest{Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata})
}
