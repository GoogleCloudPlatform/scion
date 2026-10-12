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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearHubEnvForUnlinkTest clears the hub variables a container may export,
// so the CLI is not treated as running inside a hub-connected agent.
func clearHubEnvForUnlinkTest(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_AGENT_ID",
		"SCION_HUB_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
}

// TestHubUnlink_WritesOnlyLinkedAndProjectID checks that 'scion hub unlink'
// sets hub.linked=false and clears hub.projectId, and leaves every other
// key, hub.enabled included, as it was.
func TestHubUnlink_WritesOnlyLinkedAndProjectID(t *testing.T) {
	clearHubEnvForUnlinkTest(t)
	origProjectPath, origGlobal, origYes := projectPath, globalMode, autoConfirm
	t.Cleanup(func() { projectPath, globalMode, autoConfirm = origProjectPath, origGlobal, origYes })

	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(t.TempDir(), "repo", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(
		"project_id: local-id\nhub:\n  enabled: true\n  linked: true\n  endpoint: https://hub.example.com\n  projectId: hub-id\n"), 0o644))
	t.Chdir(filepath.Dir(projectDir))
	projectPath, globalMode, autoConfirm = projectDir, false, true

	before, err := config.LoadSettingsFromDir(projectDir)
	require.NoError(t, err)

	var runErr error
	stdout := captureStdout(t, func() { runErr = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, runErr)

	after, err := config.LoadSettingsFromDir(projectDir)
	require.NoError(t, err)
	require.NotNil(t, after.Hub)
	require.NotNil(t, after.Hub.Linked)
	assert.False(t, *after.Hub.Linked, "hub.linked is false")
	assert.Empty(t, after.Hub.ProjectID, "hub.projectId is cleared")
	require.NotNil(t, after.Hub.Enabled, "hub.enabled is not removed")
	assert.True(t, *after.Hub.Enabled, "hub.enabled is not touched")
	assert.Equal(t, before.Hub.Endpoint, after.Hub.Endpoint)
	assert.Equal(t, before.ProjectID, after.ProjectID)

	// Every other key is unchanged: compare the two settings with the two
	// written keys set to their new values.
	before.Hub.Linked = after.Hub.Linked
	before.Hub.ProjectID = ""
	assert.Equal(t, before, after)

	assert.Contains(t, stdout, "has been unlinked from the Hub")
	assert.Contains(t, stdout, "manage them with '--project <hub-slug>' or re-link")
	assert.Contains(t, stdout, "scion hub link")
	assert.Contains(t, stdout, "scion hub disable", "local-only mode still exists in this phase")
}

// TestHubUnlink_Gate checks which projects 'scion hub unlink' acts on:
// hub.linked=true, or hub.linked unset with hub mode on (registered before
// the link was recorded). An explicit false, or unset with hub mode off,
// is "not linked".
func TestHubUnlink_Gate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		settings   string
		wantUnlink bool
		wantLine   string
	}{
		{name: "explicitly unlinked", settings: "hub:\n  enabled: true\n  linked: false\n", wantLine: "This project is not linked to the Hub."},
		{name: "hub off, linked unset", settings: "hub:\n  enabled: false\n", wantLine: "This project is not linked to the Hub."},
		{name: "legacy: hub on, linked unset, nothing on a hub", settings: "hub:\n  enabled: true\n", wantUnlink: true,
			wantLine: "Project 'repo' marked as unlinked; agent commands here will refuse until 'scion hub link'."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHubEnvForUnlinkTest(t)
			origProjectPath, origGlobal, origYes := projectPath, globalMode, autoConfirm
			t.Cleanup(func() { projectPath, globalMode, autoConfirm = origProjectPath, origGlobal, origYes })

			t.Setenv("HOME", t.TempDir())
			projectDir := filepath.Join(t.TempDir(), "repo", ".scion")
			require.NoError(t, os.MkdirAll(projectDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(tc.settings), 0o644))
			t.Chdir(filepath.Dir(projectDir))
			projectPath, globalMode, autoConfirm = projectDir, false, true
			before, err := config.LoadSettingsFromDir(projectDir)
			require.NoError(t, err)

			var runErr error
			stdout := captureStdout(t, func() { runErr = runHubUnlink(hubUnlinkCmd, nil) })
			require.NoError(t, runErr)
			assert.Contains(t, stdout, tc.wantLine)

			after, err := config.LoadSettingsFromDir(projectDir)
			require.NoError(t, err)
			if !tc.wantUnlink {
				assert.Equal(t, before, after, "nothing is written")
				return
			}
			require.NotNil(t, after.Hub.Linked)
			assert.False(t, *after.Hub.Linked)
			assert.NotContains(t, stdout, "remain on the Hub", "nothing was registered, so the output does not say so")
		})
	}
}

