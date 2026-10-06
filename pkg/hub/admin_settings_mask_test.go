// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	yamlv3 "gopkg.in/yaml.v3"
)

const (
	rtWebhook    = "https://hooks.example.test/services/T000/B000/real"
	rtPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nreal\n-----END RSA PRIVATE KEY-----\n"
	rtWebhookSec = "whsec_real"
	rtOAuthSec   = "oauth-real-secret"
)

// setTempScionHome points HOME at a fresh temp dir with a .scion directory
// and returns the settings.yaml path.
func setTempScionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".scion", "settings.yaml")
}

// getServerSection decodes the "server" object of a GET response body.
func getServerSection(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	server, ok := resp["server"].(map[string]interface{})
	if !ok {
		t.Fatalf("GET body has no server object: %s", body)
	}
	return server
}

func firstChannelParams(t *testing.T, server map[string]interface{}) map[string]interface{} {
	t.Helper()
	chans, ok := server["notification_channels"].([]interface{})
	if !ok || len(chans) == 0 {
		t.Fatalf("no notification_channels in %v", server)
	}
	params, _ := chans[0].(map[string]interface{})["params"].(map[string]interface{})
	return params
}

// File mode: PUT real secrets, GET shows them masked, PUT the GET server
// block back with one unrelated change, and settings.yaml still holds the
// real values.
func TestPutServerConfig_FileMode_MaskedRoundTripKeepsSecrets(t *testing.T) {
	settingsPath := setTempScionHome(t)
	srv := &Server{}

	initial := `{"server":{
		"hub":{"port":9810,"admin_emails":["a@example.com"]},
		"oauth":{"web":{"google":{"client_id":"cid","client_secret":"` + rtOAuthSec + `"}}},
		"github_app":{"app_id":42,"private_key":` + jsonString(rtPrivateKey) + `,"webhook_secret":"` + rtWebhookSec + `"},
		"notification_channels":[{"type":"slack","params":{"webhook_url":"` + rtWebhook + `","channel":"#ops"}}]
	}}`
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", initial))
	if rr.Code != http.StatusOK {
		t.Fatalf("initial PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	server := getServerSection(t, rr.Body.Bytes())
	if got := firstChannelParams(t, server)["webhook_url"]; got != maskedValue {
		t.Fatalf("GET should mask webhook_url, got %v", got)
	}

	// Send the GET server block back with one unrelated change.
	server["hub"].(map[string]interface{})["admin_emails"] = []string{"b@example.com"}
	body, _ := json.Marshal(map[string]interface{}{"server": server})
	rr = httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("round-trip PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), maskedValue) {
		t.Errorf("settings.yaml contains the masked placeholder:\n%s", data)
	}
	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		t.Fatal(err)
	}
	s := vs.Server
	if got := s.NotificationChannels[0].Params["webhook_url"]; got != rtWebhook {
		t.Errorf("webhook_url = %q, want the real value", got)
	}
	if got := s.NotificationChannels[0].Params["channel"]; got != "#ops" {
		t.Errorf("channel = %q, want #ops", got)
	}
	if got := s.OAuth.Web.Google.ClientSecret; got != rtOAuthSec {
		t.Errorf("oauth web google client_secret = %q, want the real value", got)
	}
	if got := s.GitHubApp.PrivateKey; got != rtPrivateKey {
		t.Errorf("github_app private_key = %q, want the real value", got)
	}
	if got := s.GitHubApp.WebhookSecret; got != rtWebhookSec {
		t.Errorf("github_app webhook_secret = %q, want the real value", got)
	}
	if got := s.Hub.AdminEmails; len(got) != 1 || got[0] != "b@example.com" {
		t.Errorf("admin_emails = %v, want the edited value", got)
	}
}

// File mode: a real new value still replaces the stored one.
func TestPutServerConfig_FileMode_RealValueReplacesStored(t *testing.T) {
	settingsPath := setTempScionHome(t)
	srv := &Server{}
	put := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	}
	put(`{"server":{"github_app":{"app_id":1,"webhook_secret":"old"},"notification_channels":[{"type":"slack","params":{"webhook_url":"https://old"}}]}}`)
	put(`{"server":{"github_app":{"app_id":1,"webhook_secret":"new"},"notification_channels":[{"type":"slack","params":{"webhook_url":"https://new"}}]}}`)

	data, _ := os.ReadFile(settingsPath)
	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		t.Fatal(err)
	}
	if got := vs.Server.GitHubApp.WebhookSecret; got != "new" {
		t.Errorf("webhook_secret = %q, want new", got)
	}
	if got := vs.Server.NotificationChannels[0].Params["webhook_url"]; got != "https://new" {
		t.Errorf("webhook_url = %q, want https://new", got)
	}
}

// File mode: a masked placeholder with nothing stored is rejected, not
// written.
func TestPutServerConfig_FileMode_MaskedWithoutStoredRejected(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"server":{"oauth":{"web":{"github":{"client_id":"x","client_secret":"********"}}}}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(settingsPath)
		t.Errorf("nothing should be written, got settings.yaml: %s", data)
	}
}

