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
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// storedServerSettings is a settings.yaml whose server sections hold
// credential and connection fields that the admin UI does not send back
// on save. Values are placeholders.
const storedServerSettings = `schema_version: "1"
server:
  log_level: info
  hub:
    port: 9810
    host: 0.0.0.0
  github_app:
    app_id: 12345
    private_key: test-private-key
    private_key_path: /keys/app.pem
    webhook_secret: test-webhook-value
    installation_url: https://github.com/apps/test-app
  database:
    driver: postgres
    url: postgres://user:test-pass@db/scion
    max_open_conns: 7
  auth:
    dev_mode: true
    dev_token: test-dev-credential
    dev_token_file: /tokens/dev
  broker:
    enabled: true
    broker_token: test-broker-credential
    broker_id: broker-1
  secrets:
    backend: gcpsm
    gcp_project_id: test-project
    gcp_credentials: /creds/sa.json
`

// putFileModeServerConfig writes stored as settings.yaml under a temp HOME,
// issues a file-mode PUT with body and returns the recorder and the
// settings.yaml path.
func putFileModeServerConfig(t *testing.T, stored, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(stored), 0644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr, settingsPath
}

// readServerSection returns server.<section> of the settings file.
func readServerSection(t *testing.T, settingsPath, section string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	srv, _ := raw["server"].(map[string]interface{})
	if section == "" {
		return srv
	}
	m, _ := srv[section].(map[string]interface{})
	return m
}

// Each server section is deep-merged: a save that leaves a field out (as
// the admin UI does for credentials and fields it does not show) keeps the
// stored value, while the fields it sends are updated.
func TestHandlePutServerConfig_SaveKeepsOmittedServerFields(t *testing.T) {
	cases := []struct {
		name    string
		section string
		body    string
		updated map[string]interface{}
		kept    map[string]interface{}
	}{
		{
			name:    "github_app",
			section: "github_app",
			// The UI's copy of github_app after loading the GitHub App
			// config: no credential fields.
			body:    `{"server":{"github_app":{"app_id":12345,"webhooks_enabled":true,"installation_url":"https://github.com/apps/test-app"}}}`,
			updated: map[string]interface{}{"webhooks_enabled": true},
			kept: map[string]interface{}{
				"private_key":      "test-private-key",
				"private_key_path": "/keys/app.pem",
				"webhook_secret":   "test-webhook-value",
			},
		},
		{
			name:    "database",
			section: "database",
			body:    `{"server":{"database":{"driver":"sqlite"}}}`,
			updated: map[string]interface{}{"driver": "sqlite"},
			kept: map[string]interface{}{
				"url":            "postgres://user:test-pass@db/scion",
				"max_open_conns": 7,
			},
		},
		{
			name:    "auth",
			section: "auth",
			body:    `{"server":{"auth":{"dev_mode":true,"user_access_mode":"open"}}}`,
			updated: map[string]interface{}{"user_access_mode": "open"},
			kept: map[string]interface{}{
				"dev_token":      "test-dev-credential",
				"dev_token_file": "/tokens/dev",
			},
		},
		{
			name:    "broker",
			section: "broker",
			body:    `{"server":{"broker":{"enabled":true,"host":"127.0.0.1"}}}`,
			updated: map[string]interface{}{"host": "127.0.0.1"},
			kept: map[string]interface{}{
				"broker_token": "test-broker-credential",
				"broker_id":    "broker-1",
			},
		},
		{
			name:    "secrets",
			section: "secrets",
			body:    `{"server":{"secrets":{"backend":"gcpsm","gcp_project_id":"other-project"}}}`,
			updated: map[string]interface{}{"gcp_project_id": "other-project"},
			kept:    map[string]interface{}{"gcp_credentials": "/creds/sa.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}
			got := readServerSection(t, settingsPath, tc.section)
			for k, want := range tc.updated {
				if got[k] != want {
					t.Errorf("server.%s.%s = %v, want updated value %v", tc.section, k, got[k], want)
				}
			}
			for k, want := range tc.kept {
				if got[k] != want {
					t.Errorf("server.%s.%s = %v, want kept value %v", tc.section, k, got[k], want)
				}
			}
			// Sections the request did not touch are kept as well.
			if tok := readServerSection(t, settingsPath, "auth")["dev_token"]; tok != "test-dev-credential" {
				t.Errorf("server.auth.dev_token = %v after saving another section", tok)
			}
		})
	}
}

