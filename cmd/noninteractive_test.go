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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkTestHub is a fake hub for runHubLink. The project lookup by ID answers
// 404, as it does for a project the caller cannot read; nameMatches are
// returned by the name search.
type linkTestHub struct {
	mu          sync.Mutex
	nameMatches []map[string]interface{}
	registers   int
	lists       int
}

func (h *linkTestHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			h.registers++
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": body["id"], "name": body["name"], "slug": body["name"]},
			})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			h.lists++
			projects := []interface{}{}
			for _, m := range h.nameMatches {
				projects = append(projects, m)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": projects})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	})
}

// setupLinkTest prepares a global project linked to nothing, a fake hub,
// and restores the CLI flags it touches.
func setupLinkTest(t *testing.T, hub *linkTestHub) {
	t.Helper()
	origProjectPath, origYes, origNonInteractive, origGlobal, origFormat := projectPath, autoConfirm, nonInteractive, globalMode, outputFormat
	origOffer := offerTemplateSyncOnLinkFn
	t.Cleanup(func() {
		projectPath, autoConfirm, nonInteractive, globalMode, outputFormat = origProjectPath, origYes, origNonInteractive, origGlobal, origFormat
		offerTemplateSyncOnLinkFn = origOffer
	})
	offerTemplateSyncOnLinkFn = func(string, string, string, bool) {}

	server := httptest.NewServer(hub.handler())
	t.Cleanup(server.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	for _, e := range []string{"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_PROJECT_PATH", "SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		t.Setenv(e, "")
		_ = os.Unsetenv(e)
	}

	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	settings, err := json.Marshal(map[string]interface{}{
		"project_id": "11111111-2222-3333-4444-555555555555",
		"hub":        map[string]interface{}{"enabled": true, "endpoint": server.URL},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.json"), settings, 0644))
	require.NoError(t, config.UpdateSetting(globalDir, "project_id", "11111111-2222-3333-4444-555555555555", true))

	t.Chdir(home)
	projectPath, globalMode, autoConfirm, nonInteractive, outputFormat = "", true, true, false, ""
}

// A user access token without project:read gets 404 on the project lookup.
// hub link must say which scope is likely missing and must not fall back to
// name matching or registration (ptone/scion#3319).
func TestRunHubLink_ScopedTokenWithoutProjectRead(t *testing.T) {
	hub := &linkTestHub{nameMatches: []map[string]interface{}{{"id": "hub-id", "name": "global", "slug": "global"}}}
	setupLinkTest(t, hub)
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_testtoken")

	err := runHubLink(hubLinkCmd, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubsync.ErrScopedTokenProjectNotFound), "error = %v", err)
	assert.Contains(t, err.Error(), "project:read")
	assert.NotContains(t, err.Error(), "Linked to existing project")

	hub.mu.Lock()
	defer hub.mu.Unlock()
	assert.Zero(t, hub.registers, "no register call")
	assert.Zero(t, hub.lists, "no name matching")
}

// Under --yes with --format json, the link confirmation, the auto-link
// note and the status lines go to stderr; stdout holds only the JSON result.
func TestRunHubLink_JSONStdoutParseableWithAutoConfirm(t *testing.T) {
	hub := &linkTestHub{nameMatches: []map[string]interface{}{{"id": "hub-id", "name": "global", "slug": "global"}}}
	setupLinkTest(t, hub)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	outputFormat = "json"

	var runErr error
	stdout := captureStdout(t, func() { runErr = runHubLink(hubLinkCmd, nil) })
	require.NoError(t, runErr)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), "stdout is not JSON: %q", stdout)
	assert.Equal(t, "success", result["status"])
	assert.NotContains(t, stdout, "auto-confirmed")
	assert.NotContains(t, stdout, "Auto-linking")
}

// --non-interactive must not pick one of several matching projects.
func TestRunHubLink_NonInteractiveAmbiguousProject(t *testing.T) {
	hub := &linkTestHub{nameMatches: []map[string]interface{}{
		{"id": "hub-id-1", "name": "global", "slug": "global"},
		{"id": "hub-id-2", "name": "global", "slug": "global-2"},
	}}
	setupLinkTest(t, hub)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	nonInteractive = true

	err := runHubLink(hubLinkCmd, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubsync.ErrAmbiguousProject), "error = %v", err)

	hub.mu.Lock()
	defer hub.mu.Unlock()
	assert.Zero(t, hub.registers, "no register call")
}

// NO_COLOR on a terminal is covered in pkg/util (TestColorForHonoursNoColorOnTerminal):
// under go test stderr is never a terminal, so a NO_COLOR test here could not fail.
func TestFormatCLIError_PlainWhenNotTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	// The error text itself may carry colour (the agent-container banner).
	got := formatCLIError(f, errors.New(util.Yellow+"boom"+util.Reset))
	assert.Equal(t, "\nError: boom\n\n", got)
}

func TestPromptChoice_NoTerminalErrors(t *testing.T) {
	origYes, origTTY := autoConfirm, isInteractiveTerminal
	t.Cleanup(func() { autoConfirm, isInteractiveTerminal = origYes, origTTY })
	autoConfirm = false
	isInteractiveTerminal = func() bool { return false }

	_, err := promptChoice("Choice", "U", []string{"U", "C"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	assert.Contains(t, err.Error(), "not a terminal")
}

func TestRecoverPromptConfirmation_NonTerminalFileAnswersNo(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	orig := recoverConfirmReader
	t.Cleanup(func() { recoverConfirmReader = orig })
	recoverConfirmReader = r // idle open pipe: a read would block

	var out strings.Builder
	assert.False(t, recoverPromptConfirmation(&out, "Disable constraint?"))
	assert.Contains(t, out.String(), "not a terminal")
}
