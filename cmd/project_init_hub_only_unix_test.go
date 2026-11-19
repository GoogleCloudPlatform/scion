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

//go:build unix

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRemoteHubServer starts an in-process mock Hub with an existing "remote-proj"
// project and records project registrations. Paired with SCION_HUB_TOKEN (and no
// SCION_DEV_TOKEN), isLocalWorkstationEndpoint returns false so the CLI treats it
// as a remote Hub without external network calls.
func newRemoteHubServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var registeredNames []string
	remoteProj := map[string]string{
		"id":   "11111111-2222-4333-8444-555555555555",
		"name": "Remote Proj",
		"slug": "remote-proj",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "user-1", "email": "user@example.com", "displayName": "User", "role": "member",
			})
		case r.URL.Path == "/api/v1/projects/11111111-2222-4333-8444-555555555555":
			_ = json.NewEncoder(w).Encode(remoteProj)
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet && q.Get("slug") == "remote-proj":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{remoteProj}})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			name, _ := body["name"].(string)
			mu.Lock()
			registeredNames = append(registeredNames, name)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": id, "name": name, "slug": name},
				"created": true,
			})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), registeredNames...)
	}
}

func setupHubOnlyInitEnv(t *testing.T) string {
	t.Helper()
	home := setupContextTest(t)
	restoreAllSilenceUsage(t)
	withTerminal(t, false)

	origGlobalInit := globalInit
	origMachineInit := machineInit
	origMachineInitForce := machineInitForce
	origInitImageRegistry := initImageRegistry
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		globalInit = origGlobalInit
		machineInit = origMachineInit
		machineInitForce = origMachineInitForce
		initImageRegistry = origInitImageRegistry
	})
	return home
}

func runInitTestCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	resetContextTargets()
	globalMode = false
	projectPath = ""
	outputFormat = ""
	nonInteractive = false
	autoConfirm = false
	globalInit = false
	machineInit = false
	machineInitForce = false
	initImageRegistry = ""
	hubEndpoint = ""

	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
		_ = f.Value.Set(f.DefValue)
	})
	for _, c := range []*cobra.Command{
		initCmd, projectInitCmd, hubAddCmd, useCmd, projectUseCmd, hubLinkCmd,
	} {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		})
	}

	var err error
	stdout, stderr := captureStdoutStderr(t, func() {
		rootCmd.SetArgs(args)
		err = rootCmd.Execute()
	})
	return stdout, stderr, err
}

func TestInitHubOnly_EmptyPathSetup(t *testing.T) {
	t.Run("context flow and directory init succeed with empty PATH and no init --machine", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		srv, registered := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, rtErr := config.DetectLocalRuntime()
		require.Error(t, rtErr, "empty PATH must provide no local container runtime")

		// 1. Hub-only context setup without scion init or ~/.scion/settings.yaml.
		_, _, err := runInitTestCLI(t, "hub", "add", "team", srv.URL)
		require.NoError(t, err)

		_, _, err = runInitTestCLI(t, "use", "--hub", "team")
		require.NoError(t, err)

		_, _, err = runInitTestCLI(t, "use", "remote-proj")
		require.NoError(t, err)

		_, statErr := os.Stat(filepath.Join(home, ".scion", "settings.yaml"))
		assert.True(t, os.IsNotExist(statErr), "hub-only context setup needs no global settings.yaml")

		// 2. scion init in a local directory with remote Hub selected in context
		// skips both ~/.scion/settings.yaml (init --machine) and runtime checks.
		workDir := filepath.Join(home, "work", "my-service")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		stdout, _, err := runInitTestCLI(t, "init", "--yes")
		require.NoError(t, err)
		assert.Contains(t, stdout, "scion project successfully initialized.")
		assert.FileExists(t, filepath.Join(workDir, ".scion"))
		assert.Contains(t, registered(), "my-service")
	})

	t.Run("init with --hub flag on fresh HOME without ~/.scion skips machine gate and runtime check", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		srv, registered := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, statErr := os.Stat(filepath.Join(home, ".scion"))
		require.True(t, os.IsNotExist(statErr), "precondition: ~/.scion does not exist")

		workDir := filepath.Join(home, "work", "fresh-app")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		stdout, _, err := runInitTestCLI(t, "init", "--hub", srv.URL, "--yes")
		require.NoError(t, err)
		assert.Contains(t, stdout, "scion project successfully initialized.")
		assert.FileExists(t, filepath.Join(workDir, ".scion"))
		assert.Contains(t, registered(), "fresh-app")
	})

	t.Run("init --machine skips runtime check and local image_registry warning when remote Hub is selected", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		srv, _ := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, _, err := runInitTestCLI(t, "hub", "add", "team", srv.URL)
		require.NoError(t, err)
		_, _, err = runInitTestCLI(t, "use", "--hub", "team")
		require.NoError(t, err)

		stdout, stderr, err := runInitTestCLI(t, "init", "--machine", "--yes")
		require.NoError(t, err)
		assert.Contains(t, stdout, "scion project successfully initialized.")
		assert.NotContains(t, stdout, "image_registry is not configured")
		assert.NotContains(t, stderr, "Scion runs agents in containers")
		assert.FileExists(t, filepath.Join(home, ".scion", "settings.yaml"))
	})
}

