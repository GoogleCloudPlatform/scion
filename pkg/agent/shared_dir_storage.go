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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// sharedDirsNotSharedMessage is the dispatch warning of a start whose
// runtime cannot mount the shared tree while the project's shared dirs are
// on a shared backend: the agent gets node-local shared dirs (a runtime
// capability gap, tracked in ptone/scion#5926), so it does not see what the
// hub or other agents put there.
const sharedDirsNotSharedMessage = "Warning: shared directories are not shared on this runtime; files uploaded through the hub or written by other agents are not visible here"

// sharedDirsNotSharedWarning returns sharedDirsNotSharedMessage for a start
// with shared dirs on a shared backend whose runtime type cannot mount the
// shared tree (it is neither a local-container nor a Kubernetes runtime),
// and "" otherwise. The legacy split is refused by resolveSharedDirs
// instead.
func sharedDirsNotSharedWarning(sdLayout projectfs.Layout, split bool, runtimeName string, dirs []api.SharedDir) string {
	if len(dirs) == 0 || split || !sdLayout.Backend.Shared() {
		return ""
	}
	if isLocalContainerRuntime(runtimeName) || isKubernetesRuntime(runtimeName) {
		return ""
	}
	return sharedDirsNotSharedMessage
}

// resolveSharedDirs is the single resolution point for a project's shared
// directories, for every runtime (design fs-cleanup §P2, one backend for workspaces and shared dirs): sdLayout is
// the broker's shared-dir Layout (config.SharedDirStorageLayout over the
// global-only settings), and Docker/Podman/Apple's bind-mount sources and
// Kubernetes' subPaths both come from sdLayout.Resolve, so they name the
// same <subpath_root>/<projectID>/shared-dirs/<name> tree the hub serves.
// split reports the transitional legacy split (shared dirs on
// server.shared_dir_storage's nfs export while workspaces are not on nfs).
//
// On the local backend, behaviour is byte-identical to the pre-Layout
// local layout: shared dirs are ensured and mounted under the broker-local
// layout, and errors are logged and swallowed rather than failing agent
// start. Runtimes that neither bind-mount host paths nor consume
// RunConfig.SharedDirStorage (Cloud Run) also keep that behaviour, unless
// the shared dirs are on the split legacy export, which they cannot mount.
//
// On a shared backend, resolution fails closed (design G5): a missing or
// invalid hub project ID, an invalid shared-dir name, or a missing host
// base on a local-container runtime return an error instead of silently
// falling back to local disk. The returned SharedDirRealization is
// non-nil exactly on a shared backend; the Kubernetes runtime's buildPod
// mounts every dir from it by subPath.
//
// Keep the shared-backend chain in lockstep with
// runtime.ResolveSharedDirHostPath, which chat plugins use to find the same
// directories: both apply the same path checks, in the same order (name
// and project ID validation, Resolve with ConfineLeaf,
// ValidateNotExportRoot, host-base stat refusal, EvalSymlinks of the host
// base, EnsureLeaf, then the resolved-path backstop). One deliberate
// difference: on Kubernetes, when the shared dirs ride the nfs workspace
// export (no split), a host base this broker has not mounted, or a walk
// refused with EACCES/EPERM/EROFS, skips EnsureLeaf, the backstop and the
// leaf-group read for that start (kubeletMayCreateSharedDirs, as
// ensureNFSWorkspaceLeaf does for the workspace); the pod then mounts only
// the leaf by the subPath sharedDirClaimSubPath checks. The plugin chain
// always refuses. A change to either chain must be made to both;
// TestSharedDirChainsParity runs one table of refusals through both.
func resolveSharedDirs(
	sdLayout projectfs.Layout,
	split bool,
	projectDir string,
	projectID string,
	runtimeName string,
	dirs []api.SharedDir,
	containerWorkspace string,
) ([]api.VolumeMount, *runtime.SharedDirRealization, error) {
	if len(dirs) == 0 {
		return nil, nil, nil
	}

	mountsShare := isLocalContainerRuntime(runtimeName) || isKubernetesRuntime(runtimeName)
	if !sdLayout.Backend.Shared() || (!mountsShare && !split) {
		// The local layout. Errors here are logged and swallowed, not
		// propagated — this preserves exact pre-existing behaviour (design
		// AC1).
		if err := config.EnsureSharedDirs(projectDir, dirs); err != nil {
			util.Debugf("Start: failed to ensure shared dirs: %v", err)
		}
		volumes, err := config.SharedDirsToVolumeMounts(projectDir, dirs, containerWorkspace)
		if err != nil {
			util.Debugf("Start: failed to resolve shared dir volumes: %v", err)
			return nil, nil, nil
		}
		return volumes, nil, nil
	}
	backend := sdLayout.Backend
	if projectID == "" {
		return nil, nil, fmt.Errorf(
			"shared dirs are on the %s backend (server.project_storage) but no hub project ID is available; shared dirs cannot be resolved", backend)
	}
	if !mountsShare {
		// e.g. cloudrun/cloudrun-sandbox with the split legacy export:
		// neither bind-mounts a host path nor consumes
		// RunConfig.SharedDirStorage, so silently succeeding would produce
		// volumes nobody reads (design G5).
		return nil, nil, fmt.Errorf("shared dirs on the server.shared_dir_storage nfs export are not supported on runtime %q", runtimeName)
	}

	// Shared-dir names and the project ID are both path segments in the
	// shared layout (<HostBase>/<subpath_root>/<projectID>/shared-dirs/
	// <name>). A project's settings.SharedDirs can come from the in-repo
	// settings.yaml of a cloned repo, and a crafted project ID could climb
	// out of the project's own subtree (round 2 security review F1, HIGH).
	// Checked here, before Resolve, for every runtime: the k8s subPath
	// comes from the same Resolve call. Resolve validates both again and
	// confines each leaf to this project's shared-dirs/ (ConfineLeaf).
	if err := api.ValidateSharedDirs(dirs); err != nil {
		return nil, nil, fmt.Errorf("shared_dirs: %w", err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return nil, nil, fmt.Errorf("shared dirs: invalid hub project ID %q", projectID)
	}

	// Resolve every dir and build the bind-mount shapes, with the
	// export-root isolation guard (ValidateNotExportRoot), BEFORE touching
	// the filesystem below: if any dir's resolved path would escape, the
	// whole call fails here and no directory is ever created (round 1
	// review finding C3/T4).
	resolved := make([]projectfs.Resolved, len(dirs))
	volumes := make([]api.VolumeMount, len(dirs))
	for i, d := range dirs {
		res, err := sdLayout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.SharedDirMount(d, containerWorkspace))
		if err != nil {
			return nil, nil, fmt.Errorf("shared dir %q: %w", d.Name, err)
		}
		if err := runtime.ValidateNotExportRoot(res.HostPath, res.HostBase); err != nil {
			return nil, nil, fmt.Errorf("shared dir %q: %w", d.Name, err)
		}
		resolved[i] = res
		volumes[i] = api.VolumeMount{Source: res.HostPath, Target: res.Target, ReadOnly: d.ReadOnly}
	}
	hostBase := sdLayout.HostBase

	// Local-container runtimes (Docker/Podman/Apple) bind-mount the resolved
	// host path directly, so it must exist. Never create the host base
	// itself — only the per-shared-dir subtree, and only when the host base
	// is present (a missing base means the share is not mounted here, and
	// writing below it would land on local disk).
	var leafGIDs []observedLeafGID
	if _, statErr := os.Stat(hostBase); statErr == nil {
		// mkdir and chmod are done via shareddirs.EnsureLeaf, which walks
		// every component of the relative path with
		// openat(O_NOFOLLOW|O_DIRECTORY) and mkdirat, refusing ANY symlink
		// — leaf or intermediate, inside the share or outside it — before
		// creating or touching anything past it. EnsureLeaf is the single
		// leaf-creation entry point shared by the broker (here) and the
		// hub browser, so a shared dir either of them causes to exist
		// always gets the same modes/ACL hardening (2755 intermediates,
		// 2775 + default ACL on a leaf this call creates) and the same
		// rollback-on-finalization-failure behavior.
		resolvedHostBase, err := filepath.EvalSymlinks(hostBase)
		if err != nil {
			return nil, nil, fmt.Errorf("shared dirs: resolve host base symlinks: %w", err)
		}
		for i, d := range dirs {
			rel := resolved[i].RelPath // relative to HostBase, e.g. projects/<pid>/shared-dirs/<name>
			// A root broker gives the directories it creates the
			// shared-dir Layout's owner (its nfs uid and gid).
			leafFd, _, walkErr := ensureSharedDirLeaf(resolvedHostBase, rel, resolved[i].Owner())
			if walkErr != nil && isNFSLeafPermissionError(walkErr) && kubeletMayCreateSharedDirs(runtimeName, sdLayout, split) {
				// As for the workspace directory on the same export
				// (ensureNFSWorkspaceLeaf): the broker may not be allowed
				// to create the directory even though the node is. The
				// node creates it at pod start and the workspace
				// provisioning container prepares it; no leaf group is
				// read for it.
				slog.Warn("shared dirs: the broker could not create the directory, so the node will create it "+
					"when the pod starts, which requires an export that allows root to create directories (no_root_squash)",
					"host_base", hostBase, "sub_path", rel, "error", walkErr)
				continue
			}
			if walkErr != nil {
				return nil, nil, fmt.Errorf("shared dir %q: %w", d.Name, walkErr)
			}
			// Read the leaf's group from the fd the walk opened (never a
			// path), at every start, so it follows the leaf as it is now.
			gid, gidErr := fdGID(leafFd)
			leafGIDs = append(leafGIDs, observedLeafGID{name: d.Name, gid: gid, err: gidErr})
			_ = shareddirs.CloseFd(leafFd)

			// Defense in depth: require the fully resolved path to equal
			// the expected clean path under the resolved host base, as a
			// backstop against a component renamed away between the walk
			// completing and this check (the walk works on descriptors
			// opened at walk time; this re-resolves the path fresh).
			wantResolvedLeaf := filepath.Join(resolvedHostBase, rel)
			resolvedLeaf, err := filepath.EvalSymlinks(resolved[i].HostPath)
			if err != nil {
				return nil, nil, fmt.Errorf("shared dirs: resolve shared dir %q: %w", d.Name, err)
			}
			if resolvedLeaf != wantResolvedLeaf {
				return nil, nil, fmt.Errorf(
					"shared dirs: shared dir %q resolves through a symlink to %q, want %q",
					d.Name, resolvedLeaf, wantResolvedLeaf)
			}
			volumes[i].Source = resolvedLeaf
		}
	} else if errors.Is(statErr, fs.ErrNotExist) {
		if !kubeletMayCreateSharedDirs(runtimeName, sdLayout, split) {
			return nil, nil, fmt.Errorf(
				"shared dirs on the %s backend require this broker to have the share mounted at %q "+
					"so it can create the project chain safely (T2 topology) -- see docs/deploy/hybrid-tier.md",
				backend, hostBase)
		}
		slog.Info("shared dirs: the nfs export is not mounted on this broker; the node creates the shared-dir directories "+
			"when the pod starts and the workspace provisioning container prepares them",
			"host_base", hostBase)
	} else {
		// Some other failure checking the host base (permissions, a stale
		// mount, etc.) -- not simply "not provisioned yet".
		return nil, nil, fmt.Errorf("shared dirs: check host base %q: %w", hostBase, statErr)
	}

	subPaths := make(map[string]string, len(dirs))
	for i, d := range dirs {
		subPaths[d.Name] = resolved[i].SubPath
	}
	return volumes, &runtime.SharedDirRealization{
		Backend:            string(backend),
		PVClaimName:        sdLayout.Share.PVName,
		ProjectID:          projectID,
		SubPaths:           subPaths,
		SupplementalGroups: sharedDirLeafGroups(leafGIDs, sdLayout.GID),
	}, nil
}

