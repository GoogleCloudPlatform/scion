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
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// --- F-111 (design §9): chown fatal-vs-tolerant, self-healing, shared dirs ---

// TestProvisionShared_SelfHeals_ExistingRootOwnedDirectory is the reasoning
// note tf-lead's addendum asked for, turned into an executable test: an
// existing project directory with NO sentinel — exactly project be2d6fe3's
// state, per vm-deploy's live evidence — must be repaired by the next
// provisioning attempt with no separate manual step. os.MkdirAll on an
// already-existing directory is a documented no-op (it does not chmod an
// existing dir to the requested mode), so the self-healing guarantee rests
// entirely on chownProjectTree running unconditionally whenever no sentinel
// is found, regardless of whether the directory pre-existed. This test
// proves exactly that: a file placed in the directory *before* calling
// ProvisionShared is still there afterwards (MkdirAll didn't touch/recreate
// it), and provisioning still completes and writes the sentinel (chown ran
// on the pre-existing tree, not just a freshly-created one).
//
// This test cannot chown to a UID other than its own process (chown-to-
// another-uid requires CAP_CHOWN, which this sandbox does not have — see
// TestProvisionShared_RequireChownSuccess_FailsOnChownError below, which
// uses exactly that restriction to force a *real* chown failure). Using
// NFSUID/GID equal to the test process's own uid/gid still exercises the
// real code path (MkdirAll-is-no-op-on-existing-dir, then chown -R runs
// unconditionally) — it just can't observe an ownership *change*, only that
// the flow completes without shortcutting via mkdir.
func TestProvisionShared_SelfHeals_ExistingRootOwnedDirectory(t *testing.T) {
	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Simulate kubelet having already auto-created the directory (no
	// sentinel was ever written for it — the exact be2d6fe3 state).
	if err := os.MkdirAll(hostPath, 0755); err != nil {
		t.Fatalf("pre-create workspace dir: %v", err)
	}
	marker := filepath.Join(hostPath, "pre-existing-file")
	if err := os.WriteFile(marker, []byte("pre-existing"), 0644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	err := ProvisionShared(ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: hostPath},
		ProjectID:           "proj-selfheal",
		Mode:                store.SharingModeSharedPlain,
		NFSUID:              os.Getuid(),
		NFSGID:              os.Getgid(),
		SentinelDir:         hostPath,
		RequireChownSuccess: true,
	})
	if err != nil {
		t.Fatalf("ProvisionShared on a pre-existing, sentinel-less directory: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("pre-existing file was lost: %v (MkdirAll should be a no-op on an existing dir)", err)
	}
	if _, err := os.Stat(filepath.Join(hostPath, ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not written after successful provisioning: %v", err)
	}
}

// TestProvisionShared_RequireChownSuccess_FailsOnChownError proves the
// "chown failure must become fatal to the init container" requirement using
// a *real* chown failure, not a mock: this sandbox process has no CAP_CHOWN,
// so chowning to any UID other than its own genuinely fails with "operation
// not permitted" (verified once, directly, before writing this test). With
// RequireChownSuccess, ProvisionShared must return an error and must NOT
// write the sentinel — if it did, a lock-loser pod polling for the sentinel
// would see it and proceed believing the workspace was properly provisioned.
func TestProvisionShared_RequireChownSuccess_FailsOnChownError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown-to-other-uid would succeed, defeating this test's premise")
	}

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	err := ProvisionShared(ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: hostPath},
		ProjectID:           "proj-chownfail",
		Mode:                store.SharingModeSharedPlain,
		NFSUID:              os.Getuid() + 1, // guaranteed not our own uid
		NFSGID:              os.Getgid(),
		SentinelDir:         hostPath,
		RequireChownSuccess: true,
	})
	if err == nil {
		t.Fatal("expected ProvisionShared to fail when chown fails and RequireChownSuccess is true")
	}
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("error should mention chown, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(hostPath, ProvisionSentinelFile)); statErr == nil {
		t.Error("sentinel must NOT be written when a required chown fails")
	}
}

