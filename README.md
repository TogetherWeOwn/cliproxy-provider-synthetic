# Synthetic provider — work in progress

This repository is not yet an installable CLIProxy plugin. Do not deploy it.
It currently contains the Synthetic quota parser, quota normalization and
header cache, plus a preliminary response-local error classifier. No inference,
authentication provider, model discovery, C ABI entry point, release, or
migration tooling is implemented yet.

The quota parser and tests are adapted from
[TogetherWeOwn/cpa-plugin-subscription-pool](https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool)
at commit `ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`, under the MIT license.
The upstream module's provenance notes Synthetic's MIT-licensed Octofriend
client as a reference for wire shapes; it states that no Octofriend code was
copied. The preliminary error classifier is based on the same commit's
`internal/providers/synthetic/module.go`.

Tests use local `httptest` servers only. Run with Go 1.26.10 or newer:

```sh
go test -race ./...
```

The relative `replace` in `go.mod` requires a sibling checkout of
`cliproxy-provider-kit`. TODO: drop the replace once kit v0.1.0 is tagged.

The classifier is not wired to the host and is not proof of host cooldown
semantics. In particular, the v8.0.48 host's plugin RPC error adapter needs
verification: its transient-rate-limit recognition is distinct from merely
returning a 429, and ordinary 404 handling may impose a cooldown. Do not use
these components for routing until real-loader tests establish the behavior.
