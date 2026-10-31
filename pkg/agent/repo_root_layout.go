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
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// Candidate roots for detectRepoRoot come from the layout authority
// (pkg/projectfs, ptone/scion#5826): the broker
// knows where a project's workspace is from the Layout, and git only checks
// that answer.

// localLayoutWorkspaceRoot returns the project-level workspace the local
// Layout places for a project whose own root directory is projectRoot
// (Layout.Resolve of the shared-plain workspace Mount: the project root
// itself). An empty projectRoot is returned as is.
func localLayoutWorkspaceRoot(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	r, err := projectfs.Layout{Backend: projectfs.BackendLocal}.Resolve(
		projectfs.Project{Dir: projectRoot}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return projectRoot
	}
	return r.HostPath
}

// sharedLayoutWorkspaceRoot returns the project-level workspace of project
// projectID on the shared Layout of the workspace storage settings selects
// (settings.Server.EffectiveWorkspaceStorage: server.project_storage's
// workspace view, or the deprecated server.workspace_storage), translated by
// config.NFSLayout, the same translation the nfs workspace backend and the
// broker's Layout (config.ProjectStorageLayout) use: Layout.Resolve of the
// workspace Mount, which is <ProjectRoot(projectID)>/workspace.
// backendWorkspace is the host path the workspace backend placed the
// workspace at; it must be that path. Any disagreement, or a Layout that
// cannot place the project, refuses the start rather than letting git pick
// a root. Only the nfs backend places a workspace on this host.
func sharedLayoutWorkspaceRoot(settings *config.VersionedSettings, projectID, backendWorkspace string) (string, error) {
	var ws *config.V1WorkspaceStorageConfig
	if settings != nil {
		ws = settings.Server.EffectiveWorkspaceStorage()
	}
	if ws == nil || ws.Backend != config.WorkspaceStorageBackendNFS {
		backend := "local"
		if ws != nil && ws.Backend != "" {
			backend = ws.Backend
		}
		return "", fmt.Errorf("the workspace backend placed the workspace on shared storage, but the project storage layout is %q; refusing to start", backend)
	}
	layout, err := config.NFSLayout(ws.NFS)
	if err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	if !layout.Backend.Shared() {
		return "", fmt.Errorf("the workspace backend placed the workspace on shared storage, but the project storage layout is %q; refusing to start", layout.Backend)
	}
	if _, err := layout.ProjectRoot(projectID); err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	resolved, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	if !samePath(resolved.HostPath, backendWorkspace) {
		return "", fmt.Errorf("the workspace backend placed the workspace at %s, but the project storage layout places it at %s; refusing to start", backendWorkspace, resolved.HostPath)
	}
	return resolved.HostPath, nil
}

// legacyAgentWorkspaces records the agents whose legacy workspace admission
// was logged (once per agent directory).
var legacyAgentWorkspaces sync.Map

// legacyAgentWorkspaceRoot returns the extra permitted workspace root of an
// agent provisioned before its project's configuration moved to the shared
// project root (ptone/scion#5823): exactly that agent's legacy state
// directory ~/.scion/projects/<slug>/.scion/agents/<name>, and only when
// agentDir (its resolved state dir) is that directory, reached through the
// legacy look-up fallback (config.LegacySharedProjectAgentDir), and the
// agent's workspace is its own (sharedOrExplicit false). An agent-scoped
// workspace there (clone-per-agent, for example) may hold unpushed work, so
// it keeps being admitted until the agent is recreated; a shared or
// project-scoped workspace is never admitted from the legacy tree, nor is
// another agent's directory. It ends for each agent when that agent is
// deleted, which removes the legacy directory (ptone/scion#5835); the code
// path is removed in a later release (ptone/scion#5839).
func legacyAgentWorkspaceRoot(projectDir, agentName, agentDir string, sharedOrExplicit bool) string {
	if sharedOrExplicit || agentDir == "" {
		return ""
	}
	legacy := config.LegacySharedProjectAgentDir(projectDir, agentName)
	if legacy == "" || filepath.Clean(agentDir) != filepath.Clean(legacy) {
		return ""
	}
	if _, logged := legacyAgentWorkspaces.LoadOrStore(legacy, struct{}{}); !logged {
		slog.Warn("legacy agent workspace admitted until this agent is recreated; push its work and recreate it at your convenience",
			"agent", agentName, "agent_dir", legacy)
	}
	return legacy
}

