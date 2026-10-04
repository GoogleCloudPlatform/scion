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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"golang.org/x/sys/unix"
)

// nfsWorktreeSelection decides whether an agent on the NFS workspace backend
// gets its own git worktree (worktrees/<agent name> under the project's
// shared checkout) instead of mounting the shared checkout itself.
//
// It returns the worktree's directory name and branch when all of these
// hold, and two empty strings otherwise:
//   - the dispatch carries the canonical workspace mode worktree-per-agent
//     (SCION_WORKSPACE_MODE, set by the broker for hub-dispatched starts);
//   - the project is git-backed (gitClone has a URL);
//   - agentName is an agent slug (see isNFSWorktreeName).
//
// The directory is named after the agent, as on the local runtimes, so the
// broker can find it again when the agent is deleted, and an agent
// recreated with the same name finds its old worktree when the delete left
// it in place.
//
// Every other case, including clone-per-agent, shared-plain, a missing mode
// and non-git projects, keeps the layout used before: every agent mounts
// <subpath_root>/<projectID>/workspace.
//
// The branch is SCION_AGENT_BRANCH when set, otherwise the agent name, the
// same choice the broker makes for worktrees on the local runtimes.
func nfsWorktreeSelection(env map[string]string, gitClone *api.GitCloneConfig, agentName string) (name, branch string) {
	if env == nil || gitClone == nil || gitClone.URL == "" {
		return "", ""
	}
	if store.ResolveWorkspaceSharingMode(env["SCION_WORKSPACE_MODE"]) != store.SharingModeWorktreePerAgent {
		return "", ""
	}
	if !isNFSWorktreeName(agentName) {
		return "", ""
	}
	branch = env["SCION_AGENT_BRANCH"]
	if branch == "" {
		branch = agentName
	}
	return agentName, branch
}

// isNFSWorktreeName reports whether name can name an agent's worktree
// directory: it must already be an agent slug (api.ValidateAgentName returns
// it unchanged), which keeps it to lower-case letters, digits and dashes,
// and so a single path segment.
func isNFSWorktreeName(name string) bool {
	slug, err := api.ValidateAgentName(name)
	return err == nil && slug == name
}

