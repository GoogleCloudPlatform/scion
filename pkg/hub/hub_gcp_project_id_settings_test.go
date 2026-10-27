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

//go:build !no_sqlite

package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// server.hub.gcp_project_id selects the service-account minting project, so
// it is startup config only (ptone/scion#4942, ptone/scion#3830): every
// settings write path answers 422 for it, in file, DB and workstation mode,
// and GET leaves it out so an echo of GET is not refused. The helpers are
// test_admin_gate_test.go's (the same never-writable mechanism as
// server.hub.class).

const hubGCPProjectSentinel = "hub-project-sentinel"

var hubGCPProjectIDBodies = map[string]string{
	"nested":          `{"server":{"hub":{"gcp_project_id":"p1"}}}`,
	"flat dotted":     `{"server.hub.gcp_project_id":"p1"}`,
	"partly dotted":   `{"server.hub":{"gcp_project_id":"p1"}}`,
	"case folded":     `{"Server":{"HUB":{"GCP_Project_ID":"p1"}}}`,
	"empty value":     `{"server":{"hub":{"gcp_project_id":""}}}`,
	"null value":      `{"server":{"hub":{"gcp_project_id":null}}}`,
	"with other keys": `{"default_model":"m1","server":{"hub":{"gcp_project_id":"p1","auto_suspend_stalled":true}}}`,
}

func TestNeverAPIWritable_HubGCPProjectIDRefusedInFileMode(t *testing.T) {
	for name, body := range hubGCPProjectIDBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.HubGCPProjectIDKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_HubGCPProjectIDRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range hubGCPProjectIDBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				require.Equal(t, workstation, srv.layer0Editable())
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.HubGCPProjectIDKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "gcp_project_id")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

func TestNeverAPIWritable_HubGCPProjectIDHasNoDBSection(t *testing.T) {
	layer1, layer0, _ := opsettings.ClassifyKeys([]string{config.HubGCPProjectIDKey})
	assert.Empty(t, layer1, "no Layer-1 section can store server.hub.gcp_project_id")
	assert.Equal(t, []string{config.HubGCPProjectIDKey}, layer0)
}

func writeHubGCPProjectSettings(t *testing.T) string {
	t.Helper()
	settingsPath := tempSettingsHome(t)
	yaml := "schema_version: \"1\"\nserver:\n  hub:\n    gcp_project_id: " + hubGCPProjectSentinel + "\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(yaml), 0o644))
	return settingsPath
}

func TestNeverAPIWritable_FileModeGetDoesNotEchoHubGCPProjectID(t *testing.T) {
	settingsPath := writeHubGCPProjectSettings(t)
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), hubGCPProjectSentinel)
	// The GET body echoes back cleanly, and the file value is preserved.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "gcp_project_id: "+hubGCPProjectSentinel, "the file value is preserved")
}

func TestNeverAPIWritable_DBAndWorkstationGetDoesNotEchoHubGCPProjectID(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		t.Run(map[bool]string{false: "db", true: "workstation"}[workstation], func(t *testing.T) {
			settingsPath := writeHubGCPProjectSettings(t)
			srv, _, ops := newSQLiteHubInMode(t, workstation, nil)
			getRR := httptest.NewRecorder()
			srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
			require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
			assert.NotContains(t, getRR.Body.String(), hubGCPProjectSentinel)
			// Sending the GET body back is not refused for the key.
			echo := putServerConfig(t, srv, getRR.Body.String())
			assert.NotContains(t, echo.Body.String(), config.HubGCPProjectIDKey, echo.Body.String())
			data, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			assert.Contains(t, string(data), "gcp_project_id: "+hubGCPProjectSentinel, "the file value is preserved")
		})
	}
}

