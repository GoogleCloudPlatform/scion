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
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security audit H1 (provisioner defence in depth): ProvisionAgent must skip
// the template chain home/ overlay when harnessConfigName == "shell", falling
// through to the embedded default home floor only so project-authored template
// files (.bashrc, .scion/hooks/pre-start.d/*) never land in a shell container.
func TestProvisionAgent_ShellSkipsTemplateHomeOverlay(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)

	// Poison the project-tier "default" template with a .bashrc and a
	// per-agent pre-start hook script.
	tplHome := filepath.Join(scionDir, "templates", "default", "home")
	require.NoError(t, os.MkdirAll(filepath.Join(tplHome, ".scion", "hooks", "pre-start.d"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(tplHome, ".bashrc"), []byte("echo PWNED_BY_TEMPLATE\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tplHome, ".scion", "hooks", "pre-start.d", "99-evil.sh"), []byte("#!/bin/sh\necho EVIL\n"), 0755))

	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)

	agentName := "shell-no-tpl-home"
	_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", config.ShellHarnessConfigName, scionDir, "", "created", "", "")
	require.NoError(t, err)

	agentHome := config.GetAgentHomePath(scionDir, agentName)
	_, err = os.Stat(filepath.Join(agentHome, ".bashrc"))
	assert.True(t, os.IsNotExist(err), "template .bashrc must not be copied into shell agent home")
	_, err = os.Stat(filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "99-evil.sh"))
	assert.True(t, os.IsNotExist(err), "template pre-start.d hook must not be copied into shell agent home")

	// Embedded default floor (.tmux.conf) must still be present.
	_, err = os.Stat(filepath.Join(agentHome, ".tmux.conf"))
	assert.NoError(t, err, "embedded default .tmux.conf floor must still be applied")
}

// Security audit H3 (provision defence in depth): finishProvision must never
// stage a project pre-start hook for a shell agent and must remove any stale
// staged hook file.
func TestFinishProvision_ShellClearsPreStartHook(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "shell-finish-hook"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)

	_, _, cfg, err := ProvisionAgent(ctx, agentName, "default", "", config.ShellHarnessConfigName, scionDir, "", "created", "", "")
	require.NoError(t, err)

	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	hookPath := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "30-project-custom")

	// Pre-plant a stale hook file to confirm finishProvision removes it.
	require.NoError(t, harness.WriteProjectPreStartHook(agentHome, "#!/bin/sh\necho stale\n"))
	_, err = os.Stat(hookPath)
	require.NoError(t, err)

	mgr := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	err = mgr.finishProvision(api.StartOptions{
		Name:                      agentName,
		ProjectPath:               scionDir,
		HarnessConfig:             config.ShellHarnessConfigName,
		ProjectPreStartHookScript: "#!/bin/sh\necho evil\n",
	}, agentDir, agentHome, cfg)
	require.NoError(t, err)

	_, err = os.Stat(hookPath)
	assert.True(t, os.IsNotExist(err), "finishProvision must remove any stale pre-start hook for shell agents")
}

// Security audit H3 (run-path defence in depth): AgentManager.Start must never
// stage a project pre-start hook for a shell agent and must remove any stale
// staged hook file before launching the container.
func TestStart_ShellClearsPreStartHook(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "shell-start-hook"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)

	_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", config.ShellHarnessConfigName, scionDir, "", "created", "", "")
	require.NoError(t, err)

	agentHome := config.GetAgentHomePath(scionDir, agentName)
	hookPath := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "30-project-custom")
	require.NoError(t, harness.WriteProjectPreStartHook(agentHome, "#!/bin/sh\necho stale\n"))

	mockRT := &runtime.MockRuntime{}
	mgr := NewManager(mockRT)
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:                      agentName,
		ProjectPath:               scionDir,
		BrokerMode:                true,
		HarnessConfig:             config.ShellHarnessConfigName,
		GitClone:                  gc,
		ProjectPreStartHookScript: "#!/bin/sh\necho evil\n",
	})
	require.NoError(t, err)

	_, err = os.Stat(hookPath)
	assert.True(t, os.IsNotExist(err), "Start must remove any stale pre-start hook for shell agents")
}