// ensureNFSWorktreeLeaf creates the agent's empty worktree directory
// (<workspace>/worktrees/<agent name>) on the broker's mount of the export
// before the pod exists, with the same helper and modes as
// ensureNFSWorkspaceLeaf. The agent container mounts that directory by
// subPath, so it must exist before the pod starts: the provisioning init
// container then adds the worktree into the empty directory. With an older
// image whose init container only provisions the shared checkout, the
// directory stays empty and the agent container's own clone step fills it,
// so the agent still gets its own checkout.
//
// The directory is only created once the shared checkout has been
// provisioned (its .scion-provisioned file exists). Before the first clone
// the workspace directory must stay empty apart from the provisioning lock,
// so the clone can run; the init container then creates the worktree
// itself before the agent container starts.
//
// It has the same no-op and fallback cases as ensureNFSWorkspaceLeaf (not
// Kubernetes, no claim, export not mounted on the broker, or a directory
// the broker is not allowed to create), and prepared has the same meaning.
// An existing directory is left exactly as it is.
func ensureNFSWorktreeLeaf(runtimeName string, resolved runtime.ResolvedWorkspace, pvClaimName, name string) (prepared bool, err error) {
	if !isKubernetesRuntime(runtimeName) || resolved.Backend != "nfs" || pvClaimName == "" {
		return false, nil
	}
	if !isNFSWorktreeName(name) {
		return false, fmt.Errorf("workspace_storage nfs: invalid agent name %q", name)
	}
	rel := resolved.ServerRelativePath
	if rel == "" || !filepath.IsLocal(rel) || filepath.Join(resolved.HostBase, rel) != resolved.HostPath {
		return false, fmt.Errorf("workspace_storage nfs: unexpected workspace path %q under %q", rel, resolved.HostBase)
	}
	leaf := nfsWorktreeSubPath(rel, name)

	info, err := os.Stat(resolved.HostBase)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Info("workspace_storage nfs: export not mounted on this broker; the node will create the agent's worktree directory "+
				"when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
				"host_base", resolved.HostBase, "sub_path", leaf)
			return false, nil
		}
		return false, fmt.Errorf("workspace_storage nfs: check export mount %q: %w; %s", resolved.HostBase, err, nfsWorkspaceExportHint)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("workspace_storage nfs: check export mount %q: not a directory; %s", resolved.HostBase, nfsWorkspaceExportHint)
	}
	resolvedHostBase, err := filepath.EvalSymlinks(resolved.HostBase)
	if err != nil {
		return false, fmt.Errorf("workspace_storage nfs: resolve export mount %q: %w", resolved.HostBase, err)
	}

	provisioned, err := nfsSharedCheckoutProvisioned(resolvedHostBase, rel)
	if err != nil && isNFSLeafPermissionError(err) {
		// The broker cannot look into the state directory (for example one
		// the node created and the init container gave to the agents' user
		// and group) and found no legacy sentinel: leave the worktree
		// directory to the node, as for a directory the broker may not
		// create.
		slog.Warn("workspace_storage nfs: the broker could not check whether the shared checkout is provisioned, so the node will create "+
			"the agent's worktree directory when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
			"host_base", resolved.HostBase, "sub_path", leaf, "error", err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !provisioned {
		// First provisioning of this project: the init container
		// clones the shared checkout and adds the worktree.
		return true, nil
	}

	ok, err := ensureNFSLeaf(resolvedHostBase, leaf)
	if err != nil && isNFSLeafPermissionError(err) {
		slog.Warn("workspace_storage nfs: the broker could not create the agent's worktree directory, so the node will create it "+
			"when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
			"host_base", resolved.HostBase, "sub_path", leaf, "error", err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workspace_storage nfs: create directory %q on the export mounted at %q: %w; %s",
			leaf, resolved.HostBase, err, nfsWorkspaceExportHint)
	}
	return ok, nil
}

// nfsSharedCheckoutProvisioned reports whether the project's shared
// checkout at rel (relative to the resolved export mount hostBase) has been
// provisioned: its sentinel is in the project's provisioning state directory
// (<project>/provision), or, for a workspace provisioned before that
// directory existed, in the workspace itself. Lstat is used, so a symlink
// named like the sentinel also counts, as before.
//
// A permission error on either location does not end the check: the other
// location is still checked, and the first permission error is returned
// (wrapped, so isNFSLeafPermissionError matches it) only when the sentinel
// is found in neither. Any other error than "does not exist" is returned
// straight away.
func nfsSharedCheckoutProvisioned(hostBase, rel string) (bool, error) {
	var permErr error
	for _, dir := range []string{provision.ProjectStateDir(rel), rel} {
		_, err := os.Lstat(filepath.Join(hostBase, dir, provision.ProvisionSentinelFile))
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
		case isNFSLeafPermissionError(err) && permErr == nil:
			permErr = err
		default:
			return false, fmt.Errorf("workspace_storage nfs: check provisioning state of %q: %w", rel, err)
		}
	}
	if permErr != nil {
		return false, fmt.Errorf("workspace_storage nfs: check provisioning state of %q: %w", rel, permErr)
	}
	return false, nil
}

