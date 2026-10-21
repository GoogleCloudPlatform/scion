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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MoveWorkspaceFreshAgentDir is the moved-agent workspace expectation of a
// forced move (api/hub ExpectExistingNFSWorkspace value): the workspace is
// recreated on the target, so the target must have nothing for the agent
// yet (CheckMoveWorkspaceAbsent).
const MoveWorkspaceFreshAgentDir = "fresh-agent-dir"

// ErrMoveWorkspacePresent is returned by CheckMoveWorkspaceAbsent when this
// broker already has a directory for the agent: a forced move never
// provisions over files it did not create.
var ErrMoveWorkspacePresent = errors.New("the agent already has files on this broker; refusing to recreate over them")

// ErrMoveWorkspaceUnconfirmed is returned by CheckMoveWorkspaceAbsent when
// it cannot tell whether this broker has files for the agent. The check
// fails closed.
var ErrMoveWorkspaceUnconfirmed = errors.New("could not confirm that the agent has no files on this broker")

// CheckMoveWorkspaceAbsent confirms, before a forced move provisions the
// agent opts names on this broker, that nothing for the agent exists here
// yet: neither its broker-side agent directory (which holds its home and,
// on local-disk runtimes, its workspace) nor, when this broker's runtime
// puts the agent's own directory on the NFS workspace export, that
// directory. Both are resolved by the functions provision and start use
// (provisionAgentStateDir, nfsOwnAgentDirSelection), never derived a second
// way. Any entry at either path, an empty directory or a symlink included,
// is ErrMoveWorkspacePresent; a path that cannot be resolved or checked is
// ErrMoveWorkspaceUnconfirmed. It returns the paths it checked, in order,
// and changes nothing.
func (m *AgentManager) CheckMoveWorkspaceAbsent(ctx context.Context, opts api.StartOptions) ([]string, error) {
	agentDir, err := provisionAgentStateDir(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve the agent directory: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	paths := []string{agentDir}
	if err := checkMovePathAbsent(agentDir); err != nil {
		return paths, err
	}
	exportDir, err := m.moveNFSOwnAgentDir(ctx, opts)
	if err != nil {
		return paths, err
	}
	if exportDir == "" {
		return paths, nil
	}
	paths = append(paths, exportDir)
	return paths, checkMovePathAbsent(exportDir)
}

// checkMovePathAbsent returns nil when nothing exists at path.
func checkMovePathAbsent(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s exists", ErrMoveWorkspacePresent, path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("%w: stat %s: %v", ErrMoveWorkspaceUnconfirmed, path, err)
	}
}

// moveNFSOwnAgentDir returns the host path, on this broker's mount of the
// NFS workspace export, of the agent directory a start of the agent opts
// names would use, or "" when the agent would get none here. It makes the
// same choice as Start (nfsOwnAgentDirSelection, the workspace backend for
// the agent's sharing mode) and resolves the same path that
// ensureNFSAgentWorkspaceLeaf creates. When the agent would get one but the
// export is not mounted on this broker, nothing can be confirmed, and it
// returns ErrMoveWorkspaceUnconfirmed.
func (m *AgentManager) moveNFSOwnAgentDir(ctx context.Context, opts api.StartOptions) (string, error) {
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return "", fmt.Errorf("%w: resolve the project directory: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	settings, _, err := config.LoadEffectiveSettingsFor(ctx, projectDir)
	if err != nil {
		return "", fmt.Errorf("%w: load settings: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	if settings == nil || settings.Server == nil || settings.Server.WorkspaceStorage == nil {
		return "", nil
	}
	ws := settings.Server.WorkspaceStorage
	runtimeName := ""
	if m.Runtime != nil {
		runtimeName = m.Runtime.Name()
	}
	emptyPerAgent := isEmptyPerAgentStart(opts)
	emptyDirName, cloneDirName, _, err := nfsOwnAgentDirSelection(runtimeName, ws, emptyPerAgent, opts.Env, opts.GitClone, opts.Name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	dirName, mode := cloneDirName, store.SharingModeSharedPlain
	if emptyPerAgent {
		dirName, mode = emptyDirName, store.SharingModeEmptyPerAgent
	}
	if dirName == "" {
		return "", nil
	}
	backend := runtime.SelectWorkspaceBackend(ws, mode)
	if backend.Name() != "nfs" {
		return "", nil
	}
	// The project ID as Start takes it: from the dispatch, else settings.
	projectID := opts.Env["SCION_PROJECT_ID"]
	if projectID == "" && settings.Hub != nil {
		projectID = settings.Hub.ProjectID
	}
	resolved, err := backend.Resolve(runtime.ResolveInput{
		ProjectID:   projectID,
		AgentID:     opts.Name,
		ProjectSlug: api.Slugify(config.GetProjectName(projectDir)),
		Mode:        mode,
		ProjectDir:  projectDir,
	})
	if err != nil {
		return "", fmt.Errorf("%w: resolve the workspace: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	dir, err := nfsOwnAgentDirPath(resolved, dirName)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMoveWorkspaceUnconfirmed, err)
	}
	if info, err := os.Stat(resolved.HostBase); err != nil || !info.IsDir() {
		return dir, fmt.Errorf("%w: the NFS workspace export is not mounted on this broker at %s", ErrMoveWorkspaceUnconfirmed, resolved.HostBase)
	}
	return dir, nil
}

// nfsOwnAgentDirPath returns the host path of the agent directory dirName
// on the broker's mount of the export resolved describes:
// <host base>/<project>/agents/<dirName> (nfsOwnAgentDirRel).
func nfsOwnAgentDirPath(resolved runtime.ResolvedWorkspace, dirName string) (string, error) {
	agentRel, err := nfsOwnAgentDirRel(resolved, dirName)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved.HostBase, agentRel), nil
}

// nfsOwnAgentDirRel returns the export-relative path of the agent directory
// dirName, <project>/agents/<dirName>, a sibling of the project's workspace
// path resolved describes. ensureNFSAgentWorkspaceLeaf creates it there and
// CheckMoveWorkspaceAbsent checks it, both through this function.
func nfsOwnAgentDirRel(resolved runtime.ResolvedWorkspace, dirName string) (string, error) {
	rel, err := nfsWorkspaceRel(resolved)
	if err != nil {
		return "", err
	}
	agentRel, err := runtime.NFSAgentDirSubPath(rel, dirName)
	if err != nil {
		return "", fmt.Errorf("workspace_storage nfs: %w", err)
	}
	return agentRel, nil
}

// nfsWorkspaceRel returns the project's export-relative workspace path that
// resolved describes, after checking that it is a local path and that the
// host base joined with it is the resolved host path.
func nfsWorkspaceRel(resolved runtime.ResolvedWorkspace) (string, error) {
	rel := resolved.ServerRelativePath
	if rel == "" || !filepath.IsLocal(rel) || filepath.Join(resolved.HostBase, rel) != resolved.HostPath {
		return "", fmt.Errorf("workspace_storage nfs: unexpected workspace path %q under %q", rel, resolved.HostBase)
	}
	return rel, nil
}
