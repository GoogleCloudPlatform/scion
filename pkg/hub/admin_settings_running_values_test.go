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
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/require"
)

// runningSentinel is a secret-looking value that must never reach a GET
// body.
const runningSentinel = "SENTINEL-running-db-url-raw"

// fileTierGETWithStartup runs the file-tier GET with the given start-up
// server settings.
func fileTierGETWithStartup(t *testing.T, startup *config.V1ServerConfig) (string, ServerConfigResponse) {
	t.Helper()
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.StartupServerSettings = startup
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("file-mode GET: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ServerConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return rr.Body.String(), resp
}

// workstationStartup is a start-up server block as a workstation start
// resolves it: loopback host, local storage, and a database URL.
func workstationStartup() *config.V1ServerConfig {
	return &config.V1ServerConfig{
		Hub:      &config.V1ServerHubConfig{Host: "127.0.0.1", Port: 9810},
		Storage:  &config.V1StorageConfig{Provider: "local"},
		Database: &config.V1DatabaseConfig{Driver: "sqlite", URL: "postgres://u:" + runningSentinel + "@db/scion"},
	}
}

// TestFileMode_RunningValues: a start-up-set key whose running value
// differs from the settings.yaml value is reported with its running value;
// an equal key is not; the database URL (masked) never is, and its value
// never reaches the body.
func TestFileMode_RunningValues(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    host: 0.0.0.0\n    port: 9810\n  storage:\n    provider: gcs\n  database:\n    driver: sqlite\n    url: postgres://u:other@db/scion\n")
	body, resp := fileTierGETWithStartup(t, workstationStartup())
	if strings.Contains(body, runningSentinel) {
		t.Fatalf("raw running database URL in the GET body: %s", body)
	}
	want := map[string]RunningKeyValue{
		"server.hub.host":         {Value: "127.0.0.1", Source: RunningValueSourceStartup},
		"server.storage.provider": {Value: "local", Source: RunningValueSourceStartup},
	}
	if !reflect.DeepEqual(resp.RunningValues, want) {
		t.Errorf("running_values = %#v, want %#v", resp.RunningValues, want)
	}
	// The shown value is unchanged: running_values is additive.
	if resp.Server == nil || resp.Server.Hub == nil || resp.Server.Hub.Host != "0.0.0.0" {
		t.Errorf("server.hub.host in body changed: %+v", resp.Server)
	}
}

// TestFileMode_RunningValuesNoSettingsFile: the no-file branch reports the
// running values too (every set start-up value differs from "unset").
func TestFileMode_RunningValuesNoSettingsFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body, resp := fileTierGETWithStartup(t, workstationStartup())
	if strings.Contains(body, runningSentinel) {
		t.Fatalf("raw running database URL in the GET body: %s", body)
	}
	for _, k := range []string{"server.hub.host", "server.hub.port", "server.storage.provider"} {
		if _, ok := resp.RunningValues[k]; !ok {
			t.Errorf("running_values lacks %s: %#v", k, resp.RunningValues)
		}
	}
	if _, ok := resp.RunningValues["server.database.url"]; ok {
		t.Errorf("running_values lists the masked database URL")
	}
}

// TestFileMode_RunningValuesNilStartup: without start-up settings there is
// nothing to report.
func TestFileMode_RunningValuesNilStartup(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    host: 0.0.0.0\n")
	body, resp := fileTierGETWithStartup(t, nil)
	if resp.RunningValues != nil || strings.Contains(body, "running_values") {
		t.Errorf("running_values without start-up settings: %#v", resp.RunningValues)
	}
}

// TestFileMode_RunningValuesEnvInEffect: when the environment sets a key to
// the running value, env_values reports it in effect and running_values has
// no duplicate entry; when the environment value loses to start-up, the
// running value is reported next to the env entry.
func TestFileMode_RunningValuesEnvInEffect(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    host: 0.0.0.0\n  storage:\n    provider: gcs\n")
	t.Setenv("SCION_SERVER_HUB_HOST", "127.0.0.1")
	t.Setenv("SCION_SERVER_STORAGE_PROVIDER", "gcs")
	_, resp := fileTierGETWithStartup(t, workstationStartup())
	if _, ok := resp.RunningValues["server.hub.host"]; ok {
		t.Errorf("running_values duplicates an env value in effect: %#v", resp.RunningValues)
	}
	wantEnvValue(t, resp.EnvValues, "server.hub.host", EnvKeyValue{Value: "127.0.0.1", InEffect: boolPtr(true), SavedDiffers: true})
	wantEnvValue(t, resp.EnvValues, "server.storage.provider", EnvKeyValue{Value: "gcs", InEffect: boolPtr(false)})
	if got := resp.RunningValues["server.storage.provider"]; got != (RunningKeyValue{Value: "local", Source: RunningValueSourceStartup}) {
		t.Errorf("running_values[server.storage.provider] = %#v", got)
	}
}

