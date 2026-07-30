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
	"strings"
	"sync/atomic"
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
			"skillUri":  "skill://user-skill-one",
			"optional":  false,
			"sortOrder": 0,
		},
		{
			"id":        testUserEntryID2,
			"skillUri":  "skill://user-skill-two",
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

	// scion:// is explicitly rejected by NormalizeSkillURI before the hub is contacted.
	// Note: bare skill names are valid per AC #6 (e.g. "my-skill"); use a genuinely
	// invalid URI instead.
	err := runUserSkillsAdd(userSkillsAddCmd, []string{"scion://forbidden"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scion://", "error should identify the rejected scheme")
	assert.False(t, hubCalled, "hub must not be contacted when NormalizeSkillURI rejects the URI")
}

func TestRunUserSkillsAdd_BareSkillName(t *testing.T) {
	// Bare skill names (no "://") are valid since #866: NormalizeSkillURI accepts them.
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

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"bare-skill"})
	assert.NoError(t, err)
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

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"skill://scion/new-user-skill"})
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

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"skill://scion/new-user-skill"})
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
	err := runUserSkillsRemove(userSkillsRemoveCmd, []string{"skill://user-skill-one"})
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

	err := runUserSkillsRemove(userSkillsRemoveCmd, []string{"skill://nonexistent"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no injected skill with URI")
}

// =============================================================================
// --from-directory tests
// =============================================================================

func TestUserSkillsAddCmd_FromDirectoryFlag(t *testing.T) {
	f := userSkillsAddCmd.Flags().Lookup("from-directory")
	assert.NotNil(t, f, "add command should have --from-directory flag")
	assert.Equal(t, "string", f.Value.Type())
}

// newUserFromDirMockServer returns a mock hub that handles discover-directory
// and user injected-skills add endpoints.
func newUserFromDirMockServer(t *testing.T, skills []map[string]interface{}, addCalls *atomic.Int32, failSecondAdd bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			// Verify no projectId for user scope.
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if _, ok := req["projectId"]; ok && req["projectId"] != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code":    "bad_request",
					"message": "unexpected projectId in user-scope discover",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPost:
			n := addCalls.Add(1)
			if failSecondAdd && n == 2 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code":    "bad_request",
					"message": "skill already exists",
				})
				return
			}
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "new-user-entry-uuid",
				"skillUri": req["skillUri"],
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "not_found",
				"message": "not found",
			})
		}
	}))
}

func TestRunUserSkillsFromDirectory_NonTTY_AddsAll(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newUserFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var buf bytes.Buffer
	userSkillsAddCmd.SetOut(&buf)
	defer userSkillsAddCmd.SetOut(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(2), addCalls.Load())
	assert.Contains(t, buf.String(), "Added 2 of 2")
}

func TestRunUserSkillsFromDirectory_TTY_Abort(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
	}
	var addCalls atomic.Int32
	server := newUserFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return true }

	origAutoConfirm := autoConfirm
	defer func() { autoConfirm = origAutoConfirm }()
	autoConfirm = false

	var buf bytes.Buffer
	userSkillsAddCmd.SetOut(&buf)
	defer userSkillsAddCmd.SetOut(nil)
	userSkillsAddCmd.SetIn(strings.NewReader("n\n"))
	defer userSkillsAddCmd.SetIn(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(0), addCalls.Load())
	assert.Contains(t, buf.String(), "Aborted")
}

func TestRunUserSkillsFromDirectory_NoSkills(t *testing.T) {
	skills := []map[string]interface{}{}
	var addCalls atomic.Int32
	server := newUserFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	var buf bytes.Buffer
	userSkillsAddCmd.SetOut(&buf)
	defer userSkillsAddCmd.SetOut(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/empty")
	assert.NoError(t, err)
	assert.Equal(t, int32(0), addCalls.Load())
	assert.Contains(t, buf.String(), "No skills found")
}

func TestRunUserSkillsFromDirectory_PartialFailure(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newUserFromDirMockServer(t, skills, &addCalls, true)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	userSkillsAddCmd.SetOut(&outBuf)
	userSkillsAddCmd.SetErr(&errBuf)
	defer userSkillsAddCmd.SetOut(nil)
	defer userSkillsAddCmd.SetErr(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Contains(t, outBuf.String(), "Added 1 of 2")
	assert.Contains(t, errBuf.String(), "Warning: 1 of 2 skills could not be added")
}

func TestRunUserSkillsFromDirectory_TTY_Yes_AddsAll(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newUserFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return true }

	origAutoConfirm := autoConfirm
	defer func() { autoConfirm = origAutoConfirm }()
	autoConfirm = true

	var buf bytes.Buffer
	userSkillsAddCmd.SetOut(&buf)
	defer userSkillsAddCmd.SetOut(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(2), addCalls.Load())
	assert.Contains(t, buf.String(), "Added 2 of 2")
}

func TestRunUserSkillsFromDirectory_DiscoverError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case "/api/v1/skills/discover-directory":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "internal_error",
				"message": "internal server error",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "skill discovery failed")
}

func TestRunUserSkillsFromDirectory_StripsUserinfo(t *testing.T) {
	var receivedSourceURL string
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			receivedSourceURL, _ = req["sourceUrl"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "new-user-entry-uuid",
				"skillUri": req["skillUri"],
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var buf bytes.Buffer
	userSkillsAddCmd.SetOut(&buf)
	defer userSkillsAddCmd.SetOut(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd,
		"https://token:secret@github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.NotContains(t, receivedSourceURL, "token")
	assert.NotContains(t, receivedSourceURL, "secret")
	assert.Contains(t, receivedSourceURL, "github.com/org/repo/tree/main/skills")
}

func TestRunUserSkillsFromDirectory_InvalidURL(t *testing.T) {
	err := runUserSkillsFromDirectory(userSkillsAddCmd, "ftp://example.com/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--from-directory must be an https://github.com/")
}

func TestRunUserSkillsAdd_FromDirConflictWithAs(t *testing.T) {
	origFromDir := userSkillsFromDir
	origAs := userSkillsAs
	defer func() {
		userSkillsFromDir = origFromDir
		userSkillsAs = origAs
	}()
	userSkillsFromDir = "https://github.com/org/repo/tree/main/skills"
	userSkillsAs = "my-alias"

	err := runUserSkillsAdd(userSkillsAddCmd, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--as and --optional cannot be used with --from-directory")
}

func TestRunUserSkillsAdd_FromDirConflictWithSkillURI(t *testing.T) {
	origFromDir := userSkillsFromDir
	defer func() { userSkillsFromDir = origFromDir }()
	userSkillsFromDir = "https://github.com/org/repo/tree/main/skills"

	err := runUserSkillsAdd(userSkillsAddCmd, []string{"skill://foo"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot combine a skill URI argument with --from-directory")
}

func TestRunUserSkillsFromDirectory_TotalFailure(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
	}
	var addCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case r.URL.Path == "/api/v1/users/me/injected-skills" && r.Method == http.MethodPost:
			addCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "bad_request",
				"message": "skill already exists",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setUserSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	_, projectDir := setupUserSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	userSkillsAddCmd.SetOut(&outBuf)
	userSkillsAddCmd.SetErr(&errBuf)
	defer userSkillsAddCmd.SetOut(nil)
	defer userSkillsAddCmd.SetErr(nil)

	err := runUserSkillsFromDirectory(userSkillsAddCmd, "https://github.com/org/repo/tree/main/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "all 1 skill(s) failed to add")
	assert.Equal(t, int32(1), addCalls.Load())
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
