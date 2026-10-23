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
// (pkg/projectfs, design-v7 §P1 invariants, ptone/scion#5826): the broker
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
// projectID on the shared Layout built from settings' storage block (the
// same server.project_storage struct, resolved by config.ResolveProjectStorage
// and translated by config.ProjectStorageLayout, that the broker and hub
// build their Layouts from): Layout.Resolve of the workspace Mount, which is
// <ProjectRoot(projectID)>/workspace. backendWorkspace is the host path the
// workspace backend placed the workspace at; it must be that path. Any
// disagreement, or a Layout that cannot place the project on a shared
// backend, refuses the start rather than letting git pick a root.
func sharedLayoutWorkspaceRoot(settings *config.VersionedSettings, projectID, backendWorkspace string) (string, error) {
	var srv *config.V1ServerConfig
	if settings != nil {
		srv = settings.Server
	}
	ps, _, err := config.ResolveProjectStorage(srv)
	if err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	layout, err := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase)
	if err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	if !layout.Backend.Shared() {
		return "", fmt.Errorf("the workspace backend placed the workspace on shared storage, but the project storage layout is %q; refusing to start", layout.Backend)
	}
	if _, err := layout.ProjectRoot(projectID); err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	ws, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return "", fmt.Errorf("cannot place the workspace through the project storage layout: %w; refusing to start", err)
	}
	if !samePath(ws.HostPath, backendWorkspace) {
		return "", fmt.Errorf("the workspace backend placed the workspace at %s, but the project storage layout places it at %s; refusing to start", backendWorkspace, ws.HostPath)
	}
	return ws.HostPath, nil
}