// TestProvisionShared_ChownFailure_NonFatal_ByDefault confirms the broker's
// own worktree-per-agent flow keeps its existing tolerant behavior — this is
// the negative control for the test above: same forced chown failure,
// RequireChownSuccess left at its zero value (false), and provisioning must
// still succeed (an operator may have pre-chowned).
func TestProvisionShared_ChownFailure_NonFatal_ByDefault(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown-to-other-uid would succeed, defeating this test's premise")
	}

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	err := ProvisionShared(ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath},
		ProjectID:   "proj-chownfail-tolerant",
		Mode:        store.SharingModeSharedPlain,
		NFSUID:      os.Getuid() + 1,
		NFSGID:      os.Getgid(),
		SentinelDir: hostPath,
		// RequireChownSuccess not set — defaults to false.
	})
	if err != nil {
		t.Fatalf("ProvisionShared should tolerate a chown failure by default, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(hostPath, ProvisionSentinelFile)); statErr != nil {
		t.Errorf("sentinel should still be written when chown is non-fatal: %v", statErr)
	}
}

// TestProvisionShared_ChownsSharedDirsIndependently proves the F-111 gap
// tf-lead's addendum flagged: a shared dir on its own, separate mount (a
// different tree entirely from the workspace dir, standing in for a distinct
// k8s subPath volume mount) is not reachable by chownRoot's recursion —
// mkdir+chown must be applied to it explicitly, not assumed to be covered by
// the workspace's own chown.
func TestProvisionShared_ChownsSharedDirsIndependently(t *testing.T) {
	workspaceRoot := t.TempDir()
	hostPath := filepath.Join(workspaceRoot, "workspace")

	// A completely separate directory tree — NOT a subdirectory of hostPath
	// or its parent, standing in for a shared dir mounted at its own subPath
	// (e.g. /scion-volumes/scratchpad, not nested under /workspace).
	sharedRoot := t.TempDir()
	sharedPath := filepath.Join(sharedRoot, "scratchpad")

	err := ProvisionShared(ProvisionInput{
		Resolved: ResolvedWorkspace{
			HostPath: hostPath,
			SharedDirs: map[string]ResolvedSharedDir{
				"scratchpad": {HostPath: sharedPath},
			},
		},
		ProjectID:           "proj-shareddirs",
		Mode:                store.SharingModeSharedPlain,
		NFSUID:              os.Getuid(),
		NFSGID:              os.Getgid(),
		SentinelDir:         hostPath,
		RequireChownSuccess: true,
	})
	if err != nil {
		t.Fatalf("ProvisionShared: %v", err)
	}

	if _, statErr := os.Stat(sharedPath); statErr != nil {
		t.Errorf("shared dir was not created: %v", statErr)
	}
	// RequireChownSuccess: true would have failed the whole call (asserted
	// above) if chowning sharedPath had errored — success here is the
	// positive proof that the shared-dir chown loop actually ran and
	// reported success, not just that mkdir happened to work.
}

