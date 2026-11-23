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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNeverAPIWritable_AllowedImportSourcesRefused: server.hub.allowed_import_sources
// is settings-file startup config only. The server-config API refuses it in
// every mode, and a file-mode GET leaves it out so an echo of GET is not refused.
func TestNeverAPIWritable_AllowedImportSourcesRefused(t *testing.T) {
	body := `{"server":{"hub":{"allowed_import_sources":["https://internal.example/"]}}}`
	t.Run("file", func(t *testing.T) {
		rr, settingsPath := fileModePutServerConfig(t, &Server{}, body)
		assert.Equal(t, []string{config.AllowedImportSourcesKey}, tiaErrorKeys(t, rr))
		_, err := os.Stat(settingsPath)
		assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
	})
	for _, workstation := range []bool{false, true} {
		t.Run(map[bool]string{false: "db", true: "workstation"}[workstation], func(t *testing.T) {
			workstationHome(t)
			srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
			before := hubSettingRevisions(t, st)
			rr := putServerConfig(t, srv, body)
			assert.Equal(t, []string{config.AllowedImportSourcesKey}, tiaErrorKeys(t, rr))
			assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
		})
	}

	t.Run("get does not echo", func(t *testing.T) {
		settingsPath := tempSettingsHome(t)
		yaml := "schema_version: \"1\"\nserver:\n  hub:\n    allowed_import_sources:\n      - gs://sentinel-bucket/templates\n"
		require.NoError(t, os.WriteFile(settingsPath, []byte(yaml), 0o644))
		srv := &Server{}
		getRR := httptest.NewRecorder()
		srv.handleGetServerConfig(getRR)
		require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
		assert.NotContains(t, getRR.Body.String(), "sentinel-bucket")
		echo := httptest.NewRecorder()
		srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
		assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		assert.Contains(t, string(data), "gs://sentinel-bucket/templates", "the file value is preserved")
	})
}
