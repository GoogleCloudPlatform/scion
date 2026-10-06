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

package hubsync

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestEnsureHubReady_ProjectPrecedence covers which project ID a hub
// context resolves to when an explicit --project / -g flag, the
// SCION_PROJECT* environment of an agent container, a project .scion
// directory and the global directory disagree (ptone/scion#3123), and how
// -g global resolves when the environment carries no project ID
// (ptone/scion#3124).
//
// Order: an explicit flag wins over the environment; without a flag the
// environment wins inside a hub-connected container (the dispatcher sets the
// authoritative project for the agent); otherwise the project .scion, then
// the global directory.
func TestEnsureHubReady_ProjectPrecedence(t *testing.T) {
	const (
		envProjectID       = "env-project-id"
		envProjectSlug     = "env-project"
		fileProjectID      = "file-project-id"
		globalLocalID      = "global-local-id"
		hubGlobalProjectID = "hub-global-project-id"
		hubOtherProjectID  = "hub-other-project-id"
	)

	type testCase struct {
		name string
		// flag is the --project / -g value ("" = flag not given).
		flag string
		// flagIsProjectDir replaces flag with the temp project directory.
		flagIsProjectDir bool
		// cwdInProject runs from inside the temp project directory.
		cwdInProject bool
		// passResolvedCwd passes config.GetResolvedProjectDir("") with no
		// flag, as cmd callers such as harness-config sync do.
		passResolvedCwd bool
		// globalProjectID is written to ~/.scion/settings.yaml when set.
		globalProjectID string
		// env sets SCION_PROJECT / SCION_PROJECT_ID ("" = unset).
		envID, envSlug string
		// hubHasGlobal makes the mock hub serve a project with slug "global".
		hubHasGlobal bool
		// hubHasNamedGlobal makes the mock hub serve a project named
		// "Global" whose slug is not "global" for a ?name= lookup.
		hubHasNamedGlobal bool
		wantID            string
		wantGlobal        bool
		wantErr           string
		// wantNoGlobalLookup fails the case if the hub Global project is
		// looked up.
		wantNoGlobalLookup bool
	}

	cases := []testCase{
		// --- ptone/scion#3123: explicit flag vs environment ---
		{
			name:            "-g global beats env, global settings project_id",
			flag:            "global",
			globalProjectID: globalLocalID,
			envID:           envProjectID, envSlug: envProjectSlug,
			wantID:     globalLocalID,
			wantGlobal: true,
		},
		{
			name:  "-g global beats env, hub Global project",
			flag:  "global",
			envID: envProjectID, envSlug: envProjectSlug,
			hubHasGlobal: true,
			wantID:       hubGlobalProjectID,
			wantGlobal:   true,
		},
		{
			name:             "--project <dir> beats env",
			flagIsProjectDir: true,
			envID:            envProjectID, envSlug: envProjectSlug,
			wantID: fileProjectID,
		},
		{
			name:  "--project <hub slug> beats env",
			flag:  "other-project",
			envID: envProjectID, envSlug: envProjectSlug,
			wantID: hubOtherProjectID,
			// IsGlobal describes the fallback settings path used for a hub
			// project reference (here the global dir), not the target.
			wantGlobal: true,
		},
		// --- no flag: environment, then .scion, then global ---
		{
			name:         "no flag, in project, env wins in hub container",
			cwdInProject: true,
			envID:        envProjectID, envSlug: envProjectSlug,
			wantID: envProjectID,
		},
		{
			name:            "no flag, no project, env wins over global",
			globalProjectID: globalLocalID,
			envID:           envProjectID, envSlug: envProjectSlug,
			wantID:     envProjectID,
			wantGlobal: true,
		},
		{
			// A resolved directory is not a flag: the env still wins.
			name:            "no flag, resolved cwd dir passed, env wins",
			cwdInProject:    true,
			passResolvedCwd: true,
			envID:           envProjectID, envSlug: envProjectSlug,
			wantID: envProjectID,
		},
		{
			name:         "no flag, in project, no env project",
			cwdInProject: true,
			wantID:       fileProjectID,
		},
		{
			name:            "no flag, no project, no env project",
			globalProjectID: globalLocalID,
			wantID:          globalLocalID,
			wantGlobal:      true,
		},
		{
			// Only an explicit global target looks up the hub Global
			// project; without a flag the ID stays empty.
			name:               "no flag, no project ID anywhere, hub has Global",
			hubHasGlobal:       true,
			wantID:             "",
			wantGlobal:         true,
			wantNoGlobalLookup: true,
		},
		// --- ptone/scion#3124: -g global with no project ID anywhere ---
		{
			name:         "-g global, no env project, hub Global project",
			flag:         "global",
			hubHasGlobal: true,
			wantID:       hubGlobalProjectID,
			wantGlobal:   true,
		},
		{
			name:         "-g home, no env project, hub Global project",
			flag:         "home",
			hubHasGlobal: true,
			wantID:       hubGlobalProjectID,
			wantGlobal:   true,
		},
		{
			name:    "-g global, no env project, hub has no Global project",
			flag:    "global",
			wantErr: `or you do not have access to it`,
		},
		{
			// Only the slug identifies the Global project; a project
			// merely named "Global" must not be picked.
			name:              "-g global, hub has only a project named Global",
			flag:              "global",
			hubHasNamedGlobal: true,
			wantErr:           `no project with slug "global"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var globalLookups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/healthz":
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				case "/api/v1/projects":
					var projects []hubclient.Project
					switch q := r.URL.Query(); {
					case q.Get("slug") == "global" && tc.hubHasGlobal:
						globalLookups.Add(1)
						projects = append(projects, hubclient.Project{ID: hubGlobalProjectID, Name: "Global", Slug: "global"})
					case q.Get("name") != "" && tc.hubHasNamedGlobal:
						projects = append(projects, hubclient.Project{ID: "hub-named-global-id", Name: "Global", Slug: "global-team"})
					case q.Get("slug") == "other-project":
						projects = append(projects, hubclient.Project{ID: hubOtherProjectID, Name: "Other", Slug: "other-project"})
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"projects":   projects,
						"totalCount": len(projects),
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			tmpHome := t.TempDir()
			globalDir := filepath.Join(tmpHome, ".scion")
			if err := os.MkdirAll(globalDir, 0755); err != nil {
				t.Fatal(err)
			}
			settings := fmt.Sprintf("hub:\n  enabled: true\n  endpoint: %s\n", server.URL)
			if tc.globalProjectID != "" {
				settings = "project_id: " + tc.globalProjectID + "\n" + settings
			}
			if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0644); err != nil {
				t.Fatal(err)
			}

			// A git-style project: .scion/project-id holds the identity.
			projectRoot := filepath.Join(t.TempDir(), "proj")
			projectDotScion := filepath.Join(projectRoot, ".scion")
			if err := os.MkdirAll(projectDotScion, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDotScion, "project-id"), []byte(fileProjectID+"\n"), 0644); err != nil {
				t.Fatal(err)
			}

			t.Setenv("HOME", tmpHome)
			// Hub-connected container shape: the endpoint comes from env.
			setOrUnsetEnv(t, "SCION_HUB_ENDPOINT", server.URL)
			setOrUnsetEnv(t, "SCION_HUB_URL", "")
			setOrUnsetEnv(t, "SCION_HUB_PROJECT_ID", "")
			setOrUnsetEnv(t, "SCION_PROJECT_ID", tc.envID)
			setOrUnsetEnv(t, "SCION_PROJECT", tc.envSlug)
			setOrUnsetEnv(t, "SCION_DEV_TOKEN", "test-dev-token")
			setOrUnsetEnv(t, "SCION_AUTH_TOKEN", "")

			cwd := tmpHome
			if tc.cwdInProject {
				cwd = projectRoot
			}
			t.Chdir(cwd)

			flag := tc.flag
			if tc.flagIsProjectDir {
				flag = projectRoot
			}
			explicit := flag != ""
			if tc.passResolvedCwd {
				resolved, err := config.GetResolvedProjectDir("")
				if err != nil {
					t.Fatal(err)
				}
				flag = resolved
			}

			hubCtx, err := EnsureHubReady(flag, EnsureHubReadyOptions{AutoConfirm: true, SkipSync: true, ExplicitProject: explicit})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("EnsureHubReady(%q) error = %v, want containing %q", flag, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureHubReady(%q) error: %v", flag, err)
			}
			if hubCtx == nil {
				t.Fatalf("EnsureHubReady(%q) returned nil hub context", flag)
			}
			if hubCtx.ProjectID != tc.wantID {
				t.Errorf("EnsureHubReady(%q).ProjectID = %q, want %q", flag, hubCtx.ProjectID, tc.wantID)
			}
			if hubCtx.IsGlobal != tc.wantGlobal {
				t.Errorf("EnsureHubReady(%q).IsGlobal = %v, want %v", flag, hubCtx.IsGlobal, tc.wantGlobal)
			}
			if n := globalLookups.Load(); tc.wantNoGlobalLookup && n != 0 {
				t.Errorf("EnsureHubReady(%q) looked up the hub Global project %d time(s), want none", flag, n)
			}
		})
	}
}
