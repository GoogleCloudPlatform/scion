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
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// nfsBackend resolves workspace and shared-dir paths onto an NFS-backed
// filesystem. Resolution is deterministic from project/agent IDs — no DB
// lookup, no I/O — so any replica computes the same path.
//
// Layout under the NFS mount (design §3):
//
//	<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace
//	<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/shared-dirs/<name>
//
// Provision and Realize are stubs in N1-1; full implementations land in
// N1-4 (provisioning) and N1-3 (mount wiring).
type nfsBackend struct {
	cfg *config.V1NFSConfig
}

// NewNFSBackend returns a WorkspaceBackend backed by NFS shared storage.
// The cfg must be non-nil and should have defaults applied (ApplyNFSDefaults).
func NewNFSBackend(cfg *config.V1NFSConfig) WorkspaceBackend {
	return &nfsBackend{cfg: cfg}
}

func (b *nfsBackend) Name() string { return "nfs" }

// Resolve computes workspace and shared-dir paths on the NFS filesystem.
// The result includes both the server-relative path (for K8s subPath /
// Cloud Run server path) and the full host path (for Docker bind mounts).
//
// The first configured share is used by default. ProjectID is required.
//
// Paths are deterministic: same (ProjectID, ShareID, SubPathRoot) → same path.
// No I/O, no DB lookup.
func (b *nfsBackend) Resolve(in ResolveInput) (ResolvedWorkspace, error) {
	if in.ProjectID == "" {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: ProjectID is required")
	}
	if b.cfg == nil {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: NFS config is nil")
	}
	if len(b.cfg.Shares) == 0 {
		return ResolvedWorkspace{}, fmt.Errorf("nfsBackend.Resolve: no NFS shares configured")
	}

	share := b.cfg.Shares[0]
	subPathRoot := b.cfg.SubPathRoot
	if subPathRoot == "" {
		subPathRoot = "projects"
	}

	// Server-relative workspace path: <SubPathRoot>/<projectID>/workspace
	workspaceRelPath := filepath.Join(subPathRoot, in.ProjectID, "workspace")

	// Host base: <MountRoot>/<shareID>
	hostBase := filepath.Join(b.cfg.MountRoot, share.ID)

	// Full host path: <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace
	hostPath := filepath.Join(hostBase, workspaceRelPath)

	res := ResolvedWorkspace{
		HostPath:           hostPath,
		ServerRelativePath: workspaceRelPath,
		HostBase:           hostBase,
		Backend:            "nfs",
		SharedDirs:         make(map[string]ResolvedSharedDir, len(in.SharedDirNames)),
	}

	// Resolve shared dirs on NFS: <SubPathRoot>/<projectID>/shared-dirs/<name>
	for _, name := range in.SharedDirNames {
		sdRelPath := filepath.Join(subPathRoot, in.ProjectID, "shared-dirs", name)
		res.SharedDirs[name] = ResolvedSharedDir{
			HostPath:           filepath.Join(hostBase, sdRelPath),
			ServerRelativePath: sdRelPath,
		}
	}

	return res, nil
}

// provisionSentinelFile is the name of the sentinel file written atomically
// after a successful workspace clone/setup. Its presence short-circuits
// subsequent Provision calls — the workspace is already ready.
const provisionSentinelFile = ".scion-provisioned"

// provisionLockRetries is the number of times to retry acquiring the
// per-project advisory lock before giving up. Each retry sleeps briefly
// (provisionLockRetryDelay) to allow the current holder to finish.
const provisionLockRetries = 30

// provisionLockRetryDelay is the sleep between advisory lock acquisition
// retries. Provisioning (git clone) is typically short (seconds), so a
// short retry cadence is appropriate.
const provisionLockRetryDelay = 1 * time.Second