func TestInitHubOnly_GitRepoLinkWithNoContainerRuntimeOnPath(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	require.NoError(t, err, "git binary required for git-checkout test")

	home := setupHubOnlyInitEnv(t)
	srv, registered := newRemoteHubServer(t)
	t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")

	repoDir := filepath.Join(home, "repos", "demo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	for _, gitArgs := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/demo.git"},
	} {
		cmd := exec.Command(gitBin, gitArgs...)
		cmd.Dir = repoDir
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "git %v: %s", gitArgs, string(out))
	}

	// Restrict PATH to a temp directory containing only git (no docker/podman/container).
	onlyGitBinDir := t.TempDir()
	require.NoError(t, os.Symlink(gitBin, filepath.Join(onlyGitBinDir, "git")))
	t.Setenv("PATH", onlyGitBinDir)

	_, rtErr := config.DetectLocalRuntime()
	require.Error(t, rtErr, "PATH with only git must fail local container runtime detection")

	t.Chdir(repoDir)

	_, _, err = runInitTestCLI(t, "hub", "add", "team", srv.URL)
	require.NoError(t, err)
	_, _, err = runInitTestCLI(t, "use", "--hub", "team")
	require.NoError(t, err)

	// Before scion init, scion hub link refuses in an uninitialized git repo.
	_, _, err = runInitTestCLI(t, "hub", "link", "--yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'scion init' here first")

	// scion project init succeeds with no container runtime and no prior init --machine.
	_, _, err = runInitTestCLI(t, "project", "init", "--format", "json")
	require.NoError(t, err)
	assert.DirExists(t, filepath.Join(repoDir, ".scion"))

	// scion hub link now succeeds against the remote Hub.
	_, _, err = runInitTestCLI(t, "hub", "link", "--yes")
	require.NoError(t, err)
	assert.Contains(t, registered(), "demo")
}

func TestInit_NoRemoteHub_KeepsChecksAndAppendsHubUseHint(t *testing.T) {
	t.Run("init on fresh HOME without remote Hub keeps ~/.scion check and appends hint", func(t *testing.T) {
		setupHubOnlyInitEnv(t)
		t.Setenv("PATH", "")

		_, _, err := runInitTestCLI(t, "init")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "global scion directory (~/.scion/) does not exist")
		assert.Contains(t, err.Error(), "scion init --machine")
		assert.Contains(t, err.Error(), "scion hub add")
		assert.Contains(t, err.Error(), "scion use")
	})

	t.Run("init with ~/.scion present but no remote Hub selected keeps runtime check and appends hint", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		t.Setenv("PATH", "")

		// Register a hub without selecting it (CurrentHub remains empty), creating ~/.scion.
		_, _, err := runInitTestCLI(t, "hub", "add", "team", "https://hub.example.com")
		require.NoError(t, err)
		assert.DirExists(t, filepath.Join(home, ".scion"))

		workDir := filepath.Join(home, "work", "local-app")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		_, _, err = runInitTestCLI(t, "init")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no supported container runtime found")
		assert.Contains(t, err.Error(), "scion hub add")
		assert.Contains(t, err.Error(), "scion use")
	})

	t.Run("init --machine without remote Hub keeps runtime check and appends hint", func(t *testing.T) {
		setupHubOnlyInitEnv(t)
		t.Setenv("PATH", "")

		_, _, err := runInitTestCLI(t, "init", "--machine")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no supported container runtime found")
		assert.Contains(t, err.Error(), "scion hub add")
		assert.Contains(t, err.Error(), "scion use")
	})

	t.Run("local workstation Hub still requires local container runtime", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_local")
		t.Setenv("PATH", "")

		_, _, err := runInitTestCLI(t, "hub", "add", "local", "http://127.0.0.1:8080")
		require.NoError(t, err)
		_, _, err = runInitTestCLI(t, "use", "--hub", "local")
		require.NoError(t, err)

		_, _, err = runInitTestCLI(t, "init", "--machine")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no supported container runtime found")
		assert.Contains(t, err.Error(), "scion hub add")
		assert.Contains(t, err.Error(), "scion use")

		workDir := filepath.Join(home, "work", "ws-app")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		_, _, err = runInitTestCLI(t, "init")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no supported container runtime found")
		assert.Contains(t, err.Error(), "scion hub add")
		assert.Contains(t, err.Error(), "scion use")
	})
}
