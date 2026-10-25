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

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
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

// errOutsideRootNotRegistered is returned when a workspace outside the repository
// root is not a registered worktree of it: the outside-root fallback then mounts
// nothing and refuses, rather than binding the whole checkout.
var errOutsideRootNotRegistered = errors.New("workspace is outside the project repository root and is not a registered worktree of it; refusing to mount the whole repository")

// errAppleSharedWorkspaceUnsupported is returned when a shared-workspace git layout
// is requested on the Apple runtime, which cannot bind a per-agent read-only
// .git/config without breaking the main worktree.
var errAppleSharedWorkspaceUnsupported = errors.New("the shared-workspace git layout is not supported on the Apple runtime, which cannot make .git/config read-only without breaking the main worktree; use a worktree-per-agent project or a different runtime")

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
	// The entry and its container target come from the layout authority
	// (projectfs.ProjectConfigEntry, realized as a bind), never joined here.
	entry, err := projectfs.ProjectConfigEntry(projectfs.Resolved{
		Backend: projectfs.BackendLocal, Kind: projectfs.KindWorkspace, HostPath: root, Target: containerWorkspace,
	})
	if err != nil {
		return "", "", false, err
	}
	realized, err := projectfs.Layout{Backend: projectfs.BackendLocal}.Realize(entry, projectfs.RuntimeBind)
	if err != nil {
		return "", "", false, err
	}
	candidate := realized.Source
	info, err := os.Lstat(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("cannot check the workspace's .scion to mount it read-only: %w", err)
	}
	target = realized.Target
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

// ensureAgentsMaskPoint creates <src>/agents, the mount point of the agents
// mask under the read-only project configuration directory src, on the
// runtimes that mask the agents directory whenever src is a directory
// (Docker and Podman). The read-only mount leaves the container runtime no
// way to create it, so a failure to create it, or an existing entry that is
// not a real directory, refuses the start instead of running without the
// mask. Apple masks only an existing agents directory and creates nothing;
// a src that is not a directory (a marker file) has no agents directory.
func ensureAgentsMaskPoint(config RunConfig, src string) error {
	if config.RuntimeName == appleContainerRuntimeName || !isDir(src) {
		return nil
	}
	agents := filepath.Join(src, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return fmt.Errorf("cannot create the project's agents directory to mask it: %w", err)
	}
	info, err := os.Lstat(agents)
	if err != nil {
		return fmt.Errorf("cannot check the project's agents directory to mask it: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the project's agents directory is not a directory (%s), so it cannot be masked", info.Mode().Type())
	}
	return nil
}

// agentsMaskLstat is the os.Lstat seam used only by requireExistingAgentsMask,
// so tests can inject an entry type or a stat error for src or <src>/agents
// without a privileged filesystem setup. Tests swap it and restore it with
// t.Cleanup, and must not run in parallel.
var agentsMaskLstat = os.Lstat

// errAgentsMaskPointMissing is returned on Docker and Podman when a nested or
// outside-root own-checkout project .scion directory has no agents directory.
// This runtime masks .scion/agents in the container but, unlike the
// workspace-root layouts, never creates it under these read-only binds, so the
// start is refused until the directory exists.
var errAgentsMaskPointMissing = errors.New("the project's .scion directory has no agents directory; this runtime masks .scion/agents in the container and does not create it in this directory")

// errAgentsMaskNotDir is returned when a project .scion entry, or its agents
// entry, exists but is not a real directory, so it cannot be masked and is not
// created in its place.
var errAgentsMaskNotDir = errors.New("the project's .scion/agents entry is not a directory, so it cannot be masked")

