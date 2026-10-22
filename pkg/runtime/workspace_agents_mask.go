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

package runtime

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// sharedWorkspaceAgentsDir is the in-project agents root, relative to the
// workspace. Agent state for shared-workspace projects lives in the
// broker-side agent dir, so the runtimes keep this path out of the
// container's view of a shared workspace, to keep per-agent state dirs out
// of the shared workspace view on all runtimes (ptone/scion#5513).
const sharedWorkspaceAgentsDir = ".scion/agents"

// sharedWorkspaceAgentsMaskTarget returns the container path of the
// in-project agents root of a shared workspace mount, and whether it is to
// be kept out of the container's view. Callers invoke it only where they
// mount cfg.Workspace as the project's shared workspace at
// containerWorkspace.
//
// The condition: the workspace is the repo root itself (compared after
// resolving symlinks on both paths), and <workspace>/.scion is a directory
// on the broker. With requireAgentsDir, <workspace>/.scion/agents must
// already be a directory as well: a runtime whose mask would otherwise make
// the container runtime create that mount point in the shared tree passes
// it, so the mask never creates anything there. When the directory is
// missing there is nothing to hide.
//
// If either path cannot be resolved, the result is false and the reason is
// logged.
func sharedWorkspaceAgentsMaskTarget(cfg RunConfig, containerWorkspace string, requireAgentsDir bool) (string, bool) {
	if cfg.RepoRoot == "" || cfg.Workspace == "" || containerWorkspace == "" {
		return "", false
	}
	repoRoot, err := filepath.EvalSymlinks(cfg.RepoRoot)
	if err != nil {
		runtimeLog().Warn("Not masking the in-project agents dir: repo root does not resolve",
			"agent", cfg.Name, "repo_root", cfg.RepoRoot, "error", err)
		return "", false
	}
	workspace, err := filepath.EvalSymlinks(cfg.Workspace)
	if err != nil {
		runtimeLog().Warn("Not masking the in-project agents dir: workspace does not resolve",
			"agent", cfg.Name, "workspace", cfg.Workspace, "error", err)
		return "", false
	}
	if repoRoot != workspace {
		return "", false
	}
	if !isDir(filepath.Join(workspace, ".scion")) {
		return "", false
	}
	if requireAgentsDir && !isDir(filepath.Join(workspace, filepath.FromSlash(sharedWorkspaceAgentsDir))) {
		runtimeLog().Debug("Not masking the in-project agents dir: it does not exist",
			"agent", cfg.Name, "workspace", workspace)
		return "", false
	}
	return path.Join(containerWorkspace, sharedWorkspaceAgentsDir), true
}

// isDir reports whether p is a directory, following symlinks.
func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// workspaceProjectConfigDir is the project configuration directory,
// relative to a workspace mounted at the container's workspace path.
const workspaceProjectConfigDir = ".scion"

// workspaceProjectConfigMount returns the broker-side .scion directory of
// a workspace that is mounted into the container at containerWorkspace,
// and the container path it is mounted read-only at. The broker reads
// project settings, templates and harness-configs from that directory when
// it starts agents in the project, so an agent that shares the workspace
// gets it read-only. Agent homes and agent state never live under it in
// this layout (they are external, see the agents mask), so the agent's own
// writable paths are unaffected.
//
// ok is false when <workspace>/.scion is not a directory on the broker (a
// project marker file, or no .scion at all): there is no project
// configuration in the workspace to protect, and binding a missing path
// would make the container runtime create it in the host checkout. It is
// also false when .scion is a symlink to a directory outside the workspace,
// which the container cannot reach.
func workspaceProjectConfigMount(workspace, containerWorkspace string) (hostPath, target string, ok bool) {
	if workspace == "" || containerWorkspace == "" {
		return "", "", false
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, workspaceProjectConfigDir))
	if err != nil || !isDir(resolved) {
		return "", "", false
	}
	// A .scion symlink is followed the way the broker follows it. Its
	// target is only reachable in the container when it lies inside the
	// workspace; that path is the one mounted read-only.
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	return resolved, path.Join(containerWorkspace, filepath.ToSlash(rel)), true
}