// nfsRemovalStateDir returns the provisioning state directory whose lock
// RemoveNFSWorktree takes after the legacy lock in the workspace, the same
// order as the provisioning init containers. The path is derived the same
// way as the runtime's mount (runtime.NFSProvisionStateSubPath) and the
// directory is created like before a pod starts (ensureNFSLeaf, no symlink
// following).
//
// When the broker is not allowed to create, open or write in the directory
// (a permission error, or no write and search access to an existing one),
// it returns "" with a warning: the removal then takes only the legacy lock,
// which every provisioning init container and broker takes first while
// legacy support exists, so it still excludes them all. A symlink, a
// regular file or any other unusable path is an error, and the worktree is
// left in place.
//
// TODO(ptone/scion#2974): when the legacy lock is dropped, this fallback
// must go: the state-directory lock is then the only lock, and a removal
// that cannot take it must fail.
func nfsRemovalStateDir(hostBase, rel, projectID string) (string, error) {
	stateRel, err := runtime.NFSProvisionStateSubPath(rel, projectID)
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: %w; left in place", err)
	}
	stateDir := filepath.Join(hostBase, stateRel)
	_, err = ensureNFSLeaf(hostBase, stateRel)
	if err == nil {
		err = unix.Access(stateDir, unix.W_OK|unix.X_OK)
	}
	if err != nil && isNFSLeafPermissionError(err) {
		slog.Warn("workspace_storage nfs: the broker cannot use the provisioning state directory; removing the worktree under the legacy provisioning lock only",
			"path", stateDir, "error", err)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: prepare the provisioning state directory %q: %w; left in place", stateRel, err)
	}
	return stateDir, nil
}

// nfsWorktreeSubPath is the export-relative path of an agent's worktree:
// <workspace>/worktrees/<agent name>, the same layout provision.WorktreePath
// uses under the shared checkout.
func nfsWorktreeSubPath(workspaceRel, name string) string {
	return filepath.Join(workspaceRel, "worktrees", name)
}

// nfsWorktreeLockWait bounds how long RemoveNFSWorktree waits for the
// project's provisioning lock, so a delete is never held up for long. The
// steps under the lock are quick renames and git worktree prune, and run to
// completion. A variable so tests can shorten it.
var nfsWorktreeLockWait = 30 * time.Second

// nfsWorktreeBackend selects the workspace backend RemoveNFSWorktree
// resolves paths with. A variable so tests can stand in for a backend that
// resolves a path outside the export.
var nfsWorktreeBackend = runtime.SelectWorkspaceBackend

// purgeRemovedWorktrees deletes the worktree directories that
// RemoveNFSWorktree moved aside under base/worktrees. Deleting a large
// worktree over NFS can take a while, so it runs in the background, without
// a deadline. A variable so tests can run it inline.
var purgeRemovedWorktrees = func(base string) {
	go func() {
		if err := provision.PurgeRemovedWorktrees(base); err != nil {
			slog.Warn("workspace_storage nfs: could not delete a removed worktree's files", "path", base, "error", err)
		}
	}()
}

// nfsExportWorkspace resolves, for the delete path, the project's
// workspace on the NFS workspace export as seen through the broker's own
// mount: the export mount with symlinks resolved, and the workspace path
// relative to it (<subpath_root>/<projectID>/workspace). It returns two
// empty strings and no error when there is nothing to do from this broker:
// the runtime is not Kubernetes, the project's workspace_storage backend is
// not nfs, or the export is not mounted here. agentName must be an agent
// slug and projectID a valid project ID; the workspace path is checked the
// same way as when it is created before a pod starts.
func (m *AgentManager) nfsExportWorkspace(projectPath, projectID, agentName string) (resolvedHostBase, rel string, err error) {
	if m.Runtime == nil || !isKubernetesRuntime(m.Runtime.Name()) {
		return "", "", nil
	}
	if !isNFSWorktreeName(agentName) {
		return "", "", fmt.Errorf("workspace_storage nfs: invalid agent name %q", agentName)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return "", "", fmt.Errorf("workspace_storage nfs: invalid project ID %q", projectID)
	}
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", "", fmt.Errorf("workspace_storage nfs: resolve project directory: %w", err)
	}
	settings, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		return "", "", fmt.Errorf("workspace_storage nfs: load settings: %w", err)
	}
	if settings == nil || settings.Server == nil || settings.Server.WorkspaceStorage == nil {
		return "", "", nil
	}
	backend := nfsWorktreeBackend(settings.Server.WorkspaceStorage, store.SharingModeWorktreePerAgent)
	if backend.Name() != "nfs" {
		return "", "", nil
	}
	resolved, err := backend.Resolve(runtime.ResolveInput{
		ProjectID:   projectID,
		AgentID:     agentName,
		ProjectSlug: api.Slugify(config.GetProjectName(projectDir)),
		Mode:        store.SharingModeWorktreePerAgent,
		ProjectDir:  projectDir,
	})
	if err != nil {
		return "", "", fmt.Errorf("workspace_storage nfs: resolve workspace: %w", err)
	}
	rel = resolved.ServerRelativePath
	if rel == "" || !filepath.IsLocal(rel) || filepath.Join(resolved.HostBase, rel) != resolved.HostPath {
		return "", "", fmt.Errorf("workspace_storage nfs: unexpected workspace path %q under %q", rel, resolved.HostBase)
	}
	info, err := os.Stat(resolved.HostBase)
	if errors.Is(err, fs.ErrNotExist) {
		// Export not mounted on this broker: nothing to remove from here.
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("workspace_storage nfs: check export mount %q: %w", resolved.HostBase, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("workspace_storage nfs: check export mount %q: not a directory", resolved.HostBase)
	}
	resolvedHostBase, err = filepath.EvalSymlinks(resolved.HostBase)
	if err != nil {
		return "", "", fmt.Errorf("workspace_storage nfs: resolve export mount %q: %w", resolved.HostBase, err)
	}
	return resolvedHostBase, rel, nil
}

