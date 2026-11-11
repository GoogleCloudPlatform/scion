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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for scion hub test-identity web-code (ptone/scion#4240, WS3). The
// hub is a mock; the code is a synthetic placeholder string, not a
// credential.

const tiWebCodeFake = "tiwc_synthetic-web-code-DO-NOT-PRINT-4b2e"

// tiWebCodeHub is a mock hub for the web-login-code route.
type tiWebCodeHub struct {
	mu     sync.Mutex
	mints  int
	paths  []string
	status int    // answer status; 201 when zero
	body   string // answer body for a non-zero status
}

func (h *tiWebCodeHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/web-login-code") {
			h.mu.Lock()
			h.mints++
			h.paths = append(h.paths, r.URL.Path)
			h.mu.Unlock()
			if h.status != 0 {
				w.WriteHeader(h.status)
				_, _ = w.Write([]byte(h.body))
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"code":"` + tiWebCodeFake + `","codeId":"code-1","expiresAt":"2026-10-12T02:05:00Z"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *tiWebCodeHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(h.server(t).URL)
	require.NoError(t, err)
	return c
}

func assertNoWebCode(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		assert.NotContains(t, o, tiWebCodeFake)
		assert.NotContains(t, o, "DO-NOT-PRINT")
	}
}

// resolvedTempDir is t.TempDir() with symlinks resolved, so the symlinked
// parent refusal does not fire on hosts whose temp dir is itself under a
// symlink (macOS /var).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// Golden (WS3 AC1): stdout is exactly the non-secret facts; the file holds
// only the code, mode 0600.
func TestHubTestIdentityWebCode_GoldenOutputAndFile(t *testing.T) {
	hub := &tiWebCodeHub{}
	c := hub.client(t)
	out := filepath.Join(resolvedTempDir(t), "code.txt")

	var stdout bytes.Buffer
	err := mintTestIdentityWebCode(context.Background(), &stdout, c.TestIdentities(), testIdentityWebCodeOptions{ID: "fx-1", Out: out})
	require.NoError(t, err)

	golden := "Web-login code for test identity fx-1\n" +
		"  Code ID:       code-1\n" +
		"  Expires:       2026-10-12T02:05:00Z (single use)\n" +
		"  Code file:     " + out + " (mode 0600)\n"
	assert.Equal(t, golden, stdout.String())
	assertNoWebCode(t, stdout.String())

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, tiWebCodeFake, string(data), "the file holds only the code")
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(out)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	assert.Equal(t, []string{"/api/v1/test-identities/fx-1/web-login-code"}, hub.paths)
}

// --json carries the code ID, expiry and file, never the code.
func TestHubTestIdentityWebCode_JSONHasNoCode(t *testing.T) {
	hub := &tiWebCodeHub{}
	c := hub.client(t)
	out := filepath.Join(resolvedTempDir(t), "code.txt")

	var stdout bytes.Buffer
	require.NoError(t, mintTestIdentityWebCode(context.Background(), &stdout, c.TestIdentities(), testIdentityWebCodeOptions{ID: "fx-1", Out: out, JSON: true}))
	assertNoWebCode(t, stdout.String())
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, map[string]interface{}{
		"identityId": "fx-1", "codeId": "code-1", "expiresAt": "2026-10-12T02:05:00Z", "codeFile": out,
	}, got)
}

// WS3 AC1: an existing path, a symlink (live or dangling) and a path under a
// symlinked directory are refused before any request; nothing is written
// through them.
func TestHubTestIdentityWebCode_RefusesExistingOrSymlinkedOut(t *testing.T) {
	dir := resolvedTempDir(t)
	existing := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0o644))
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("target"), 0o644))
	realDir := filepath.Join(dir, "real")
	require.NoError(t, os.Mkdir(realDir, 0o700))

	paths := map[string]string{"existing": existing}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		require.NoError(t, os.Symlink(target, link))
		dangling := filepath.Join(dir, "dangling")
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), dangling))
		linkDir := filepath.Join(dir, "linkdir")
		require.NoError(t, os.Symlink(realDir, linkDir))
		paths["symlink"] = link
		paths["dangling symlink"] = dangling
		paths["symlinked parent"] = filepath.Join(linkDir, "code.txt")
		paths["symlinked grandparent"] = filepath.Join(linkDir, "sub", "code.txt")
	}

	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			hub := &tiWebCodeHub{}
			c := hub.client(t)
			var stdout bytes.Buffer
			err := mintTestIdentityWebCode(context.Background(), &stdout, c.TestIdentities(), testIdentityWebCodeOptions{ID: "fx-1", Out: path})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing --out")
			assert.Zero(t, hub.mints, "no code is minted for a refused --out")
			assert.Empty(t, stdout.String())
			assertNoWebCode(t, err.Error())
		})
	}
	data, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "target", string(data))
	entries, err := os.ReadDir(realDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created through the symlinked directory")
	if runtime.GOOS != "windows" {
		_, err = os.Lstat(filepath.Join(dir, "missing"))
		assert.True(t, os.IsNotExist(err), "the dangling symlink's target is not created")
	}
}

