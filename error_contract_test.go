package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
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
	// A header observation carries no credential material, so it stays an
	// unbound retry hint and can never scope whichever credential is current.
	if e.CredentialScoped {
		t.Fatalf("unbound header observation must not scope the credential %+v", e)
	}
	_, err = newExecutor(q).Execute(context.Background(), pluginapi.ExecutorRequest{AuthID: "other-account", Payload: []byte(`{"model":"synthetic/syn:small:text"}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}})
	if !errors.As(err, &e) || e.RetryAfterSeconds != 60 {
		t.Fatalf("cache leaked between accounts %v", err)
	}
	if e.CredentialScoped {
		t.Fatalf("header-less generic 429 must stay model-scoped %+v", e)
	}
}

// TestExecutorStreamQuotaErrorIsCredentialScoped proves the stream path
// surfaces the same evidence-gated credential scope as Execute: a non-2xx
// stream response classifies through the shared ClassifyError, and the
// kit runtime's stream-close error_detail passes the mapped
// *pluginabi.Error through MapError with the scoped pair intact.
func TestExecutorStreamQuotaErrorIsCredentialScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Synthetic-Quotas", `{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:00:00Z"}}`)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"fixture upstream error"}`))
	}))
	defer server.Close()
	r := pluginapi.ExecutorRequest{Payload: []byte(`{"model":"synthetic/syn:small:text","stream":true}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: mockHostClient{server}}
	_, err := newExecutor(nil).ExecuteStream(context.Background(), r)
	var e *pluginabi.Error
	if !errors.As(err, &e) {
		t.Fatalf("stream error not typed: %v", err)
	}
	if !e.CredentialScoped || e.HTTPStatus != 429 || e.RetryAfterSeconds <= 0 {
		t.Fatalf("scoped stream error %+v", e)
	}
	remapped := kit.MapError(err)
	if !remapped.CredentialScoped || remapped.RetryAfterSeconds != e.RetryAfterSeconds || remapped.HTTPStatus != 429 {
		t.Fatalf("error_detail mapping lost the scoped pair %+v", remapped)
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