// TestChownProjectTree_SymlinkOutsideTree_TargetOwnershipUnchanged is the
// F-111 review fix (tf-lead): chownProjectTree now runs `chown -R -h`, not
// plain `-R` — as root with CAP_DAC_OVERRIDE (the k8s init container's
// winner security context), a symlink inside a cloned/possibly-untrusted
// tree pointing OUTSIDE it (elsewhere in the init container's filesystem
// view, or another mounted shared dir) must never have its REFERENT
// re-owned, only the link itself.
//
// This sandbox has no CAP_CHOWN (verified directly, matching the pattern in
// cloudrun_sandbox_runtime_test.go's TestPrepareScionLayout_ChownsDirectories),
// so this cannot chown to a different uid at all, let alone reproduce the
// real k8s init container (root, CAP_DAC_OVERRIDE, a target uid that
// actually differs from the caller's). This test is a same-uid chown, and
// checks that ctime — which chown/lchown update unconditionally, even when
// the new owner equals the old one — is untouched on the outside target
// (verified empirically before writing this test: a `chown -R -h` to the
// process's own uid:gid left the outside target's ctime byte-for-byte
// identical down to the nanosecond). That is a real regression guard on
// -h's dereference behavior specifically, and the best this environment can
// exercise — it is NOT proof that the real root/CAP_DAC_OVERRIDE scenario
// is safe, only that -h itself does what it's documented to do here.
func TestChownProjectTree_SymlinkOutsideTree_TargetOwnershipUnchanged(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("ctime/uid check uses syscall.Stat_t (Linux only)")
	}

	// A tree we're about to chown -R -h...
	treeRoot := t.TempDir()
	// ...containing a symlink to a file in a completely separate directory
	// (standing in for "elsewhere in the container's filesystem view" or
	// "another mounted shared dir" — not a subdirectory of treeRoot at all).
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside-target.txt")
	if err := os.WriteFile(outsideTarget, []byte("do not touch"), 0644); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	linkPath := filepath.Join(treeRoot, "link-to-outside")
	if err := os.Symlink(outsideTarget, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	before, err := os.Lstat(outsideTarget)
	if err != nil {
		t.Fatalf("lstat outside target before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	if err := chownProjectTree(context.Background(), treeRoot, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownProjectTree: %v", err)
	}

	after, err := os.Lstat(outsideTarget)
	if err != nil {
		t.Fatalf("lstat outside target after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)

	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("outside target ownership changed: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
	beforeCtime := beforeStat.Ctim
	afterCtime := afterStat.Ctim
	if beforeCtime != afterCtime {
		t.Errorf("outside target ctime changed (target was touched, meaning the symlink was dereferenced): before %+v, after %+v",
			beforeCtime, afterCtime)
	}

	// Positive control: the symlink itself (not the target) must actually
	// have been (l)chowned — proves chown -R -h did something, not that it
	// silently no-op'd on everything including the link.
	linkInfo, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	linkStat := linkInfo.Sys().(*syscall.Stat_t)
	if int(linkStat.Uid) != os.Getuid() || int(linkStat.Gid) != os.Getgid() {
		t.Errorf("link itself not chowned: uid=%d gid=%d, want %d:%d",
			linkStat.Uid, linkStat.Gid, os.Getuid(), os.Getgid())
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
		GitClone:  &api.GitCloneConfig{URL: bareRepoA, Branch: "main"},
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
		GitClone:  &api.GitCloneConfig{URL: bareRepoB, Branch: "main"},
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

	// Depth 0 means full clone (no --depth flag).
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
			Depth:  intPtr(0),
		},
	})
	if err != nil {
		t.Fatalf("Provision with Depth=0 (full clone): %v", err)
	}

	// Verify the clone is NOT shallow (full history).
	shallowFile := filepath.Join(hostPath, ".git", "shallow")
	if _, err := os.Stat(shallowFile); err == nil {
		t.Error("expected full clone (no .git/shallow), but shallow file exists")
	}
}

// TestProvision_SelfHealRefusesWhenSiblingWorktreePresent proves the
// self-heal path in gitCloneWorkspace (reached when the provisioning
// sentinel and the base's own .git are both missing) refuses to clear the
// shared base when a "worktrees" subdirectory already holds another
// agent's checkout, instead of silently wiping it.
func TestProvision_SelfHealRefusesWhenSiblingWorktreePresent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Provision agent-1 normally: creates the shared base plus agent-1's worktree.
	if err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-selfheal-1",
		AgentID:   "agent-1",
		AgentName: "agent-1",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}); err != nil {
		t.Fatalf("initial provision: %v", err)
	}

	agent1Worktree := WorktreePath(hostPath, "agent-1")
	sentinelFile := filepath.Join(agent1Worktree, "sentinel.go")
	if err := os.WriteFile(sentinelFile, []byte("package main // must survive\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate corruption: both the provisioning sentinel and the base's
	// own .git go missing, but agent-1's worktree under worktrees/ is
	// still there.
	if err := os.Remove(filepath.Join(projectDir, ProvisionSentinelFile)); err != nil {
		t.Fatalf("remove sentinel: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(hostPath, ".git")); err != nil {
		t.Fatalf("remove base .git: %v", err)
	}

	// Provision agent-2 against the same shared base: ProvisionShared sees
	// the sentinel is gone, tries to clone into hostPath, finds it
	// non-empty (worktrees/agent-1 is there) with no .git, and must refuse
	// to self-heal by clearing it rather than wiping agent-1's worktree.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-selfheal-1",
		AgentID:   "agent-2",
		AgentName: "agent-2",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected ProvisionShared to fail rather than clear a base with a sibling worktree present")
	}

	if _, statErr := os.Stat(agent1Worktree); statErr != nil {
		t.Errorf("agent-1's worktree must survive, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(sentinelFile)
	if readErr != nil {
		t.Fatalf("agent-1's file must survive, but reading it failed: %v", readErr)
	}
	if string(got) != "package main // must survive\n" {
		t.Errorf("sentinel file content changed: %q", got)
	}
}

// TestProvision_SelfHealRefusesWhenWorktreesCheckErrors proves the self-heal
// guard fails closed, rather than silently proceeding, when it cannot even
// determine whether "worktrees" holds anything — for example because
// "worktrees" itself is a regular file rather than a directory, which makes
// the check's own directory read return an error rather than "not found".
func TestProvision_SelfHealRefusesWhenWorktreesCheckErrors(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// A non-empty, un-provisioned workspace dir (no sentinel, no .git): the
	// clone attempt inside ProvisionShared fails with "not an empty
	// directory", reaching the self-heal check.
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatal(err)
	}
	preExistingFile := filepath.Join(hostPath, "pre-existing.txt")
	if err := os.WriteFile(preExistingFile, []byte("must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "worktrees" is a plain file, not a directory: the self-heal check's
	// own os.ReadDir on it fails with an error other than "not exist".
	worktreesAsFile := filepath.Join(hostPath, "worktrees")
	if err := os.WriteFile(worktreesAsFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-selfheal-2",
		AgentID:   "agent-1",
		AgentName: "agent-1",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected ProvisionShared to fail rather than clear a base whose worktrees entry cannot be checked")
	}

	got, readErr := os.ReadFile(preExistingFile)
	if readErr != nil {
		t.Fatalf("pre-existing file must survive, but reading it failed: %v", readErr)
	}
	if string(got) != "must survive" {
		t.Errorf("pre-existing file content changed: %q", got)
	}
	if fi, statErr := os.Stat(worktreesAsFile); statErr != nil || fi.IsDir() {
		t.Errorf("expected worktreesAsFile to survive unchanged as a plain file, stat: %+v, err: %v", fi, statErr)
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

// --- IsRealWorktreeDir ---

func TestIsRealWorktreeDir(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	base := filepath.Join(projectDir, "workspace")
	if err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: base, Backend: "local"},
		ProjectID: "proj-real-wt-1",
		AgentID:   "agent-1",
		AgentName: "agent-1",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}); err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	realWorktree := WorktreePath(base, "agent-1")

	t.Run("a real worktree", func(t *testing.T) {
		if !IsRealWorktreeDir(realWorktree, base) {
			t.Error("expected the freshly created worktree to be recognized as real")
		}
	})

	t.Run("a plain file", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "not-a-dir")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a plain file to be rejected")
		}
	})

	t.Run("a symlink to a directory", func(t *testing.T) {
		target := t.TempDir()
		p := filepath.Join(base, "worktrees", "a-symlink")
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a symlink to be rejected even when it points at a directory")
		}
	})

	t.Run("a symlink to another agent's real worktree", func(t *testing.T) {
		// Unlike the previous case, the symlink target here is itself a real
		// worktree of this same base — everything the shape check inspects
		// past the symlink (a .git file resolving into this base's own admin
		// directory) would pass. Only checking the symlink itself with Lstat,
		// rather than following it with Stat, rejects this.
		p := filepath.Join(base, "worktrees", "a-symlink-to-real-worktree")
		if err := os.Symlink(realWorktree, p); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a symlink to another agent's real worktree to be rejected")
		}
	})

	t.Run("a directory with no .git at all", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "no-git")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a directory with no .git to be rejected")
		}
	})

	t.Run("a directory whose .git is itself a directory", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "git-is-a-dir")
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a directory whose .git is a directory (a full clone, not a worktree) to be rejected")
		}
	})

	t.Run("a .git file pointing outside this base's admin directory", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "foreign-gitfile")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		outsideAdminDir := filepath.Join(t.TempDir(), ".git", "worktrees", "elsewhere")
		if err := os.MkdirAll(outsideAdminDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".git"), []byte("gitdir: "+outsideAdminDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a .git file pointing outside this base's admin directory to be rejected")
		}
	})

	t.Run("a .git file naming the admin directory itself", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "gitdir-is-admin-root")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		adminDir := filepath.Join(base, ".git", "worktrees")
		if err := os.WriteFile(filepath.Join(p, ".git"), []byte("gitdir: "+adminDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a .git file naming the admin directory itself (rel \".\") to be rejected")
		}
	})

	t.Run("a .git file naming a nested admin path", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "gitdir-is-nested")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(base, ".git", "worktrees", "agent-1", "extra")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".git"), []byte("gitdir: "+nested+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a .git file naming a path nested more than one element under the admin directory to be rejected")
		}
	})

	t.Run("a .git file naming the base's own .git directory", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "gitdir-is-dotgit")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		dotGit := filepath.Join(base, ".git")
		if err := os.WriteFile(filepath.Join(p, ".git"), []byte("gitdir: "+dotGit+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if IsRealWorktreeDir(p, base) {
			t.Error("expected a .git file naming the base's own .git directory (rel \"..\") to be rejected")
		}
	})
}

