package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthDuplicateKeyAndReload(t *testing.T) {
	p := newAuthProvider()
	ctx := context.Background()
	parse := func(file, raw string) (pluginapi.AuthParseResponse, error) {
		return p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "synthetic", FileName: file, RawJSON: []byte(raw)})
	}
	a, err := parse("first.json", `{"type":"synthetic","api_key":"example-key","plan":"big","weight":2}`)
	if err != nil || !a.Handled || a.Auth.ID == "" {
		t.Fatalf("parse %+v %v", a, err)
	}
	if _, err = parse("second.json", `{"type":"synthetic","api_key":"example-key"}`); err == nil {
		t.Fatal("duplicate key in a second file accepted")
	}
	b, err := parse("first.json", `{"type":"synthetic","api_key":" example-key "}`)
	if err != nil || b.Auth.ID != a.Auth.ID {
		t.Fatalf("reload %+v %v", b, err)
	}
	if _, err = parse("first.json", `{"type":"synthetic","api_key":"replacement-key"}`); err != nil {
		t.Fatal(err)
	}
	if _, err = parse("second.json", `{"type":"synthetic","api_key":"example-key"}`); err != nil {
		t.Fatal("old key ownership not released", err)
	}
	for _, raw := range []string{`{"type":"synthetic","api_key":"a","plan":"invalid"}`, `{"type":"synthetic","api_key":"a","id":"custom"}`, `{"type":"synthetic","api_key":""}`} {
		if _, err = parse("invalid.json", raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err = p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("API key provider offered login")
	}
	if _, err = p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{}); err == nil {
		t.Fatal("API key provider polled login")
	}
	refreshed, err := p.RefreshAuth(ctx, pluginapi.AuthRefreshRequest{AuthID: a.Auth.ID, StorageJSON: a.Auth.StorageJSON, Metadata: a.Auth.Metadata})
	if err != nil || string(refreshed.Auth.StorageJSON) != string(a.Auth.StorageJSON) {
		t.Fatalf("refresh %+v %v", refreshed, err)
	}
}

