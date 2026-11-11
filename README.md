# Synthetic provider for CLIProxyAPI

First-class Synthetic API-key auth files, namespaced model discovery, OpenAI
chat execution and read-only quota cards. Built on
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at tag `v0.6.0` (`c860882`) (v8.0.63 plugin ABI: answers RPC schema 10, refuses hosts older than schema 10)
and tested with the **real CLIProxyAPI v8.0.63 C-ABI loader** (ABI 1, RPC schema 10).
Requires CLIProxyAPI v8.0.63 or newer.

Multiple distinct-key auth files use the host's native routing and cooldowns.
There is **no Scheduler**, preemptive quota-percentage avoidance, separate
Infinite account lane, or automatic fallback to Infinite. Quota is display-only;
recent observations can supply retry timing after an actual upstream rejection.

**Cutover is not yet recommended.** The pinned host has a verified concurrency
classification limitation described below. Mock verification is not live-account
verification. See [migration](docs/MIGRATION.md).

## Auth files

The provider is API-key only: there is no login card in the management UI.
Upload one private JSON file per API key through the management UI's auth-file
upload (the same path built-in API-key providers use), or place the file
directly in the CLIProxy auth directory with mode `0600`. Never put real auth
files in this repository.

```json
{
  "type": "synthetic",
  "api_key": "<synthetic-api-key>",
  "plan": "big",
  "weight": 1,
  "priority": 0,
  "disabled": false
}
```

`plan` is an optional hint (`go`, `standard`, `big`), **not** proof of entitlement.
The auth ID is the file's relative path under the auth directory; a
user-supplied `id` field is stored but ignored. Every file loads as its own
auth, so use one file per key — the same key in two files loads twice instead
of being rejected. Renaming, editing or deleting a file takes effect when the
host rescans the auth directory; no plugin restart is needed. Quota is
display-only and cached for 60 seconds. Polled readings and classification
hints are bound to the credential generation: editing a key in place does not
reuse the old key's polled evidence. Header observations carry no credential
context and remain available to every generation for their TTL.

Optional host fields include `prefix`, `proxy_url`, `weight`, `priority`
and `disabled`. The management UI derives the display label from the file
itself; a file-level `label` is ignored on the generic file path. Fixed model namespace `synthetic/`; auth `prefix` does not
replace it. Refresh preserves storage and performs no upstream request. API
keys cannot be refreshed by this plugin. Uploaded files are not validated
against the shape above at upload time (unknown `plan` values are kept, not
rejected); the unit-tested ParseAuth in `auth.go` documents the intended shape.

## Models and execution

Every registered/discovered chat model has the fixed `synthetic/` prefix.
When per-credential pinning leaves a host prefix in the payload, one leading
`<prefix>/` segment matching the credential metadata is stripped first. The
`synthetic/` namespace is then stripped before the upstream request. A wrong
host prefix is still rejected; this does not change account selection. Discovery
is served by the kit `ModelCatalog` (`providerkit.NewModelCatalog`, namespace
`synthetic`): each per-auth callback performs one OpenAI-style fetch of
`/openai/v1/models` with Bearer auth — the kit's `OpenAIModelsFetcher` in
`models.go` — then applies the allow/deny globs (chat namespaces only;
embedding models denied), the Validate hook, durable per-generation
last-good caching and the `synthetic/` prefix. The catalog derives a 15-second
fetch deadline (the earlier of its own timeout and the parent context wins)
and drops discovery bodies over 1 MiB without parsing them; a failed fetch
falls back to last-good, then to the static registrar's stable aliases. The
kit performs a single fetch per callback with no retry: if that fetch fails,
recovery waits for the next host event, so failure-then-recovery is never
promised. Embedding models are outside v1's scope.
Discovery does not assume that a `plan` hint grants access to any model.

| Client model ID | Upstream model ID | Notes |
| --- | --- | --- |
| `synthetic/syn:large:text` | `syn:large:text` | Stable alias |
| `synthetic/syn:small:text` | `syn:small:text` | Stable alias |
| `synthetic/syn:large:vision` | `syn:large:vision` | Stable alias |
| `synthetic/syn:small:vision` | `syn:small:vision` | Stable alias |
| `synthetic/infinite:small:vision` | `infinite:small:vision` | Requires upstream entitlement; currently Big-only |
| `synthetic/hf:<organization>/<model>` | `hf:<organization>/<model>` | Discovered pinned IDs can rotate and return 404 |

Inference uses `POST https://api.synthetic.new/openai/v1/chat/completions`,
Bearer auth and the injected **host HTTP client**, preserving host transport,
proxy and request-logging policy. Ordinary non-stream responses are passed
through; stream responses are relayed incrementally, not buffered to completion.
Arbitrary raw-HTTP destinations and caller authorization headers are never
forwarded. Token counting is a local, explicitly estimated count, not a tokenizer
or an upstream billable operation.

`infinite:*` always sends upstream `stream:true`. For a non-streaming client, SSE
is aggregated into one `chat.completion`, including indexed choices, fragmented
tool calls, reasoning and summed reported usage. Malformed streams, and streams
that end before every choice carries a finish reason, fail rather than invent
a successful result; a clean channel close after complete choices is the
terminal marker (the kit drops the upstream `[DONE]`), so a stream cut after
its final choice succeeds with complete content. Aggregation is bounded to 32 MiB
total; each upstream event is additionally bounded by the kit's 8 MiB SSE event
limit. It does not change which account/model the host selects.

## Quota and failure classification

