package main

import (
	"context"
	"crypto/sha256"
	"sync"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ownership is by filename, so a watcher's repeated parse of one file is safe.
// Only digests are retained; keys never appear in duplicate-file errors.
type authProvider struct {
	kit.APIKeyAuth
	mu     sync.Mutex
	files  map[string][32]byte
	owners map[[32]byte]string
}

func newAuthProvider() (*authProvider, error) {
	p := &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(m map[string]any) error {
		if _, ok := m["id"]; ok {
			return &pluginabi.Error{Code: "invalid_auth", Message: "auth identity is derived from the API key", HTTPStatus: 400}
		}
		if p, ok := m["plan"]; ok && p != "go" && p != "standard" && p != "big" {
			return &pluginabi.Error{Code: "invalid_auth", Message: "plan must be go, standard or big", HTTPStatus: 400}
		}
		return nil
	}}, files: make(map[string][32]byte), owners: make(map[[32]byte]string)}
	// The login page offers the optional plan field; Parse is the full
	// ParseAuth pipeline below, including the duplicate-key guard, so a
	// submitted key faces the same checks as an uploaded auth file.
	login, err := kit.NewAPIKeyLogin(kit.APIKeyLoginConfig{Provider: provider, Parse: p.ParseAuth, Fields: []kit.APIKeyLoginField{
		{Name: "plan", Label: "Plan", Description: "Synthetic plan: go, standard or big. Leave blank to skip.", Required: false},
	}})
	if err != nil {
		return nil, err
	}
	p.APIKeyAuth.Login = login
	return p, nil
}

func (p *authProvider) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	out, err := p.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := kit.APIKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := sha256.Sum256([]byte(key))
	p.mu.Lock()
	defer p.mu.Unlock()
	if file, ok := p.owners[digest]; ok && file != r.FileName {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "duplicate_auth", Message: "the same API key is already configured in another auth file", HTTPStatus: 400}
	}
	if old, ok := p.files[r.FileName]; ok && old != digest {
		delete(p.owners, old)
	}
	p.files[r.FileName] = digest
	p.owners[digest] = r.FileName
	return out, nil
}