// Provision ensures the NFS workspace is ready for use. It implements the
// first-access provisioning flow from design §7 and §8.1:
//
//  1. Acquire per-project advisory lock (try with retry — provisioning is short).
//  2. If sentinel <projectRoot>/.scion-provisioned exists → done (reuse).
//  3. Else: mkdir -p, git clone, chown 1000:1000, mode 0770, write sentinel.
//  4. Release lock.
//
// For WorktreePerAgent mode, the shared checkout is cloned once under the lock;
// each agent then adds its own git worktree (also under the lock, because
// worktree add/remove touches shared .git metadata — design §9.2).
//
// ClonePerAgent mode MUST NOT reach this path — it is node-local and handled
// by localBackend. An assert guards this.
//
// The flow is idempotent and race-safe: two agents for the same project
// starting on two different nodes contend on the advisory lock; exactly one
// clones, the second sees the sentinel and reuses the workspace.
func (b *nfsBackend) Provision(in ProvisionInput) error {
	// Guard: ClonePerAgent must never use the NFS path. SelectWorkspaceBackend
	// already routes it to localBackend, but assert here as defense in depth.
	if in.Mode == store.SharingModeClonePerAgent {
		return fmt.Errorf("nfsBackend.Provision: ClonePerAgent mode must not use NFS backend " +
			"(should be routed to localBackend by SelectWorkspaceBackend)")
	}

	if in.Resolved.HostPath == "" {
		return fmt.Errorf("nfsBackend.Provision: Resolved.HostPath is required")
	}
	if in.ProjectID == "" {
		return fmt.Errorf("nfsBackend.Provision: ProjectID is required")
	}

	// The project root is the parent of the workspace dir:
	// <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/ contains workspace/ + shared-dirs/.
	projectRoot := filepath.Dir(in.Resolved.HostPath)

	ctx := context.Background()

	// --- Step 1: Acquire per-project advisory lock ---
	release, err := b.acquireProvisionLock(ctx, in)
	if err != nil {
		return fmt.Errorf("nfsBackend.Provision: failed to acquire lock for project %s: %w", in.ProjectID, err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			slog.Warn("nfsBackend.Provision: failed to release advisory lock",
				"project_id", in.ProjectID, "error", releaseErr)
		}
	}()

	// --- Step 2: Check sentinel ---
	sentinelPath := filepath.Join(projectRoot, provisionSentinelFile)
	if _, err := os.Stat(sentinelPath); err == nil {
		// Already provisioned — skip to worktree setup if needed.
		slog.Debug("nfsBackend.Provision: workspace already provisioned (sentinel exists)",
			"project_id", in.ProjectID, "sentinel", sentinelPath)
		return b.ensureWorktree(in)
	}

	// --- Step 3: Provision (mkdir + clone + chown + sentinel) ---
	slog.Info("nfsBackend.Provision: provisioning workspace",
		"project_id", in.ProjectID, "host_path", in.Resolved.HostPath)

	// Create workspace directory.
	if err := os.MkdirAll(in.Resolved.HostPath, 0770); err != nil {
		return fmt.Errorf("nfsBackend.Provision: mkdir workspace %s: %w", in.Resolved.HostPath, err)
	}

	// Create shared-dir directories.
	for name, sd := range in.Resolved.SharedDirs {
		if err := os.MkdirAll(sd.HostPath, 0770); err != nil {
			return fmt.Errorf("nfsBackend.Provision: mkdir shared-dir %q %s: %w", name, sd.HostPath, err)
		}
	}

	// Git clone if project is git-backed.
	if in.GitClone != nil && in.GitClone.URL != "" {
		if err := b.gitCloneWorkspace(in); err != nil {
			return fmt.Errorf("nfsBackend.Provision: git clone: %w", err)
		}
	}

	// Chown to stable NFS UID/GID (design §9.1). This is a ONE-TIME operation
	// under the advisory lock — per-start chown is skipped for NFS (see N1-5).
	uid, gid := b.resolveUID(in), b.resolveGID(in)
	if err := b.chownProjectTree(projectRoot, uid, gid); err != nil {
		slog.Warn("nfsBackend.Provision: chown failed (non-fatal, may lack privileges)",
			"project_id", in.ProjectID, "path", projectRoot, "uid", uid, "gid", gid, "error", err)
		// Non-fatal: operator may have pre-chowned. Continue to write sentinel.
	}

	// Write sentinel atomically.
	if err := writeSentinel(sentinelPath); err != nil {
		return fmt.Errorf("nfsBackend.Provision: write sentinel: %w", err)
	}

	slog.Info("nfsBackend.Provision: workspace provisioned successfully",
		"project_id", in.ProjectID, "host_path", in.Resolved.HostPath)

	// --- Step 4: Worktree setup (if WorktreePerAgent) ---
	return b.ensureWorktree(in)
}

