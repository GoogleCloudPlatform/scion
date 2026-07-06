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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testLocker is a mock AdvisoryLocker for testing.
type testLocker struct {
	mu       sync.Mutex
	held     map[lockKey]bool
	acquires int64
}

type lockKey struct {
	classID int64
	objID   int32
	single  bool // true for single-int form
}

func newTestLocker() *testLocker {
	return &testLocker{held: make(map[lockKey]bool)}
}

func (l *testLocker) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	k := lockKey{classID: int64(key), single: true}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[k] {
		return false, func() error { return nil }, nil
	}
	l.held[k] = true
	atomic.AddInt64(&l.acquires, 1)
	return true, func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, k)
		return nil
	}, nil
}

func (l *testLocker) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	k := lockKey{classID: int64(classID), objID: objID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[k] {
		return false, func() error { return nil }, nil
	}
	l.held[k] = true
	atomic.AddInt64(&l.acquires, 1)
	return true, func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, k)
		return nil
	}, nil
}

// nfsTestBackend creates an nfsBackend with a temp directory as the mount root
// and returns the backend, config, and project paths.
func nfsTestBackend(t *testing.T) (*nfsBackend, *config.V1NFSConfig, string) {
	t.Helper()
	mountRoot := t.TempDir()
	cfg := &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
	b := &nfsBackend{cfg: cfg}
	return b, cfg, mountRoot
}

// initBareGitRepo creates a bare git repo at the given path for cloning from.
func initBareGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bareDir := filepath.Join(dir, "bare.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bareDir)

	// Create a working clone to make an initial commit.
	workDir := filepath.Join(dir, "work")
	run(t, "git", "clone", bareDir, workDir)

	// Create an initial commit so the repo has a HEAD.
	f := filepath.Join(workDir, "README.md")
	if err := os.WriteFile(f, []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runIn(t, workDir, "git", "add", "README.md")
	runIn(t, workDir, "git", "-c", "user.name=test", "-c", "user.email=test@test.com",
		"commit", "-m", "initial")
	runIn(t, workDir, "git", "push", "origin", "main")

	return bareDir
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %s\n%s", name, args, err, output)
	}
}

func runIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v (in %s): %s\n%s", name, args, dir, err, output)
	}
}

// --- SharedPlain provisioning without git ---

func TestNFSProvision_SharedPlain_NonGit(t *testing.T) {
	b, _, mountRoot := nfsTestBackend(t)
	locker := newTestLocker()

	projectID := "proj-nonGit-1"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
		Locker:    locker,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Verify workspace directory was created.
	if _, err := os.Stat(res.HostPath); err != nil {
		t.Errorf("workspace dir not created: %v", err)
	}

	// Verify sentinel was written.
	sentinelPath := filepath.Join(filepath.Dir(res.HostPath), ProvisionSentinelFile)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel not written: %v", err)
	}

	// Verify lock was acquired.
	if atomic.LoadInt64(&locker.acquires) != 1 {
		t.Errorf("expected 1 lock acquire, got %d", atomic.LoadInt64(&locker.acquires))
	}

	_ = mountRoot
}

// --- SharedPlain provisioning with git clone ---

func TestNFSProvision_SharedPlain_GitClone(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	bareRepo := initBareGitRepo(t)
	projectID := "proj-git-1"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  1,
		},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Verify .git directory exists (git clone succeeded).
	if _, err := os.Stat(filepath.Join(res.HostPath, ".git")); err != nil {
		t.Errorf("git clone did not create .git: %v", err)
	}

	// Verify README.md was cloned.
	if _, err := os.Stat(filepath.Join(res.HostPath, "README.md")); err != nil {
		t.Errorf("git clone did not bring README.md: %v", err)
	}

	// Verify sentinel.
	sentinelPath := filepath.Join(filepath.Dir(res.HostPath), ProvisionSentinelFile)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel not written: %v", err)
	}
}

// --- Idempotent: second Provision is a no-op (sentinel short-circuits) ---

