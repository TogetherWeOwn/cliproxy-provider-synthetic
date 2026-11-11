package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestClassifyFailure(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		body   string
		header http.Header
		code   string
		retry  bool
		delay  int
		scoped bool
	}{
		{"concurrency", 429, "Too many concurrent requests", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":10}}`}}, "concurrency_limit", true, 5, false},
		{"exhaustion", 429, "", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:01:00Z"}}`}}, "quota_exhausted", true, 60, true},
		{"header zero no regen", 429, "", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0}}`}}, "quota_exhausted", true, 60, true},
		{"regen-only header unparseable", 429, "", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"nextRegenAt":"2030-01-01T00:01:00Z"}}`}}, "quota_exhausted", true, 60, false},
		{"positive remaining no concurrency wording", 429, "slow down", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":10}}`}}, "quota_exhausted", true, 60, false},
		{"positive first bucket exhausted second", 429, "rate limited", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50},"rollingFiveHourLimit":{"remaining":0,"max":100,"nextTickAt":"2030-01-01T00:01:00Z"}}`}}, "quota_exhausted", true, 60, true},
		{"positive first bucket regen exhausted second longer", 429, "rate limited", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50,"nextRegenAt":"2030-01-01T00:00:30Z"},"rollingFiveHourLimit":{"remaining":0,"max":100,"nextTickAt":"2030-01-01T02:00:00Z"}}`}}, "quota_exhausted", true, 7200, true},
		{"positive first bucket exhausted second regen only", 429, "rate limited", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50},"rollingFiveHourLimit":{"remaining":0,"max":100}}`}}, "quota_exhausted", true, 60, true},
		{"positive first regen exhausted second no regen", 429, "rate limited", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50,"nextRegenAt":"2030-01-01T00:00:30Z"},"rollingFiveHourLimit":{"remaining":0,"max":100}}`}}, "quota_exhausted", true, 60, true},
		{"positive remaining with future regen", 429, "slow down", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":10,"nextRegenAt":"2030-01-01T00:01:00Z"}}`}}, "quota_exhausted", true, 60, false},
		{"quota body marker", 429, `{"error":"Weekly quota exhausted"}`, nil, "quota_exhausted", true, 60, true},
		{"credit body marker", 429, `{"error":"weekly credits exhausted"}`, nil, "quota_exhausted", true, 60, true},
		{"generic header-less 429", 429, "", nil, "quota_exhausted", true, 60, false},
		{"bare rate limit", 429, "rate limited", nil, "quota_exhausted", true, 60, false},
		{"marker past scan bound", 429, strings.Repeat("x", 4096) + "quota", nil, "quota_exhausted", true, 60, false},
		{"payment", 402, "", nil, "payment_required", false, 3600, false},
		{"authentication", 401, "", nil, "invalid_credentials", false, 0, false},
		{"model missing", 404, "", nil, "upstream_not_found", false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body), Headers: tc.header}, now)
			if e.Code != tc.code || e.Retryable != tc.retry || e.RetryAfterSeconds != tc.delay || e.CredentialScoped != tc.scoped {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

// TestClassifyFailureScopedRetryAfter pins the exact reset-derived
// RetryAfterSeconds on the scoped branch under a fixed now.
func TestClassifyFailureScopedRetryAfter(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	// Header regen 61.4s away rounds up to 62; credential scope is set.
	e := classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Headers:    http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:01:01.4Z"}}`}},
	}, now)
	if !e.CredentialScoped || e.RetryAfterSeconds != 62 {
		t.Fatalf("scoped regen retry-after %+v", e)
	}
	// A server Retry-After overrides the regen-derived timing as today.
	e = classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Headers: http.Header{
			"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:01:00Z"}}`},
			"Retry-After":        {"8"},
		},
	}, now)
	if !e.CredentialScoped || e.RetryAfterSeconds != 8 {
		t.Fatalf("server Retry-After precedence %+v", e)
	}
	// A bound cached reading supplies regen evidence on a header-less 429.
	cached := []quotaReading{{NextRegenAt: now.Add(90 * time.Second)}}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, cached, cached)
	if !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("cached regen evidence %+v", e)
	}
	zero := float64(0)
	boundZero := []quotaReading{{PercentRemaining: &zero, NextRegenAt: now.Add(90 * time.Second)}}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, boundZero, boundZero)
	if !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("cached zero with future regen must scope %+v", e)
	}
	positive := float64(10)
	boundPositive := []quotaReading{{PercentRemaining: &positive, NextRegenAt: now.Add(90 * time.Second)}}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, boundPositive, boundPositive)
	if e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("cached positive percent with future regen must stay model-scoped %+v", e)
	}
	e = classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Headers:    http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":10,"nextRegenAt":"2030-01-01T00:01:01.4Z"}}`}},
	}, now)
	if e.CredentialScoped || e.RetryAfterSeconds != 62 {
		t.Fatalf("header positive percent with future regen must stay model-scoped %+v", e)
	}
	boundZeroNoRegen := []quotaReading{{PercentRemaining: &zero}}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, boundZeroNoRegen, boundZeroNoRegen)
	if e.CredentialScoped || e.RetryAfterSeconds != 60 {
		t.Fatalf("cached zero without regen must stay model-scoped %+v", e)
	}
	boundPast := []quotaReading{{NextRegenAt: now.Add(-time.Minute)}}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, boundPast, boundPast)
	if e.CredentialScoped || e.RetryAfterSeconds != 60 {
		t.Fatalf("past regen must stay model-scoped %+v", e)
	}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, []quotaReading{{PercentRemaining: &zero, NextRegenAt: now.Add(90 * time.Second)}}, nil)
	if e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("unbound cached reading must stay a retry hint only %+v", e)
	}
	// A body marker still scopes on response-local evidence; an unbound
	// hint's exhausted refill may time it without authorizing it.
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}, Body: []byte(`{"error":"weekly credits exhausted"}`)}, now, []quotaReading{{PercentRemaining: &positive, NextRegenAt: now.Add(90 * time.Second)}}, nil)
	if !e.CredentialScoped {
		t.Fatalf("response-local quota marker must still scope %+v", e)
	}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}, Body: []byte(`{"error":"weekly credits exhausted"}`)}, now, []quotaReading{{PercentRemaining: &zero, NextRegenAt: now.Add(120 * time.Second)}}, nil)
	if !e.CredentialScoped || e.RetryAfterSeconds != 120 {
		t.Fatalf("body marker scope must take the unbound exhausted refill timing %+v", e)
	}
}

