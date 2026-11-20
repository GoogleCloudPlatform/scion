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
	"context"
	"encoding/json"
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

// Item 9 / PV3 Addendum F §F10 (architect r1.22):
// server.hub.system_one.secret_names is a Layer-0 startup-only operator setting
// that is unconditionally refused by the server-config API across every
// accepted write shape (direct key, nested child key, parent/section
// replacement carrying secret_names, case-folded/camelCase variants, empty map
// and null, and section reset) on both file-backed and DB-backed settings
// paths, in both workstation and hosted modes.

var systemOneSecretNamesRefusedWriteBodies = map[string]string{
	"direct_dotted":                 `{"server.hub.system_one.secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}`,
	"nested_child_dotted":           `{"server.hub.system_one.secret_names.gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}`,
	"nested_child_object":           `{"server":{"hub":{"system_one":{"secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}}`,
	"parent_system_one_replacement": `{"server":{"hub":{"system_one":{"max_eval_time":"5s","secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}}`,
	"parent_hub_replacement":        `{"server":{"hub":{"auto_suspend_stalled":true,"system_one":{"secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}}`,
	"parent_server_replacement":     `{"default_model":"m1","server":{"log_level":"info","hub":{"system_one":{"secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}}`,
	"partly_dotted_server_hub":      `{"server.hub":{"system_one":{"secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}`,
	"partly_dotted_system_one":      `{"server":{"hub":{"system_one.secret_names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}`,
	"case_folded":                   `{"Server":{"HUB":{"System_One":{"Secret_Names":{"gemini_decisions":["GEMINI_API_KEY_BILLING_B"]}}}}}`,
	"empty_object":                  `{"server":{"hub":{"system_one":{"secret_names":{}}}}}`,
	"null_value":                    `{"server":{"hub":{"system_one":{"secret_names":null}}}}`,
}

func TestNeverAPIWritable_SystemOneSecretNamesClassifiedLayer0Only(t *testing.T) {
	keys := []string{
		config.SystemOneSecretNamesKey,
		config.SystemOneSecretNamesKey + ".gemini_decisions",
	}
	layer1, layer0, unclass := opsettings.ClassifyKeys(keys)
	assert.Empty(t, layer1, "no Layer-1 DB section may own server.hub.system_one.secret_names")
	assert.Empty(t, unclass)
	assert.Equal(t, keys, layer0)
	assert.Empty(t, opsettings.OwningSection(config.SystemOneSecretNamesKey))
	assert.Empty(t, opsettings.OwningSection(config.SystemOneSecretNamesKey+".gemini_decisions"))
}

func TestNeverAPIWritable_SystemOneSecretNamesRefusedInFileMode(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
		for shape, body := range systemOneSecretNamesRefusedWriteBodies {
			t.Run(method+"/"+shape, func(t *testing.T) {
				tmpHome := t.TempDir()
				t.Setenv("HOME", tmpHome)
				settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
				require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))

				srv := &Server{}
				rr := httptest.NewRecorder()
				srv.handleAdminServerConfig(rr, adminRequest(method, "/api/v1/admin/server-config", body))
				require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
				assert.Equal(t, []string{config.SystemOneSecretNamesKey}, tiaErrorKeys(t, rr))
				_, err := os.Stat(settingsPath)
				assert.True(t, os.IsNotExist(err), "a refused %s writes no settings file", method)
			})
		}
	}
}

func TestNeverAPIWritable_SystemOneSecretNamesRefusedInDBHostedAndWorkstationModes(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		modeName := map[bool]string{false: "hosted_db", true: "workstation_db"}[workstation]
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
			for shape, body := range systemOneSecretNamesRefusedWriteBodies {
				t.Run(modeName+"/"+method+"/"+shape, func(t *testing.T) {
					settingsPath := workstationHome(t)
					srv, st, _ := newSQLiteHubInMode(t, workstation, nil)
					require.Equal(t, workstation, srv.layer0Editable())
					beforeFile, err := os.ReadFile(settingsPath)
					require.NoError(t, err)
					beforeRevs := hubSettingRevisions(t, st)

					rr := httptest.NewRecorder()
					srv.handleAdminServerConfig(rr, adminRequest(method, "/api/v1/admin/server-config", body))
					require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
					assert.Equal(t, []string{config.SystemOneSecretNamesKey}, tiaErrorKeys(t, rr))

					afterFile, err := os.ReadFile(settingsPath)
					require.NoError(t, err)
					assert.Equal(t, string(beforeFile), string(afterFile), "a refused write must not alter settings.yaml")
					assert.NotContains(t, string(afterFile), "secret_names")
					assert.Equal(t, beforeRevs, hubSettingRevisions(t, st), "a refused write must write no DB row")
				})
			}
		}
	}
}

