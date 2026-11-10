package main

import (
	"context"
	"net/http"
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Model serving is delegated to the kit ModelCatalog (v0.5.0): every
// ModelsForAuth invocation attempts one fresh fetch (fetch → glob →
// overrides → Validate → cache → serve); a failed fetch, oversized body or
// failed Validate falls back to the durable last-good list for that
// credential generation, then to Static. No background fetch runs: a failed
// one-shot callback is not retried by the kit and recovery waits for the
// next host event. Static models are upstream (pre-namespace) ids; the
// catalog prefixes them with modelNamespace.
func fallbackModels() []pluginapi.ModelInfo {
	ids := []string{"syn:large:text", "syn:small:text", "syn:large:vision", "syn:small:vision", "infinite:small:vision"}
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}

// modelGlobs translates the previous hand-rolled discovery filter into the
// kit's path globs. Deny wins and "*" does not cross "/", so the hf shapes
// need both a slashless and a slashed pattern:
//
//   - allowed upstream shapes are the chat namespaces the v1 executor
//     implements (syn:, infinite:, hf:<org>/<model>);
//   - embedding models are outside v1's scope. The case-sensitive globs
//     below cover the common lowercase shapes; validateModels is the
//     authoritative case-insensitive check for the rest.
func modelGlobs() (allow, deny []string) {
	return []string{"syn:*", "infinite:*", "hf:*", "hf:*/*"}, []string{"*embed*", "*/*embed*"}
}

// validateModels is the kit post-fetch hook. It runs after the globs and
// overrides, before namespacing, on the fresh path only. It mirrors the
// previous discovery rule that embedding models are dropped
// case-insensitively: glob deny is case-sensitive, so an id such as
// "syn:EMBED-model" which passed the allow globs is still rejected here.
func validateModels(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.ID), "embed") {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// fetchModels is this plugin's OpenAI-style discovery fetch. The kit
// fetcher is built per call so the effective base URL is resolved through
// selectedBase at request time: the hostcompat test build points discovery
// at its loopback mock while production targets api.synthetic.new, exactly
// like the previously hand-wired fetch. OpenAIModelsFetcher sends the
// Bearer auth derived from the request; the kit never derives credentials
// itself. The catalog enforces the kit's 15s ModelsFetchTimeout deadline
// and 1 MiB post-return parse cap (MaxModelsBody) around the fetch: an
// oversized buffered body is a failed fetch (never parsed, never cached as
// last-good). The kit also bounds a nil host client to the same fallback:
// when nothing is available the fetch error is surfaced by the catalog.
func fetchModels(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	// Cheap local validation before any network use; the catalog turns
	// the error below into a last-good/static fallback for this
	// credential generation instead of surfacing it directly.
	if _, err := kit.APIKey(req.StorageJSON, req.Metadata); err != nil {
		return nil, err
	}
	fetcher := kit.OpenAIModelsFetcher(selectedBase(upstreamBase), "/openai/v1/models", func(r pluginapi.AuthModelRequest) http.Header {
		key, _ := kit.APIKey(r.StorageJSON, r.Metadata)
		return http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}
	})
	raw, err := fetcher(ctx, req)
	if err != nil {
		return nil, err
	}
	// Preserve the previous trimming: upstream ids arrive clean, but a
	// padded id would fail the allow globs untrimmed, so trim before the
	// kit pipeline (globs/overrides) sees it.
	out := make([]pluginapi.ModelInfo, 0, len(raw))
	for _, m := range raw {
		if m.ID = strings.TrimSpace(m.ID); m.ID == "" {
			continue
		}
		// The kit fetcher passes upstream object/owned_by through and
		// never sets a display name, so an upstream list that omits the
		// optional fields would serve models without metadata. Fill the
		// same defaults the hand-rolled discovery always produced
		// (overrides may still replace the display name downstream).
		if m.Object == "" {
			m.Object = "model"
		}
		if m.OwnedBy == "" {
			m.OwnedBy = provider
		}
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		out = append(out, m)
	}
	return out, nil
}

func newModelCatalog() *kit.ModelCatalog {
	allow, deny := modelGlobs()
	return kit.NewModelCatalog(kit.ModelCatalogConfig{
		Namespace: modelNamespace,
		Static:    fallbackModels(),
		Fetch:     fetchModels,
		Allow:     allow,
		Deny:      deny,
		Validate:  validateModels,
	})
}