// acquireProvisionLock acquires the per-project advisory lock, retrying briefly
// if another node currently holds it. Returns a release func.
//
// The retry loop respects context cancellation so that server shutdown is not
// blocked for up to provisionLockRetries × provisionLockRetryDelay.
func (b *nfsBackend) acquireProvisionLock(ctx context.Context, in ProvisionInput) (func() error, error) {
	if in.Locker == nil {
		// No locker available — degrade to unguarded (correct for single-node,
		// unsafe for multi-node). Log a warning.
		slog.Warn("nfsBackend.Provision: no advisory locker available — provisioning is unguarded",
			"project_id", in.ProjectID)
		return func() error { return nil }, nil
	}

	objID := store.StableProjectHash(in.ProjectID)
	ticker := time.NewTicker(provisionLockRetryDelay)
	defer ticker.Stop()

	for attempt := 0; attempt < provisionLockRetries; attempt++ {
		acquired, release, err := in.Locker.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, objID)
		if err != nil {
			return nil, fmt.Errorf("advisory lock attempt %d: %w", attempt, err)
		}
		if acquired {
			return release, nil
		}
		// Another node holds the lock — it's provisioning this project.
		// Wait briefly and retry, but honour context cancellation.
		slog.Debug("nfsBackend.Provision: lock held by another node, retrying",
			"project_id", in.ProjectID, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while waiting for provisioning lock (project %s): %w",
				in.ProjectID, ctx.Err())
		case <-ticker.C:
		}
	}

	return nil, fmt.Errorf("failed to acquire provisioning lock after %d attempts (project %s)",
		provisionLockRetries, in.ProjectID)
}

// gitCloneWorkspace performs the git clone into the NFS workspace directory.
// It clones into the workspace path (in.Resolved.HostPath).
func (b *nfsBackend) gitCloneWorkspace(in ProvisionInput) error {
	gc := in.GitClone

	args := []string{"clone"}

	// Set depth (default: 1 for shallow clone, 0 = full).
	depth := gc.Depth
	if depth == 0 {
		depth = 1
	}
	if depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", depth))
	}

	// Set branch if specified.
	if gc.Branch != "" {
		args = append(args, "--branch", gc.Branch)
	}

	// Clone into the workspace directory.
	args = append(args, gc.URL, in.Resolved.HostPath)

	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		// Disable interactive prompts during provisioning.
		"GIT_TERMINAL_PROMPT=0",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// If workspace is not empty (e.g. a partially-failed prior attempt),
		// the clone will fail. Check and handle.
		if strings.Contains(string(output), "already exists and is not an empty directory") {
			slog.Warn("nfsBackend.Provision: workspace directory not empty, assuming prior partial clone",
				"project_id", in.ProjectID, "path", in.Resolved.HostPath)
			// The sentinel wasn't written, so this is a prior failed attempt.
			// Reuse what's there — if .git exists, it may be usable.
			if _, statErr := os.Stat(filepath.Join(in.Resolved.HostPath, ".git")); statErr == nil {
				return nil // .git exists, treat as provisioned
			}
			return fmt.Errorf("git clone failed and no .git in %s: %s", in.Resolved.HostPath, string(output))
		}
		return fmt.Errorf("git clone %s: %s", gc.URL, strings.TrimSpace(string(output)))
	}
	return nil
}

// ensureWorktree creates a per-agent worktree if the mode is WorktreePerAgent.
// For SharedPlain mode this is a no-op.
// The worktree add is done under the already-held advisory lock (design §9.2:
// worktree add/remove touches shared .git metadata).
func (b *nfsBackend) ensureWorktree(in ProvisionInput) error {
	if in.Mode != store.SharingModeWorktreePerAgent {
		return nil // SharedPlain: nothing to do
	}

	if in.AgentID == "" {
		return fmt.Errorf("nfsBackend.Provision: AgentID is required for WorktreePerAgent mode")
	}

	// Worktree path: <workspace>/worktrees/<agentID>
	worktreePath := filepath.Join(in.Resolved.HostPath, "worktrees", in.AgentID)

	// If the worktree already exists, skip.
	if _, err := os.Stat(worktreePath); err == nil {
		slog.Debug("nfsBackend.Provision: worktree already exists",
			"agent_id", in.AgentID, "path", worktreePath)
		return nil
	}

	// Verify the shared checkout exists (.git dir present).
	gitDir := filepath.Join(in.Resolved.HostPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return fmt.Errorf("nfsBackend.Provision: shared checkout .git not found at %s — "+
			"cannot create worktree without a cloned repository", gitDir)
	}

	// Derive a branch name from the agent name or ID.
	branchName := in.AgentID
	if in.AgentName != "" {
		branchName = sanitizeBranchName(in.AgentName)
	}

	slog.Info("nfsBackend.Provision: creating worktree",
		"agent_id", in.AgentID, "branch", branchName, "path", worktreePath)

	// git worktree add <path> -b <branch>
	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath)
	cmd.Dir = in.Resolved.HostPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := strings.TrimSpace(string(output))
		// If branch already exists, try without -b.
		if strings.Contains(outputStr, "already exists") {
			cmd = exec.Command("git", "worktree", "add", worktreePath, branchName)
			cmd.Dir = in.Resolved.HostPath
			output, err = cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("git worktree add (reuse branch): %s", strings.TrimSpace(string(output)))
			}
			return nil
		}
		return fmt.Errorf("git worktree add: %s", outputStr)
	}
	return nil
}

