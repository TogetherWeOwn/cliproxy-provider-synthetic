package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every id this plugin can emit carries the fixed "synthetic/" namespace
// exactly once: static fallback, registrar, and a discovery refresh.
func TestModelNamespaceConformance(t *testing.T) {
	ctx := context.Background()
	var all []pluginapi.ModelInfo
	all = append(all, fallbackModels()...)

	reg, err := catalog{}.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, reg.Models...)

	static, err := catalog{}.StaticModels(ctx, pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, static.Models...)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"syn:large:text"},{"id":"infinite:small:vision"},{"id":"hf:example/model"}]}`)
	}))
	defer server.Close()
	discovered, err := catalog{}.ModelsForAuth(ctx, pluginapi.AuthModelRequest{StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, discovered.Models...)

	if len(all) == 0 {
		t.Fatal("no models to check")
	}
	kit.AssertModelNamespace(t, "synthetic", all)
}