// --- Create-or-Attach + Sharer Registration ---

func TestProvision_WorktreePerAgent_CreateAndJoin(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Agent A creates worktree on branch "shared-branch".
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-join-1",
		AgentID:   "agent-a",
		AgentName: "shared-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Verify worktree created for A.
	wtA := WorktreePath(hostPath, "agent-a")
	require.DirExists(t, wtA)

	// Verify sharers=[A].
	sharers, wtPath, err := ListSharers(hostPath, "shared-branch")
	require.NoError(t, err)
	assert.Equal(t, wtA, wtPath)
	assert.Equal(t, []string{"agent-a"}, sharers)

	// Agent B joins same branch "shared-branch" (JOIN).
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-join-1",
		AgentID:   "agent-b",
		AgentName: "shared-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Verify NO second worktree created for B.
	wtB := WorktreePath(hostPath, "agent-b")
	_, statErr := os.Stat(wtB)
	assert.True(t, os.IsNotExist(statErr), "JOIN should NOT create a second worktree at %s", wtB)

	// Verify sharers=[A,B] and B's registered path == A's path.
	sharers, wtPath, err = ListSharers(hostPath, "shared-branch")
	require.NoError(t, err)
	assert.Equal(t, wtA, wtPath, "B's resolved worktree path should equal A's")
	assert.Len(t, sharers, 2)
	assert.Contains(t, sharers, "agent-a")
	assert.Contains(t, sharers, "agent-b")
}

