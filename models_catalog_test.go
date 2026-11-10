package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Fixture tests for the kit ModelCatalog migration (kit v0.5.0), adapted to
// this plugin's fetch shape: one OpenAI-style GET /openai/v1/models through
// the host HTTP client, Bearer auth per request, the synthetic/ namespace,
// the syn:/infinite:/hf: allow globs with embedding models denied, and the
// five stable aliases as Static. They exercise the kit pipeline
// (fetch → glob → overrides → Validate → cache → serve) at the released
// version; the kit's own suite covers the generic machinery.

// stubFetch is a controllable FetchModelsFunc: tests flip its result between
// invocations to model fresh convergence, transient failure and rotation.
type stubFetch struct {
	models []pluginapi.ModelInfo
	err    error
	calls  int
}

func (s *stubFetch) fetch(context.Context, pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return slices.Clone(s.models), nil
}

func stubModels(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", DisplayName: id})
	}
	return out
}

// modelTestCatalog mirrors newModelCatalog's production wiring with an
// injected fetch so fixture tests exercise this plugin's real globs, static
// list and Validate hook instead of a re-declared copy.
func modelTestCatalog(fetch kit.FetchModelsFunc, overrides map[string]kit.ModelOverride) *kit.ModelCatalog {
	allow, deny := modelGlobs()
	return kit.NewModelCatalog(kit.ModelCatalogConfig{
		Namespace: modelNamespace,
		Static:    fallbackModels(),
		Fetch:     fetch,
		Allow:     allow,
		Deny:      deny,
		Validate:  validateModels,
		Overrides: overrides,
	})
}

func catalogReq(key string) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{
		AuthID:       "auth-1",
		AuthProvider: provider,
		StorageJSON:  []byte(`{"api_key":"` + key + `"}`),
		Plugin:       pluginapi.Metadata{Name: provider},
	}
}

func modelIDs(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// OpenAI-style parse through the production fetch path plus override
// application on both the fresh and static pipeline paths.
func TestModelCatalogParseAndOverrides(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("bad discovery request")
		}
		io.WriteString(w, `{"data":[{"id":"syn:small:text","object":"model","created":123,"owned_by":"synthetic"},{"id":"hf:acme/widget"}]}`)
	}))
	defer server.Close()

	// Overrides come through the same pipeline for fresh and static
	// results. The production catalog ships none; assert the mechanism
	// with an identical test catalog on this plugin's ids and fetch.
	over := modelTestCatalog(fetchModels, map[string]kit.ModelOverride{
		"syn:small:text":        {DisplayName: "Syn Small Text"},
		"infinite:small:vision": {DisplayName: "Infinite Small Vision"},
	})

	req := catalogReq("example-key")
	req.HTTPClient = mockHostClient{server}
	resp, err := over.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text", "synthetic/hf:acme/widget"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v", got, want)
	}
	if resp.Models[0].DisplayName != "Syn Small Text" || resp.Models[0].Created != 123 || resp.Models[0].OwnedBy != "synthetic" {
		t.Fatalf("parsed/overridden %+v, want created/owned_by carried and DisplayName overridden", resp.Models[0])
	}

	sresp, err := over.StaticModels(context.Background(), pluginapi.StaticModelRequest{Plugin: pluginapi.Metadata{Name: provider}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range sresp.Models {
		if m.ID == "synthetic/infinite:small:vision" {
			found = true
			if m.DisplayName != "Infinite Small Vision" {
				t.Fatalf("static display %q, want overridden", m.DisplayName)
			}
		}
	}
	if !found {
		t.Fatalf("static %+v, want the overridden alias present", modelIDs(sresp.Models))
	}
}

// The case-insensitive embedding drop lives in Validate, not the
// case-sensitive globs: "syn:EMBED-model" passes the syn allow glob and
// "hf:embedorg/model" passes the hf allow glob without tripping either
// glob-deny pattern, so both must be dropped by Validate alone.
func TestModelCatalogValidateDropsCaseInsensitiveEmbeds(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:EMBED-model", "hf:embedorg/model", "syn:small:text")}
	cat := modelTestCatalog(fix.fetch, nil)
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v (only the non-embedding model survives)", got, want)
	}
}