// TestFileMode_RunningValuesNoGuessOracle: whether a masked key has an
// entry must not depend on the stored value, or a settings writer could
// confirm a guessed secret by writing it and reading the GET. With the
// stored database URL equal to the running one, and with it different, the
// response has no entry for it either way.
func TestFileMode_RunningValuesNoGuessOracle(t *testing.T) {
	running := workstationStartup().Database.URL
	for _, stored := range []string{running, "postgres://u:guess@db/scion"} {
		writeFileTierSettings(t, fmt.Sprintf("schema_version: \"1\"\nserver:\n  database:\n    driver: sqlite\n    url: %q\n", stored))
		body, resp := fileTierGETWithStartup(t, workstationStartup())
		if _, ok := resp.RunningValues["server.database.url"]; ok {
			t.Errorf("stored %q: running_values lists the database URL", stored)
		}
		if strings.Contains(body, runningSentinel) && stored != running {
			t.Errorf("stored %q: raw running database URL in the body", stored)
		}
	}
}

// TestStartupRunningValues_SecretKeysNeverListed: every start-up-set key the
// env_values masking refuses (a key-word or never-API-writable key, or a
// masked field) has no entry, even when its running value differs.
func TestStartupRunningValues_SecretKeysNeverListed(t *testing.T) {
	startup := &config.V1ServerConfig{
		Secrets:  &config.V1SecretsConfig{Backend: "gcpsm"},
		Database: &config.V1DatabaseConfig{URL: "postgres://u:" + runningSentinel + "@db/x"},
		Hub:      &config.V1ServerHubConfig{Port: 1234},
	}
	got := startupRunningValues(nil, startup, &ServerConfigResponse{Server: &config.V1ServerConfig{}})
	if _, ok := got["server.secrets.backend"]; ok {
		t.Errorf("running_values lists server.secrets.backend (key word): %#v", got)
	}
	if _, ok := got["server.database.url"]; ok {
		t.Errorf("running_values lists server.database.url (masked): %#v", got)
	}
	// Control: a plain key with a differing value is listed.
	if got["server.hub.port"] != (RunningKeyValue{Value: float64(1234), Source: RunningValueSourceStartup}) {
		t.Errorf("running_values[server.hub.port] = %#v", got["server.hub.port"])
	}
	// Only start-up-set keys are ever reported.
	for k := range got {
		if !startupOverriddenServerKeys[k] {
			t.Errorf("running_values lists %s, not a start-up-set key", k)
		}
	}
}

// TestGetServerConfigDB_RunningValues: the DB-tier GET reports running
// values too, compared with the settings.yaml Layer-0 values it shows.
func TestGetServerConfigDB_RunningValues(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    host: 0.0.0.0\n    port: 9810\n")
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, newFileKoanf(t, map[string]any{}), newEnvKoanf(t, map[string]any{}))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.workstation = true
	srv.config.StartupServerSettings = workstationStartup()
	srv.SetOperationalSettings(ops)
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	if strings.Contains(rr.Body.String(), runningSentinel) {
		t.Fatalf("raw running database URL in the DB-tier GET body")
	}
	var resp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	want := map[string]RunningKeyValue{
		"server.hub.host":         {Value: "127.0.0.1", Source: RunningValueSourceStartup},
		"server.storage.provider": {Value: "local", Source: RunningValueSourceStartup},
	}
	if !reflect.DeepEqual(resp.RunningValues, want) {
		t.Errorf("running_values = %#v, want %#v", resp.RunningValues, want)
	}
}

