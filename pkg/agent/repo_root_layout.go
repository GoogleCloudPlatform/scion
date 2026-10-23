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
