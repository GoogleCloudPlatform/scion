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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// errEmptyPerAgentNFSRuntime is returned for an empty-per-agent agent on NFS
// workspace storage when the runtime is not Kubernetes: only the Kubernetes
// runtime mounts the agent's own directory on the export, and every other
// path would mount the project's workspace instead.
var errEmptyPerAgentNFSRuntime = errors.New("workspace_storage nfs: empty-per-agent workspaces on NFS workspace storage " +
	"need the Kubernetes runtime; use a broker with local workspace storage for other runtimes")

// errEmptyPerAgentNFSNoClaim is returned for an empty-per-agent agent on NFS
// workspace storage when the share has no PV claim (pv_name), so the pod
// could not mount the agent's own directory.
var errEmptyPerAgentNFSNoClaim = errors.New("workspace_storage nfs: empty-per-agent workspaces need the NFS share's pv_name " +
	"so the pod can mount the agent's own directory")

// nfsEmptyAgentDirSelection returns the name of the agent directory an
// empty-per-agent agent gets on the NFS workspace export
// (<subpath_root>/<projectID>/agents/<agent name>, with the agent's
// workspace in it), or "" when the workspace storage is not NFS and the
// agent keeps its node-local (or pod-local) private directory.
//
// On NFS storage it is an error, rather than "", when the runtime is not
// Kubernetes or the agent name is not an agent slug, so an empty-per-agent
// agent never falls back to the project's shared workspace path.
func nfsEmptyAgentDirSelection(runtimeName string, cfg *config.V1WorkspaceStorageConfig, agentName string) (string, error) {
	if cfg == nil || cfg.Backend != "nfs" {
		return "", nil
	}
	if !isKubernetesRuntime(runtimeName) {
		return "", errEmptyPerAgentNFSRuntime
	}
	if !isNFSWorktreeName(agentName) {
		return "", fmt.Errorf("workspace_storage nfs: empty-per-agent needs the agent name to be an agent slug (lower-case letters, digits and dashes), got %q", agentName)
	}
	return agentName, nil
}
