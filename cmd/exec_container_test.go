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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupExecContainerTestProject(t *testing.T, endpoint, projID string) (string, func()) {
	t.Helper()
	tmpHome := t.TempDir()
	projDir := filepath.Join(tmpHome, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projDir, 0755))

	settings := map[string]any{
		"project_id": projID,
		"hub": map[string]any{
			"enabled":    true,
			"endpoint":   endpoint,
			"project_id": projID,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projDir, "settings.json"), data, 0644))

	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	useHubForTest(t, endpoint)

	origProjectPath := projectPath
	origOutputFormat := outputFormat
	projectPath = projDir
	outputFormat = ""

	cleanup := func() {
		projectPath = origProjectPath
		outputFormat = origOutputFormat
		_ = os.Setenv("HOME", origHome)
	}
	t.Cleanup(cleanup)
	return projDir, cleanup
}

// TestExecContainerCLI_ShowSetClear verifies AC 6:
// `scion exec-container show`, `set <agent>`, and `clear` work against a Hub,
// and `set` resolves <agent> locally in the current project to its UUID before
// calling PUT /api/v1/users/me/exec-container.
func TestExecContainerCLI_ShowSetClear(t *testing.T) {
	const (
		projID  = "proj-exec-1"
		agentID = "11111111-2222-3333-4444-555555555555"
	)

	var registeredID string
	var lastPutBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+projID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   projID,
				"name": "Demo Project",
				"slug": "demo-project",
			})

		case r.URL.Path == "/api/v1/projects/"+projID+"/agents" && r.Method == http.MethodGet:
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"agents": []map[string]any{
						{
							"id":        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
							"slug":      "other-agent",
							"name":      "other-agent",
							"projectId": projID,
							"phase":     "running",
							"activity":  "working",
						},
					},
					"nextCursor": "page-2",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{
						"id":        agentID,
						"slug":      "ops-shell",
						"name":      "ops-shell",
						"projectId": projID,
						"phase":     "running",
						"activity":  "idle",
					},
				},
			})

		case r.URL.Path == "/api/v1/users/me/exec-container" && r.Method == http.MethodGet:
			if registeredID == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"configured": false})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"configured": true,
				"state":      "ready",
				"agent": map[string]any{
					"id":          registeredID,
					"slug":        "ops-shell",
					"name":        "ops-shell",
					"projectId":   projID,
					"projectName": "Demo Project",
					"phase":       "running",
					"activity":    "idle",
				},
			})

		case r.URL.Path == "/api/v1/users/me/exec-container" && r.Method == http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&lastPutBody))
			registeredID = lastPutBody["agentId"]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"configured": true,
				"state":      "ready",
				"agent": map[string]any{
					"id":          registeredID,
					"slug":        "ops-shell",
					"name":        "ops-shell",
					"projectId":   projID,
					"projectName": "Demo Project",
					"phase":       "running",
					"activity":    "idle",
				},
			})

		case r.URL.Path == "/api/v1/users/me/exec-container" && r.Method == http.MethodDelete:
			registeredID = ""
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "unexpected route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	setupExecContainerTestProject(t, srv.URL, projID)

	// 1. show when unconfigured
	var out bytes.Buffer
	execContainerShowCmd.SetOut(&out)
	require.NoError(t, runExecContainerShow(execContainerShowCmd, nil))
	assert.Contains(t, out.String(), "Not configured")

	// 2. set with unknown agent name fails locally without calling PUT
	err := runExecContainerSet(execContainerSetCmd, []string{"no-such-agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in current project")
	assert.Nil(t, lastPutBody)

	// 3. set with local agent name resolves to UUID and PUTs it
	out.Reset()
	execContainerSetCmd.SetOut(&out)
	require.NoError(t, runExecContainerSet(execContainerSetCmd, []string{"ops-shell"}))
	assert.Equal(t, agentID, lastPutBody["agentId"])
	assert.Contains(t, out.String(), "ready")
	assert.Contains(t, out.String(), "ops-shell")

	// 4. show reflects registered container
	out.Reset()
	execContainerShowCmd.SetOut(&out)
	require.NoError(t, runExecContainerShow(execContainerShowCmd, nil))
	assert.Contains(t, out.String(), "ready")
	assert.Contains(t, out.String(), agentID)
	assert.Contains(t, out.String(), "Demo Project")

	// 5. clear deletes registration
	out.Reset()
	execContainerClearCmd.SetOut(&out)
	require.NoError(t, runExecContainerClear(execContainerClearCmd, nil))
	assert.Contains(t, out.String(), "cleared")
	assert.Empty(t, registeredID)
}

