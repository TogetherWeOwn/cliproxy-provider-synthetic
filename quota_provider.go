package main

import (
	"context"
	"crypto/sha256"
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
	readings map[readingKey]timedReading
}

// readingKey binds a classification hint to the credential generation that
// produced it (G-K3). A rotated key under a reused auth identity digests
// differently, so the previous generation's regeneration evidence can never
// classify the new generation's 429s. Header observations arrive without
// credential context and stay unbound (hasCred false), served to every
// generation until the TTL expires — the same fast path the kit cache keeps.
type readingKey struct {
	id      string
	cred    [32]byte
	hasCred bool
}
type timedReading struct {
	reading    quotaReading
	observedAt time.Time
	expires    time.Time
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
func (q *quotaProvider) remember(id string, cred [32]byte, hasCred bool, reading quotaReading, observedAt time.Time) {
	if id == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if q.readings == nil {
		q.readings = make(map[readingKey]timedReading)
	}
	for key, r := range q.readings {
		if !now.Before(r.expires) {
			delete(q.readings, key)
		}
	}
	key := readingKey{id: id, cred: cred, hasCred: hasCred}
	if current, ok := q.readings[key]; ok && current.observedAt.After(observedAt) {
		return
	}
	q.readings[key] = timedReading{reading: reading, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
}
func (q *quotaProvider) latest(id string, cred [32]byte) *quotaReading {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	var best *timedReading
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
	}
	if best == nil {
		return nil
	}
	reading := best.reading
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
		q.remember(quotaIdentity(req.AuthID, req.AuthIndex), credentialDigest(req.StorageJSON, req.Metadata), true, reading, observedAt)
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
	switch {
	case window == "weekly":
		out.Summary = []pluginapi.QuotaMetric{{Key: "weekly_credits_remaining_percent", Label: "Remaining", Value: percent, Unit: "%", Format: "number"}}
	case window == "subscription" && reading.Limit != nil && reading.Requests != nil:
		// The legacy subscription shape reports absolute counters, not a
		// percentage: surface both so the connector can derive usedPercent
		// instead of dividing by an assumed limit (G-S3).
		out.Summary = []pluginapi.QuotaMetric{
			{Key: "subscription_requests_used", Label: "Used", Value: *reading.Requests, Unit: "requests", Format: "number"},
			{Key: "subscription_request_limit", Label: "Limit", Value: *reading.Limit, Unit: "requests", Format: "number"},
		}
	}
	return out
}

// HandleUsage consumes only provider-matched response headers and never persists keys.
func (q *quotaProvider) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if record.Provider != "synthetic" {
		return
	}
	if reading, ok := quotaFromHeaders(record.ResponseHeaders); ok {
		// UsageRecords carry no credential context, so header observations stay
		// unbound: served to every generation of the identity until the TTL.
		q.remember(quotaIdentity(record.AuthID, record.AuthIndex), [32]byte{}, false, reading, time.Now())
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, normalizeQuota(reading))
	}
}
