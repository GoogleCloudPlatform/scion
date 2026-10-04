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

package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// AgentWorkspaceDir is the name of the agent's own workspace inside its
	// agent directory (<project>/agents/<agent name>/workspace) for
	// clone-per-agent projects on the NFS workspace. The agent container
	// mounts it at /workspace and clones the repository into it.
	AgentWorkspaceDir = "workspace"

	// AgentBranchFile is the file in the agent directory that records the
	// branch the agent's workspace was created for.
	AgentBranchFile = ".scion-agent-branch"

	// removedAgentWorkspacePrefix names an agent's workspace once
	// RemoveAgentWorkspace has moved it aside inside the agent directory;
	// PurgeRemovedAgentWorkspaces deletes such directories.
	removedAgentWorkspacePrefix = ".removing-"
)

// ErrAgentBranchMismatch is returned by ProvisionAgentDir when the agent's
// kept workspace was created for another branch than the one requested.
var ErrAgentBranchMismatch = errors.New("the agent's kept workspace was created for another branch")

// ProvisionAgentDir prepares the agent directory of a clone-per-agent agent
// on the NFS workspace. The Kubernetes provisioning init container calls it
// with in.Resolved.HostPath set to the agent directory
// (<project>/agents/<agent name>), in.AgentID set to the agent's slug and
// in.AgentName set to the agent's branch. It does not clone and runs no
// git: the agent container clones into the workspace, as on the local
// runtimes, with the agent's own credentials.
//
// Under a file lock in the agent directory, it:
//   - creates the workspace directory (<agent dir>/workspace) when it is
//     missing; an existing one must be a plain directory;
//   - checks the branch record (AgentBranchFile): when the workspace holds
//     files other than marker entries (see api.IsWorkspaceMarker) and the
//     record names another branch, it returns
//     ErrAgentBranchMismatch and changes nothing; otherwise it records the
//     requested branch;
//   - chowns the workspace directory itself (not its contents) to
//     NFSUID:NFSGID while it holds only marker entries, and the branch
//     record when it was written; a workspace that holds files is left as
//     it is;
//   - creates each shared directory, and chowns it while it is empty;
//   - writes the provisioning sentinel in the agent directory.
//
// The lock, the record and the sentinel live in the agent directory, next
// to the workspace and not in it, so the agent container still finds an
// empty workspace to clone into.
//
// With in.Mode empty-per-agent (design #2703 P3) it does the same except
// for the branch: in.AgentName is ignored, and the branch record is neither
// checked nor written. Nothing ever clones into that workspace.
func ProvisionAgentDir(in ProvisionInput) error {
	emptyWorkspace := in.Mode == store.SharingModeEmptyPerAgent
	if in.Mode != store.SharingModeClonePerAgent && !emptyWorkspace {
		return fmt.Errorf("ProvisionAgentDir: mode %q is not clone-per-agent or empty-per-agent", in.Mode)
	}
	if slug, err := api.ValidateAgentName(in.AgentID); err != nil || slug != in.AgentID {
		return fmt.Errorf("ProvisionAgentDir: agent name %q is not an agent slug", in.AgentID)
	}
	branch := in.AgentName
	if !emptyWorkspace && (branch == "" || branch != strings.TrimSpace(branch) || strings.ContainsAny(branch, "\n\r")) {
		return fmt.Errorf("ProvisionAgentDir: invalid branch %q", branch)
	}
	agentDir := in.Resolved.HostPath
	if agentDir == "" {
		return fmt.Errorf("ProvisionAgentDir: Resolved.HostPath is required")
	}
	if err := checkPlainDir(agentDir); err != nil {
		return fmt.Errorf("ProvisionAgentDir: %w", err)
	}

	ctx := in.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	held, err := acquireFileLockWithin(ctx, agentDir, fileLockWait(in))
	if err != nil {
		return fmt.Errorf("ProvisionAgentDir: failed to acquire the lock in %s: %w", agentDir, err)
	}
	defer func() {
		if releaseErr := held.release(); releaseErr != nil {
			slog.Warn("ProvisionAgentDir: failed to release the lock", "path", agentDir, "error", releaseErr)
		}
	}()

	uid, gid := resolveUID(in), resolveGID(in)
	chown := func(path string) error {
		if err := lchownFile(path, uid, gid); err != nil {
			if in.RequireChownSuccess {
				return fmt.Errorf("ProvisionAgentDir: chown %s to %d:%d: %w", path, uid, gid, err)
			}
			slog.Warn("ProvisionAgentDir: chown failed (continuing)", "path", path, "uid", uid, "gid", gid, "error", err)
		}
		return nil
	}

	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	if err := ensurePlainDir(workspace); err != nil {
		return fmt.Errorf("ProvisionAgentDir: %w", err)
	}
	populated, err := WorkspaceHasContent(workspace)
	if err != nil {
		return fmt.Errorf("ProvisionAgentDir: read %s: %w", workspace, err)
	}

	recordPath := filepath.Join(agentDir, AgentBranchFile)
	recorded := readAgentBranch(recordPath)
	if emptyWorkspace {
		// No branch: the record is neither checked nor written.
		recorded = branch
	}
	if populated && recorded != "" && recorded != branch {
		return fmt.Errorf("%w: agent %q has a kept workspace created for branch %q, not %q; "+
			"delete the agent with its files, or create it with branch %q", ErrAgentBranchMismatch, in.AgentID, recorded, branch, recorded)
	}
	recordWritten := false
	if recorded != branch {
		if !held.stillOwned() {
			return fmt.Errorf("ProvisionAgentDir: lost the lock in %s before recording the branch", agentDir)
		}
		if err := writeFileAtomic(recordPath, []byte(branch+"\n"), 0o644); err != nil {
			return fmt.Errorf("ProvisionAgentDir: record the branch: %w", err)
		}
		recordWritten = true
	}

	if !populated {
		if err := chown(workspace); err != nil {
			return err
		}
	}
	if recordWritten {
		if err := chown(recordPath); err != nil {
			return err
		}
	}

	for name, sd := range in.Resolved.SharedDirs {
		if err := os.MkdirAll(sd.HostPath, 0o770); err != nil {
			return fmt.Errorf("ProvisionAgentDir: mkdir shared-dir %q %s: %w", name, sd.HostPath, err)
		}
		if has, err := dirHasEntries(sd.HostPath); err == nil && !has {
			if err := chown(sd.HostPath); err != nil {
				return err
			}
		}
	}

	sentinel := filepath.Join(agentDir, ProvisionSentinelFile)
	if _, err := os.Lstat(sentinel); errors.Is(err, fs.ErrNotExist) {
		if !held.stillOwned() {
			return fmt.Errorf("ProvisionAgentDir: lost the lock in %s before writing the sentinel", agentDir)
		}
		if err := writeSentinel(sentinel); err != nil {
			return fmt.Errorf("ProvisionAgentDir: write sentinel: %w", err)
		}
	}
	return nil
}

