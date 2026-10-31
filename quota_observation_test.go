package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestLateQuotaPollCannotOverwriteNewerHeaderRetryEvidence(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + now.Add(time.Hour).Format(time.RFC3339Nano) + `"}}`))
	}))
	defer server.Close()
	q := newQuotaProvider(server.URL, server.Client())
	finished := make(chan error, 1)
	go func() {
		_, err := q.cache.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "account", StorageJSON: []byte(`{"api_key":"example-key"}`)})
		finished <- err
	}()
	<-started
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "account", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + now.Add(120*time.Second).Format(time.RFC3339Nano) + `"}}`}}})
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, q.latest("account"))
	if e.RetryAfterSeconds != 120 {
		t.Fatalf("older poll replaced header retry evidence: %+v", e)
	}
}
