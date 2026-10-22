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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const createHubInlineConfigYAML = `model: gemini-2.5-pro
thinking_level: 40
max_turns: 25
system_prompt: file://prompt.md
volumes:
  - source: /data/shared
    target: /mnt/shared
    read_only: true
skills:
  - uri: file://skills/review
    as: review
mcp_servers:
  docs:
    transport: stdio
    command: docs-mcp
    args: ["--port", "0"]
`

// ptone/scion#4012: scion create --config sends the file's config as the
// create request's config, as scion start does, so the Hub stores it with
// the agent. Without --config the request config is absent (or carries only
// --image). The RunE wiring that loads --config is out of scope here, since
// it first resolves the hub with requireHub; this test covers
// loadCreateInlineConfig and createAgentViaHub, which build the request.
func TestCreateAgentViaHub_SendsInlineConfig(t *testing.T) {
	const projectID, agentName = "proj-create-config", "create-config-agent"

	writeConfig := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Be terse."), 0o644))
		path := filepath.Join(dir, "agent.yaml")
		require.NoError(t, os.WriteFile(path, []byte(createHubInlineConfigYAML), 0o644))
		return path
	}

	for _, tc := range []struct {
		name       string
		withConfig bool
		image      string
	}{
		{name: "with --config", withConfig: true},
		{name: "with --config and --image", withConfig: true, image: "img:flag"},
		{name: "without --config"},
		{name: "without --config, --image only", image: "img:flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origPath := inlineConfigPath
			t.Cleanup(func() { inlineConfigPath = origPath })
			inlineConfigPath = ""
			if tc.withConfig {
				inlineConfigPath = writeConfig(t)
			}
			agentImage = tc.image

			inlineCfg, err := loadCreateInlineConfig(nil)
			require.NoError(t, err)

			stub := newHubStartStub(t, projectID, agentName, "")
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, projectID), agentName, "", inlineCfg)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)

			rawCfg, present := stub.createBody["config"]
			if !tc.withConfig && tc.image == "" {
				assert.False(t, present, "config must be omitted without --config or --image")
				return
			}
			require.True(t, present, "create request must carry a config")
			cfg, ok := rawCfg.(map[string]interface{})
			require.True(t, ok, "config must be an object, got %T", rawCfg)

			if tc.image != "" {
				assert.Equal(t, tc.image, cfg["image"], "--image overrides the config image")
			} else {
				assert.NotContains(t, cfg, "image")
			}

			if !tc.withConfig {
				for _, key := range []string{"model", "thinking_level", "max_turns", "volumes", "skills", "mcp_servers", "system_prompt"} {
					assert.NotContains(t, cfg, key)
				}
				return
			}

			assert.Equal(t, "gemini-2.5-pro", cfg["model"])
			assert.Equal(t, float64(40), cfg["thinking_level"])
			assert.Equal(t, float64(25), cfg["max_turns"])
			assert.Equal(t, "Be terse.", cfg["system_prompt"], "file:// content is resolved before sending")
			assert.Equal(t, []interface{}{map[string]interface{}{
				"source": "/data/shared", "target": "/mnt/shared", "read_only": true,
			}}, cfg["volumes"])
			assert.Equal(t, []interface{}{map[string]interface{}{
				"uri": "file://skills/review", "as": "review",
			}}, cfg["skills"])
			assert.Equal(t, map[string]interface{}{
				"docs": map[string]interface{}{
					"transport": "stdio", "command": "docs-mcp", "args": []interface{}{"--port", "0"},
				},
			}, cfg["mcp_servers"])
		})
	}
}

// ptone/scion#378: scion create takes --model and --thinking-level, as
// scion start does. They are sent as the request config's explicit model
// and thinking level (the top tier of the hub's order), over a --config
// file's values, and without either flag no config is sent.
func TestCreateAgentViaHub_ModelFlags(t *testing.T) {
	const projectID, agentName = "proj-create-model", "create-model-agent"

	for _, flag := range []string{"model", "thinking-level"} {
		f := createCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "create has no --%s flag", flag)
		assert.Equal(t, "", f.DefValue, "--%s default", flag)
	}
	assert.Equal(t, thinkingLevelFlagUsage, createCmd.Flags().Lookup("thinking-level").Usage)

	for _, tc := range []struct {
		name          string
		model         string
		thinking      string
		withConfig    bool
		wantModel     any
		wantThinking  any
		wantNoConfig  bool
		wantUsageErrs string
	}{
		{name: "no flags: no config", wantNoConfig: true},
		{name: "--model explicit ID", model: "claude-opus-4", wantModel: "claude-opus-4"},
		{name: "--model alias is normalized", model: "XL", wantModel: "extra-large"},
		{name: "--thinking-level shorthand", thinking: "high", wantThinking: float64(75)},
		{name: "--thinking-level integer", thinking: "0", wantThinking: float64(0)},
		{name: "both flags over --config", model: "flag-model", thinking: "10", withConfig: true, wantModel: "flag-model", wantThinking: float64(10)},
		{name: "--config alone keeps its values", withConfig: true, wantModel: "gemini-2.5-pro", wantThinking: float64(40)},
		{name: "invalid --thinking-level", thinking: "lots", wantUsageErrs: "--thinking-level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origPath, origModel, origThinking := inlineConfigPath, modelFlag, thinkingLevelFlag
			t.Cleanup(func() { inlineConfigPath, modelFlag, thinkingLevelFlag = origPath, origModel, origThinking })
			inlineConfigPath, modelFlag, thinkingLevelFlag = "", tc.model, tc.thinking
			if tc.withConfig {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Be terse."), 0o644))
				inlineConfigPath = filepath.Join(dir, "agent.yaml")
				require.NoError(t, os.WriteFile(inlineConfigPath, []byte(createHubInlineConfigYAML), 0o644))
			}

			inlineCfg, err := loadCreateInlineConfig(nil)
			if tc.wantUsageErrs != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantUsageErrs)
				assert.True(t, isUsageError(err), "error %q is not a usage error", err)
				return
			}
			require.NoError(t, err)

			stub := newHubStartStub(t, projectID, agentName, "")
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, projectID), agentName, "", inlineCfg)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)

			rawCfg, present := stub.createBody["config"]
			if tc.wantNoConfig {
				assert.False(t, present, "config must be omitted without --config, --model or --thinking-level")
				return
			}
			require.True(t, present, "create request must carry a config")
			cfg, ok := rawCfg.(map[string]interface{})
			require.True(t, ok, "config must be an object, got %T", rawCfg)
			if tc.wantModel == nil {
				assert.NotContains(t, cfg, "model")
			} else {
				assert.Equal(t, tc.wantModel, cfg["model"])
			}
			if tc.wantThinking == nil {
				assert.NotContains(t, cfg, "thinking_level")
			} else {
				assert.Equal(t, tc.wantThinking, cfg["thinking_level"])
			}
		})
	}
}

// An explicit --thinking-level "" on scion create fails as a usage error
// before any hub work, instead of being treated as unset.
func TestCreateRejectsExplicitEmptyThinkingLevel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)

	f := createCmd.Flags().Lookup("thinking-level")
	saved := thinkingLevelFlag
	t.Cleanup(func() {
		thinkingLevelFlag = saved
		f.Changed = false
	})
	require.NoError(t, createCmd.Flags().Set("thinking-level", ""))

	err := createCmd.RunE(createCmd, []string{"agent-x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--thinking-level")
	assert.True(t, isUsageError(err), "error %q is not a usage error", err)
}