// kubeletMayCreateSharedDirs reports whether a Kubernetes start may leave
// missing shared-dir directories to the node when this broker has no mount
// of the share: only when the shared dirs ride the nfs workspace export
// (no legacy split), as the workspace directory itself does
// (ensureNFSWorkspaceLeaf). The workspace provisioning init container then
// mounts the dirs and prepares their ownership (F-111). Every other case
// refuses: the kubelet would create the leaf under the export's squashed
// anonymous identity with no one to fix it, exactly the identity the leaf
// hardening (mode + ACL) defends against.
func kubeletMayCreateSharedDirs(runtimeName string, sdLayout projectfs.Layout, split bool) bool {
	return isKubernetesRuntime(runtimeName) && sdLayout.Backend == projectfs.BackendNFS && !split
}

// ensureSharedDirLeaf is shareddirs.EnsureLeafOwned, replaceable in tests
// to replace a path component after the walk and reach the backstop check.
var ensureSharedDirLeaf = shareddirs.EnsureLeafOwned

// fdGID is shareddirs.FdGID, replaceable in tests to simulate a failed stat.
var fdGID = shareddirs.FdGID

// observedLeafGID is the result of reading one shared-dir leaf's group.
type observedLeafGID struct {
	name string
	gid  uint32
	err  error
}