// TestFileMode_RunningValuesEchoIsNotAChange: a client that sends the GET
// body back, running_values included, still gets 200; a changed entry is
// refused like any other read-only GET field.
func TestFileMode_RunningValuesEchoIsNotAChange(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  hub:\n    host: 0.0.0.0\n")
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.StartupServerSettings = workstationStartup()
	get := httptest.NewRecorder()
	srv.handleAdminServerConfig(get, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
	require.Equal(t, http.StatusOK, get.Code)
	require.Contains(t, get.Body.String(), `"running_values"`)

	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", get.Body.String()))
	require.Equal(t, http.StatusOK, echo.Code, "GET body echoed back: %s", echo.Body.String())

	changed := httptest.NewRecorder()
	srv.handleAdminServerConfig(changed, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"running_values": {"server.hub.host": {"value": "10.0.0.1", "source": "startup"}}}`))
	require.Equal(t, http.StatusUnprocessableEntity, changed.Code, "body: %s", changed.Body.String())
}

// TestFileMode_RunningValuesEmptySecretNoEntry: an EMPTY running database
// URL with a stored DSN gets no entry. Masking leaves "" unchanged, so only
// the explicit never-shown rule keeps it out; an entry would reveal that the
// node runs without the stored secret.
func TestFileMode_RunningValuesEmptySecretNoEntry(t *testing.T) {
	writeFileTierSettings(t, "schema_version: \"1\"\nserver:\n  database:\n    driver: postgres\n    url: postgres://u:"+runningSentinel+"@db/scion\n")
	startup := &config.V1ServerConfig{
		Hub:      &config.V1ServerHubConfig{Port: 1234},
		Database: &config.V1DatabaseConfig{Driver: "postgres", URL: ""},
	}
	body, resp := fileTierGETWithStartup(t, startup)
	if _, ok := resp.RunningValues["server.database.url"]; ok {
		t.Errorf("running_values lists server.database.url for an empty running value: %#v", resp.RunningValues)
	}
	if strings.Contains(body, runningSentinel) {
		t.Errorf("raw stored database URL in the GET body")
	}
	// Control: a plain differing key is still listed in the same response.
	if _, ok := resp.RunningValues["server.hub.port"]; !ok {
		t.Errorf("control: running_values lacks server.hub.port: %#v", resp.RunningValues)
	}
}

// TestStartupRunningValues_EmptySecretSkippedByRuleNotTag: the unit view of
// the same edge, independent of the URL field's omitempty tag. The view
// holds a stored DSN, the running value is empty.
func TestStartupRunningValues_EmptySecretSkippedByRuleNotTag(t *testing.T) {
	view := &ServerConfigResponse{Server: &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: "postgres://stored"}}}
	got := startupRunningValues(nil, &config.V1ServerConfig{Database: &config.V1DatabaseConfig{URL: ""}}, view)
	if _, ok := got["server.database.url"]; ok {
		t.Errorf("running_values lists server.database.url: %#v", got)
	}
	if !runningValuesNeverShown["server.database.url"] {
		t.Errorf("server.database.url is not in runningValuesNeverShown")
	}
}

// TestRunningValuesNeverShown_CoversMaskedKeys: every start-up-set string
// key whose non-empty value the GET masking masks is in
// runningValuesNeverShown, so a new secret key in startupOverriddenServerKeys
// cannot reach running_values with an empty value. Keys that
// envValueMayBeShown already refuses (for example the secrets backend) are
// skipped before the comparison and need no entry here.
func TestRunningValuesNeverShown_CoversMaskedKeys(t *testing.T) {
	stringKeys := 0
	for key := range startupOverriddenServerKeys {
		probe := newEnvKoanf(t, map[string]any{key: "probe-nonempty-value"})
		vs, err := config.DecodeSettingsKey(probe, key)
		if err != nil {
			continue // not a string-typed key (port, switch)
		}
		if _, err := config.FlattenSettingsJSON(vs); err != nil {
			continue
		}
		stringKeys++
		_, masked := viewMaskedEnvValue(probe, key)
		if masked && envValueMayBeShown(key) && !runningValuesNeverShown[key] {
			t.Errorf("%s: the GET masks it but it is not in runningValuesNeverShown", key)
		}
	}
	if stringKeys < 5 {
		t.Fatalf("probed only %d string keys; the probe is broken", stringKeys)
	}
	// Control: the database URL is masked by the view.
	if _, masked := viewMaskedEnvValue(newEnvKoanf(t, map[string]any{"server.database.url": "probe"}), "server.database.url"); !masked {
		t.Errorf("control: server.database.url is not masked by the GET view")
	}
}
