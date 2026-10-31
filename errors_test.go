package synthetic

import (
	"net/http"
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
	}{
		{"concurrency", 429, "Too many concurrent requests", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":10}}`}}, "concurrency_limit", true, 5},
		{"exhaustion", 429, "", http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"2030-01-01T00:01:00Z"}}`}}, "quota_exhausted", true, 60},
		{"payment", 402, "", nil, "payment_required", false, 3600},
		{"authentication", 401, "", nil, "invalid_credentials", false, 0},
		{"model missing", 404, "", nil, "upstream_not_found", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classifyFailure(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body), Headers: tc.header}, now)
			if e.Code != tc.code || e.Retryable != tc.retry || e.RetryAfterSeconds != tc.delay {
				t.Fatalf("got %+v", e)
			}
		})
	}
}
