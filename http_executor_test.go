package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRawHTTPInfiniteUsesSameAggregatePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Errorf("unsafe raw execution %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, infiniteSSE)
	}))
	defer server.Close()
	result, err := newExecutor(nil).HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/ignored", Headers: http.Header{"Authorization": {"Bearer untrusted"}}, Body: []byte(`{"model":"synthetic/infinite:small:vision","stream":false}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil || result.StatusCode != 200 || result.Headers.Get("Content-Type") != "application/json" || !strings.Contains(string(result.Body), `"content":"hello"`) {
		t.Fatalf("raw aggregate %s %+v %v", result.Body, result.Headers, err)
	}
}

func TestExecutorTokenCountIsLocalAndEstimated(t *testing.T) {
	out, err := newExecutor(nil).CountTokens(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/syn:small:text"}`)})
	if err != nil || out.Metadata["estimated"] != true || !strings.Contains(string(out.Payload), `"total_tokens":`) {
		t.Fatalf("count %+v %v", out, err)
	}
}