// RemoveAgentWorkspace removes the workspace of a clone-per-agent agent from
// its agent directory (agentDir/workspace) and drops its branch record, so
// an agent created again with the same name starts from an empty
// workspace. It runs under the same file lock as ProvisionAgentDir, which
// lives in agentDir. lockWait bounds only the wait for that lock; the
// caller's cancellation is ignored, so the steps under the lock always
// finish.
//
// agentDir must be a plain directory. The workspace is renamed to a
// .removing- name inside agentDir, a single step, so a removal that stops
// partway never leaves a half-deleted workspace at the agent's path.
// PurgeRemovedAgentWorkspaces deletes the renamed directory afterwards.
// Nothing outside agentDir is touched. A missing agent directory or
// workspace is not an error.
func RemoveAgentWorkspace(ctx context.Context, agentDir string, lockWait time.Duration) error {
	if _, err := os.Lstat(agentDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := checkPlainDir(agentDir); err != nil {
		return fmt.Errorf("remove agent workspace: %w; left in place", err)
	}
	held, err := acquireFileLockWithin(context.WithoutCancel(ctx), agentDir, lockWait)
	if err != nil {
		return fmt.Errorf("remove agent workspace in %s: %w", agentDir, err)
	}
	defer func() {
		if releaseErr := held.release(); releaseErr != nil {
			slog.Warn("remove agent workspace: failed to release the lock", "path", agentDir, "error", releaseErr)
		}
	}()

	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	fi, err := os.Lstat(workspace)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("remove agent workspace %s: %w", workspace, err)
	case !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("remove agent workspace %s: not a plain directory; left in place", workspace)
	default:
		if !held.stillOwned() {
			return fmt.Errorf("remove agent workspace: lost the lock in %s; left in place", agentDir)
		}
		to := filepath.Join(agentDir, removedAgentWorkspacePrefix+AgentWorkspaceDir+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Rename(workspace, to); err != nil {
			return fmt.Errorf("remove agent workspace: move %s aside: %w", workspace, err)
		}
	}
	if err := os.Remove(filepath.Join(agentDir, AgentBranchFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove agent workspace: drop the branch record in %s: %w", agentDir, err)
	}
	return nil
}

// PurgeRemovedAgentWorkspaces deletes the directories RemoveAgentWorkspace
// moved aside in agentDir, including any left by an earlier removal that
// stopped partway. Nothing refers to them any more, so it runs without the
// lock and without a deadline. agentDir must be a plain directory; the
// deletes run inside that directory as opened, so they stay there even if
// the path is replaced meanwhile.
func PurgeRemovedAgentWorkspaces(agentDir string) error {
	return purgePrefixedEntries(agentDir, removedAgentWorkspacePrefix)
}

// purgePrefixedEntries deletes every entry of the plain directory dir whose
// name starts with prefix, through an os.Root opened on dir.
func purgePrefixedEntries(dir, prefix string) error {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	// Lstat does not follow symlinks, so this holds only when dir is the
	// plain directory that was opened.
	if fi, err := os.Lstat(dir); err != nil || !os.SameFile(fi, opened) {
		return nil
	}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if err := root.RemoveAll(e.Name()); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(dir, e.Name()), err))
		}
	}
	return errors.Join(errs...)
}

// WorkspaceHasContent reports whether dir holds any entry other than the
// marker entries api.IsWorkspaceMarker names, the same rule as the clone
// step. A missing dir has no content.
func WorkspaceHasContent(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if !api.IsWorkspaceMarker(e.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// readAgentBranch returns the branch recorded in path, or "" when there is
// no record. Only a regular file is read.
func readAgentBranch(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// checkPlainDir returns an error unless path is a directory and not a
// symlink.
func checkPlainDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a plain directory", path)
	}
	return nil
}

// ensurePlainDir creates the directory path when it is missing, and
// otherwise checks that it is a plain directory.
func ensurePlainDir(path string) error {
	err := os.Mkdir(path, 0o775)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	return checkPlainDir(path)
}
