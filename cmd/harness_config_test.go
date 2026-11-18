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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessConfigList(t *testing.T) {
	tmpDir := t.TempDir()

	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	t.Setenv("HOME", tmpDir)

	// Seed harness-configs via InitMachine
	require.NoError(t, config.InitMachine(harness.EmbedOnlyHarnesses()))

	// List harness-configs
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	configs, err := config.ListHarnessConfigDirs("")
	require.NoError(t, err)

	// Should have at least gemini-cli and claude configs
	names := make(map[string]bool)
	for _, hc := range configs {
		names[hc.Name] = true
	}
	assert.True(t, names["gemini-cli"], "expected gemini-cli harness-config")
	assert.True(t, names["claude"], "expected claude harness-config")

	// Verify they're in the global directory
	geminiDir := filepath.Join(globalDir, "harness-configs", "gemini-cli")
	assert.DirExists(t, geminiDir)
}

func TestHarnessConfigReset(t *testing.T) {
	tmpDir := t.TempDir()

	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	t.Setenv("HOME", tmpDir)

	// Seed harness-configs via InitMachine
	require.NoError(t, config.InitMachine(harness.EmbedOnlyHarnesses()))

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	// Corrupt a harness-config file
	claudeConfigPath := filepath.Join(globalDir, "harness-configs", "claude", "config.yaml")
	require.FileExists(t, claudeConfigPath)

	originalData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(claudeConfigPath, []byte("CORRUPTED"), 0644))

	corruptedData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)
	assert.Equal(t, "CORRUPTED", string(corruptedData))

	require.NoError(t, os.WriteFile(claudeConfigPath, originalData, 0644))

	// Now reset using SeedHarnessConfig with force
	h := harness.New("claude")
	targetDir := filepath.Join(globalDir, "harness-configs", "claude")
	require.NoError(t, config.SeedHarnessConfig(targetDir, h, true))

	// Verify it was restored (not corrupted)
	restoredData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)
	assert.NotEqual(t, "CORRUPTED", string(restoredData))
	assert.Equal(t, string(originalData), string(restoredData))
}