// TestProvision_EnsureWorktree_OwnPathNotRealWorktree_Refused proves
// ProvisionShared refuses to reuse or remove an agent's own worktree target
// when it exists but is not a real worktree of this checkout — directly at
// the provision package level, not through the broker's own later mount-time
// check, so this refusal is pinned as its own, independent guarantee.
func TestProvision_EnsureWorktree_OwnPathNotRealWorktree_Refused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// First provision establishes the shared base and agent-a's own worktree.
	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-ownpath-refused",
		AgentID:   "agent-a",
		AgentName: "agent-a",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	}))

	// Occupy agent-b's own worktree target with a plain file before agent-b
	// is ever provisioned.
	wtB := WorktreePath(hostPath, "agent-b")
	require.NoError(t, os.WriteFile(wtB, []byte("not a worktree"), 0o644))

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-ownpath-refused",
		AgentID:   "agent-b",
		AgentName: "agent-b",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "expected ProvisionShared to refuse a non-worktree occupant of agent-b's own path")

	// The occupying file must survive untouched.
	got, readErr := os.ReadFile(wtB)
	require.NoError(t, readErr)
	assert.Equal(t, "not a worktree", string(got))

	// No sharer marker was ever written for agent-b's branch.
	sharers, _, listErr := ListSharers(hostPath, "agent-b")
	require.NoError(t, listErr)
	assert.Empty(t, sharers, "expected no sharer marker written on refusal")
}

