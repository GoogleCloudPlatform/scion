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

// newRemoteHubServer starts an in-process TLS mock Hub exposed under a
// non-loopback hostname (https://hub.example.test:<port>) via hostnameURL,
// with an existing "remote-proj" project and recording project registrations.
func newRemoteHubServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var registeredNames []string
	remoteProj := map[string]string{
		"id":   "11111111-2222-4333-8444-555555555555",
		"name": "Remote Proj",
		"slug": "remote-proj",
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return hostnameURL(t, srv), func() []string {
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

	// Reset cached runtime detection before and after each test (registered
	// before callers' t.Setenv("PATH", ...) so LIFO cleanup restores PATH first).
	config.ResetRuntimeDetectionCache()
	t.Cleanup(config.ResetRuntimeDetectionCache)

	// Stub hubResponds so 'scion hub add' never makes outbound network calls.
	origResponds := hubResponds
	hubResponds = func(string, bool) error { return nil }

	// Redirect os.Stdin to /dev/null so promptImageRegistry (which checks
	// util.IsTerminal on os.Stdin) cannot hang when cmd.test runs on a PTY.
	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	origStdin := os.Stdin
	os.Stdin = devNull

	origGlobalInit := globalInit
	origMachineInit := machineInit
	origMachineInitForce := machineInitForce
	origInitImageRegistry := initImageRegistry
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		hubResponds = origResponds
		os.Stdin = origStdin
		_ = devNull.Close()
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
		srvURL, registered := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, rtErr := config.DetectLocalRuntime()
		require.Error(t, rtErr, "empty PATH must provide no local container runtime")

		// 1. Hub-only context setup without scion init or ~/.scion/settings.yaml.
		_, _, err := runInitTestCLI(t, "hub", "add", "team", srvURL)
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
		srvURL, registered := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, statErr := os.Stat(filepath.Join(home, ".scion"))
		require.True(t, os.IsNotExist(statErr), "precondition: ~/.scion does not exist")

		workDir := filepath.Join(home, "work", "fresh-app")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		stdout, _, err := runInitTestCLI(t, "init", "--hub", srvURL, "--yes")
		require.NoError(t, err)
		assert.Contains(t, stdout, "scion project successfully initialized.")
		assert.FileExists(t, filepath.Join(workDir, ".scion"))
		assert.Contains(t, registered(), "fresh-app")
	})

	t.Run("unauthenticated remote https Hub registered via hub add + use --hub skips local checks before login", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		t.Setenv("PATH", "")

		unauthSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/healthz" {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
		}))
		t.Cleanup(unauthSrv.Close)
		unauthURL := hostnameURL(t, unauthSrv)

		_, _, err := runInitTestCLI(t, "hub", "add", "team", unauthURL)
		require.NoError(t, err)
		_, _, err = runInitTestCLI(t, "use", "--hub", "team")
		require.NoError(t, err)

		workDir := filepath.Join(home, "work", "pre-login-app")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		t.Chdir(workDir)

		stdout, _, err := runInitTestCLI(t, "init", "--yes")
		require.NoError(t, err)
		assert.Contains(t, stdout, "scion project successfully initialized.")
		assert.Contains(t, stdout, "Note: authentication failed")
		assert.FileExists(t, filepath.Join(workDir, ".scion"))
	})

	t.Run("init --machine skips runtime check and local image_registry warning when remote Hub is selected", func(t *testing.T) {
		home := setupHubOnlyInitEnv(t)
		srvURL, _ := newRemoteHubServer(t)
		t.Setenv("SCION_HUB_TOKEN", "bearer-remote-token")
		t.Setenv("PATH", "")

		_, _, err := runInitTestCLI(t, "hub", "add", "team", srvURL)
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
	srvURL, registered := newRemoteHubServer(t)
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

	_, _, err = runInitTestCLI(t, "hub", "add", "team", srvURL)
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

func TestInit_EndpointClassificationProbes_FailClosed(t *testing.T) {
	probes := []struct {
		name      string
		env       map[string]string
		setupArgs [][]string
		initArgs  []string
		wantErr   string
	}{
		{
			name:     "loopback --hub without dev-token keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "http://127.0.0.1:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "loopback --hub without dev-token keeps init --machine runtime check",
			initArgs: []string{"init", "--machine", "--hub", "http://127.0.0.1:8080", "--yes"},
			wantErr:  "no supported container runtime found",
		},
		{
			name:     "SCION_HUB_ENDPOINT=http://localhost:8080 without dev-token keeps ~/.scion gate",
			env:      map[string]string{"SCION_HUB_ENDPOINT": "http://localhost:8080"},
			initArgs: []string{"init", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name: "hub add local + use --hub local without dev-token keeps runtime check",
			setupArgs: [][]string{
				{"hub", "add", "local", "http://127.0.0.1:8080"},
				{"use", "--hub", "local"},
			},
			initArgs: []string{"init", "--yes"},
			wantErr:  "no supported container runtime found",
		},
		{
			name:     "uppercase LOCALHOST with dev-token keeps ~/.scion gate",
			env:      map[string]string{"SCION_DEV_TOKEN": "scion_dev_local"},
			initArgs: []string{"init", "--hub", "http://LOCALHOST:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "127.0.0.2 loopback variant with dev-token keeps ~/.scion gate",
			env:      map[string]string{"SCION_DEV_TOKEN": "scion_dev_local"},
			initArgs: []string{"init", "--hub", "http://127.0.0.2:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "IPv6 loopback [::1] without dev-token keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "http://[::1]:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "dev-prefixed token in SCION_HUB_TOKEN on loopback keeps ~/.scion gate",
			env:      map[string]string{"SCION_HUB_TOKEN": "scion_dev_spoof"},
			initArgs: []string{"init", "--hub", "http://127.0.0.1:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "plaintext non-loopback http:// --hub keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "http://evil.example.com:8080", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "plaintext non-loopback http:// --hub keeps init --machine runtime check",
			initArgs: []string{"init", "--machine", "--hub", "http://evil.example.com:8080", "--yes"},
			wantErr:  "no supported container runtime found",
		},
		{
			name:     "malformed SCION_HUB_ENDPOINT=not-a-url keeps ~/.scion gate",
			env:      map[string]string{"SCION_HUB_ENDPOINT": "not-a-url"},
			initArgs: []string{"init", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "whitespace --hub in --format json mode keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "   ", "--format", "json"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "userinfo in --hub URL keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://user:pass@hub.example.com", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "localhost with trailing dot over https keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://localhost.:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "dotted hex IPv4 loopback keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://0x7f.0x0.0x0.0x1:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "short hex IPv4 loopback keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://0x7f.0x1:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "mixed-radix decimal+hex IPv4 loopback keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://127.0.0.0x1:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "mixed-radix octal+hex IPv4 loopback keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://0177.0.0.0x1:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "0.0.0.0/8 non-zero address keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://0.0.0.1:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "IPv4-mapped IPv6 0.0.0.1 keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://[::ffff:0.0.0.1]:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "RFC 1918 private IPv4 keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://10.0.0.5:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "localhost.localdomain alias keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://localhost.localdomain:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
		{
			name:     "ip6-localhost alias keeps ~/.scion gate",
			initArgs: []string{"init", "--hub", "https://ip6-localhost:8443", "--yes"},
			wantErr:  "global scion directory (~/.scion/) does not exist",
		},
	}

	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			home := setupHubOnlyInitEnv(t)
			t.Setenv("PATH", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			for _, args := range tc.setupArgs {
				_, _, err := runInitTestCLI(t, args...)
				require.NoError(t, err)
			}

			workDir := filepath.Join(home, "work", "probe-app")
			require.NoError(t, os.MkdirAll(workDir, 0o755))
			t.Chdir(workDir)

			_, _, err := runInitTestCLI(t, tc.initArgs...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "scion hub add")
			assert.Contains(t, err.Error(), "scion use")
			assert.NoFileExists(t, filepath.Join(workDir, ".scion"))
		})
	}
}

func TestIsRemoteHubEndpoint_EndpointShapes(t *testing.T) {
	cases := []struct {
		name     string
		category string
		endpoint string
		want     bool
	}{
		// Valid HTTPS remote DNS names (public/multi-label with alphabetic final label).
		{name: "https remote domain", category: "valid https remote", endpoint: "https://hub.example.com", want: true},
		{name: "https remote domain with port", category: "valid https remote", endpoint: "https://hub.example.com:8443", want: true},
		{name: "https remote domain with subpath", category: "valid https remote", endpoint: "https://hub.example.com/subpath", want: true},
		{name: "uppercase HTTPS scheme and host", category: "valid https remote", endpoint: "HTTPS://HUB.EXAMPLE.COM", want: true},
		{name: "https remote FQDN with single trailing dot", category: "valid https remote", endpoint: "https://hub.example.com.", want: true},
		{name: "https remote hyphenated subdomain", category: "valid https remote", endpoint: "https://hub-1.prod.example.io:8443", want: true},
		{name: "https remote test TLD", category: "valid https remote", endpoint: "https://hub.example.test:8443", want: true},
		// Lexical-only classification: DNS names that resolve to loopback are out of scope.
		{name: "https lexical DNS name (nip.io)", category: "valid https remote", endpoint: "https://127.0.0.1.nip.io:8443", want: true},

		// Valid HTTPS globally routable IP literals.
		{name: "https public IPv4 literal 93.184.216.34", category: "valid https remote ip", endpoint: "https://93.184.216.34:8443", want: true},
		{name: "https public IPv4 literal 1.1.1.1", category: "valid https remote ip", endpoint: "https://1.1.1.1", want: true},
		{name: "https public IPv4 literal 8.8.8.8", category: "valid https remote ip", endpoint: "https://8.8.8.8:443", want: true},
		{name: "https public IPv6 literal 2606:4700:4700::1111", category: "valid https remote ip", endpoint: "https://[2606:4700:4700::1111]:8443", want: true},
		{name: "https public IPv6 literal 2001:4860:4860::8888", category: "valid https remote ip", endpoint: "https://[2001:4860:4860::8888]", want: true},

		// Valid HTTP non-loopback (rejected by CheckHubTransport and https-only allowlist).
		{name: "http non-loopback domain", category: "http non-loopback", endpoint: "http://hub.example.com", want: false},
		{name: "http non-loopback domain with port", category: "http non-loopback", endpoint: "http://evil.example.com:8080", want: false},
		{name: "http non-loopback public IPv4", category: "http non-loopback", endpoint: "http://93.184.216.34:8080", want: false},
		{name: "http non-loopback public IPv6", category: "http non-loopback", endpoint: "http://[2606:4700:4700::1111]:8080", want: false},

		// Loopback IPv4 & localhost variants.
		{name: "http 127.0.0.1", category: "loopback", endpoint: "http://127.0.0.1:8080", want: false},
		{name: "http 127.0.0.2 alternate loopback", category: "loopback", endpoint: "http://127.0.0.2:8080", want: false},
		{name: "http 127.255.255.254 loopback range", category: "loopback", endpoint: "http://127.255.255.254:9810", want: false},
		{name: "https 127.0.0.1", category: "loopback", endpoint: "https://127.0.0.1:8443", want: false},
		{name: "https 127.0.0.2", category: "loopback", endpoint: "https://127.0.0.2:8443", want: false},
		{name: "http localhost", category: "loopback", endpoint: "http://localhost:8080", want: false},
		{name: "http uppercase LOCALHOST", category: "loopback", endpoint: "http://LOCALHOST:8080", want: false},
		{name: "https localhost", category: "loopback", endpoint: "https://localhost:8443", want: false},
		{name: "https uppercase LOCALHOST", category: "loopback", endpoint: "https://LOCALHOST:8443", want: false},
		{name: "https subdomain of localhost", category: "loopback", endpoint: "https://sub.localhost:8443", want: false},
		{name: "https nested subdomain of localhost", category: "loopback", endpoint: "https://foo.bar.localhost:8443", want: false},

		// Common loopback / local / container host aliases and single-label names.
		{name: "https localhost.localdomain", category: "local alias", endpoint: "https://localhost.localdomain:8443", want: false},
		{name: "https uppercase LOCALHOST.LOCALDOMAIN", category: "local alias", endpoint: "https://LOCALHOST.LOCALDOMAIN:8443", want: false},
		{name: "https ip6-localhost", category: "local alias", endpoint: "https://ip6-localhost:8443", want: false},
		{name: "https ip6-loopback", category: "local alias", endpoint: "https://ip6-loopback:8443", want: false},
		{name: "https mDNS .local host", category: "local alias", endpoint: "https://myhost.local:8443", want: false},
		{name: "https container host alias host.containers.internal", category: "local alias", endpoint: "https://host.containers.internal:8443", want: false},
		{name: "https container host alias host.docker.internal", category: "local alias", endpoint: "https://host.docker.internal:8443", want: false},
		{name: "https container host alias scion-hub.internal", category: "local alias", endpoint: "https://scion-hub.internal:8443", want: false},
		{name: "https single-label hostname", category: "single-label", endpoint: "https://myhub:8443", want: false},
		{name: "https single-label underscore hostname", category: "single-label", endpoint: "https://scion_hub:8443", want: false},

		// Non-standard numeric IPv4 encodings (short, decimal, octal, hex, mixed-radix).
		{name: "short IPv4 127.1", category: "numeric ipv4", endpoint: "https://127.1:8443", want: false},
		{name: "short IPv4 127.0.1", category: "numeric ipv4", endpoint: "https://127.0.1:8443", want: false},
		{name: "decimal integer IPv4 2130706433", category: "numeric ipv4", endpoint: "https://2130706433:8443", want: false},
		{name: "octal IPv4 0177.0.0.1", category: "numeric ipv4", endpoint: "https://0177.0.0.1:8443", want: false},
		{name: "short octal IPv4 0177.1", category: "numeric ipv4", endpoint: "https://0177.1:8443", want: false},
		{name: "hex integer IPv4 0x7f000001", category: "numeric ipv4", endpoint: "https://0x7f000001:8443", want: false},
		{name: "uppercase hex integer IPv4 0X7F000001", category: "numeric ipv4", endpoint: "https://0X7F000001:8443", want: false},
		{name: "dotted hex IPv4 0x7f.0x0.0x0.0x1", category: "numeric ipv4", endpoint: "https://0x7f.0x0.0x0.0x1:8443", want: false},
		{name: "short hex IPv4 0x7f.0x1", category: "numeric ipv4", endpoint: "https://0x7f.0x1:8443", want: false},
		{name: "mixed-radix decimal+hex IPv4 127.0.0.0x1", category: "numeric ipv4", endpoint: "https://127.0.0.0x1:8443", want: false},
		{name: "mixed-radix octal+hex IPv4 0177.0.0.0x1", category: "numeric ipv4", endpoint: "https://0177.0.0.0x1:8443", want: false},
		{name: "dotted hex IPv4 ending in hex letter 0x7f.0x0.0x0.0xa", category: "numeric ipv4", endpoint: "https://0x7f.0x0.0x0.0xa:8443", want: false},

		// 0.0.0.0/8, unspecified, private, link-local, multicast, CGNAT, documentation, reserved IPs.
		{name: "unspecified IPv4 0.0.0.0", category: "non-global ip", endpoint: "https://0.0.0.0:8443", want: false},
		{name: "0.0.0.0/8 non-zero 0.0.0.1", category: "non-global ip", endpoint: "https://0.0.0.1:8443", want: false},
		{name: "0.0.0.0/8 non-zero 0.1.2.3", category: "non-global ip", endpoint: "https://0.1.2.3:8443", want: false},
		{name: "unspecified IPv6 [::]", category: "non-global ip", endpoint: "https://[::]:8443", want: false},
		{name: "private IPv4 10.0.0.5", category: "non-global ip", endpoint: "https://10.0.0.5:8443", want: false},
		{name: "private IPv4 172.16.0.1", category: "non-global ip", endpoint: "https://172.16.0.1:8443", want: false},
		{name: "private IPv4 192.168.1.10", category: "non-global ip", endpoint: "https://192.168.1.10:8443", want: false},
		{name: "private IPv6 ULA [fc00::1]", category: "non-global ip", endpoint: "https://[fc00::1]:8443", want: false},
		{name: "private IPv6 ULA [fd12:3456::1]", category: "non-global ip", endpoint: "https://[fd12:3456::1]:8443", want: false},
		{name: "link-local IPv4 169.254.169.254", category: "non-global ip", endpoint: "https://169.254.169.254:8443", want: false},
		{name: "link-local IPv6 [fe80::1]", category: "non-global ip", endpoint: "https://[fe80::1]:8443", want: false},
		{name: "multicast IPv4 224.0.0.1", category: "non-global ip", endpoint: "https://224.0.0.1:8443", want: false},
		{name: "multicast IPv6 [ff02::1]", category: "non-global ip", endpoint: "https://[ff02::1]:8443", want: false},
		{name: "CGNAT IPv4 100.64.0.1", category: "non-global ip", endpoint: "https://100.64.0.1:8443", want: false},
		{name: "TEST-NET-1 192.0.2.1", category: "non-global ip", endpoint: "https://192.0.2.1:8443", want: false},
		{name: "TEST-NET-2 198.51.100.1", category: "non-global ip", endpoint: "https://198.51.100.1:8443", want: false},
		{name: "TEST-NET-3 203.0.113.1", category: "non-global ip", endpoint: "https://203.0.113.1:8443", want: false},
		{name: "IPv6 documentation [2001:db8::1]", category: "non-global ip", endpoint: "https://[2001:db8::1]:8443", want: false},
		{name: "reserved IPv4 240.0.0.1", category: "non-global ip", endpoint: "https://240.0.0.1:8443", want: false},
		{name: "broadcast IPv4 255.255.255.255", category: "non-global ip", endpoint: "https://255.255.255.255:8443", want: false},

		// IPv6 loopback, mapped, translated, NAT64, zone IDs, and bracketed IPv4.
		{name: "http IPv6 loopback [::1]", category: "ipv6 special", endpoint: "http://[::1]:8080", want: false},
		{name: "https IPv6 loopback [::1]", category: "ipv6 special", endpoint: "https://[::1]:8443", want: false},
		{name: "https expanded IPv6 loopback", category: "ipv6 special", endpoint: "https://[0:0:0:0:0:0:0:1]:8443", want: false},
		{name: "https IPv4-mapped IPv6 loopback 127.0.0.1", category: "ipv6 special", endpoint: "https://[::ffff:127.0.0.1]:8443", want: false},
		{name: "https IPv4-mapped IPv6 loopback 127.0.0.2", category: "ipv6 special", endpoint: "https://[::ffff:127.0.0.2]:8443", want: false},
		{name: "https IPv4-mapped IPv6 loopback hex 7f00:1", category: "ipv6 special", endpoint: "https://[::ffff:7f00:1]:8443", want: false},
		{name: "https IPv4-mapped IPv6 unspecified 0.0.0.0", category: "ipv6 special", endpoint: "https://[::ffff:0.0.0.0]:8443", want: false},
		{name: "https IPv4-mapped IPv6 0.0.0.1", category: "ipv6 special", endpoint: "https://[::ffff:0.0.0.1]:8443", want: false},
		{name: "https IPv4-mapped IPv6 public 8.8.8.8", category: "ipv6 special", endpoint: "https://[::ffff:8.8.8.8]:8443", want: false},
		{name: "https IPv4-compatible IPv6 [::127.0.0.1]", category: "ipv6 special", endpoint: "https://[::127.0.0.1]:8443", want: false},
		{name: "https IPv4-translated SIIT [::ffff:0:127.0.0.1]", category: "ipv6 special", endpoint: "https://[::ffff:0:127.0.0.1]:8443", want: false},
		{name: "https NAT64 [64:ff9b::127.0.0.1]", category: "ipv6 special", endpoint: "https://[64:ff9b::127.0.0.1]:8443", want: false},
		{name: "https IPv6 loopback with zone id", category: "ipv6 special", endpoint: "https://[::1%25lo0]:8443", want: false},
		{name: "https public IPv6 with zone id", category: "ipv6 special", endpoint: "https://[2606:4700:4700::1111%25eth0]:8443", want: false},
		{name: "https bracketed IPv4 literal", category: "ipv6 special", endpoint: "https://[1.1.1.1]:8443", want: false},

		// Empty and whitespace.
		{name: "empty string", category: "empty", endpoint: "", want: false},
		{name: "spaces only", category: "whitespace", endpoint: "   ", want: false},
		{name: "tabs and newline", category: "whitespace", endpoint: "\t\n", want: false},
		{name: "leading space on valid https URL", category: "whitespace", endpoint: " https://hub.example.com", want: false},
		{name: "trailing space on valid https URL", category: "whitespace", endpoint: "https://hub.example.com ", want: false},

		// Malformed, invalid ports, non-https schemes, and non-alphabetic final labels.
		{name: "bare word", category: "malformed", endpoint: "not-a-url", want: false},
		{name: "schemeless host", category: "malformed", endpoint: "hub.example.com", want: false},
		{name: "missing scheme name", category: "malformed", endpoint: "://hub.example.com", want: false},
		{name: "https with empty authority", category: "malformed", endpoint: "https://", want: false},
		{name: "https with port only and empty host", category: "malformed", endpoint: "https://:8443", want: false},
		{name: "https with trailing colon empty port", category: "malformed", endpoint: "https://hub.example.com:", want: false},
		{name: "https with port zero", category: "malformed", endpoint: "https://hub.example.com:0", want: false},
		{name: "https with port above 65535", category: "malformed", endpoint: "https://hub.example.com:70000", want: false},
		{name: "https with plus-prefixed port", category: "malformed", endpoint: "https://hub.example.com:+443", want: false},
		{name: "https with leading-zero port", category: "malformed", endpoint: "https://hub.example.com:0443", want: false},
		{name: "https with query string", category: "malformed", endpoint: "https://hub.example.com?a=1", want: false},
		{name: "https with fragment", category: "malformed", endpoint: "https://hub.example.com#frag", want: false},
		{name: "wss scheme", category: "malformed", endpoint: "wss://hub.example.com", want: false},
		{name: "ftp scheme", category: "malformed", endpoint: "ftp://hub.example.com", want: false},
		{name: "invalid hostname punctuation", category: "malformed", endpoint: "https://bad!host.example.com", want: false},
		{name: "invalid hostname underscore", category: "malformed", endpoint: "https://bad_label.example.com", want: false},
		{name: "numeric final label", category: "malformed", endpoint: "https://hub.example.123", want: false},
		{name: "single-letter final label", category: "malformed", endpoint: "https://hub.a", want: false},

		// Userinfo / '@'.
		{name: "https user and password", category: "userinfo", endpoint: "https://user:pass@hub.example.com", want: false},
		{name: "https user only", category: "userinfo", endpoint: "https://user@hub.example.com", want: false},
		{name: "https loopback user before remote host", category: "userinfo", endpoint: "https://127.0.0.1@hub.example.com", want: false},
		{name: "https @ in path", category: "userinfo", endpoint: "https://hub.example.com/@team", want: false},

		// Trailing dot variants.
		{name: "http localhost with trailing dot", category: "trailing dot", endpoint: "http://localhost.:8080", want: false},
		{name: "https localhost with trailing dot", category: "trailing dot", endpoint: "https://localhost.:8443", want: false},
		{name: "https uppercase LOCALHOST with trailing dot", category: "trailing dot", endpoint: "https://LOCALHOST.:8443", want: false},
		{name: "https localhost.localdomain with trailing dot", category: "trailing dot", endpoint: "https://localhost.localdomain.:8443", want: false},
		{name: "https IPv4 loopback with trailing dot", category: "trailing dot", endpoint: "https://127.0.0.1.:8443", want: false},
		{name: "https public IPv4 with trailing dot", category: "trailing dot", endpoint: "https://1.1.1.1.:8443", want: false},
		{name: "https remote domain with double trailing dot", category: "trailing dot", endpoint: "https://hub.example.com..", want: false},
	}

	tokenEnvs := []struct {
		name string
		env  map[string]string
	}{
		{name: "without_dev_token", env: nil},
		{name: "with_dev_token", env: map[string]string{"SCION_DEV_TOKEN": "scion_dev_local"}},
		{name: "with_dev_prefixed_hub_token", env: map[string]string{"SCION_HUB_TOKEN": "scion_dev_spoof"}},
		{name: "with_bearer_hub_token", env: map[string]string{"SCION_HUB_TOKEN": "bearer-remote-token"}},
	}

	for _, te := range tokenEnvs {
		t.Run(te.name, func(t *testing.T) {
			setupContextTest(t)
			for k, v := range te.env {
				t.Setenv(k, v)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					assert.Equal(t, tc.want, isRemoteHubEndpoint(tc.endpoint),
						"category=%s endpoint=%q env=%s", tc.category, tc.endpoint, te.name)
				})
			}
		})
	}
}
