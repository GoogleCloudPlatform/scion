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
	"fmt"
	"io/fs"
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

// errProjectConfigSymlink is returned when a workspace's .scion is a
// symbolic link: the link itself would stay writable in the container, so
// the agent could point it elsewhere for later starts.
var errProjectConfigSymlink = errors.New("the workspace's .scion is a symbolic link; project configuration in a workspace that agents share must be a directory or a project marker file, so that it can be mounted read-only")

// errProjectConfigMarkerNoBind is returned for a workspace whose .scion is
// a project marker file on a runtime that cannot bind a single file: the
// marker would stay writable, so the agent could replace it.
var errProjectConfigMarkerNoBind = errors.New("the workspace's .scion is a project marker file, which this container runtime cannot mount read-only: on this runtime, commit a .scion/ directory to the repository, or use a non-shared workspace")

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
// Runtimes that cannot bind a single file pass false; a marker is then
// refused with errProjectConfigMarkerNoBind.
//
// ok is false when there is no .scion at all: binding a missing path would
// make the container runtime create it in the host checkout. A .scion that
// is a symbolic link is refused with errProjectConfigSymlink, and a
// workspace or .scion entry that cannot be checked is refused with an error.
func workspaceProjectConfigMount(workspace, containerWorkspace string, allowMarkerFile bool) (hostPath, target string, ok bool, err error) {
	if workspace == "" || containerWorkspace == "" {
		return "", "", false, nil
	}
	// Fail closed: a workspace or .scion entry that cannot be checked (other
	// than not existing) is refused, never left without the read-only
	// mount.
	root, err := filepath.EvalSymlinks(workspace)
	if errors.Is(err, fs.ErrNotExist) {
		// No workspace yet, so no .scion in it to protect.
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("cannot check the workspace for project configuration to mount read-only: %w", err)
	}
	candidate := filepath.Join(root, workspaceProjectConfigDir)
	info, err := os.Lstat(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("cannot check the workspace's .scion to mount it read-only: %w", err)
	}
	target = path.Join(containerWorkspace, workspaceProjectConfigDir)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "", "", false, errProjectConfigSymlink
	case info.IsDir():
		return candidate, target, true, nil
	case info.Mode().IsRegular():
		if !allowMarkerFile {
			return "", "", false, errProjectConfigMarkerNoBind
		}
		return candidate, target, true, nil
	default:
		return "", "", false, nil
	}
}

// nestedProjectConfigRel returns the path of the project's .scion entry
// (projectConfigPath, or <projectConfigPath>/.scion when that is a project
// root) relative to repoRoot, when it lies below repoRoot somewhere other
// than <repoRoot>/.scion. repoRoot must already be resolved (symlink-free),
// as buildCommonRunArgs resolves it; a caller passing an unresolved repo
// root must resolve it first.
func nestedProjectConfigRel(repoRoot, projectConfigPath string) (string, bool) {
	if repoRoot == "" || projectConfigPath == "" {
		return "", false
	}
	p := filepath.Clean(projectConfigPath)
	if filepath.Base(p) != workspaceProjectConfigDir {
		p = filepath.Join(p, workspaceProjectConfigDir)
	}
	// Resolve the parent (not the entry itself, which may be a marker
	// file or a link) the way repoRoot was resolved.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(parent, filepath.Base(p))
	}
	rel, err := filepath.Rel(repoRoot, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == workspaceProjectConfigDir {
		return "", false
	}
	return rel, true
}