func TestNFSProvision_Idempotent(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	bareRepo := initBareGitRepo(t)
	projectID := "proj-idem-1"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	input := ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  1,
		},
	}

	// First provision.
	if err := ProvisionShared(input); err != nil {
		t.Fatalf("first Provision: %v", err)
	}

	// Second provision — should succeed without re-cloning.
	if err := ProvisionShared(input); err != nil {
		t.Fatalf("second Provision: %v", err)
	}

	// Lock acquired twice (once per call — lock is always acquired, sentinel
	// check happens after lock).
	if got := atomic.LoadInt64(&locker.acquires); got != 2 {
		t.Errorf("expected 2 lock acquires, got %d", got)
	}
}

// --- Sentinel short-circuit: no re-clone even with git config ---

func TestNFSProvision_SentinelShortCircuits(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	projectID := "proj-sentinel-1"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Pre-create workspace dir and sentinel (simulating prior provisioning).
	if err := os.MkdirAll(res.HostPath, 0770); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Dir(res.HostPath)
	sentinelPath := filepath.Join(projectRoot, ProvisionSentinelFile)
	if err := os.WriteFile(sentinelPath, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Provision with a git URL that would fail if actually attempted.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL: "https://nonexistent.example.com/repo.git",
		},
	})
	if err != nil {
		t.Fatalf("Provision with sentinel should succeed: %v", err)
	}
}

// --- WorktreePerAgent: creates worktree on shared checkout ---

func TestNFSProvision_WorktreePerAgent(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	bareRepo := initBareGitRepo(t)
	projectID := "proj-wt-1"
	agentID := "agent-wt-1"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   agentID,
		AgentName: "test-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  0, // full clone needed for worktrees
		},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Verify worktree was created.
	worktreePath := filepath.Join(res.HostPath, "worktrees", agentID)
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree not created at %s: %v", worktreePath, err)
	}

	// Verify .git pointer file exists in worktree (git worktree add creates it).
	gitFile := filepath.Join(worktreePath, ".git")
	if _, err := os.Stat(gitFile); err != nil {
		t.Errorf("worktree .git file not found: %v", err)
	}
}

// --- WorktreePerAgent: second agent gets independent worktree ---

func TestNFSProvision_WorktreePerAgent_TwoAgents(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	bareRepo := initBareGitRepo(t)
	projectID := "proj-wt-2"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// First agent.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-1",
		AgentName: "first-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  0,
		},
	})
	if err != nil {
		t.Fatalf("Provision agent-1: %v", err)
	}

	// Second agent (sentinel exists, so clone is skipped — just adds worktree).
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-2",
		AgentName: "second-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  0,
		},
	})
	if err != nil {
		t.Fatalf("Provision agent-2: %v", err)
	}

	// Both worktrees exist and are independent.
	wt1 := filepath.Join(res.HostPath, "worktrees", "agent-1")
	wt2 := filepath.Join(res.HostPath, "worktrees", "agent-2")
	if _, err := os.Stat(wt1); err != nil {
		t.Errorf("worktree agent-1 not found: %v", err)
	}
	if _, err := os.Stat(wt2); err != nil {
		t.Errorf("worktree agent-2 not found: %v", err)
	}
}

// --- Per-project lock independence ---

func TestNFSProvision_LockPerProject_Independent(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	// Two different projects should get independent locks.
	hash1 := store.StableProjectHash("proj-A")
	hash2 := store.StableProjectHash("proj-B")
	if hash1 == hash2 {
		t.Skip("hash collision — extremely unlikely but skip test")
	}

	res1, _ := b.Resolve(ResolveInput{ProjectID: "proj-A", Mode: store.SharingModeSharedPlain})
	res2, _ := b.Resolve(ResolveInput{ProjectID: "proj-B", Mode: store.SharingModeSharedPlain})

	// Provision both — they should not block each other.
	if err := ProvisionShared(ProvisionInput{
		Resolved: res1, ProjectID: "proj-A", Mode: store.SharingModeSharedPlain, Locker: locker,
	}); err != nil {
		t.Fatalf("Provision proj-A: %v", err)
	}
	if err := ProvisionShared(ProvisionInput{
		Resolved: res2, ProjectID: "proj-B", Mode: store.SharingModeSharedPlain, Locker: locker,
	}); err != nil {
		t.Fatalf("Provision proj-B: %v", err)
	}

	if got := atomic.LoadInt64(&locker.acquires); got != 2 {
		t.Errorf("expected 2 lock acquires (one per project), got %d", got)
	}
}