// DB mode: same round trip. Channels live in the notifications section; the
// GitHub App secrets come from settings.yaml (they are not a DB field), so
// sending them back masked must not fail the save.
func TestPutServerConfigDB_MaskedRoundTripKeepsSecrets(t *testing.T) {
	settingsPath := setTempScionHome(t)
	fileSettings := "schema_version: \"1\"\nserver:\n  github_app:\n    app_id: 42\n    webhook_secret: " + rtWebhookSec + "\n"
	if err := os.WriteFile(settingsPath, []byte(fileSettings), 0600); err != nil {
		t.Fatal(err)
	}
	srv, fakeStore, ops := newTestDBServer(t)

	initial := `{"server":{
		"hub":{"admin_emails":["a@example.com"]},
		"notification_channels":[
			{"type":"slack","params":{"webhook_url":"` + rtWebhook + `"}},
			{"type":"slack","params":{"webhook_url":"https://hooks.example.test/second"}}
		]
	}}`
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", initial), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	server := getServerSection(t, rr.Body.Bytes())
	if got := firstChannelParams(t, server)["webhook_url"]; got != maskedValue {
		t.Fatalf("GET should mask webhook_url, got %v", got)
	}
	if got := server["github_app"].(map[string]interface{})["webhook_secret"]; got != maskedValue {
		t.Fatalf("GET should mask github_app webhook_secret, got %v", got)
	}

	// Send back what the admin UI sends in DB mode: the GET's notification
	// channels and github_app block, plus one unrelated edit.
	payload := map[string]interface{}{"server": map[string]interface{}{
		"hub":                   map[string]interface{}{"admin_emails": []string{"b@example.com"}},
		"notification_channels": server["notification_channels"],
		"github_app":            server["github_app"],
	}}
	body, _ := json.Marshal(payload)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(body)), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("round-trip PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	fakeStore.mu.Lock()
	notifRow := fakeStore.settings["notifications"]
	ghRow := fakeStore.settings["github_app"]
	fakeStore.mu.Unlock()
	if strings.Contains(string(notifRow.Value), maskedValue) || strings.Contains(string(ghRow.Value), maskedValue) {
		t.Fatalf("stored section contains the masked placeholder: notifications=%s github_app=%s", notifRow.Value, ghRow.Value)
	}
	var notif opsettings.NotificationsSettings
	if err := json.Unmarshal(notifRow.Value, &notif); err != nil {
		t.Fatal(err)
	}
	if len(notif.NotificationChannels) != 2 {
		t.Fatalf("expected 2 channels, got %v", notif.NotificationChannels)
	}
	if got := notif.NotificationChannels[0].Params["webhook_url"]; got != rtWebhook {
		t.Errorf("channel 0 webhook_url = %q, want the real value", got)
	}
	if got := notif.NotificationChannels[1].Params["webhook_url"]; got != "https://hooks.example.test/second" {
		t.Errorf("channel 1 webhook_url = %q, want its own real value", got)
	}
	if got := ops.Snapshot().AdminEmails; len(got) != 1 || got[0] != "b@example.com" {
		t.Errorf("admin_emails = %v, want the edited value", got)
	}
}

// DB mode: a new channel carrying the placeholder, with no stored channel
// to restore from, is rejected and nothing is written.
func TestPutServerConfigDB_MaskedNewChannelRejected(t *testing.T) {
	setTempScionHome(t)
	srv, fakeStore, ops := newTestDBServer(t)

	body := `{"server":{"notification_channels":[{"type":"slack","params":{"webhook_url":"********"}}]}}`
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()
	if _, ok := fakeStore.settings["notifications"]; ok {
		t.Error("notifications section should not be written")
	}
}

// TestMaskedServerFields_Golden pins the scalar secrets that are masked on
// GET and restored on PUT. Removing an entry from maskedServerFields fails
// here; adding one means adding it here too.
func TestMaskedServerFields_Golden(t *testing.T) {
	want := []string{
		"server.auth.dev_token",
		"server.broker.broker_token",
		"server.database.url",
		"server.github_app.private_key",
		"server.github_app.webhook_secret",
		"server.oauth.cli.github.client_secret",
		"server.oauth.cli.google.client_secret",
		"server.oauth.device.github.client_secret",
		"server.oauth.device.google.client_secret",
		"server.oauth.web.github.client_secret",
		"server.oauth.web.google.client_secret",
		"server.secrets.gcp_credentials",
	}
	got := make([]string, 0, len(maskedServerFields))
	for _, f := range maskedServerFields {
		got = append(got, f.name)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("maskedServerFields names:\n got %v\nwant %v", got, want)
	}
}

