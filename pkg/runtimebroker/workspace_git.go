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

package runtimebroker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// sharedWorkspaceKnownNotGit reports whether the broker positively knows
// that this start's workspace is the project's shared workspace on the
// broker's shared Layout and that it is not a git repository, so
// SCION_WORKSPACE_GIT=false may be sent (the Kubernetes runtime then allows
// the shared workspace; see k8sSharedRepoShapeRefusal in pkg/runtime).
//
// Every input is a broker or hub fact a project member cannot set:
//   - the broker's Layout is shared and the start has a project ID;
//   - the hub dispatched the shared-plain sharing mode (its workspaceMode
//     field, where empty means shared-plain), never a clone, worktree or
//     empty-per-agent mode;
//   - the hub sent no git source for the workspace: no per-agent GitClone,
//     no shared-workspace clone settings and no SharedWorkspace flag (in the
//     Config or at the top level of the start inputs). The
//     hub sets the flag and the clone settings only for a project with a git
//     remote (store.Project.IsSharedWorkspace, sharedWorkspaceCloneConfig)
//     and re-sends them on every start;
//   - no worktree was provisioned and no linked checkout is used;
//   - at decision time, the project's workspace directory on the share is a
//     visible directory (not a symlink) with no .git entry, and a bounded
//     `git rev-parse --is-inside-work-tree` in it fails with "not a git
//     repository" (workspaceGitProbe). Any other git outcome (a missing
//     binary, an exec or IO error, a timeout, an ownership refusal) is
//     unknown.
//
// Anything else, including a workspace directory that does not exist yet,
// returns false: the variable stays absent and the Kubernetes guard refuses
// the start (fail closed). Labels, project env, request env and template
// values are never read.
func (s *Server) sharedWorkspaceKnownNotGit(ctx context.Context, in startContextInputs, opts *api.StartOptions, worktreeProvisioned, linkedCheckout bool) bool {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() || in.ProjectID == "" {
		return false
	}
	if store.ResolveWorkspaceSharingMode(in.WorkspaceMode) != store.SharingModeSharedPlain {
		return false
	}
	// The top-level flag is what a restart carries when it builds no Config
	// (restartAgentConfig returns nil for an older hub's request).
	if in.SharedWorkspace {
		return false
	}
	if c := in.Config; c != nil && (c.SharedWorkspace || c.GitClone != nil || c.SharedWorkspaceClone != nil) {
		return false
	}
	if worktreeProvisioned || linkedCheckout || opts.GitClone != nil || opts.SharedWorkspaceClone != nil {
		return false
	}
	leaf, err := layout.Resolve(projectfs.Project{ID: in.ProjectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return false
	}
	return workspaceDirKnownNotGit(ctx, leaf.HostPath)
}

// workspaceDirKnownNotGit reports whether dir is, right now, a visible
// directory (not a symlink) that holds no .git entry and that git positively
// reports as not inside a work tree. Any error or doubt returns false.
func workspaceDirKnownNotGit(ctx context.Context, dir string) bool {
	if dir == "" || projectfs.ShareDirVisible(ctx, dir) != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(err, fs.ErrNotExist) {
		return false // a .git entry, or it could not be checked
	}
	return gitReportsNotARepository(ctx, dir)
}

// workspaceGitTimeout bounds the git call of gitReportsNotARepository: the
// directory is on the share, where a hung mount can block it.
var workspaceGitTimeout = projectfs.ShareVisibleTimeout

// workspaceGitWaitDelay bounds how long Run waits, after the context ends or
// git exits, for git's stderr to close (exec.Cmd.WaitDelay), so a descendant
// holding the pipe cannot keep the probe running.
const workspaceGitWaitDelay = 2 * time.Second

// workspaceGitProbe runs `git rev-parse --is-inside-work-tree` in dir with the
// host git arguments and environment (util.HostGitArgs, util.HostGitEnv) and
// LC_ALL=C, and returns git's stderr and the run error. A variable so tests
// can simulate exec failures.
//
// ctx bounds the call (gitReportsNotARepository gives it
// workspaceGitTimeout), and WaitDelay bounds the wait for the pipes after
// that. A git process stuck in uninterruptible NFS IO cannot be killed
// until that IO returns, so the probe can still outlive the bound in that
// case; ShareDirVisible, which runs first with its own bound, makes that
// unlikely.
var workspaceGitProbe = func(ctx context.Context, dir string) (stderr string, err error) {
	cmd := exec.CommandContext(ctx, "git", append(util.HostGitArgs(), "rev-parse", "--is-inside-work-tree")...)
	cmd.Dir = dir
	// Untranslated messages, so the "not a git repository" match is locale-stable.
	cmd.Env = append(util.HostGitEnv(os.Environ()), "LC_ALL=C")
	return runWorkspaceGitProbe(cmd)
}

// runWorkspaceGitProbe runs cmd with WaitDelay set and returns its stderr and
// the run error.
func runWorkspaceGitProbe(cmd *exec.Cmd) (string, error) {
	cmd.WaitDelay = workspaceGitWaitDelay
	var buf strings.Builder
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// gitReportsNotARepository reports whether git, run in dir within
// workspaceGitTimeout, cleanly failed (a positive exit code) with a stderr
// line starting "fatal: not a git repository": the only outcome read as
// "not git". Success (a work tree), a missing git binary, an exec or IO
// error, a timeout, a signal, or any other failure (such as an ownership
// refusal) returns false: unknown, so SCION_WORKSPACE_GIT stays absent. It
// is a separate helper from util.CheckGitRepoDir, whose (false, nil) for
// any non-ownership failure its other callers rely on.
func gitReportsNotARepository(ctx context.Context, dir string) bool {
	ctx, cancel := context.WithTimeout(ctx, workspaceGitTimeout)
	defer cancel()
	stderr, err := workspaceGitProbe(ctx, dir)
	if err == nil || ctx.Err() != nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !exitErr.Exited() || exitErr.ExitCode() <= 0 {
		return false
	}
	return gitNotARepositoryMessage(stderr)
}

// gitNotARepositoryMessage reports whether a line of git's stderr starts with
// git's own "fatal: not a git repository" message.
func gitNotARepositoryMessage(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), "fatal: not a git repository") {
			return true
		}
	}
	return false
}