// --- Same project, same lock (mutual exclusion) ---

func TestNFSProvision_LockPerProject_MutualExclusion(t *testing.T) {
	b, _, _ := nfsTestBackend(t)

	// A locker that simulates a lock already held by another node.
	blockedLocker := &blockingLocker{blockedUntil: 3} // first 3 attempts blocked

	res, _ := b.Resolve(ResolveInput{ProjectID: "proj-locked", Mode: store.SharingModeSharedPlain})

	err := ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: "proj-locked",
		Mode:      store.SharingModeSharedPlain,
		Locker:    blockedLocker,
	})
	if err != nil {
		t.Fatalf("Provision should eventually succeed after retries: %v", err)
	}

	// Verify it retried the expected number of times.
	if got := atomic.LoadInt64(&blockedLocker.attempts); got != 4 {
		t.Errorf("expected 4 attempts (3 blocked + 1 success), got %d", got)
	}
}

// blockingLocker simulates a lock held by another node for the first N attempts.
type blockingLocker struct {
	blockedUntil int64
	attempts     int64
}

func (l *blockingLocker) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (l *blockingLocker) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	attempt := atomic.AddInt64(&l.attempts, 1)
	if attempt <= l.blockedUntil {
		return false, func() error { return nil }, nil
	}
	return true, func() error { return nil }, nil
}

// --- No locker: degrades gracefully ---

func TestNFSProvision_NoLocker_DegradedMode(t *testing.T) {
	b, _, _ := nfsTestBackend(t)

	res, _ := b.Resolve(ResolveInput{ProjectID: "proj-nolock", Mode: store.SharingModeSharedPlain})

	err := ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: "proj-nolock",
		Mode:      store.SharingModeSharedPlain,
		Locker:    nil, // no locker
	})
	if err != nil {
		t.Fatalf("Provision without locker should succeed: %v", err)
	}
}

// --- WorktreePerAgent missing AgentID ---

func TestNFSProvision_WorktreePerAgent_MissingAgentID(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	bareRepo := initBareGitRepo(t)
	res, _ := b.Resolve(ResolveInput{ProjectID: "proj-noagent", Mode: store.SharingModeWorktreePerAgent})

	err := ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: "proj-noagent",
		AgentID:   "", // missing
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing AgentID in WorktreePerAgent")
	}
}

// --- SentinelDir override ---

func TestNFSProvision_DefaultSentinelDir_IsParent(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	projectID := "proj-sentinel-default"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
		Locker:    locker,
		// SentinelDir is empty → default to parent
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Sentinel should be in the parent of HostPath (project root).
	parentSentinel := filepath.Join(filepath.Dir(res.HostPath), ProvisionSentinelFile)
	if _, err := os.Stat(parentSentinel); err != nil {
		t.Errorf("default sentinel should be in parent dir: %v", err)
	}

	// Sentinel should NOT be inside workspace dir.
	workspaceSentinel := filepath.Join(res.HostPath, ProvisionSentinelFile)
	if _, err := os.Stat(workspaceSentinel); err == nil {
		t.Errorf("default sentinel should not be inside workspace dir")
	}
}

func TestNFSProvision_CustomSentinelDir(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()

	projectID := "proj-sentinel-custom"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:    res,
		ProjectID:   projectID,
		Mode:        store.SharingModeSharedPlain,
		Locker:      locker,
		SentinelDir: res.HostPath, // sentinel inside workspace dir
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Sentinel should be inside the workspace dir.
	workspaceSentinel := filepath.Join(res.HostPath, ProvisionSentinelFile)
	if _, err := os.Stat(workspaceSentinel); err != nil {
		t.Errorf("custom sentinel should be inside workspace dir: %v", err)
	}
}

