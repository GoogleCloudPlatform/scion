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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"golang.org/x/sys/unix"
)

// nfsWorkspaceExportHint is appended to every pre-create failure so the
// error names what the export has to allow, not just the failing syscall.
const nfsWorkspaceExportHint = "workspace_storage nfs needs the broker to be able to create directories " +
	"under the project's directory on the export (its writes must not be mapped to an anonymous user " +
	"that cannot write there); see the NFS export requirements in the Kubernetes HA guide"

// nfsLeafGroupAccessBits are the mode bits a directory must carry for agents
// to reach it through its group: setgid (new entries inherit the group) and
// group write. A directory the broker creates always has them (2775); an
// existing one only counts as prepared when it has them too.
const nfsLeafGroupAccessBits = unix.S_ISGID | 0o020

// ensureNFSWorkspaceLeaf makes sure the project's NFS workspace directory
// (<subpath_root>/<projectID>/workspace), its provisioning state directory
// (<subpath_root>/<projectID>/provision, see runtime.NFSProvisionStateSubPath),
// and the directory of every shared
// dir served from the same claim (<subpath_root>/<projectID>/shared-dirs/<name>),
// exist before a Kubernetes pod that mounts them by subPath is created.
//
// Without this, the kubelet creates each missing subPath itself. That only
// works when the export lets the node create directories as root
// (no_root_squash); on exports that map root to an anonymous user, the
// kubelet's mkdir is denied and the pod never starts
// (CreateContainerConfigError, "failed to create subPath directory").
//
// The directories are created through shareddirs.EnsureLeaf, the same helper
// the broker and hub use for shared-dir leaves, so they get the same modes
// (2755 intermediates, 2775 plus a default ACL on a newly created leaf) and
// the same no-follow component walk. A directory that already exists is
// left exactly as it is, contents included.
//
// sharedDirNames lists the shared dirs mounted from the workspace claim; the
// caller passes none when shared dirs use their own storage. The names must
// already have passed api.ValidateSharedDirs; they are checked again here,
// and each one's resolved path (resolved.SharedDirs) must match the leaf
// computed here, the same layout the Kubernetes runtime mounts.
//
// It is a no-op unless the runtime is Kubernetes and the workspace is
// mounted from an NFS PV claim. When the broker has no local mount of the
// export (the host base does not exist), the previous behavior is kept and
// the kubelet creates the directories; this is logged so the export
// requirement is visible. The same fallback applies to a directory the
// broker is not allowed to create (EACCES, EPERM or EROFS): it is logged as
// a warning and left to the node. Any other failure (a symlink, a regular
// file in the path, an unexpected path) returns an error so the create
// fails immediately instead of after the pod times out.
//
// prepared reports whether every one of these directories was either created
// by this call or found with setgid and group write. It is false for every
// no-op case, for any directory left to the node, and whenever an existing
// directory lacks those bits (for
// example one the kubelet created root-owned, or one made by hand), so the
// provisioning chown stays strict for it.
func ensureNFSWorkspaceLeaf(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string) (prepared bool, err error) {
	return ensureNFSWorkspaceLeaves(runtimeName, projectID, resolved, pvClaimName, sharedDirNames, "")
}

// ensureNFSAgentWorkspaceLeaf is ensureNFSWorkspaceLeaf for a
// clone-per-agent agent: instead of the project's workspace directory, it
// creates the agent's directory (<subpath_root>/<projectID>/agents/<agent
// name>), which the provisioning init container mounts, and in it the
// agent's own workspace, which the agent container mounts and clones into,
// together with the same shared-dir directories. Both get the leaf modes,
// and prepared requires both. The project's workspace directory is not
// created. An existing directory is left exactly as it is, contents
// included.
func ensureNFSAgentWorkspaceLeaf(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string, agentName string) (prepared bool, err error) {
	if !isNFSWorktreeName(agentName) {
		return false, fmt.Errorf("workspace_storage nfs: invalid agent name %q", agentName)
	}
	return ensureNFSWorkspaceLeaves(runtimeName, projectID, resolved, pvClaimName, sharedDirNames, agentName)
}

