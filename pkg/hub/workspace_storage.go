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
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// volumeMountBase is the directory under which platform-managed workspace
// volumes are mounted into the hub container. Cloud Run mounts a declared
// volume at /mnt/<volume_name>; a Kubernetes pod spec must be written to do
// the same for the GKE shared volume PVC, since nothing in the config surface
// can express any other location. Overridden by tests.
var volumeMountBase = "/mnt"

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