// minLeafGroupID is the lowest leaf group id an agent may be given as a
// supplemental group. Ids below it are system groups (root, adm, disk,
// docker, ...) on common distributions; a leaf made outside scion could
// carry one, and the agent would then get that group's access to
// everything else visible in its container.
const minLeafGroupID = 1000

// sharedDirLeafGroups returns the distinct leaf group ids, in first-seen
// order, that agents mounting these nfs shared dirs get as supplemental
// groups, so files written by other agent kinds in the leaf's group stay
// writable (ptone/scion#3155). The guard skips, with a warning naming the
// shared dir, any gid that:
//   - could not be read (the agent starts unchanged, without it),
//   - is below minLeafGroupID (system groups, including 0),
//   - is an overflow/"nobody" id (65534, 4294967294) that NFSv4 idmapping
//     reports when it cannot map the real group,
//   - differs from allowGID, when the shared-dir Layout's nfs gid is set.
func sharedDirLeafGroups(observed []observedLeafGID, allowGID int) []int64 {
	var out []int64
	seen := make(map[int64]bool)
	for _, o := range observed {
		if o.err != nil {
			slog.Warn("Start: could not read the group of a shared dir; the agent starts without that group, "+
				"so it may be unable to modify files other agents create there",
				"shared_dir", o.name, "error", o.err)
			continue
		}
		gid := int64(o.gid)
		reason := ""
		switch {
		case gid < minLeafGroupID:
			reason = "group id is below 1000 (system group)"
		case gid == 65534 || gid == 4294967294:
			reason = "group id is an overflow (nobody) id"
		case allowGID != 0 && gid != int64(allowGID):
			reason = "group id does not match the shared-dir storage nfs.gid"
		}
		if reason != "" {
			slog.Warn("Start: not adding a shared dir's group to the agent; "+
				"the agent may be unable to modify files other agents create there",
				"shared_dir", o.name, "gid", gid, "allowed_gid", allowGID, "reason", reason)
			continue
		}
		if !seen[gid] {
			seen[gid] = true
			out = append(out, gid)
		}
	}
	return out
}

