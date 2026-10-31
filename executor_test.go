package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This test client substitutes only the transport. It exercises real HTTP,
// streaming body boundaries and cancellation without contacting the provider.
type mockHostClient struct{ server *httptest.Server }

func (c mockHostClient) request(ctx context.Context, r pluginapi.HTTPRequest) (*http.Response, error) {
	if !strings.HasPrefix(r.URL, upstreamBase+"/") {
		return nil, fmt.Errorf("unexpected upstream")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, c.server.URL+strings.TrimPrefix(r.URL, upstreamBase), bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Headers.Clone()
	return c.server.Client().Do(req)
}
func (c mockHostClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
}
func (c mockHostClient) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.request(ctx, r)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	out := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		for {
			buf := make([]byte, 7)
			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case out <- pluginapi.HTTPStreamChunk{Payload: buf[:n]}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case out <- pluginapi.HTTPStreamChunk{Err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Chunks: out}, nil
}

const infiniteSSE = "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"infinite:small:vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\",\"reasoning_content\":\"think\",\"tool_calls\":[{\"index\":0,\"id\":\"call-example\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":3}}\n\n" +
	"data: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\",\"reasoning_content\":\"ing\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"} \"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

func TestExecutorInfiniteAggregatesSSEAndOrdinaryNonstreamUntouched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Errorf("bad inference request %s", r.URL.Path)
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad request JSON")
		}
		w.Header().Set("X-Synthetic-Quotas", `{"planWeeklyLimit":{"percentRemaining":88}}`)
		if body["model"] == "infinite:small:vision" {
			if body["stream"] != true {
				t.Error("Infinite was not forced to stream")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, infiniteSSE)
		} else if body["model"] == "syn:small:text" {
			if body["stream"] == true {
				t.Error("ordinary nonstream changed to streaming")
			}
			io.WriteString(w, `{"id":"ordinary","choices":[{"message":{"content":"unchanged"}}]}`)
		} else {
			t.Errorf("unexpected model %v", body["model"])
		}
	}))
	defer server.Close()
	ex := newExecutor(nil)
	for _, model := range []string{"synthetic/infinite:small:vision", "synthetic/syn:small:text"} {
		payload, _ := json.Marshal(map[string]any{"model": model, "messages": []any{}})
		result, err := ex.Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "account", Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
		if err != nil {
			t.Fatal(err)
		}
		if model == "synthetic/syn:small:text" {
			if string(result.Payload) != `{"id":"ordinary","choices":[{"message":{"content":"unchanged"}}]}` {
				t.Fatalf("changed ordinary result %s", result.Payload)
			}
			continue
		}
		var got struct {
			ID      string
			Object  string
			Choices []struct {
				Message struct {
					Role, Content string
					Reasoning     string `json:"reasoning_content"`
					Tools         []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				Finish string `json:"finish_reason"`
			}
			Usage map[string]int
		}
		if json.Unmarshal(result.Payload, &got) != nil || got.ID != "chat-example" || got.Object != "chat.completion" || len(got.Choices) != 1 {
			t.Fatalf("invalid completion %s", result.Payload)
		}
		c := got.Choices[0]
		if c.Message.Content != "hello" || c.Message.Role != "assistant" || c.Message.Reasoning != "thinking" || c.Finish != "tool_calls" || len(c.Message.Tools) != 1 || c.Message.Tools[0].Function.Arguments != "{} " {
			t.Fatalf("incorrect aggregation %s", result.Payload)
		}
		if got.Usage["prompt_tokens"] != 3 || got.Usage["completion_tokens"] != 2 || got.Usage["total_tokens"] != 5 {
			t.Fatalf("lost usage %s", result.Payload)
		}
		if result.Headers.Get("Content-Type") != "application/json" || result.Headers.Get("X-Synthetic-Quotas") == "" {
			t.Fatalf("wrong aggregation headers %+v", result.Headers)
		}
	}
}

func TestInfiniteRejectsMalformedTruncatedAndCanceledStream(t *testing.T) {
	for _, sse := range []string{"data: not-json\n\n", "data: {}\n\n", "data: [DONE]\n\n", strings.TrimSuffix(infiniteSSE, "data: [DONE]\n\n")} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse)
		}))
		_, err := newExecutor(nil).Execute(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/infinite:small:vision"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
		server.Close()
		if err == nil {
			t.Fatalf("accepted invalid/truncated stream %q", sse)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if _, err := newExecutor(nil).Execute(ctx, pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/infinite:small:vision"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}}); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestExecutorStreamsBeforeUpstreamCompletes(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := newExecutor(nil).ExecuteStream(ctx, pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-stream.Chunks:
		if chunk.Err != nil || len(chunk.Payload) == 0 {
			t.Fatalf("first chunk %+v", chunk)
		}
	case <-ctx.Done():
		t.Fatal("buffered complete stream")
	}
}

func TestDiscoveryPrefixesFiltersAndDoesNotInventEntitlements(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("bad discovery request")
		}
		io.WriteString(w, `{"data":[{"id":"syn:small:text"},{"id":"infinite:small:vision"},{"id":"hf:example/model"},{"id":"hf:nomic-ai/nomic-embed-text-v1.5"},{"id":"syn:small:text"},{"id":""}]}`)
	}))
	defer server.Close()
	out, err := (catalog{}).ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"example-key","plan":"go"}`), HTTPClient: mockHostClient{server}})
	if err != nil || len(out.Models) != 3 {
		t.Fatalf("discovery %+v %v", out, err)
	}
	for i, want := range []string{"synthetic/syn:small:text", "synthetic/infinite:small:vision", "synthetic/hf:example/model"} {
		if out.Models[i].ID != want {
			t.Fatalf("model %q want %q", out.Models[i].ID, want)
		}
	}
}
