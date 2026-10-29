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
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
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
const nfsLeafGroupAccessBits = 0o2000 | 0o020 // S_ISGID | group write

// ErrProjectShareNotMounted refuses a start whose project storage share is
// not mounted on this broker.
var ErrProjectShareNotMounted = errors.New("project storage share is not mounted on this broker")

// projectShareError is a refusal for a share the broker does not see: its
// text is the operator remedy (runtime.ProjectShareUnavailableText) and it
// matches ErrProjectShareNotMounted for errors.Is.
type projectShareError struct{ msg string }

func (e *projectShareError) Error() string { return e.msg }
func (e *projectShareError) Unwrap() error { return ErrProjectShareNotMounted }

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
// export (the host base does not exist), the start is refused with
// ErrProjectShareNotMounted: the broker reads and creates project storage
// through its own mount (ptone/scion#5823). A directory the
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
	prepared, _, err = ensureNFSAgentWorkspaceLeafClaim(runtimeName, projectID, resolved, pvClaimName, sharedDirNames, agentName)
	return prepared, err
}

// ensureNFSAgentWorkspaceLeafClaim is ensureNFSAgentWorkspaceLeaf that also
// reports whether this call created the agent's directory itself (an
// atomic claim, not a check before the create): a forced move's start
// marks only a directory it created (markMoveExportAgentDir).
func ensureNFSAgentWorkspaceLeafClaim(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string, agentName string) (prepared, agentDirCreated bool, err error) {
	if !isNFSWorktreeName(agentName) {
		return false, false, fmt.Errorf("workspace_storage nfs: invalid agent name %q", agentName)
	}
	return ensureNFSWorkspaceLeavesClaim(runtimeName, projectID, resolved, pvClaimName, sharedDirNames, agentName)
}

// ensureNFSWorkspaceLeaves implements ensureNFSWorkspaceLeaf and, with
// agentName set, ensureNFSAgentWorkspaceLeaf.
func ensureNFSWorkspaceLeaves(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string, agentName string) (prepared bool, err error) {
	prepared, _, err = ensureNFSWorkspaceLeavesClaim(runtimeName, projectID, resolved, pvClaimName, sharedDirNames, agentName)
	return prepared, err
}

// ensureNFSWorkspaceLeavesClaim is ensureNFSWorkspaceLeaves that also
// reports, with agentName set, whether this call created the agent's
// directory.
func ensureNFSWorkspaceLeavesClaim(runtimeName, projectID string, resolved runtime.ResolvedWorkspace, pvClaimName string, sharedDirNames []string, agentName string) (prepared, agentDirCreated bool, err error) {
	if !isKubernetesRuntime(runtimeName) || resolved.Backend != "nfs" || pvClaimName == "" {
		return false, false, nil
	}

	if !shareddirs.ValidProjectID(projectID) {
		return false, false, fmt.Errorf("workspace_storage nfs: invalid project ID %q", projectID)
	}
	rel, err := nfsWorkspaceRel(resolved)
	if err != nil {
		return false, false, err
	}
	workspaceLeaf := rel
	var leaves []string
	agentDirLeaf := ""
	if agentName != "" {
		agentDir, err := nfsOwnAgentDirRel(resolved, agentName)
		if err != nil {
			return false, false, err
		}
		workspaceLeaf = filepath.Join(agentDir, provision.AgentWorkspaceDir)
		// The provisioning init container writes its lock, the branch
		// record and the sentinel in the agent directory, so it needs the
		// same group access as the workspace: an intermediate created by
		// the walk alone would have no group write.
		leaves = append(leaves, agentDir)
		agentDirLeaf = agentDir
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
			return false, false, fmt.Errorf("workspace_storage nfs: %w", err)
		}
		leaves = append(leaves, stateLeaf)
	}
	if len(sharedDirNames) > 0 {
		dirs := make([]api.SharedDir, 0, len(sharedDirNames))
		for _, name := range sharedDirNames {
			dirs = append(dirs, api.SharedDir{Name: name})
		}
		if err := api.ValidateSharedDirs(dirs); err != nil {
			return false, false, fmt.Errorf("workspace_storage nfs: shared_dirs: %w", err)
		}
		// Same layout as projectfs.Layout.Resolve for a shared dir:
		// a sibling "shared-dirs" directory next to the workspace.
		sharedRoot := filepath.Join(filepath.Dir(rel), "shared-dirs")
		for _, name := range sharedDirNames {
			leaf := filepath.Join(sharedRoot, name)
			if got := resolved.SharedDirs[name].ServerRelativePath; got != leaf {
				return false, false, fmt.Errorf("workspace_storage nfs: shared dir %q resolves to %q, expected %q next to the workspace", name, got, leaf)
			}
			leaves = append(leaves, leaf)
		}
	}

	// The share-visibility predicate shared with the broker's own checks
	// (projectfs.ShareDirVisible, bounded): the share's mount point must be
	// a directory on this broker. No mount-table source match is required
	// here: this runtime never mounts the share on the broker.
	if err := projectfs.ShareDirVisible(context.Background(), resolved.HostBase); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A broker reads and creates project storage through its own
			// mount of the share; without it the start is refused rather
			// than left to the node (ptone/scion#5823).
			// This path runs only for the Kubernetes runtime, which never
			// mounts the share on the broker: the remedy is the broker's
			// own container, not nfs.auto_mount. The text names the path:
			// a start failure reaches the client only as fixed text
			// (runtimeOpError), and the broker logs this error.
			return false, false, &projectShareError{msg: runtime.ProjectShareUnavailableText(resolved.HostBase, true)}
		}
		return false, false, fmt.Errorf("workspace_storage nfs: check export mount %q: %w; %s", resolved.HostBase, err, nfsWorkspaceExportHint)
	}

	resolvedHostBase, err := filepath.EvalSymlinks(resolved.HostBase)
	if err != nil {
		return false, false, fmt.Errorf("workspace_storage nfs: resolve export mount %q: %w", resolved.HostBase, err)
	}

	prepared = true
	for _, leaf := range leaves {
		ok, created, err := ensureNFSLeafClaim(resolvedHostBase, leaf)
		if created && agentName != "" && leaf == agentDirLeaf {
			agentDirCreated = true
		}
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
			// agentDirCreated is still reported: the caller marks a
			// directory this call created before it fails.
			return false, agentDirCreated, fmt.Errorf("workspace_storage nfs: create directory %q on the export mounted at %q: %w; %s",
				leaf, resolved.HostBase, err, nfsWorkspaceExportHint)
		}
		if !ok {
			slog.Info("workspace_storage nfs: existing directory lacks setgid and group write; provisioning keeps a failed chown fatal",
				"host_base", resolved.HostBase, "sub_path", leaf)
			prepared = false
		}
	}
	return prepared, agentDirCreated, nil
}
