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
	"errors"
	"os"
	"path"
	"path/filepath"
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

// errProjectConfigSymlink is returned when a workspace's .scion is a
// symbolic link: the link itself would stay writable in the container, so
// the agent could point it elsewhere for later starts.
var errProjectConfigSymlink = errors.New("the workspace's .scion is a symbolic link; project configuration in a workspace that agents share must be a directory or a project marker file, so that it can be mounted read-only")

// workspaceProjectConfigMount returns the broker-side .scion entry of a
// workspace that is mounted into the container at containerWorkspace, and
// the container path it is mounted read-only at. The broker reads project
// settings, templates and harness-configs from a .scion directory when it
// starts agents in the project, so an agent that shares the workspace gets
// it read-only. Agent homes and agent state never live under it in this
// layout (they are external, see the agents mask), so the agent's own
// writable paths are unaffected.
//
// A .scion project marker file (a regular file naming the project's
// external configuration directory) is mounted read-only too when
// allowMarkerFile is set: the broker reads it to locate the project's
// configuration, so the agent must not change it or replace it with a
// directory (a mount point cannot be removed or renamed in the container).
// Runtimes that cannot bind a single file pass false and rely on the
// broker's record of the workspace's .scion state.
//
// ok is false when there is no .scion at all: binding a missing path would
// make the container runtime create it in the host checkout. A .scion that
// is a symbolic link is refused with errProjectConfigSymlink.
func workspaceProjectConfigMount(workspace, containerWorkspace string, allowMarkerFile bool) (hostPath, target string, ok bool, err error) {
	if workspace == "" || containerWorkspace == "" {
		return "", "", false, nil
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", false, nil
	}
	candidate := filepath.Join(root, workspaceProjectConfigDir)
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", "", false, nil
	}
	target = path.Join(containerWorkspace, workspaceProjectConfigDir)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "", "", false, errProjectConfigSymlink
	case info.IsDir():
		return candidate, target, true, nil
	case info.Mode().IsRegular() && allowMarkerFile:
		return candidate, target, true, nil
	default:
		return "", "", false, nil
	}
}

// isLinkedWorktree reports whether dir is a linked git worktree: its .git
// is a file pointing at the main repository's admin dir.
func isLinkedWorktree(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && info.Mode().IsRegular()
}
