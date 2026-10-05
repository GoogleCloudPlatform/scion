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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// setProjectFlagForTest sets the --project flag variable as the root command
// leaves it (--global becomes "global") and restores it after the test.
func setProjectFlagForTest(t *testing.T, flag string) {
	t.Helper()
	origProject, origGlobal := projectPath, globalMode
	t.Cleanup(func() { projectPath, globalMode = origProject, origGlobal })
	projectPath, globalMode = flag, false
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
		name string
		flag string
		// passResolvedCwd passes config.GetResolvedProjectDir("") with no
		// flag, as harness-config sync, push and install do.
		passResolvedCwd bool
		globalProjectID string
		envID, envSlug  string
		wantID          string
	}{
		{name: "-g global beats env", flag: "global", globalProjectID: globalLocalID, envID: envProjectID, envSlug: "env-project", wantID: globalLocalID},
		{name: "-g global beats env, hub Global", flag: "global", envID: envProjectID, envSlug: "env-project", wantID: hubGlobalID},
		{name: "-g global, no env, hub Global", flag: "global", wantID: hubGlobalID},
		{name: "no flag, env wins", flag: "", globalProjectID: globalLocalID, envID: envProjectID, envSlug: "env-project", wantID: envProjectID},
		{name: "no flag, no env, global settings", flag: "", globalProjectID: globalLocalID, wantID: globalLocalID},
		{name: "no flag, resolved cwd dir, env wins", passResolvedCwd: true, globalProjectID: globalLocalID, envID: envProjectID, envSlug: "env-project", wantID: envProjectID},
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
			setProjectFlagForTest(t, tc.flag)

			arg := tc.flag
			if tc.passResolvedCwd {
				resolved, err := config.GetResolvedProjectDir("")
				require.NoError(t, err)
				arg = resolved
			}

			hubCtx, err := CheckHubAvailabilityWithOptions(arg, true)
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
	assert.Contains(t, err.Error(), "--project <slug|id>")
	assert.NotContains(t, err.Error(), "Pass --global")
	assert.NotContains(t, err.Error(), "git origin remote")

	// A non-global project keeps the git remote guidance.
	_, err = GetProjectID(&HubContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no git origin remote found")
}

// TestRequireHubClient_ProjectFlagPrecedence covers the conversation and
// notifications path: with --global (or -g <dir>) the settings project ID
// comes from that project, not from SCION_PROJECT_ID in the agent
// container's environment; without a flag the environment still wins
// (ptone/scion#3123).
func TestRequireHubClient_ProjectFlagPrecedence(t *testing.T) {
	const (
		envProjectID  = "env-project-id"
		globalLocalID = "global-local-id"
	)
	cases := []struct {
		name   string
		flag   string
		wantID string
	}{
		{name: "--global beats env", flag: "global", wantID: globalLocalID},
		{name: "no flag, env wins", flag: "", wantID: envProjectID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			globalDir := filepath.Join(tmpHome, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0755))
			settings := "project_id: " + globalLocalID + "\nhub:\n  enabled: true\n  endpoint: http://hub.invalid\n"
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0644))

			t.Setenv("HOME", tmpHome)
			setOrUnsetTestEnv(t, "SCION_HUB_ENDPOINT", "http://hub.invalid")
			setOrUnsetTestEnv(t, "SCION_HUB_URL", "")
			setOrUnsetTestEnv(t, "SCION_HUB_PROJECT_ID", "")
			setOrUnsetTestEnv(t, "SCION_PROJECT_ID", envProjectID)
			setOrUnsetTestEnv(t, "SCION_PROJECT", "env-project")
			setOrUnsetTestEnv(t, "SCION_DEV_TOKEN", "test-dev-token")
			setOrUnsetTestEnv(t, "SCION_AUTH_TOKEN", "")
			t.Chdir(tmpHome)
			setProjectFlagForTest(t, tc.flag)

			got, _, err := requireHubClient()
			require.NoError(t, err)
			id, err := resolveProjectID(got, "")
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// setupFlagPrecedenceHome writes ~/.scion/settings.yaml (with project_id
// localID when set) pointing at endpoint, makes HOME the cwd, and sets an
// agent container environment with SCION_PROJECT_ID envID.
func setupFlagPrecedenceHome(t *testing.T, endpoint, localID, envID string) {
	t.Helper()
	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	settings := fmt.Sprintf("hub:\n  enabled: true\n  endpoint: %s\n", endpoint)
	if localID != "" {
		settings = "project_id: " + localID + "\n" + settings
	}
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0644))

	t.Setenv("HOME", tmpHome)
	setOrUnsetTestEnv(t, "SCION_HUB_ENDPOINT", endpoint)
	setOrUnsetTestEnv(t, "SCION_HUB_URL", "")
	setOrUnsetTestEnv(t, "SCION_HUB_PROJECT_ID", "")
	setOrUnsetTestEnv(t, "SCION_PROJECT_ID", envID)
	setOrUnsetTestEnv(t, "SCION_PROJECT", "env-project")
	setOrUnsetTestEnv(t, "SCION_DEV_TOKEN", "test-dev-token")
	setOrUnsetTestEnv(t, "SCION_AUTH_TOKEN", "")
	t.Chdir(tmpHome)
}