// Every masked field names its block, and that block is the struct that
// holds the field (so its siblings are what restoreMaskedServerSecrets
// compares, via jsonEqual against the masked stored block); a
// new entry cannot skip the check.
func TestMaskedServerFields_EveryFieldHasBlock(t *testing.T) {
	// A sibling each block must contain, by JSON key, besides the field.
	siblings := map[string]string{
		"server.auth.dev_token":            "dev_token_file",
		"server.broker.broker_token":       "hub_endpoint",
		"server.database.url":              "driver",
		"server.secrets.gcp_credentials":   "gcp_project_id",
		"server.github_app.private_key":    "app_id",
		"server.github_app.webhook_secret": "app_id",
	}
	for _, f := range maskedServerFields {
		if f.block == nil {
			t.Errorf("%s: no block", f.name)
			continue
		}
		full := fullServerConfig()
		full.Auth.DevTokenFile = "/token"
		full.Broker.HubEndpoint = "https://hub.example.test"
		full.Database.Driver = "sqlite"
		full.Secrets.GCPProjectID = "proj"
		const marker = "field-marker-value"
		*f.get(full) = marker
		blk := f.block(full)
		if blk == nil {
			t.Errorf("%s: block is nil on a fully populated config", f.name)
			continue
		}
		data, err := json.Marshal(blk)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: block is not a JSON object: %s", f.name, data)
		}
		leaf := f.name[strings.LastIndex(f.name, ".")+1:]
		if m[leaf] != marker {
			t.Errorf("%s: block does not hold the field (%s)", f.name, data)
		}
		want := siblings[f.name]
		if strings.HasPrefix(f.name, "server.oauth.") {
			want = "client_id"
		}
		if _, ok := m[want]; !ok {
			t.Errorf("%s: block lacks sibling %q (%s)", f.name, want, data)
		}
		if f.block(&config.V1ServerConfig{}) != nil {
			t.Errorf("%s: block should be nil when its parent is absent", f.name)
		}
	}
}

// A masked field without a block is refused, never restored unchecked.
func TestRestoreMaskedServerSecrets_FieldWithoutBlockFailsClosed(t *testing.T) {
	orig := maskedServerFields
	t.Cleanup(func() { maskedServerFields = orig })
	maskedServerFields = []maskedServerField{{
		name: "server.database.url",
		get: func(s *config.V1ServerConfig) *string {
			if s.Database == nil {
				return nil
			}
			return &s.Database.URL
		},
	}}
	stored := &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: "real"}}
	incoming := &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: maskedValue}}
	if err := restoreMaskedServerSecrets(incoming, stored); err == nil {
		t.Fatal("expected an error for a field without a block")
	}
	if incoming.Database.URL != maskedValue {
		t.Errorf("value was restored without a block check: %q", incoming.Database.URL)
	}
}

// fullServerConfig returns a config where every maskedServerFields accessor
// resolves, with fixed OAuth client IDs and GitHub App identity.
func fullServerConfig() *config.V1ServerConfig {
	prov := func(id string) *config.V1OAuthProviderConfig { return &config.V1OAuthProviderConfig{ClientID: id} }
	client := func(p string) *config.V1OAuthClientConfig {
		return &config.V1OAuthClientConfig{Google: prov(p + "-google"), GitHub: prov(p + "-github")}
	}
	return &config.V1ServerConfig{
		OAuth:     &config.V1OAuthConfig{Web: client("web"), CLI: client("cli"), Device: client("device")},
		Auth:      &config.V1AuthConfig{},
		Broker:    &config.V1BrokerConfig{},
		Database:  &config.V1DatabaseConfig{},
		Secrets:   &config.V1SecretsConfig{},
		GitHubApp: &config.V1GitHubAppConfig{AppID: 42, APIBaseURL: "https://ghe.example.test/api/v3"},
	}
}

