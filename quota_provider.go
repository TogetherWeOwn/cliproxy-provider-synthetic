package synthetic

import (
	"context"
	"fmt"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaProvider shares the same cache with the UsagePlugin, keyed by auth ID.
type quotaProvider struct{ cache *providerkit.CachedQuota }

func newQuotaProvider(endpoint string, client httpDoer) *quotaProvider {
	q := &quotaProvider{}
	q.cache = providerkit.NewCachedQuota("synthetic", "Synthetic", func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		key, err := providerkit.APIKey(req.StorageJSON, req.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		reading, err := fetchQuota(ctx, client, endpoint, key)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		return normalizeQuota(reading), nil
	})
	return q
}

func normalizeQuota(reading quotaReading) pluginapi.QuotaFetchResponse {
	if reading.PercentRemaining == nil {
		return pluginapi.QuotaFetchResponse{}
	}
	percent := clamp(*reading.PercentRemaining, 0, 100)
	window, name, description := "weekly", "Weekly credits", "Next regeneration, not a full reset"
	switch reading.Shape {
	case "rollingFiveHourLimit":
		window, name, description = "5h", "Legacy request limit", "Next regeneration, not a full reset"
	case "subscription":
		window, name, description = "subscription", "Legacy subscription", "Subscription renewal"
	}
	bucket := pluginapi.QuotaBucket{Window: window, RemainingFraction: percent / 100, Description: description}
	if !reading.NextRegenAt.IsZero() {
		bucket.ResetTime = reading.NextRegenAt.Format(time.RFC3339Nano)
	}
	if reading.NextRegenPercent != nil {
		bucket.Description += fmt.Sprintf(" (next increment %.2g%%)", *reading.NextRegenPercent)
	}
	out := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: name, Buckets: []pluginapi.QuotaBucket{bucket}}}}
	if window == "weekly" {
		out.Summary = []pluginapi.QuotaMetric{{Key: "weekly_credits_remaining_percent", Label: "Remaining", Value: percent, Unit: "%", Format: "number"}}
	}
	return out
}

// HandleUsage consumes only provider-matched response headers and never persists keys.
func (q *quotaProvider) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if record.Provider != "synthetic" {
		return
	}
	if reading, ok := quotaFromHeaders(record.ResponseHeaders); ok {
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, normalizeQuota(reading))
	}
}
