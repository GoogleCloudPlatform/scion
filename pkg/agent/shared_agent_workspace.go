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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentRuntimeKey carries the agent manager's runtime name
// (contextWithAgentRuntime).
type agentRuntimeKey struct{}

// contextWithAgentRuntime records the runtime the agent manager starts the
// agent on, for decisions made below Start, Provision and Reprovision
// (sharedLayoutAgentWorkspace).
func contextWithAgentRuntime(ctx context.Context, runtimeName string) context.Context {
	return context.WithValue(ctx, agentRuntimeKey{}, runtimeName)
}

// agentRuntimeFromContext is the runtime name contextWithAgentRuntime
// recorded, or "" when none was.
func agentRuntimeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(agentRuntimeKey{}).(string)
	return v
}

// runtimeName is the manager's runtime name, or "" without a runtime.
func (m *AgentManager) runtimeName() string {
	if m == nil || m.Runtime == nil {
		return ""
	}
	return m.Runtime.Name()
}

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
//   - the agent's runtime (agentRuntimeFromContext) is not Kubernetes,
//     including a caller that recorded none. Only Kubernetes creates the
//     agent's directory on the share (the NFS leaf helpers and the workspace
//     init container), so every other runtime keeps the broker-local
//     workspace it had before. For clone-per-agent the workspace source
//     guard refuses that workspace (it is outside the project's root on the
//     share), so such a start fails closed, as before; empty-per-agent is
//     admitted (its own workspace is its guard root). Placing it on the
//     share for other runtimes is a follow-up;
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
	if !isKubernetesRuntime(agentRuntimeFromContext(ctx)) {
		return "", nil
	}
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

// startWorkspaceSharingMode is the sharing mode Start selects the workspace
// backend with, before the Kubernetes worktree adjustment: shared-plain for
// a shared workspace or a git clone, empty-per-agent for an empty-per-agent
// agent (it has no shared checkout: without NFS storage it is node-local, or
// pod-local EmptyDir on Kubernetes; with NFS storage it gets its own agent
// directory on the export, design #2703 P3, which only the Kubernetes
// runtime can mount, see nfsEmptyAgentDirSelection), and worktree-per-agent
// otherwise.
func startWorkspaceSharingMode(opts api.StartOptions, emptyPerAgent bool) store.WorkspaceSharingMode {
	switch {
	case emptyPerAgent:
		return store.SharingModeEmptyPerAgent
	case opts.SharedWorkspace || opts.GitClone != nil:
		return store.SharingModeSharedPlain
	default:
		return store.SharingModeWorktreePerAgent
	}
}

// startUsesNFSWorkspaceBackend reports whether Start lays out the workspace
// through the nfs workspace backend (runtime.SelectWorkspaceBackend). The
// Kubernetes worktree adjustment Start applies later (shared-plain to
// worktree-per-agent) does not change the answer: the nfs backend serves
// both modes. Today the mode argument cannot change the nfs answer for any
// mode Start passes (shared-plain, worktree-per-agent, empty-per-agent all
// select nfs when the backend is nfs); it is passed so the answer stays
// Start's own if SelectWorkspaceBackend changes.
func startUsesNFSWorkspaceBackend(settings *config.VersionedSettings, opts api.StartOptions, emptyPerAgent bool) bool {
	if settings == nil || settings.Server == nil {
		return false
	}
	ws := settings.Server.EffectiveWorkspaceStorage()
	if ws == nil {
		return false
	}
	return runtime.SelectWorkspaceBackend(ws, startWorkspaceSharingMode(opts, emptyPerAgent)).Name() == "nfs"
}

// checkStartWorkspaceExists is Start's safety net before the workspace is
// mounted: GetAgent should have created or recreated workspace, but if it is
// still missing the start fails with a clear error rather than a cryptic
// mount error from the container runtime.
//
// A Kubernetes start through the nfs workspace backend
// (startUsesNFSWorkspaceBackend) skips it. There the workspace is created
// later in Start, by the NFS leaf helpers (which create the leaf, with its
// atomic claim, or return an error), by the Kubernetes workspace init
// container, or the start is refused (errEmptyPerAgentNFSNoClaim). An
// agent-scoped workspace on a shared Layout does not exist yet on a fresh
// agent, because provisioning leaves it to those helpers. Every other start
// keeps the check, including an nfs start on another runtime: nothing
// creates the workspace there, and a bind mount of a missing path could
// create it with the wrong owner.
func checkStartWorkspaceExists(runtimeName, workspace string, settings *config.VersionedSettings, opts api.StartOptions, emptyPerAgent bool) error {
	if workspace == "" || (isKubernetesRuntime(runtimeName) && startUsesNFSWorkspaceBackend(settings, opts, emptyPerAgent)) {
		return nil
	}
	if _, err := os.Stat(workspace); os.IsNotExist(err) {
		return fmt.Errorf("workspace directory does not exist: %s (try deleting and recreating the agent)", workspace)
	}
	return nil
}