func TestUnlinkGateOpen(t *testing.T) {
	yes, no := true, false
	assert.True(t, unlinkGateOpen(&yes, false))
	assert.True(t, unlinkGateOpen(&yes, true))
	assert.False(t, unlinkGateOpen(&no, true))
	assert.False(t, unlinkGateOpen(&no, false))
	assert.True(t, unlinkGateOpen(nil, true))
	assert.False(t, unlinkGateOpen(nil, false))
}

// roundTripHub is a local workstation hub for link -> unlink -> link. It
// counts requests so a test can check that a refused command made none.
func roundTripHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	var mu sync.Mutex
	var projectID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		id := projectID
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			projectID, _ = body["id"].(string)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": body["id"], "name": body["name"], "slug": body["name"]},
				"created": id == "",
			})
		case id != "" && r.URL.Path == "/api/v1/projects/"+id && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "name": "global", "slug": "global"})
		case id != "" && r.URL.Path == "/api/v1/projects/"+id+"/agents" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []interface{}{}})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// TestHubUnlink_ListRefusesUntilRelinked runs hub link -> list works ->
// hub unlink -> list refuses with the re-link pointer, without contacting
// the hub or offering the link prompt -> hub link -> list works. It runs on
// the local workstation hub, where the auto-link would otherwise link the
// unlinked project again without asking, and with hub mode enabled in the
// global settings.
func TestHubUnlink_ListRefusesUntilRelinked(t *testing.T) {
	clearHubEnvForUnlinkTest(t)
	globalDir := setupNoEndpointLink(t)
	server, requests := roundTripHub(t)
	require.NoError(t, config.UpdateVersionedSetting(globalDir, "hub.endpoint", server.URL))
	require.NoError(t, config.UpdateSetting(globalDir, "hub.enabled", "true", true))
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })
	outputFormat = "json"

	link := func() {
		t.Helper()
		var err error
		captureStderr(t, func() {
			captureStdout(t, func() { err = runHubLink(hubLinkCmd, nil) })
		})
		require.NoError(t, err, "hub link")
	}
	runList := func() error {
		var err error
		captureStderr(t, func() {
			captureStdout(t, func() { err = listCmd.RunE(listCmd, nil) })
		})
		return err
	}

	// Link (no prompt on the local workstation hub), then list works.
	autoConfirm = false
	link()
	require.NoError(t, runList(), "list after link")

	// Unlink, then list refuses before any hub call.
	// autoConfirm (-y) stays on for the list: it would accept the link
	// prompt if the prompt were offered.
	autoConfirm = true
	var unlinkErr error
	captureStdout(t, func() { unlinkErr = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, unlinkErr)
	before := requests.Load()
	err := runList()
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubsync.ErrProjectUnlinked), "error = %v", err)
	assert.Contains(t, err.Error(), "run 'scion hub link' to re-link")
	assert.Equal(t, before, requests.Load(), "a refused list makes no hub request")
	own, loadErr := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, loadErr)
	require.NotNil(t, own.Hub.Linked)
	assert.False(t, *own.Hub.Linked, "the refusal does not re-link the project")

	// Re-link, then list works again.
	autoConfirm = false
	link()
	require.NoError(t, runList(), "list after re-link")
	own, loadErr = config.LoadSettingsFromDir(globalDir)
	require.NoError(t, loadErr)
	require.NotNil(t, own.Hub.Linked)
	assert.True(t, *own.Hub.Linked)
}

