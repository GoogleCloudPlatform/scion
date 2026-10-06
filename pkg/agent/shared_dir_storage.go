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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// resolveSharedDirs is the single resolution point for a project's shared
// directories, used by both container runtimes (design
// deploy-config-explore §3.2.2/§3.2.3): given one settings file, Docker's
// bind-mount source and Kubernetes' PVC subPath must resolve to the same
// projects/<pid>/shared-dirs/<name> path under the same base.
//
// When sdCfg is nil or its Backend is "" / "local", behaviour is
// byte-identical to today: shared dirs are ensured and mounted under the
// broker-local layout, and errors are logged and swallowed rather than
// failing agent start (matching the pre-existing run.go:952-972 behaviour).
//
// When sdCfg.Backend is "nfs", resolution fails closed (design G5): a
// missing project ID or a missing NFS host base (on a local-container
// runtime) return an error instead of silently falling back to local disk.
// The returned SharedDirRealization is nil unless backend is "nfs"; it is
// consumed by the Kubernetes runtime's buildPod to mount the shared PVC by
// subPath instead of creating per-dir dynamic PVCs.
func resolveSharedDirs(
	sdCfg *config.V1SharedDirStorageConfig,
	projectDir string,
	projectID string,
	runtimeName string,
	dirs []api.SharedDir,
	containerWorkspace string,
	nfsWorkspaceBackend bool,
) ([]api.VolumeMount, *runtime.SharedDirRealization, error) {
	if len(dirs) == 0 {
		return nil, nil, nil
	}

	// Validate whenever a block is configured, regardless of backend, so an
	// unrecognized value (a typo, wrong case, trailing space) fails closed
	// instead of silently taking the local-layout branch below (design G5;
	// round 1 review finding C2/T3).
	if sdCfg != nil {
		if err := sdCfg.Validate(); err != nil {
			return nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
		}
	}

	if sdCfg == nil || sdCfg.Backend == "" || sdCfg.Backend == "local" {
		// F-111 review (tf-lead/tf-review-nfsfix, BLOCKING): this branch also
		// fires when server.shared_dir_storage is unset/"local" but
		// server.workspace_storage.backend IS "nfs" — the OLDER, separate NFS
		// mechanism the k8s runtime's nfsSharedDirs path (k8s_runtime.go)
		// consumes directly from RunConfig.SharedDirs. Names there become NFS
		// subPaths (nfsSharedDirSubPath) exactly the way names under
		// server.shared_dir_storage=nfs do — and dirs can come from a cloned
		// repo's in-repo settings.yaml — so the same path-escape risk that
		// round 2's security review (F1, HIGH) fixed for the newer mechanism
		// applied here too, unfixed, until now. Validate and fail closed when
		// nfsWorkspaceBackend, since an invalid name there is a real subPath
		// escape risk (F-111 additionally gave the winner init container
		// CHOWN/FOWNER/DAC_OVERRIDE, turning an escape into a cross-project
		// ownership hijack, not just a leak) — but keep the existing
		// swallow-and-log behavior for every other case (local-container
		// runtimes, or nfs disabled), to preserve design AC1 exactly as
		// before for configurations this bug never affected.
		if nfsWorkspaceBackend {
			if err := api.ValidateSharedDirs(dirs); err != nil {
				return nil, nil, fmt.Errorf("shared_dirs: %w", err)
			}
		}
		// Default/unset/"local": today's local layout. Errors here are
		// logged and swallowed, not propagated — this preserves exact
		// pre-existing behaviour (design AC1).
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

	// sdCfg.Backend == "nfs" — Validate above already rejected every other
	// value, so this is the only remaining case.
	if projectID == "" {
		return nil, nil, fmt.Errorf(
			"server.shared_dir_storage backend is \"nfs\" but no hub project ID is available; shared dirs cannot be resolved")
	}
	if !isLocalContainerRuntime(runtimeName) && !isKubernetesRuntime(runtimeName) {
		// e.g. cloudrun/cloudrun-sandbox: neither bind-mounts a host path nor
		// consumes RunConfig.SharedDirStorage, so silently succeeding would
		// produce volumes/realizations nobody reads (design G5; round 1
		// review finding C8/T11).
		return nil, nil, fmt.Errorf("server.shared_dir_storage=nfs is not supported on runtime %q", runtimeName)
	}

	// Round 2 security review finding F1 (HIGH): shared-dir names and the
	// project ID are both path segments in the NFS layout
	// (<HostBase>/<subpath_root>/<projectID>/shared-dirs/<name>). Neither
	// was validated before this fix, so a project's settings.SharedDirs
	// (which can come from the in-repo settings.yaml of a cloned repo) or a
	// crafted project ID could climb out of the project's own subtree —
	// e.g. name "../../../projects" resolves to every project's shared
	// dirs, and project ID "../victim" resolves into victim's tree. This
	// must be checked here, before Resolve, for every runtime (not only
	// local-container ones): the k8s subPath comes from the same Resolve
	// call and inherits the same paths.
	if err := api.ValidateSharedDirs(dirs); err != nil {
		return nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return nil, nil, fmt.Errorf("server.shared_dir_storage: invalid hub project ID %q", projectID)
	}

	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		names = append(names, d.Name)
	}

	res, err := runtime.NewNFSBackend(sdCfg.NFS).Resolve(runtime.ResolveInput{
		ProjectID:      projectID,
		SharedDirNames: names,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("server.shared_dir_storage: resolve: %w", err)
	}

	// Defense in depth alongside the name/ID validation above, extracted
	// into confineSharedDir so it can be tested independently of Resolve
	// and the name/ID checks (round 3 review finding N1/T1 part 1): every
	// resolved shared dir must sit directly under
	// <HostBase>/<subpath_root>/<projectID>/shared-dirs — never anywhere
	// else in the export, even if a name/ID somehow validated but Resolve
	// computed something unexpected (round 2 review finding F1). This also
	// protects the K8s subPath, since it comes from the same
	// ServerRelativePath.
	subPathRoot := config.SubPathRootOrDefault(sdCfg.NFS.SubPathRoot)
	for _, name := range names {
		sd, ok := res.SharedDirs[name]
		if !ok {
			return nil, nil, fmt.Errorf("server.shared_dir_storage: shared dir %q not found in NFS resolution", name)
		}
		if err := shareddirs.ConfineLeaf(sd.HostPath, res.HostBase, subPathRoot, projectID, name); err != nil {
			return nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
		}
	}

	// Compute the Docker-shaped volumes — and, inside NFSSharedDirsToVolumeMounts,
	// run the export-root isolation guard (ValidateNotExportRoot) — BEFORE
	// touching the filesystem below. This function does no I/O, so if any
	// shared dir's resolved path would escape the host base, the whole call
	// fails here and no directory is ever created (round 1 review finding
	// C3/T4). The confinement check above already covers escapes that stay
	// inside the host base but outside the project's own subtree; this
	// guard covers the base itself (round 2 review finding S-F5: the guard
	// only ever bounded the host base, not the per-project subtree — that
	// gap is closed by the confinement check above, not by this call).
	// Sources are patched to the resolved real path below (disposition 7');
	// this call only produces the Target/ReadOnly shape and runs the guard.
	volumes, err := runtime.NFSSharedDirsToVolumeMounts(res, dirs, containerWorkspace)
	if err != nil {
		return nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
	}

	// Local-container runtimes (Docker/Podman/Apple) bind-mount the resolved
	// host path directly, so it must exist. Never create the host base
	// itself (design §3.2.3) — only mkdir the per-shared-dir subtree, and
	// only when the host base is present.
	//
	// This tier ships only the co-located-broker topology (T2, design
	// §3.3) — a broker with local filesystem access to the NFS export. A
	// missing host base must refuse, regardless of which of the two
	// supported runtimes triggered it: kubelet auto-vivifies a missing
	// subPath itself, under the export's SQUASHED anonymous identity on an
	// all_squash export — exactly the identity the leaf's upper-directory
	// hardening (mode + ACL) defends against.
	var leafGIDs []observedLeafGID
	if _, statErr := os.Stat(res.HostBase); statErr == nil {
		// mkdir and chmod are done via shareddirs.EnsureLeaf, which walks
		// every component of `rel` with openat(O_NOFOLLOW|O_DIRECTORY) and
		// mkdirat, refusing ANY symlink — leaf or intermediate, inside the
		// export or outside it — before creating or touching anything past
		// it. The O_NOFOLLOW walk refuses a symlink atomically as part of
		// the open call, before any subsequent component — including the
		// leaf — is ever created, so nothing is created or chmod'd
		// anywhere outside the exact directory chain this call is
		// resolving, regardless of any concurrent symlink swap. The walk
		// itself lives in pkg/shareddirs so pkg/hub's file browser
		// (§3.2.5) can reuse it without importing pkg/agent.
		//
		// EnsureLeaf is the single leaf-creation entry point shared by the
		// broker (here) and the hub browser, so a shared dir either of
		// them causes to exist always gets the same modes/ACL hardening
		// (2755 intermediates, 2775 + default ACL on a leaf this call
		// creates) and the same rollback-on-finalization-failure behavior.
		resolvedHostBase, err := filepath.EvalSymlinks(res.HostBase)
		if err != nil {
			return nil, nil, fmt.Errorf("server.shared_dir_storage: resolve host base symlinks: %w", err)
		}

		for i, name := range names {
			sd := res.SharedDirs[name]
			rel := sd.ServerRelativePath // relative to HostBase, e.g. projects/<pid>/shared-dirs/<name>

			leafFd, _, walkErr := shareddirs.EnsureLeaf(resolvedHostBase, rel)
			if walkErr != nil {
				return nil, nil, fmt.Errorf("server.shared_dir_storage: shared dir %q: %w", name, walkErr)
			}
			// Read the leaf's group from the fd the walk opened (never a
			// path), at every start, so it follows the leaf as it is now.
			gid, gidErr := fdGID(leafFd)
			leafGIDs = append(leafGIDs, observedLeafGID{name: name, gid: gid, err: gidErr})
			_ = shareddirs.CloseFd(leafFd)

			// Defense in depth, even though the component walk above should
			// make it structurally impossible for a symlink to be
			// involved: require the fully resolved path to equal the
			// expected clean path under the resolved host base, as a
			// backstop against a component renamed away between the walk
			// completing and this check. This is not redundant with the
			// walk, since the walk operates on file descriptors opened at
			// walk time, while this re-resolves the path fresh.
			wantResolvedLeaf := filepath.Join(resolvedHostBase, rel)
			resolvedLeaf, err := filepath.EvalSymlinks(sd.HostPath)
			if err != nil {
				return nil, nil, fmt.Errorf("server.shared_dir_storage: resolve shared dir %q: %w", name, err)
			}
			if resolvedLeaf != wantResolvedLeaf {
				return nil, nil, fmt.Errorf(
					"server.shared_dir_storage: shared dir %q resolves through a symlink to %q, want %q",
					name, resolvedLeaf, wantResolvedLeaf)
			}
			volumes[i].Source = resolvedLeaf
		}
	} else if errors.Is(statErr, fs.ErrNotExist) {
		// Reachable only for local-container or kubernetes runtimes (the
		// unsupported-runtime check above already returned for anything
		// else). Naming the actual resolved path makes this actionable
		// regardless of which runtime hit it.
		return nil, nil, fmt.Errorf(
			"server.shared_dir_storage=nfs requires this broker to have the export mounted at %q "+
				"so it can create the project chain safely (T2 topology) -- see docs/deploy/hybrid-tier.md",
			res.HostBase)
	} else {
		// Some other failure checking the host base (permissions, a stale
		// mount, etc.) -- not simply "not provisioned yet". Include the
		// underlying error rather than the fixed not-mounted wording, which
		// would misdescribe the problem.
		return nil, nil, fmt.Errorf("server.shared_dir_storage: check host base %q: %w", res.HostBase, statErr)
	}

	subPaths := make(map[string]string, len(names))
	for _, name := range names {
		subPaths[name] = res.SharedDirs[name].ServerRelativePath
	}
	pvClaimName := ""
	if len(sdCfg.NFS.Shares) > 0 {
		pvClaimName = sdCfg.NFS.Shares[0].PVName
	}

	return volumes, &runtime.SharedDirRealization{
		Backend:            "nfs",
		PVClaimName:        pvClaimName,
		SubPaths:           subPaths,
		SupplementalGroups: sharedDirLeafGroups(leafGIDs, sdCfg.NFS.GID),
	}, nil
}

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
//   - differs from allowGID, when shared_dir_storage.nfs.gid is set.
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
			reason = "group id does not match server.shared_dir_storage.nfs.gid"
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
// runtime, the only other runtime that consumes shared_dir_storage=nfs (via
// RunConfig.SharedDirStorage / buildPod's PVC-by-subPath branch).
func isKubernetesRuntime(name string) bool {
	return name == "kubernetes"
}

// sharedDirStorageBackendName returns the backend a resolved shared-dir
// storage config selects: "nfs", or "local" for nil, "" and "local".
func sharedDirStorageBackendName(cfg *config.V1SharedDirStorageConfig) string {
	if cfg != nil && cfg.Backend == "nfs" {
		return "nfs"
	}
	return "local"
}

// sharedDirStorageRecordFile is the per-agent file, in the agent directory
// next to scion-agent.json, that records the backend the agent's shared
// dirs were first set up with. It is outside the agent home, so the
// agent's container never mounts it, and reprovisioning the agent leaves
// it in place.
const sharedDirStorageRecordFile = "shared-dir-storage.json"

// sharedDirStorageRecord is the content of sharedDirStorageRecordFile.
// Backend applies to every shared dir that Dirs does not name, including a
// dir added to the project after the record was written. Dirs names the
// dirs whose backend differs from Backend. A record written before per-dir
// backends existed has no Dirs, so Backend applies to every dir, exactly as
// before.
//
// Previous names the dirs whose backend an explicit change (see
// changeSharedDirBackends) moved to nfs, with the backend they had
// before. A start checks each such dir once for an empty nfs directory
// while its previous local directory is not empty, then drops the entry.
type sharedDirStorageRecord struct {
	Backend  string            `json:"backend"`
	Dirs     map[string]string `json:"dirs,omitempty"`
	Previous map[string]string `json:"previous,omitempty"`
}

// backendFor returns the recorded backend of the shared dir name.
func (r *sharedDirStorageRecord) backendFor(name string) string {
	if b, ok := r.Dirs[name]; ok {
		return b
	}
	return r.Backend
}

// loadSharedDirStorageRecord returns the shared-dir storage record of the
// agent whose directory is agentDir, or nil when none is recorded (a first
// start, or an agent created before the backend was recorded). A record
// that exists but cannot be read or parsed, that names no backend, or
// whose dirs entries are not valid shared dir names mapped to "local" or
// "nfs" (or previous entries mapped to "local"), is an error, so a damaged
// record never silently falls back to the current settings.
func loadSharedDirStorageRecord(agentDir string) (*sharedDirStorageRecord, error) {
	if agentDir == "" {
		return nil, nil
	}
	path := filepath.Join(agentDir, sharedDirStorageRecordFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the agent's shared-dir storage record: %w", err)
	}
	var rec sharedDirStorageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing the agent's shared-dir storage record %s: %w", path, err)
	}
	if rec.Backend == "" {
		return nil, fmt.Errorf("the agent's shared-dir storage record %s names no backend", path)
	}
	for name, backend := range rec.Dirs {
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return nil, fmt.Errorf("the agent's shared-dir storage record %s names an invalid shared dir %q", path, name)
		}
		if backend != "local" && backend != "nfs" {
			return nil, fmt.Errorf("the agent's shared-dir storage record %s records an unknown backend %q for shared dir %q", path, backend, name)
		}
	}
	for name, backend := range rec.Previous {
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return nil, fmt.Errorf("the agent's shared-dir storage record %s names an invalid shared dir %q", path, name)
		}
		if backend != "local" {
			return nil, fmt.Errorf("the agent's shared-dir storage record %s records an unknown previous backend %q for shared dir %q", path, backend, name)
		}
	}
	return &rec, nil
}

