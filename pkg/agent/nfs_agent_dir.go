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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// nfsAgentDirSelection decides whether an agent on the NFS workspace backend
// gets its own directory (agents/<agent name> next to the project's
// workspace path), with its own workspace the agent container clones into,
// instead of mounting the project's shared checkout.
//
// It returns the directory name and the agent's branch when the dispatch
// carries the canonical workspace mode clone-per-agent
// (SCION_WORKSPACE_MODE) and the project is git-backed (gitClone has a
// URL), and two empty strings otherwise. In that mode the agent name must
// be an agent slug: any other name is an error, so the agent never falls
// back to the shared checkout.
//
// The branch is SCION_AGENT_BRANCH when set, otherwise scion/<agent name>,
// the same choice the agent container's clone step makes.
func nfsAgentDirSelection(env map[string]string, gitClone *api.GitCloneConfig, agentName string) (name, branch string, err error) {
	if env == nil || gitClone == nil || gitClone.URL == "" {
		return "", "", nil
	}
	if store.ResolveWorkspaceSharingMode(env["SCION_WORKSPACE_MODE"]) != store.SharingModeClonePerAgent {
		return "", "", nil
	}
	if !isNFSWorktreeName(agentName) {
		return "", "", fmt.Errorf("workspace_storage nfs: clone-per-agent needs the agent name to be an agent slug (lower-case letters, digits and dashes), got %q", agentName)
	}
	branch = env["SCION_AGENT_BRANCH"]
	if branch == "" {
		branch = "scion/" + agentName
	}
	return agentName, branch, nil
}

// nfsAgentDirLockWait bounds how long RemoveNFSAgentFiles waits for the
// agent directory's lock. A variable so tests can shorten it.
var nfsAgentDirLockWait = 30 * time.Second

// purgeRemovedAgentWorkspaces deletes the workspaces RemoveNFSAgentFiles
// moved aside in an agent directory, in the background and without a
// deadline. A variable so tests can run it inline.
var purgeRemovedAgentWorkspaces = func(agentDir string) {
	go func() {
		if err := provision.PurgeRemovedAgentWorkspaces(agentDir); err != nil {
			slog.Warn("workspace_storage nfs: could not delete a removed agent workspace's files", "path", agentDir, "error", err)
		}
	}()
}

// RemoveNFSAgentFiles removes what the agent named agentName has on the NFS
// workspace export, through the broker's own mount of the export. The
// broker calls it when it deletes an agent with its files. It removes,
// independently of each other:
//   - the agent's worktree under the project's shared checkout
//     (RemoveNFSWorktree);
//   - the agent's own workspace in its agent directory
//     (<subpath_root>/<projectID>/agents/<agentName>/workspace), with the
//     branch record next to it.
//
// The project's workspace mode is not consulted: it can change after an
// agent was created, so each removal acts on what is on the export.
// Nothing outside worktrees/<agentName> and agents/<agentName> is touched.
// It returns the paths it looked at and any failures, joined; each failure
// leaves that path in place, and an agent created again with the same name
// reuses it. It does nothing unless the runtime is Kubernetes, the
// project's workspace_storage backend is nfs and the export is mounted on
// the broker.
func (m *AgentManager) RemoveNFSAgentFiles(ctx context.Context, projectPath, projectID, agentName string) (paths []string, err error) {
	var errs []error
	if path, err := m.RemoveNFSWorktree(ctx, projectPath, projectID, agentName); path != "" || err != nil {
		if path != "" {
			paths = append(paths, path)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if path, err := m.removeNFSAgentWorkspace(ctx, projectPath, projectID, agentName); path != "" || err != nil {
		if path != "" {
			paths = append(paths, path)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return paths, errors.Join(errs...)
}

// removeNFSAgentWorkspace removes the workspace of the clone-per-agent agent
// named agentName from its agent directory on the export: under the agent
// directory's lock, the workspace is moved aside and the branch record
// dropped (provision.RemoveAgentWorkspace), then the files are deleted in
// the background. It returns the agent directory. A missing agent
// directory is not an error. Every component from the export down to the
// agent directory must be a real directory, not a symlink.
func (m *AgentManager) removeNFSAgentWorkspace(ctx context.Context, projectPath, projectID, agentName string) (path string, err error) {
	resolvedHostBase, rel, err := m.nfsExportWorkspace(projectPath, projectID, agentName)
	if err != nil || resolvedHostBase == "" {
		return "", err
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, agentName)
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: %w", err)
	}
	agentDir := filepath.Join(resolvedHostBase, agentRel)
	if _, err := os.Lstat(agentDir); errors.Is(err, fs.ErrNotExist) {
		return agentDir, nil
	}
	if real, err := filepath.EvalSymlinks(agentDir); err != nil || real != agentDir {
		return agentDir, fmt.Errorf("workspace_storage nfs: %s is not a plain directory on the export; left in place", agentDir)
	}
	if err := provision.RemoveAgentWorkspace(ctx, agentDir, nfsAgentDirLockWait); err != nil {
		return agentDir, err
	}
	purgeRemovedAgentWorkspaces(agentDir)
	slog.Info("workspace_storage nfs: removed the agent's workspace", "path", filepath.Join(agentDir, provision.AgentWorkspaceDir))
	return agentDir, nil
}
