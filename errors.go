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
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	return classifyWithQuota(resp, now, nil)
}
func classifyWithQuota(resp pluginapi.HTTPResponse, now time.Time, cached *quotaReading) *pluginabi.Error {
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
		reading, hasHeader := quotaFromHeaders(resp.Headers)
		body := strings.ToLower(string(resp.Body))
		// Only a response-local positive-credit header vouches for concurrency.
		// Cached positive credit alone must not downgrade a later exhaustion.
		if hasHeader && reading.PercentRemaining != nil && *reading.PercentRemaining > 0 && (strings.Contains(body, "concurrent") || strings.Contains(body, "concurrency")) {
			e.Code, e.Message, e.RetryAfterSeconds = "concurrency_limit", "Synthetic concurrency limit reached", 5
		} else {
			e.Code, e.Message, e.RetryAfterSeconds = "quota_exhausted", "Synthetic quota or rate limit reached", 60
			if !hasHeader && cached != nil {
				reading = *cached
			}
			if reading.NextRegenAt.After(now) {
				seconds := math.Ceil(reading.NextRegenAt.Sub(now).Seconds())
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
