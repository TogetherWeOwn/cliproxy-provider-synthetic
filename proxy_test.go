package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaPollUsesAuthOrHostProxy(t *testing.T) {
	recorder := func() (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var seen []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.String())
			mu.Unlock()
			fmt.Fprint(w, `{"planWeeklyLimit":{"percentRemaining":40}}`)
		}))
		t.Cleanup(s.Close)
		return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	}
	authProxy, authSeen := recorder()
	hostProxy, hostSeen := recorder()
	client := newQuotaHTTPClient()
	defer client.CloseIdleConnections()
	q := newQuotaProvider("http://synthetic.invalid/v2/quotas", client)
	host := pluginapi.HostConfigSummary{ProxyURL: hostProxy.URL}
	for _, r := range []pluginapi.QuotaFetchRequest{
		{AuthID: "a", StorageJSON: []byte(`{"api_key":"one"}`), Metadata: map[string]any{"proxy_url": authProxy.URL}, Host: host},
		{AuthID: "b", StorageJSON: []byte(`{"api_key":"two"}`), Host: host},
	} {
		if _, err := q.cache.FetchQuota(context.Background(), r); err != nil {
			t.Fatalf("%s: %v", r.AuthID, err)
		}
	}
	if got := authSeen(); len(got) != 1 || got[0] != "http://synthetic.invalid/v2/quotas" {
		t.Fatalf("auth proxy saw %v", got)
	}
	if got := hostSeen(); len(got) != 1 || got[0] != "http://synthetic.invalid/v2/quotas" {
		t.Fatalf("host proxy saw %v", got)
	}
}

// HTTP/2 connections are reused by destination before the proxy is chosen, so
// a transport shared across auths must not speak HTTP/2.
func TestQuotaTransportDoesNotShareConnectionsAcrossProxies(t *testing.T) {
	transport, ok := newQuotaHTTPClient().Transport.(*http.Transport)
	if !ok || transport.Proxy == nil || transport.Protocols == nil || transport.Protocols.HTTP2() || !transport.Protocols.HTTP1() {
		t.Fatal("quota transport must be HTTP/1.1-only with a proxy function")
	}
}