// TestCheckHubAvailability_ClearedPathKeepsEnvProject covers a
// cross-project message send: --project names another project, but the
// caller clears the path to resolve the sender's own project. An empty
// path is not an explicit target, so SCION_PROJECT_ID wins and the hub
// Global project is never looked up.
func TestCheckHubAvailability_ClearedPathKeepsEnvProject(t *testing.T) {
	const envProjectID = "env-project-id"
	for _, hubHasGlobal := range []bool{true, false} {
		t.Run(fmt.Sprintf("hub Global %v", hubHasGlobal), func(t *testing.T) {
			globalLookups := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/healthz":
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				case "/api/v1/projects":
					var projects []hubclient.Project
					if r.URL.Query().Get("slug") == "global" {
						globalLookups++
						if hubHasGlobal {
							projects = append(projects, hubclient.Project{ID: "hub-global", Name: "Global", Slug: "global"})
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": projects, "totalCount": len(projects)})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			setupFlagPrecedenceHome(t, server.URL, "", envProjectID)
			setProjectFlagForTest(t, "other-project")

			hubCtx, err := CheckHubAvailabilityWithOptions("", true)
			require.NoError(t, err)
			require.NotNil(t, hubCtx)
			assert.Equal(t, envProjectID, hubCtx.ProjectID)
			assert.Zero(t, globalLookups, "no Global project lookup expected")
		})
	}
}

// TestSkillResolverHubOptions pins the create command's skill-resolver
// hub context: only a flag-named, non-empty path is an explicit target.
func TestSkillResolverHubOptions(t *testing.T) {
	cases := []struct {
		name, flag, path string
		want             bool
	}{
		{name: "-g global", flag: "global", path: "global", want: true},
		{name: "-g dir", flag: "/some/project", path: "/some/project", want: true},
		{name: "no flag", flag: "", path: "", want: false},
		{name: "flag, cleared path", flag: "other-project", path: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setProjectFlagForTest(t, tc.flag)
			opts := skillResolverHubOptions(tc.path)
			assert.Equal(t, tc.want, opts.ExplicitProject)
			assert.True(t, opts.SkipSync)
			assert.True(t, opts.AutoConfirm)
		})
	}
}

// TestTemplateSyncHubContext covers template sync after hub link: the
// context targets the just-linked project, and with --global the settings
// keep the global project's own ID over SCION_PROJECT_ID.
func TestTemplateSyncHubContext(t *testing.T) {
	const (
		envProjectID  = "env-project-id"
		globalLocalID = "global-local-id"
		linkedID      = "linked-project-id"
	)
	setupFlagPrecedenceHome(t, "http://hub.invalid", globalLocalID, envProjectID)
	setProjectFlagForTest(t, "global")

	globalDir, err := config.GetResolvedProjectDir("global")
	require.NoError(t, err)

	hubCtx, err := templateSyncHubContext(globalDir, "http://hub.invalid", linkedID)
	require.NoError(t, err)
	require.NotNil(t, hubCtx.Client)
	assert.Equal(t, linkedID, hubCtx.ProjectID)
	assert.Equal(t, globalDir, hubCtx.ProjectPath)
	require.NotNil(t, hubCtx.Settings)
	assert.Equal(t, globalLocalID, hubCtx.Settings.ProjectID)
	assert.True(t, hubCtx.IsGlobal, "the global project path must be reported as global")

	id, err := GetProjectID(hubCtx)
	require.NoError(t, err)
	assert.Equal(t, linkedID, id)
}

// TestTemplateSyncHubContext_NonGlobalProject pins IsGlobal to false when
// template sync targets a regular project directory.
func TestTemplateSyncHubContext_NonGlobalProject(t *testing.T) {
	const linkedID = "linked-project-id"
	setupFlagPrecedenceHome(t, "http://hub.invalid", "", "")

	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("hub:\n  enabled: true\n  endpoint: http://hub.invalid\n"), 0644))
	resolvedDir, err := filepath.EvalSymlinks(projectDir)
	require.NoError(t, err)

	hubCtx, err := templateSyncHubContext(resolvedDir, "http://hub.invalid", linkedID)
	require.NoError(t, err)
	assert.False(t, hubCtx.IsGlobal, "a regular project path must not be reported as global")
	assert.Equal(t, linkedID, hubCtx.ProjectID)
}

// TestResolveProjectIDByGitRemote_NilContext checks that a nil hub context
// returns an error instead of panicking.
func TestResolveProjectIDByGitRemote_NilContext(t *testing.T) {
	for _, failOnAmbiguous := range []bool{true, false} {
		assert.NotPanics(t, func() {
			id, err := resolveProjectIDByGitRemote(nil, failOnAmbiguous)
			assert.Error(t, err)
			assert.Empty(t, id)
		})
	}
	assert.NotPanics(t, func() {
		_, err := GetProjectID(nil)
		assert.Error(t, err)
	})
}
