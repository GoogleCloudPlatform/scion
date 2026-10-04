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

import (
	"fmt"
	"path"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// NFSProvisionStateMountPath is where the workspace-provision init container
// mounts the project's provisioning state directory (shared-plain and
// worktree-per-agent modes on the NFS workspace backend). The agent
// container never mounts it.
const NFSProvisionStateMountPath = "/scion-provision"

// NFSProvisionStateEnv tells `sciontool provision` where the provisioning
// state directory is mounted. An environment variable rather than a flag,
// so an older sciontool ignores it and keeps its sentinel and lock in the
// workspace.
const NFSProvisionStateEnv = "SCION_PROVISION_STATE_DIR"

// NFSProvisionStateSubPath returns the export-relative path of a project's
// provisioning state directory, <subPathRoot>/<projectID>/provision: a
// sibling of the project's workspace path workspaceSubPath
// (<subPathRoot>/<projectID>/workspace). It holds the provisioning sentinel
// and the provisioning lock, so neither is visible in the workspace.
//
// It fails closed. workspaceSubPath must be a clean, relative path (no "..",
// no "." components, no leading, trailing or doubled slashes) whose last
// element is "workspace" and that has a parent. When projectID is not
// empty, the parent's last element must be projectID. The result is derived
// only from workspaceSubPath and is checked to be a direct child of the
// same parent. Kubernetes subPaths use forward slashes whatever the OS.
func NFSProvisionStateSubPath(workspaceSubPath, projectID string) (string, error) {
	if workspaceSubPath == "" || !filepath.IsLocal(workspaceSubPath) || path.Clean(workspaceSubPath) != workspaceSubPath ||
		path.Base(workspaceSubPath) != "workspace" || path.Dir(workspaceSubPath) == "." {
		return "", fmt.Errorf("provisioning state: unexpected NFS workspace subPath %q", workspaceSubPath)
	}
	projectDir := path.Dir(workspaceSubPath)
	if projectID != "" && path.Base(projectDir) != projectID {
		return "", fmt.Errorf("provisioning state: NFS workspace subPath %q is not under project %q", workspaceSubPath, projectID)
	}
	stateSubPath := path.Join(projectDir, provision.ProvisionStateDirName)
	if path.Clean(stateSubPath) != stateSubPath || !filepath.IsLocal(stateSubPath) || path.Dir(stateSubPath) != projectDir {
		return "", fmt.Errorf("provisioning state: resolved subPath %q escapes %q", stateSubPath, projectDir)
	}
	return stateSubPath, nil
}

// nfsProvisionStateInitMount returns the workspace-provision init
// container's mount of the project's provisioning state directory and the
// environment variable that names it, for shared-plain and
// worktree-per-agent pods on the NFS workspace backend. It returns nil, nil,
// nil when the pod has no provisioning init container, and for the
// agent-directory modes (clone-per-agent and empty-per-agent), whose init
// container mounts the agent's directory and keeps its sentinel there. Any
// other NFSSubPath that is not <subPathRoot>/<projectID>/workspace is an
// error (see NFSProvisionStateSubPath), so the pod is not created.
//
// Both the provisioning container and the wait-for-sentinel container get
// it: the waiter must see the sentinel where the provisioner writes it. The
// agent container never gets it, and the project directory itself is never
// mounted (it holds every agent's directory).
func nfsProvisionStateInitMount(config RunConfig, nfsAgentDir bool) (*corev1.VolumeMount, *corev1.EnvVar, error) {
	// An empty NFSSubPath mounts the claim's root as the workspace: there is
	// no project directory to put a sibling in, so the sentinel stays in the
	// workspace as before.
	if !nfsInitContainerInjected(config) || nfsAgentDir || config.NFSSubPath == "" {
		return nil, nil, nil
	}
	subPath, err := NFSProvisionStateSubPath(config.NFSSubPath, config.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	return &corev1.VolumeMount{Name: "workspace", MountPath: NFSProvisionStateMountPath, SubPath: subPath},
		&corev1.EnvVar{Name: NFSProvisionStateEnv, Value: NFSProvisionStateMountPath},
		nil
}
