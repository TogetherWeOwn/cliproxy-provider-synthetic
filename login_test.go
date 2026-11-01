package main

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

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
	if !strings.HasPrefix(start.URL, "http://127.0.0.1:19000"+testResourceBase+"/login?state=") {
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
