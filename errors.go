package synthetic

import (
	"math"
	"net/http"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Based on Synthetic failure handling in TogetherWeOwn/cpa-plugin-subscription-pool
// at ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b, internal/providers/synthetic/module.go.
// It classifies response-local evidence only; cached observations need integration.
func classifyFailure(resp pluginapi.HTTPResponse, now time.Time) *pluginabi.Error {
	e := &pluginabi.Error{Code: "upstream_error", Message: "Synthetic request failed", HTTPStatus: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Code, e.Message = "invalid_credentials", "Synthetic rejected the credential"
	case http.StatusPaymentRequired:
		e.Code, e.Message, e.RetryAfterSeconds = "payment_required", "Synthetic reported a payment issue", 3600
	case http.StatusNotFound:
		e.Code, e.Message = "upstream_not_found", "Synthetic endpoint or model was not found"
	case http.StatusTooManyRequests:
		e.Retryable = true
		reading, ok := quotaFromHeaders(resp.Headers)
		if ok && reading.PercentRemaining != nil && *reading.PercentRemaining > 0 && strings.Contains(strings.ToLower(string(resp.Body)), "concurrent") {
			e.Code, e.Message, e.RetryAfterSeconds = "concurrency_limit", "Synthetic concurrency limit reached", 5
		} else {
			e.Code, e.Message, e.RetryAfterSeconds = "quota_exhausted", "Synthetic quota or rate limit reached", 60
			if ok && reading.NextRegenAt.After(now) {
				e.RetryAfterSeconds = int(math.Ceil(reading.NextRegenAt.Sub(now).Seconds()))
			}
		}
		if n := providerkit.RetryAfterSeconds(resp.Headers.Get("Retry-After"), now); n > 0 {
			e.RetryAfterSeconds = n
		}
	default:
		e.Retryable = resp.StatusCode >= 500
	}
	return e
}