func TestBuildRequestStripsOnlyProviderPrefixAndPreservesPayload(t *testing.T) {
	for _, tc := range []struct {
		model      string
		stream     bool
		wantStream bool
	}{
		{"synthetic/syn:small:text", false, false},
		{"synthetic/infinite:small:vision", false, true},
		{"synthetic/hf:example/model", true, true},
	} {
		payload, _ := json.Marshal(map[string]any{"model": tc.model, "messages": []any{}, "stream": tc.stream, "temperature": 0.25, "custom": map[string]any{"preserve": true}})
		got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if json.Unmarshal(got.Body, &body) != nil {
			t.Fatal("invalid JSON")
		}
		if got.URL != "https://api.synthetic.new/openai/v1/chat/completions" || got.Method != "POST" || got.Headers.Get("Authorization") != "Bearer example-key" {
			t.Fatalf("bad upstream request %+v", got)
		}
		if body["model"] != tc.model[len("synthetic/"):] || body["stream"] != tc.wantStream || body["temperature"] != 0.25 || body["custom"].(map[string]any)["preserve"] != true {
			t.Fatalf("changed payload %s", got.Body)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"model":"syn:small:text"}`, `{"model":"synthetic/"}`} {
		if _, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(raw), StorageJSON: []byte(`{"api_key":"example-key"}`)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestStaticCatalogOnlyPrefixedChatModels(t *testing.T) {
	c := catalog{}
	static, err := c.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || len(static.Models) < 5 {
		t.Fatalf("catalog %+v %v", static, err)
	}
	reg, err := c.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || len(reg.Models) != len(static.Models) {
		t.Fatalf("registration %+v %v", reg, err)
	}
	for _, m := range static.Models {
		if len(m.ID) <= len("synthetic/") || m.ID[:len("synthetic/")] != "synthetic/" {
			t.Fatalf("unprefixed model %q", m.ID)
		}
	}
}

func TestBuildRequestStripsPinnedHostPrefix(t *testing.T) {
	pinned := map[string]any{"prefix": "acct"}
	for _, tc := range []struct {
		name       string
		model      string
		meta       map[string]any
		wantUp     string
		wantStream bool
	}{
		{"pinned chat", "acct/synthetic/syn:small:text", pinned, "syn:small:text", false},
		{"pinned infinite forces stream", "acct/synthetic/infinite:small:vision", pinned, "infinite:small:vision", true},
		{"host already stripped", "synthetic/syn:small:text", pinned, "syn:small:text", false},
		{"no metadata", "synthetic/syn:small:text", nil, "syn:small:text", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"model": tc.model, "messages": []any{}})
			got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`), AuthMetadata: tc.meta})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if json.Unmarshal(got.Body, &body) != nil || body["model"] != tc.wantUp || (body["stream"] == true) != tc.wantStream {
				t.Fatalf("pinned model not stripped: %s", got.Body)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		model string
		meta  map[string]any
	}{
		{"wrong prefix is not a provider model", "other/synthetic/syn:small:text", pinned},
		{"slash prefix is unpinned and never stripped", "a/b/synthetic/syn:small:text", map[string]any{"prefix": "a/b"}},
		{"pinned bare id has no provider prefix", "acct/syn:small:text", pinned},
		{"pinned provider prefix alone names no model", "acct/synthetic/", pinned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"model": tc.model})
			if _, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`), AuthMetadata: tc.meta}); err == nil {
				t.Fatalf("accepted %q", tc.model)
			}
		})
	}
}

func TestAuthDuplicatePrefixRejectedAcrossFiles(t *testing.T) {
	p := newAuthProvider()
	ctx := context.Background()
	parse := func(file, raw string) error {
		_, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "synthetic", FileName: file, RawJSON: []byte(raw)})
		return err
	}
	if err := parse("first.json", `{"type":"synthetic","api_key":"example-key","prefix":"acct"}`); err != nil {
		t.Fatal(err)
	}
	if err := parse("second.json", `{"type":"synthetic","api_key":"second-key","prefix":"acct"}`); err == nil {
		t.Fatal("second credential accepted under a claimed prefix")
	}
	// The rejected claim leaves no residue: the first file still owns it.
	if err := parse("third.json", `{"type":"synthetic","api_key":"third-key","prefix":"acct"}`); err == nil {
		t.Fatal("rejected claim evicted the owner")
	}
	// The same key under a second file is still a duplicate credential.
	if err := parse("second.json", `{"type":"synthetic","api_key":"example-key","prefix":"acct"}`); err == nil {
		t.Fatal("duplicate key accepted under the same prefix")
	}
	// Reparsing the owning file is a no-op, and slash-bearing prefixes are
	// unpinned: any number of files may share them.
	if err := parse("first.json", `{"type":"synthetic","api_key":"example-key","prefix":"acct"}`); err != nil {
		t.Fatal(err)
	}
	if err := parse("a.json", `{"type":"synthetic","api_key":"key-a","prefix":"has/slash"}`); err != nil {
		t.Fatal(err)
	}
	if err := parse("b.json", `{"type":"synthetic","api_key":"key-b","prefix":"has/slash"}`); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareHTTPDoesNotForwardUntrustedURLOrHeaders(t *testing.T) {
	req, err := prepareHTTP(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "DELETE", URL: "https://example.invalid/private", Headers: http.Header{"Authorization": {"Bearer attacker"}}, Body: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil || req.URL != "https://api.synthetic.new/openai/v1/chat/completions" || req.Method != "POST" || req.Headers.Get("Authorization") != "Bearer example-key" {
		t.Fatalf("unsafe HTTP request %+v %v", req, err)
	}
	var body map[string]any
	_ = json.Unmarshal(req.Body, &body)
	if body["model"] != "syn:small:text" {
		t.Fatalf("prefix not stripped: %s", req.Body)
	}
}
