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
// Tests for looksLikeGitHubDirectoryURL
// =============================================================================

func TestLooksLikeGitHubDirectoryURL(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid directory URL", "https://github.com/org/repo/tree/main/skills", true},
		{"valid with nested path", "https://github.com/org/repo/tree/v1.0/skills/sub", true},
		{"valid with uppercase host", "https://GitHub.com/org/repo/tree/main/skills", true},
		{"missing tree segment", "https://github.com/org/repo/blob/main/file.go", false},
		{"not github host", "https://gitlab.com/org/repo/tree/main/skills", false},
		{"http scheme", "http://github.com/org/repo/tree/main/skills", false},
		{"ftp scheme", "ftp://github.com/org/repo/tree/main/skills", false},
		{"bare path", "/home/user/skills", false},
		{"empty string", "", false},
		{"skill URI", "skill://my-skill", false},
		{"invalid URL", "://bad", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, looksLikeGitHubDirectoryURL(c.input))
		})
	}
}

// =============================================================================
// Unit tests for pure helper functions (no network, no Hub)
// =============================================================================

func TestIsSkillURI(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"skill://my-skill", true},
		{"skill://my-skill@1.0", true},
		{"gcs://bucket/path", true},
		{"github://org/repo/skill", true},
		{"", false},
		{"my-project", false},
		{"some-uuid-string", false},
		{"abc123", false},
		{"://", true}, // degenerate but still matches
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, isSkillURI(c.input))
		})
	}
}

func TestSplitProjectSkillsArgs(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantProjectArg string
		wantSkillRef   string
	}{
		{
			name:           "empty",
			args:           []string{},
			wantProjectArg: "",
			wantSkillRef:   "",
		},
		{
			name:           "uri only",
			args:           []string{"skill://my-skill"},
			wantProjectArg: "",
			wantSkillRef:   "skill://my-skill",
		},
		{
			name:           "project name only",
			args:           []string{"my-project"},
			wantProjectArg: "my-project",
			wantSkillRef:   "",
		},
		{
			name:           "project then uri",
			args:           []string{"my-project", "skill://my-skill"},
			wantProjectArg: "my-project",
			wantSkillRef:   "skill://my-skill",
		},
		{
			name:           "project then uuid",
			args:           []string{"my-project", "abc123-uuid"},
			wantProjectArg: "my-project",
			wantSkillRef:   "abc123-uuid",
		},
		{
			name:           "uuid only (treated as skill ref, not project)",
			args:           []string{"11111111-2222-3333-4444-555555555555"},
			wantProjectArg: "",
			wantSkillRef:   "11111111-2222-3333-4444-555555555555",
		},
		{
			name:           "gcs uri",
			args:           []string{"gcs://bucket/path"},
			wantProjectArg: "",
			wantSkillRef:   "gcs://bucket/path",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotProject, gotSkill := splitProjectSkillsArgs(c.args)
			assert.Equal(t, c.wantProjectArg, gotProject)
			assert.Equal(t, c.wantSkillRef, gotSkill)
		})
	}
}

// =============================================================================
// Command registration tests
// =============================================================================

func TestProjectSkillsCmd_IsRegistered(t *testing.T) {
	// Verify projectSkillsCmd is registered under projectCmd.
	found := false
	for _, sub := range projectCmd.Commands() {
		if sub.Use == "skills" {
			found = true
			break
		}
	}
	assert.True(t, found, "projectCmd should have a 'skills' subcommand")
}

func TestProjectSkillsListCmd_IsRegistered(t *testing.T) {
	found := false
	for _, sub := range projectSkillsCmd.Commands() {
		if sub.Use == "list [project]" {
			found = true
			break
		}
	}
	assert.True(t, found, "projectSkillsCmd should have a 'list' subcommand")
}

func TestProjectSkillsAddCmd_Flags(t *testing.T) {
	assert.NotNil(t, projectSkillsAddCmd.Flags().Lookup("as"), "add command should have --as flag")
	assert.NotNil(t, projectSkillsAddCmd.Flags().Lookup("optional"), "add command should have --optional flag")
}

func TestProjectSkillsRemoveCmd_Aliases(t *testing.T) {
	aliases := projectSkillsRemoveCmd.Aliases
	assert.Contains(t, aliases, "rm")
	assert.Contains(t, aliases, "delete")
}

// =============================================================================
// Integration-style tests with a mock HTTP server
// =============================================================================

const testProjectID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const testEntryID1 = "11111111-2222-3333-4444-555555555555"
const testEntryID2 = "66666666-7777-8888-9999-aaaaaaaaaaaa"

