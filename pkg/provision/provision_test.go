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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	single  bool
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

// initBareGitRepo creates a bare git repo at a temporary path for cloning from.
func initBareGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bareDir := filepath.Join(dir, "bare.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bareDir)

	workDir := filepath.Join(dir, "work")
	run(t, "git", "clone", bareDir, workDir)

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

// --- ClonePerAgent rejection ---

func TestProvision_RejectsClonePerAgent(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		ProjectID: "proj-1",
		Mode:      store.SharingModeClonePerAgent,
		Resolved: ResolvedWorkspace{
			HostPath: "/some/path",
		},
	})
	if err == nil {
		t.Fatal("expected error for ClonePerAgent on NFS backend")
	}
	if !strings.Contains(err.Error(), "ClonePerAgent") {
		t.Errorf("error should mention ClonePerAgent, got: %v", err)
	}
}

// --- Missing required fields ---

func TestProvision_MissingHostPath(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		ProjectID: "proj-1",
		Mode:      store.SharingModeSharedPlain,
		Resolved:  ResolvedWorkspace{},
	})
	if err == nil {
		t.Fatal("expected error for empty HostPath")
	}
}

func TestProvision_MissingProjectID(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		Mode: store.SharingModeSharedPlain,
		Resolved: ResolvedWorkspace{
			HostPath: "/some/path",
		},
	})
	if err == nil {
		t.Fatal("expected error for empty ProjectID")
	}
}

// --- sanitizeBranchName ---

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with spaces", "with-spaces"},
		{"with/slash", "with-slash"},
		{"with..dots", "with-dots"},
		{"with~tilde", "with-tilde"},
		{".leading-dot", "leading-dot"},
		{"-leading-dash", "leading-dash"},
		{"trailing-.", "trailing"},
		{"", "agent"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeBranchName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestChownTarget(t *testing.T) {
	tests := []struct {
		name     string
		hostPath string
		want     string
	}{
		// Broker-side: chown the project root (parent of the workspace dir).
		{"broker project root", "/srv/nfs/share1/proj-abc/workspace", "/srv/nfs/share1/proj-abc"},
		// k8s init container subPath mount: parent is "/", fall back to the
		// workspace dir itself rather than chown -R the whole container root.
		{"k8s workspace mount", "/workspace", "/workspace"},
		// Relative path has no real parent ("."); fall back to the path itself.
		{"relative path", "workspace", "workspace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chownTarget(tt.hostPath); got != tt.want {
				t.Errorf("chownTarget(%q) = %q, want %q", tt.hostPath, got, tt.want)
			}
		})
	}
}

// --- writeSentinel ---

func TestWriteSentinel_Atomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProvisionSentinelFile)

	if err := writeSentinel(path); err != nil {
		t.Fatalf("writeSentinel: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(data), "provisioned_at=") {
		t.Errorf("sentinel content unexpected: %s", string(data))
	}

	// Overwrite should also work (idempotent).
	if err := writeSentinel(path); err != nil {
		t.Fatalf("writeSentinel overwrite: %v", err)
	}
}

// --- acquireProvisionLock context cancellation ---

// alwaysLoseLocker is an AdvisoryLocker where TryAdvisoryLockObject always
// returns acquired=false (another node holds the lock).
type alwaysLoseLocker struct{}

func (l *alwaysLoseLocker) TryAdvisoryLock(_ context.Context, _ store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (l *alwaysLoseLocker) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func TestAcquireProvisionLock_ContextCancellation(t *testing.T) {
	locker := &alwaysLoseLocker{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := ProvisionInput{
		ProjectID: "proj-cancel-test",
		Locker:    locker,
	}

	start := time.Now()
	_, err := acquireProvisionLock(ctx, in)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context cancelled")
	assert.Less(t, elapsed, 2*time.Second, "should return promptly on context cancellation, not wait for all retries")
}

// --- WorktreePerAgent: creates worktree on shared checkout ---

func TestProvision_WorktreePerAgent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-wt-1",
		AgentID:   "agent-wt-1",
		AgentName: "test-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  0,
		},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Verify base HEAD is detached.
	cmd := exec.Command("git", "-C", hostPath, "symbolic-ref", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Error("expected HEAD to be detached in base, but symbolic-ref succeeded")
	}

	// Verify gc.auto is disabled.
	out, err := exec.Command("git", "-C", hostPath, "config", "gc.auto").Output()
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Errorf("expected gc.auto=0 in base repo, got %q (err=%v)", strings.TrimSpace(string(out)), err)
	}

	// Verify worktree was created.
	worktreePath := WorktreePath(hostPath, "agent-wt-1")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree not created at %s: %v", worktreePath, err)
	}

	// Verify .git is a file (pointer), not a directory.
	gitFile := filepath.Join(worktreePath, ".git")
	fi, err := os.Lstat(gitFile)
	if err != nil {
		t.Fatalf("worktree .git not found: %v", err)
	}
	if fi.IsDir() {
		t.Error("worktree .git should be a file (pointer), not a directory")
	}

	// Verify .git pointer uses a relative path (--relative-paths).
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
		t.Errorf("worktree .git should use a relative path, got: %s", gitdirPath)
	}
}

// --- WorktreePerAgent: second agent gets independent worktree ---