// TestClassifyFailureMultiBucketEvidence proves exhaustion evidence is
// evaluated across every parsed bucket and that a scoped cooldown is
// timed by the exhausted buckets, never a positive bucket's tick.
func TestClassifyFailureMultiBucketEvidence(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	// Positive weekly beside an empty 5h bucket: the second bucket scopes
	// and its own tick times the cooldown.
	e := classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Body:       []byte("rate limited"),
		Headers: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50,"nextRegenAt":"2030-01-01T00:00:30Z"},` +
			`"rollingFiveHourLimit":{"remaining":0,"max":100,"nextTickAt":"2030-01-01T00:01:00Z"}}`}},
	}, now)
	if !e.CredentialScoped || e.RetryAfterSeconds != 60 {
		t.Fatalf("exhausted second bucket must scope with its own refill timing %+v", e)
	}
	e = classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Body:       []byte("rate limited"),
		Headers: http.Header{"X-Synthetic-Quotas": {`{"rollingFiveHourLimit":{"remaining":0,"max":100,"nextTickAt":"2030-01-01T00:01:00Z"},` +
			`"subscription":{"limit":100,"requests":100,"renewsAt":"2030-01-01T02:00:00Z"}}`}},
	}, now)
	if !e.CredentialScoped || e.RetryAfterSeconds != 7200 {
		t.Fatalf("cooldown must last until the latest exhausted bucket refills %+v", e)
	}
	positive := float64(50)
	zero := float64(0)
	mixed := []quotaReading{
		{PercentRemaining: &positive, NextRegenAt: now.Add(30 * time.Second), Shape: "planWeeklyLimit"},
		{PercentRemaining: &zero, NextRegenAt: now.Add(90 * time.Second), Shape: "rollingFiveHourLimit"},
	}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, mixed, mixed)
	if !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("cached exhausted second bucket must scope with its own refill timing %+v", e)
	}
	// A newer unbound hint must not hide a still-fresh bound poll.
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, []quotaReading{
		{PercentRemaining: &positive, NextRegenAt: now.Add(45 * time.Second), Shape: "planWeeklyLimit"},
	}, mixed)
	if !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("newer unbound header must not shadow the bound poll's exhaustion %+v", e)
	}
	allPositive := []quotaReading{
		{PercentRemaining: &positive, NextRegenAt: now.Add(30 * time.Second), Shape: "planWeeklyLimit"},
		{PercentRemaining: &positive, Shape: "rollingFiveHourLimit"},
	}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{}}, now, allPositive, allPositive)
	if e.CredentialScoped || e.RetryAfterSeconds != 30 {
		t.Fatalf("all-positive buckets must stay model-scoped %+v", e)
	}
	// Concurrency wording with a positive first bucket still wins over an
	// exhausted second bucket: the model-scoped 5s branch is unchanged.
	e = classifyFailure(pluginapi.HTTPResponse{
		StatusCode: 429,
		Body:       []byte("Too many concurrent requests"),
		Headers: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":50},` +
			`"rollingFiveHourLimit":{"remaining":0,"max":100,"nextTickAt":"2030-01-01T00:01:00Z"}}`}},
	}, now)
	if e.CredentialScoped || e.Code != "concurrency_limit" || e.RetryAfterSeconds != 5 {
		t.Fatalf("concurrency precedence changed %+v", e)
	}
}
