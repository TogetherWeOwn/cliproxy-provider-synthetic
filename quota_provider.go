package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaProvider shares the same cache with the UsagePlugin, keyed by auth ID.
type quotaProvider struct {
	cache    *providerkit.CachedQuota
	mu       sync.Mutex
	readings map[string]timedReading
}
type timedReading struct {
	reading    quotaReading
	observedAt time.Time
	expires    time.Time
}

// These observations are response-classification hints only. They never select
// an account, avoid an account preemptively, or alter the host's scheduler.
func (q *quotaProvider) remember(id string, reading quotaReading, observedAt time.Time) {
	if id == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if q.readings == nil {
		q.readings = make(map[string]timedReading)
	}
	for key, r := range q.readings {
		if !now.Before(r.expires) {
			delete(q.readings, key)
		}
	}
	if current, ok := q.readings[id]; ok && current.observedAt.After(observedAt) {
		return
	}
	q.readings[id] = timedReading{reading: reading, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
}
func (q *quotaProvider) latest(id string) *quotaReading {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.readings[id]
	if !ok || !time.Now().Before(r.expires) {
		delete(q.readings, id)
		return nil
	}
	reading := r.reading
	return &reading
}
func quotaIdentity(id, index string) string {
	if id != "" {
		return id
	}
	return index
}

func newQuotaProvider(endpoint string, client httpDoer) *quotaProvider {
	q := &quotaProvider{}
	q.cache = providerkit.NewCachedQuota("synthetic", "Synthetic", func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		key, err := providerkit.APIKey(req.StorageJSON, req.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		url := endpoint
		if url == "" {
			url = selectedBase(upstreamBase) + "/v2/quotas"
		}
		// A header received while this poll is in flight is newer evidence.
		observedAt := time.Now()
		reading, err := fetchQuota(ctx, client, url, key)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		q.remember(quotaIdentity(req.AuthID, req.AuthIndex), reading, observedAt)
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
		q.remember(quotaIdentity(record.AuthID, record.AuthIndex), reading, time.Now())
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, normalizeQuota(reading))
	}
}
