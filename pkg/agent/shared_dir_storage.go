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
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

	// Compute the Docker-shaped volumes — and, inside NFSSharedDirsToVolumeMounts,
	// run the export-root isolation guard (ValidateNotExportRoot) — BEFORE
	// touching the filesystem below. This function does no I/O, so if any
	// shared dir's resolved path would escape the host base (e.g. a
	// traversal in a malformed project ID), the whole call fails here and no
	// directory is ever created (round 1 review finding C3/T4).
	volumes, err := runtime.NFSSharedDirsToVolumeMounts(res, dirs, containerWorkspace)
	if err != nil {
		return nil, nil, fmt.Errorf("server.shared_dir_storage: %w", err)
	}

	// Local-container runtimes (Docker/Podman/Apple) bind-mount the resolved
	// host path directly, so it must exist. Never create the host base
	// itself (design §3.2.3) — only mkdir the per-shared-dir subtree, and
	// only when the host base is present (it won't be inside a future
	// in-cluster GKE broker, design §3.3 T1 compatibility).
	if _, statErr := os.Stat(res.HostBase); statErr == nil {
		for _, name := range names {
			sd := res.SharedDirs[name]
			_, existsErr := os.Stat(sd.HostPath)
			alreadyExisted := existsErr == nil
			if err := os.MkdirAll(sd.HostPath, 0o775); err != nil {
				return nil, nil, fmt.Errorf("server.shared_dir_storage: mkdir shared dir %q: %w", name, err)
			}
			if !alreadyExisted {
				// mkdir(2) only ever applies setgid via kernel inheritance
				// from an already-setgid parent, so chmod the directory we
				// just created explicitly (design §3.5(5): 0o2775). Only the
				// leaf this call created — never a pre-existing shared dir.
				if err := os.Chmod(sd.HostPath, os.ModeSetgid|0o775); err != nil {
					return nil, nil, fmt.Errorf("server.shared_dir_storage: chmod shared dir %q: %w", name, err)
				}
			}
		}
	} else if isLocalContainerRuntime(runtimeName) {
		return nil, nil, fmt.Errorf(
			"server.shared_dir_storage host base %q does not exist; provision the NFS export before starting agents", res.HostBase)
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
