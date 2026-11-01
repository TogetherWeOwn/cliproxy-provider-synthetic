# Synthetic provider for CLIProxyAPI

First-class Synthetic API-key auth files, namespaced model discovery, OpenAI
chat execution and read-only quota cards. Built on
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at `21069bf` (v8.0.49 plugin ABI: answers RPC schema 10, refuses hosts older than schema 10)
and tested with the **real CLIProxyAPI v8.0.49 C-ABI loader** (ABI 1, RPC schema 10).
Requires CLIProxyAPI v8.0.49 or newer.

Multiple distinct-key auth files use the host's native routing and cooldowns.
There is **no Scheduler**, preemptive quota-percentage avoidance, separate
Infinite account lane, or automatic fallback to Infinite. Quota is display-only;
recent observations can supply retry timing after an actual upstream rejection.

**Cutover is not yet recommended.** The pinned host has a verified concurrency
classification limitation described below. Mock verification is not live-account
verification. See [migration](docs/MIGRATION.md).

## Auth files

Create one private JSON file per API key in the CLIProxy auth directory, using
mode `0600`. Never put real auth files in this repository.

```json
{
  "type": "synthetic",
  "api_key": "<synthetic-api-key>",
  "label": "<account-label>",
  "plan": "big",
  "weight": 1,
  "priority": 0,
  "disabled": false
}
```

`plan` is an optional hint (`go`, `standard`, `big`), **not** proof of entitlement.
The default/fixed ID is `synthetic:` followed by the first 12 hexadecimal digits
of SHA-256 of the trimmed key. User-supplied `id` is rejected. Duplicate keys in
different files are configuration errors; reparsing the same filename is safe.
Two credentials must never share one host `prefix`: their per-account pinning
namespaces would merge, so the second file is rejected with `duplicate_prefix`.
Changing a key in a file releases that file's old key. Renaming/deleting auth
files requires a plugin lifecycle restart to clear its in-memory duplicate-file
ownership; do not copy an existing key into another file during a rename.

Optional host fields include `prefix`, `proxy_url`, `weight`, `priority`, `label`
and `disabled`. The optional auth `prefix` does not replace the fixed model
namespace. It is normalized exactly like the host filestore: surrounding
whitespace and `/` are trimmed, and a `prefix` still containing `/` is rejected
with `invalid_auth` — the plugin path pins whatever parse returns verbatim, so
a slashed prefix could otherwise merge two pinning namespaces while looking
unpinned. Under per-credential pinning the payload model may still carry the
host prefix (`<acct>/synthetic/<id>`); one leading `<prefix>/` segment is
stripped before the provider-prefix check, so pinned and unpinned callers take
the same path. Login/poll operations report API-key-only; refresh preserves storage
and performs no upstream request. API keys cannot be refreshed by this plugin.

## Models and execution

Every registered/discovered chat model has the fixed `synthetic/` prefix.
Only that leading namespace is stripped before the upstream request. Discovery
sends Bearer auth to `/openai/v1/models`; the static registrar supplies stable
aliases if discovery is unavailable. Embedding models are outside v1's scope.
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
tool calls, reasoning and summed reported usage. Truncated/malformed streams
fail rather than invent a successful result. Aggregation is bounded to 32 MiB
and 2 MiB per SSE line. It does not change which account/model the host selects.

## Quota and failure classification

`GET https://api.synthetic.new/v2/quotas` is read-only and uses Bearer auth.
The quota client has a 15-second timeout, 1 MiB body cap, no redirects and
redacted errors. It goes through the auth's `proxy_url`, else the host's global
`proxy-url`, like inference. The shared kit caches successful per-auth readings for 60
seconds. The UsagePlugin updates the same cache from `x-synthetic-quotas` headers,
without polling. Expired observations are discarded, not projected or displayed
as fresh; polling errors do not fabricate quota.

Parser priority is `planWeeklyLimit` > `weeklyTokenLimit` >
`rollingFiveHourLimit` > legacy `subscription`. Current weekly credits are shown
as remaining fraction and percent. A regeneration timestamp is the **next
increment, not a full weekly reset**. Legacy five-hour/subscription cards appear
only if those shapes are actually returned. The legacy `subscription` shape
reports absolute counters, surfaced in Summary as `subscription_requests_used`
and `subscription_request_limit` so callers derive utilization instead of
dividing by an assumed limit. Classification hints are bound to the credential
generation that produced them: a rotated key under a reused identity never
inherits the previous generation's regeneration evidence, while header
observations (which carry no credential context) serve every generation until
the 60-second TTL expires. No numeric plan allowance is
hard-coded. Quota reset is unsupported; a web-UI reset entitlement is not an API.

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

The v8.0.49 `internal/pluginhost/rpc_client.go` error adapter carries HTTP status
and retry timing, but does **not** implement the native transient-rate-limit
marker or preserve the RPC `retryable` flag. Consequently a concurrency-only 429
can still set `ModelStates[model].Quota.Exceeded` in native cooldown handling,
despite the plugin's correct classification. The integration test reproduces
this with positive credits. Do not claim native transient/exhaustion parity.

The host also controls actual cooldown scope/duration: 401/402/403 currently get
a 30-minute model cooldown regardless of the one-hour RPC hint; ordinary 404 may
get a 12-hour **model** cooldown. A failing payment auth can still rotate to a
healthy sibling, as verified through the real auth manager. An upstream host
RPC contract change and a new pinned-host test are required to close the
concurrency gap; this repository contains no core patch or HTTP-status disguise.

## Build, test and package

Requires Go 1.26.10, CGO, a C compiler, `nm`, Python 3 and `zip` on Linux/amd64.
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

The package script creates `synthetic_0.1.3_linux_amd64.zip` containing
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
- [Pinned host source](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.49)
- [Parser provenance](docs/PROVENANCE.md)