// RemoveNFSWorktree removes the worktree of the agent named agentName from
// the project's shared checkout on the NFS workspace export
// (<subpath_root>/<projectID>/workspace/worktrees/<agentName>), through the
// broker's own mount of the export. The broker calls it when it deletes an
// agent with its files, so an agent created again with the same name (and
// so the same branch) gets a fresh worktree.
//
// It does nothing, and returns nil, unless the runtime is Kubernetes, the
// project's workspace_storage backend is nfs, the export is mounted on the
// broker, and the agent's worktree directory exists. agentName must be an
// agent slug and projectID a valid project ID; the workspace path is
// checked the same way as when it is created before a pod starts, and must
// contain no symlinks below the export. The removal itself
// (provision.RemoveMountedWorktree) runs under the project's provisioning
// lock and only ever touches worktrees/<agentName>: the worktree is moved
// aside and dropped from git, and its files are then deleted in the
// background. The agent's branch is always kept. Any failure is returned
// for the caller to log; the worktree is then left in place, and an agent
// created again with the same name reuses it.
func (m *AgentManager) RemoveNFSWorktree(ctx context.Context, projectPath, projectID, agentName string) (path string, err error) {
	resolvedHostBase, rel, err := m.nfsExportWorkspace(projectPath, projectID, agentName)
	if err != nil || resolvedHostBase == "" {
		return "", err
	}
	workspace := filepath.Join(resolvedHostBase, rel)
	worktree := filepath.Join(resolvedHostBase, nfsWorktreeSubPath(rel, agentName))
	if _, err := os.Lstat(worktree); errors.Is(err, fs.ErrNotExist) {
		return worktree, nil
	}
	// Every component from the export down to worktrees/ must be a real
	// directory, not a symlink.
	if real, err := filepath.EvalSymlinks(filepath.Dir(worktree)); err != nil || real != filepath.Dir(worktree) {
		return worktree, fmt.Errorf("workspace_storage nfs: %s is not a plain directory on the export; left in place", filepath.Dir(worktree))
	}

	stateDir, err := nfsRemovalStateDir(resolvedHostBase, rel, projectID)
	if err != nil {
		return worktree, err
	}
	if err := provision.RemoveMountedWorktree(ctx, workspace, stateDir, agentName, nfsWorktreeLockWait); err != nil {
		return worktree, err
	}
	purgeRemovedWorktrees(workspace)
	slog.Info("workspace_storage nfs: removed the agent's worktree", "path", worktree)
	return worktree, nil
}
