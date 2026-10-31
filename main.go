package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider     = "synthetic"
	version      = "0.1.0"
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
	return kit.New(kit.Definition{
		Metadata:     pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-synthetic"},
		Configure:    configureBase,
		Capabilities: pluginapi.Capabilities{AuthProvider: newAuthProvider(), ModelRegistrar: catalog{}, ModelProvider: catalog{}, Executor: ex, ExecutorModelScope: pluginapi.ExecutorModelScopeBoth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota.cache, UsagePlugin: quota},
		OnShutdown:   func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
