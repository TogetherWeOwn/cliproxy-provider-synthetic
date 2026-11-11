package main

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaProvider shares the same cache with the UsagePlugin, keyed by auth ID.
type quotaProvider struct {
	cache    *providerkit.CachedQuota
	mu       sync.Mutex
	readings map[readingKey]timedReading
	now      func() time.Time
}

// Polled hints belong to one credential generation. Usage records carry no
// credential context, so header hints remain unbound until their TTL expires.
type readingKey struct {
	id      string
	cred    [32]byte
	hasCred bool
}

type timedReading struct {
	readings   []quotaReading
	observedAt time.Time
	expires    time.Time
}

func (q *quotaProvider) clock() time.Time {
	if q != nil && q.now != nil {
		return q.now()
	}
	return time.Now()
}

// credentialDigest identifies the credential generation behind a request. Only
// the API key feeds the digest — proxy or metadata drift must not orphan quota
// evidence, while a key rotation must. When the key is unparseable the raw
// storage feeds it instead: still stable per generation, never logged.
func credentialDigest(storage []byte, meta map[string]any) [32]byte {
	if key, err := providerkit.APIKey(storage, meta); err == nil {
		return sha256.Sum256([]byte(key))
	}
	return sha256.Sum256(storage)
}

// These observations are response-classification hints only. They never select
// an account, avoid an account preemptively, or alter the host's scheduler.
func (q *quotaProvider) remember(key readingKey, readings []quotaReading, observedAt time.Time) {
	if key.id == "" || len(readings) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock()
	if q.readings == nil {
		q.readings = make(map[readingKey]timedReading)
	}
	for key, r := range q.readings {
		if !now.Before(r.expires) {
			delete(q.readings, key)
		}
	}
	if current, ok := q.readings[key]; ok && current.observedAt.After(observedAt) {
		return
	}
	kept := make([]quotaReading, len(readings))
	copy(kept, readings)
	q.readings[key] = timedReading{readings: kept, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
}

func (q *quotaProvider) Identifier() string { return q.cache.Identifier() }

func (q *quotaProvider) DescribeQuota(ctx context.Context, req pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return q.cache.DescribeQuota(ctx, req)
}

func (q *quotaProvider) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return q.cache.ResetQuota(ctx, req)
}

func (q *quotaProvider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	out, err := q.cache.FetchQuota(ctx, req)
	if err != nil {
		return out, err
	}
	// Plans are display-only auth fields, never header observations. Attach
	// the current request's plan after any cache hit so edits and key rotations
	// cannot repeat an old credential's label under the same auth identity.
	out.Subscription = nil
	if fields, ferr := providerkit.AuthFields(req.StorageJSON, req.Metadata); ferr == nil {
		if p, ok := fields["plan"].(string); ok && strings.TrimSpace(p) != "" {
			out.Subscription = &pluginapi.QuotaSubscription{Plan: strings.TrimSpace(p)}
		}
	}
	return out, nil
}

// latest returns the first shape from the newest matching generation or
// unbound header, preserving the historical first-match RetryAfter hint.
func (q *quotaProvider) latest(id string, cred [32]byte) *quotaReading {
	hint, _ := q.latestReadings(id, cred)
	if len(hint) == 0 {
		return nil
	}
	return &hint[0]
}