func TestRestoreMaskedServerSecrets(t *testing.T) {
	slack := func(url string) config.V1NotificationChannelConfig {
		return config.V1NotificationChannelConfig{Type: "slack", Params: map[string]string{"webhook_url": url}}
	}
	email := func(to string) config.V1NotificationChannelConfig {
		return config.V1NotificationChannelConfig{Type: "email", Params: map[string]string{"to": to}}
	}
	// roundTrip returns what a client sends back after GET of stored.
	roundTrip := func(t *testing.T, stored *config.V1ServerConfig) *config.V1ServerConfig {
		t.Helper()
		resp := &ServerConfigResponse{Server: cloneServerConfig(t, stored)}
		maskSensitiveFields(resp)
		return resp.Server
	}
	wantErr := func(t *testing.T, err error, substr string) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected an error containing %q", substr)
		}
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not contain %q", err, substr)
		}
	}

	t.Run("every masked scalar field round-trips", func(t *testing.T) {
		stored := fullServerConfig()
		for i, f := range maskedServerFields {
			*f.get(stored) = fmt.Sprintf("secret-%d", i)
		}
		incoming := roundTrip(t, stored)
		for _, f := range maskedServerFields {
			if got := *f.get(incoming); got != maskedValue {
				t.Fatalf("%s not masked: %q", f.name, got)
			}
		}
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		for i, f := range maskedServerFields {
			if got, want := *f.get(incoming), fmt.Sprintf("secret-%d", i); got != want {
				t.Errorf("%s = %q, want %q", f.name, got, want)
			}
		}
	})

	t.Run("masked scalar with nothing stored is an error", func(t *testing.T) {
		incoming := &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: maskedValue}}
		wantErr(t, restoreMaskedServerSecrets(incoming, &config.V1ServerConfig{}), "no value is stored")
		wantErr(t, restoreMaskedServerSecrets(incoming, nil), "no value is stored")
	})

	t.Run("stored value that is itself the placeholder is reported as lost", func(t *testing.T) {
		stored := &config.V1ServerConfig{
			Database:             &config.V1DatabaseConfig{URL: maskedValue},
			NotificationChannels: []config.V1NotificationChannelConfig{slack(maskedValue)},
		}
		wantErr(t, restoreMaskedServerSecrets(&config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: maskedValue}}, stored), "lost by an earlier save")
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack(maskedValue)}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "lost by an earlier save")
	})

	t.Run("real value replaces stored", func(t *testing.T) {
		incoming := &config.V1ServerConfig{
			GitHubApp:            &config.V1GitHubAppConfig{PrivateKey: "new"},
			NotificationChannels: []config.V1NotificationChannelConfig{slack("https://new")},
		}
		stored := &config.V1ServerConfig{
			GitHubApp:            &config.V1GitHubAppConfig{PrivateKey: "old"},
			NotificationChannels: []config.V1NotificationChannelConfig{slack("https://old")},
		}
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		if incoming.GitHubApp.PrivateKey != "new" || incoming.NotificationChannels[0].Params["webhook_url"] != "https://new" {
			t.Errorf("real values were overwritten: %+v", incoming)
		}
	})

	t.Run("oauth client_id changed: masked client_secret rejected", func(t *testing.T) {
		stored := fullServerConfig()
		stored.OAuth.Web.GitHub.ClientSecret = "real"
		incoming := roundTrip(t, stored)
		incoming.OAuth.Web.GitHub.ClientID = "other-client"
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "other fields of its block changed")
	})

	t.Run("github_app other field changed: masked secrets rejected", func(t *testing.T) {
		for name, edit := range map[string]func(g *config.V1GitHubAppConfig){
			"app_id":       func(g *config.V1GitHubAppConfig) { g.AppID = 43 },
			"api_base_url": func(g *config.V1GitHubAppConfig) { g.APIBaseURL = "https://api.github.com" },
			// Not an identity allowlist: any other non-masked field counts.
			"installation_url": func(g *config.V1GitHubAppConfig) { g.InstallationURL = "https://github.com/apps/other" },
			"webhooks_enabled": func(g *config.V1GitHubAppConfig) { g.WebhooksEnabled = true },
		} {
			t.Run(name, func(t *testing.T) {
				stored := fullServerConfig()
				stored.GitHubApp.PrivateKey = "pk"
				stored.GitHubApp.WebhookSecret = "ws"
				incoming := roundTrip(t, stored)
				edit(incoming.GitHubApp)
				wantErr(t, restoreMaskedServerSecrets(incoming, stored), "other fields of its block changed")
			})
		}
	})

	t.Run("auth, broker, database, secrets: sibling field changed", func(t *testing.T) {
		cases := []struct {
			field string
			edit  func(s *config.V1ServerConfig)
		}{
			{"server.auth.dev_token", func(s *config.V1ServerConfig) { s.Auth.DevTokenFile = "/other/token" }},
			{"server.broker.broker_token", func(s *config.V1ServerConfig) { s.Broker.Port = 9999 }},
			{"server.database.url", func(s *config.V1ServerConfig) { s.Database.Driver = "postgres" }},
			{"server.secrets.gcp_credentials", func(s *config.V1ServerConfig) { s.Secrets.GCPProjectID = "other-project" }},
		}
		for _, tc := range cases {
			t.Run(tc.field, func(t *testing.T) {
				var f maskedServerField
				for _, e := range maskedServerFields {
					if e.name == tc.field {
						f = e
					}
				}
				if f.get == nil {
					t.Fatalf("%s not in maskedServerFields", tc.field)
				}
				stored := fullServerConfig()
				*f.get(stored) = "real"

				// Unchanged block: restored.
				incoming := roundTrip(t, stored)
				if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
					t.Fatalf("unchanged: %v", err)
				}
				if got := *f.get(incoming); got != "real" {
					t.Errorf("unchanged: %s = %q, want real", tc.field, got)
				}

				// A non-masked sibling changed: rejected.
				incoming = roundTrip(t, stored)
				tc.edit(incoming)
				wantErr(t, restoreMaskedServerSecrets(incoming, stored), "other fields of its block changed")
			})
		}
	})

	t.Run("unedited channel round trip, same types matched by position", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A"), email("E"), slack("B")}}
		incoming := roundTrip(t, stored)
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		got := []string{incoming.NotificationChannels[0].Params["webhook_url"], incoming.NotificationChannels[1].Params["to"], incoming.NotificationChannels[2].Params["webhook_url"]}
		if got[0] != "A" || got[1] != "E" || got[2] != "B" {
			t.Errorf("got %v, want [A E B]", got)
		}
		if stored.NotificationChannels[0].Params["webhook_url"] != "A" {
			t.Error("stored state was modified")
		}
	})

	t.Run("empty channel param round-trips", func(t *testing.T) {
		ch := slack("A")
		ch.Params["channel"] = ""
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{ch}}
		incoming := roundTrip(t, stored)
		if got := incoming.NotificationChannels[0].Params["channel"]; got != "" {
			t.Fatalf("empty param should stay empty on GET, got %q", got)
		}
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		if p := incoming.NotificationChannels[0].Params; p["webhook_url"] != "A" || p["channel"] != "" {
			t.Errorf("params = %v", p)
		}
	})

	t.Run("channel removed: unique type still matched", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A"), email("E")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{email(maskedValue)}}
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		if got := incoming.NotificationChannels[0].Params["to"]; got != "E" {
			t.Errorf("to = %q, want E", got)
		}
	})

	t.Run("reorder of different types matched correctly", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A"), email("E")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{email(maskedValue), slack(maskedValue)}}
		if err := restoreMaskedServerSecrets(incoming, stored); err != nil {
			t.Fatal(err)
		}
		if incoming.NotificationChannels[0].Params["to"] != "E" || incoming.NotificationChannels[1].Params["webhook_url"] != "A" {
			t.Errorf("cross-attached: %+v", incoming.NotificationChannels)
		}
	})

	t.Run("channel removed: ambiguous same type rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A"), slack("B")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack(maskedValue)}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("delete one and add one of the same type rejected", func(t *testing.T) {
		ch := func(url, name string) config.V1NotificationChannelConfig {
			return config.V1NotificationChannelConfig{Type: "slack", Params: map[string]string{"webhook_url": url, "channel": name}}
		}
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{ch("URL-A", "#a"), ch("URL-B", "#b")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{ch(maskedValue, maskedValue), ch("URL-C", "#c")}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("reorder of same-type channels with different filters rejected", func(t *testing.T) {
		withFilter := func(url, f string) config.V1NotificationChannelConfig {
			c := slack(url)
			c.FilterTypes = []string{f}
			return c
		}
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{withFilter("URL-A", "x"), withFilter("URL-B", "y")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{withFilter(maskedValue, "y"), withFilter(maskedValue, "x")}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("non-secret param changed next to a masked one rejected", func(t *testing.T) {
		ch := slack("URL-A")
		ch.Params["channel"] = "#a"
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{ch}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{
			{Type: "slack", Params: map[string]string{"webhook_url": maskedValue, "channel": "#other"}},
		}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("filter changed on a masked channel rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A")}}
		incoming := roundTrip(t, stored)
		incoming.NotificationChannels[0].FilterUrgentOnly = true
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("new channel with placeholder rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{email("E")}}
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{email(maskedValue), slack(maskedValue)}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})

	t.Run("placeholder on a param the stored channel lacks rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{slack("A")}}
		ch := slack(maskedValue)
		ch.Params["token"] = maskedValue
		incoming := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{ch}}
		wantErr(t, restoreMaskedServerSecrets(incoming, stored), "send all params in clear")
	})
}

// DB mode: a masked Layer-0 field is still rejected with 422 layer0_rejected
// (the restore runs after the Layer-0 checks), not with a 400 from restore.
func TestPutServerConfigDB_MaskedLayer0StillRejected422(t *testing.T) {
	setTempScionHome(t)
	srv, _, ops := newTestDBServer(t)
	body := `{"server":{"auth":{"dev_token":"********"}}}`
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "layer0_rejected") {
		t.Fatalf("expected 422 layer0_rejected, got %d: %s", rr.Code, rr.Body.String())
	}
}

func cloneServerConfig(t *testing.T, s *config.V1ServerConfig) *config.V1ServerConfig {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out config.V1ServerConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// fieldBlockPath returns the JSON path of a masked field's block under
// "server", and the field's own key: "server.oauth.web.google.client_secret"
// -> [oauth web google], "client_secret".
func fieldBlockPath(name string) ([]string, string) {
	parts := strings.Split(strings.TrimPrefix(name, "server."), ".")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// jsonObjectAt walks m along path, failing the test when a step is missing.
func jsonObjectAt(t *testing.T, m map[string]any, path []string) map[string]any {
	t.Helper()
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("no JSON object at %v (step %q)", path, p)
		}
		cur = next
	}
	return cur
}

// storedWithAllSecrets is fullServerConfig with every masked field and one
// slack channel set to real values, and every nested struct of each block
// allocated (so the negative matrix reaches nested fields).
func storedWithAllSecrets() *config.V1ServerConfig {
	s := fullServerConfig()
	for i, f := range maskedServerFields {
		*f.get(s) = fmt.Sprintf("real-%d", i)
	}
	s.Auth.DevTokenFile = "/token"
	s.Auth.Proxy = &config.V1ProxyConfig{Provider: "iap"}
	s.Auth.Transport = &config.V1TransportConfig{Mode: "oidc", OIDCAudience: "aud"}
	s.Broker.HubEndpoint = "https://hub.example.test"
	s.Broker.CORS = &config.V1CORSConfig{AllowedOrigins: []string{"https://ui.example.test"}}
	s.Database.Driver = "sqlite"
	s.Secrets.GCPProjectID = "proj"
	s.GitHubApp.InstallationURL = "https://github.com/apps/real"
	for _, f := range maskedServerFields {
		allocNestedStructs(reflect.ValueOf(f.block(s)).Elem())
	}
	s.NotificationChannels = []config.V1NotificationChannelConfig{{
		Type:        "slack",
		Params:      map[string]string{"webhook_url": "https://hooks.example.test/real", "channel": ""},
		FilterTypes: []string{"x"},
	}}
	return s
}

// allocNestedStructs sets every nil pointer-to-struct field of v (a struct)
// to a new zero value, recursively.
func allocNestedStructs(v reflect.Value) {
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct && f.CanSet() {
			if f.IsNil() {
				f.Set(reflect.New(f.Type().Elem()))
			}
			allocNestedStructs(f.Elem())
		}
	}
}