func TestProvision_WorktreePerAgent_TwoAgents(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// First agent.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-wt-2",
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
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-wt-2",
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
	wt1 := WorktreePath(hostPath, "agent-1")
	wt2 := WorktreePath(hostPath, "agent-2")
	if _, err := os.Stat(wt1); err != nil {
		t.Errorf("worktree agent-1 not found: %v", err)
	}
	if _, err := os.Stat(wt2); err != nil {
		t.Errorf("worktree agent-2 not found: %v", err)
	}

	// Verify both worktrees have relative .git pointers.
	for _, wt := range []string{wt1, wt2} {
		data, err := os.ReadFile(filepath.Join(wt, ".git"))
		if err != nil {
			t.Errorf("read .git in %s: %v", wt, err)
			continue
		}
		gitdirLine := strings.TrimSpace(string(data))
		if !strings.HasPrefix(gitdirLine, "gitdir: ") {
			t.Errorf("unexpected .git content in %s: %s", wt, gitdirLine)
			continue
		}
		gitdirPath := strings.TrimPrefix(gitdirLine, "gitdir: ")
		if filepath.IsAbs(gitdirPath) {
			t.Errorf("worktree %s .git should use relative path, got: %s", wt, gitdirPath)
		}
	}
}

// --- Two projects sharing a parent dir: independent sentinels ---

func TestProvision_WorktreePerAgent_TwoProjects(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepoA := initBareGitRepo(t)
	bareRepoB := initBareGitRepo(t)

	parentDir := t.TempDir()
	projectDirA := filepath.Join(parentDir, "project-alpha")
	projectDirB := filepath.Join(parentDir, "project-beta")
	if err := os.MkdirAll(projectDirA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDirB, 0755); err != nil {
		t.Fatal(err)
	}

	hostPathA := filepath.Join(projectDirA, "workspace")
	hostPathB := filepath.Join(projectDirB, "workspace")

	// --- Project A ---
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPathA, Backend: "local"},
		ProjectID: "proj-alpha",
		AgentID:   "agent-a1",
		AgentName: "alpha-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepoA, Branch: "main", Depth: 0},
	})
	if err != nil {
		t.Fatalf("Provision project A: %v", err)
	}

	if _, err := os.Stat(filepath.Join(hostPathA, ".git")); err != nil {
		t.Fatalf("project A: .git not found: %v", err)
	}
	if _, err := os.Stat(WorktreePath(hostPathA, "agent-a1")); err != nil {
		t.Fatalf("project A: worktree not created: %v", err)
	}

	// --- Project B ---
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPathB, Backend: "local"},
		ProjectID: "proj-beta",
		AgentID:   "agent-b1",
		AgentName: "beta-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepoB, Branch: "main", Depth: 0},
	})
	if err != nil {
		t.Fatalf("Provision project B: %v", err)
	}

	if _, err := os.Stat(filepath.Join(hostPathB, ".git")); err != nil {
		t.Fatalf("project B: .git not found — sentinel collision?")
	}
	if _, err := os.Stat(WorktreePath(hostPathB, "agent-b1")); err != nil {
		t.Fatalf("project B: worktree not created: %v", err)
	}

	// Sentinels must be per-project.
	sentinelA := filepath.Join(projectDirA, ProvisionSentinelFile)
	sentinelB := filepath.Join(projectDirB, ProvisionSentinelFile)
	if _, err := os.Stat(sentinelA); err != nil {
		t.Errorf("project A sentinel missing at %s", sentinelA)
	}
	if _, err := os.Stat(sentinelB); err != nil {
		t.Errorf("project B sentinel missing at %s", sentinelB)
	}
	parentSentinel := filepath.Join(parentDir, ProvisionSentinelFile)
	if _, err := os.Stat(parentSentinel); err == nil {
		t.Errorf("sentinel found in shared parent dir %s — sentinel collision", parentDir)
	}
}

// --- Concurrent same-project provisioning ---

func TestProvision_WorktreePerAgent_ConcurrentSameProject(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	var wg sync.WaitGroup
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-concurrent-%d", idx)
			errs[idx] = ProvisionShared(ProvisionInput{
				Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
				ProjectID: "proj-concurrent-1",
				AgentID:   agentID,
				AgentName: fmt.Sprintf("concurrent-agent-%d", idx),
				Mode:      store.SharingModeWorktreePerAgent,
				Locker:    locker,
				GitClone: &api.GitCloneConfig{
					URL:    bareRepo,
					Branch: "main",
					Depth:  0,
				},
			})
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}

	for i := 0; i < 2; i++ {
		wt := WorktreePath(hostPath, fmt.Sprintf("agent-concurrent-%d", i))
		if _, err := os.Stat(wt); err != nil {
			t.Errorf("worktree agent-concurrent-%d not found at %s: %v", i, wt, err)
		}
	}

	if _, err := os.Stat(filepath.Join(hostPath, ".git")); err != nil {
		t.Fatalf("shared base .git not found: %v", err)
	}
}

// --- Full clone depth for worktree mode ---

func TestProvision_WorktreePerAgent_FullCloneDepth(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Depth -1 means full clone (no --depth flag).
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-depth-1",
		AgentID:   "agent-depth-1",
		AgentName: "depth-agent",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone: &api.GitCloneConfig{
			URL:    bareRepo,
			Branch: "main",
			Depth:  -1,
		},
	})
	if err != nil {
		t.Fatalf("Provision with Depth=-1: %v", err)
	}

	// Verify the clone is NOT shallow (full history).
	shallowFile := filepath.Join(hostPath, ".git", "shallow")
	if _, err := os.Stat(shallowFile); err == nil {
		t.Error("expected full clone (no .git/shallow), but shallow file exists")
	}
}

// --- WorktreePath ---

func TestWorktreePath(t *testing.T) {
	got := WorktreePath("/srv/nfs/proj/workspace", "agent-42")
	want := "/srv/nfs/proj/workspace/worktrees/agent-42"
	if got != want {
		t.Errorf("WorktreePath() = %q, want %q", got, want)
	}
}
