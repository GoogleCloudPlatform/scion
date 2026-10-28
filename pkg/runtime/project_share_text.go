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

import "fmt"

// ProjectShareMountPathField names the project storage share's mount point
// (Layout.HostBase) by its configuration fields. Refusal bodies pass it to
// ProjectShareUnavailableText in place of the path, which goes to the
// broker log only.
const ProjectShareMountPathField = "server.project_storage.nfs.mount_root/shares[0].id"

// ProjectShareUnavailableText is the refusal for a start in a shared-storage
// project on a broker that does not see the project storage share at
// mountPath.
//
// platformMounts is true when the broker's runtime mounts the export into
// each agent itself (Kubernetes, Cloud Run). Such a broker never mounts the
// share, so nfs.auto_mount does not apply: the share has to be made
// available in the broker's own container. Any other broker can mount the
// share itself, or be told to with nfs.auto_mount.
func ProjectShareUnavailableText(mountPath string, platformMounts bool) string {
	if platformMounts {
		return fmt.Sprintf("project storage share is not visible on this broker (%s): make the share available in this broker's container at %s (the same server and export as server.project_storage.nfs.shares[0])", mountPath, mountPath)
	}
	return fmt.Sprintf("project storage share is not mounted on this broker (%s): mount it, or set server.project_storage.nfs.auto_mount: true", mountPath)
}