// restoreFromJSON decodes a request server section and restores it. A
// decode error fails the test: every matrix edit is type-valid, so only the
// restore may reject it.
func restoreFromJSON(t *testing.T, serverJSON map[string]any, stored *config.V1ServerConfig) (*config.V1ServerConfig, error) {
	t.Helper()
	data, err := json.Marshal(serverJSON)
	if err != nil {
		t.Fatal(err)
	}
	var in config.V1ServerConfig
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatalf("matrix edit is not type-valid: %v\n%s", err, data)
	}
	return &in, restoreMaskedServerSecrets(&in, stored)
}

// maskedJSON returns what GET shows for s, as a JSON object.
func maskedJSON(t *testing.T, s *config.V1ServerConfig) map[string]any {
	t.Helper()
	shown, err := maskedCopy(s)
	if err != nil {
		t.Fatal(err)
	}
	v, err := toJSONValue(shown)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

// matrixEdit changes one leaf of a JSON object in a type-valid way.
type matrixEdit struct {
	name  string
	apply func(obj map[string]any)
}

// objectAt walks obj along path, creating empty objects for missing steps
// (a nested struct omitted from the JSON because it is empty).
func objectAt(obj map[string]any, path []string) map[string]any {
	cur := obj
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	return cur
}

// leafEdits enumerates, from the struct type, one type-valid edit per leaf
// field (recursing into nested structs): string -> placeholder, []string ->
// [placeholder], map[string]string -> {k: placeholder}, bool or *bool ->
// negated, number -> changed. skip lists top-level keys to leave alone (the
// block's own secrets). An unsupported field kind fails the test, so a new
// field type has to be handled here rather than silently skipped.
func leafEdits(t *testing.T, typ reflect.Type, path []string, skip map[string]bool) []matrixEdit {
	t.Helper()
	var edits []matrixEdit
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if key == "" || key == "-" || (len(path) == 0 && skip[key]) {
			continue
		}
		ft := typ.Field(i).Type
		name := strings.Join(append(append([]string{}, path...), key), ".")
		p := append([]string{}, path...)
		k := key
		if ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct {
			edits = append(edits, leafEdits(t, ft.Elem(), append(p, k), nil)...)
			continue
		}
		base := ft
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		var apply func(obj map[string]any)
		switch {
		case base.Kind() == reflect.String:
			apply = func(obj map[string]any) { objectAt(obj, p)[k] = maskedValue }
		case base.Kind() == reflect.Bool:
			apply = func(obj map[string]any) {
				o := objectAt(obj, p)
				cur, _ := o[k].(bool)
				o[k] = !cur
			}
		case base.Kind() >= reflect.Int && base.Kind() <= reflect.Float64:
			apply = func(obj map[string]any) {
				o := objectAt(obj, p)
				cur, _ := o[k].(float64)
				o[k] = cur + 1
			}
		case base.Kind() == reflect.Slice && base.Elem().Kind() == reflect.String:
			apply = func(obj map[string]any) { objectAt(obj, p)[k] = []any{maskedValue} }
		case base.Kind() == reflect.Map && base.Key().Kind() == reflect.String && base.Elem().Kind() == reflect.String:
			apply = func(obj map[string]any) { objectAt(obj, p)[k] = map[string]any{"k": maskedValue} }
		default:
			t.Fatalf("leafEdits: unsupported field %s of type %s; add an edit for it", name, ft)
		}
		edits = append(edits, matrixEdit{name: name, apply: apply})
	}
	return edits
}