func TestNeverAPIWritable_SystemOneSecretNamesParentUpdatesAndSectionResetPreserveOperatorConfig(t *testing.T) {
	const seededYAML = `schema_version: "1"
server:
  mode: workstation
  hub:
    system_one:
      max_eval_time: 4s
      secret_names:
        gemini_decisions:
          - GEMINI_API_KEY_BILLING_B
`

	t.Run("file_mode_get_hides_and_parent_or_null_writes_preserve_secret_names_and_section_reset_refused", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
		require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o700))
		require.NoError(t, os.WriteFile(settingsPath, []byte(seededYAML), 0o644))

		srv := &Server{
			config: ServerConfig{
				StartupServerSettings: &config.V1ServerConfig{
					Hub: &config.V1ServerHubConfig{
						SystemOne: &config.V1ServerHubSystemOneConfig{
							MaxEvalTime: "4s",
							SecretNames: map[string][]string{"gemini_decisions": {"GEMINI_API_KEY_BILLING_B"}},
						},
					},
				},
			},
		}
		srv.initSystemOneRuntime()

		// GET never echoes secret_names or the extra name, and does not mutate StartupServerSettings.
		getRR := httptest.NewRecorder()
		srv.handleGetServerConfig(getRR)
		require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
		assert.NotContains(t, getRR.Body.String(), "secret_names")
		assert.NotContains(t, getRR.Body.String(), "GEMINI_API_KEY_BILLING_B")
		assert.Equal(t, []string{"GEMINI_API_KEY_BILLING_B"}, srv.config.StartupServerSettings.Hub.SystemOne.SecretNames["gemini_decisions"])

		// Echoing the GET body back succeeds and preserves secret_names on disk.
		echoRR := httptest.NewRecorder()
		srv.handleAdminServerConfig(echoRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config", getRR.Body.String()))
		require.Equal(t, http.StatusOK, echoRR.Code, echoRR.Body.String())

		// Sibling edit and parent null clears (system_one:null, hub:null) never erase secret_names on disk.
		for _, parentBody := range []string{
			`{"server":{"hub":{"system_one":{"max_eval_time":"6s"}}}}`,
			`{"server":{"hub":{"system_one":{}}}}`,
			`{"server":{"hub":{"system_one":null}}}`,
			`{"server":{"hub":null}}`,
		} {
			rr := httptest.NewRecorder()
			srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", parentBody))
			require.Equal(t, http.StatusOK, rr.Code, "body=%s resp=%s", parentBody, rr.Body.String())
			disk, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			assert.Contains(t, string(disk), "GEMINI_API_KEY_BILLING_B", "parent body %s must not erase operator secret_names", parentBody)
		}

		// Section reset on file-backed hub is refused (400).
		for _, sec := range []string{"system_one", "secret_names", "server.hub.system_one", "server.hub.system_one.secret_names", "quotas"} {
			resetRR := httptest.NewRecorder()
			srv.handleAdminServerConfigSectionReset(resetRR, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+sec, ""))
			assert.Equal(t, http.StatusBadRequest, resetRR.Code, "file-mode section reset %q must be refused: %s", sec, resetRR.Body.String())
		}
		disk, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		assert.Contains(t, string(disk), "GEMINI_API_KEY_BILLING_B")
	})

	for _, workstation := range []bool{false, true} {
		modeName := map[bool]string{false: "hosted_db", true: "workstation_db"}[workstation]
		t.Run(modeName+"_get_parent_updates_and_section_reset", func(t *testing.T) {
			settingsPath := startupBlockHome(t, seededYAML)
			srv, st, ops := newSQLiteHubInMode(t, workstation, map[string]string{
				"quotas": `{"enforce_broker_quotas":true}`,
			})
			srv.config.StartupServerSettings = &config.V1ServerConfig{
				Hub: &config.V1ServerHubConfig{
					SystemOne: &config.V1ServerHubSystemOneConfig{
						MaxEvalTime: "4s",
						SecretNames: map[string][]string{"gemini_decisions": {"GEMINI_API_KEY_BILLING_B"}},
					},
				},
			}
			srv.initSystemOneRuntime()

			// DB-backed GET hides secret_names and does not mutate StartupServerSettings.
			getRR := httptest.NewRecorder()
			srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
			require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
			assert.NotContains(t, getRR.Body.String(), "secret_names")
			assert.NotContains(t, getRR.Body.String(), "GEMINI_API_KEY_BILLING_B")
			assert.Equal(t, []string{"GEMINI_API_KEY_BILLING_B"}, srv.config.StartupServerSettings.Hub.SystemOne.SecretNames["gemini_decisions"])

			if workstation {
				// Workstation allows editing max_eval_time or nulling system_one/hub/server, while preserving secret_names on disk!
				for _, parentBody := range []string{
					`{"server":{"hub":{"system_one":{"max_eval_time":"6s"}}}}`,
					`{"server":{"hub":{"system_one":null}}}`,
					`{"server":{"hub":null}}`,
					`{"server":null}`,
				} {
					rr := putServerConfig(t, srv, parentBody)
					require.Equal(t, http.StatusOK, rr.Code, "workstation body=%s resp=%s", parentBody, rr.Body.String())
					disk, err := os.ReadFile(settingsPath)
					require.NoError(t, err)
					assert.Contains(t, string(disk), "GEMINI_API_KEY_BILLING_B", "workstation body %s must preserve secret_names", parentBody)
				}
			} else {
				// Hosted DB mode refuses all server.hub.system_one Layer-0 writes, including null/sibling writes.
				beforeRevs := hubSettingRevisions(t, st)
				for _, parentBody := range []string{
					`{"server":{"hub":{"system_one":{"max_eval_time":"6s"}}}}`,
					`{"server":{"hub":{"system_one":null}}}`,
				} {
					rr := putServerConfig(t, srv, parentBody)
					require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "hosted body=%s resp=%s", parentBody, rr.Body.String())
				}
				assert.Equal(t, beforeRevs, hubSettingRevisions(t, st))
			}

			// Section reset for system_one / secret_names is refused (404 Unknown section) in both hosted and workstation DB modes.
			for _, sec := range []string{"system_one", "secret_names", "server.hub.system_one", "server.hub.system_one.secret_names"} {
				beforeRevs := hubSettingRevisions(t, st)
				resetRR := httptest.NewRecorder()
				srv.handleAdminServerConfigSectionReset(resetRR, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+sec, ""))
				assert.Equal(t, http.StatusNotFound, resetRR.Code, "%s section reset %q must be refused: %s", modeName, sec, resetRR.Body.String())
				assert.Equal(t, beforeRevs, hubSettingRevisions(t, st))
			}

			// Resetting an unrelated managed Layer-1 section ("quotas") succeeds without affecting settings.yaml or the snapshotted allowlist.
			_, err := ops.Update(context.Background(), "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "admin@example.com", ops.SectionRevision("quotas"), "managed")
			require.NoError(t, err)
			quotasReset := httptest.NewRecorder()
			srv.handleAdminServerConfigSectionReset(quotasReset, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/quotas", ""))
			require.Equal(t, http.StatusOK, quotasReset.Code, quotasReset.Body.String())

			gemAdapter, ok := srv.SystemOneProviderRegistry().Lookup("gemini_decisions")
			require.True(t, ok)
			assert.Equal(t, []string{"GEMINI_API_KEY", "GEMINI_API_KEY_BILLING_B"}, srv.allowedSecretNamesFor(gemAdapter))
			disk, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			assert.Contains(t, string(disk), "GEMINI_API_KEY_BILLING_B")
		})
	}
}
