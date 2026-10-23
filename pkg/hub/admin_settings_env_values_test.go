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
	"strings"
	"testing"
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
// hub class), the map sections whose values are withheld, and a key that
// matches no settings field. The values are strings unless noted.
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
	} {
		keys[k] = envSecretSentinel(k)
	}
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
		"server.auth.authorized_domains": {Value: envDomains, InEffect: true},
		"server.hub.admin_emails":        {Value: []any{"env@example.com"}, InEffect: false},
		"telemetry.enabled":              {Value: true, InEffect: true},
		"server.database.driver":         {Value: "postgres", InEffect: true},
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
	for k := range keys {
		ev, ok := resp.EnvValues[k]
		if !ok {
			t.Errorf("env_values lacks %s", k)
			continue
		}
		if !ev.Masked || ev.Value != maskedValue {
			t.Errorf("env_values[%s] = %#v, want masked placeholder", k, ev)
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

	rr, resp := fileTierGET(t)
	if strings.Contains(rr.Body.String(), "SENTINEL-") {
		t.Fatalf("raw secret env value in the file-mode GET body: %s", rr.Body.String())
	}
	if got := resp.EnvValues["server.auth.authorized_domains"]; !reflect.DeepEqual(got, EnvKeyValue{Value: []any{"env-a.example", "env-b.example"}, InEffect: true}) {
		t.Errorf("env_values[server.auth.authorized_domains] = %#v", got)
	}
	for _, k := range []string{"server.auth.dev_token", "server.database.url"} {
		if got := resp.EnvValues[k]; !got.Masked || got.Value != maskedValue || !got.InEffect {
			t.Errorf("env_values[%s] = %#v, want masked and in effect", k, got)
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
	if got := resp.EnvValues["server.auth.authorized_domains"]; !reflect.DeepEqual(got, EnvKeyValue{Value: []any{"env-a.example"}, InEffect: true}) {
		t.Errorf("env_values[server.auth.authorized_domains] = %#v", got)
	}
}
