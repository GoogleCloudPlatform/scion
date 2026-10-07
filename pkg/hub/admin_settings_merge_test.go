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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// encoding/json and the strict unknown-key check both match field names
// case-insensitively, so a miscased key decodes into the request. The
// merge must find the same field and write it under its yaml name;
// otherwise the PUT returns 200 and writes nothing.
func TestHandlePutServerConfig_MiscasedKeyWrittenUnderCanonicalName(t *testing.T) {
	body := `{"server":{"Log_Level":"debug","github_app":{"Webhooks_Enabled":true}}}`
	rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	srv := readServerSection(t, settingsPath, "")
	if srv["log_level"] != "debug" {
		t.Errorf("server.log_level = %v, want debug", srv["log_level"])
	}
	if _, ok := srv["Log_Level"]; ok {
		t.Errorf("miscased key written as-is: %v", srv)
	}
	gh := readServerSection(t, settingsPath, "github_app")
	if gh["webhooks_enabled"] != true {
		t.Errorf("server.github_app.webhooks_enabled = %v, want true", gh["webhooks_enabled"])
	}
	if _, ok := gh["Webhooks_Enabled"]; ok {
		t.Errorf("miscased key written as-is: %v", gh)
	}
	if gh["private_key"] != "test-private-key" {
		t.Errorf("server.github_app.private_key = %v, want kept value", gh["private_key"])
	}
}

