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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// MoveMarkerFile is the move marker a forced move's target broker writes
// into each agent directory the move creates (the agent directory, and on
// Kubernetes with NFS workspace storage the agent's own directory on the
// export), holding the move's reincarnation ID. It sits beside the agent's
// home and workspace, never inside the workspace. A rollback delete removes
// only directories that carry the marker with its own reincarnation ID
// (CheckMoveMarkers). It stays after a successful move; the next forced
// move has a new reincarnation ID.
const MoveMarkerFile = ".scion-move"

// moveMarkerProjectDir resolves CheckMoveMarkers' project directory
// (config.GetResolvedProjectDir); a variable so a test can make it fail.
var moveMarkerProjectDir = config.GetResolvedProjectDir

// ErrMoveMarkerMismatch is returned by CheckMoveMarkers when an agent
// directory exists without the move marker, or with another move's ID, or
// cannot be checked: those files were not created by this move.
var ErrMoveMarkerMismatch = errors.New("the agent's files on this broker were not created by this move")

type moveReincarnationIDKey struct{}

// withMoveReincarnationID returns ctx carrying a forced move's
// reincarnation ID for ProvisionAgent's move marker.
func withMoveReincarnationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, moveReincarnationIDKey{}, id)
}

// moveReincarnationIDFrom returns the forced move's reincarnation ID ctx
// carries, or "".
func moveReincarnationIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(moveReincarnationIDKey{}).(string)
	return id
}

