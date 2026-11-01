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
	e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, q.latest("account", credentialDigest([]byte(`{"api_key":"example-key"}`), nil)))
	if e.RetryAfterSeconds != 120 {
		t.Fatalf("older poll replaced newer header retry evidence: %+v", e)
	}
}

func TestQuotaHintsAreBoundToTheirCredentialGeneration(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + now.Add(time.Hour).Format(time.RFC3339Nano) + `"}}`))
	}))
	defer server.Close()
	q := newQuotaProvider(server.URL, server.Client())
	genA := pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "account", StorageJSON: []byte(`{"api_key":"key-a"}`)}
	genB := pluginapi.QuotaFetchRequest{Provider: "synthetic", AuthID: "account", StorageJSON: []byte(`{"api_key":"key-b"}`)}
	if _, err := q.cache.FetchQuota(context.Background(), genA); err != nil {
		t.Fatal(err)
	}
	digestA := credentialDigest(genA.StorageJSON, genA.Metadata)
	digestB := credentialDigest(genB.StorageJSON, genB.Metadata)
	// The bound poll hint serves its own generation …
	if hint := q.latest("account", digestA); hint == nil || hint.NextRegenAt.Hour() != now.Add(time.Hour).Hour() {
		t.Fatalf("bound hint missed: %+v", hint)
	}
	// … but never a rotated credential under the reused identity.
	if hint := q.latest("account", digestB); hint != nil {
		t.Fatalf("stale generation hint served: %+v", hint)
	}
	// Header observations carry no credential context, so they stay unbound
	// and serve every generation until the TTL expires.
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "account", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + now.Add(120*time.Second).Format(time.RFC3339Nano) + `"}}`}}})
	if hint := q.latest("account", digestB); hint == nil {
		t.Fatal("unbound header hint not served")
	}
	// … and the newer unbound header wins over the older bound poll.
	if hint := q.latest("account", digestA); hint == nil || hint.NextRegenAt.After(now.Add(time.Hour)) {
		t.Fatalf("newer header did not win: %+v", hint)
	}
}
