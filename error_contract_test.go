package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestFailureClassificationContract(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                    string
		status                  int
		body, quota, retryAfter string
		code                    string
		retry                   bool
		delay                   int
	}{
		{"auth", 401, "example-secret", "", "", "invalid_credentials", false, 0},
		{"payment", 402, "example-secret", "", "", "payment_required", false, 3600},
		{"entitlement", 403, "Big required", "", "", "entitlement_required", false, 3600},
		{"rotated pinned model", 404, "model unavailable", "", "", "upstream_not_found", false, 0},
		{"transient concurrency", 429, "Too many concurrent requests", `{"planWeeklyLimit":{"percentRemaining":20}}`, "", "concurrency_limit", true, 5},
		{"transient alternate wording", 429, "Concurrency limit", `{"planWeeklyLimit":{"percentRemaining":20}}`, "", "concurrency_limit", true, 5},
		{"no positive evidence", 429, "Too many concurrent requests", "", "", "quota_exhausted", true, 60},
		{"zero remaining", 429, "Too many concurrent requests", `{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:02:00Z"}}`, "", "quota_exhausted", true, 120},
		{"bare rate limit", 429, "rate limited", "", "", "quota_exhausted", true, 60},
		{"retry seconds", 429, "", `{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:02:00Z"}}`, "8", "quota_exhausted", true, 8},
		{"retry HTTP date", 429, "", "", "Tue, 01 Jan 2030 00:00:09 GMT", "quota_exhausted", true, 9},
		{"past reset", 429, "", `{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2029-01-01T00:00:00Z"}}`, "", "quota_exhausted", true, 60},
		{"upstream overload", 503, "example-secret", "", "12", "upstream_error", true, 12},
		{"bad request", 400, "example-secret", "", "", "upstream_error", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			if tc.quota != "" {
				h.Set("X-Synthetic-Quotas", tc.quota)
			}
			if tc.retryAfter != "" {
				h.Set("Retry-After", tc.retryAfter)
			}
			e := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body), Headers: h}, now)
			if e.Code != tc.code || e.HTTPStatus != tc.status || e.Retryable != tc.retry || e.RetryAfterSeconds != tc.delay {
				t.Fatalf("classification %+v", e)
			}
			if e.Message == tc.body {
				t.Fatal("error exposed upstream body")
			}
		})
	}
}

func TestExecutorExhaustionUsesFreshCachedRegenerationWithoutPolling(t *testing.T) {
	q := newQuotaProvider("", nil)
	future := time.Now().Add(120 * time.Second).UTC().Format(time.RFC3339Nano)
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "account", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + future + `"}}`}}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer server.Close()
	_, err := newExecutor(q).Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "account", Payload: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	var e *pluginabi.Error
	if !errors.As(err, &e) || e.Code != "quota_exhausted" || e.RetryAfterSeconds < 119 || e.RetryAfterSeconds > 120 {
		t.Fatalf("cached regeneration not used %+v", err)
	}
	_, err = newExecutor(q).Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "other-account", Payload: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if !errors.As(err, &e) || e.RetryAfterSeconds != 60 {
		t.Fatalf("cache leaked between accounts %v", err)
	}
}

func TestExecutorPropagatesStreamAndNonstreamStatusErrors(t *testing.T) {
	for _, status := range []int{401, 402, 403, 404, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"example-secret"}`))
			}))
			defer server.Close()
			r := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}}
			ex := newExecutor(nil)
			_, err := ex.Execute(context.Background(), r)
			var nonstream *pluginabi.Error
			if !errors.As(err, &nonstream) {
				t.Fatalf("no typed error: %v", err)
			}
			_, err = ex.ExecuteStream(context.Background(), r)
			var stream *pluginabi.Error
			if !errors.As(err, &stream) || *stream != *nonstream {
				t.Fatalf("inconsistent error classification %v vs %v", stream, nonstream)
			}
		})
	}
}
