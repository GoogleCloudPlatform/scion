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

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setOrUnsetTestEnv sets key to val for the test, or removes it entirely
// when val is empty (an empty SCION_ value would still override settings).
func setOrUnsetTestEnv(t *testing.T, key, val string) {
	t.Helper()
	t.Setenv(key, val)
	if val == "" {
		require.NoError(t, os.Unsetenv(key))
	}
}

// TestCheckHubAvailability_ProjectFlagPrecedence drives the CLI entry point
// with the --project / -g / --global values the root command passes through
// (--global becomes "global"). An explicit flag must win over the
// SCION_PROJECT* environment of an agent container (ptone/scion#3123), and
// -g global with no project ID resolves to the hub's Global project
// (ptone/scion#3124). The full precedence table lives in
// pkg/hubsync TestEnsureHubReady_ProjectPrecedence.
func TestCheckHubAvailability_ProjectFlagPrecedence(t *testing.T) {
	const (
		envProjectID  = "env-project-id"
		globalLocalID = "global-local-id"
		hubGlobalID   = "hub-global-project-id"
	)

	cases := []struct {
		name            string
		flag            string
		globalProjectID string
		envID, envSlug  string
		wantID          string
	}{
		{name: "-g global beats env", flag: "global", globalProjectID: globalLocalID, envID: envProjectID, envSlug: "env-project", wantID: globalLocalID},
		{name: "-g global beats env, hub Global", flag: "global", envID: envProjectID, envSlug: "env-project", wantID: hubGlobalID},
		{name: "-g global, no env, hub Global", flag: "global", wantID: hubGlobalID},
		{name: "no flag, env wins", flag: "", globalProjectID: globalLocalID, envID: envProjectID, envSlug: "env-project", wantID: envProjectID},
		{name: "no flag, no env, global settings", flag: "", globalProjectID: globalLocalID, wantID: globalLocalID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/healthz":
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				case "/api/v1/projects":
					var projects []hubclient.Project
					if r.URL.Query().Get("slug") == "global" {
						projects = append(projects, hubclient.Project{ID: hubGlobalID, Name: "Global", Slug: "global"})
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": projects, "totalCount": len(projects)})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			tmpHome := t.TempDir()
			globalDir := filepath.Join(tmpHome, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0755))
			settings := fmt.Sprintf("hub:\n  enabled: true\n  endpoint: %s\n", server.URL)
			if tc.globalProjectID != "" {
				settings = "project_id: " + tc.globalProjectID + "\n" + settings
			}
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0644))

			t.Setenv("HOME", tmpHome)
			setOrUnsetTestEnv(t, "SCION_HUB_ENDPOINT", server.URL)
			setOrUnsetTestEnv(t, "SCION_HUB_URL", "")
			setOrUnsetTestEnv(t, "SCION_HUB_PROJECT_ID", "")
			setOrUnsetTestEnv(t, "SCION_PROJECT_ID", tc.envID)
			setOrUnsetTestEnv(t, "SCION_PROJECT", tc.envSlug)
			setOrUnsetTestEnv(t, "SCION_DEV_TOKEN", "test-dev-token")
			setOrUnsetTestEnv(t, "SCION_AUTH_TOKEN", "")
			t.Chdir(tmpHome)

			hubCtx, err := CheckHubAvailabilityWithOptions(tc.flag, true)
			require.NoError(t, err)
			require.NotNil(t, hubCtx)
			assert.Equal(t, tc.wantID, hubCtx.ProjectID)

			id, err := GetProjectID(hubCtx)
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// TestGetProjectID_UnlinkedGlobalExplainsFlag covers the remaining
// ptone/scion#3124 case: a hub context on the local global directory with
// no project ID and no git remote explains how to reach a hub project
// instead of asking for a git origin remote.
func TestGetProjectID_UnlinkedGlobalExplainsFlag(t *testing.T) {
	t.Chdir(t.TempDir()) // not a git repository

	_, err := GetProjectID(&HubContext{IsGlobal: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global (-g global)")
	assert.NotContains(t, err.Error(), "git origin remote")

	// A non-global project keeps the git remote guidance.
	_, err = GetProjectID(&HubContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no git origin remote found")
}
