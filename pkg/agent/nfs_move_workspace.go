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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Which workspace a moved agent expects to find on the NFS export
// (api/hub ExpectExistingNFSWorkspace values).
const (
	// MoveWorkspaceAgentDir: the agent's own directory,
	// <subPathRoot>/<projectID>/agents/<name>/workspace (clone-per-agent and
	// empty-per-agent).
	MoveWorkspaceAgentDir = "agent-dir"
	// MoveWorkspaceProject: the project's shared checkout,
	// <subPathRoot>/<projectID>/workspace (shared-plain and hub-managed).
	MoveWorkspaceProject = "project"
)

// ErrMoveWorkspaceMissing is returned by CheckNFSMoveWorkspace when the
// workspace a moved agent expects is not on this broker's mount of the
// export.
var ErrMoveWorkspaceMissing = errors.New("the agent's workspace is not on this broker's NFS export")

// CheckNFSMoveWorkspace confirms, through this broker's own mount of the
// project's NFS workspace export, that the workspace of an agent moved here
// from another broker exists, before the agent is provisioned. kind is
// MoveWorkspaceAgentDir or MoveWorkspaceProject. It returns the path it
// checked and ErrMoveWorkspaceMissing (wrapped) when the workspace is not
// there, the export is not mounted here, or the project's workspace storage
// is not nfs: provisioning would then create an empty workspace and the
// agent would lose its work.
func (m *AgentManager) CheckNFSMoveWorkspace(projectPath, projectID, agentName, kind string) (string, error) {
	hostBase, rel, err := nfsExportWorkspaceOnAnyRuntime(projectPath, projectID, agentName)
	if err != nil {
		return "", err
	}
	if hostBase == "" {
		return "", fmt.Errorf("%w: the project's NFS workspace export is not mounted on this broker", ErrMoveWorkspaceMissing)
	}
	var path string
	switch kind {
	case MoveWorkspaceAgentDir:
		agentRel, err := runtime.NFSAgentDirSubPath(rel, agentName)
		if err != nil {
			return "", fmt.Errorf("workspace_storage nfs: %w", err)
		}
		path = filepath.Join(hostBase, agentRel, provision.AgentWorkspaceDir)
	case MoveWorkspaceProject:
		path = filepath.Join(hostBase, rel)
	default:
		return "", fmt.Errorf("unknown expected NFS workspace kind %q", kind)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return path, fmt.Errorf("%w: %s does not exist", ErrMoveWorkspaceMissing, path)
	}
	return path, nil
}