// ErrLegacyAgentStateRetired refuses the create of a new agent whose name
// resolves, through the legacy look-up fallback, to a legacy in-project
// agent directory: that directory belongs to an earlier agent of the same
// name, provisioned before its project's configuration moved to the shared
// project root, whose record is gone. Nothing may start from it, and the new
// agent must not reuse its state. Deleting the new agent removes the
// directory (ptone/scion#5835); creating it again then provisions it in the
// external root. It is an agent-state conflict (config.IsAgentStateConflict),
// so brokers answer it with 409 and its message.
var ErrLegacyAgentStateRetired error = legacyAgentStateRetiredError{}

type legacyAgentStateRetiredError struct{}

func (legacyAgentStateRetiredError) Error() string {
	return "an earlier agent of this name, provisioned before its project's configuration moved to the shared project root, left its state on this broker and no agent owns it any more; delete this agent to remove that state, then create it again"
}

func (legacyAgentStateRetiredError) Unwrap() error { return config.ErrAgentStateConflict }

// refuseRetiredLegacyAgent returns ErrLegacyAgentStateRetired for a create
// (opts.FreshProvision) of an agent of a shared project configuration that
// has no state in the external agents root while a legacy in-project
// directory of its name exists (config.LegacySharedProjectAgentDir): a
// legacy directory with no live agent record. Starts and restarts of an
// existing legacy agent are not refused; they keep the read-only fallback
// and the narrow workspace admission until the agent is deleted.
func refuseRetiredLegacyAgent(projectDir string, opts api.StartOptions) error {
	if !opts.FreshProvision || !config.IsSharedProjectConfigDir(projectDir) {
		return nil
	}
	legacy := config.LegacySharedProjectAgentDir(projectDir, opts.Name)
	if legacy == "" {
		return nil
	}
	// The same predicate as the look-up (config.HasExternalAgentState), so
	// a create is admitted exactly when every look-up resolves the agent to
	// the external root, never to the legacy directory. An agents root that
	// cannot be resolved refuses (fail closed).
	if root, err := config.AgentsRootForProject(projectDir, false, opts.HubProjectID); err == nil &&
		config.HasExternalAgentState(filepath.Join(root, opts.Name)) {
		return nil
	}
	slog.Warn("create refused: the agent name resolves to a legacy agent directory no agent owns",
		"agent", opts.Name, "agent_dir", legacy)
	return ErrLegacyAgentStateRetired
}

// composeSharedRepoRoot is composeRepoRoot for a workspace the backend placed
// on the shared tree: the derived root falls back to the shared layout's
// workspace (sharedLayoutWorkspaceRoot, which also checks that the backend's
// path is that one) instead of the local layout's, and the fail-closed record
// intersection (reconcileRepoRoot) is the same. An explicit workspace derives
// no root, as in detectRepoRoot.
func composeSharedRepoRoot(agentName, candidate string, rec repoRootRecordState, workspace, projectDir, projectID string, explicit bool, settings *config.VersionedSettings) (string, error) {
	derived := validatedWorktreeRepoRoot(candidate, workspace)
	var detectErr error
	if derived == "" && !explicit {
		var sharedRoot string
		sharedRoot, detectErr = sharedLayoutWorkspaceRoot(settings, projectID, workspace)
		if detectErr == nil {
			derived, detectErr = detectRepoRootFrom(workspace, projectDir, sharedRoot)
		}
	}
	return reconcileRepoRoot(agentName, derived, detectErr, rec)
}
