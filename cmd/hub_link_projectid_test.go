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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runHubLinkQuiet runs 'scion hub link' with its output captured.
func runHubLinkQuiet(t *testing.T) {
	t.Helper()
	var err error
	captureStderr(t, func() {
		captureStdout(t, func() { err = runHubLink(hubLinkCmd, nil) })
	})
	require.NoError(t, err, "hub link")
}

// runHubUnlinkQuiet runs 'scion hub unlink' with its output captured.
func runHubUnlinkQuiet(t *testing.T) {
	t.Helper()
	var err error
	captureStdout(t, func() { err = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, err, "hub unlink")
}

// TestRunHubLink_RelinkWithLinkedGlobalKeepsProjectHubID runs link ->
// unlink -> link on a split-storage git project while the global settings
// are linked to another hub project. The first link registers the project;
// the re-link finds it already linked. Both must write the project's own
// hub.projectId (even though it equals the local project ID), so the
// merged settings resolve to the project and not to the global one.
func TestRunHubLink_RelinkWithLinkedGlobalKeepsProjectHubID(t *testing.T) {
	const localID = "0b5e6a1c-0000-4000-8000-0000000000b1"
	clearHubEnvForUnlinkTest(t)
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	origProjectPath, origGlobal, origYes, origFormat := projectPath, globalMode, autoConfirm, outputFormat
	origOffer := offerTemplateSyncOnLinkFn
	t.Cleanup(func() {
		projectPath, globalMode, autoConfirm, outputFormat = origProjectPath, origGlobal, origYes, origFormat
		offerTemplateSyncOnLinkFn = origOffer
	})
	offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}

	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nhub:\n  enabled: true\n  linked: true\n  project_id: \"global-hub-project-id\"\n"), 0o644))

	repo := filepath.Join(home, "work", "relinkrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitRun(t, repo, "init", "-q")
	scionDir := filepath.Join(repo, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "project-id"), []byte(localID+"\n"), 0o644))
	extDir := config.GetProjectConfigDir(scionDir)
	require.NotEqual(t, scionDir, extDir, "split storage puts settings outside the repo")
	t.Chdir(repo)

	server, alreadyLinked := nameKeepingHub(t)
	require.NoError(t, config.UpdateSetting(scionDir, "hub.endpoint", server.URL, false))
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	projectPath, globalMode, outputFormat, autoConfirm = "", false, "json", true

	assertOwnHubID := func(step string) {
		t.Helper()
		ext, err := config.LoadSingleFileVersioned(extDir)
		require.NoError(t, err)
		require.NotNil(t, ext.Hub, "%s: the project's own settings have a hub block", step)
		assert.Equal(t, localID, ext.Hub.ProjectID, "%s: hub.project_id in the project's own settings", step)
		merged, err := config.LoadSettings(scionDir)
		require.NoError(t, err)
		assert.Equal(t, localID, merged.GetHubProjectID(), "%s: merged settings resolve to the project", step)
	}

	runHubLinkQuiet(t)
	assertOwnHubID("first link")

	runHubUnlinkQuiet(t)
	merged, err := config.LoadSettings(scionDir)
	require.NoError(t, err)
	assert.Empty(t, merged.GetHubProjectID(), "after unlink the global hub project ID does not leak in")

	runHubLinkQuiet(t)
	assert.True(t, *alreadyLinked, "the re-link takes the already-linked path")
	assertOwnHubID("re-link")
}