// TestProvision_EnsureWorktree_RegistryNamesNonWorktree_Refused proves
// ProvisionShared refuses to join a sharer-registry entry that does not name
// a real, direct-child worktree of this checkout — directly at the provision
// package level. The original sharer's own registration must survive
// unchanged, and no new agent must be added to it.
func TestProvision_EnsureWorktree_RegistryNamesNonWorktree_Refused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-refused",
		AgentID:   "agent-a",
		AgentName: "agent-a",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	}))

	// Plant a sharer-registry marker for a different branch, naming a
	// subdirectory of agent-a's own real worktree — not a direct child of
	// the shared base's own "worktrees" directory — with a gitfile that
	// otherwise looks like a valid worktree of this checkout.
	nested := filepath.Join(WorktreePath(hostPath, "agent-a"), "sub")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	adminEntry := filepath.Join(hostPath, ".git", "worktrees", "fake-entry")
	require.NoError(t, os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644))
	require.NoError(t, RegisterSharer(hostPath, "other-branch", nested, "agent-a"))

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-refused",
		AgentID:   "agent-c",
		AgentName: "other-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "expected ProvisionShared to refuse joining a registry entry naming a non-direct-child path")

	// agent-c must never have been added as a sharer.
	sharers, wtPath, listErr := ListSharers(hostPath, "other-branch")
	require.NoError(t, listErr)
	assert.Equal(t, nested, wtPath, "the original (bogus) registration must survive unchanged")
	assert.Equal(t, []string{"agent-a"}, sharers, "expected no new agent added to the registry on refusal")
}

// TestProvision_EnsureWorktree_RegistryNamesDirectChildNonWorktree_Refused
// pins the registry refusal's IsRealWorktreeDir half specifically: the
// marker here names a direct child of "worktrees" (so the direct-child check
// alone would accept it), but that child has no .git at all, so it is not a
// real worktree of this checkout.
func TestProvision_EnsureWorktree_RegistryNamesDirectChildNonWorktree_Refused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-refused-2",
		AgentID:   "agent-a",
		AgentName: "agent-a",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	}))

	plainDir := filepath.Join(hostPath, "worktrees", "plain-dir")
	require.NoError(t, os.MkdirAll(plainDir, 0o755))
	require.NoError(t, RegisterSharer(hostPath, "other-branch-2", plainDir, "agent-a"))

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-refused-2",
		AgentID:   "agent-c",
		AgentName: "other-branch-2",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "expected ProvisionShared to refuse joining a direct-child registry entry with no .git")

	sharers, wtPath, listErr := ListSharers(hostPath, "other-branch-2")
	require.NoError(t, listErr)
	assert.Equal(t, plainDir, wtPath, "the original (bogus) registration must survive unchanged")
	assert.Equal(t, []string{"agent-a"}, sharers, "expected no new agent added to the registry on refusal")
}

