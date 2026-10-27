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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// envSecretSentinel builds a unique secret-looking value per key so a test
// can search the response body for each one.
func envSecretSentinel(key string) string {
	return "SENTINEL-" + strings.NewReplacer(".", "-", "_", "-").Replace(key) + "-raw"
}

// secretEnvKeys are environment-set keys covering every masked or hidden
// field class of the server-config GET: each maskedServerFields entry, the
// telemetry cloud headers, notification channel params, the never-shown
// server keys (relay-peer secret, test identities, proxy driver settings,
// hub class, the server keyring file and its world-readable override), the
// map sections whose values are withheld, and a key that matches no
// settings field. The values are strings unless noted.
func secretEnvKeys() map[string]any {
	keys := map[string]any{}
	for _, f := range maskedServerFields {
		keys[f.name] = envSecretSentinel(f.name)
	}
	for _, k := range []string{
		"telemetry.cloud.headers.authorization",
		"server.hub.conduit.peer_secret",
		"server.hub.test_identities.admin_web_sessions",
		"server.hub.class",
		"server.auth.proxy.driver_token_audience",
		"profiles.local.env.API_KEY",
		"runtimes.docker.env.SOME_VALUE",
		"harness_configs.claude.env.ANTHROPIC_API_KEY",
		"hub.token",
		"server.auth.devtokn",
		"server.keyring_file",
	} {
		keys[k] = envSecretSentinel(k)
	}
	keys["server.keyring_file_allow_world_readable"] = true
	keys["server.auth.proxy.driver_principals"] = []any{envSecretSentinel("server.auth.proxy.driver_principals")}
	keys["server.notification_channels"] = []any{map[string]any{
		"type":   "webhook",
		"params": map[string]any{"url": envSecretSentinel("server.notification_channels")},
	}}
	return keys
}

func isWithheldEnvValueRoot(key string) bool {
	for _, root := range withheldEnvValueRoots {
		if strings.HasPrefix(key, root) {
			return true
		}
	}
	return false
}

// sentinelsIn returns every sentinel of keys found in body.
func sentinelsIn(body string, keys map[string]any) []string {
	var found []string
	for k := range keys {
		if s := envSecretSentinel(k); strings.Contains(body, s) {
			found = append(found, k)
		}
	}
	return found
}

// TestViewMaskedEnvValue_EveryMaskedFieldClass proves the GET masking alone
// (without the key-name refusal) masks every secret field class.
func TestViewMaskedEnvValue_EveryMaskedFieldClass(t *testing.T) {
	keys := secretEnvKeys()
	envK := newEnvKoanf(t, keys)
	for key := range keys {
		if isWithheldEnvValueRoot(key) {
			// The GET view shows these maps unmasked; envValueMayBeShown
			// refuses them (TestEnvValueMayBeShown).
			continue
		}
		v, masked := viewMaskedEnvValue(envK, key)
		if !masked || v != maskedValue {
			t.Errorf("%s: viewMaskedEnvValue = (%v, %v), want (%q, true)", key, v, masked, maskedValue)
		}
	}
}

func TestShownEnvValue_PlainAndZeroValues(t *testing.T) {
	envK := newEnvKoanf(t, map[string]any{
		"server.auth.authorized_domains":  []any{"env-a.example", "env-b.example"},
		"server.database.driver":          "postgres",
		"server.hub.auto_suspend_stalled": false,
	})
	cases := map[string]any{
		"server.auth.authorized_domains":  []any{"env-a.example", "env-b.example"},
		"server.database.driver":          "postgres",
		"server.hub.auto_suspend_stalled": false,
	}
	for key, want := range cases {
		v, masked := shownEnvValue(envK, key)
		if masked || !reflect.DeepEqual(v, want) {
			t.Errorf("%s: shownEnvValue = (%#v, %v), want (%#v, false)", key, v, masked, want)
		}
	}
}

