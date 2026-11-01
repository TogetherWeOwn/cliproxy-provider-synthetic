package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
	result, err := q.cache.FetchQuota(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || result.Groups[0].Buckets[0].RemainingFraction != 0.25 || result.Groups[0].Buckets[0].ResetTime != "2030-01-01T00:00:00Z" {
		t.Fatalf("unexpected quota result: %+v", result)
	}
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "a", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
	result, err = q.cache.FetchQuota(context.Background(), req)
	if err != nil || result.Groups[0].Buckets[0].RemainingFraction != 0.8 || count != 1 {
		t.Fatalf("header did not replace cached poll: %+v %v", result, err)
	}
}
func TestQuotaUsageRejectsForeignProvider(t *testing.T) {
	q := newQuotaProvider("", nil)
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "foreign", AuthID: "a", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":80}}`}}})
	_, err := q.cache.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "a"})
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
	if len(got.Groups) != 1 || got.Groups[0].Buckets[0].Window == "Weekly credits" {
		t.Fatalf("legacy five-hour quota mislabeled as weekly credits: %+v", got)
	}
	if len(got.Summary) != 0 {
		t.Fatalf("legacy quota must not emit a weekly summary: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
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
	for _, b := range got.Groups[0].Buckets {
		windows[b.Window] = b.RemainingFraction
		if b.Description != "" {
			t.Fatalf("bucket %+v carries description text", b)
		}
	}
	if windows["Weekly credits"] != 0.25 || windows[providerkit.CardWindowFiveHour] != 0.5 || windows["Subscription"] != 0.6 {
		t.Fatalf("wrong per-shape fractions: %+v", got)
	}
	// None of these shapes regenerates, so no refill chip is emitted.
	if len(got.Summary) != 0 {
		t.Fatalf("unexpected summary: %+v", got)
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

// A non-zero regeneration percent appears as a refill chip; a zero percent
// and a missing percent emit no summary.
func TestNextRefillChipOnlyWhenNonZero(t *testing.T) {
	regen := func(pct *float64) pluginapi.QuotaFetchResponse {
		return normalizeQuota(quotaReading{Shape: "planWeeklyLimit", PercentRemaining: floatPtr(25), NextRegenPercent: pct})
	}
	got := regen(floatPtr(2))
	if len(got.Summary) != 1 || got.Summary[0].Key != "next_refill_percent" || got.Summary[0].Value != 2 {
		t.Fatalf("refill chip missing: %+v", got)
	}
	if violations := providerkit.CheckCard(got); len(violations) != 0 {
		t.Fatalf("CheckCard violations: %v", violations)
	}
	if got := regen(floatPtr(0)); len(got.Summary) != 0 {
		t.Fatalf("zero refill must not emit a chip: %+v", got)
	}
	if got := regen(nil); len(got.Summary) != 0 {
		t.Fatalf("missing refill must not emit a chip: %+v", got)
	}
}