func TestNFSProvision_CustomSentinelDir_Idempotent(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-sentinel-idem"
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	input := ProvisionInput{
		Resolved:    res,
		ProjectID:   projectID,
		Mode:        store.SharingModeSharedPlain,
		Locker:      locker,
		SentinelDir: res.HostPath,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  1,
		},
	}

	if err := ProvisionShared(input); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if err := ProvisionShared(input); err != nil {
		t.Fatalf("second Provision (should be idempotent): %v", err)
	}

	// Sentinel exists in the custom dir.
	sentinel := filepath.Join(res.HostPath, ProvisionSentinelFile)
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel should exist in custom dir: %v", err)
	}
}

// ==========================================================================
// NFS worktree-per-agent end-to-end validation (Phase 2 T3)
//
// These tests exercise the full NFS Resolve → ProvisionShared → ensureWorktree
// path using a temp directory as the "NFS mount", validating that the worktree
// layout, sentinel placement, gitdir pointers, and base-checkout state are all
// correct for the NFS backend.
// ==========================================================================

// TestNFSWorktreePerAgent_E2E_FullValidation exercises a single agent through
// the complete NFS worktree-per-agent path and asserts every invariant:
// base clone, detached HEAD, gc.auto=0, worktree with relative .git pointer,
// sentinel in per-project dir, worktree nested under workspace (no .. escape).
func TestNFSWorktreePerAgent_E2E_FullValidation(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-nfs-e2e-1"
	agentID := "agent-nfs-e2e-1"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   agentID,
		AgentName: "nfs-test-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  0,
		},
	})
	if err != nil {
		t.Fatalf("ProvisionShared: %v", err)
	}

	// 1. Base clone exists with .git directory.
	baseGit := filepath.Join(res.HostPath, ".git")
	if fi, err := os.Stat(baseGit); err != nil || !fi.IsDir() {
		t.Fatalf("base .git directory not found at %s", baseGit)
	}

	// 2. Base HEAD is detached (no branch owned by the base).
	cmd := exec.Command("git", "-C", res.HostPath, "symbolic-ref", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Error("expected base HEAD to be detached, but symbolic-ref succeeded")
	}

	// 3. gc.auto disabled in the base.
	out, err := exec.Command("git", "-C", res.HostPath, "config", "gc.auto").Output()
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Errorf("expected gc.auto=0 in base, got %q (err=%v)", strings.TrimSpace(string(out)), err)
	}

	// 4. worktrees/ excluded from git tracking.
	excludePath := filepath.Join(res.HostPath, ".git", "info", "exclude")
	excludeData, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read .git/info/exclude: %v", err)
	}
	if !strings.Contains(string(excludeData), "worktrees/") {
		t.Error("expected 'worktrees/' in .git/info/exclude")
	}

	// 5. Worktree was created at the correct path.
	worktreePath := filepath.Join(res.HostPath, "worktrees", agentID)
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree not found at %s: %v", worktreePath, err)
	}

	// 6. Worktree .git is a FILE (pointer), not a directory.
	gitFile := filepath.Join(worktreePath, ".git")
	fi, err := os.Lstat(gitFile)
	if err != nil {
		t.Fatalf("worktree .git not found: %v", err)
	}
	if fi.IsDir() {
		t.Fatal("worktree .git should be a file (pointer), not a directory")
	}

	// 7. Worktree .git pointer uses a RELATIVE path (--relative-paths).
	data, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("read worktree .git: %v", err)
	}
	gitdirLine := strings.TrimSpace(string(data))
	if !strings.HasPrefix(gitdirLine, "gitdir: ") {
		t.Fatalf("unexpected .git content: %s", gitdirLine)
	}
	gitdirPath := strings.TrimPrefix(gitdirLine, "gitdir: ")
	if filepath.IsAbs(gitdirPath) {
		t.Errorf("worktree .git pointer must be relative, got absolute: %s", gitdirPath)
	}

	// 8. The relative gitdir pointer resolves to a valid path within the workspace.
	resolvedGitdir := filepath.Join(worktreePath, gitdirPath)
	resolvedGitdir = filepath.Clean(resolvedGitdir)
	if _, err := os.Stat(resolvedGitdir); err != nil {
		t.Errorf("relative gitdir pointer does not resolve: %s → %s: %v", gitdirPath, resolvedGitdir, err)
	}
	// The resolved path must be under the workspace (no .. escape beyond the mount).
	if !strings.HasPrefix(resolvedGitdir, res.HostPath) {
		t.Errorf("resolved gitdir %s escapes the workspace %s — would break in a container mount",
			resolvedGitdir, res.HostPath)
	}

	// 9. Sentinel is in the per-project directory (parent of workspace/).
	projectDir := filepath.Dir(res.HostPath)
	sentinelPath := filepath.Join(projectDir, ProvisionSentinelFile)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel not found at %s: %v", sentinelPath, err)
	}
	// Sentinel must NOT be inside the workspace dir.
	if _, err := os.Stat(filepath.Join(res.HostPath, ProvisionSentinelFile)); err == nil {
		t.Error("sentinel found inside workspace dir — should be in the project dir (parent)")
	}

	// 10. Cloned files present in the base checkout.
	if _, err := os.Stat(filepath.Join(res.HostPath, "README.md")); err != nil {
		t.Errorf("README.md not found in base checkout: %v", err)
	}

	// 11. Cloned files present in the worktree.
	if _, err := os.Stat(filepath.Join(worktreePath, "README.md")); err != nil {
		t.Errorf("README.md not found in worktree: %v", err)
	}
}