func TestEnvValueMayBeShown(t *testing.T) {
	for _, k := range []string{"server.auth.authorized_domains", "server.database.driver", "telemetry.enabled"} {
		if !envValueMayBeShown(k) {
			t.Errorf("envValueMayBeShown(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"profiles.local.runtime", "runtimes.docker.host", "harness_configs.x.image", "server.auth.dev_token", "server.secrets.gcp_credentials", "telemetry.cloud.headers.x"} {
		if envValueMayBeShown(k) {
			t.Errorf("envValueMayBeShown(%q) = true, want false", k)
		}
	}
}

// TestGetServerConfigDB_EnvValues covers the DB tier: the authorized domains
// the hub runs with come from the environment (the saved value is never
// applied, ptone/scion#6000), so the env value is in effect and the key is
// not reported as superseded; a key whose managed row wins is not in effect;
// a key with no row is in effect.
func TestGetServerConfigDB_EnvValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no settings.yaml
	envDomains := []any{"env-a.example", "env-b.example"}
	fakeStore := newFakeHubSettingStore()
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["db@example.com"],"authorized_domains":["saved.example"]}`), "managed")
	envK := newEnvKoanf(t, map[string]any{
		"server.auth.authorized_domains": envDomains,
		"server.hub.admin_emails":        []any{"env@example.com"},
		"telemetry.enabled":              true,
		"server.database.driver":         "postgres",
	})
	// The bootstrap merge has SCION_SERVER_* as its top layer.
	bootK := newFileKoanf(t, map[string]any{
		"server.auth.authorized_domains": envDomains,
		"server.hub.admin_emails":        []any{"env@example.com"},
		"telemetry.enabled":              true,
	})
	ops := NewOperationalSettings(fakeStore, bootK, envK)
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	// As at startup: the running list is the bootstrap value.
	srv.config.AuthorizedDomains = []string{"env-a.example", "env-b.example"}
	srv.SetOperationalSettings(ops)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// env_overrides is unchanged.
	for _, k := range []string{"server.auth.authorized_domains", "server.hub.admin_emails", "telemetry.enabled", "server.database.driver"} {
		found := false
		for _, o := range resp.EnvOverrides {
			found = found || o == k
		}
		if !found {
			t.Errorf("env_overrides lacks %s: %v", k, resp.EnvOverrides)
		}
	}
	if len(resp.EnvValues) != len(resp.EnvOverrides) {
		t.Errorf("env_values has %d entries, env_overrides %d", len(resp.EnvValues), len(resp.EnvOverrides))
	}

	want := map[string]EnvKeyValue{
		"server.auth.authorized_domains": {Value: envDomains, InEffect: boolPtr(true), SavedDiffers: true},
		"server.hub.admin_emails":        {Value: []any{"env@example.com"}, InEffect: boolPtr(false), SavedDiffers: true},
		"telemetry.enabled":              {Value: true, InEffect: boolPtr(true)},
		"server.database.driver":         {Value: "postgres", InEffect: boolPtr(true)},
	}
	for k, w := range want {
		got, ok := resp.EnvValues[k]
		if !ok {
			t.Errorf("env_values lacks %s", k)
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("env_values[%s] = %#v, want %#v", k, got, w)
		}
	}

	// The saved authorized domains supersede nothing; admin_emails does.
	var supersededAccess []string
	for _, sk := range resp.SupersededKeys["access"] {
		supersededAccess = append(supersededAccess, sk.Key)
	}
	for _, k := range supersededAccess {
		if k == "server.auth.authorized_domains" {
			t.Errorf("authorized_domains reported superseded: %v", supersededAccess)
		}
	}
	if !strings.Contains(strings.Join(supersededAccess, ","), "server.hub.admin_emails") {
		t.Errorf("admin_emails not reported superseded: %v", supersededAccess)
	}
	// The GET body still shows the saved value in the form field.
	if resp.Server == nil || resp.Server.Auth == nil || !reflect.DeepEqual(resp.Server.Auth.AuthorizedDomains, []string{"saved.example"}) {
		t.Errorf("server.auth.authorized_domains in body changed: %+v", resp.Server)
	}
}

// TestGetServerConfigDB_EnvValuesNeverRawSecrets proves no secret env value
// appears raw anywhere in the DB-tier GET body, for every masked field class.
func TestGetServerConfigDB_EnvValuesNeverRawSecrets(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no settings.yaml
	keys := secretEnvKeys()
	envK := newEnvKoanf(t, keys)
	ops := NewOperationalSettings(newFakeHubSettingStore(), emptyKoanf(), envK)
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if found := sentinelsIn(rr.Body.String(), keys); len(found) > 0 {
		t.Fatalf("raw secret env values in the GET body for %v", found)
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertEnvValuesMatchOverrides(t, resp.EnvOverrides, resp.EnvValues, keys)
}

// assertEnvValuesMatchOverrides checks, for a GET body: the env_values keys
// are exactly the env_overrides keys; no never-API-writable key (nor one
// under it) is in either list, so the GET does not reveal which are set;
// every other key of keys is listed and masked; and the keyring file path
// appears nowhere.
func assertEnvValuesMatchOverrides(t *testing.T, overrides []string, values map[string]EnvKeyValue, keys map[string]any) {
	t.Helper()
	got := make([]string, 0, len(values))
	for k := range values {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), overrides...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env_values keys %v != env_overrides %v", got, want)
	}
	for k := range keys {
		neverWritable := len(withoutNeverAPIWritableKeys([]string{k})) == 0
		_, inValues := values[k]
		inOverrides := false
		for _, o := range overrides {
			inOverrides = inOverrides || o == k
		}
		if neverWritable {
			if inValues || inOverrides {
				t.Errorf("never-API-writable key %s listed (env_values %v, env_overrides %v)", k, inValues, inOverrides)
			}
			continue
		}
		ev, ok := values[k]
		if !ok {
			t.Errorf("env_values lacks %s", k)
			continue
		}
		if !reflect.DeepEqual(ev, maskedEnvKeyValue()) {
			t.Errorf("env_values[%s] = %#v, want the masked placeholder with neither bit", k, ev)
		}
	}
	for _, nw := range []string{config.KeyringFileKey, config.KeyringFileAllowWorldReadableKey, config.HubClassKey, config.ConduitPeerSecretKey} {
		if _, ok := values[nw]; ok {
			t.Errorf("env_values lists never-API-writable key %s", nw)
		}
	}
}

// writeFileTierSettings writes settings.yaml under a fresh HOME.
func writeFileTierSettings(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileTierGET(t *testing.T) (*httptest.ResponseRecorder, ServerConfigResponse) {
	t.Helper()
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return rr, resp
}

// TestFileMode_EnvValues: in the file tier the environment value is in
// effect (the loader layers SCION_SERVER_* above settings.yaml; see
// config.TestLoadGlobalConfig_ServerEnvBeatsSettingsFile), and the body
// still shows the settings file value in the form field.
func TestFileMode_EnvValues(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  auth:\n    authorized_domains:\n      - saved.example\n")
	t.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "env-a.example,env-b.example")
	t.Setenv("SCION_SERVER_AUTH_DEVTOKEN", envSecretSentinel("server.auth.dev_token"))
	t.Setenv("SCION_SERVER_DATABASE_URL", envSecretSentinel("server.database.url"))
	t.Setenv("SCION_SERVER_KEYRING_FILE", envSecretSentinel("server.keyring_file"))

	rr, resp := fileTierGET(t)
	if strings.Contains(rr.Body.String(), "SENTINEL-") {
		t.Fatalf("raw secret env value in the file-mode GET body: %s", rr.Body.String())
	}
	if got := resp.EnvValues["server.auth.authorized_domains"]; !reflect.DeepEqual(got, EnvKeyValue{Value: []any{"env-a.example", "env-b.example"}, InEffect: boolPtr(true), SavedDiffers: true}) {
		t.Errorf("env_values[server.auth.authorized_domains] = %#v", got)
	}
	// The keyring file path is never-API-writable: not listed at all.
	if _, ok := resp.EnvValues[config.KeyringFileKey]; ok {
		t.Errorf("env_values lists %s", config.KeyringFileKey)
	}
	for _, o := range resp.EnvOverrides {
		if o == config.KeyringFileKey {
			t.Errorf("env_overrides lists %s", config.KeyringFileKey)
		}
	}
	for _, k := range []string{"server.auth.dev_token", "server.database.url"} {
		if got := resp.EnvValues[k]; !reflect.DeepEqual(got, maskedEnvKeyValue()) {
			t.Errorf("env_values[%s] = %#v, want the masked placeholder with neither bit", k, got)
		}
	}
	if resp.Server == nil || resp.Server.Auth == nil || !reflect.DeepEqual(resp.Server.Auth.AuthorizedDomains, []string{"saved.example"}) {
		t.Errorf("server.auth.authorized_domains in body changed: %+v", resp.Server)
	}
}

// TestFileMode_EnvValuesNoSettingsFile: the no-file branch reports
// env_values too.
func TestFileMode_EnvValuesNoSettingsFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "env-a.example")
	_, resp := fileTierGET(t)
	if got := resp.EnvValues["server.auth.authorized_domains"]; !reflect.DeepEqual(got, EnvKeyValue{Value: []any{"env-a.example"}, InEffect: boolPtr(true)}) {
		t.Errorf("env_values[server.auth.authorized_domains] = %#v", got)
	}
}

// dbEnvValuesGET runs the DB-tier GET on a server built from rows (section
// name to origin and JSON document), the bootstrap merge boot and the env
// koanf env, with configure applied to the server first, and returns
// env_values. HOME is a fresh directory (no settings.yaml).
func dbEnvValuesGET(t *testing.T, rows map[string][2]string, boot, env map[string]any, configure func(*Server)) map[string]EnvKeyValue {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if boot == nil {
		boot = map[string]any{}
	}
	if env == nil {
		env = map[string]any{}
	}
	fakeStore := newFakeHubSettingStore()
	for section, r := range rows {
		fakeStore.seedWithOrigin(section, json.RawMessage(r[1]), r[0])
	}
	ops := NewOperationalSettings(fakeStore, newFileKoanf(t, boot), newEnvKoanf(t, env))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	if configure != nil {
		configure(srv)
	}
	srv.SetOperationalSettings(ops)
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigDBResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp.EnvValues
}

func wantEnvValue(t *testing.T, values map[string]EnvKeyValue, key string, want EnvKeyValue) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Errorf("env_values lacks %s", key)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env_values[%s] = %#v, want %#v", key, got, want)
	}
}

// TestGetServerConfigDB_EnvValuesWorkstationStartupOverrides: on a
// workstation hub, start-up sets hub.host to loopback (and storage, secrets
// and the broker switch) whatever the environment says, so an environment
// value of such a key is in effect only when it equals the start-up value.
func TestGetServerConfigDB_EnvValuesWorkstationStartupOverrides(t *testing.T) {
	enabled := true
	values := dbEnvValuesGET(t, nil, nil, map[string]any{
		"server.hub.host":         "0.0.0.0",
		"server.hub.port":         9810,
		"server.storage.provider": "gcs",
		"server.broker.enabled":   true,
		"server.secrets.backend":  "local",
	}, func(s *Server) {
		s.workstation = true
		s.config.StartupServerSettings = &config.V1ServerConfig{
			Hub:     &config.V1ServerHubConfig{Host: "127.0.0.1", Port: 9810},
			Broker:  &config.V1BrokerConfig{Enabled: enabled},
			Storage: &config.V1StorageConfig{Provider: "local"},
			Secrets: &config.V1SecretsConfig{Backend: "local"},
		}
	})
	wantEnvValue(t, values, "server.hub.host", EnvKeyValue{Value: "0.0.0.0", InEffect: boolPtr(false)})
	wantEnvValue(t, values, "server.hub.port", EnvKeyValue{Value: float64(9810), InEffect: boolPtr(true)})
	wantEnvValue(t, values, "server.storage.provider", EnvKeyValue{Value: "gcs", InEffect: boolPtr(false)})
	wantEnvValue(t, values, "server.broker.enabled", EnvKeyValue{Value: true, InEffect: boolPtr(true)})
	// "secret" in the key name: masked by the fail-closed key-word rule, so
	// neither bit is reported, whatever the start-up value.
	wantEnvValue(t, values, "server.secrets.backend", maskedEnvKeyValue())
}

// TestFileMode_EnvValuesStartupOverrides: a flag or workstation value beats
// the environment in the settings.yaml tier too.
func TestFileMode_EnvValuesStartupOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_HOST", "0.0.0.0")
	t.Setenv("SCION_SERVER_HUB_PORT", "9090")
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.StartupServerSettings = &config.V1ServerConfig{Hub: &config.V1ServerHubConfig{Host: "127.0.0.1", Port: 9090}}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantEnvValue(t, resp.EnvValues, "server.hub.host", EnvKeyValue{Value: "0.0.0.0", InEffect: boolPtr(false)})
	wantEnvValue(t, resp.EnvValues, "server.hub.port", EnvKeyValue{Value: float64(9090), InEffect: boolPtr(true)})
}

// TestGetServerConfigDB_EnvValuesSavedDiffers: saved_differs is set only
// when a managed row holds the key with another value; a row equal to the
// environment value, a section without a row, and a seeded row are not
// "saved, not in effect".
func TestGetServerConfigDB_EnvValuesSavedDiffers(t *testing.T) {
	values := dbEnvValuesGET(t, map[string][2]string{
		"access":    {"managed", `{"admin_emails":["env@example.com"],"authorized_domains":["saved.example"]}`},
		"endpoints": {"seeded", `{"monitoring_dashboard_url":"https://seed.example"}`},
	}, map[string]any{
		"server.hub.admin_emails":             []any{"env@example.com"},
		"server.auth.authorized_domains":      []any{"env.example"},
		"server.hub.monitoring_dashboard_url": "https://env.example",
		"telemetry.enabled":                   true,
	}, map[string]any{
		"server.hub.admin_emails":             []any{"env@example.com"},
		"server.hub.monitoring_dashboard_url": "https://env.example",
		"telemetry.enabled":                   true,
	}, func(s *Server) { s.config.AuthorizedDomains = []string{"env.example"} })
	// A managed row equal to the environment value: in effect, nothing to label.
	wantEnvValue(t, values, "server.hub.admin_emails", EnvKeyValue{Value: []any{"env@example.com"}, InEffect: boolPtr(true)})
	// No row: the bootstrap (environment) value is in effect, nothing saved.
	wantEnvValue(t, values, "telemetry.enabled", EnvKeyValue{Value: true, InEffect: boolPtr(true)})
	// A seeded row holding another replica's value wins over this node's
	// environment, but it is not a saved value.
	wantEnvValue(t, values, "server.hub.monitoring_dashboard_url", EnvKeyValue{Value: "https://env.example", InEffect: boolPtr(false)})
}

// TestGetServerConfigDB_EnvValuesSeededRowFromThisBootstrap: a seeded row
// holding this node's bootstrap value: the environment value is in effect.
func TestGetServerConfigDB_EnvValuesSeededRowFromThisBootstrap(t *testing.T) {
	values := dbEnvValuesGET(t, map[string][2]string{
		"endpoints": {"seeded", `{"monitoring_dashboard_url":"https://env.example"}`},
	}, nil, map[string]any{
		"server.hub.monitoring_dashboard_url": "https://env.example",
	}, nil)
	wantEnvValue(t, values, "server.hub.monitoring_dashboard_url", EnvKeyValue{Value: "https://env.example", InEffect: boolPtr(true)})
}

// TestGetServerConfigDB_EnvValuesManagedRowLackingKey: a managed row that
// does not hold the key replaces the whole section (Snapshot loads the
// bootstrap only for sections without a row), so the hub applies the key's
// zero value and the environment value is not in effect.
func TestGetServerConfigDB_EnvValuesManagedRowLackingKey(t *testing.T) {
	values := dbEnvValuesGET(t, map[string][2]string{
		"lifecycle": {"managed", `{"soft_delete_retention":"72h"}`},
	}, map[string]any{
		"server.hub.auto_suspend_stalled": true,
	}, map[string]any{
		"server.hub.auto_suspend_stalled": true,
	}, nil)
	wantEnvValue(t, values, "server.hub.auto_suspend_stalled", EnvKeyValue{Value: true, InEffect: boolPtr(false)})
}

// TestGetServerConfigDB_EnvValuesPinnedPublicURL: an operator setting pins
// the public URL on this node, so its environment value is in effect even
// against a saved row.
func TestGetServerConfigDB_EnvValuesPinnedPublicURL(t *testing.T) {
	values := dbEnvValuesGET(t, map[string][2]string{
		"endpoints": {"managed", `{"public_url":"https://saved.example"}`},
	}, map[string]any{
		config.PublicURLKey: "https://env.example",
	}, map[string]any{
		config.PublicURLKey: "https://env.example",
	}, func(s *Server) {
		s.config.HubEndpoints = &HubEndpoints{Operator: "https://env.example", Public: "https://env.example", Pin: "SCION_SERVER_HUB_PUBLICURL"}
	})
	wantEnvValue(t, values, config.PublicURLKey, EnvKeyValue{Value: "https://env.example", InEffect: boolPtr(true), SavedDiffers: true})
}

// TestFileMode_EnvValuesSavedDiffers: in the settings.yaml tier
// saved_differs is set only for a key the file holds with another value.
func TestFileMode_EnvValuesSavedDiffers(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    admin_emails:\n      - env@example.com\n")
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "env@example.com")
	t.Setenv("SCION_SERVER_HUB_PORT", "9090")
	_, resp := fileTierGET(t)
	wantEnvValue(t, resp.EnvValues, "server.hub.admin_emails", EnvKeyValue{Value: []any{"env@example.com"}, InEffect: boolPtr(true)})
	// Not in the file: the page's default is not a saved value.
	wantEnvValue(t, resp.EnvValues, "server.hub.port", EnvKeyValue{Value: float64(9090), InEffect: boolPtr(true)})
}

// envValuesJSON returns the raw JSON of the env_values member of a GET body.
func envValuesJSON(t *testing.T, body []byte) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return string(top["env_values"])
}

// TestFileMode_MaskedEnvValueIsNoGuessOracle (G1): for a masked key,
// env_values carries neither in_effect nor saved_differs, so a caller who
// can write settings cannot confirm a guess of the secret: with the stored
// value (settings.yaml) and the start-up value set to the secret itself or
// to a wrong guess, the GET bodies are byte-identical. A non-masked key in
// the same body keeps both bits.
func TestFileMode_MaskedEnvValueIsNoGuessOracle(t *testing.T) {
	const secret = "postgres://user:SENTINEL-db-secret@db.example/scion"
	get := func(guess string) []byte {
		writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  database:\n    url: \""+guess+"\"\n  hub:\n    admin_emails:\n      - saved@example.com\n")
		t.Setenv("SCION_SERVER_DATABASE_URL", secret)
		t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "env@example.com")
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		srv.config.StartupServerSettings = &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: guess}}
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
		if rr.Code != http.StatusOK {
			t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}
	right := get(secret)
	wrong := get("postgres://user:wrong-guess@db.example/scion")
	if string(right) != string(wrong) {
		t.Fatalf("GET bodies differ between a right and a wrong guess:\nright: %s\nwrong: %s", right, wrong)
	}
	if strings.Contains(string(right), "SENTINEL-db-secret") {
		t.Fatalf("raw secret in the body: %s", right)
	}
	var resp ServerConfigResponse
	if err := json.Unmarshal(right, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantEnvValue(t, resp.EnvValues, "server.database.url", maskedEnvKeyValue())
	if strings.Contains(envValuesJSON(t, right), `"server.database.url":{"value":"********","masked":true,"`) {
		t.Errorf("masked entry carries more than value and masked: %s", envValuesJSON(t, right))
	}
	// A non-masked key keeps both bits.
	wantEnvValue(t, resp.EnvValues, "server.hub.admin_emails", EnvKeyValue{Value: []any{"env@example.com"}, InEffect: boolPtr(true), SavedDiffers: true})
}

