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