// Negative matrix: with every secret masked as GET shows it, a type-valid
// change to any non-secret leaf of a masked field's block (nested structs
// included), or of the channel, is rejected by the restore with the block
// or channel error. Leaves are enumerated from the struct types; a field of
// a kind leafEdits does not handle fails the test.
func TestRestoreMaskedServerSecrets_PlaceholderInNonSecretFieldMatrix(t *testing.T) {
	stored := storedWithAllSecrets()

	// Unchanged round trip succeeds (the matrix baseline).
	if _, err := restoreFromJSON(t, maskedJSON(t, stored), stored); err != nil {
		t.Fatalf("baseline round trip: %v", err)
	}

	// Secret keys per block path, and one block per path.
	secrets := map[string]map[string]bool{}
	var blockPaths [][]string
	for _, f := range maskedServerFields {
		path, key := fieldBlockPath(f.name)
		id := strings.Join(path, ".")
		if secrets[id] == nil {
			secrets[id] = map[string]bool{}
			blockPaths = append(blockPaths, path)
		}
		secrets[id][key] = true
	}
	var blockFields []maskedServerField
	for _, path := range blockPaths {
		for _, f := range maskedServerFields {
			if p, _ := fieldBlockPath(f.name); strings.Join(p, ".") == strings.Join(path, ".") {
				blockFields = append(blockFields, f)
				break
			}
		}
	}

	cases := 0
	for i, path := range blockPaths {
		f := blockFields[i]
		typ := reflect.TypeOf(f.block(stored)).Elem()
		for _, e := range leafEdits(t, typ, nil, secrets[strings.Join(path, ".")]) {
			cases++
			t.Run(strings.Join(path, ".")+"/"+e.name, func(t *testing.T) {
				in := maskedJSON(t, stored)
				e.apply(jsonObjectAt(t, in, path))
				_, err := restoreFromJSON(t, in, stored)
				if err == nil || !strings.Contains(err.Error(), "other fields of its block changed") {
					t.Fatalf("expected the block-changed error, got %v", err)
				}
			})
		}
	}

	chanEdits := leafEdits(t, reflect.TypeOf(config.V1NotificationChannelConfig{}), nil, nil)
	// Every non-empty param is masked on GET (all params are treated as
	// secrets), so the param cases are one stored empty and one new key.
	chanEdits = append(chanEdits,
		matrixEdit{"params.channel (stored empty)", func(ch map[string]any) { ch["params"].(map[string]any)["channel"] = maskedValue }},
		matrixEdit{"params.extra", func(ch map[string]any) { ch["params"].(map[string]any)["extra"] = maskedValue }},
	)
	for _, e := range chanEdits {
		cases++
		t.Run("notification_channels/"+e.name, func(t *testing.T) {
			in := maskedJSON(t, stored)
			ch := in["notification_channels"].([]any)[0].(map[string]any)
			// The webhook_url stays masked (except where params are replaced
			// wholesale, which keeps a masked value), so a restore is asked.
			e.apply(ch)
			_, err := restoreFromJSON(t, in, stored)
			if err == nil || !strings.Contains(err.Error(), "send all params in clear") {
				t.Fatalf("expected the channel error, got %v", err)
			}
		})
	}
	if cases < 40 {
		t.Errorf("matrix generated only %d cases; leaf enumeration looks broken", cases)
	}
}