// requireExistingAgentsMask requests a tmpfs mask of <tgt>/agents for a project
// .scion bound read-only at tgt whose broker-side source is src. It NEVER
// creates anything: these binds (a nested .scion, or an outside-root own
// checkout's .scion) are the user's checkout, which the runtime must not write
// to. src and <src>/agents are classified with os.Lstat (never following a
// symlink), so a link or other non-directory fails closed rather than masking
// the wrong target. On Docker and Podman an absent agents directory refuses the
// start, so every start that succeeds already has a real directory to mask;
// Apple masks only an existing directory and creates nothing. The mask target is
// handed to addMask, which the caller emits as a tmpfs AFTER the bind mounts, so
// the mask lands over the bind. It is the single no-create code path shared by
// the nested and outside-root own-.scion cases.
func requireExistingAgentsMask(config RunConfig, src, tgt string, addMask func(maskTarget string)) error {
	srcInfo, err := agentsMaskLstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// workspaceProjectConfigMount already found src, so a not-exist here
			// is a race: binding a missing source would make the runtime create
			// it and start unmasked. Fail closed.
			return fmt.Errorf("agent %q: %s disappeared before the start: %w", config.Name, src, err)
		}
		return fmt.Errorf("agent %q: cannot check %s to mask it: %w", config.Name, src, err)
	}
	switch mode := srcInfo.Mode(); {
	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("agent %q: %s: %w", config.Name, src, errProjectConfigSymlink)
	case mode.IsDir():
		// Fall through to the agents/ entry below.
	case mode.IsRegular():
		// A project marker file has no agents/ to mask.
		return nil
	default:
		return fmt.Errorf("agent %q: %s (%s): %w; replace it with a directory", config.Name, src, agentsMaskEntryType(mode), errAgentsMaskNotDir)
	}

	agentsPath := filepath.Join(src, "agents")
	agentsInfo, err := agentsMaskLstat(agentsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if config.RuntimeName == appleContainerRuntimeName {
				// Apple masks only an existing agents dir and creates nothing.
				return nil
			}
			return fmt.Errorf("agent %q: %s: %w; run: mkdir %q and start the agent again", config.Name, src, errAgentsMaskPointMissing, agentsPath)
		}
		return fmt.Errorf("agent %q: cannot check %s to mask it: %w", config.Name, agentsPath, err)
	}
	if !agentsInfo.Mode().IsDir() {
		return fmt.Errorf("agent %q: %s (%s): %w; replace it with a directory", config.Name, agentsPath, agentsMaskEntryType(agentsInfo.Mode()), errAgentsMaskNotDir)
	}
	addMask(path.Join(tgt, "agents"))
	return nil
}

// agentsMaskEntryType names a non-directory entry type for the fail-closed mask
// errors: "symlink", "regular file", or "special file (<type>)".
func agentsMaskEntryType(mode fs.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "regular file"
	default:
		return "special file (" + mode.Type().String() + ")"
	}
}

// applyNestedProjectConfig handles a project whose configuration directory lies
// in a SUBDIRECTORY of the bound workspace (ProjectConfigPath is not
// <workspace>/.scion), so that binding the workspace read-write would otherwise
// leave the real <workspace>/<sub>/.scion (settings, templates, agents/)
// writable in the container. It binds that nested .scion READ-ONLY and masks its
// agents/ via requireExistingAgentsMask (emitted after the bind mounts, and
// never created under the read-only bind). A symlinked or (on a no-file-bind
// runtime) marker nested .scion is refused fail-closed (errProjectConfigSymlink
// / errProjectConfigMarkerNoBind). It is a no-op when the project config is at
// <workspace>/.scion or absent. containerWorkspace is where the workspace is
// bound (e.g. "/workspace" or "/repo-root/<rel>").
func applyNestedProjectConfig(config RunConfig, containerWorkspace string, registerMount func(string, string, bool, bool), addMask func(maskTarget string)) error {
	rel, ok := nestedProjectConfigRel(config.Workspace, config.ProjectConfigPath)
	if !ok {
		return nil
	}
	subdir := filepath.Dir(rel) // rel is "<sub>/.scion"
	projectDir := filepath.Join(config.Workspace, subdir)
	containerProjectDir := path.Join(containerWorkspace, filepath.ToSlash(subdir))
	src, tgt, ok, err := workspaceProjectConfigMount(projectDir, containerProjectDir, config.RuntimeName != appleContainerRuntimeName)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	registerMount(src, tgt, true, true) // nested .scion read-only (after the /workspace bind)
	// Mask the nested .scion's agents/ when it exists, failing closed otherwise;
	// requireExistingAgentsMask never creates anything under this read-only bind.
	return requireExistingAgentsMask(config, src, tgt, addMask)
}
