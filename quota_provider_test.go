package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaProviderPollAndHeader(t *testing.T) {
	count := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.Method != "GET" || r.URL.Path != "/v2/quotas" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Errorf("unexpected quota request")
		}
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":25,"nextRegenAt":"2030-01-01T00:00:00Z","nextRegenPercent":2}}`))
	}))
	defer s.Close()
	q := newQuotaProvider(s.URL+"/v2/quotas", s.Client())
	req := pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "a", StorageJSON: []byte(`{"api_key":"example-key"}`)}
	result, err := q.FetchQuota(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// The weekly nextRegenAt is an incremental refill, not a reset, and the
	// card carries no refill chip — so the row keeps no reset time.
	if count != 1 || result.Groups[0].Buckets[0].Window != "weekly" || result.Groups[0].Buckets[0].RemainingFraction != 0.25 || result.Groups[0].Buckets[0].ResetTime != "" {
		t.Fatalf("unexpected quota result: %+v", result)
	}
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "a", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
	result, err = q.FetchQuota(context.Background(), req)
	if err != nil || result.Groups[0].Buckets[0].RemainingFraction != 0.8 || count != 1 {
		t.Fatalf("header did not replace cached poll: %+v %v", result, err)
	}
}
func TestQuotaPollSurfacesPlanChip(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":50}}`))
	}))
	defer s.Close()
	q := newQuotaProvider(s.URL, s.Client())
	for _, tc := range []struct {
		name    string
		storage string
		want    string
	}{
		{"plan field becomes the plan chip", `{"api_key":"example-key","plan":"go"}`, "go"},
		{"missing plan leaves no subscription", `{"api_key":"example-key"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: tc.name, StorageJSON: []byte(tc.storage)})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if result.Subscription != nil {
				got = result.Subscription.Plan
			}
			if got != tc.want {
				t.Fatalf("plan chip = %q, want %q: %+v", got, tc.want, result.Subscription)
			}
			if violations := providerkit.CheckCard(result); len(violations) != 0 {
				t.Fatalf("CheckCard violations: %v", violations)
			}
		})
	}
}

// A header update carries no auth fields, so it must keep the plan chip the
// poll attached instead of wiping it until the next poll.
func TestQuotaHeaderPreservesPlanChip(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":25}}`))
	}))
	defer s.Close()
	q := newQuotaProvider(s.URL, s.Client())
	req := pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "plan-auth", StorageJSON: []byte(`{"api_key":"example-key","plan":"big"}`)}
	if _, err := q.FetchQuota(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "plan-auth", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
	result, err := q.FetchQuota(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Groups[0].Buckets[0].RemainingFraction != 0.8 {
		t.Fatalf("header did not update the bar: %+v", result)
	}
	if result.Subscription == nil || result.Subscription.Plan != "big" {
		t.Fatalf("header wiped the plan chip: %+v", result.Subscription)
	}
	if violations := providerkit.CheckCard(result); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
}

// Header observations are unbound, but their plan chip must always come from
// the current fetch's auth fields, even before the old poll's TTL expires.
func TestQuotaHeaderUsesCurrentPlanAcrossEditsAndRotations(t *testing.T) {
	for _, tc := range []struct {
		name, storage, want string
	}{
		{"same key edited plan", `{"api_key":"example-key","plan":"go"}`, "go"},
		{"same key removed plan", `{"api_key":"example-key"}`, ""},
		{"rotated key new plan", `{"api_key":"rotated-key","plan":"standard"}`, "standard"},
		{"rotated key no plan", `{"api_key":"rotated-key"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":25}}`))
			}))
			defer s.Close()
			q := newQuotaProvider(s.URL, s.Client())
			now := time.Now()
			q.now = func() time.Time { return now }
			req := pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "edit-auth", StorageJSON: []byte(`{"api_key":"example-key","plan":"big"}`)}
			if _, err := q.FetchQuota(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			req.StorageJSON = []byte(tc.storage)
			// Refresh observations below their TTL for longer than the old plan
			// age limit. No expiry or traffic gap can hide a stale-plan bug.
			for elapsed := time.Duration(0); elapsed <= 11*time.Minute; elapsed += 30 * time.Second {
				q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: req.AuthID, ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
				result, err := q.FetchQuota(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if result.Groups[0].Buckets[0].RemainingFraction != 0.8 || count != 1 {
					t.Fatalf("header should serve without polling: %+v, polls=%d", result, count)
				}
				got := ""
				if result.Subscription != nil {
					got = result.Subscription.Plan
				}
				if got != tc.want {
					t.Fatalf("plan after %v = %q, want %q", elapsed, got, tc.want)
				}
				now = now.Add(30 * time.Second)
			}
		})
	}
}
func TestQuotaUsageRejectsForeignProvider(t *testing.T) {
	q := newQuotaProvider("", nil)
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "foreign", AuthID: "a", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
	_, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "a"})
	if err == nil {
		t.Fatal("foreign header was accepted")
	}
}