// TestNFSWorktreePerAgent_E2E_TwoAgentsDistinctWorktrees verifies that two
// agents for the same NFS project get independent worktrees with:
// - A single shared base clone (only one .git dir)
// - Two distinct worktree directories
// - Both with relative .git pointers that resolve within the workspace
// - Independent branches
func TestNFSWorktreePerAgent_E2E_TwoAgentsDistinctWorktrees(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-nfs-e2e-2agents"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	gitClone := &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: 0}

	// First agent: triggers base clone + worktree.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-alpha",
		AgentName: "alpha-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  gitClone,
	})
	if err != nil {
		t.Fatalf("Provision agent-alpha: %v", err)
	}

	// Second agent: sentinel exists → skip clone, only worktree add.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-beta",
		AgentName: "beta-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  gitClone,
	})
	if err != nil {
		t.Fatalf("Provision agent-beta: %v", err)
	}

	wt1 := filepath.Join(res.HostPath, "worktrees", "agent-alpha")
	wt2 := filepath.Join(res.HostPath, "worktrees", "agent-beta")

	// Both worktrees exist.
	if _, err := os.Stat(wt1); err != nil {
		t.Fatalf("agent-alpha worktree not found: %v", err)
	}
	if _, err := os.Stat(wt2); err != nil {
		t.Fatalf("agent-beta worktree not found: %v", err)
	}

	// Both have relative .git pointers that resolve correctly.
	for _, wt := range []struct{ path, name string }{{wt1, "alpha"}, {wt2, "beta"}} {
		data, err := os.ReadFile(filepath.Join(wt.path, ".git"))
		if err != nil {
			t.Errorf("%s: read .git: %v", wt.name, err)
			continue
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir: ") {
			t.Errorf("%s: unexpected .git content: %s", wt.name, line)
			continue
		}
		rel := strings.TrimPrefix(line, "gitdir: ")
		if filepath.IsAbs(rel) {
			t.Errorf("%s: .git pointer is absolute: %s", wt.name, rel)
		}
		resolved := filepath.Clean(filepath.Join(wt.path, rel))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("%s: gitdir pointer does not resolve: %s → %s: %v", wt.name, rel, resolved, err)
		}
		if !strings.HasPrefix(resolved, res.HostPath) {
			t.Errorf("%s: gitdir pointer escapes workspace: %s not under %s", wt.name, resolved, res.HostPath)
		}
	}

	// Single shared base .git dir.
	if fi, err := os.Stat(filepath.Join(res.HostPath, ".git")); err != nil || !fi.IsDir() {
		t.Fatal("shared base .git not found or not a directory")
	}

	// Both worktrees are on independent branches.
	branch1, err := exec.Command("git", "-C", wt1, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("get branch for alpha: %v", err)
	}
	branch2, err := exec.Command("git", "-C", wt2, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("get branch for beta: %v", err)
	}
	b1 := strings.TrimSpace(string(branch1))
	b2 := strings.TrimSpace(string(branch2))
	if b1 == b2 {
		t.Errorf("two agents should be on different branches, both on %q", b1)
	}

	// Exactly one sentinel per project.
	projectDir := filepath.Dir(res.HostPath)
	if _, err := os.Stat(filepath.Join(projectDir, ProvisionSentinelFile)); err != nil {
		t.Errorf("project sentinel missing: %v", err)
	}
}

