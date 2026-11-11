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
	// The newer header still owns the unscoped retry hint, but a
	// header-less 429 on this generation is scoped by the still-fresh
	// bound poll and timed by the bound poll's exhausted refill.
	digest := credentialDigest([]byte(`{"api_key":"example-key"}`), nil)
	hint, bound := q.latestReadings("account", digest)
	if bound == nil {
		t.Fatal("bound poll hidden behind the newer unbound header")
	}
	if got := hint[0].NextRegenAt; !got.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("newer header lost the retry hint: %+v", hint)
	}
	e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, hint, bound)
	if !e.CredentialScoped || e.RetryAfterSeconds != 3600 {
		t.Fatalf("bound poll evidence must scope and time the cooldown: %+v", e)
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
	if hint := q.latest("account", digestA); hint == nil || !hint.NextRegenAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("bound hint missed: %+v", hint)
	}
	// … and same-generation bound evidence still scopes a header-less 429.
	hint, bound := q.latestReadings("account", digestA)
	if bound == nil {
		t.Fatal("bound poll reported unbound")
	}
	if e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, hint, bound); !e.CredentialScoped || e.RetryAfterSeconds != 3600 {
		t.Fatalf("same-generation bound evidence must scope: %+v", e)
	}
	// … but the hint never serves a rotated credential under the reused
	// identity.
	if hint := q.latest("account", digestB); hint != nil {
		t.Fatalf("stale generation hint served: %+v", hint)
	}
	// Header observations carry no credential context, so they stay unbound
	// and still feed the retry hint of every generation until the TTL
	// expires — including a rotated key under the same auth identity …
	q.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "synthetic", AuthID: "account", ResponseHeaders: http.Header{"X-Synthetic-Quotas": {`{"planWeeklyLimit":{"percentRemaining":0,"nextRegenAt":"` + now.Add(120*time.Second).Format(time.RFC3339Nano) + `"}}`}}})
	if hint := q.latest("account", digestB); hint == nil {
		t.Fatal("unbound header hint not served")
	}
	// … and the newer unbound header wins over the older bound poll.
	if hint := q.latest("account", digestA); hint == nil || !hint.NextRegenAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("newer header did not win: %+v", hint)
	}
	// … but an unbound observation never scopes: A's 0%+regen header
	// must not cool rotated key B's generic 429; the hint still applies.
	hint, bound = q.latestReadings("account", digestB)
	if bound != nil {
		t.Fatal("unbound header observation reported bound")
	}
	e := classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, hint, bound)
	if e.CredentialScoped || e.RetryAfterSeconds != 120 {
		t.Fatalf("unbound A evidence scoped rotated B: %+v", e)
	}
	// A's own generation differs: A's still-fresh bound poll scopes its
	// header-less 429 — a newer unbound header must not hide it.
	hint, bound = q.latestReadings("account", digestA)
	if bound == nil {
		t.Fatal("bound poll hidden behind the newer unbound header")
	}
	e = classifyWithQuota(pluginapi.HTTPResponse{StatusCode: 429}, now, hint, bound)
	if !e.CredentialScoped || e.RetryAfterSeconds != 3600 {
		t.Fatalf("bound poll evidence shadowed by unbound header: %+v", e)
	}
}

func TestQuotaHintsExpireForBoundAndUnboundReadings(t *testing.T) {
	now := time.Now().UTC()
	digest := credentialDigest([]byte(`{"api_key":"example-key"}`), nil)
	for _, hasCred := range []bool{true, false} {
		q := &quotaProvider{now: func() time.Time { return now }}
		key := readingKey{id: "account", hasCred: hasCred}
		if hasCred {
			key.cred = digest
		}
		observedAt := now
		q.remember(key, []quotaReading{{PercentRemaining: floatPtr(0), NextRegenAt: now.Add(time.Hour)}}, observedAt)
		if q.latest("account", digest) == nil {
			t.Fatalf("fresh hint missing, bound=%v", hasCred)
		}
		now = now.Add(61 * time.Second)
		if hint := q.latest("account", digest); hint != nil {
			t.Fatalf("expired hint served, bound=%v: %+v", hasCred, hint)
		}
		if len(q.readings) != 0 {
			t.Fatalf("expired hint retained, bound=%v", hasCred)
		}
	}
}
