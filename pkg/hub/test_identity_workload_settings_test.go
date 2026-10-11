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

//go:build !no_sqlite && (!hubshard || hubshard_2)

package hub

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AC X2-9 / advisor (b): the workload issuer allowlist
// (server.hub.test_identities) is startup config only. Every settings write
// path answers 422 for it, in file, DB and workstation mode, so the trust
// set cannot grow through the API. The helpers are test_admin_gate_test.go's
// (the same never-writable mechanism as server.hub.class).

var x2WorkloadIssuerBodies = map[string]string{
	"nested":        `{"server":{"hub":{"test_identities":{"workload_issuers":[{"sub":"1","max_tier":"member"}]}}}}`,
	"flat dotted":   `{"server.hub.test_identities.workload_issuers":[{"sub":"1","max_tier":"member"}]}`,
	"partly dotted": `{"server.hub":{"test_identities":{"workload_issuers":[]}}}`,
	"case folded":   `{"Server":{"HUB":{"Test_Identities":{"Workload_Issuers":[{"sub":"1","max_tier":"member"}]}}}}`,
	"empty object":  `{"server":{"hub":{"test_identities":{}}}}`,
	"null value":    `{"server":{"hub":{"test_identities":null}}}`,
	"with others":   `{"default_model":"m1","server":{"hub":{"test_identities":{"workload_issuers":[]},"auto_suspend_stalled":true}}}`,
}

func TestNeverAPIWritable_WorkloadIssuersRefusedInFileMode(t *testing.T) {
	for name, body := range x2WorkloadIssuerBodies {
		t.Run(name, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			assert.Equal(t, []string{config.TestIdentitiesKey}, tiaErrorKeys(t, rr))
			_, err := os.Stat(settingsPath)
			assert.True(t, os.IsNotExist(err), "a refused PUT writes no settings file")
		})
	}
}

func TestNeverAPIWritable_WorkloadIssuersRefusedInDBAndWorkstationMode(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		for name, body := range x2WorkloadIssuerBodies {
			t.Run(map[bool]string{false: "db", true: "workstation"}[workstation]+"/"+name, func(t *testing.T) {
				settingsPath := workstationHome(t)
				srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
				require.Equal(t, workstation, srv.layer0Editable())
				before := hubSettingRevisions(t, st)
				rr := putServerConfig(t, srv, body)
				assert.Equal(t, []string{config.TestIdentitiesKey}, tiaErrorKeys(t, rr))
				data, err := os.ReadFile(settingsPath)
				require.NoError(t, err)
				assert.NotContains(t, string(data), "test_identities")
				assert.Equal(t, before, hubSettingRevisions(t, st), "a refused PUT writes no DB row")
			})
		}
	}
}

func TestNeverAPIWritable_WorkloadIssuersHaveNoDBSection(t *testing.T) {
	key := config.TestIdentitiesKey + ".workload_issuers"
	layer1, layer0, _ := opsettings.ClassifyKeys([]string{key})
	assert.Empty(t, layer1, "no Layer-1 section can store the workload issuer allowlist")
	assert.Equal(t, []string{key}, layer0)
}

func TestNeverAPIWritable_GetDoesNotEchoWorkloadIssuers(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))
	yaml := "schema_version: \"1\"\nserver:\n  hub:\n    test_identities:\n      workload_issuers:\n        - sub: \"100000000000000000001\"\n          max_tier: member\n"
	require.NoError(t, os.WriteFile(settingsPath, []byte(yaml), 0o644))
	srv := &Server{}
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	assert.NotContains(t, getRR.Body.String(), "test_identities")
	assert.NotContains(t, getRR.Body.String(), "100000000000000000001")
	// The GET body echoes back cleanly, and the file value is preserved.
	echo := httptest.NewRecorder()
	srv.handleAdminServerConfig(echo, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
	assert.Equal(t, http.StatusOK, echo.Code, echo.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "100000000000000000001")
}
