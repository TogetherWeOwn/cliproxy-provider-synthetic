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
func (e executor) forAuth(id string) kit.Passthrough {
	p := e.Passthrough
	p.ClassifyError = func(resp pluginapi.HTTPResponse) error {
		return classifyWithQuota(resp, time.Now(), e.quota.latest(id))
	}
	return p
}

func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	// Execute is non-streaming regardless of an inconsistent payload stream flag.
	var input struct{ Model string }
	_ = json.Unmarshal(r.Payload, &input)
	if !strings.HasPrefix(input.Model, modelPrefix+"infinite:") {
		return e.forAuth(r.AuthID).Execute(ctx, r)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Stream = true
	stream, err := e.forAuth(r.AuthID).ExecuteStream(streamCtx, r)
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
	return e.forAuth(r.AuthID).ExecuteStream(ctx, r)
}

func (e executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	var mode struct {
		Model  string
		Stream bool
	}
	_ = json.Unmarshal(r.Body, &mode)
	if strings.HasPrefix(mode.Model, modelPrefix+"infinite:") && !mode.Stream {
		out, err := e.Execute(ctx, pluginapi.ExecutorRequest{AuthID: r.AuthID, Payload: r.Body, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata, HTTPClient: r.HTTPClient})
		if err != nil {
			return pluginapi.ExecutorHTTPResponse{}, err
		}
		return pluginapi.ExecutorHTTPResponse{StatusCode: http.StatusOK, Headers: out.Headers, Body: out.Payload}, nil
	}
	return e.forAuth(r.AuthID).HttpRequest(ctx, r)
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
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) || len(model) == len(modelPrefix) {
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