// writeMoveMarker creates dir's move marker holding id. It never replaces
// an existing marker.
func writeMoveMarker(dir, id string) error {
	if id == "" || strings.ContainsAny(id, "\n\r") {
		return fmt.Errorf("invalid move reincarnation ID %q", id)
	}
	f, err := os.OpenFile(filepath.Join(dir, MoveMarkerFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readMoveMarker returns the reincarnation ID of dir's move marker. The
// marker must be a regular file, never followed through a symlink.
func readMoveMarker(dir string) (string, error) {
	path := filepath.Join(dir, MoveMarkerFile)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// checkMoveMarker returns nil when dir does not exist, or carries the move
// marker with exactly id, and ErrMoveMarkerMismatch otherwise.
func checkMoveMarker(dir, id string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrMoveMarkerMismatch, dir, err)
	}
	got, err := readMoveMarker(dir)
	if err != nil {
		return fmt.Errorf("%w: %s has no readable move marker: %v", ErrMoveMarkerMismatch, dir, err)
	}
	if got != id {
		return fmt.Errorf("%w: %s is marked by another move", ErrMoveMarkerMismatch, dir)
	}
	return nil
}

// CheckMoveMarkers confirms, before a forced move's rollback deletes the
// agent agentName's files on this broker, that every agent directory the
// delete would remove and that exists carries the move marker with
// exactly moveID. The directories are those DeleteAgentFiles removes (the
// project's agents/<name>, the external agents root's <name>, the global
// project's per-agent workspace) and the agent's own directory on the NFS
// export that RemoveNFSAgentFiles removes. A directory that does not
// exist is fine. The worktrees the same delete removes (the
// worktree-per-agent worktree under the shared base, a shared worktree the
// agent is registered on, the agent's worktree on the NFS export) must not
// exist at all: a forced move never creates one. The check is all or
// nothing: any present directory without the marker, or with another ID,
// any present worktree or registration, or anything that cannot be
// checked, is ErrMoveMarkerMismatch, and the delete must remove nothing. It
// returns the paths it checked, and changes nothing.
func (m *AgentManager) CheckMoveMarkers(ctx context.Context, projectPath, projectID, agentName, moveID string) ([]string, error) {
	if moveID == "" {
		return nil, fmt.Errorf("%w: no move reincarnation ID", ErrMoveMarkerMismatch)
	}
	if _, err := checkAgentDirContained(projectPath, agentName, false); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMoveMarkerMismatch, err)
	}
	// dirs must each be absent or carry this move's marker; worktrees must
	// each be absent (a recreate never creates a worktree, so one that
	// exists predates the move).
	var dirs, worktrees []string
	// The project directory decides most of what the delete removes: one
	// that cannot be resolved refuses (fail closed), like any other lookup
	// error here.
	projectDir, err := moveMarkerProjectDir(projectPath)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve the project directory", ErrMoveMarkerMismatch)
	}
	dirs = append(dirs, filepath.Join(projectDir, "agents", agentName))
	if config.IsGlobalProjectDir(projectDir) {
		dirs = append(dirs, filepath.Join(projectDir, "workspace", agentName))
	}
	layout := resolveAgentProjectLayout(projectDir)
	if layout.externalAgentsRoot != "" {
		dirs = append(dirs, filepath.Join(layout.externalAgentsRoot, agentName))
	}
	// DeleteAgentFiles removes the worktree-per-agent worktree under
	// the shared base, and a shared worktree the agent is registered
	// on; a forced move creates neither.
	if layout.sharedBaseIsRepo {
		worktrees = append(worktrees, filepath.Join(layout.sharedBase, "worktrees", agentName))
	}
	if layout.repoRoot != "" {
		_, _, found, findErr := provision.FindBranchForAgent(layout.repoRoot, projectDir, agentName)
		if findErr != nil {
			return dirs, fmt.Errorf("%w: read the worktree sharer registry: %v", ErrMoveMarkerMismatch, findErr)
		}
		if found {
			return dirs, fmt.Errorf("%w: the agent is registered on a shared worktree", ErrMoveMarkerMismatch)
		}
	}
	hostBase, rel, err := m.nfsExportWorkspace(projectPath, projectID, agentName)
	if err != nil {
		return dirs, fmt.Errorf("%w: resolve the NFS export: %v", ErrMoveMarkerMismatch, err)
	}
	if hostBase != "" {
		agentRel, err := runtime.NFSAgentDirSubPath(rel, agentName)
		if err != nil {
			return dirs, fmt.Errorf("%w: workspace_storage nfs: %v", ErrMoveMarkerMismatch, err)
		}
		dirs = append(dirs, filepath.Join(hostBase, agentRel))
		// RemoveNFSAgentFiles also removes the agent's worktree under the
		// project's shared checkout on the export.
		worktrees = append(worktrees, filepath.Join(hostBase, nfsWorktreeSubPath(rel, agentName)))
	}
	checked := append(append([]string(nil), dirs...), worktrees...)
	for _, dir := range dirs {
		if err := checkMoveMarker(dir, moveID); err != nil {
			return checked, err
		}
	}
	for _, wt := range worktrees {
		if _, err := os.Lstat(wt); err == nil {
			return checked, fmt.Errorf("%w: a worktree exists at %s", ErrMoveMarkerMismatch, wt)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return checked, fmt.Errorf("%w: stat %s: %v", ErrMoveMarkerMismatch, wt, err)
		}
	}
	return checked, nil
}

// markMoveExportAgentDir marks the agent's own directory on the NFS export,
// exportDir, which this start has just created (the caller's atomic claim),
// with the move marker of its local agent directory, agentDir, when agentDir
// carries one: the start of a forced move's agent creates that directory
// just before its pod, so nothing else has written there yet. An agent
// without a local move marker is not affected. A directory that already
// existed, or that this start did not create (the node creates it at pod
// start), is never passed here and stays unmarked; a rollback delete then
// refuses it.
func markMoveExportAgentDir(agentDir, exportDir string) error {
	moveID, err := readMoveMarker(agentDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the agent's move marker: %w", err)
	}
	if info, err := os.Lstat(exportDir); err != nil || !info.IsDir() {
		return nil
	}
	if err := writeMoveMarker(exportDir, moveID); err != nil {
		return fmt.Errorf("write the move marker on the NFS export: %w", err)
	}
	return nil
}