func TestHarnessConfigReset_BundleHarnessReturnsError(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	// Create a harness-config for an opt-in harness (opencode resolves to Generic)
	hcDir := filepath.Join(globalDir, "harness-configs", "opencode")
	require.NoError(t, os.MkdirAll(hcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: opencode\nimage: scion-opencode:latest\nuser: scion\n"), 0644))

	// harness.New("opencode") returns &Generic{} which has no embeds
	h := harness.New("opencode")
	_, basePath := h.GetHarnessEmbedsFS()
	assert.Equal(t, "", basePath, "opencode should have no embedded defaults")

	// Verify the error message mentions reinstall
	err = fmt.Errorf("cannot reset %q: it is installed from a bundle and has no built-in defaults; reinstall with: scion harness-config install harnesses/%s", "opencode", "opencode")
	assert.Contains(t, err.Error(), "installed from a bundle")
	assert.Contains(t, err.Error(), "harnesses/opencode")
}

func TestHarnessConfigShow_LocalPrintsCapabilities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)

	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	codexDir := filepath.Join(globalDir, "harness-configs", "codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.yaml"), []byte(
		"harness: codex\nimage: scion-codex:latest\nuser: scion\n"+
			"capabilities:\n"+
			"  limits:\n"+
			"    max_turns: { support: \"yes\" }\n"+
			"    max_model_calls: { support: \"no\", reason: \"no model events\" }\n"+
			"  resume: { support: \"yes\" }\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, config.AgentPluginsSidecarFile), []byte(
		"support: \"partial\"\nreason: \"Native install via local marketplace\"\n",
	), 0o644))

	outputFormat = ""
	out := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"codex"}))
	})
	assert.Contains(t, out, "Name:    codex\n")
	assert.Contains(t, out, "Source:  local\n")
	assert.Contains(t, out, "Capabilities:\n")
	assert.Contains(t, out, "limits.max_turns:")
	assert.Contains(t, out, "no (no model events)")
	assert.Contains(t, out, "resume:")
	assert.Contains(t, out, "agent_plugins:")
	assert.Contains(t, out, "partial (Native install via local marketplace)")

	outputFormat = "json"
	jsonOut := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"codex"}))
	})
	var parsed struct {
		Source       string                           `json:"source"`
		Name         string                           `json:"name"`
		Capabilities *api.HarnessAdvancedCapabilities `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	assert.Equal(t, "local", parsed.Source)
	require.NotNil(t, parsed.Capabilities)
	assert.Equal(t, api.SupportYes, parsed.Capabilities.Limits.MaxTurns.Support)
	assert.Equal(t, api.CapabilityField{Support: api.SupportNo, Reason: "no model events"}, parsed.Capabilities.Limits.MaxModelCalls)
	assert.Equal(t, api.SupportYes, parsed.Capabilities.Resume.Support)
	assert.Equal(t, api.CapabilityField{
		Support: api.SupportPartial,
		Reason:  "Native install via local marketplace",
	}, parsed.Capabilities.AgentPlugins)

	// Bare config with no declared capabilities omits the Capabilities section and JSON key.
	bareDir := filepath.Join(globalDir, "harness-configs", "bare")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "config.yaml"), []byte("harness: generic\nimage: scion-base:latest\nuser: scion\n"), 0o644))

	outputFormat = ""
	bareText := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"bare"}))
	})
	assert.NotContains(t, bareText, "Capabilities:")

	outputFormat = "json"
	bareJSON := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"bare"}))
	})
	var rawBare map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(bareJSON), &rawBare))
	_, hasCaps := rawBare["capabilities"]
	assert.False(t, hasCaps, "undeclared capabilities should be omitted from JSON output")
}

func TestHarnessConfigShow_HubPrintsCapabilities(t *testing.T) {
	getCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/harness-configs":
			// List returns a summary item; Get returns the full capabilities (including backfilled rows).
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"harnessConfigs": []map[string]interface{}{{
					"id":      "hc-hub-1",
					"name":    "codex",
					"slug":    "codex",
					"harness": "codex",
					"scope":   "global",
					"status":  "active",
				}},
			})
		case "/api/v1/harness-configs/hc-hub-1":
			getCalled = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "hc-hub-1",
				"name":        "codex",
				"slug":        "codex",
				"harness":     "codex",
				"scope":       "global",
				"status":      "active",
				"contentHash": "sha256:1234567890abcdef",
				"config": map[string]interface{}{
					"harness": "codex",
					"image":   "scion-codex:latest",
					"capabilities": map[string]interface{}{
						"harness": "codex",
						"limits": map[string]interface{}{
							"max_turns": map[string]interface{}{"support": "yes"},
						},
						"resume": map[string]interface{}{"support": "yes"},
						"agent_plugins": map[string]interface{}{
							"support": "partial",
							"reason":  "Native install via local marketplace",
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pfSetup(t, srv)
	globalMode = true

	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })

	outputFormat = ""
	out := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"codex"}))
	})
	assert.True(t, getCalled, "harness-config show should fetch GET /api/v1/harness-configs/{id}")
	assert.Contains(t, out, "Source:       hub\n")
	assert.Contains(t, out, "Capabilities:\n")
	assert.Contains(t, out, "limits.max_turns:")
	assert.Contains(t, out, "resume:")
	assert.Contains(t, out, "agent_plugins:")
	assert.Contains(t, out, "partial (Native install via local marketplace)")

	outputFormat = "json"
	jsonOut := captureStdout(t, func() {
		require.NoError(t, harnessConfigShowCmd.RunE(harnessConfigShowCmd, []string{"codex"}))
	})
	var got hubclient.HarnessConfig
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &got))
	require.NotNil(t, got.Config)
	require.NotNil(t, got.Config.Capabilities)
	assert.Equal(t, api.CapabilityField{
		Support: api.SupportPartial,
		Reason:  "Native install via local marketplace",
	}, got.Config.Capabilities.AgentPlugins)
}