// Padded upstream ids are trimmed before the kit pipeline so they still
// pass the allow globs; whitespace-only ids are dropped.
func TestModelCatalogPaddedIDsTrimmed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":" syn:small:text "},{"id":"  "},{"id":"hf:acme/widget"}]}`)
	}))
	defer server.Close()
	req := catalogReq("example-key")
	req.HTTPClient = mockHostClient{server}
	resp, err := newModelCatalog().ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text", "synthetic/hf:acme/widget"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v", got, want)
	}
}

// Production failure modes all land on the static list with no error: a
// nil host client, a credential without an api_key, and a non-2xx upstream
// are failed fetches, never surfaced (nothing else is available to serve).
func TestModelCatalogFailureFallsBackToStatic(t *testing.T) {
	cat := newModelCatalog()

	nilClient, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil || len(nilClient.Models) != 5 {
		t.Fatalf("nil client %+v %v, want the static list and no error", modelIDs(nilClient.Models), err)
	}

	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"syn:small:text"}]}`)
	}))
	defer goodServer.Close()
	badKey := catalogReq("example-key")
	badKey.StorageJSON = []byte(`{"plan":"go"}`)
	badKey.HTTPClient = mockHostClient{goodServer}
	noKey, err := cat.ModelsForAuth(context.Background(), badKey)
	if err != nil || len(noKey.Models) != 5 {
		t.Fatalf("no api_key %+v %v, want the static list and no error", modelIDs(noKey.Models), err)
	}

	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "boom")
	}))
	defer errServer.Close()
	non2xx := catalogReq("other-key")
	non2xx.HTTPClient = mockHostClient{errServer}
	got, err := cat.ModelsForAuth(context.Background(), non2xx)
	if err != nil || len(got.Models) != 5 {
		t.Fatalf("non-2xx %+v %v, want the static list and no error", modelIDs(got.Models), err)
	}
	if got.Models[0].ID != "synthetic/syn:large:text" {
		t.Fatalf("fallback %+v, want the stable aliases in order", modelIDs(got.Models))
	}
}

// Contributor pairing with and without fixtures, plus the executor-mapping
// assertion: every emitted id strips exactly one synthetic/ prefix in the
// executor and reaches the upstream unchanged.
func TestModelCatalogContributorFixturesAndExecutorMapping(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:large:text", "syn:large:text-contributor", "syn:small:text", "hf:acme/widget")}
	cat := modelTestCatalog(fix.fetch, nil)
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:large:text-contributor", "synthetic/syn:small:text", "synthetic/hf:acme/widget"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v (contributor wins, base dropped, solo kept)", got, want)
	}

	// Without any -contributor sibling in the fixture, base ids are kept
	// as-is.
	solo := &stubFetch{models: stubModels("syn:large:text", "syn:small:vision")}
	resp, err = modelTestCatalog(solo.fetch, nil).ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:large:text", "synthetic/syn:small:vision"}; !slices.Equal(got, want) {
		t.Fatalf("solo ids %v, want %v", got, want)
	}

	// Executor mapping: buildRequest strips one provider prefix and the
	// upstream model is the contributor (or base) id verbatim.
	for _, emitted := range []string{"synthetic/syn:large:text-contributor", "synthetic/syn:small:text", "synthetic/hf:acme/widget"} {
		payload, _ := json.Marshal(map[string]any{"model": emitted, "messages": []any{}})
		got, err := buildRequest(context.Background(), pluginapi.ExecutorRequest{Payload: payload, StorageJSON: []byte(`{"api_key":"example-key"}`)})
		if err != nil {
			t.Fatalf("buildRequest %q: %v", emitted, err)
		}
		var body map[string]any
		if err := json.Unmarshal(got.Body, &body); err != nil {
			t.Fatal("invalid JSON")
		}
		if want := strings.TrimPrefix(emitted, modelPrefix); body["model"] != want {
			t.Fatalf("emitted %q maps upstream to %v, want %q", emitted, body["model"], want)
		}
	}
}

// Timeline A: fresh convergence. A TTL that covers both invocations must
// not suppress the second fetch: every ModelsForAuth invocation fetches, so
// a newly published model converges on the next host event.
func TestModelCatalogTimelineAFreshConverges(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:small:text")}
	allow, deny := modelGlobs()
	cat := kit.NewModelCatalogWithTTL(kit.ModelCatalogConfig{
		Namespace: modelNamespace,
		Fetch:     fix.fetch,
		Allow:     allow,
		Deny:      deny,
		Validate:  validateModels,
	}, time.Hour)
	if _, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key")); err != nil {
		t.Fatal(err)
	}
	fix.models = stubModels("syn:small:text", "hf:acme/widget")
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text", "synthetic/hf:acme/widget"}; !slices.Equal(got, want) {
		t.Fatalf("timeline A served %v, want fresh convergence %v", got, want)
	}
	if fix.calls != 2 {
		t.Fatalf("fetches %d, want every invocation to fetch (2)", fix.calls)
	}
}

// Timeline B: failure-then-no-recovery, documented and never claimed as a
// guarantee. The released kit performs a single fetch per ModelsForAuth
// invocation with no bounded retry, so when the one-shot callback's fetch
// fails transiently the catalog serves the durable last-good list and does
// not publish the new model; recovery waits for the next host event.
func TestModelCatalogTimelineBFailureThenNoGuaranteedRecovery(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:small:text")}
	cat := modelTestCatalog(fix.fetch, nil)
	if _, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key")); err != nil {
		t.Fatal(err)
	}
	fix.models = stubModels("syn:small:text", "hf:acme/widget")
	fix.err = errors.New("transient fetch failure")
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text"}; !slices.Equal(got, want) {
		t.Fatalf("timeline B served %v, want last-good %v (new model unpublished)", got, want)
	}
	before := fix.calls
	time.Sleep(50 * time.Millisecond)
	if fix.calls != before {
		t.Fatalf("fetches %d, want %d: no background fetch publishes the model without a host callback", fix.calls, before)
	}
}

// TTL bounds only the kit's freshness markers; the durable last-good store
// has no TTL eviction. After freshness expiry a failing fetch still serves
// last-good, never static.
func TestModelCatalogTTLLastGoodIsolation(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:small:text")}
	allow, deny := modelGlobs()
	cat := kit.NewModelCatalogWithTTL(kit.ModelCatalogConfig{
		Namespace: modelNamespace,
		Static:    fallbackModels(),
		Fetch:     fix.fetch,
		Allow:     allow,
		Deny:      deny,
		Validate:  validateModels,
	}, time.Millisecond)
	if _, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	fix.err = errors.New("boom")
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("example-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text"}; !slices.Equal(got, want) {
		t.Fatalf("past-TTL served %v, want durable last-good %v", got, want)
	}
}

// A rotated credential under the same auth identity never receives the old
// generation's last-good list; its failing fetch falls back to static. The
// original generation still gets its own last-good.
func TestModelCatalogCredentialGenerationIsolation(t *testing.T) {
	fix := &stubFetch{models: stubModels("syn:small:text")}
	cat := modelTestCatalog(fix.fetch, nil)
	reqA := catalogReq("example-key")
	if _, err := cat.ModelsForAuth(context.Background(), reqA); err != nil {
		t.Fatal(err)
	}
	fix.err = errors.New("boom")
	resp, err := cat.ModelsForAuth(context.Background(), catalogReq("rotated-key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 5 || resp.Models[0].ID != "synthetic/syn:large:text" {
		t.Fatalf("rotated credential served %v, want the static list, not stale last-good", modelIDs(resp.Models))
	}
	resp, err = cat.ModelsForAuth(context.Background(), reqA)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text"}; !slices.Equal(got, want) {
		t.Fatalf("original generation served %v, want last-good %v", got, want)
	}
}

// Cancellation: a parent cancel during the fetch, and a pre-cancelled
// context, both stay bounded. The blocking host client records that the
// derived fetch context reached it, and the catalog falls back to static.
type blockingHostClient struct {
	entered chan struct{}
	sawDone bool
}

func (b *blockingHostClient) Do(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	b.sawDone = true
	return pluginapi.HTTPResponse{}, ctx.Err()
}

func (b *blockingHostClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming not supported in fixture")
}

func TestModelCatalogCancellationAndEnforceableDeadline(t *testing.T) {
	t.Run("parent cancel during fetch", func(t *testing.T) {
		host := &blockingHostClient{entered: make(chan struct{}, 1)}
		cat := newModelCatalog()
		req := catalogReq("example-key")
		req.HTTPClient = host
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan pluginapi.ModelResponse, 1)
		go func() {
			resp, _ := cat.ModelsForAuth(ctx, req)
			done <- resp
		}()
		select {
		case <-host.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("fetch never reached the host call")
		}
		cancel()
		select {
		case resp := <-done:
			if len(resp.Models) != 5 {
				t.Fatalf("canceled served %v, want the static list", modelIDs(resp.Models))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("canceled call did not return")
		}
		if !host.sawDone {
			t.Fatal("host client never saw the fetch context cancel")
		}
	})

	t.Run("parent deadline bounds the fetch", func(t *testing.T) {
		host := &blockingHostClient{entered: make(chan struct{}, 1)}
		cat := newModelCatalog()
		req := catalogReq("example-key")
		req.HTTPClient = host
		// The kit derives its contexts with ModelsFetchTimeout (15s) but a
		// parent with an earlier deadline wins: the earlier of the two
		// governs, so the fetch stays enforceably bounded here.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		resp, err := cat.ModelsForAuth(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("blocked fetch returned after %s, want bounded by the derived deadline", elapsed)
		}
		if len(resp.Models) != 5 {
			t.Fatalf("deadline served %v, want the static list", modelIDs(resp.Models))
		}
		if !host.sawDone {
			t.Fatal("host client never saw the derived deadline cancel its context")
		}
	})

	t.Run("pre-cancelled context errors without fetching", func(t *testing.T) {
		fix := &stubFetch{models: stubModels("syn:small:text")}
		cat := modelTestCatalog(fix.fetch, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := cat.ModelsForAuth(ctx, catalogReq("example-key")); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-cancelled err = %v, want context.Canceled", err)
		}
		if fix.calls != 0 {
			t.Fatalf("fetches %d, want 0 for a pre-cancelled call", fix.calls)
		}
	})
}

// Size fallback: an oversized discovery body (over the kit's 1 MiB
// MaxModelsBody post-return parse cap) is a failed fetch — never parsed and
// never cached as last-good. With no last-good it serves static; after a
// good fetch it keeps serving the untouched last-good.
func TestModelCatalogOversizedBodyFallsBack(t *testing.T) {
	oversize := make([]byte, kit.MaxModelsBody+1)
	oversizeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(oversize)
	}))
	defer oversizeServer.Close()

	cat := newModelCatalog()
	bad := catalogReq("example-key")
	bad.HTTPClient = mockHostClient{oversizeServer}
	resp, err := cat.ModelsForAuth(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 5 {
		t.Fatalf("oversized served %v, want the static list", modelIDs(resp.Models))
	}

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"syn:small:text"}]}`)
	}))
	defer good.Close()
	ok := catalogReq("example-key")
	ok.HTTPClient = mockHostClient{good}
	if _, err := cat.ModelsForAuth(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	resp, err = cat.ModelsForAuth(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"synthetic/syn:small:text"}; !slices.Equal(got, want) {
		t.Fatalf("oversized after good fetch served %v, want untouched last-good %v", got, want)
	}
}