// saveSharedDirStorageRecord writes rec for the agent whose directory is
// agentDir, through writeAgentRecordFile.
func saveSharedDirStorageRecord(agentDir string, rec *sharedDirStorageRecord) error {
	if agentDir == "" {
		return fmt.Errorf("no agent directory to record the shared-dir storage backend in")
	}
	return writeAgentRecordFile(agentDir, sharedDirStorageRecordFile, rec)
}

// writeAgentRecordFile writes v as JSON to the file name in agentDir. It is
// the single writer for the per-agent storage records (shared-dir storage
// and home storage). The file is written to a temporary name and renamed
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

// selectSharedDirStorage returns the shared-dir storage config for one
// agent start. gs must come from config.LoadGlobalSettingsWithOverlay, so a
// project's own settings never choose the backend.
//
// With a recorded backend that backend is kept, so a settings change never
// moves an existing agent's shared dirs. The recorded backend is checked
// first: a profile or runtime override that is now invalid only logs a
// warning for such an agent, since the agent does not use it. A recorded
// nfs backend whose server.shared_dir_storage.nfs block is no longer
// complete is an error, returned before anything touches the filesystem.
//
// Without a recorded backend (a first start, or an agent created before
// the backend was recorded) it is the per-profile resolution: the
// profile's shared_dir_storage_backend, else its runtime entry's, else
// server.shared_dir_storage.backend. An invalid override is an error that
// names the key.
func selectSharedDirStorage(gs *config.VersionedSettings, profile, recorded, agentName string) (*config.V1SharedDirStorageConfig, error) {
	if gs == nil {
		return nil, fmt.Errorf("no global settings to choose the shared-dir storage backend for agent %q from", agentName)
	}
	current, source := gs.ResolveSharedDirStorage(profile)
	var overrideErr error
	if current != nil && source != config.SharedDirStorageGlobalSource {
		// An override names its own key, so a bad value or an nfs
		// override without an nfs block points at where it is set.
		if err := current.Validate(); err != nil {
			overrideErr = fmt.Errorf("%s: %w", source, err)
		}
	}
	if recorded == "" {
		if overrideErr != nil {
			return nil, overrideErr
		}
		return current, nil
	}
	if overrideErr != nil {
		slog.Warn("Start: ignoring an invalid shared-dir storage override; the agent keeps its recorded backend",
			"agent", agentName, "recorded", recorded, "error", overrideErr)
	} else if recorded != sharedDirStorageBackendName(current) {
		slog.Warn("Start: settings now select a different shared-dir storage backend than the agent was created with; keeping the agent's backend",
			"agent", agentName, "recorded", recorded, "current", sharedDirStorageBackendName(current), "source", source)
	}
	switch recorded {
	case "local":
		return &config.V1SharedDirStorageConfig{Backend: "local"}, nil
	case "nfs":
		out := &config.V1SharedDirStorageConfig{Backend: "nfs"}
		if gs.Server != nil && gs.Server.SharedDirStorage != nil {
			out.NFS = gs.Server.SharedDirStorage.NFS
		}
		if err := out.Validate(); err != nil {
			return nil, fmt.Errorf("agent %q was created with the nfs shared-dir storage backend, but server.shared_dir_storage.nfs is no longer complete: %w", agentName, err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("agent %q records an unknown shared-dir storage backend %q", agentName, recorded)
	}
}

// selectSharedDirBackends returns the config for each shared dir in dirs
// whose backend differs from defaultCfg, the config selectSharedDirStorage
// chose for the agent. It is nil when every dir uses defaultCfg, which is
// always the case without per-dir settings or a per-dir record.
//
// With a record (rec non-nil) each dir keeps its recorded backend: its
// dirs entry, else the record's backend. A record without dirs entries
// therefore gives every dir the default, as before per-dir backends. A
// per-dir setting that differs from the record only logs a warning.
//
// Without a record (a first start) a dir whose backend comes from a
// shared_dir_storage_backends entry (see
// VersionedSettings.ResolveSharedDirStorageBackend) uses that entry. Any
// other dir uses defaultCfg. An entry naming a dir the project does not
// have is never looked up. An invalid entry, or an nfs entry without a
// complete server.shared_dir_storage.nfs block, is an error that names
// the key.
func selectSharedDirBackends(gs *config.VersionedSettings, profile string, rec *sharedDirStorageRecord, defaultCfg *config.V1SharedDirStorageConfig, dirs []api.SharedDir, agentName string) (map[string]*config.V1SharedDirStorageConfig, error) {
	if gs == nil {
		return nil, nil
	}
	defaultName := sharedDirStorageBackendName(defaultCfg)
	var out map[string]*config.V1SharedDirStorageConfig
	for _, d := range dirs {
		current, source, perDirSetting := gs.ResolveSharedDirStorageBackend(profile, d.Name)
		backend := defaultName
		if rec != nil {
			backend = rec.backendFor(d.Name)
			if perDirSetting && current != backend {
				slog.Warn("Start: settings select a different shared-dir storage backend for a shared dir than the agent recorded; keeping the agent's backend",
					"agent", agentName, "shared_dir", d.Name, "recorded", backend, "current", current, "source", source)
			}
		} else if perDirSetting {
			backend = current
		}
		if backend == defaultName {
			continue
		}
		var cfg *config.V1SharedDirStorageConfig
		switch backend {
		case "local":
			cfg = &config.V1SharedDirStorageConfig{Backend: "local"}
		case "nfs":
			cfg = &config.V1SharedDirStorageConfig{Backend: "nfs"}
			if gs.Server != nil && gs.Server.SharedDirStorage != nil {
				cfg.NFS = gs.Server.SharedDirStorage.NFS
			}
			if err := cfg.Validate(); err != nil {
				if rec != nil {
					return nil, fmt.Errorf("agent %q records the nfs shared-dir storage backend for shared dir %q, but server.shared_dir_storage.nfs is no longer complete: %w", agentName, d.Name, err)
				}
				return nil, fmt.Errorf("%s: %w", source, err)
			}
		default:
			if rec != nil {
				return nil, fmt.Errorf("agent %q records an unknown shared-dir storage backend %q for shared dir %q", agentName, backend, d.Name)
			}
			return nil, fmt.Errorf("%s: must be \"local\" or \"nfs\" (got %q)", source, backend)
		}
		if out == nil {
			out = make(map[string]*config.V1SharedDirStorageConfig)
		}
		out[d.Name] = cfg
	}
	return out, nil
}

// newSharedDirStorageRecord returns the record for an agent's first start:
// the default backend, plus a dirs entry for each dir in overrides.
func newSharedDirStorageRecord(defaultCfg *config.V1SharedDirStorageConfig, overrides map[string]*config.V1SharedDirStorageConfig) *sharedDirStorageRecord {
	rec := &sharedDirStorageRecord{Backend: sharedDirStorageBackendName(defaultCfg)}
	for name, cfg := range overrides {
		if rec.Dirs == nil {
			rec.Dirs = make(map[string]string, len(overrides))
		}
		rec.Dirs[name] = sharedDirStorageBackendName(cfg)
	}
	return rec
}

// resolveSharedDirsPerDir is resolveSharedDirs with a backend per shared
// dir. overrides, from selectSharedDirBackends, maps a dir name to its
// config when it differs from defaultCfg. Without overrides it is exactly
// resolveSharedDirs(defaultCfg, ...). Otherwise the dirs are split into a
// local set and an nfs set, resolveSharedDirs runs once per set, and the
// volumes are returned in the order of dirs. The realization's LocalDirs
// names the local dirs, so the Kubernetes runtime mounts only the nfs dirs
// from the export. byName maps each mounted dir's name to its volume; a dir
// whose local volume was dropped after a path error is not in it.
func resolveSharedDirsPerDir(
	defaultCfg *config.V1SharedDirStorageConfig,
	overrides map[string]*config.V1SharedDirStorageConfig,
	projectDir string,
	projectID string,
	runtimeName string,
	dirs []api.SharedDir,
	containerWorkspace string,
	nfsWorkspaceBackend bool,
) (volumes []api.VolumeMount, realization *runtime.SharedDirRealization, byName map[string]api.VolumeMount, err error) {
	if len(overrides) == 0 {
		volumes, realization, err = resolveSharedDirs(defaultCfg, projectDir, projectID, runtimeName, dirs, containerWorkspace, nfsWorkspaceBackend)
		if err != nil {
			return nil, nil, nil, err
		}
		// resolveSharedDirs returns one volume per dir, in order, or none
		// when the local layout dropped them after a path error.
		byName = make(map[string]api.VolumeMount, len(volumes))
		if len(volumes) == len(dirs) {
			for i, d := range dirs {
				byName[d.Name] = volumes[i]
			}
		}
		return volumes, realization, byName, nil
	}
	// The default is checked even when every dir is overridden, as
	// resolveSharedDirs checks it whenever a block is configured.
	if err := defaultCfg.Validate(); err != nil {
		return nil, nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
	}

	var localDirs, nfsDirs []api.SharedDir
	var localCfg, nfsCfg *config.V1SharedDirStorageConfig
	for _, d := range dirs {
		cfg, ok := overrides[d.Name]
		if !ok {
			cfg = defaultCfg
		}
		if sharedDirStorageBackendName(cfg) == "nfs" {
			nfsDirs = append(nfsDirs, d)
			if nfsCfg == nil {
				nfsCfg = cfg
			}
		} else {
			localDirs = append(localDirs, d)
			if localCfg == nil {
				localCfg = cfg
			}
		}
	}

	byName = make(map[string]api.VolumeMount, len(dirs))
	if len(nfsDirs) > 0 {
		vols, res, err := resolveSharedDirs(nfsCfg, projectDir, projectID, runtimeName, nfsDirs, containerWorkspace, nfsWorkspaceBackend)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(vols) != len(nfsDirs) {
			return nil, nil, nil, fmt.Errorf("server.shared_dir_storage: resolved %d volumes for %d nfs shared dirs", len(vols), len(nfsDirs))
		}
		for i, d := range nfsDirs {
			byName[d.Name] = vols[i]
		}
		realization = res
	}
	if len(localDirs) > 0 {
		vols, _, err := resolveSharedDirs(localCfg, projectDir, projectID, runtimeName, localDirs, containerWorkspace, nfsWorkspaceBackend)
		if err != nil {
			return nil, nil, nil, err
		}
		if realization != nil {
			realization.LocalDirs = make(map[string]bool, len(localDirs))
			for _, d := range localDirs {
				realization.LocalDirs[d.Name] = true
			}
		}
		// The local layout logs and drops its volumes on a path error
		// instead of failing the start; the dirs are then not mounted, as
		// before per-dir backends.
		if len(vols) == len(localDirs) {
			for i, d := range localDirs {
				byName[d.Name] = vols[i]
			}
		}
	}

	volumes = make([]api.VolumeMount, 0, len(byName))
	for _, d := range dirs {
		if v, ok := byName[d.Name]; ok {
			volumes = append(volumes, v)
		}
	}
	return volumes, realization, byName, nil
}
