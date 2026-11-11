package main

import (
	"math"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Based on Synthetic failure handling in TogetherWeOwn/cpa-plugin-subscription-pool
// at ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b, internal/providers/synthetic/module.go.
// No raw body or credential is copied into an RPC error.

// maxQuotaMarkerScan bounds how much of a 429 body the quota-marker arm reads.
const maxQuotaMarkerScan = 4096

// quotaBodyMarker reports whether a lowercased 429 body names the per-credential
// quota, using Synthetic's own vocabulary ("quota", "credit"). It is one of the
// three credential-scope evidence arms and never feeds the public message.
func quotaBodyMarker(body string) bool {
	return strings.Contains(body, "quota") || strings.Contains(body, "credit")
}

func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	return classifyWithQuota(resp, now, nil, false)
}
func classifyWithQuota(resp pluginapi.HTTPResponse, now time.Time, cached []quotaReading, cachedBound bool) *pluginabi.Error {
	e := &pluginabi.Error{Code: "upstream_error", Message: "Synthetic request failed", HTTPStatus: resp.StatusCode, RetryAfterSeconds: kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now)}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Code, e.Message, e.RetryAfterSeconds = "invalid_credentials", "Synthetic rejected the credential", 0
	case http.StatusPaymentRequired:
		e.Code, e.Message, e.RetryAfterSeconds = "payment_required", "Synthetic reported a payment issue", 3600
	case http.StatusForbidden:
		e.Code, e.Message, e.RetryAfterSeconds = "entitlement_required", "Synthetic requires a different entitlement", 3600
	case http.StatusNotFound:
		e.Code, e.Message, e.RetryAfterSeconds = "upstream_not_found", "Synthetic endpoint or model was not found", 0
	case http.StatusTooManyRequests:
		e.Retryable = true
		readings, hasHeader := quotaReadingsFromHeaders(resp.Headers)
		if !hasHeader {
			readings = cached
		}
		var reading quotaReading
		if len(readings) > 0 {
			reading = readings[0]
		}
		// Scope evidence is the response's own header, or a cached reading
		// bound to this credential generation. A header observation carries
		// no credential material, so a cached-but-unbound one stays a retry
		// hint only: it cannot cool whichever key happens to be current.
		evidence := readings
		if !hasHeader && !cachedBound {
			evidence = nil
		}
		body := strings.ToLower(string(resp.Body))
		// Only a response-local positive-credit header vouches for concurrency.
		// Cached positive credit alone must not downgrade a later exhaustion.
		if hasHeader && reading.PercentRemaining != nil && *reading.PercentRemaining > 0 && (strings.Contains(body, "concurrent") || strings.Contains(body, "concurrency")) {
			e.Code, e.Message, e.RetryAfterSeconds = "concurrency_limit", "Synthetic concurrency limit reached", 5
		} else {
			e.Code, e.Message, e.RetryAfterSeconds = "quota_exhausted", "Synthetic quota or rate limit reached", 60
			// Credential scope is evidence-gated: any response-header reading
			// with 0% remaining, a known future NextRegenAt on an effective
			// reading that does NOT affirmatively show credit remaining —
			// percentRemaining absent or <= 0 — or a quota marker in the
			// bounded lowercased body. Exhaustion evidence is checked across
			// every parsed bucket, not only the first, because a header can
			// report positive weekly credit beside an empty rolling-five-hour
			// bucket that still means the credential cannot serve. NextRegenAt
			// is a routine incremental-refill tick present at every balance
			// level, so a reading that reports credit left is never exhaustion
			// evidence on regen alone; a server Retry-After can also override
			// the timing, so the arm keys on the reading, not on which source
			// produced RetryAfterSeconds. A header-less generic 429 falling
			// back to the flat 60s with none of those stays model-scoped —
			// cooling the whole credential on an unclassified 429 repeats the
			// opposite bug. A 0% cached-only reading without a regen never
			// scopes. When evidence comes from a non-first bucket, the
			// cooldown is timed by the exhausted buckets' regens, not the
			// first bucket's.
			exhaustedAt := time.Time{}
			for _, r := range evidence {
				if hasHeader && r.PercentRemaining != nil && *r.PercentRemaining <= 0 {
					e.CredentialScoped = true
				}
				exhausted := r.PercentRemaining == nil || *r.PercentRemaining <= 0
				if exhausted && r.NextRegenAt.After(now) {
					e.CredentialScoped = true
					// The latest future regen among buckets without
					// affirmative credit bounds the cooldown: the credential
					// cannot serve until every empty bucket refills, never on
					// a positive bucket's routine refill tick.
					if r.NextRegenAt.After(exhaustedAt) {
						exhaustedAt = r.NextRegenAt
					}
				}
			}
			if quotaBodyMarker(body[:min(len(body), maxQuotaMarkerScan)]) {
				e.CredentialScoped = true
			}
			// Timing falls back to the first parsed reading only for an
			// unscoped 429, which keeps the first reading's
			// incremental-refill hint; a scoped error with no exhausted
			// bucket regen keeps the flat default rather than a positive
			// bucket's tick.
			regenAt := exhaustedAt
			if regenAt.IsZero() && !e.CredentialScoped {
				regenAt = reading.NextRegenAt
			}
			if regenAt.After(now) {
				seconds := math.Ceil(regenAt.Sub(now).Seconds())
				if seconds > math.MaxInt32 {
					seconds = math.MaxInt32
				}
				e.RetryAfterSeconds = int(seconds)
			}
		}
		if n := kit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now); n > 0 {
			e.RetryAfterSeconds = n
		}
	default:
		e.Retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
	}
	return e
}
