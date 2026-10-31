# Protocol and parser provenance

The quota parser and its original shape tests are adapted from the MIT-licensed
[TogetherWeOwn/cpa-plugin-subscription-pool](https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool)
commit `ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`, specifically
`internal/providers/synthetic/quota.go`. The failure classification is based on
response handling in that commit's `internal/providers/synthetic/module.go`.
The MIT license is included at the repository root and in release archives.

That implementation's provenance cites Synthetic's MIT-licensed
[Octofriend quota utility](https://github.com/synthetic-lab/octofriend/blob/main/source/utils/quota.ts)
and
[OpenAI error adapter](https://github.com/synthetic-lab/octofriend/blob/main/source/libocto/compilers/openai-shared.ts)
as behavioral references; it states that no Octofriend code was copied. This
provider's new C-ABI/auth/model/executor wiring and SSE aggregator use the
published provider kit and the pinned CLIProxy SDK, not an additional routing
engine.

Published sources establish the Bearer authorization, OpenAI endpoint, Infinite
streaming requirement and quota/header shapes. They do not establish guaranteed
per-tier numeric limits, concurrency allowances or exact exhaustion error
contracts for all accounts. Those remain pending live verification; historical
fixtures are not current entitlement guarantees.

The native behavior references are CLIProxyAPI v8.0.48:

- `sdk/pluginabi/types.go`: ABI/RPC envelopes and retry timing.
- `internal/pluginhost/rpc_client.go`: RPC-to-host error adaptation.
- `sdk/cliproxy/auth/conductor_cooldown.go`: native cooldown state.
- `internal/pluginhost/adapters_executors.go`: executor registration/ownership.
- `internal/pluginstore/registry.go` and `github.go`: registry/release assets.

See the README for the verified RPC transient-marker limitation and
`integration/hostcompat/compat_test.go` for its executable characterization.
