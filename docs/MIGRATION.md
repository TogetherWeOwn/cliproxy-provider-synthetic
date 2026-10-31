# Migration: staging CLIProxy first, then per-provider cutover

This is an operator runbook, not an automated deployment. The build/test/package
commands do not touch a running CLIProxy. Do not cut over while the README's
pinned-host concurrency-classification gap is unresolved.

## 1. Prepare rollback and validate the artifact

1. Privately back up the existing CLIProxy config, auth directory, plugin binary
   and plugin settings. Preserve permissions; never commit the backup or keys.
2. Build and run both test modules, or obtain the reviewed production release.
   Verify the archive with its `checksums.txt`. Do not install a library built
   with the `hostcompat` tag; that tag is only for mock integration tests.
3. Record the current callers/model aliases and expected quota-card behavior.
   Do not assume different keys on one upstream account have independent quota.
4. Plan a provider-scoped rollback. Leave every unrelated provider unchanged.

## 2. Create an isolated staging instance

Use a **second CLIProxy process/instance** with its own configuration, auth dir,
plugin dir and listening port. Preserve the real instance and its old entries.
Bind staging to loopback, with separate access credentials and no automatic
production traffic. Use the same reviewed host version/configuration policy.

Why a second instance: native `openai-compatibility` executors for a provider can
preempt plugin executor ownership, and model claims are first-wins. Running both
implementations for the same provider inside one instance does not prove the
plugin's inference path.

Install the reviewed production plugin through the native plugin store using
this repository's registry source, or place the verified binary in the staging
plugin directory. An illustrative config is below; all locations are placeholders.

```yaml
auth-dir: <staging-auth-directory>
plugins:
  enabled: true
  dir: <staging-plugin-directory>
  store-sources:
    - https://raw.githubusercontent.com/TogetherWeOwn/cliproxy-provider-synthetic/main/registry.json
  configs:
    synthetic:
      enabled: true
```

The native store installs platform binaries beneath `linux/amd64` and enables
the plugin config. Manual installs must follow the host's documented platform
directory layout. Verify the plugin metadata/version and native auth/quota
capabilities rather than inferring success from the presence of a file.

## 3. Migrate credentials without duplicating the key

- The legacy `synthetic` and `synthetic-infinite` compatibility entries can share
  **one API key**. Create **one** private `type: synthetic` auth file for that key,
  not one per old entry. Infinite is a model family on the same auth now.
- For additional distinct legitimate keys, create one file per key. Duplicate
  keys in separate files are rejected at parse time. The plugin does not infer
  independent entitlements from multiple keys on the same upstream account.
- Use the README schema and mode `0600`. API-key values and labels stay local;
  do not print keys into a shell history, test output, issue or public artifact.
- Preserve optional native weights, priorities, proxy and disabled settings.
  The optional `plan` field is only a hint, never proof of Big entitlement.
- Do not set custom auth IDs. They are derived from key fingerprints. Clear the
  plugin lifecycle before moving an existing key between filenames so stale
  in-memory duplicate ownership cannot reject the new filename.

In staging, omit **both** old Synthetic compatibility entries and remove the
Synthetic block from the legacy subscription-pool overlay. Keep other provider
blocks intact. This plugin never registers a scheduler and never uses quota
percentages for preemptive account avoidance.

## 4. Staging acceptance

The repository's tests use mocks only. The following account checks require
separate authorization; they are not performed by the test or packaging tools.

1. Confirm native plugin registration, auth parse/persistence and duplicate-file
   rejection. Confirm a stable auth fingerprint across reloading the same file.
2. Inspect the quota card from `/v2/quotas`. Record the actual shape and plan;
   do not hard-code old five-hour counters or a sample subscription allowance.
   A regeneration timestamp must be labeled as the next increment, not a full
   weekly reset. Confirm reset is unsupported.
3. Confirm model discovery returns only `synthetic/`-prefixed chat IDs and the
   executor strips the namespace before upstream. Prefer stable `syn:` aliases.
4. With authorization for inference, send one small normal stream, one ordinary
   non-stream request, and one Infinite stream if the account is entitled. Check
   Infinite non-stream aggregation and usage without enabling an automatic
   Infinite fallback. Do not intentionally exhaust a real account.
5. Verify inference-header observations update the quota card without a poll.
6. Confirm native routing across distinct auths and sibling isolation for payment
   errors. Re-run mock concurrency/exhaustion tests on the proposed host version.
   **The current v8.0.48 RPC bridge cannot preserve the native transient marker:**
   its concurrency-only 429 still becomes model quota-exceeded state. Do not
   accept native parity or proceed to cutover until the upstream contract and
   pinned compatibility test are updated. No local core patch is supplied.

## 5. Cut over Synthetic only

Once all staging acceptance and host-contract prerequisites pass, during an
authorized deployment window:

1. Retain the private rollback snapshot and verified old binaries/config.
2. Remove **both** legacy compatibility entries (`synthetic` and
   `synthetic-infinite`) from the target instance; leaving either behind can
   retain native executor ownership and invalidate the test result.
3. Remove only the Synthetic overlay block from subscription-pool. Do not change
   unrelated provider routing or quota settings.
4. Install/enable the verified production Synthetic plugin and its deduplicated
   auth files through native host mechanisms. Reload/restart using the host's
   supported lifecycle. Never enable a hostcompat-tagged artifact.
5. Update downstream model IDs, explicitly communicating the Infinite rename:

   | Intended model | New client ID |
   | --- | --- |
   | Small text alias | `synthetic/syn:small:text` |
   | Large text alias | `synthetic/syn:large:text` |
   | Infinite model previously reached through a separate provider | `synthetic/infinite:small:vision` |
   | Discovered pinned model | `synthetic/hf:<organization>/<model>` |

6. Verify actual executor ownership, native auth/quota UI, low-volume authorized
   inference and error behavior. Keep the rollback snapshot until the agreed
   observation period is complete. Do not remove old entries from the backup.

## 6. Roll back

1. Stop routing new traffic to the plugin and disable `configs.synthetic` through
   native configuration. Restore the previous caller IDs/aliases.
2. Restore both compatibility entries and the previous Synthetic overlay block
   from the private snapshot, keeping unrelated provider config unchanged.
3. Restore the prior auth/plugin files as needed. Linux Go C-shared images remain
   resident after logical disable; a supported **host restart is required to
   fully evict a bad loaded library**. Disabling alone is not an unload guarantee.
4. Verify prior inference and quota behavior before resuming normal traffic.
   Retain only private, redacted diagnostics; never publish the credential backup.