// isLocalContainerRuntime reports whether name identifies a runtime that
// bind-mounts host paths directly (Docker, Podman, Apple's `container`), as
// opposed to Kubernetes, which mounts a PVC and never needs a local host
// base to exist on the broker process that builds the pod spec.
func isLocalContainerRuntime(name string) bool {
	switch name {
	case "docker", "podman", "container":
		return true
	default:
		return false
	}
}

// isKubernetesRuntime reports whether name identifies the Kubernetes
// runtime, the only other runtime that consumes shared dirs on a shared
// backend (via RunConfig.SharedDirStorage / buildPod's subPath branch).
func isKubernetesRuntime(name string) bool {
	return name == "kubernetes"
}

// writeAgentRecordFile writes v as JSON to the file name in agentDir. It is
// the single writer for the per-agent storage records (home storage). The file is written to a temporary name and renamed
// into place, so a reader never sees a partial record. The file and the
// directory are synced so the record survives a crash.
func writeAgentRecordFile(agentDir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(agentDir, name)
	tmp, err := os.CreateTemp(agentDir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed, renamed := false, false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	closed = true
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	// Sync the directory so the rename survives a crash. Not all platforms
	// support this; a failure here does not undo the write.
	if d, derr := os.Open(agentDir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// agentSharedDirs returns the agent's shared dirs: the project
// settings' shared_dirs when set, else the dirs the hub dispatched.
func agentSharedDirs(settings *config.VersionedSettings, opts api.StartOptions) []api.SharedDir {
	if settings != nil && len(settings.SharedDirs) > 0 {
		return settings.SharedDirs
	}
	return opts.SharedDirs
}

// legacySharedDirStorageRecordFile is the per-agent shared-dir backend
// record of earlier versions (shared-dir-storage.json). Nothing reads it:
// server.project_storage places every agent's shared dirs. It is only
// moved with the rest of a legacy agent directory (migrateLegacyAgentState).
const legacySharedDirStorageRecordFile = "shared-dir-storage.json"

// retiredSharedDirRecordSuffix is appended to a retired per-agent shared-dir
// backend record when it is moved aside.
const retiredSharedDirRecordSuffix = ".retired"

// retireSharedDirRecord handles an agent's per-agent shared-dir backend
// record from earlier versions, once: when it named another backend for any
// of dirs than the one sdLayout now places them on, it returns a warning for
// the start naming those dirs. The record is then moved aside, so the
// warning is shown only on the first start after the upgrade. The agent now
// follows the project's layout; node-local content is copied onto the shared
// tree by the storage migration.
func retireSharedDirRecord(agentDir string, sdLayout projectfs.Layout, dirs []api.SharedDir) string {
	if agentDir == "" {
		return ""
	}
	path := filepath.Join(agentDir, legacySharedDirStorageRecordFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec struct {
		Backend string            `json:"backend"`
		Dirs    map[string]string `json:"dirs,omitempty"`
	}
	current := "local"
	if sdLayout.Backend.Shared() {
		current = "nfs"
	}
	var changed []string
	if json.Unmarshal(data, &rec) == nil {
		for _, d := range dirs {
			recorded := rec.Backend
			if b, ok := rec.Dirs[d.Name]; ok {
				recorded = b
			}
			if recorded != "" && recorded != current {
				changed = append(changed, d.Name)
			}
		}
	}
	if err := os.Rename(path, path+retiredSharedDirRecordSuffix); err != nil {
		slog.Warn("Start: could not move the retired shared-dir storage record aside", "path", path, "error", err)
	}
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("Warning: this agent's shared dirs %s were pinned to another storage backend by an earlier version; "+
		"they now follow the project's storage layout (server.project_storage), and earlier content stays where it was until it is moved: %s",
		strings.Join(changed, ", "), config.ProjectStorageMigrationText)
}