// An explicit false on a pointer bool is a setting of its own (nil means
// "no preference"), so it is written as false rather than deleted.
func TestHandlePutServerConfig_ExplicitFalsePointerBoolWritten(t *testing.T) {
	stored := storedServerSettings + `  native_chat:
    enabled: true
`
	stored = strings.Replace(stored, "    host: 0.0.0.0\n", "    host: 0.0.0.0\n    soft_delete_retain_files: true\n", 1)
	body := `{"server":{"native_chat":{"enabled":false},"hub":{"soft_delete_retain_files":false}}}`
	rr, settingsPath := putFileModeServerConfig(t, stored, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	if v, ok := readServerSection(t, settingsPath, "native_chat")["enabled"]; !ok || v != false {
		t.Errorf("server.native_chat.enabled = %v (present %v), want false", v, ok)
	}
	hub := readServerSection(t, settingsPath, "hub")
	if v, ok := hub["soft_delete_retain_files"]; !ok || v != false {
		t.Errorf("server.hub.soft_delete_retain_files = %v (present %v), want false", v, ok)
	}
	if hub["port"] != 9810 {
		t.Errorf("server.hub.port = %v, want kept 9810", hub["port"])
	}
}

// Sections nested two levels deep are merged field by field too.
func TestHandlePutServerConfig_DepthTwoPartialUpdateKeepsSiblings(t *testing.T) {
	stored := strings.Replace(storedServerSettings, "    host: 0.0.0.0\n", `    host: 0.0.0.0
    cors:
      enabled: true
      allowed_origins:
        - https://example.com
      max_age: 600
`, 1)
	stored = strings.Replace(stored, "    dev_token_file: /tokens/dev\n", `    dev_token_file: /tokens/dev
    proxy:
      provider: iap
      iap:
        audience: test-audience
        issuer: https://issuer.example.com
`, 1)
	body := `{"server":{"hub":{"cors":{"max_age":120}},"auth":{"proxy":{"iap":{"issuer":"https://other.example.com"}}}}}`
	rr, settingsPath := putFileModeServerConfig(t, stored, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	hub := readServerSection(t, settingsPath, "hub")
	cors, _ := hub["cors"].(map[string]interface{})
	if cors["max_age"] != 120 {
		t.Errorf("server.hub.cors.max_age = %v, want 120", cors["max_age"])
	}
	if cors["enabled"] != true {
		t.Errorf("server.hub.cors.enabled = %v, want kept true", cors["enabled"])
	}
	if origins, _ := cors["allowed_origins"].([]interface{}); len(origins) != 1 || origins[0] != "https://example.com" {
		t.Errorf("server.hub.cors.allowed_origins = %v, want kept", cors["allowed_origins"])
	}
	if hub["port"] != 9810 {
		t.Errorf("server.hub.port = %v, want kept 9810", hub["port"])
	}
	auth := readServerSection(t, settingsPath, "auth")
	proxy, _ := auth["proxy"].(map[string]interface{})
	iap, _ := proxy["iap"].(map[string]interface{})
	if iap["issuer"] != "https://other.example.com" {
		t.Errorf("server.auth.proxy.iap.issuer = %v, want updated", iap["issuer"])
	}
	if iap["audience"] != "test-audience" {
		t.Errorf("server.auth.proxy.iap.audience = %v, want kept", iap["audience"])
	}
	if proxy["provider"] != "iap" {
		t.Errorf("server.auth.proxy.provider = %v, want kept iap", proxy["provider"])
	}
	if auth["dev_token"] != "test-dev-credential" {
		t.Errorf("server.auth.dev_token = %v, want kept", auth["dev_token"])
	}
}

// When the merged result fails validateMergedServerSections, the PUT is a
// 400 and settings.yaml is left byte-identical.
func TestHandlePutServerConfig_MergedValidationRejectsWithoutWriting(t *testing.T) {
	stored := storedServerSettings + `  home_storage:
    backend: local
    leaf: bogus
`
	rr, settingsPath := putFileModeServerConfig(t, stored, `{"server":{"home_storage":{"backend":"local"}}}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT: status %d, want 400: %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stored {
		t.Errorf("settings.yaml changed by a rejected PUT:\n%s", data)
	}
}

// validateMergedServerSections selects the sent sections with the same
// key matching as the merge, so a section sent under a miscased key is
// validated on the merged result.
func TestHandlePutServerConfig_MiscasedHomeStorageValidatedAsMerged(t *testing.T) {
	stored := storedServerSettings + `  home_storage:
    backend: local
    leaf: bogus
`
	rr, settingsPath := putFileModeServerConfig(t, stored, `{"server":{"Home_Storage":{"backend":"local"}}}`)
	if rr.Code == http.StatusOK {
		t.Fatalf("PUT: status 200, want an error: %s", rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stored {
		t.Errorf("settings.yaml changed by a rejected PUT:\n%s", data)
	}
}

// The sent nfs block is complete on its own; the merge keeps the stored
// subpath_root, which is invalid, so only the merged result fails.
func TestHandlePutServerConfig_MiscasedSharedDirStorageValidatedAsMerged(t *testing.T) {
	stored := storedServerSettings + `  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/shared
      subpath_root: /abs
      shares:
        - id: s1
runtimes:
  docker:
    type: docker
    shared_dir_storage_backend: nfs
`
	body := `{"server":{"Shared_Dir_Storage":{"backend":"nfs","nfs":{"mount_root":"/mnt/other","shares":[{"id":"s2"}]}}}}`
	rr, settingsPath := putFileModeServerConfig(t, stored, body)
	if rr.Code == http.StatusOK {
		t.Fatalf("PUT: status 200, want an error: %s", rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stored {
		t.Errorf("settings.yaml changed by a rejected PUT:\n%s", data)
	}
}

// When two sent keys fold to the same field and one value is null, the
// later value replaces the earlier one, as in encoding/json, on every run.
func TestMergeServerSettings_DuplicateFoldedKeysLastWins(t *testing.T) {
	cases := []struct {
		body     string
		wantKept bool
	}{
		{`{"github_app":null,"GitHub_App":{"app_id":99}}`, true},
		{`{"GitHub_App":{"app_id":99},"github_app":null}`, false},
	}
	for _, tc := range cases {
		for i := 0; i < 50; i++ {
			var incoming config.V1ServerConfig
			if err := json.Unmarshal([]byte(tc.body), &incoming); err != nil {
				t.Fatal(err)
			}
			raw := map[string]interface{}{
				"server": map[string]interface{}{
					"github_app": map[string]interface{}{"app_id": 1, "private_key": "test-private-key"},
				},
			}
			mergeServerSettings(raw, &incoming, json.RawMessage(tc.body))
			srv := raw["server"].(map[string]interface{})
			gh, ok := srv["github_app"].(map[string]interface{})
			if ok != tc.wantKept {
				t.Fatalf("%s (run %d): github_app present = %v, want %v", tc.body, i, ok, tc.wantKept)
			}
			if !tc.wantKept {
				continue
			}
			if fmt.Sprint(gh["app_id"]) != "99" {
				t.Fatalf("%s (run %d): app_id = %v, want 99", tc.body, i, gh["app_id"])
			}
			if gh["private_key"] != "test-private-key" {
				t.Fatalf("%s (run %d): private_key = %v, want kept", tc.body, i, gh["private_key"])
			}
		}
	}
}

// The request decode matches the top-level "server" key
// case-insensitively, so rawServerObject resolves it with the same rule.
// A partial home_storage sent under "Server" is validated on the merged
// result, and a failing result leaves settings.yaml byte-identical.
func TestHandlePutServerConfig_MixedCaseServerKeyValidatedAsMerged(t *testing.T) {
	stored := storedServerSettings + `  home_storage:
    backend: local
    leaf: bogus
`
	for _, key := range []string{"Server", "SERVER"} {
		t.Run(key, func(t *testing.T) {
			body := fmt.Sprintf(`{%q:{"home_storage":{"backend":"local"}}}`, key)
			rr, settingsPath := putFileModeServerConfig(t, stored, body)
			if rr.Code == http.StatusOK {
				t.Fatalf("PUT: status 200, want an error: %s", rr.Body.String())
			}
			data, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != stored {
				t.Errorf("settings.yaml changed by a rejected PUT:\n%s", data)
			}
		})
	}
}

// A valid partial update sent under a mixed-case top-level server key
// merges exactly like the lower-case key: sent fields are updated,
// explicit zero values clear their field, and siblings are kept.
func TestHandlePutServerConfig_MixedCaseServerKeyMergesLikeLowerCase(t *testing.T) {
	const update = `{"log_level":"debug","github_app":{"installation_url":""},"database":{"max_open_conns":3}}`
	read := func(t *testing.T, key string) []byte {
		t.Helper()
		rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, fmt.Sprintf(`{%q:%s}`, key, update))
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", key, rr.Code, rr.Body.String())
		}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	want := read(t, "server")
	for _, key := range []string{"Server", "SERVER"} {
		t.Run(key, func(t *testing.T) {
			got := read(t, key)
			if !bytes.Equal(got, want) {
				t.Errorf("settings.yaml for %q differs from \"server\":\n got:\n%s\nwant:\n%s", key, got, want)
			}
			settingsPath := filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml")
			gh := readServerSection(t, settingsPath, "github_app")
			if _, ok := gh["installation_url"]; ok {
				t.Errorf("server.github_app.installation_url = %v, want cleared", gh["installation_url"])
			}
			if gh["private_key"] != "test-private-key" {
				t.Errorf("server.github_app.private_key = %v, want kept", gh["private_key"])
			}
			db := readServerSection(t, settingsPath, "database")
			if db["max_open_conns"] != 3 {
				t.Errorf("server.database.max_open_conns = %v, want 3", db["max_open_conns"])
			}
			if db["url"] != "postgres://user:test-pass@db/scion" {
				t.Errorf("server.database.url = %v, want kept", db["url"])
			}
		})
	}
}

// Object values sent under keys that fold to the same field are applied
// in body order, as encoding/json decodes them into the request: the
// members of both objects are written. This holds for sections and for
// the top-level server key.
func TestHandlePutServerConfig_DuplicateFoldedObjectsMergedInBodyOrder(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"section", `{"server":{"github_app":{"app_id":5},"GitHub_App":{"installation_url":"x"}}}`},
		{"server", `{"server":{"github_app":{"app_id":5}},"Server":{"github_app":{"installation_url":"x"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, settingsPath := putFileModeServerConfig(t, storedServerSettings, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}
			gh := readServerSection(t, settingsPath, "github_app")
			if gh["app_id"] != 5 {
				t.Errorf("server.github_app.app_id = %v, want 5", gh["app_id"])
			}
			if gh["installation_url"] != "x" {
				t.Errorf("server.github_app.installation_url = %v, want x", gh["installation_url"])
			}
			if gh["private_key"] != "test-private-key" {
				t.Errorf("server.github_app.private_key = %v, want kept", gh["private_key"])
			}
		})
	}
}
