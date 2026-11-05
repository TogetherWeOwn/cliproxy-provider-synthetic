package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ownership is by filename, so a watcher's repeated parse of one file is safe.
// Only digests are retained; keys never appear in duplicate-file errors.
//
// The provider is API-key only: it registers no AuthProvider, so the
// management UI shows no login card and auth files load through the host's
// generic file path. ParseAuth stays as the documented auth-file
// validation beside the JSON shape in the README. Its claims do not validate
// installed host auths or reserve their prefixes.
type authProvider struct {
	kit.APIKeyAuth
	claims kit.PrefixClaims
}

func newAuthProvider() *authProvider {
	return &authProvider{APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(m map[string]any) error {
		if _, ok := m["id"]; ok {
			return &pluginabi.Error{Code: "invalid_auth", Message: "auth identity is derived from the API key", HTTPStatus: 400}
		}
		if p, ok := m["plan"]; ok && p != "go" && p != "standard" && p != "big" {
			return &pluginabi.Error{Code: "invalid_auth", Message: "plan must be go, standard or big", HTTPStatus: 400}
		}
		return nil
	}}}
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
	if err := p.claims.Claim(r.FileName, out.Auth.Prefix, []byte(key)); err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	return out, nil
}
