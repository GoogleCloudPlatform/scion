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

package hub

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// volumeMountBase is the directory under which platform-managed workspace
// volumes are mounted into the hub container. Cloud Run mounts a declared
// volume at /mnt/<volume_name>; a Kubernetes pod spec must be written to do
// the same for the GKE shared volume PVC, since nothing in the config surface
// can express any other location. Overridden by tests.
var volumeMountBase = "/mnt"

// containerRootPath is the container root filesystem, used as the reference
// device for isMountedVolume. Overridden by tests.
var containerRootPath = "/"

// workspaceMountRoot returns the absolute path at which the configured
// workspace storage backend is mounted, or "" when the config does not name
// one. It is the single place the mount location is derived, so the readiness
// check and hub-managed project paths cannot drift apart.
//
// For the volume backends the mount root is <volumeMountBase>/<volume_name>;
// the volume name is what identifies the mount point, since neither
// V1CloudRunVolumeConfig nor V1GKESharedVolumeConfig has a mount path field
// (PVClaimName names the PVC and SubPathRoot is a prefix within the volume).
func workspaceMountRoot(wsCfg *config.V1WorkspaceStorageConfig) string {
	if wsCfg == nil {
		return ""
	}

	switch wsCfg.Backend {
	case "nfs":
		if wsCfg.NFS != nil && len(wsCfg.NFS.Shares) > 0 {
			return filepath.Join(wsCfg.NFS.MountRoot, wsCfg.NFS.Shares[0].ID)
		}
	case "cloudrun-volume":
		if wsCfg.CloudRunVolume != nil && wsCfg.CloudRunVolume.VolumeName != "" {
			return filepath.Join(volumeMountBase, wsCfg.CloudRunVolume.VolumeName)
		}
	case "gke-shared-volume":
		if wsCfg.GKESharedVolume != nil && wsCfg.GKESharedVolume.VolumeName != "" {
			return filepath.Join(volumeMountBase, wsCfg.GKESharedVolume.VolumeName)
		}
	}

	return ""
}

// isMountedVolume reports whether path lives on a filesystem other than the
// container root — that is, whether a volume is actually mounted there.
//
// Presence alone does not answer that question. The hub image declares no USER
// and runs as root, so when the volume is mounted somewhere other than the
// expected path, initHubManagedProject's MkdirAll happily creates the
// directory on the container's ephemeral overlay. From then on a plain
// os.Stat succeeds and readiness would report healthy forever while every
// project tree is written to storage that vanishes on reschedule. Comparing
// device IDs distinguishes the two: a directory created on the overlay shares
// the root filesystem's device, a mounted volume does not.
//
// The comparison is against the container root rather than the path's parent
// so that a deployment mounting the volume one level up — at volumeMountBase
// itself, with a subPath — is still recognized as mounted. Writes in that
// layout do land in the volume.
//
// The second return reports whether the answer could be established at all.
// When it is false the first return is true, because an unenforceable check
// must not fail readiness — but the caller is expected to surface the fact,
// since that state silently reinstates the bug this function exists to catch.
// It happens when a FileInfo does not carry a unix stat (a fake FileInfo or a
// filesystem implementation outside package os) or when the container root
// cannot be stat'ed. Note this file needs syscall.Stat_t at compile time and
// so builds only on unix targets, which is everything build-release.yml ships;
// on a platform without that type it would not compile rather than fail open.
func isMountedVolume(fi os.FileInfo, rootPath string) (mounted, determinable bool) {
	dev, ok := deviceID(fi)
	if !ok {
		return true, false
	}

	rootFI, err := os.Stat(rootPath)
	if err != nil {
		return true, false
	}
	rootDev, ok := deviceID(rootFI)
	if !ok {
		return true, false
	}

	return dev != rootDev, true
}

// deviceID returns the filesystem device ID for fi, and whether it could be
// determined.
func deviceID(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// warnEphemeralProjectPath reports, once per slug, that a hub-managed project
// is being served from the pod's local disk because the shared volume has no
// content for it yet.
//
// Once per slug rather than once per call: hubManagedProjectPath is on the
// WebDAV, clone and cache request paths, so a deployment sitting in this state
// would otherwise emit a line per request for as long as it runs. The
// condition is a property of the deployment, not of the request.
func (s *Server) warnEphemeralProjectPath(slug, localPath, volumePath string) {
	if _, alreadyWarned := s.warnedEphemeralProjects.LoadOrStore(slug, struct{}{}); alreadyWarned {
		return
	}
	s.projectsLogger().Warn("hub-managed project served from ephemeral local path; gke-shared-volume mount has no content yet",
		"slug", slug, "local_path", localPath, "volume_path", volumePath)
}
