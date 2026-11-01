package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider     = "synthetic"
	version      = "0.1.2"
	modelPrefix  = "synthetic/"
	upstreamBase = "https://api.synthetic.new"
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
	auth, err := newAuthProvider()
	if err != nil {
		return nil, err
	}
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-synthetic"},
		ModelNamespace: "synthetic",
		Configure:      configureBase,
		Capabilities:   pluginapi.Capabilities{AuthProvider: auth, ModelRegistrar: catalog{}, ModelProvider: catalog{}, Executor: ex, ExecutorModelScope: pluginapi.ExecutorModelScopeBoth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota.cache, UsagePlugin: quota, ManagementAPI: auth.Login},
		OnShutdown:     func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