// TestPromptHubRegistration_AlwaysWritesHubProjectID checks the hub
// registration offered by 'scion init': it records hub.projectId in the
// project's own settings (the external directory of a split-storage git
// project) even when the hub ID equals the local project ID, while the global
// settings are linked to another hub project.
func TestPromptHubRegistration_AlwaysWritesHubProjectID(t *testing.T) {
	const localID = "0b5e6a1c-0000-4000-8000-0000000000c1"
	clearHubEnvForUnlinkTest(t)
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	origYes, origNoHub := autoConfirm, noHub
	t.Cleanup(func() { autoConfirm, noHub = origYes, origNoHub })

	server, _ := nameKeepingHub(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nhub:\n  enabled: true\n  linked: true\n  endpoint: \""+server.URL+
			"\"\n  project_id: \"global-hub-project-id\"\n"), 0o644))

	repo := filepath.Join(home, "work", "initrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitRun(t, repo, "init", "-q")
	scionDir := filepath.Join(repo, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "project-id"), []byte(localID+"\n"), 0o644))
	extDir := config.GetProjectConfigDir(scionDir)
	require.NotEqual(t, scionDir, extDir, "split storage puts settings outside the repo")
	t.Chdir(repo)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	autoConfirm, noHub = true, false

	var err error
	captureStderr(t, func() {
		captureStdout(t, func() { err = promptHubRegistration(false) })
	})
	require.NoError(t, err)

	ext, err := config.LoadSingleFileVersioned(extDir)
	require.NoError(t, err)
	require.NotNil(t, ext.Hub, "the project's own settings have a hub block")
	assert.Equal(t, localID, ext.Hub.ProjectID, "hub.project_id in the project's own settings")
	merged, err := config.LoadSettings(scionDir)
	require.NoError(t, err)
	assert.Equal(t, localID, merged.GetHubProjectID(), "merged settings resolve to the project")
}

// TestRunHubLink_RefusesGlobalFallbackInGitRepo checks 'scion hub link' in
// a git repository with no .scion under HOME, where resolution falls back to
// the global project: it refuses with the 'scion init' pointer, before any
// hub request and without writing settings. With --global it links the
// global project. (A repository with its own .scion links normally; see
// TestRunHubLink_RelinkWithLinkedGlobalKeepsProjectHubID.)
func TestRunHubLink_RefusesGlobalFallbackInGitRepo(t *testing.T) {
	clearHubEnvForUnlinkTest(t)
	origProjectPath, origGlobal, origYes, origFormat := projectPath, globalMode, autoConfirm, outputFormat
	origOffer := offerTemplateSyncOnLinkFn
	t.Cleanup(func() {
		projectPath, globalMode, autoConfirm, outputFormat = origProjectPath, origGlobal, origYes, origFormat
		offerTemplateSyncOnLinkFn = origOffer
	})
	offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}

	server, requests := roundTripHub(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	globalSettings := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(globalSettings, []byte(
		"schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: \""+server.URL+"\"\n"), 0o644))
	repo := filepath.Join(home, "src", "fresh-clone")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitRun(t, repo, "init", "-q")
	t.Chdir(repo)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	projectPath, globalMode, outputFormat, autoConfirm = "", false, "json", true

	before, err := os.ReadFile(globalSettings)
	require.NoError(t, err)
	var linkErr error
	captureStderr(t, func() {
		captureStdout(t, func() { linkErr = runHubLink(hubLinkCmd, nil) })
	})
	require.Error(t, linkErr)
	assert.True(t, errors.Is(linkErr, hubsync.ErrGitRepoNotProject), "error = %v", linkErr)
	assert.Contains(t, linkErr.Error(), "run 'scion init' here first")
	assert.Contains(t, linkErr.Error(), "--global")
	assert.Zero(t, requests.Load(), "the refusal makes no hub request")
	after, err := os.ReadFile(globalSettings)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the refusal writes no settings")
	_, statErr := os.Stat(filepath.Join(repo, ".scion"))
	assert.True(t, os.IsNotExist(statErr), "no .scion is created in the repository")

	// --global names the global project explicitly: it links.
	globalMode = true
	runHubLinkQuiet(t)
	linked := hubsync.ProjectLinkedSetting(globalDir, true)
	require.NotNil(t, linked)
	assert.True(t, *linked, "--global links the global project")
}

// nameKeepingHub is a local workstation hub that remembers the ID and name
// of the project registered on it and answers lookups of that ID with that
// name, so a second 'scion hub link' takes the already-linked path. The
// returned flag reports that a lookup found the registered project.
func nameKeepingHub(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	var mu sync.Mutex
	var id, name string
	found := new(bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			created := id == ""
			id, _ = body["id"].(string)
			name, _ = body["name"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": id, "name": name, "slug": name},
				"created": created,
			})
		case id != "" && r.URL.Path == "/api/v1/projects/"+id && r.Method == http.MethodGet:
			*found = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "name": name, "slug": name})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server, found
}
