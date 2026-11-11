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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The server keyring file path (server.keyring_file, ptone/scion#1530) is
// a deploy-time secret input: every settings write path refuses a
// non-empty value in file, DB and workstation mode, ignores an empty one,
// and GET never shows it. Same contract as server.hub.conduit.peer_secret.

const keyringFileSettingValue = "/run/secrets/test-only-keyring.json"

var keyringFileBodies = map[string]string{
	"nested":      `{"server":{"keyring_file":"` + keyringFileSettingValue + `"}}`,
	"flat dotted": `{"server.keyring_file":"` + keyringFileSettingValue + `"}`,
	"case folded": `{"Server":{"Keyring_File":"` + keyringFileSettingValue + `"}}`,
	"with others": `{"default_model":"m1","server":{"keyring_file":"` + keyringFileSettingValue + `"}}`,
}

func TestNeverAPIWritable_KeyringFileRefusedInFileMode(t *testing.T) {
	for name, body := range keyringFileBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.KeyringFileKey}, tiaErrorKeys(t, rr))
			assert.NotContains(t, rr.Body.String(), keyringFileSettingValue, "the refusal must not echo the value")
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_KeyringFileRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range keyringFileBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.KeyringFileKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "keyring_file")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

func writeKeyringFileSettings(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	yaml := "schema_version: \"1\"\nserver:\n  log_level: warn\n  keyring_file: \"" + keyringFileSettingValue + "\"\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return path
}

func TestNeverAPIWritable_GetDoesNotShowKeyringFile(t *testing.T) {
	settingsPath := writeKeyringFileSettings(t)
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), keyringFileSettingValue)
	assert.NotContains(t, getRR.Body.String(), "keyring_file")
	// The GET body echoes back cleanly, and the file value is preserved.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), keyringFileSettingValue)
}

func TestNeverAPIWritable_DBGetDoesNotShowKeyringFile(t *testing.T) {
	writeKeyringFileSettings(t)
	srv, _, ops := newSQLiteHubInMode(t, false, nil)
	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), keyringFileSettingValue)
}

func TestDropEmptySecretSettings_KeyringFile(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"nested empty", `{"server":{"keyring_file":""}}`, `{}`},
		{"null", `{"server":{"keyring_file":null}}`, `{}`},
		{"dotted", `{"server.keyring_file":"","default_model":"m1"}`, `{"default_model":"m1"}`},
		{"both secrets", `{"server":{"keyring_file":"","log_level":"warn","hub":{"conduit":{"peer_secret":""}}}}`, `{"server":{"log_level":"warn"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.JSONEq(t, tc.want, string(dropEmptySecretSettings([]byte(tc.in))))
		})
	}
	body := `{"server":{"keyring_file":"x"}}`
	assert.Equal(t, body, string(dropEmptySecretSettings([]byte(body))))
}

func TestNeverAPIWritable_EmptyKeyringFileIgnoredInFileMode(t *testing.T) {
	settingsPath := writeKeyringFileSettings(t)
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"keyring_file":"","log_level":"info"}}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), keyringFileSettingValue, "an empty value does not clear the stored path")
}

// The world-readable keyring file override
// (server.keyring_file_allow_world_readable) is operator-controlled only:
// every settings write path refuses it in file, DB and workstation mode,
// and GET never shows it.
var keyringAllowWorldReadableBodies = map[string]string{
	"nested true":  `{"server":{"keyring_file_allow_world_readable":true}}`,
	"nested false": `{"server":{"keyring_file_allow_world_readable":false}}`,
	"flat dotted":  `{"server.keyring_file_allow_world_readable":true}`,
	"case folded":  `{"Server":{"Keyring_File_Allow_World_Readable":true}}`,
}

func TestNeverAPIWritable_KeyringAllowWorldReadableRefusedInFileMode(t *testing.T) {
	for name, body := range keyringAllowWorldReadableBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.KeyringFileAllowWorldReadableKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_KeyringAllowWorldReadableRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range keyringAllowWorldReadableBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.KeyringFileAllowWorldReadableKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "keyring_file_allow_world_readable")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

func TestNeverAPIWritable_GetDoesNotShowKeyringAllowWorldReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  log_level: warn\n  keyring_file_allow_world_readable: true\n"), 0o600))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), "keyring_file_allow_world_readable")
	// The GET body echoes back cleanly, and the file value is preserved.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "keyring_file_allow_world_readable: true")

	db, _, ops := newSQLiteHubInMode(t, false, nil)
	rr := httptest.NewRecorder()
	db.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "keyring_file_allow_world_readable")
}

// TestNeverAPIWritable_DBGetHidesKeyringAllowWorldReadable: in DB mode the
// GET reads settings.yaml (log_level is shown, proving the file was read)
// yet never shows the override, which is true in that file.
func TestNeverAPIWritable_DBGetHidesKeyringAllowWorldReadable(t *testing.T) {
	path := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  log_level: warn\n  keyring_file_allow_world_readable: true\n"), 0o600))
	db, _, ops := newSQLiteHubInMode(t, false, nil)
	rr := httptest.NewRecorder()
	db.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"log_level":"warn"`, "control: the settings file was read")
	assert.NotContains(t, rr.Body.String(), "keyring_file_allow_world_readable")
}

// TestNeverAPIWritable_EnvOverridesHideNeverWritableKeys: GET's
// env_overrides list does not reveal that a never-API-writable key is set
// from the environment, in file and DB mode; other env-set keys still show.
func TestNeverAPIWritable_EnvOverridesHideNeverWritableKeys(t *testing.T) {
	t.Setenv("SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE", "true")
	t.Setenv("SCION_SERVER_LOGLEVEL", "warn")

	writeKeyringFileSettings(t)
	srv := &Server{}
	fileRR := httptest.NewRecorder()
	srv.handleGetServerConfig(fileRR)
	require.Equal(t, http.StatusOK, fileRR.Code, fileRR.Body.String())
	assert.Contains(t, fileRR.Body.String(), "server.log_level", "control: env overrides are listed")
	assert.NotContains(t, fileRR.Body.String(), "keyring_file_allow_world_readable")

	db, _, ops := newSQLiteHubInMode(t, false, nil)
	dbRR := httptest.NewRecorder()
	db.handleGetServerConfigDB(dbRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, dbRR.Code, dbRR.Body.String())
	assert.NotContains(t, dbRR.Body.String(), "keyring_file_allow_world_readable")

	assert.Equal(t, []string{"server.log_level"}, withoutNeverAPIWritableKeys([]string{
		"server.keyring_file", "server.keyring_file_allow_world_readable", "server.log_level", "server.hub.conduit.peer_secret",
	}))
}