// setupProjectSkillsProject creates a temp project dir pointed at the given hub.
func setupProjectSkillsProject(t *testing.T, endpoint string) string {
	t.Helper()
	tmpHome := t.TempDir()
	projectDir := filepath.Join(tmpHome, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settings := map[string]interface{}{
		"project_id": testProjectID,
		"hub": map[string]interface{}{
			"enabled":   true,
			"endpoint":  endpoint,
			"projectId": testProjectID,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	return projectDir
}

// newProjectSkillsMockServer returns a mock hub that handles injected-skills endpoints.
func newProjectSkillsMockServer(t *testing.T) *httptest.Server {
	t.Helper()

	entries := []map[string]interface{}{
		{
			"id":        testEntryID1,
			"skillUri":  "skill://skill-one",
			"skillAs":   "skill1",
			"optional":  false,
			"sortOrder": 0,
			"skillName": "Skill One",
		},
		{
			"id":        testEntryID2,
			"skillUri":  "skill://skill-two",
			"optional":  true,
			"sortOrder": 1,
		},
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+testProjectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   testProjectID,
				"name": "test-project",
				"slug": "test-project",
			})

		case r.URL.Path == "/api/v1/projects/"+testProjectID+"/injected-skills" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})

		case r.URL.Path == "/api/v1/projects/"+testProjectID+"/injected-skills" && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			newEntry := map[string]interface{}{
				"id":       "new-entry-uuid",
				"skillUri": req["skillUri"],
				"optional": req["optional"],
			}
			if v, ok := req["skillAs"]; ok {
				newEntry["skillAs"] = v
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(newEntry)

		case r.URL.Path == "/api/v1/projects/"+testProjectID+"/injected-skills/"+testEntryID1 && r.Method == http.MethodDelete:
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

// setProjectSkillsHubEnv overrides hub endpoint env vars to point at the mock
// server, preventing the real SCION_HUB_ENDPOINT from being used in tests.
func setProjectSkillsHubEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv("SCION_HUB_ENDPOINT", serverURL)
	t.Setenv("SCION_HUB_URL", serverURL)
}

func TestRunProjectSkillsList_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case "/api/v1/projects/" + testProjectID + "/injected-skills":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"entries": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runProjectSkillsList(projectSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunProjectSkillsList_WithEntries(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runProjectSkillsList(projectSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunProjectSkillsList_JSONOutput(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = "json"

	err := runProjectSkillsList(projectSkillsListCmd, nil)
	assert.NoError(t, err)
}

func TestRunProjectSkillsAdd_Success(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()
	origAs := projectSkillsAs
	defer func() { projectSkillsAs = origAs }()
	origOpt := projectSkillsOptional
	defer func() { projectSkillsOptional = origOpt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""
	projectSkillsAs = ""
	projectSkillsOptional = false

	// Single arg: URI only (project inferred from settings)
	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{"skill://scion/new-skill"})
	assert.NoError(t, err)
}

func TestRunProjectSkillsAdd_WithAlias(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()
	origAs := projectSkillsAs
	defer func() { projectSkillsAs = origAs }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""
	projectSkillsAs = "my-alias"
	projectSkillsOptional = false

	// Simulate flag state for the command:
	_ = projectSkillsAddCmd.Flags().Set("as", "my-alias")

	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{"skill://scion/new-skill"})
	assert.NoError(t, err)
}

func TestRunProjectSkillsAdd_NoURIError(t *testing.T) {
	// No skill URI → error (pure logic test, no hub connection needed).
	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{"my-project"})
	// "my-project" is treated as a project name (not a URI), skill ref is empty.
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "skill URI is required (expected format containing ://)") // message quotes the actual arg
}

func TestRunProjectSkillsAdd_NoURIError_TwoArgs(t *testing.T) {
	// Set up a mock hub and record whether it was ever contacted.
	hubCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	// Case 1: invalid scheme — scion:// is explicitly rejected by NormalizeSkillURI.
	// Note: bare skill names are valid per AC #6; use a genuinely bad URI instead.
	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{"my-project", "scion://forbidden"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scion://", "error should quote the offending URI scheme")
	assert.False(t, hubCalled, "hub must not be contacted when NormalizeSkillURI rejects the URI")

	// Case 2: invalid bare name (uppercase) — error quotes the skill, not the project name.
	hubCalled = false
	err = runProjectSkillsAdd(projectSkillsAddCmd, []string{"my-project", "MySkill"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "MySkill", "error should quote the invalid skill name, not the project name")
	assert.False(t, hubCalled, "hub must not be contacted when NormalizeSkillURI rejects the bare name")
}

func TestRunProjectSkillsAdd_BareSkillName(t *testing.T) {
	// Bare skill names (no "://") are valid since #866: NormalizeSkillURI accepts them.
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	// Two-arg form: project UUID + bare name. splitProjectSkillsArgs consumes a lone bare arg
	// as the project name, so bare skill names are only reachable in the 2-arg form.
	// Use the UUID directly so the mock server can resolve it without a search.
	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{testProjectID, "bare-skill"})
	assert.NoError(t, err)
}

func TestRunProjectSkillsRemove_ByID(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	// Remove by UUID directly.
	err := runProjectSkillsRemove(projectSkillsRemoveCmd, []string{testEntryID1})
	assert.NoError(t, err)
}

func TestRunProjectSkillsRemove_ByURI(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	// Remove by URI: should list first, then delete by resolved ID.
	err := runProjectSkillsRemove(projectSkillsRemoveCmd, []string{"skill://skill-one"})
	assert.NoError(t, err)
}

func TestRunProjectSkillsRemove_URINotFound(t *testing.T) {
	server := newProjectSkillsMockServer(t)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runProjectSkillsRemove(projectSkillsRemoveCmd, []string{"skill://nonexistent-skill"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no injected skill with URI")
}

func TestRunProjectSkillsRemove_NoRefError(t *testing.T) {
	// Pure logic test: "my-project" has no "://" → treated as UUID → hub error.
	// Don't bother with mock server; we expect an error about hub not being available.
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	orig := projectPath
	defer func() { projectPath = orig }()
	projectPath = ""
	err := runProjectSkillsRemove(projectSkillsRemoveCmd, []string{"my-project"})
	assert.Error(t, err)
}

func TestRunProjectSkillsList_APIError(t *testing.T) {
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
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	origFmt := outputFormat
	defer func() { outputFormat = origFmt }()

	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir
	outputFormat = ""

	err := runProjectSkillsList(projectSkillsListCmd, nil)
	assert.Error(t, err)
}

func TestPrintSkillInjectionTable_NoOutput(t *testing.T) {
	// Smoke test — should not panic on empty list.
	printSkillInjectionTable(nil)
}

// =============================================================================
// --from-directory tests
// =============================================================================

func TestProjectSkillsAddCmd_FromDirectoryFlag(t *testing.T) {
	f := projectSkillsAddCmd.Flags().Lookup("from-directory")
	assert.NotNil(t, f, "add command should have --from-directory flag")
	assert.Equal(t, "string", f.Value.Type())
}

// newProjectFromDirMockServer returns a mock hub that handles discover-directory
// and injected-skills add endpoints. addCalls tracks how many add POSTs were made.
func newProjectFromDirMockServer(t *testing.T, skills []map[string]interface{}, addCalls *atomic.Int32, failSecondAdd bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+testProjectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   testProjectID,
				"name": "test-project",
				"slug": "test-project",
			})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case strings.HasSuffix(r.URL.Path, "/injected-skills") && r.Method == http.MethodPost:
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
				"id":       "new-entry-uuid",
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

func TestRunProjectSkillsFromDirectory_NonTTY_AddsAll(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newProjectFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var buf bytes.Buffer
	projectSkillsAddCmd.SetOut(&buf)
	defer projectSkillsAddCmd.SetOut(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(2), addCalls.Load())
	assert.Contains(t, buf.String(), "Added 2 of 2")
}

func TestRunProjectSkillsFromDirectory_TTY_Yes_AddsAll(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newProjectFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return true }

	origAutoConfirm := autoConfirm
	defer func() { autoConfirm = origAutoConfirm }()
	autoConfirm = true

	var buf bytes.Buffer
	projectSkillsAddCmd.SetOut(&buf)
	defer projectSkillsAddCmd.SetOut(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(2), addCalls.Load())
	assert.Contains(t, buf.String(), "Added 2 of 2")
}

func TestRunProjectSkillsFromDirectory_TTY_Abort(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
	}
	var addCalls atomic.Int32
	server := newProjectFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return true }

	origAutoConfirm := autoConfirm
	defer func() { autoConfirm = origAutoConfirm }()
	autoConfirm = false

	var buf bytes.Buffer
	projectSkillsAddCmd.SetOut(&buf)
	defer projectSkillsAddCmd.SetOut(nil)
	projectSkillsAddCmd.SetIn(strings.NewReader("n\n"))
	defer projectSkillsAddCmd.SetIn(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Equal(t, int32(0), addCalls.Load())
	assert.Contains(t, buf.String(), "Aborted")
}

func TestRunProjectSkillsFromDirectory_NoSkills(t *testing.T) {
	skills := []map[string]interface{}{}
	var addCalls atomic.Int32
	server := newProjectFromDirMockServer(t, skills, &addCalls, false)
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	var buf bytes.Buffer
	projectSkillsAddCmd.SetOut(&buf)
	defer projectSkillsAddCmd.SetOut(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/empty")
	assert.NoError(t, err)
	assert.Equal(t, int32(0), addCalls.Load())
	assert.Contains(t, buf.String(), "No skills found")
}

func TestRunProjectSkillsFromDirectory_PartialFailure(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	server := newProjectFromDirMockServer(t, skills, &addCalls, true) // second add fails
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	projectSkillsAddCmd.SetOut(&outBuf)
	projectSkillsAddCmd.SetErr(&errBuf)
	defer projectSkillsAddCmd.SetOut(nil)
	defer projectSkillsAddCmd.SetErr(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.Contains(t, outBuf.String(), "Added 1 of 2")
	assert.Contains(t, errBuf.String(), "Warning: 1 of 2 skills could not be added")
}

func TestRunProjectSkillsFromDirectory_DiscoverError(t *testing.T) {
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
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "skill discovery failed")
}

func TestRunProjectSkillsFromDirectory_StripsUserinfo(t *testing.T) {
	var receivedSourceURL string
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+testProjectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   testProjectID,
				"name": "test-project",
				"slug": "test-project",
			})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			receivedSourceURL, _ = req["sourceUrl"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case strings.HasSuffix(r.URL.Path, "/injected-skills") && r.Method == http.MethodPost:
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "new-entry-uuid",
				"skillUri": req["skillUri"],
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var buf bytes.Buffer
	projectSkillsAddCmd.SetOut(&buf)
	defer projectSkillsAddCmd.SetOut(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "",
		"https://token:secret@github.com/org/repo/tree/main/skills")
	assert.NoError(t, err)
	assert.NotContains(t, receivedSourceURL, "token")
	assert.NotContains(t, receivedSourceURL, "secret")
	assert.Contains(t, receivedSourceURL, "github.com/org/repo/tree/main/skills")
}

func TestRunProjectSkillsFromDirectory_InvalidURL(t *testing.T) {
	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "ftp://example.com/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--from-directory must be an https://github.com/")
}