func TestRestoreMaskedServerSecrets_Round2(t *testing.T) {
	t.Run("oauth client_id sent as placeholder rejected", func(t *testing.T) {
		stored := fullServerConfig()
		stored.OAuth.Web.Google.ClientSecret = "S"
		in := &config.V1ServerConfig{OAuth: &config.V1OAuthConfig{Web: &config.V1OAuthClientConfig{
			Google: &config.V1OAuthProviderConfig{ClientID: maskedValue, ClientSecret: maskedValue},
		}}}
		if err := restoreMaskedServerSecrets(in, stored); err == nil {
			t.Fatalf("accepted; client_id=%q client_secret=%q", in.OAuth.Web.Google.ClientID, in.OAuth.Web.Google.ClientSecret)
		}
	})

	t.Run("channel type sent as placeholder rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{
			{Type: "slack", Params: map[string]string{"webhook_url": "URL-A"}},
		}}
		in := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{
			{Type: maskedValue, Params: map[string]string{"webhook_url": maskedValue}},
		}}
		if err := restoreMaskedServerSecrets(in, stored); err == nil {
			t.Fatalf("accepted: %+v", in.NotificationChannels)
		}
	})

	t.Run("github_app api_base_url sent as placeholder rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: "pk", APIBaseURL: "https://ghe.example.test"}}
		in := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: maskedValue, APIBaseURL: maskedValue}}
		if err := restoreMaskedServerSecrets(in, stored); err == nil {
			t.Fatalf("accepted: %+v", in.GitHubApp)
		}
	})

	t.Run("one stored channel cannot fill two incoming channels", func(t *testing.T) {
		stored := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{
			{Type: "slack", Params: map[string]string{"webhook_url": "URL-A"}},
		}}
		masked := config.V1NotificationChannelConfig{Type: "slack", Params: map[string]string{"webhook_url": maskedValue}}
		in := &config.V1ServerConfig{NotificationChannels: []config.V1NotificationChannelConfig{masked, masked}}
		if err := restoreMaskedServerSecrets(in, stored); err == nil {
			t.Fatalf("accepted: %+v", in.NotificationChannels)
		}
	})

	t.Run("real webhook_secret next to masked private_key rejected", func(t *testing.T) {
		stored := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: "pk", WebhookSecret: "ws"}}
		in := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: maskedValue, WebhookSecret: "ws-new"}}
		err := restoreMaskedServerSecrets(in, stored)
		if err == nil || !strings.Contains(err.Error(), "other fields of its block changed") {
			t.Fatalf("expected block-changed error, got %v", err)
		}
		if in.GitHubApp.PrivateKey != maskedValue {
			t.Error("private_key restored despite the error")
		}
	})

	t.Run("both github_app secrets masked are restored together", func(t *testing.T) {
		stored := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: "pk", WebhookSecret: "ws"}}
		in := &config.V1ServerConfig{GitHubApp: &config.V1GitHubAppConfig{AppID: 1, PrivateKey: maskedValue, WebhookSecret: maskedValue}}
		if err := restoreMaskedServerSecrets(in, stored); err != nil {
			t.Fatal(err)
		}
		if in.GitHubApp.PrivateKey != "pk" || in.GitHubApp.WebhookSecret != "ws" {
			t.Errorf("got %+v", in.GitHubApp)
		}
	})
}

