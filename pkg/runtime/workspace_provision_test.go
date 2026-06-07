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

// resolveForTest resolves workspace paths for a test project.
func resolveForTest(t *testing.T, b WorkspaceBackend, projectID string) ResolvedWorkspace {
	t.Helper()
	res, err := b.Resolve(ResolveInput{
		ProjectID: projectID,
		Mode:      store.SharingModeSharedPlain,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return res
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
