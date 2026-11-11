package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider       = "synthetic"
	displayName    = "Synthetic"
	version        = "0.2.5"
	modelPrefix    = "synthetic/"
	modelNamespace = "synthetic"
	upstreamBase   = "https://api.synthetic.new"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	client := newQuotaHTTPClient()
	quota := newQuotaProvider("", client)
	ex := newExecutor(quota)
	cat := newModelCatalog()
	// API-key only: no AuthProvider and no ManagementAPI login
	// routes, so the management UI shows no login card. Auth files load
	// through the host's generic file path.
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: displayName, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-synthetic", Logo: "https://www.google.com/s2/favicons?domain=synthetic.new"},
		ModelNamespace: modelNamespace,
		Configure:      configureBase,
		Capabilities:   pluginapi.Capabilities{ModelRegistrar: cat, ModelProvider: cat, Executor: ex, ExecutorModelScope: pluginapi.ExecutorModelScopeBoth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota, UsagePlugin: quota},
		OnShutdown:     func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