func TestLegacyQuotaDoesNotClaimWeeklyCredits(t *testing.T) {
	r, err := parseQuota([]byte(`{"rollingFiveHourLimit":{"remaining":1,"max":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeQuota(r)
	if len(got.Groups) != 1 || got.Groups[0].Buckets[0].Window == "weekly" {
		t.Fatalf("legacy five-hour quota mislabeled as weekly credits: %+v", got)
	}
	if len(got.Summary) != 0 {
		t.Fatalf("legacy quota must not emit a weekly summary: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
}

func TestSubscriptionSummaryReportsAbsoluteCounters(t *testing.T) {
	r, err := parseQuota([]byte(`{"subscription":{"limit":135,"requests":27,"renewsAt":"2025-09-21T14:36:14.288Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeQuota(r)
	if len(got.Groups) != 1 || got.Groups[0].Buckets[0].Window != "Renewal" || got.Groups[0].Buckets[0].RemainingFraction != 0.8 {
		t.Fatalf("subscription bucket wrong: %+v", got)
	}
	used, limit := -1.0, -1.0
	for _, metric := range got.Summary {
		switch metric.Key {
		case "subscription_requests_used":
			used = metric.Value
		case "subscription_request_limit":
			limit = metric.Value
		}
	}
	if used != 27 || limit != 135 {
		t.Fatalf("subscription absolutes missing from Summary: %+v", got.Summary)
	}
}

func TestNormalizeQuotaMissingRemainingDoesNotInventQuota(t *testing.T) {
	got := normalizeQuota(quotaReading{Shape: "planWeeklyLimit"})
	if len(got.Groups) != 0 || len(got.Summary) != 0 {
		t.Fatalf("missing remaining percentage must not create quota data: %+v", got)
	}
}

func TestMultiShapeQuotaEmitsOneBucketPerShape(t *testing.T) {
	readings, err := parseQuotas([]byte(`{"planWeeklyLimit":{"percentRemaining":25,"nextRegenAt":"2030-01-01T00:00:00Z"},` +
		`"rollingFiveHourLimit":{"remaining":1,"max":2},` +
		`"subscription":{"limit":100,"requests":40,"renewsAt":"2030-02-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 3 {
		t.Fatalf("expected 3 shapes, got %+v", readings)
	}
	got := normalizeQuotas(readings)
	if len(got.Groups) != 1 {
		t.Fatalf("expected one group for all shapes: %+v", got)
	}
	if got.Groups[0].DisplayName != "Synthetic" {
		t.Fatalf("group %+v does not use the plugin name", got.Groups[0])
	}
	if len(got.Groups[0].Buckets) != 3 {
		t.Fatalf("expected one bucket per shape, got %+v", got)
	}
	windows := map[string]float64{}
	resets := map[string]string{}
	for _, b := range got.Groups[0].Buckets {
		windows[b.Window] = b.RemainingFraction
		resets[b.Window] = b.ResetTime
		if b.Description != "" {
			t.Fatalf("bucket %+v carries description text", b)
		}
	}
	// The weekly nextRegenAt is an incremental refill with no quantified
	// increment here, so it must not pose as a reset; the subscription
	// renewal is a genuine reset and keeps its timestamp.
	if resets["weekly"] != "" {
		t.Fatalf("incremental refill shown as reset: %+v", got)
	}
	if resets["Renewal"] == "" {
		t.Fatalf("renewal lost its reset time: %+v", got)
	}
	if windows["weekly"] != 0.25 || windows[providerkit.CardWindowFiveHour] != 0.5 || windows["Renewal"] != 0.6 {
		t.Fatalf("wrong per-shape fractions: %+v", got)
	}
	// Only the subscription's absolute counters become summary metrics;
	// weekly and five-hour regeneration never emits a refill chip.
	if len(got.Summary) != 2 || got.Summary[0].Key != "subscription_requests_used" || got.Summary[0].Value != 40 || got.Summary[1].Key != "subscription_request_limit" || got.Summary[1].Value != 100 {
		t.Fatalf("subscription counters lost in multi-shape output: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
	// A single-shape input renders exactly as before.
	single, err := parseQuota([]byte(`{"rollingFiveHourLimit":{"remaining":1,"max":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if multi := normalizeQuotas([]quotaReading{single}); len(multi.Groups) != 1 || multi.Groups[0].DisplayName != "Synthetic" {
		t.Fatalf("single-shape output changed: %+v", multi)
	}
}

// The legacy subscription row must render untruncated beside its renewal
// timestamp: "Subscription" was cut to "Subscri…" on staging, and it matches
// no window the panel recognises. The short "Renewal" label keeps the
// genuine renewal reset time, while weekly and five-hour rows keep no reset
// time (their nextRegenAt is an incremental refill, not a reset).
func TestSubscriptionRowKeepsShortRenewalLabel(t *testing.T) {
	renewAt := time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)
	got := normalizeQuota(quotaReading{Shape: "subscription", PercentRemaining: floatPtr(60), NextRegenAt: renewAt})
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("subscription must render one bucket: %+v", got)
	}
	bucket := got.Groups[0].Buckets[0]
	if bucket.Window != "Renewal" {
		t.Fatalf("subscription label = %q, want %q: %+v", bucket.Window, "Renewal", got)
	}
	if len(bucket.Window) > 8 {
		t.Fatalf("subscription label %q is too long to render beside its reset time", bucket.Window)
	}
	if bucket.Description != "" {
		t.Fatalf("subscription bucket carries description text: %+v", bucket)
	}
	if bucket.ResetTime == "" {
		t.Fatalf("renewal lost its reset time: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
}

// The card carries no refill chip: regeneration percents are incremental
// hints, not plan data, so they stay off the card however they are reported.
func TestNoRefillChip(t *testing.T) {
	regen := func(pct *float64) pluginapi.QuotaFetchResponse {
		return normalizeQuota(quotaReading{Shape: "planWeeklyLimit", PercentRemaining: floatPtr(25), NextRegenPercent: pct})
	}
	for _, pct := range []*float64{floatPtr(2), floatPtr(0), nil} {
		if got := regen(pct); len(got.Summary) != 0 {
			t.Fatalf("refill percent must not emit a chip: %+v", got)
		}
	}
	if got := regen(floatPtr(2)); len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("bar missing without the chip: %+v", got)
	}
	if violations := providerkit.CheckCard(regen(floatPtr(2))); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
}

// Weekly and five-hour shapes render side by side as two bars with no
// summary: neither tick poses as the other's refill.
func TestRefillShapesRenderAsTwoBars(t *testing.T) {
	weekly := quotaReading{Shape: "planWeeklyLimit", PercentRemaining: floatPtr(25), NextRegenPercent: floatPtr(2)}
	tick := quotaReading{Shape: "rollingFiveHourLimit", PercentRemaining: floatPtr(50), NextRegenPercent: floatPtr(10)}
	got := normalizeQuotas([]quotaReading{weekly, tick})
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 2 || len(got.Summary) != 0 {
		t.Fatalf("two shapes must render two bars and no summary: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
}
