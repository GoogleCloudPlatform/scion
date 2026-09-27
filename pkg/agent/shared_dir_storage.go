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
	subPathRoot := sdCfg.NFS.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}
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
		Backend:     "nfs",
		PVClaimName: pvClaimName,
		SubPaths:    subPaths,
	}, nil
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