// --out is required, and a failed mint leaves no file and no code behind.
func TestHubTestIdentityWebCode_OutRequiredAndFailureLeavesNoFile(t *testing.T) {
	hub := &tiWebCodeHub{}
	c := hub.client(t)
	var stdout bytes.Buffer
	err := mintTestIdentityWebCode(context.Background(), &stdout, c.TestIdentities(), testIdentityWebCodeOptions{ID: "fx-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--out is required")
	assert.Zero(t, hub.mints)

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unavailable", http.StatusNotFound, `{"error":{"code":"not_found","message":"test identity web sessions are not available on this hub","details":{"reason":"test_identity_web_sessions_unavailable","cause":"web_session_key_unavailable"}}}`, "web sessions are not available on this hub"},
		{"expired", http.StatusConflict, `{"error":{"code":"conflict","message":"test identity has expired"}}`, "test identity has expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failing := &tiWebCodeHub{status: tc.status, body: tc.body}
			fc := failing.client(t)
			out := filepath.Join(resolvedTempDir(t), "code.txt")
			var stdout bytes.Buffer
			err := mintTestIdentityWebCode(context.Background(), &stdout, fc.TestIdentities(), testIdentityWebCodeOptions{ID: "fx-1", Out: out})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, stdout.String())
			_, statErr := os.Lstat(out)
			assert.True(t, os.IsNotExist(statErr), "no code file is left after a failed mint")
		})
	}
}

// Golden through the real command tree (WS3 AC1): process stdout and stderr
// are captured whole, so no log line, notice or error path can carry the
// code.
func TestHubTestIdentityWebCode_RootCommandNeverPrintsCode(t *testing.T) {
	hub := &tiWebCodeHub{}
	srv := hub.server(t)
	setupGoldenTimeProject(t, srv.URL)
	t.Setenv("SCION_HUB_TOKEN", "synthetic-issuer-placeholder")
	restoreAllSilenceUsage(t)
	origOut, origJSON, origProject := testIdentityOut, testIdentityJSON, projectPath
	t.Cleanup(func() {
		testIdentityOut, testIdentityJSON, projectPath = origOut, origJSON, origProject
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	out := filepath.Join(resolvedTempDir(t), "code.txt")

	rootCmd.SetArgs([]string{"hub", "test-identity", "web-code", "fx-1", "--out", out})
	var runErr error
	stdout, stderr := captureStdoutStderr(t, func() { runErr = rootCmd.Execute() })
	require.NoError(t, runErr, "stderr: %s", stderr)
	assertNoWebCode(t, stdout, stderr)
	assert.Contains(t, stdout, "Web-login code for test identity fx-1\n")
	assert.Contains(t, stdout, "  Code file:     "+out+" (mode 0600)\n")
	assert.Equal(t, 1, hub.mints)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, tiWebCodeFake, string(data))

	// A second run with the same --out is refused and mints nothing.
	rootCmd.SetArgs([]string{"hub", "test-identity", "web-code", "fx-1", "--out", out})
	stdout, stderr = captureStdoutStderr(t, func() { runErr = rootCmd.Execute() })
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "refusing --out")
	assertNoWebCode(t, stdout, stderr, runErr.Error())
	assert.Equal(t, 1, hub.mints)
}

// X3b (ptone/scion#4240): web-code exists in human mode and in agent mode,
// where checkHubProfileGate lets it run only with a target-hub profile for
// another hub (TestHubTestIdentityWebCode_Profile covers the run-time rule).
func TestHubTestIdentityWebCodeCmd_ModeAvailability(t *testing.T) {
	const p = "hub.test-identity.web-code"
	require.NotNil(t, resolveCommandPath(rootCmd, p), "%s must exist in the real command tree", p)
	assert.True(t, agentAllowed[p], "%s is agent-allowed (with a target-hub profile only)", p)
	assert.True(t, hubProfileAllowed[p], "%s runs with a hub profile", p)
	assert.True(t, hubProfileAgentRuntime[p], "%s in agent mode needs a target-hub profile", p)
	assert.True(t, agentAllowed["hub.test-identity"])
	for _, mode := range []string{"human", "agent"} {
		t.Run(mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))
			t.Setenv("SCION_CLI_MODE", mode)
			applyModeRestrictions(root, resolveMode())
			assert.NotNil(t, resolveCommandPath(root, p))
		})
	}
	// --out is a required flag.
	cmd := resolveCommandPath(rootCmd, p)
	require.NotNil(t, cmd)
	f := cmd.Flags().Lookup("out")
	require.NotNil(t, f)
	assert.Equal(t, []string{"true"}, f.Annotations[cobra.BashCompOneRequiredFlag])
}

// writeExact (and write, which uses it) removes the file when the write
// fails, so no partial secret is left: the only cleanup on the web-code
// path, and the token paths rely on it too. A closed file forces the
// failure.
func TestTokenFileWriteExact_FailureRemovesFile(t *testing.T) {
	for name, write := range map[string]func(*tokenFile) error{
		"writeExact": func(tf *tokenFile) error { return tf.writeExact("synthetic-placeholder") },
		"write":      func(tf *tokenFile) error { return tf.write("synthetic-placeholder") },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(resolvedTempDir(t), "code.txt")
			tf, err := createTokenFile(path)
			require.NoError(t, err)
			require.NoError(t, tf.f.Close())

			require.Error(t, write(tf), "a write to a closed file must fail")
			_, err = os.Lstat(path)
			assert.True(t, os.IsNotExist(err), "the file is removed after a failed write, got %v", err)
		})
	}
}
