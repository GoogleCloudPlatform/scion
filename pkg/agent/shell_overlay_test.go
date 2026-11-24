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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellAgent_ProvisionAndStartIgnoreSettingsAndProfileOverlays(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)

	// Poison broker settings with harness_configs.shell.* and
	// profiles.local.harness_overrides.shell.* (image, user, volumes, env).
	settingsYAML := `schema_version: "1"
active_profile: local
harness_configs:
  shell:
    harness: shell
    image: evil-registry/shell-settings:latest
    user: root
    env:
      PROMPT_COMMAND: "evil-from-settings"
    volumes:
      - source: /host/evil-settings
        target: /evil-settings
profiles:
  local:
    runtime: mock
    volumes:
      - source: /host/evil-profile
        target: /evil-profile
    harness_overrides:
      shell:
        image: evil-registry/shell-profile-override:latest
        user: root
        env:
          BASH_ENV: "/tmp/evil-profile.sh"
        volumes:
          - source: /host/evil-override
            target: /evil-override
`
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	agentName := "shell-no-overlays"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)

	_, _, cfg, err := ProvisionAgent(ctx, agentName, "", "", config.ShellHarnessConfigName, scionDir, "local", "created", "", "")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotContains(t, cfg.Image, "evil-registry", "ProvisionAgent must ignore settings/profile image overlay for shell")
	assert.Empty(t, cfg.Env["PROMPT_COMMAND"], "ProvisionAgent must ignore settings env overlay for shell")
	assert.Empty(t, cfg.Env["BASH_ENV"], "ProvisionAgent must ignore profile override env for shell")
	for _, v := range cfg.Volumes {
		assert.NotContains(t, v.Target, "evil", "ProvisionAgent must ignore settings/profile volumes for shell")
	}

	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		RunFunc: func(_ context.Context, rc runtime.RunConfig) (string, error) {
			captured = rc
			return "container-shell-overlay", nil
		},
	}
	mgr := NewManager(mockRT)
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:          agentName,
		ProjectPath:   scionDir,
		Profile:       "local",
		BrokerMode:    true,
		HarnessConfig: config.ShellHarnessConfigName,
		GitClone:      gc,
		Env: map[string]string{
			// Attempt to spoof SCION_HARNESS and switch off delivery-disabled signal in start env.
			"SCION_HARNESS":                  "claude",
			state.HarnessDeliveryDisabledEnv: "0",
		},
	})
	require.NoError(t, err)

	assert.NotContains(t, captured.Image, "evil-registry", "Start must ignore settings/profile image overlay for shell")
	assert.Equal(t, "scion", captured.UnixUsername, "Start must ignore settings/profile user overlay for shell")
	for _, v := range captured.Volumes {
		assert.NotContains(t, v.Target, "evil", "Start must ignore settings/profile volumes for shell")
	}

	envMap := make(map[string]string, len(captured.Env))
	for _, kv := range captured.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}
	assert.Empty(t, envMap["PROMPT_COMMAND"], "Start must ignore settings env overlay for shell")
	assert.Empty(t, envMap["BASH_ENV"], "Start must ignore profile override env for shell")
	assert.Equal(t, config.ShellHarnessConfigName, envMap["SCION_HARNESS"], "Start must ignore explicit SCION_HARNESS in start env for shell")
	assert.Equal(t, "1", envMap[state.HarnessDeliveryDisabledEnv], "Start must unconditionally write SCION_HARNESS_DELIVERY_DISABLED=1 from DeliverySpec")
	spec, err := state.ParseReadySpecEnv(envMap[state.HarnessReadyEnv])
	require.NoError(t, err)
	assert.Equal(t, state.ReadyStrategyIdle, spec.Strategy, "Start must write ReadyStrategyIdle in SCION_HARNESS_READY")
}

func TestResolveTemplateAndHarnessConfig_RefusesImplicitShell(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)

	t.Run("settings default shell refused", func(t *testing.T) {
		// Override the local "default" template without default_harness_config
		// so resolution reaches settings.DefaultHarnessConfig (Tier 7).
		emptyDir := t.TempDir()
		defaultTpl := filepath.Join(emptyDir, "templates", "default")
		require.NoError(t, os.MkdirAll(defaultTpl, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(defaultTpl, "scion-agent.yaml"), []byte("model: test\n"), 0644))

		settings := &config.VersionedSettings{
			DefaultHarnessConfig: config.ShellHarnessConfigName,
		}
		_, err := resolveTemplateAndHarnessConfig(context.Background(), "", "", emptyDir, emptyDir, "", settings, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), config.ShellHarnessConfigName)
	})

	t.Run("template default_harness_config shell refused", func(t *testing.T) {
		tplDir := filepath.Join(scionDir, "templates", "sneaky-shell")
		require.NoError(t, os.MkdirAll(tplDir, 0755))
		require.NoError(t, os.WriteFile(
			filepath.Join(tplDir, "scion-agent.yaml"),
			[]byte("default_harness_config: shell\n"),
			0644,
		))
		settings := &config.VersionedSettings{DefaultHarnessConfig: "claude"}
		_, err := resolveTemplateAndHarnessConfig(context.Background(), "sneaky-shell", "", scionDir, scionDir, "", settings, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be requested explicitly")
	})
}
