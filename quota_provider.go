package main

import (
	"context"
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
	readings map[string]timedReading
	now      func() time.Time
}

// planMaxAge bounds how long a header update may repeat a remembered plan.
// Headers carry no auth fields, so a plan edited mid-traffic would otherwise
// read stale until a traffic gap lets a poll through; after this age the
// chip drops instead of risking the wrong plan. Polls always reattach the
// current plan.
const planMaxAge = 10 * time.Minute

type timedReading struct {
	readings   []quotaReading
	observedAt time.Time
	expires    time.Time
	// plan is the display-only auth plan remembered alongside the readings so
	// a header update (which carries no auth fields) keeps the plan chip
	// instead of wiping it until the next poll.
	plan string
	// planAt is when that plan was last reported by a poll; header-only
	// remembers never move it.
	planAt time.Time
}

func (q *quotaProvider) clock() time.Time {
	if q != nil && q.now != nil {
		return q.now()
	}
	return time.Now()
}

// These observations are response-classification hints only. They never select
// an account, avoid an account preemptively, or alter the host's scheduler.
func (q *quotaProvider) remember(id string, readings []quotaReading, observedAt time.Time, plan string) {
	if id == "" || len(readings) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock()
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
	next := timedReading{readings: kept, observedAt: observedAt, expires: observedAt.Add(providerkit.QuotaCacheTTL)}
	if plan != "" {
		next.plan, next.planAt = plan, observedAt
	} else if current, ok := q.readings[id]; ok {
		// A poll without a plan field (or a header, which never carries
		// one) must not wipe a plan a previous poll remembered.
		next.plan, next.planAt = current.plan, current.planAt
	}
	q.readings[id] = next
}

// planFor returns the remembered display-only plan for an auth, or "" when
// none was reported, the observation expired, or the plan itself is older
// than planMaxAge (an edited plan then drops the chip instead of showing the
// wrong one until a poll refreshes it).
func (q *quotaProvider) planFor(id string) string {
	if q == nil || id == "" {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.readings[id]
	if !ok || !q.clock().Before(r.expires) {
		delete(q.readings, id)
		return ""
	}
	if r.plan == "" || r.planAt.IsZero() || q.clock().Sub(r.planAt) >= planMaxAge {
		return ""
	}
	return r.plan
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
		// The plan field is display-only: the panel shows it as the plan
		// chip, and it never affects admission or identity.
		plan := ""
		if fields, ferr := providerkit.AuthFields(req.StorageJSON, req.Metadata); ferr == nil {
			if p, ok := fields["plan"].(string); ok {
				plan = strings.TrimSpace(p)
			}
		}
		q.remember(quotaIdentity(req.AuthID, req.AuthIndex), readings, observedAt, plan)
		out := normalizeQuotas(readings)
		if plan != "" {
			out.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
		}
		return out, nil
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
		window = "Subscription"
		fullReset = true
	}
	bucket := pluginapi.QuotaBucket{Window: window, RemainingFraction: percent / 100}
	out := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Synthetic", Buckets: []pluginapi.QuotaBucket{bucket}}}}
	// NextRegenAt marks an incremental refill, not a full reset, and the card
	// carries no refill chip naming the increment — so a weekly tick must not
	// pose as a reset. Only a subscription renewal resets the allowance and
	// keeps its reset time.
	if !reading.NextRegenAt.IsZero() && fullReset {
		out.Groups[0].Buckets[0].ResetTime = reading.NextRegenAt.Format(time.RFC3339Nano)
	}
	return out
}

// normalizeQuotas collects every shape into a single Synthetic group with one
// bucket per shape, so a payload reporting weekly credits alongside legacy
// request packs no longer hides the latter — and the panel renders one group
// header instead of a repeated header per shape. A single-shape input renders
// exactly as normalizeQuota renders it. The card carries no summary chips:
// regeneration percents are incremental hints, not plan data.
// planWeeklyLimit and weeklyTokenLimit are the same bucket in current and
// legacy form: when both are present only the current plan shape is rendered.
func normalizeQuotas(readings []quotaReading) pluginapi.QuotaFetchResponse {
	var buckets []pluginapi.QuotaBucket
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
	}
	out := pluginapi.QuotaFetchResponse{}
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
		q.remember(id, readings, time.Now(), "")
		out := normalizeQuotas(readings)
		// Headers carry no auth fields, so reattach the remembered plan:
		// otherwise the plan chip vanishes until the next poll.
		if plan := q.planFor(id); plan != "" {
			out.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
		}
		_ = q.cache.Store(pluginapi.QuotaFetchRequest{Provider: record.Provider, AuthID: record.AuthID, AuthIndex: record.AuthIndex}, out)
	}
}