func TestRunProjectSkillsAdd_FromDirConflictWithAs(t *testing.T) {
	origFromDir := projectSkillsFromDir
	origAs := projectSkillsAs
	defer func() {
		projectSkillsFromDir = origFromDir
		projectSkillsAs = origAs
	}()
	projectSkillsFromDir = "https://github.com/org/repo/tree/main/skills"
	projectSkillsAs = "my-alias"

	err := runProjectSkillsAdd(projectSkillsAddCmd, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--as and --optional cannot be used with --from-directory")
}

func TestRunProjectSkillsAdd_FromDirConflictWithOptional(t *testing.T) {
	origFromDir := projectSkillsFromDir
	origOpt := projectSkillsOptional
	defer func() {
		projectSkillsFromDir = origFromDir
		projectSkillsOptional = origOpt
	}()
	projectSkillsFromDir = "https://github.com/org/repo/tree/main/skills"
	projectSkillsOptional = true

	err := runProjectSkillsAdd(projectSkillsAddCmd, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--as and --optional cannot be used with --from-directory")
}

func TestRunProjectSkillsAdd_FromDirConflictWithSkillURI(t *testing.T) {
	origFromDir := projectSkillsFromDir
	defer func() { projectSkillsFromDir = origFromDir }()
	projectSkillsFromDir = "https://github.com/org/repo/tree/main/skills"

	err := runProjectSkillsAdd(projectSkillsAddCmd, []string{"skill://foo"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot combine a skill URI argument with --from-directory")
}

func TestRunProjectSkillsFromDirectory_TotalFailure(t *testing.T) {
	skills := []map[string]interface{}{
		{"uri": "skill://org/skill-a", "name": "skill-a"},
		{"uri": "skill://org/skill-b", "name": "skill-b"},
	}
	var addCalls atomic.Int32
	// Server that fails ALL add calls.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+testProjectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   testProjectID,
				"name": "test-project",
				"slug": "test-project",
			})

		case r.URL.Path == "/api/v1/skills/discover-directory" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"skills":  skills,
				"count":   len(skills),
				"skipped": []string{},
			})

		case strings.HasSuffix(r.URL.Path, "/injected-skills") && r.Method == http.MethodPost:
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
	setProjectSkillsHubEnv(t, server.URL)

	orig := projectPath
	defer func() { projectPath = orig }()
	projectDir := setupProjectSkillsProject(t, server.URL)
	projectPath = projectDir

	origInteractive := isInteractiveTerminal
	defer func() { isInteractiveTerminal = origInteractive }()
	isInteractiveTerminal = func() bool { return false }

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	projectSkillsAddCmd.SetOut(&outBuf)
	projectSkillsAddCmd.SetErr(&errBuf)
	defer projectSkillsAddCmd.SetOut(nil)
	defer projectSkillsAddCmd.SetErr(nil)

	err := runProjectSkillsFromDirectory(projectSkillsAddCmd, "", "https://github.com/org/repo/tree/main/skills")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "all 2 skill(s) failed to add")
	assert.Equal(t, int32(2), addCalls.Load())
}