`GET https://api.synthetic.new/v2/quotas` is read-only and uses Bearer auth.
The quota client has a 15-second timeout, 1 MiB body cap, no redirects and
redacted errors. It goes through the auth's `proxy_url`, else the host's global
`proxy-url`, like inference. The shared kit caches successful per-auth readings for 60
seconds. The UsagePlugin updates the same cache from `x-synthetic-quotas` headers,
without polling. Expired observations are discarded, not projected or displayed
as fresh; polling errors do not fabricate quota.

Every shape the quota payload reports gets one bucket in a single `Synthetic`
group: weekly (`planWeeklyLimit`, `weeklyTokenLimit`) as `weekly`, rolling
five-hour (`rollingFiveHourLimit`) as `5h`, and legacy `subscription` as
`Renewal` (short enough to render beside its renewal timestamp).
Buckets carry the window id only, with no description. The legacy
`subscription` shape also exposes `subscription_requests_used` and
`subscription_request_limit` as absolute Summary counters so callers can
derive utilization. Regeneration percents are incremental hints, not plan
data, and never become refill chips. A regeneration timestamp is the **next increment, not a full
reset**, so it is never shown for weekly ticks — only `subscription`
renewals reset the allowance and keep their reset time. The optional auth
`plan` field (go, standard or big) surfaces as the plan chip. Each quota fetch
attaches the current auth fields after reading cached quota data, so header
updates cannot repeat a previous plan after an edit or key rotation. Legacy
five-hour/subscription buckets appear only if those shapes are actually
returned. No numeric plan allowance is hard-coded. Quota reset is unsupported;
a web-UI reset entitlement is not an API.

| Upstream result | Plugin classification | Retry guidance |
| --- | --- | --- |
| 401 | `invalid_credentials` | Non-retryable |
| 402 | `payment_required` | Non-retryable; one-hour hint |
| 403 | `entitlement_required` | Non-retryable; one-hour hint |
| 404 | `upstream_not_found` | Non-retryable; no auth-exhaustion claim |
| 429 with concurrency wording **and response-local positive remaining credits** | `concurrency_limit` | Retryable; 5 seconds by default |
| Other 429 | `quota_exhausted` | Retryable; next regeneration from response/fresh cached evidence, otherwise 60 seconds |
| 408 / 5xx | `upstream_error` | Retryable |

A valid upstream `Retry-After` overrides the 429/default server-error timing.
Bodies and secrets are never included in public error messages.

### Pinned host RPC limitation

The v8.0.63 `internal/pluginhost/rpc_client.go` error adapter carries HTTP status
and retry timing, but a concurrency-only 429 still cannot reach the native
transient-rate-limit path: the plugin reports code `concurrency_limit` while the
adapter only recognizes `transient_rate_limit`, and the RPC `retryable` flag is
not preserved either. Consequently a concurrency 429 falls into the generic
429 path and can still set `ModelStates[model].Quota.Exceeded` in native
cooldown handling, despite the plugin's correct classification. The integration
test reproduces this with positive credits. Do not claim native
transient/exhaustion parity.

The host also controls actual cooldown scope/duration: 401/402/403 currently get
a 30-minute model cooldown regardless of the one-hour RPC hint; ordinary 404 may
get a 12-hour **model** cooldown. A failing payment auth can still rotate to a
healthy sibling, as verified through the real auth manager. An upstream host
RPC contract change and a new pinned-host test are required to close the
concurrency gap; this repository contains no core patch or HTTP-status disguise.

## Build, test and package

Requires Go 1.26.11, CGO, a C compiler, `nm`, Python 3 and `zip` on Linux/amd64.
There are no `replace` directives or sibling-checkout dependencies.

```sh
go vet ./...
go test -race -count=1 ./...
go -C integration/hostcompat vet ./...
go -C integration/hostcompat test -race -count=1 ./...
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o <output-dir>/synthetic.so .
nm -D <output-dir>/synthetic.so | grep cliproxy_plugin_init
bash scripts/package-release.sh <output-dir>
```

The package script creates `synthetic_0.2.5_linux_amd64.zip` containing
`synthetic.so` and `LICENSE`, plus `checksums.txt` for the archive. It does not
publish, install or reload anything. Publish both files together to a matching
GitHub release; `registry.json` uses the native schema-v1 GitHub-release contract.
The registry is release metadata, not a claim that an asset is already published.

CI checks formatting, vet, race tests (including packaging), real-loader
compatibility, the production C-shared entry point, gitleaks and supplemental
public-file hygiene. All upstream tests use fake keys and loopback `httptest`
servers. The hostcompat-tagged library rejects non-loopback upstreams and is
**test-only**; never distribute it as the production release.

## Pending live verification

- Resolve the host RPC concurrency-classification gap before cutover.
- Verify current quota shape, account plan and regeneration semantics on an
  isolated staging CLIProxy with separately authorized credentials.
- Verify discovered model availability and Big-only Infinite entitlement.
- With explicit authorization, verify one small normal stream and one Infinite
  stream, non-stream aggregation, native quota cards and sibling cooldowns.
- Confirm actual exhaustion status/body/headers without deliberately draining
  an account, and review applicable account/credential-sharing terms.

**No real provider inference, quota or model endpoint is called by this test
suite, and no live-account validation is claimed.**

## Sources

- [API overview](https://dev.synthetic.new/docs/api/overview)
- [Models and Infinite](https://dev.synthetic.new/docs/api/models)
- [Quota API](https://dev.synthetic.new/docs/synthetic/quotas)
- [Weekly regeneration](https://synthetic.new/rate-limits)
- [Official client quota shapes](https://github.com/synthetic-lab/octofriend/blob/main/source/utils/quota.ts)
- [Pinned host source](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.63)
- [Parser provenance](docs/PROVENANCE.md)
