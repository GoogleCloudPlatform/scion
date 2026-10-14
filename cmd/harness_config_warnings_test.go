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
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// The CLI prints harness-config load warnings (unknown top-level config.yaml
// keys) to stderr and carries on (ptone/scion#4975).

const futureKeyWarning = "config.yaml: unknown top-level field(s) future_block ignored"

func TestPrintHarnessConfigWarnings(t *testing.T) {
	var buf bytes.Buffer
	printHarnessConfigWarnings(&buf, "claude", nil)
	assert.Empty(t, buf.String())

	printHarnessConfigWarnings(&buf, "claude", []string{"first", "second"})
	assert.Equal(t, "warning: claude: first\nwarning: claude: second\n", buf.String())
}

func TestHarnessConfigInstall_UnknownTopLevelKeyWarns(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	orig := saveTemplateTestState()
	defer orig.restore()
	tmpHome := t.TempDir()
	require.NoError(t, os.Setenv("HOME", tmpHome))
	globalMode = false
	noHub = false
	autoConfirm = true

	// Install always goes to a hub now: use the mock hub the sync test uses.
	const projectID = "proj-4975-install"
	rec := &hcScopeRecorder{}
	scopeHub := harnessConfigScopeHubHandler(t, rec)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "test-project"})
		default:
			scopeHub(w, r)
		}
	}))
	defer hub.Close()
	projectPath = setupHubProject(t, tmpHome, hub.URL, projectID)

	src := filepath.Join(tmpHome, "source", "future-hc")
	createTestHarnessConfig(t, src)
	f, err := os.OpenFile(filepath.Join(src, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("future_block:\n  enabled: true\n")
	require.NoError(t, err)

	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("force", false, "")
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	// Assert outside captureStdout: a failing require inside it would leave os.Stdout redirected for later tests.
	var runErr error
	_ = captureStdout(t, func() {
		runErr = runHarnessConfigInstall(cmd, []string{"file://" + src})
	})
	require.NoError(t, runErr)

	assert.Contains(t, stderr.String(), "warning: claude: "+futureKeyWarning)
	require.Len(t, rec.createReqs, 1, "install must still create the config on the hub")
	assert.Equal(t, "project", rec.createReqs[0].Scope)
}

func TestHarnessConfigUpgrade_UnknownTopLevelKeyWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, config.GlobalDir, "harness-configs", "claude")
	require.NoError(t, config.SeedHarnessConfigFromDir(dir, harness.HarnessesFS(), "claude", true))
	f, err := os.OpenFile(filepath.Join(dir, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("\nfuture_block:\n  enabled: true\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, harnessConfigUpgradeCmd.Flags().Set("dry-run", "true"))
	t.Cleanup(func() { _ = harnessConfigUpgradeCmd.Flags().Set("dry-run", "false") })
	var stderr bytes.Buffer
	harnessConfigUpgradeCmd.SetErr(&stderr)
	t.Cleanup(func() { harnessConfigUpgradeCmd.SetErr(nil) })

	var runErr error
	_ = captureStdout(t, func() {
		runErr = harnessConfigUpgradeCmd.RunE(harnessConfigUpgradeCmd, []string{"claude"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr.String(), "warning: claude: "+futureKeyWarning)
}

func TestHarnessConfigSync_UnknownTopLevelKeyWarns(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	orig := saveTemplateTestState()
	defer orig.restore()
	tmpHome := t.TempDir()
	require.NoError(t, os.Setenv("HOME", tmpHome))
	globalMode = false
	autoConfirm = true
	noHub = false

	const projectID = "proj-4975"
	rec := &hcScopeRecorder{}
	scopeHub := harnessConfigScopeHubHandler(t, rec)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "test-project"})
		default:
			scopeHub(w, r)
		}
	}))
	defer hub.Close()
	projectPath = setupHubProject(t, tmpHome, hub.URL, projectID)

	hcDir := filepath.Join(projectPath, "harness-configs", "future-hc")
	createTestHarnessConfig(t, hcDir)
	f, err := os.OpenFile(filepath.Join(hcDir, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("future_block:\n  enabled: true\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	var stderr bytes.Buffer
	harnessConfigSyncCmd.SetErr(&stderr)
	t.Cleanup(func() { harnessConfigSyncCmd.SetErr(nil) })
	var runErr error
	_ = captureStdout(t, func() {
		runErr = harnessConfigSyncCmd.RunE(harnessConfigSyncCmd, []string{"future-hc"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr.String(), "warning: future-hc: "+futureKeyWarning)
	require.Len(t, rec.createReqs, 1, "sync must still upload the config")
}