// fieldSiblingEdit is a non-secret field in each masked field's block, and a
// new value for it, used by the per-field handler tests.
func fieldSiblingEdit(name string) (string, string) {
	if strings.HasPrefix(name, "server.oauth.") {
		return "client_id", "other-client"
	}
	switch name {
	case "server.auth.dev_token":
		return "dev_token_file", "/other/token"
	case "server.broker.broker_token":
		return "hub_endpoint", "https://other.example.test"
	case "server.database.url":
		return "driver", "postgres"
	case "server.secrets.gcp_credentials":
		return "gcp_project_id", "other-project"
	default: // github_app
		return "installation_url", "https://github.com/apps/other"
	}
}

func writeAllSecretsSettings(t *testing.T, path string) *config.V1ServerConfig {
	t.Helper()
	stored := storedWithAllSecrets()
	stored.Hub = &config.V1ServerHubConfig{Port: 9810}
	data, err := yamlv3.Marshal(config.VersionedSettings{SchemaVersion: "1", Server: stored})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return stored
}

// File mode, every masked field: the unedited GET body round-trips all
// secrets; a changed sibling next to a masked secret is rejected with 400
// and settings.yaml is left untouched.
func TestPutServerConfig_FileMode_EveryMaskedField(t *testing.T) {
	settingsPath := setTempScionHome(t)
	srv := &Server{}
	get := func(t *testing.T) map[string]interface{} {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
		}
		return getServerSection(t, rr.Body.Bytes())
	}
	put := func(server map[string]interface{}) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{"server": server})
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(body)))
		return rr
	}

	t.Run("unedited round trip keeps every secret", func(t *testing.T) {
		stored := writeAllSecretsSettings(t, settingsPath)
		if rr := put(get(t)); rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		data, _ := os.ReadFile(settingsPath)
		var vs config.VersionedSettings
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			t.Fatal(err)
		}
		for _, f := range maskedServerFields {
			if got, want := *f.get(vs.Server), *f.get(stored); got != want {
				t.Errorf("%s = %q, want %q", f.name, got, want)
			}
		}
		if got := vs.Server.NotificationChannels[0].Params["webhook_url"]; got != stored.NotificationChannels[0].Params["webhook_url"] {
			t.Errorf("webhook_url = %q", got)
		}
	})

	for _, f := range maskedServerFields {
		t.Run(f.name+"/sibling changed", func(t *testing.T) {
			writeAllSecretsSettings(t, settingsPath)
			before, _ := os.ReadFile(settingsPath)
			server := get(t)
			path, _ := fieldBlockPath(f.name)
			key, val := fieldSiblingEdit(f.name)
			jsonObjectAt(t, server, path)[key] = val
			rr := put(server)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			after, _ := os.ReadFile(settingsPath)
			if string(before) != string(after) {
				t.Error("settings.yaml changed on a rejected save")
			}
		})
	}
}

// DB mode (hosted), every masked field: sending the field's block back
// (unchanged or with a changed sibling) never persists the placeholder.
// Layer-0 blocks: an unchanged echo of the GET view is ignored (200, nothing
// written), a changed sibling is rejected with 422 layer0_rejected before
// any restore. github_app (Layer-1) restores when unchanged and is rejected
// with 400 when a sibling changed.
func TestPutServerConfigDB_EveryMaskedField(t *testing.T) {
	settingsPath := setTempScionHome(t)
	writeAllSecretsSettings(t, settingsPath)
	srv, fakeStore, ops := newTestDBServer(t)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.Bytes()

	for _, f := range maskedServerFields {
		path, _ := fieldBlockPath(f.name)
		top := path[0]
		layer1 := top == "github_app"
		for _, edited := range []bool{false, true} {
			name := f.name + "/unchanged"
			if edited {
				name = f.name + "/sibling changed"
			}
			t.Run(name, func(t *testing.T) {
				server := getServerSection(t, body) // fresh copy per case
				if got := *maskedFieldFromJSON(t, server, f); got != maskedValue {
					t.Fatalf("GET should mask %s, got %q", f.name, got)
				}
				if edited {
					key, val := fieldSiblingEdit(f.name)
					jsonObjectAt(t, server, path)[key] = val
				}
				reqBody, _ := json.Marshal(map[string]interface{}{"server": map[string]interface{}{top: server[top]}})
				rr := httptest.NewRecorder()
				srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(reqBody)), ops)
				want := http.StatusUnprocessableEntity
				if !layer1 && !edited {
					want = http.StatusOK // Layer-0 echo: ignored
				}
				if layer1 {
					want = http.StatusOK
					if edited {
						want = http.StatusBadRequest
					}
				}
				if rr.Code != want {
					t.Fatalf("expected %d, got %d: %s", want, rr.Code, rr.Body.String())
				}
				fakeStore.mu.Lock()
				defer fakeStore.mu.Unlock()
				for sec, row := range fakeStore.settings {
					if strings.Contains(string(row.Value), maskedValue) {
						t.Errorf("section %s stored the placeholder: %s", sec, row.Value)
					}
				}
			})
		}
	}
}

// maskedFieldFromJSON reads a masked field's value from a GET server object.
func maskedFieldFromJSON(t *testing.T, server map[string]interface{}, f maskedServerField) *string {
	t.Helper()
	data, _ := json.Marshal(server)
	var s config.V1ServerConfig
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	p := f.get(&s)
	if p == nil {
		t.Fatalf("%s absent from GET", f.name)
	}
	return p
}
