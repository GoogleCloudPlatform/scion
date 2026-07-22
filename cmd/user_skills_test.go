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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Command registration tests
// =============================================================================

func TestUserCmd_IsRegistered(t *testing.T) {
	found := false
	for _, sub := range rootCmd.Commands() {
		if sub.Use == "user" {
			found = true
			break
		}
	}
	assert.True(t, found, "rootCmd should have a 'user' subcommand")
}

func TestUserSkillsCmd_IsRegistered(t *testing.T) {
	found := false
	for _, sub := range userCmd.Commands() {
		if sub.Use == "skills" {
			found = true
			break
		}
	}
	assert.True(t, found, "userCmd should have a 'skills' subcommand")
}

func TestUserSkillsListCmd_IsRegistered(t *testing.T) {
	found := false
	for _, sub := range userSkillsCmd.Commands() {
		if sub.Use == "list" {
			found = true
			break
		}
	}
	assert.True(t, found, "userSkillsCmd should have a 'list' subcommand")
}

func TestUserSkillsAddCmd_Flags(t *testing.T) {
	assert.NotNil(t, userSkillsAddCmd.Flags().Lookup("as"), "add command should have --as flag")
	assert.NotNil(t, userSkillsAddCmd.Flags().Lookup("optional"), "add command should have --optional flag")
}

func TestUserSkillsRemoveCmd_Aliases(t *testing.T) {
	aliases := userSkillsRemoveCmd.Aliases
	assert.Contains(t, aliases, "rm")
	assert.Contains(t, aliases, "delete")
}

// =============================================================================
// Shared test helpers for user skills
// =============================================================================

const testUserEntryID1 = "aaaabbbb-cccc-dddd-eeee-ffffffffffff"
const testUserEntryID2 = "bbbbcccc-dddd-eeee-ffff-000000000000"

// setupUserSkillsProject creates a temp home and project dir pointed at a mock hub.
func setupUserSkillsProject(t *testing.T, endpoint string) (tmpHome, projectDir string) {
	t.Helper()
	tmpHome = t.TempDir()
	projectDir = filepath.Join(tmpHome, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settings := map[string]interface{}{
		"project_id": "test-project",
		"hub": map[string]interface{}{
			"enabled":  true,
			"endpoint": endpoint,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	return tmpHome, projectDir
}

// newUserSkillsMockServer returns a mock hub that handles user injected-skills endpoints.
func newUserSkillsMockServer(t *testing.T) *httptest.Server {
	t.Helper()

	entries := []map[string]interface{}{
		{
			"id":        testUserEntryID1,
			"skillUri":  "scion://user-skill-one",
			"optional":  false,
			"sortOrder": 0,
		},
		{
			"id":        testUserEntryID2,
			"skillUri":  "scion://user-skill-two",
			"skillAs":   "skill2",
			"optional":  true,
			"sortOrder": 1,
			"skillName": "User Skill Two",
			"skillSlug": "user-skill-two",
		},
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			newEntry := map[string]interface{}{
				"id":       "new-user-entry-uuid",
				"skillUri": req["skillUri"],
				"optional": req["optional"],
			}
			if v, ok := req["skillAs"]; ok {
				newEntry["skillAs"] = v
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(newEntry)

		case r.URL.Path == "/api/v1/users/me/injected-skills/"+testUserEntryID1 && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "not_found",
				"message": "not found",
			})
		}
	}))
}

// setUserSkillsHubEnv overrides hub endpoint env vars to point at the mock
// server, preventing the real SCION_HUB_ENDPOINT from being used in tests.
func setUserSkillsHubEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv("SCION_HUB_ENDPOINT", serverURL)
	t.Setenv("SCION_HUB_URL", serverURL)
}

// =============================================================================
// Integration-style tests with mock HTTP server
// =============================================================================

func TestRunUserSkillsList_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case "/api/v1/users/me/injected-skills":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runUserSkillsList(userSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunUserSkillsList_WithEntries(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runUserSkillsList(userSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunUserSkillsList_JSONOutput(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = "json"

	err := runUserSkillsList(userSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunUserSkillsAdd_NoURIError(t *testing.T) {
	// Set up a mock hub and record whether it was ever contacted.
	hubCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	// Plain string (no "://") → validation error before hub is contacted.
	err := runUserSkillsAdd(userSkillsAddCmd, []string{"mypkg"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "skill URI is required (expected format containing ://)")
	assert.False(t, hubCalled, "hub must not be contacted when the argument is not a skill URI")
}

func TestRunUserSkillsAdd_Success(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()
	origAs := userSkillsAs
	defer func() { userSkillsAs = origAs }()
	origOpt := userSkillsOptional
	defer func() { userSkillsOptional = origOpt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""
	userSkillsAs = ""
	userSkillsOptional = false

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"scion://new-user-skill"})
	assert.NoError(t, err)
}

func TestRunUserSkillsAdd_WithAliasAndOptional(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	// Save and restore --as flag so this test does not pollute others.
	prevAs, _ := userSkillsAddCmd.Flags().GetString("as")
	t.Cleanup(func() { _ = userSkillsAddCmd.Flags().Set("as", prevAs) })
	_ = userSkillsAddCmd.Flags().Set("as", "my-alias")

	// Save and restore --optional flag.
	prevOpt, _ := userSkillsAddCmd.Flags().GetBool("optional")
	prevOptStr := "false"
	if prevOpt {
		prevOptStr = "true"
	}
	t.Cleanup(func() { _ = userSkillsAddCmd.Flags().Set("optional", prevOptStr) })
	_ = userSkillsAddCmd.Flags().Set("optional", "true")

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"scion://new-user-skill"})
	assert.NoError(t, err)
}

func TestRunUserSkillsRemove_ByID(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runUserSkillsRemove(userSkillsRemoveCmd, []string{testUserEntryID1})
	assert.NoError(t, err)
}

func TestRunUserSkillsRemove_ByURI(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	// Remove by URI — resolves via list first.
	err := runUserSkillsRemove(userSkillsRemoveCmd, []string{"scion://user-skill-one"})
	assert.NoError(t, err)
}

func TestRunUserSkillsRemove_URINotFound(t *testing.T) {
	server := newUserSkillsMockServer(t)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runUserSkillsRemove(userSkillsRemoveCmd, []string{"scion://nonexistent"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no injected skill with URI")
}

func TestRunUserSkillsList_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "internal_error",
				"message": "internal server error",
			})
		}
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runUserSkillsList(userSkillsListCmd, nil)
	assert.Error(t, err)
}