// ensureNFSWorkspaceLeaves implements ensureNFSWorkspaceLeaf and, with
// agentName set, ensureNFSAgentWorkspaceLeaf.
func ensureNFSWorkspaceLeaves(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string, agentName string) (prepared bool, err error) {
	if !isKubernetesRuntime(runtimeName) || resolved.Backend != "nfs" || pvClaimName == "" {
		return false, nil
	}

	rel := resolved.ServerRelativePath
	if !shareddirs.ValidProjectID(projectID) {
		return false, fmt.Errorf("workspace_storage nfs: invalid project ID %q", projectID)
	}
	if rel == "" || !filepath.IsLocal(rel) || filepath.Join(resolved.HostBase, rel) != resolved.HostPath {
		return false, fmt.Errorf("workspace_storage nfs: unexpected workspace path %q under %q", rel, resolved.HostBase)
	}
	workspaceLeaf := rel
	var leaves []string
	if agentName != "" {
		agentDir, err := runtime.NFSAgentDirSubPath(rel, agentName)
		if err != nil {
			return false, fmt.Errorf("workspace_storage nfs: %w", err)
		}
		workspaceLeaf = filepath.Join(agentDir, provision.AgentWorkspaceDir)
		// The provisioning init container writes its lock, the branch
		// record and the sentinel in the agent directory, so it needs the
		// same group access as the workspace: an intermediate created by
		// the walk alone would have no group write.
		leaves = append(leaves, agentDir)
	}
	leaves = append(leaves, workspaceLeaf)
	if agentName == "" {
		// Shared-plain and worktree-per-agent: the provisioning init
		// container keeps its sentinel and lock in the project's
		// provisioning state directory (<project>/provision), which it
		// mounts by subPath next to the workspace. Same derivation as the
		// runtime's mount, and the same leaf modes, so both the broker and
		// the init container can take the lock in it.
		stateLeaf, err := runtime.NFSProvisionStateSubPath(rel, projectID)
		if err != nil {
			return false, fmt.Errorf("workspace_storage nfs: %w", err)
		}
		leaves = append(leaves, stateLeaf)
	}
	if len(sharedDirNames) > 0 {
		dirs := make([]api.SharedDir, 0, len(sharedDirNames))
		for _, name := range sharedDirNames {
			dirs = append(dirs, api.SharedDir{Name: name})
		}
		if err := api.ValidateSharedDirs(dirs); err != nil {
			return false, fmt.Errorf("workspace_storage nfs: shared_dirs: %w", err)
		}
		// Same layout as the Kubernetes runtime's nfsSharedDirSubPath:
		// a sibling "shared-dirs" directory next to the workspace.
		sharedRoot := filepath.Join(filepath.Dir(rel), "shared-dirs")
		for _, name := range sharedDirNames {
			leaf := filepath.Join(sharedRoot, name)
			if got := resolved.SharedDirs[name].ServerRelativePath; got != leaf {
				return false, fmt.Errorf("workspace_storage nfs: shared dir %q resolves to %q, expected %q next to the workspace", name, got, leaf)
			}
			leaves = append(leaves, leaf)
		}
	}

	info, err := os.Stat(resolved.HostBase)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Info("workspace_storage nfs: export not mounted on this broker; the node will create the workspace directory "+
				"when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
				"host_base", resolved.HostBase, "sub_path", workspaceLeaf)
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

	prepared = true
	for _, leaf := range leaves {
		ok, err := ensureNFSLeaf(resolvedHostBase, leaf)
		if err != nil && isNFSLeafPermissionError(err) {
			// The broker may not be allowed to create the directory even
			// though the node is, for example a broker that does not run
			// as root on an export whose project directories are owned by
			// root. Keep the previous behavior for that leaf: the node
			// creates it at pod start, and the chown stays strict.
			slog.Warn("workspace_storage nfs: the broker could not create the directory, so the node will create it "+
				"when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
				"host_base", resolved.HostBase, "sub_path", leaf, "error", err)
			prepared = false
			continue
		}
		if err != nil {
			return false, fmt.Errorf("workspace_storage nfs: create directory %q on the export mounted at %q: %w; %s",
				leaf, resolved.HostBase, err, nfsWorkspaceExportHint)
		}
		if !ok {
			slog.Info("workspace_storage nfs: existing directory lacks setgid and group write; provisioning keeps a failed chown fatal",
				"host_base", resolved.HostBase, "sub_path", leaf)
			prepared = false
		}
	}
	return prepared, nil
}

// isNFSLeafPermissionError reports whether err means the broker was not
// allowed to create or open a directory (as opposed to a path that is not a
// usable directory, such as a symlink or a regular file).
func isNFSLeafPermissionError(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS)
}

// ensureNFSLeaf creates rel under hostBase via shareddirs.EnsureLeaf and
// reports whether the leaf was created now or already carries
// nfsLeafGroupAccessBits.
func ensureNFSLeaf(hostBase, rel string) (prepared bool, err error) {
	leafFd, existed, err := shareddirs.EnsureLeaf(hostBase, rel)
	if err != nil {
		return false, err
	}
	defer func() { _ = shareddirs.CloseFd(leafFd) }()
	if !existed {
		return true, nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(leafFd, &st); err != nil {
		return false, fmt.Errorf("stat existing directory: %w", err)
	}
	return st.Mode&nfsLeafGroupAccessBits == nfsLeafGroupAccessBits, nil
}