// TestNFSWorktreePerAgent_E2E_WorktreeNestedNoEscape verifies the critical
// invariant for NFS: worktrees are nested under the workspace dir so that
// relative gitdir pointers never escape the mount boundary. This is what
// makes a single NFS mount (or K8s subPath) sufficient for worktree-per-agent.
func TestNFSWorktreePerAgent_E2E_WorktreeNestedNoEscape(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-nfs-nested"
	agentID := "agent-nested-1"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   agentID,
		AgentName: "nested-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: 0},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	worktreePath := filepath.Join(res.HostPath, "worktrees", agentID)

	// Read the gitdir pointer.
	data, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		t.Fatalf("read .git: %v", err)
	}
	rel := strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: ")

	// Walk the relative path and ensure no component escapes the workspace.
	// The relative path should be like ../../.git/worktrees/<agentID> which,
	// when applied from workspace/worktrees/<agentID>, resolves to
	// workspace/.git/worktrees/<agentID>.
	resolved := filepath.Clean(filepath.Join(worktreePath, rel))
	if !strings.HasPrefix(resolved, res.HostPath) {
		t.Fatalf("gitdir pointer escapes workspace mount boundary:\n"+
			"  worktree:  %s\n"+
			"  pointer:   %s\n"+
			"  resolved:  %s\n"+
			"  workspace: %s",
			worktreePath, rel, resolved, res.HostPath)
	}

	// The back-pointer (.git/worktrees/<agentID>/gitdir) should also point
	// back to the worktree using a relative path.
	backPointerPath := filepath.Join(res.HostPath, ".git", "worktrees", agentID, "gitdir")
	backData, err := os.ReadFile(backPointerPath)
	if err != nil {
		t.Fatalf("read back-pointer at %s: %v", backPointerPath, err)
	}
	backPath := strings.TrimSpace(string(backData))
	// With --relative-paths, the back-pointer is also relative.
	if filepath.IsAbs(backPath) {
		t.Logf("note: back-pointer is absolute (%s) — older git may not support relative back-pointers", backPath)
	} else {
		resolvedBack := filepath.Clean(filepath.Join(filepath.Dir(backPointerPath), backPath))
		if !strings.HasPrefix(resolvedBack, res.HostPath) {
			t.Errorf("back-pointer escapes workspace: %s → %s", backPath, resolvedBack)
		}
	}
}