// A field is cleared by sending it explicitly as "" / 0 / false / [] or as
// null; null also removes a whole section.
func TestHandlePutServerConfig_ExplicitEmptyOrNullClearsServerField(t *testing.T) {
	body := `{"server":{
		"log_level":"",
		"hub":{"port":0,"host":null},
		"database":{"url":""},
		"broker":{"broker_token":null},
		"secrets":{"gcp_credentials":"","gcp_replication_locations":[]},
		"github_app":null
	}}`
	rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	srv := readServerSection(t, settingsPath, "")
	if _, ok := srv["log_level"]; ok {
		t.Errorf("server.log_level not cleared by \"\": %v", srv["log_level"])
	}
	if _, ok := srv["github_app"]; ok {
		t.Errorf("server.github_app not removed by null: %v", srv["github_app"])
	}
	checks := []struct {
		section, key string
	}{
		{"hub", "port"},
		{"hub", "host"},
		{"database", "url"},
		{"broker", "broker_token"},
		{"secrets", "gcp_credentials"},
	}
	for _, c := range checks {
		if v, ok := readServerSection(t, settingsPath, c.section)[c.key]; ok {
			t.Errorf("server.%s.%s not cleared: %v", c.section, c.key, v)
		}
	}
	// Fields next to the cleared ones are kept.
	if d := readServerSection(t, settingsPath, "database")["driver"]; d != "postgres" {
		t.Errorf("server.database.driver = %v, want kept postgres", d)
	}
	if id := readServerSection(t, settingsPath, "broker")["broker_id"]; id != "broker-1" {
		t.Errorf("server.broker.broker_id = %v, want kept broker-1", id)
	}
}

// Masked placeholders sent back from GET keep the stored value (the
// existing restore), also when other sections of the save are partial.
func TestHandlePutServerConfig_MaskedPlaceholderKeepsStoredValue(t *testing.T) {
	body := `{"server":{
		"github_app":{"app_id":12345,"private_key":"********","private_key_path":"/keys/app.pem","webhook_secret":"********","installation_url":"https://github.com/apps/test-app"},
		"database":{"driver":"postgres","url":"********","max_open_conns":7},
		"broker":{"enabled":true}
	}}`
	rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	gh := readServerSection(t, settingsPath, "github_app")
	if gh["private_key"] != "test-private-key" || gh["webhook_secret"] != "test-webhook-value" {
		t.Errorf("github_app credentials not restored: %v", gh)
	}
	if u := readServerSection(t, settingsPath, "database")["url"]; u != "postgres://user:test-pass@db/scion" {
		t.Errorf("server.database.url = %v, want stored value", u)
	}
	if tok := readServerSection(t, settingsPath, "broker")["broker_token"]; tok != "test-broker-credential" {
		t.Errorf("server.broker.broker_token = %v, want stored value", tok)
	}
	data, _ := os.ReadFile(settingsPath)
	if bytes.Contains(data, []byte("********")) {
		t.Errorf("masked placeholder written to settings.yaml:\n%s", data)
	}
}

// The strict PUT still holds with the deep merge: an unknown key inside a
// merged server section is a 422 and nothing is written.
func TestHandlePutServerConfig_DeepMergeStillRejectsUnknownKeys(t *testing.T) {
	for _, body := range []string{
		`{"server":{"github_app":{"app_id":1,"bogus":"x"}}}`,
		`{"server":{"database":{"driver":"sqlite","url_typo":""}}}`,
	} {
		rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT %s: status %d, want 422: %s", body, rr.Code, rr.Body.String())
		}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != storedServerSettings {
			t.Errorf("settings.yaml changed by a rejected PUT:\n%s", data)
		}
	}
}

// Sections whose validity depends on more than one field are validated
// again as merged with the stored fields.
func TestValidateMergedServerSections_ChecksMergedResult(t *testing.T) {
	raw := map[string]interface{}{
		"server": map[string]interface{}{
			"home_storage": map[string]interface{}{"backend": "local", "leaf": "bogus"},
		},
	}
	if err := validateMergedServerSections(raw, []byte(`{"home_storage":{"backend":"local"}}`)); err == nil {
		t.Error("expected an error for an invalid merged home_storage")
	}
	// A section the request did not send is not checked.
	if err := validateMergedServerSections(raw, []byte(`{"log_level":"info"}`)); err != nil {
		t.Errorf("unsent section checked: %v", err)
	}
}