// TestNeverAPIWritable_EnvOverridesHideHubGCPProjectID: when
// server.hub.gcp_project_id is set from the environment, GET's env_overrides
// list does not reveal it, in file and DB mode (the never-API-writable
// filter); another env-set key still shows.
func TestNeverAPIWritable_EnvOverridesHideHubGCPProjectID(t *testing.T) {
	const envSentinel = "hub-project-env-sentinel"
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", envSentinel)
	t.Setenv("SCION_SERVER_LOGLEVEL", "warn")
	require.Contains(t, opsettings.DetectEnvOverrides(config.LoadEnvKoanf()), config.HubGCPProjectIDKey,
		"control: the environment variable sets server.hub.gcp_project_id")

	writeHubGCPProjectSettings(t)
	srv := &Server{}
	fileRR := httptest.NewRecorder()
	srv.handleGetServerConfig(fileRR)
	require.Equal(t, http.StatusOK, fileRR.Code, fileRR.Body.String())
	assert.Contains(t, fileRR.Body.String(), "server.log_level", "control: env overrides are listed")
	assert.NotContains(t, fileRR.Body.String(), config.HubGCPProjectIDKey)
	assert.NotContains(t, fileRR.Body.String(), envSentinel)

	// DB mode: newSQLiteHubInMode's settings use an empty env koanf, so
	// build them again over the same store from the real environment;
	// otherwise env_overrides would be empty and the check vacuous.
	db, st, _ := newSQLiteHubInMode(t, false, nil)
	ops := NewOperationalSettings(st, emptyKoanf(), config.LoadEnvKoanf())
	_, err := ops.Refresh(t.Context())
	require.NoError(t, err)
	require.Contains(t, ops.EnvOverriddenKeys(), config.HubGCPProjectIDKey,
		"control: the DB-mode settings see the environment variable")
	dbRR := httptest.NewRecorder()
	db.handleGetServerConfigDB(dbRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, dbRR.Code, dbRR.Body.String())
	assert.Contains(t, dbRR.Body.String(), `"server.log_level"`, "control: env overrides are listed")
	assert.NotContains(t, dbRR.Body.String(), config.HubGCPProjectIDKey)
	assert.NotContains(t, dbRR.Body.String(), envSentinel)

	assert.Equal(t, []string{"server.log_level"}, withoutNeverAPIWritableKeys([]string{
		config.HubGCPProjectIDKey, "server.log_level",
	}))
}

// envValuesOf decodes a server-config GET body's env_values map.
func envValuesOf(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		EnvValues map[string]json.RawMessage `json:"env_values"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	return resp.EnvValues
}

// TestNeverAPIWritable_EnvValuesHideHubGCPProjectID: GET's env_values (the
// value each env-overridden key has in the environment) never carries
// server.hub.gcp_project_id, in file and DB mode, while another env-set key
// does appear (positive control), and the environment value is nowhere in
// the body.
func TestNeverAPIWritable_EnvValuesHideHubGCPProjectID(t *testing.T) {
	const envSentinel = "hub-project-env-values-sentinel"
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", envSentinel)
	t.Setenv("SCION_SERVER_LOGLEVEL", "warn")
	require.Contains(t, opsettings.DetectEnvOverrides(config.LoadEnvKoanf()), config.HubGCPProjectIDKey,
		"control: the environment variable sets server.hub.gcp_project_id")

	writeHubGCPProjectSettings(t)
	srv := &Server{}
	fileRR := httptest.NewRecorder()
	srv.handleGetServerConfig(fileRR)
	require.Equal(t, http.StatusOK, fileRR.Code, fileRR.Body.String())
	fileValues := envValuesOf(t, fileRR.Body.Bytes())
	assert.Contains(t, fileValues, "server.log_level", "control: env_values lists another env-set key")
	assert.NotContains(t, fileValues, config.HubGCPProjectIDKey)
	assert.NotContains(t, fileRR.Body.String(), envSentinel)

	db, st, _ := newSQLiteHubInMode(t, false, nil)
	ops := NewOperationalSettings(st, emptyKoanf(), config.LoadEnvKoanf())
	_, err := ops.Refresh(t.Context())
	require.NoError(t, err)
	dbRR := httptest.NewRecorder()
	db.handleGetServerConfigDB(dbRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, dbRR.Code, dbRR.Body.String())
	dbValues := envValuesOf(t, dbRR.Body.Bytes())
	assert.Contains(t, dbValues, "server.log_level", "control: env_values lists another env-set key")
	assert.NotContains(t, dbValues, config.HubGCPProjectIDKey)
	assert.NotContains(t, dbRR.Body.String(), envSentinel)
}