// TestProvision_EnsureWorktree_RegistryNamesNonCanonicalPath_Refused proves
// the registry refusal's lexical check is load-bearing: a marker whose own
// text is not yet the canonical "worktrees/<name>" form is refused even when
// every resolved-path check on it would otherwise pass, because ensureWorktree
// only registers a sharer against a path that already is that canonical form.
func TestProvision_EnsureWorktree_RegistryNamesNonCanonicalPath_Refused(t *testing.T) {
	cases := []struct {
		name        string
		buildMarker func(t *testing.T, agentAWorktree, hostPath string) string
	}{
		{
			name: "symlink-and-dot-dot",
			buildMarker: func(t *testing.T, agentAWorktree, hostPath string) string {
				adminEntry := filepath.Join(hostPath, ".git", "worktrees", "agent-a")
				inner := filepath.Join(agentAWorktree, "agent-a")
				require.NoError(t, os.MkdirAll(inner, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644))
				up := filepath.Join(agentAWorktree, "up")
				require.NoError(t, os.Symlink(agentAWorktree, up))
				sep := string(filepath.Separator)
				return up + sep + ".." + sep + "agent-a"
			},
		},
		{
			name: "symlink-back-to-worktrees",
			buildMarker: func(t *testing.T, agentAWorktree, hostPath string) string {
				adminEntry := filepath.Join(hostPath, ".git", "worktrees", "agent-a")
				// the same gitdir in absolute form, so the shape check alone
				// accepts the path below.
				require.NoError(t, os.WriteFile(filepath.Join(agentAWorktree, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644))
				wt := filepath.Join(agentAWorktree, "wt")
				require.NoError(t, os.Symlink(filepath.Join(hostPath, "worktrees"), wt))
				return filepath.Join(wt, "agent-a")
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_HOST_UID", "")
			locker := newTestLocker()
			bareRepo := initBareGitRepo(t)

			projectDir := t.TempDir()
			hostPath := filepath.Join(projectDir, "workspace")
			projectID := fmt.Sprintf("proj-noncanonical-%d", i)

			require.NoError(t, ProvisionShared(ProvisionInput{
				Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
				ProjectID: projectID,
				AgentID:   "agent-a",
				AgentName: "agent-a",
				Mode:      store.SharingModeWorktreePerAgent,
				Locker:    locker,
				GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
			}))

			agentAWorktree := WorktreePath(hostPath, "agent-a")
			marker := tc.buildMarker(t, agentAWorktree, hostPath)

			branch := fmt.Sprintf("other-branch-3-%d", i)
			require.NoError(t, RegisterSharer(hostPath, branch, marker, "agent-a"))

			err := ProvisionShared(ProvisionInput{
				Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
				ProjectID: projectID,
				AgentID:   "agent-c",
				AgentName: branch,
				Mode:      store.SharingModeWorktreePerAgent,
				Locker:    locker,
				GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
			})
			require.Error(t, err, "expected ProvisionShared to refuse joining a non-canonical registry marker")

			sharers, wtPath, listErr := ListSharers(hostPath, branch)
			require.NoError(t, listErr)
			assert.Equal(t, marker, wtPath, "the original (bogus) registration must survive unchanged")
			assert.Equal(t, []string{"agent-a"}, sharers, "expected no new agent added to the registry on refusal")
		})
	}
}

// TestProvision_EnsureWorktree_CreateCollisionFallbackRefusesNonDirectChild
// proves the "already used by worktree" fallback in the CREATE path — reached
// when git itself reports the branch already checked out elsewhere — applies
// the same real-worktree and direct-child checks as the proactive JOIN check
// above it, rather than trusting findWorktreeForBranch's result unchecked.
// git permits a worktree nested inside another worktree, so this uses a real
// `git worktree add` to create one, not a hand-built fixture.
func TestProvision_EnsureWorktree_CreateCollisionFallbackRefusesNonDirectChild(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-create-collision",
		AgentID:   "agent-a",
		AgentName: "agent-a",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	}))

	// A real worktree, created by git itself, nested inside agent-a's own
	// worktree — not a hand-built fixture. git worktree add permits this.
	nested := filepath.Join(WorktreePath(hostPath, "agent-a"), "sub")
	runIn(t, hostPath, "git", "worktree", "add", "--relative-paths", "-b", "nested-br", nested)

	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-create-collision",
		AgentID:   "agent-c",
		AgentName: "nested-br",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "expected ProvisionShared to refuse the collision fallback joining a nested worktree")

	// No marker was ever written naming the nested path.
	sharers, wtPath, listErr := ListSharers(hostPath, "nested-br")
	require.NoError(t, listErr)
	if wtPath == nested {
		t.Errorf("expected no marker naming the nested path %s, but the registry has one (sharers=%v)", nested, sharers)
	}
}