// TestGetServerConfigDB_MaskedEnvValueIsNoGuessOracle (G1, DB tier): a
// Layer-1 secret (a telemetry cloud header) whose managed row holds the
// secret or a wrong guess gives byte-identical env_values (neither bit);
// with no bootstrap value for it the whole bodies are byte-identical too.
// A non-masked Layer-1 key keeps both bits.
func TestGetServerConfigDB_MaskedEnvValueIsNoGuessOracle(t *testing.T) {
	const secret = "Bearer SENTINEL-header-secret"
	get := func(guess string) ([]byte, map[string]EnvKeyValue) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		fakeStore := newFakeHubSettingStore()
		fakeStore.seedWithOrigin("telemetry", json.RawMessage(`{"enabled":true,"cloud":{"headers":{"authorization":"`+guess+`"}}}`), "managed")
		fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["saved@example.com"]}`), "managed")
		envK := newEnvKoanf(t, map[string]any{
			"telemetry.cloud.headers.authorization": secret,
			"server.hub.admin_emails":               []any{"env@example.com"},
		})
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), envK)
		if _, err := ops.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
		srv.SetOperationalSettings(ops)
		rr := httptest.NewRecorder()
		srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp ServerConfigDBResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return rr.Body.Bytes(), resp.EnvValues
	}
	right, rv := get(secret)
	wrong, _ := get("Bearer wrong-guess")
	if envValuesJSON(t, right) != envValuesJSON(t, wrong) {
		t.Fatalf("env_values differ between a right and a wrong guess:\nright: %s\nwrong: %s", envValuesJSON(t, right), envValuesJSON(t, wrong))
	}
	if string(right) != string(wrong) {
		t.Fatalf("GET bodies differ between a right and a wrong guess:\nright: %s\nwrong: %s", right, wrong)
	}
	if strings.Contains(string(right), "SENTINEL-header-secret") {
		t.Fatalf("raw secret in the body: %s", right)
	}
	wantEnvValue(t, rv, "telemetry.cloud.headers.authorization", maskedEnvKeyValue())
	wantEnvValue(t, rv, "server.hub.admin_emails", EnvKeyValue{Value: []any{"env@example.com"}, InEffect: boolPtr(false), SavedDiffers: true})
}

// TestGetServerConfigDB_EnvValuesReportedTwoDomainCase: the reported case on
// a DB-backed hub. SCION_SERVER_AUTH_AUTHORIZEDDOMAINS holds two
// comma-separated domains; the env layer keeps it as one string (as
// LoadEnvKoanf does), env_values shows the two domains (the same split as the
// bootstrap merge), and, the running list being those two domains, reports
// them in effect, with the different saved value labelled.
func TestGetServerConfigDB_EnvValuesReportedTwoDomainCase(t *testing.T) {
	values := dbEnvValuesGET(t, map[string][2]string{
		"access": {"managed", `{"authorized_domains":["google.com"]}`},
	}, map[string]any{
		"server.auth.authorized_domains": []any{"env-a.example", "env-b.example"},
	}, map[string]any{
		"server.auth.authorized_domains": "env-a.example,env-b.example",
	}, func(s *Server) { s.config.AuthorizedDomains = []string{"env-a.example", "env-b.example"} })
	wantEnvValue(t, values, "server.auth.authorized_domains", EnvKeyValue{
		Value:        []any{"env-a.example", "env-b.example"},
		InEffect:     boolPtr(true),
		SavedDiffers: true,
	})
}
