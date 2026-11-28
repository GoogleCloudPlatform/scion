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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStart_ShellOmitsCLIMode verifies §4.2 (ptone/scion#9743): shell execution
// container launches must not inject SCION_CLI_MODE=agent into the container
// environment, while non-shell launches continue to set SCION_CLI_MODE=agent.
// Also confirms A6: a freshly provisioned shell agent home ships no settings
// file setting cli.mode.
func TestStart_ShellOmitsCLIMode(t *testing.T) {
	t.Run("shell launch omits SCION_CLI_MODE and ships no cli.mode setting", func(t *testing.T) {
		scionDir, _ := reprovisionSetup(t)
		agentName := "shell-cli-mode"
		gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		ctx := api.ContextWithGitClone(context.Background(), gc)

		_, _, _, err := ProvisionAgent(ctx, agentName, "", "", config.ShellHarnessConfigName, scionDir, "", "created", "", "")
		require.NoError(t, err)

		// A6: verify the provisioned shell agent home ships no settings file
		// with cli.mode: agent.
		agentHome := config.GetAgentHomePath(scionDir, agentName)
		for _, name := range []string{"settings.yaml", "settings.json"} {
			p := filepath.Join(agentHome, ".scion", name)
			data, readErr := os.ReadFile(p)
			if readErr == nil {
				assert.NotContains(t, string(data), "mode: agent", "shell agent home %s must not ship cli.mode: agent", name)
			} else {
				assert.True(t, os.IsNotExist(readErr), "unexpected error reading %s: %v", p, readErr)
			}
		}

		var capturedRunCfg runtime.RunConfig
		ran := false
		mockRT := &runtime.MockRuntime{
			RunFunc: func(_ context.Context, rc runtime.RunConfig) (string, error) {
				ran = true
				capturedRunCfg = rc
				return "container-shell-cli-mode", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err = mgr.Start(context.Background(), api.StartOptions{
			Name:          agentName,
			ProjectPath:   scionDir,
			BrokerMode:    true,
			HarnessConfig: config.ShellHarnessConfigName,
			GitClone:      gc,
		})
		require.NoError(t, err)
		require.True(t, ran, "expected MockRuntime.Run to be called")

		envMap := envSliceToMap(capturedRunCfg.Env)
		_, hasCLIMode := envMap["SCION_CLI_MODE"]
		assert.False(t, hasCLIMode, "shell launch must not set SCION_CLI_MODE in container env")
	})

	t.Run("non-shell launch sets SCION_CLI_MODE=agent", func(t *testing.T) {
		scionDir, _ := reprovisionSetup(t)
		agentName := "nonshell-cli-mode"
		gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		ctx := api.ContextWithGitClone(context.Background(), gc)

		_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "generic", scionDir, "", "created", "", "")
		require.NoError(t, err)

		var capturedRunCfg runtime.RunConfig
		ran := false
		mockRT := &runtime.MockRuntime{
			RunFunc: func(_ context.Context, rc runtime.RunConfig) (string, error) {
				ran = true
				capturedRunCfg = rc
				return "container-nonshell-cli-mode", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err = mgr.Start(context.Background(), api.StartOptions{
			Name:          agentName,
			ProjectPath:   scionDir,
			BrokerMode:    true,
			HarnessConfig: "generic",
			GitClone:      gc,
		})
		require.NoError(t, err)
		require.True(t, ran, "expected MockRuntime.Run to be called")

		envMap := envSliceToMap(capturedRunCfg.Env)
		assert.Equal(t, "agent", envMap["SCION_CLI_MODE"], "non-shell launch must keep SCION_CLI_MODE=agent")
	})
}
