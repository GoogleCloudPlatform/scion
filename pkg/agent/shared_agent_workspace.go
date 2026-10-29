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
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// clonePerAgentWorkspaceKey marks a provisioning or start context whose
// agent uses the clone-per-agent sharing mode (contextWithClonePerAgentWorkspace).
type clonePerAgentWorkspaceKey struct{}

// isClonePerAgentStart reports whether opts asks for a clone-per-agent
// workspace: a git clone, with the clone-per-agent sharing mode in the
// request env (SCION_WORKSPACE_MODE). A git clone in any other mode (for
// example the worktree-per-agent dispatch on Kubernetes, which also
// carries the clone) is not.
func isClonePerAgentStart(opts api.StartOptions) bool {
	return opts.GitClone != nil &&
		store.ResolveWorkspaceSharingMode(opts.Env["SCION_WORKSPACE_MODE"]) == store.SharingModeClonePerAgent
}

// contextWithClonePerAgentWorkspace marks ctx when opts asks for a
// clone-per-agent workspace (isClonePerAgentStart).
func contextWithClonePerAgentWorkspace(ctx context.Context, opts api.StartOptions) context.Context {
	if !isClonePerAgentStart(opts) {
		return ctx
	}
	return context.WithValue(ctx, clonePerAgentWorkspaceKey{}, true)
}

// isClonePerAgentWorkspace reports whether ctx was marked by
// contextWithClonePerAgentWorkspace.
func isClonePerAgentWorkspace(ctx context.Context) bool {
	v, _ := ctx.Value(clonePerAgentWorkspaceKey{}).(bool)
	return v
}

// agentScopedWorkspace reports whether ctx's agent uses an agent-scoped
// sharing mode, empty-per-agent or clone-per-agent: the modes whose
// workspace is the agent's own directory (sharedLayoutAgentWorkspace).
func agentScopedWorkspace(ctx context.Context) bool {
	return api.IsEmptyPerAgentWorkspaceFromContext(ctx) || isClonePerAgentWorkspace(ctx)
}

// sharedLayoutAgentWorkspace returns the workspace directory of an
// agent-scoped workspace (clone-per-agent or empty-per-agent) of a project
// whose configuration is on a shared Layout: <AgentRoot>/workspace, where
// AgentRoot is projectfs.Layout.AgentRoot(projectID, agentName)
// (<HostBase>/<subpath_root>/<projectID>/agents/<agent>), the directory
// runtime.NFSAgentDirSubPath and the NFS workspace leaf helpers address.
//
// The agent's broker-private state directory (agentDir) is broker-local for
// such a project (config.AgentsRootForProject), so the workspace is never
// derived from it: <agentDir>/workspace would put the agent's workspace off
// the share, outside the project's tree.
//
// It returns "" (and no error) when the rule does not apply, and the caller
// keeps <agentDir>/workspace:
//   - projectDir is not a shared project configuration directory
//     (config.IsSharedProjectConfigDir);
//   - agentDir is the agent's legacy in-project directory
//     (config.LegacySharedProjectAgentDir), whose workspace stays where it
//     is until the agent is recreated (legacyAgentWorkspaceRoot);
//   - the project storage Layout is not a shared one.
//
// The Layout is built from settings (config.ResolveProjectStorage and
// config.ProjectStorageLayout), or loaded for projectDir when settings is
// nil. A Layout that cannot be built, or whose project root is not the
// directory holding projectDir, is an error: the start is refused rather
// than placing the workspace somewhere else.
func sharedLayoutAgentWorkspace(ctx context.Context, settings *config.VersionedSettings, projectDir, agentName, agentDir string) (string, error) {
	if !config.IsSharedProjectConfigDir(projectDir) {
		return "", nil
	}
	if legacy := config.LegacySharedProjectAgentDir(projectDir, agentName); legacy != "" && samePath(legacy, agentDir) {
		return "", nil
	}
	if settings == nil {
		vs, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
		if err != nil {
			return "", fmt.Errorf("agent workspace on the shared project tree: load settings: %w", err)
		}
		settings = vs
	}
	var srv *config.V1ServerConfig
	if settings != nil {
		srv = settings.Server
	}
	ps, _, err := config.ResolveProjectStorage(srv)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	layout, err := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	if !layout.Backend.Shared() {
		return "", nil
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	// IsSharedProjectConfigDir checked that the parent's name is the
	// project ID recorded in the configuration.
	projectID := filepath.Base(filepath.Dir(abs))
	projectRoot, err := layout.ProjectRoot(projectID)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	if !samePath(projectRoot.HostPath, filepath.Dir(abs)) {
		return "", fmt.Errorf("agent workspace on the shared project tree: the project storage layout places project %s at %s, not at %s; refusing to start", projectID, projectRoot.HostPath, filepath.Dir(abs))
	}
	agentRoot, err := layout.AgentRoot(projectID, agentName)
	if err != nil {
		return "", fmt.Errorf("agent workspace on the shared project tree: %w", err)
	}
	return filepath.Join(agentRoot.HostPath, "workspace"), nil
}