// sanitizeBranchName produces a git-safe branch name from an agent name.
func sanitizeBranchName(name string) string {
	// Replace characters invalid in git branch names.
	replacer := strings.NewReplacer(
		" ", "-", "/", "-", "\\", "-", "..", "-",
		"~", "-", "^", "-", ":", "-", "?", "-",
		"*", "-", "[", "-", "]", "-",
	)
	result := replacer.Replace(name)
	// Trim leading/trailing dashes and dots.
	result = strings.Trim(result, "-.")
	if result == "" {
		return "agent"
	}
	return result
}

// chownProjectTree sets ownership of the project root and its contents to the
// given UID/GID. This is a ONE-TIME operation done under the advisory lock
// during first provisioning (design §9.1). Per-start chown is NOT done for
// NFS (slow/racy over the network).
func (b *nfsBackend) chownProjectTree(projectRoot string, uid, gid int) error {
	// Use chown -R for recursive ownership change.
	cmd := exec.Command("chown", "-R", fmt.Sprintf("%d:%d", uid, gid), projectRoot)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown -R %d:%d %s: %s", uid, gid, projectRoot, strings.TrimSpace(string(output)))
	}
	return nil
}

// resolveUID returns the NFS UID to use for chown, defaulting to 1000.
func (b *nfsBackend) resolveUID(in ProvisionInput) int {
	if in.NFSUID != 0 {
		return in.NFSUID
	}
	if b.cfg != nil && b.cfg.UID != 0 {
		return b.cfg.UID
	}
	return 1000
}

// resolveGID returns the NFS GID to use for chown, defaulting to 1000.
func (b *nfsBackend) resolveGID(in ProvisionInput) int {
	if in.NFSGID != 0 {
		return in.NFSGID
	}
	if b.cfg != nil && b.cfg.GID != 0 {
		return b.cfg.GID
	}
	return 1000
}

// writeSentinel writes the provisioning sentinel file atomically using
// write-to-temp + rename. The sentinel's existence is the fast-path check
// that short-circuits re-provisioning.
func writeSentinel(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".scion-provisioned-*")
	if err != nil {
		return fmt.Errorf("create temp sentinel: %w", err)
	}
	tmpName := tmp.Name()

	// Write a timestamp for debugging.
	_, _ = fmt.Fprintf(tmp, "provisioned_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp sentinel: %w", err)
	}

	// Atomic rename.
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename sentinel: %w", err)
	}
	return nil
}

// Realize emits a Docker bind-mount descriptor from the NFS host path to the
// container workspace. The host path points at the project subtree under the
// NFS mount (<MountRoot>/<shareID>/<SubPathRoot>/<projectID>/workspace), NOT
// the export root — this is the critical isolation guarantee (design §9.4).
//
// For K8s the SubPath and PVClaimName fields are populated for PVC+subPath
// wiring; for Docker, HostPath is the bind-mount source.
func (b *nfsBackend) Realize(in RealizeInput) (MountDescriptor, error) {
	target := in.ContainerWorkspace
	if target == "" {
		target = "/workspace"
	}

	// Isolation guard: never bind the host base (export root mount) directly.
	// The resolved HostPath must be a subdirectory of HostBase, not equal to it.
	if err := ValidateNotExportRoot(in.Resolved.HostPath, in.Resolved.HostBase); err != nil {
		return MountDescriptor{}, err
	}

	desc := MountDescriptor{
		Type:     "nfs",
		HostPath: in.Resolved.HostPath,
		Target:   target,
		SubPath:  in.Resolved.ServerRelativePath,
	}

	// Populate K8s PVC info from the first share if available.
	if b.cfg != nil && len(b.cfg.Shares) > 0 && b.cfg.Shares[0].PVName != "" {
		desc.PVClaimName = b.cfg.Shares[0].PVName
	}

	return desc, nil
}