// TestExecContainerCLI_RefusedInAgentModeAndWindows verifies AC 6 & D4:
// `scion exec-container` is absent from agentAllowed (refused in agent mode)
// and absent from windowsAllowed (hidden and refused on Windows).
func TestExecContainerCLI_RefusedInAgentModeAndWindows(t *testing.T) {
	assert.False(t, agentAllowed["exec-container"])
	assert.False(t, agentAllowed["exec-container.show"])
	assert.False(t, agentAllowed["exec-container.set"])
	assert.False(t, agentAllowed["exec-container.clear"])

	assert.False(t, windowsAllowed["exec-container"])
	assert.False(t, windowsAllowed["exec-container.show"])
	assert.False(t, windowsAllowed["exec-container.set"])
	assert.False(t, windowsAllowed["exec-container.clear"])

	// Verify applyModeRestrictions strips exec-container in agent mode.
	fakeRoot := &cobra.Command{Use: "scion"}
	ec := &cobra.Command{Use: "exec-container"}
	ec.AddCommand(&cobra.Command{Use: "show", RunE: func(*cobra.Command, []string) error { return nil }})
	ec.AddCommand(&cobra.Command{Use: "set", RunE: func(*cobra.Command, []string) error { return nil }})
	ec.AddCommand(&cobra.Command{Use: "clear", RunE: func(*cobra.Command, []string) error { return nil }})
	fakeRoot.AddCommand(ec)

	applyModeRestrictions(fakeRoot, ModeAgent)
	for _, c := range fakeRoot.Commands() {
		assert.NotEqual(t, "exec-container", c.Name(), "exec-container must be removed in agent mode")
	}

	// Verify applyPlatformRestrictions gates exec-container on Windows.
	winRoot := &cobra.Command{Use: "scion"}
	winEC := &cobra.Command{Use: "exec-container"}
	winShow := &cobra.Command{Use: "show", RunE: func(*cobra.Command, []string) error { return nil }}
	winEC.AddCommand(winShow)
	winRoot.AddCommand(winEC)

	applyPlatformRestrictions(winRoot, "windows")
	assert.True(t, winEC.Hidden)
	assert.ErrorIs(t, winShow.RunE(winShow, nil), errUnsupportedOnWindows)
}

// TestResolveLocalProjectAgentID_PrecedenceAndAmbiguity verifies O5:
// resolution prefers exact ID, then slug, then name, and errors on ambiguous names.
func TestResolveLocalProjectAgentID_PrecedenceAndAmbiguity(t *testing.T) {
	const (
		projID  = "proj-prec-1"
		idSlug  = "11111111-1111-1111-1111-111111111111"
		idName1 = "22222222-2222-2222-2222-222222222222"
		idName2 = "33333333-3333-3333-3333-333333333333"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/"+projID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": projID, "name": "Prec", "slug": "prec"})
		case r.URL.Path == "/api/v1/projects/"+projID+"/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{"id": idName1, "slug": "ops-1", "name": "ops"},
					{"id": idSlug, "slug": "ops", "name": "different-name"},
					{"id": idName2, "slug": "dup-2", "name": "dup"},
					{"id": "44444444-4444-4444-4444-444444444444", "slug": "dup-1", "name": "dup"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	setupExecContainerTestProject(t, srv.URL, projID)
	client, _, err := requireHubProjectClient()
	require.NoError(t, err)

	// Exact ID match wins.
	got, err := resolveLocalProjectAgentID(t.Context(), client, projID, idName1)
	require.NoError(t, err)
	assert.Equal(t, idName1, got)

	// Slug match beats Name match ("ops" matches slug of idSlug even though idName1 appears first with name "ops").
	got, err = resolveLocalProjectAgentID(t.Context(), client, projID, "ops")
	require.NoError(t, err)
	assert.Equal(t, idSlug, got)

	// Ambiguous name returns an error instead of picking arbitrarily.
	_, err = resolveLocalProjectAgentID(t.Context(), client, projID, "dup")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}
