package main

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/synthetic-id"
	testSubmitPath   = "/v0/management/plugin-login/synthetic"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

// TestAPIKeyLoginWired drives the management-UI login flow end to end through
// the provider's own ParseAuth pipeline: page, key+plan submit, poll.
func TestAPIKeyLoginWired(t *testing.T) {
	ctx := context.Background()
	p, err := newAuthProvider()
	if err != nil {
		t.Fatal(err)
	}
	login := p.Login
	if login == nil {
		t.Fatal("API-key login not wired")
	}
	reg, err := login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase})
	if err != nil {
		t.Fatal(err)
	}
	var sawSubmit, sawPage bool
	for _, r := range reg.Routes {
		if r.Method == "POST" && r.Path == "plugin-login/synthetic" {
			sawSubmit = true
		}
	}
	for _, r := range reg.Resources {
		if r.Path == "login" {
			sawPage = true
		}
	}
	if !sawSubmit || !sawPage {
		t.Fatalf("login routes not registered: %+v", reg)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "synthetic",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/synthetic-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(start.URL, testResourceBase+"/login?state=") {
		t.Fatalf("unexpected login URL %q", start.URL)
	}
	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}
	page, err := login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET",
		Path: testResourceBase + "/login", Query: map[string][]string{"state": {start.State}}})
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("login page %+v %v", page, err)
	}
	body := html.UnescapeString(string(page.Body))
	for _, want := range []string{"read -r PLUGIN_FIELD_PLAN", `"plan":"$ESC_FIELD_PLAN"`, "Plan (optional)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing plan wiring %q", want)
		}
	}
	submit, err := login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","plan":"big"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v %v", done, err)
	}
	var store map[string]any
	if err := json.Unmarshal(done.Auth.StorageJSON, &store); err != nil {
		t.Fatal(err)
	}
	if done.Auth.Provider != "synthetic" || store["plan"] != "big" {
		t.Fatalf("plan lost through login: %+v %s", done.Auth, done.Auth.StorageJSON)
	}
	// A second poll must not redeliver credentials.
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("consumed login redelivered: %+v %v", again, err)
	}
}

// TestAPIKeyLoginRejectsBadPlan submits an invalid plan through the login
// endpoint and expects the provider's own validation to reject it.
func TestAPIKeyLoginRejectsBadPlan(t *testing.T) {
	ctx := context.Background()
	p, err := newAuthProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase}); err != nil {
		t.Fatal(err)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "synthetic",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/synthetic-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","plan":"unlimited"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if submit.StatusCode != 400 || !strings.Contains(string(submit.Body), "plan must be") {
		t.Fatalf("bad plan accepted: %+v", submit)
	}
	if strings.Contains(string(submit.Body), "example-key") {
		t.Fatalf("rejection echoed the key: %s", submit.Body)
	}
}

// TestLoginAbandonedReleasesOwnership mirrors the kit's
// TestLoginAbandonedSubmitReleasesOwnership against this provider's own
// ParseAuth pipeline: a key submitted through login but never polled holds
// its ownership only until the login session expires. After that, parsing
// the same key under a different filename succeeds (and commits ownership)
// instead of failing with duplicate_auth until plugin restart.
func TestLoginAbandonedReleasesOwnership(t *testing.T) {
	ctx := context.Background()
	p, err := newAuthProviderWithLoginTTL(500 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase}); err != nil {
		t.Fatal(err)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "synthetic",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/synthetic-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err := p.Login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"abandoned-key"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	// The login is abandoned here: it is never polled.
	upload := func(file string) error {
		_, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "synthetic", FileName: file,
			RawJSON: []byte(`{"type":"synthetic","api_key":"abandoned-key"}`)})
		return err
	}
	// While the login is still in flight its reservation blocks other files.
	if err := upload("upload.json"); err == nil || kit.MapError(err).Code != "duplicate_auth" {
		t.Fatalf("upload during login = %v, want duplicate_auth", err)
	}
	// After the abandoned session's TTL lapses the same upload owns the key.
	// A rejected claim records nothing, so retrying until the reservation
	// lapses is safe; the deadline keeps a stuck guard from hanging the test.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := upload("upload.json"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("upload after abandoned session expired: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The successful upload committed ownership, so a third filename is a
	// duplicate again.
	if err := upload("third.json"); err == nil || kit.MapError(err).Code != "duplicate_auth" {
		t.Fatalf("third file after commit = %v, want duplicate_auth", err)
	}
}
