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
	"errors"
	"fmt"
	"log/slog"

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

// ErrLegacyAgentStateRetired refuses the start of an agent whose only state
// is its legacy in-project directory: it was provisioned before its
// project's configuration moved to the shared project root, and that
// directory is no longer read (ptone/scion#5835). Deleting the agent removes
// the directory; recreating it provisions its state in the external root.
var ErrLegacyAgentStateRetired = errors.New("this agent was provisioned before its project's configuration moved to the shared project root, and its old state is no longer used; push its work and recreate the agent")

// refuseRetiredLegacyAgent returns ErrLegacyAgentStateRetired when
// projectDir is a shared project configuration, the agent has no state in
// stateDir (its resolved state directory, the external root) and a legacy
// in-project directory of the agent exists (config.LegacySharedProjectAgentDir).
// Such an agent is neither started from the legacy directory nor provisioned
// again over it, so its old workspace stays intact until it is deleted.
func refuseRetiredLegacyAgent(projectDir, agentName, stateDir string) error {
	if stateDir == "" || !config.IsSharedProjectConfigDir(projectDir) || agentStateProvisioned(stateDir) {
		return nil
	}
	legacy := config.LegacySharedProjectAgentDir(projectDir, agentName)
	if legacy == "" {
		return nil
	}
	slog.Warn("start refused: the agent's state is in its retired legacy directory; push its work and recreate the agent",
		"agent", agentName, "agent_dir", legacy)
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