// latestReadings returns every shape from two observations: hint is the
// newest matching observation of any kind (bound poll or unbound header)
// and supplies retry timing; bound is the newest observation recorded for
// this credential generation or nil. A UsageRecord carries no credential
// material, so an unbound header may hint but never scope — and a newer
// unbound header cannot hide a still-fresh bound poll's evidence.
func (q *quotaProvider) latestReadings(id string, cred [32]byte) (hint []quotaReading, bound []quotaReading) {
	if q == nil {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock()
	var best *timedReading
	var bestBound *timedReading
	for key, r := range q.readings {
		if key.id != id {
			continue
		}
		if !now.Before(r.expires) {
			delete(q.readings, key)
			continue
		}
		if key.hasCred && key.cred != cred {
			continue
		}
		if best == nil || r.observedAt.After(best.observedAt) {
			entry := r
			best = &entry
		}
		if key.hasCred && (bestBound == nil || r.observedAt.After(bestBound.observedAt)) {
			entry := r
			bestBound = &entry
		}
	}
	if best != nil && len(best.readings) > 0 {
		hint = append([]quotaReading(nil), best.readings...)
	}
	if bestBound != nil && len(bestBound.readings) > 0 {
		bound = append([]quotaReading(nil), bestBound.readings...)
	}
	return hint, bound
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
		observedAt := q.clock()
		readings, err := fetchQuotas(ctx, client, url, key)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		q.remember(readingKey{id: quotaIdentity(req.AuthID, req.AuthIndex), cred: credentialDigest(req.StorageJSON, req.Metadata), hasCred: true}, readings, observedAt)
		return normalizeQuotas(readings), nil
	})
	return q
}

func normalizeQuota(reading quotaReading) pluginapi.QuotaFetchResponse {
	if reading.PercentRemaining == nil {
		return pluginapi.QuotaFetchResponse{}
	}
	percent := clamp(*reading.PercentRemaining, 0, 100)
	window := providerkit.CardWindowWeekly
	fullReset := false
	switch reading.Shape {
	case "rollingFiveHourLimit":
		window = providerkit.CardWindowFiveHour
	case "subscription":
		// "Subscription" truncates beside the reset timestamp. Keep the
		// short label for an actual renewal, not an incremental refill.
		window = "Renewal"
		fullReset = true
	}
	bucket := pluginapi.QuotaBucket{Window: window, RemainingFraction: percent / 100}
	out := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Synthetic", Buckets: []pluginapi.QuotaBucket{bucket}}}}
	// NextRegenAt marks an incremental refill, not a full reset. Only a
	// subscription renewal resets the allowance and keeps its reset time.
	if !reading.NextRegenAt.IsZero() && fullReset {
		out.Groups[0].Buckets[0].ResetTime = reading.NextRegenAt.Format(time.RFC3339Nano)
	}
	if reading.Shape == "subscription" && reading.Limit != nil && reading.Requests != nil {
		out.Summary = []pluginapi.QuotaMetric{
			{Key: "subscription_requests_used", Label: "Used", Value: *reading.Requests, Unit: "requests", Format: "number"},
			{Key: "subscription_request_limit", Label: "Limit", Value: *reading.Limit, Unit: "requests", Format: "number"},
		}
	}
	return out
}

// normalizeQuotas collects every shape into a single Synthetic group with one
// bucket per shape. Absolute subscription counters remain available to callers;
// incremental regeneration hints never become summary chips.
// planWeeklyLimit and weeklyTokenLimit are the same bucket in current and
// legacy form: when both are present only the current plan shape is rendered.
func normalizeQuotas(readings []quotaReading) pluginapi.QuotaFetchResponse {
	var buckets []pluginapi.QuotaBucket
	var summary []pluginapi.QuotaMetric
	hasPlanWeekly := false
	for _, reading := range readings {
		if reading.Shape == "planWeeklyLimit" && reading.PercentRemaining != nil {
			hasPlanWeekly = true
			break
		}
	}
	for _, reading := range readings {
		if hasPlanWeekly && reading.Shape == "weeklyTokenLimit" {
			continue
		}
		one := normalizeQuota(reading)
		for _, g := range one.Groups {
			buckets = append(buckets, g.Buckets...)
		}
		summary = append(summary, one.Summary...)
	}
	out := pluginapi.QuotaFetchResponse{Summary: summary}
	if len(buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{{DisplayName: "Synthetic", Buckets: buckets}}
	}
	return out
}

// HandleUsage consumes only provider-matched response headers and never persists keys.
func (q *quotaProvider) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if record.Provider != "synthetic" {
		return
	}
	if readings, ok := quotaReadingsFromHeaders(record.ResponseHeaders); ok {
		id := quotaIdentity(record.AuthID, record.AuthIndex)
		q.remember(readingKey{id: id}, readings, q.clock())
		out := normalizeQuotas(readings)
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, out)
	}
}