// TestProvision_EnsureWorktree_FirstCollisionFallbackRefusesNonDirectChild
// proves the first collision fallback in the CREATE path — reached when the
// initial `git worktree add -b <branch>` attempt itself reports the branch
// already checked out or already used by worktree elsewhere, rather than via
// the "already exists" retry — applies the same real-worktree and
// direct-child checks as the second fallback. A modern git normally reports
// "already exists" on that first attempt instead (routing through the second
// fallback, covered above), so this drives the first fallback with a PATH
// wrapper around the real git binary that forces that exact message on any
// "-b" invocation.
func TestProvision_EnsureWorktree_FirstCollisionFallbackRefusesNonDirectChild(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the PATH wrapper script requires /bin/sh, not available on windows")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found on PATH")
	}

	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-first-fallback",
		AgentID:   "agent-a",
		AgentName: "agent-a",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	}))

	// A real worktree, created by git itself, nested inside agent-a's own
	// worktree.
	nested := filepath.Join(WorktreePath(hostPath, "agent-a"), "sub")
	runIn(t, hostPath, "git", "worktree", "add", "--relative-paths", "-b", "nested-br", nested)

	// t.Setenv restores the original PATH automatically when the test ends.
	binDir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = \"-b\" ]; then echo \"fatal: 'nested-br' is already used by worktree at '" + nested + "'\" >&2; exit 128; fi; done\nexec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-first-fallback",
		AgentID:   "agent-c",
		AgentName: "nested-br",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "expected ProvisionShared to refuse the first collision fallback joining a nested worktree")

	sharers, wtPath, listErr := ListSharers(hostPath, "nested-br")
	require.NoError(t, listErr)
	if wtPath == nested {
		t.Errorf("expected no marker naming the nested path %s, but the registry has one (sharers=%v)", nested, sharers)
	}
}

func TestProvision_WorktreePerAgent_UniqueBranches_SoleSharers(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Agent A with unique branch.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-unique-1",
		AgentID:   "agent-a",
		AgentName: "agent-alpha",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Agent B with unique branch.
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-unique-1",
		AgentID:   "agent-b",
		AgentName: "agent-beta",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Both have their own worktrees.
	wtA := WorktreePath(hostPath, "agent-a")
	wtB := WorktreePath(hostPath, "agent-b")
	require.DirExists(t, wtA)
	require.DirExists(t, wtB)
	assert.NotEqual(t, wtA, wtB)

	// Each is sole sharer of its own branch.
	sharersA, pathA, err := ListSharers(hostPath, "agent-alpha")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-a"}, sharersA)
	assert.Equal(t, wtA, pathA)

	sharersB, pathB, err := ListSharers(hostPath, "agent-beta")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-b"}, sharersB)
	assert.Equal(t, wtB, pathB)
}

func TestProvision_WorktreePerAgent_ExistingRegistration_Idempotent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Provision agent once.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-idem-1",
		AgentID:   "agent-a",
		AgentName: "idem-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Provision the same agent again (idempotent).
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-idem-1",
		AgentID:   "agent-a",
		AgentName: "idem-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	// Should still have exactly one sharer.
	sharers, _, err := ListSharers(hostPath, "idem-branch")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-a"}, sharers)
}

func intPtr(i int) *int { return &i }
