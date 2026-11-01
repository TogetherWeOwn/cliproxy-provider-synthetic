package main

import (
	"context"
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
	readings   []quotaReading
	observedAt time.Time
	expires    time.Time
}

// These observations are response-classification hints only. They never select
// an account, avoid an account preemptively, or alter the host's scheduler.
func (q *quotaProvider) remember(id string, readings []quotaReading, observedAt time.Time) {
	if id == "" || len(readings) == 0 {
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
	kept := make([]quotaReading, len(readings))
	copy(kept, readings)
	q.readings[id] = timedReading{readings: kept, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
}
func (q *quotaProvider) latest(id string) []quotaReading {
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
	kept := make([]quotaReading, len(r.readings))
	copy(kept, r.readings)
	return kept
}

// latestOne returns the first remembered shape for the 429 RetryAfter hint,
// preserving the historical first-match behavior.
func (q *quotaProvider) latestOne(id string) *quotaReading {
	readings := q.latest(id)
	if len(readings) == 0 {
		return nil
	}
	reading := readings[0]
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
		readings, err := fetchQuotas(ctx, client, url, key)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		q.remember(quotaIdentity(req.AuthID, req.AuthIndex), readings, observedAt)
		return normalizeQuotas(readings), nil
	})
	return q
}

// shapeWindow maps a quota shape to its card row label. Weekly shapes share
// the canonical weekly label and the legacy request pack gets the canonical
// 5-hour label; subscription is a distinct legacy shape with no canonical
// spelling, so it keeps its own name.
func shapeWindow(shape string) string {
	switch shape {
	case "rollingFiveHourLimit":
		return providerkit.QuotaWindowLabel("5h")
	case "subscription":
		return "subscription"
	default:
		return providerkit.QuotaWindowLabel("weekly")
	}
}

// quotaBucket renders one shape as a bar row: a short window label, the
// remaining fraction, and the reset time. It never sets Description, so the
// panel shows the Window label instead of a sentence. ok is false when the
// reading carries no remaining percentage, so nothing is invented.
func quotaBucket(reading quotaReading) (bucket pluginapi.QuotaBucket, ok bool) {
	if reading.PercentRemaining == nil {
		return pluginapi.QuotaBucket{}, false
	}
	reset := ""
	if !reading.NextRegenAt.IsZero() {
		reset = reading.NextRegenAt.Format(time.RFC3339)
	}
	return providerkit.NewQuotaBucket(shapeWindow(reading.Shape), clamp(*reading.PercentRemaining, 0, 100)/100, reset), true
}

// normalizeQuota renders a single shape exactly as normalizeQuotas renders a
// one-shape payload.
func normalizeQuota(reading quotaReading) pluginapi.QuotaFetchResponse {
	return normalizeQuotas([]quotaReading{reading})
}

// normalizeQuotas emits one Synthetic group with one bucket per shape
// present, so a payload reporting weekly credits alongside legacy request
// packs no longer hides the latter. The next-increment chip comes from the
// first shape reporting a non-zero one; a zero or absent increment shows no
// chip instead of a "0%" row.
// planWeeklyLimit and weeklyTokenLimit are the same bucket in current and
// legacy form: when both are present only the current plan shape is rendered.
func normalizeQuotas(readings []quotaReading) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{}
	hasPlanWeekly := false
	for _, reading := range readings {
		if reading.Shape == "planWeeklyLimit" && reading.PercentRemaining != nil {
			hasPlanWeekly = true
			break
		}
	}
	group := pluginapi.QuotaGroup{DisplayName: "Synthetic"}
	for _, reading := range readings {
		if hasPlanWeekly && reading.Shape == "weeklyTokenLimit" {
			continue
		}
		bucket, ok := quotaBucket(reading)
		if !ok {
			continue
		}
		group.Buckets = append(group.Buckets, bucket)
		if len(out.Summary) == 0 && reading.NextRegenPercent != nil && *reading.NextRegenPercent != 0 {
			out.Summary = []pluginapi.QuotaMetric{{Key: "next_increment_percent", Label: "Next increment", Value: *reading.NextRegenPercent, Unit: "%", Format: "number"}}
		}
	}
	if len(group.Buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{group}
	}
	return out
}

// HandleUsage consumes only provider-matched response headers and never persists keys.
func (q *quotaProvider) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if record.Provider != "synthetic" {
		return
	}
	if readings, ok := quotaReadingsFromHeaders(record.ResponseHeaders); ok {
		q.remember(quotaIdentity(record.AuthID, record.AuthIndex), readings, time.Now())
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, normalizeQuotas(readings))
	}
}