// TestHubUnlink_AfterRegistrationPaths checks that a project linked by the
// hub preflight (not by 'scion hub link') can be unlinked, and that list
// then refuses. Two registration paths: the local workstation hub's
// auto-link, and the link prompt on a remote-style hub accepted with --yes.
func TestHubUnlink_AfterRegistrationPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		devToken string
	}{
		{name: "local auto-link", devToken: "scion_dev_test"},
		{name: "link prompt accepted with --yes", devToken: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHubEnvForUnlinkTest(t)
			globalDir := setupNoEndpointLink(t)
			server, requests := roundTripHub(t)
			require.NoError(t, config.UpdateVersionedSetting(globalDir, "hub.endpoint", server.URL))
			require.NoError(t, config.UpdateSetting(globalDir, "hub.enabled", "true", true))
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			origFormat := outputFormat
			t.Cleanup(func() { outputFormat = origFormat })
			outputFormat = "json"
			autoConfirm = true

			runList := func() error {
				var err error
				captureStderr(t, func() {
					captureStdout(t, func() { err = listCmd.RunE(listCmd, nil) })
				})
				return err
			}

			// The first list registers the project through the preflight,
			// which records hub.linked=true.
			require.NoError(t, runList(), "list registers and works")
			linked := hubsync.ProjectLinkedSetting(globalDir, true)
			require.NotNil(t, linked, "registration records the link")
			assert.True(t, *linked)

			var unlinkErr error
			stdout := captureStdout(t, func() { unlinkErr = runHubUnlink(hubUnlinkCmd, nil) })
			require.NoError(t, unlinkErr)
			assert.Contains(t, stdout, "has been unlinked from the Hub")
			assert.NotContains(t, stdout, "not linked")

			before := requests.Load()
			err := runList()
			require.Error(t, err)
			assert.True(t, errors.Is(err, hubsync.ErrProjectUnlinked), "error = %v", err)
			assert.Equal(t, before, requests.Load(), "a refused list makes no hub request")
		})
	}
}

// TestHubUnlink_SplitStorageGitProject runs hub link -> unlink -> list on a
// git project with split storage, where settings live in the external
// project-configs directory.
func TestHubUnlink_SplitStorageGitProject(t *testing.T) {
	clearHubEnvForUnlinkTest(t)
	origProjectPath, origGlobal, origYes, origFormat := projectPath, globalMode, autoConfirm, outputFormat
	origOffer := offerTemplateSyncOnLinkFn
	t.Cleanup(func() {
		projectPath, globalMode, autoConfirm, outputFormat = origProjectPath, origGlobal, origYes, origFormat
		offerTemplateSyncOnLinkFn = origOffer
	})
	offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644))
	repo := filepath.Join(home, "work", "splitrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitRun(t, repo, "init", "-q")
	scionDir := filepath.Join(repo, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "project-id"), []byte("0b5e6a1c-0000-4000-8000-000000000002\n"), 0o644))
	extDir := config.GetProjectConfigDir(scionDir)
	require.NotEqual(t, scionDir, extDir, "split storage puts settings outside the repo")
	t.Chdir(repo)

	server, requests := roundTripHub(t)
	require.NoError(t, config.UpdateSetting(scionDir, "hub.endpoint", server.URL, false))
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	projectPath, globalMode, outputFormat = "", false, "json"

	runList := func() error {
		var err error
		captureStderr(t, func() {
			captureStdout(t, func() { err = listCmd.RunE(listCmd, nil) })
		})
		return err
	}
	link := func() {
		t.Helper()
		var err error
		captureStderr(t, func() {
			captureStdout(t, func() { err = runHubLink(hubLinkCmd, nil) })
		})
		require.NoError(t, err, "hub link")
	}

	autoConfirm = false
	link()
	require.NoError(t, runList(), "list after link")

	autoConfirm = true
	var unlinkErr error
	captureStdout(t, func() { unlinkErr = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, unlinkErr)
	ext, err := config.LoadSettingsFromDir(extDir)
	require.NoError(t, err)
	require.NotNil(t, ext.Hub.Linked, "unlink writes to the external directory")
	assert.False(t, *ext.Hub.Linked)

	before := requests.Load()
	err = runList()
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubsync.ErrProjectUnlinked), "error = %v", err)
	assert.Equal(t, before, requests.Load(), "a refused list makes no hub request")

	autoConfirm = false
	link()
	require.NoError(t, runList(), "list after re-link")
}