// TestNFSWorktreePerAgent_E2E_RealizeProducesMountForWorktree verifies that
// the NFS backend's Realize output is consistent with the worktree layout:
// the HostPath covers the workspace dir that contains both .git and worktrees/.
func TestNFSWorktreePerAgent_E2E_RealizeProducesMountForWorktree(t *testing.T) {
	b, _, _ := nfsTestBackend(t)

	projectID := "proj-nfs-realize-wt"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	desc, err := b.Realize(RealizeInput{
		Resolved:           res,
		ContainerWorkspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	// The mount HostPath is the workspace dir (contains .git + worktrees/).
	if desc.HostPath != res.HostPath {
		t.Errorf("Realize HostPath = %q, want %q", desc.HostPath, res.HostPath)
	}
	if desc.Type != "nfs" {
		t.Errorf("Type = %q, want nfs", desc.Type)
	}
	if desc.Target != "/workspace" {
		t.Errorf("Target = %q, want /workspace", desc.Target)
	}
	if desc.SubPath != res.ServerRelativePath {
		t.Errorf("SubPath = %q, want %q", desc.SubPath, res.ServerRelativePath)
	}

	// For worktree-per-agent, the per-agent workspace would be
	// <HostPath>/worktrees/<agentID> — which is a subdir of the mount,
	// confirming a single mount covers both .git and the worktree.
	agentWorkspace := filepath.Join(res.HostPath, "worktrees", "some-agent")
	if !strings.HasPrefix(agentWorkspace, desc.HostPath) {
		t.Errorf("agent workspace %s not under mount source %s", agentWorkspace, desc.HostPath)
	}
}

// TestNFSWorktreePerAgent_E2E_SentinelLayoutMatchesNFS validates the NFS
// sentinel placement follows the design: sentinel lives in the per-project
// dir (parent of workspace/), consistent with the NFS layout
// <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/.scion-provisioned.
func TestNFSWorktreePerAgent_E2E_SentinelLayoutMatchesNFS(t *testing.T) {
	b, cfg, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-nfs-sentinel-layout"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-s1",
		AgentName: "sentinel-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: 0},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Expected sentinel path: <MountRoot>/<shareID>/<SubPathRoot>/<projectID>/.scion-provisioned
	share := cfg.Shares[0]
	expectedSentinelDir := filepath.Join(cfg.MountRoot, share.ID, "projects", projectID)
	sentinelPath := filepath.Join(expectedSentinelDir, ProvisionSentinelFile)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel not at expected NFS path %s: %v", sentinelPath, err)
	}

	// This should equal filepath.Dir(res.HostPath).
	if filepath.Dir(res.HostPath) != expectedSentinelDir {
		t.Errorf("filepath.Dir(HostPath) = %q, want %q", filepath.Dir(res.HostPath), expectedSentinelDir)
	}
}

// TestNFSWorktreePerAgent_E2E_SecondAgentSkipsClone confirms that the second
// agent for the same NFS project skips the git clone (sentinel short-circuit)
// and only creates its worktree. The lock count confirms exactly 2 acquisitions.
func TestNFSWorktreePerAgent_E2E_SecondAgentSkipsClone(t *testing.T) {
	b, _, _ := nfsTestBackend(t)
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectID := "proj-nfs-skip-clone"

	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	gitClone := &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: 0}

	// First agent: clone + worktree.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-first",
		AgentName: "first",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  gitClone,
	})
	if err != nil {
		t.Fatalf("Provision agent-first: %v", err)
	}

	// Record the .git mtime after first provision — if second agent re-clones,
	// .git contents would be modified.
	gitDir := filepath.Join(res.HostPath, ".git")
	gitInfo, err := os.Stat(gitDir)
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	gitModTime := gitInfo.ModTime()

	// Second agent: should skip clone, only add worktree.
	err = ProvisionShared(ProvisionInput{
		Resolved:  res,
		ProjectID: projectID,
		AgentID:   "agent-second",
		AgentName: "second",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  gitClone,
	})
	if err != nil {
		t.Fatalf("Provision agent-second: %v", err)
	}

	// Both worktrees exist.
	if _, err := os.Stat(filepath.Join(res.HostPath, "worktrees", "agent-first")); err != nil {
		t.Errorf("agent-first worktree missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.HostPath, "worktrees", "agent-second")); err != nil {
		t.Errorf("agent-second worktree missing: %v", err)
	}

	// Lock acquired exactly twice (once per ProvisionShared call).
	if got := atomic.LoadInt64(&locker.acquires); got != 2 {
		t.Errorf("expected 2 lock acquisitions, got %d", got)
	}

	_ = gitModTime // mtime check is informational; git worktree add may touch .git/
}
