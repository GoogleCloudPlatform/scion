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
// (Layout.HostBase) without its value, for every shared backend (nfs and the
// volume backends). Refusal bodies pass it to the texts below in place of
// the path, which goes to the broker log only.
const ProjectShareMountPathField = "its configured mount point"

// ProjectShareUnavailableText is the refusal for a start in a shared-storage
// project on a broker that does not see the project storage share at
// mountPath.
//
// platformMounts is true when the broker's runtime mounts the share into
// each agent itself (Kubernetes, Cloud Run). Such a broker never mounts the
// share, so nfs.auto_mount does not apply: the share has to be made
// available in the broker's own container. Any other broker can mount the
// share itself, or be told to with nfs.auto_mount.
func ProjectShareUnavailableText(mountPath string, platformMounts bool) string {
	if platformMounts {
		return fmt.Sprintf("project storage share is not visible on this broker at %s: make the share (the same storage the hub uses) available in this broker's container at that path", mountPath)
	}
	return fmt.Sprintf("project storage share is not mounted on this broker at %s: mount it there, or set server.project_storage.nfs.auto_mount: true", mountPath)
}

// ProjectShareInlineUnavailableText is ProjectShareUnavailableText for a
// Kubernetes broker whose NFS share has no pv_name: pods then mount each
// directory inline by its server path, which the node does not create, so
// the broker has to see the share, writable, to create the directories
// before the pod starts.
func ProjectShareInlineUnavailableText(mountPath string) string {
	return fmt.Sprintf("project storage share is not visible on this broker at %s: make the share (the same storage the hub uses) available in this broker's container at that path, writable by the broker; without nfs.shares[0].pv_name, pods mount each directory by its server path, so the broker creates the directories before the pod starts", mountPath)
}

// ProjectShareMountFailedText is the refusal for a start on a broker that
// mounts the share itself (nfs.auto_mount is on) and could not mount it at
// mountPath.
func ProjectShareMountFailedText(mountPath string) string {
	return fmt.Sprintf("project storage share could not be mounted on this broker at %s: check that the NFS server and export (server.project_storage.nfs.shares[0]) are reachable from the broker; the broker log has the details", mountPath)
}

// ProjectShareMountFlagsText is the refusal for a create, start or restart
// on a local-container runtime whose project storage share at mountPath
// could not be confirmed to carry nosuid and nodev: a mount there or below
// it lacks them, or the mount table could not be read.
func ProjectShareMountFlagsText(mountPath string) string {
	return fmt.Sprintf("project storage share at %s: nosuid,nodev could not be confirmed (a mount there or below it lacks them, or the mount table could not be read): remount the share with nosuid,nodev; the broker log has the details", mountPath)
}

// ProjectShareClientTexts are the project storage refusal texts a runtime
// broker writes into a response body (each with ProjectShareMountPathField,
// never a path). The hub relays a broker's project_storage_unavailable
// message only when it is one of these.
func ProjectShareClientTexts() []string {
	return []string{
		ProjectShareUnavailableText(ProjectShareMountPathField, true),
		ProjectShareUnavailableText(ProjectShareMountPathField, false),
		ProjectShareInlineUnavailableText(ProjectShareMountPathField),
		ProjectShareMountFailedText(ProjectShareMountPathField),
		ProjectShareMountFlagsText(ProjectShareMountPathField),
	}
}