// Security audit H1-R: when a project-local "default" template defines an env
// layer (e.g. PROMPT_COMMAND), ProvisionAgent with harness_config="shell" and
// empty templateName must record NO template in agent-info.json or
// image-provenance.json, and Start must pass an empty FallbackEnv (and empty
// Template) to the runtime — even if TemplateEnvSource or a legacy recorded
// template is present.
func TestShellAgent_NoTemplateRecordedAndNoFallbackEnv(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)

	// Auditor PoC: plant PROMPT_COMMAND in <project>/.scion/templates/default/scion-agent.yaml.
	defaultTplDir := filepath.Join(scionDir, "templates", "default")
	require.NoError(t, os.MkdirAll(defaultTplDir, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(defaultTplDir, "scion-agent.yaml"),
		[]byte("env:\n  PROMPT_COMMAND: \"evil-command\"\n  BASH_ENV: \"/tmp/evil.sh\"\n"),
		0644,
	))

	agentName := "shell-no-fallback-env"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)

	// Provision with empty templateName (what Hub sends after dropping defaulted templates).
	_, _, cfg, err := ProvisionAgent(ctx, agentName, "", "", config.ShellHarnessConfigName, scionDir, "", "created", "", "")
	require.NoError(t, err)

	agentDir := filepath.Join(scionDir, "agents", agentName)
	require.NotNil(t, cfg)
	if cfg.Info != nil {
		assert.Empty(t, cfg.Info.Template, "agent-info.json must record no template for shell agents")
		assert.Empty(t, cfg.Info.TemplateHash, "agent-info.json must record no template hash for shell agents")
	}

	prov, err := readImageProvenance(agentDir)
	require.NoError(t, err)
	require.NotNil(t, prov)
	assert.Empty(t, prov.Template, "image-provenance.json must record no template for shell agents")
	assert.Empty(t, prov.TemplateHash, "image-provenance.json must record no template hash for shell agents")

	// Start the shell agent — even if TemplateEnvSource is supplied, FallbackEnv
	// and Template in RunConfig must be empty.
	var capturedRunCfg runtime.RunConfig
	ran := false
	mockRT := &runtime.MockRuntime{
		RunFunc: func(_ context.Context, rc runtime.RunConfig) (string, error) {
			ran = true
			capturedRunCfg = rc
			return "container-shell-1", nil
		},
	}
	mgr := NewManager(mockRT)
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:              agentName,
		ProjectPath:       scionDir,
		BrokerMode:        true,
		HarnessConfig:     config.ShellHarnessConfigName,
		TemplateEnvSource: "default",
		GitClone:          gc,
	})
	require.NoError(t, err)
	require.True(t, ran, "expected MockRuntime.RunFunc to be called")
	assert.Empty(t, capturedRunCfg.FallbackEnv, "shell agent Start must not pass template FallbackEnv (PROMPT_COMMAND etc.)")
	assert.Empty(t, capturedRunCfg.Template, "shell agent RunConfig.Template must be empty")
	for _, kv := range capturedRunCfg.Env {
		assert.NotContains(t, kv, "PROMPT_COMMAND=", "PROMPT_COMMAND must not leak into shell container Env")
		assert.NotContains(t, kv, "BASH_ENV=", "BASH_ENV must not leak into shell container Env")
	}
}

func TestShellAgent_ResolveAuthEnvOverlay_IgnoresSettingsHarnessConfigEnv(t *testing.T) {
	settings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			config.ShellHarnessConfigName: {
				Harness: config.ShellHarnessConfigName,
				Env: map[string]string{
					"PROMPT_COMMAND": "evil-broker-settings-cmd",
					"BASH_ENV":       "/tmp/evil.sh",
				},
			},
			"claude": {
				Harness: "claude",
				Env: map[string]string{
					"CLAUDE_SETTINGS_ENV": "allowed-for-llm",
				},
			},
		},
	}

	shellOpts := api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"OWNER_ENV": "keep-me"},
	}
	overlay, _ := resolveAuthEnvOverlay(&shellOpts, settings, "", config.ShellHarnessConfigName)
	assert.Equal(t, "keep-me", shellOpts.Env["OWNER_ENV"])
	assert.NotContains(t, shellOpts.Env, "PROMPT_COMMAND", "shell agent must not receive settings harness_configs.shell.env")
	assert.NotContains(t, shellOpts.Env, "BASH_ENV", "shell agent must not receive settings harness_configs.shell.env")
	assert.NotContains(t, overlay, "PROMPT_COMMAND")

	// Control: LLM harness config still receives its settings env.
	llmOpts := api.StartOptions{BrokerMode: true}
	llmOverlay, _ := resolveAuthEnvOverlay(&llmOpts, settings, "", "claude")
	assert.Equal(t, "allowed-for-llm", llmOpts.Env["CLAUDE_SETTINGS_ENV"])
	assert.Equal(t, "allowed-for-llm", llmOverlay["CLAUDE_SETTINGS_ENV"])
}
