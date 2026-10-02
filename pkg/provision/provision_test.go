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
	"encoding/json"
	"errors"
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
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
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
// F-111 review fix (tf-lead): chownProjectTree's native walk (os.Lchown,
// never os.Chown or os.Chmod on a walked path — see its doc) must preserve
// it: as root with CAP_DAC_OVERRIDE (the k8s init container's own security
// context), a symlink inside a cloned/possibly-untrusted tree
// pointing OUTSIDE it (elsewhere in the init container's filesystem view,
// or another mounted shared dir) must never have its REFERENT re-owned,
// only the link itself.
//
// This sandbox has no CAP_CHOWN (verified directly, matching the pattern in
// cloudrun_sandbox_runtime_test.go's TestPrepareScionLayout_ChownsDirectories),
// so this cannot chown to a different uid at all, let alone reproduce the
// real k8s init container (root, CAP_DAC_OVERRIDE, a target uid that
// actually differs from the caller's). This test is a same-uid chown, and
// checks that ctime — which chown/lchown update unconditionally, even when
// the new owner equals the old one — is untouched on the outside target
// (verified empirically before writing this test: an os.Lchown of the link
// itself to the process's own uid:gid left the outside target's ctime
// byte-for-byte identical down to the nanosecond). That is a real
// regression guard on Lchown's no-dereference behavior specifically, and
// the best this environment can exercise — it is NOT proof that the real
// root/CAP_DAC_OVERRIDE scenario is safe, only that Lchown itself does what
// it's documented to do here.
func TestChownProjectTree_SymlinkOutsideTree_TargetOwnershipUnchanged(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("ctime/uid check uses syscall.Stat_t (Linux only)")
	}

	// A tree we're about to run chownProjectTree over...
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

	if err := chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid()); err != nil {
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
	// have been (l)chowned — proves chownProjectTree's walk actually visited
	// it, not that it silently no-op'd on everything including the link.
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

// TestChownProjectTree_SymlinkToOutsideDirectory_NeverDescends proves the
// companion property to the test above for a symlink that points to a
// DIRECTORY, not a file: filepath.WalkDir must never descend into it (a
// symlinked directory's DirEntry carries the link's own mode bits, not the
// target's, so d.IsDir() is false for it and WalkDir's own traversal never
// attempts to read it as a directory). A file inside the outside directory
// must be completely untouched — not merely "not re-owned" the way the
// file-symlink case proves, but never visited at all.
func TestChownProjectTree_SymlinkToOutsideDirectory_NeverDescends(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("ctime/uid check uses syscall.Stat_t (Linux only)")
	}

	treeRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "inside-outside-dir.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("do not touch"), 0644))

	linkPath := filepath.Join(treeRoot, "link-to-outside-dir")
	require.NoError(t, os.Symlink(outsideDir, linkPath))

	before, err := os.Lstat(outsideFile)
	require.NoError(t, err)
	beforeStat := before.Sys().(*syscall.Stat_t)

	require.NoError(t, chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid()))

	after, err := os.Lstat(outsideFile)
	require.NoError(t, err)
	afterStat := after.Sys().(*syscall.Stat_t)

	assert.Equal(t, beforeStat.Ctim, afterStat.Ctim,
		"a file inside a symlinked-to directory must never be visited at all, not even lchowned to the same uid:gid")

	linkInfo, err := os.Lstat(linkPath)
	require.NoError(t, err)
	linkStat := linkInfo.Sys().(*syscall.Stat_t)
	assert.Equal(t, os.Getuid(), int(linkStat.Uid), "the symlink itself must still be lchowned")
	assert.Equal(t, os.Getgid(), int(linkStat.Gid), "the symlink itself must still be lchowned")
}

// TestChownProjectTree_DanglingSymlink_DoesNotFail proves a symlink whose
// target does not exist at all is still handled cleanly: os.Lchown operates
// on the link itself, never the (nonexistent) target, so a dangling link is
// no different from a live one as far as chownProjectTree is concerned.
func TestChownProjectTree_DanglingSymlink_DoesNotFail(t *testing.T) {
	treeRoot := t.TempDir()
	linkPath := filepath.Join(treeRoot, "dangling-link")
	require.NoError(t, os.Symlink(filepath.Join(treeRoot, "does-not-exist"), linkPath))

	err := chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid())
	assert.NoError(t, err, "a dangling symlink must not fail the walk: lchown never follows it to a target that isn't there")

	_, lstatErr := os.Lstat(linkPath)
	assert.NoError(t, lstatErr, "the dangling link itself must still be present and untouched otherwise")
}

// TestChownProjectTree_LockArtifactVanishesMidWalk_DoesNotFail proves a
// concurrent waiter's own non-destructive lock bookkeeping (a clock-probe
// file write-then-remove in serverNow, or an evicted-generation directory
// swept by garbageCollectLockLitter) can never fail this holder's chown,
// even though it can remove a lock-prefixed path out from under it at any
// moment, because the lock lives inside the very tree chownProjectTree
// walks (see its own doc).
//
// The probe file is present (and so captured by the walk's directory read)
// when the walk starts, then removed via the lchownFile hook once the walk
// reaches an earlier, lexically-preceding trigger file — reproducing an
// actual vanish-DURING-the-walk removal, not merely a file that was already
// gone before the walk started: the trigger file's name is chosen to sort
// immediately before the lock's own prefix, so its lchownFile call always
// happens (it is never excluded), and it happens before the walk would
// otherwise reach either the lock directory or the probe file. Proves the
// walk never even attempts to stat or lchown a lock-prefixed path at all —
// so a probe removed in exactly this window can never surface through it —
// while still reaching and chowning ordinary project files normally.
func TestChownProjectTree_LockArtifactVanishesMidWalk_DoesNotFail(t *testing.T) {
	treeRoot := t.TempDir()
	lockPath := filepath.Join(treeRoot, provisionFileLockName)
	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = releaseFileLock(lockPath, token)() })

	probePath := filepath.Join(treeRoot, provisionClockProbeFile+"-deadbeef")
	require.NoError(t, os.WriteFile(probePath, []byte("probe"), 0600))

	// ".aaa-" sorts lexically before ".scion-provision.lock" itself, so
	// WalkDir visits (and lchowns) this file before it would otherwise reach
	// either the lock directory or the probe file below it.
	triggerFile := filepath.Join(treeRoot, ".aaa-trigger-file.txt")
	require.NoError(t, os.WriteFile(triggerFile, []byte("content"), 0644))

	orig := lchownFile
	var sawLockPath, sawTriggerFile, removedProbe atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		if isLockArtifactPath(name) {
			sawLockPath.Store(true)
		}
		if name == triggerFile {
			sawTriggerFile.Store(true)
			if removedProbe.CompareAndSwap(false, true) {
				// Simulate a concurrent waiter's probe vanishing mid-walk,
				// exactly as serverNow's own deferred remove would do,
				// after the walk's directory read already captured it but
				// before the walk reaches it.
				require.NoError(t, os.Remove(probePath))
			}
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err = chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid())
	assert.NoError(t, err, "a lock artifact vanishing mid-walk must never fail the walk: it is excluded by name, never stat'd or lchowned at all")
	assert.True(t, removedProbe.Load(), "setup: the probe must actually have been removed mid-walk, not merely before it started")
	assert.False(t, sawLockPath.Load(), "the walk must never attempt to lchown any lock-prefixed path, present or vanished")
	assert.True(t, sawTriggerFile.Load(), "setup: the walk must still reach and chown ordinary project files, proving the exclusion is scoped to the lock prefix only")
}

// TestChownProjectTree_ExclusionLimitedToLockDirDirectChildren proves the
// exclusion in chownProjectTree is scoped to DIRECT children of lockDir
// itself, not to any path anywhere in the tree that merely shares the
// lock's name prefix: the lock only ever lives directly inside lockDir (see
// acquireFileLock's doc), so a same-named entry nested inside a cloned
// repo, or sitting in a shared dir (which never holds the lock at all, and
// passes an empty lockDir), is ordinary project content and must still be
// chowned like everything else -- not silently left under the init
// container's root ownership. Also exercises the near-name precision at the
// lock's own level: a name that merely starts with the lock's base name,
// without the separating ".", is not a variant of it and must still be
// chowned.
func TestChownProjectTree_ExclusionLimitedToLockDirDirectChildren(t *testing.T) {
	treeRoot := t.TempDir()

	// The real lock, directly inside treeRoot (== lockDir): must be excluded.
	realLockDir := filepath.Join(treeRoot, provisionFileLockName)
	require.NoError(t, os.MkdirAll(realLockDir, 0770))

	// A near-name at the lock's own level: shares the base name as a plain
	// prefix, but without the required "." separator, so it is NOT a lock
	// variant and must be chowned.
	nearName := filepath.Join(treeRoot, provisionFileLockName+"file")
	require.NoError(t, os.WriteFile(nearName, []byte("not a lock"), 0644))

	// Same-named entries NESTED inside ordinary project content -- never
	// where the real lock lives -- must be chowned like any other file.
	subDir := filepath.Join(treeRoot, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0770))
	nestedLockDir := filepath.Join(subDir, provisionFileLockName)
	require.NoError(t, os.MkdirAll(nestedLockDir, 0770))
	nestedLockVariant := filepath.Join(subDir, provisionFileLockName+".x")
	require.NoError(t, os.WriteFile(nestedLockVariant, []byte("not the lock either"), 0644))

	orig := lchownFile
	var sawRealLock, sawNearName, sawNestedLockDir, sawNestedLockVariant atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		switch name {
		case realLockDir:
			sawRealLock.Store(true)
		case nearName:
			sawNearName.Store(true)
		case nestedLockDir:
			sawNestedLockDir.Store(true)
		case nestedLockVariant:
			sawNestedLockVariant.Store(true)
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid())
	require.NoError(t, err)

	assert.False(t, sawRealLock.Load(), "the real lock, a direct child of lockDir, must be excluded")
	assert.True(t, sawNearName.Load(), "a near-name without the separating '.' must not be treated as a lock variant")
	assert.True(t, sawNestedLockDir.Load(), "a same-named directory NESTED below lockDir is not the lock and must be chowned")
	assert.True(t, sawNestedLockVariant.Load(), "a same-named file NESTED below lockDir is not the lock and must be chowned")
}

// TestChownProjectTree_EmptyLockDir_NeverExcludesAnything proves the shared-dir
// call path (lockDir == "", since a shared dir never holds the lock) applies
// no exclusion at all, even for a path that would otherwise look exactly
// like the lock's own entry at the walk's root.
func TestChownProjectTree_EmptyLockDir_NeverExcludesAnything(t *testing.T) {
	sharedDir := t.TempDir()
	lookalike := filepath.Join(sharedDir, provisionFileLockName)
	require.NoError(t, os.MkdirAll(lookalike, 0770))

	orig := lchownFile
	var sawLookalike atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		if name == lookalike {
			sawLookalike.Store(true)
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := chownProjectTree(context.Background(), sharedDir, "", os.Getuid(), os.Getgid())
	require.NoError(t, err)

	assert.True(t, sawLookalike.Load(), "with an empty lockDir, nothing is excluded, even a lock-shaped name")
}

// TestChownProjectTree_ContinuesPastAFailedEntry proves the walk's
// best-effort semantics directly: a failure on one early entry must not
// stop the walk from reaching and chowning later entries, and the error
// chownProjectTree eventually returns must still report that failure. This
// matters most on the broker's own path (RequireChownSuccess false): an
// aborted walk would otherwise leave the sentinel written over a tree that
// is only partially chowned.
func TestChownProjectTree_ContinuesPastAFailedEntry(t *testing.T) {
	treeRoot := t.TempDir()
	// Lexical order: a.txt, b.txt, sub/c.txt.
	aPath := filepath.Join(treeRoot, "a.txt")
	bPath := filepath.Join(treeRoot, "b.txt")
	subDir := filepath.Join(treeRoot, "sub")
	cPath := filepath.Join(subDir, "c.txt")
	require.NoError(t, os.WriteFile(aPath, []byte("a"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("b"), 0644))
	require.NoError(t, os.MkdirAll(subDir, 0770))
	require.NoError(t, os.WriteFile(cPath, []byte("c"), 0644))

	injected := errors.New("simulated lchown failure on a.txt")
	orig := lchownFile
	var sawB, sawC atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		switch name {
		case aPath:
			return injected
		case bPath:
			sawB.Store(true)
		case cPath:
			sawC.Store(true)
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid())
	require.Error(t, err, "a failed entry must still surface as an overall error")
	assert.ErrorIs(t, err, injected, "the reported error must wrap the actual failure, not a generic one")
	assert.True(t, sawB.Load(), "a later sibling must still be reached after an earlier entry's failure")
	assert.True(t, sawC.Load(), "a later entry in a subdirectory must still be reached after an earlier entry's failure")
}

// TestChownProjectTree_ContinuesPastADirectoryReadError proves the OTHER
// continue-past-failure case, distinct from a failed Lchown: a directory
// that WalkDir itself fails to read (removed between being visited and
// being descended into) must not abort the rest of the walk either — a
// sibling directory must still be reached and chowned.
func TestChownProjectTree_ContinuesPastADirectoryReadError(t *testing.T) {
	treeRoot := t.TempDir()
	subA := filepath.Join(treeRoot, "subA")
	subB := filepath.Join(treeRoot, "subB")
	subBFile := filepath.Join(subB, "file.txt")
	require.NoError(t, os.MkdirAll(subA, 0770))
	require.NoError(t, os.MkdirAll(subB, 0770))
	require.NoError(t, os.WriteFile(subBFile, []byte("content"), 0644))

	orig := lchownFile
	var sawSubBFile atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		if name == subA {
			// Remove subA entirely right after it is visited (but before
			// WalkDir tries to read its contents to recurse into it),
			// simulating a directory vanishing mid-walk.
			require.NoError(t, os.RemoveAll(subA))
		}
		if name == subBFile {
			sawSubBFile.Store(true)
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := chownProjectTree(context.Background(), treeRoot, treeRoot, os.Getuid(), os.Getgid())
	assert.Error(t, err, "a directory that vanished mid-walk must still surface as an overall error")
	assert.True(t, sawSubBFile.Load(), "a sibling directory's contents must still be reached after an earlier directory's read error")
}

// TestChownProjectTree_CtxCancellation_StopsTheWalk proves ctx is the one
// thing that stops the walk outright, unlike an ordinary per-entry failure:
// cancelling ctx during the very first entry must prevent any later entry
// from being visited at all, and the returned error must reflect the
// cancellation.
func TestChownProjectTree_CtxCancellation_StopsTheWalk(t *testing.T) {
	treeRoot := t.TempDir()
	aPath := filepath.Join(treeRoot, "a.txt")
	bPath := filepath.Join(treeRoot, "b.txt")
	require.NoError(t, os.WriteFile(aPath, []byte("a"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("b"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	orig := lchownFile
	var sawB atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		if name == aPath {
			cancel()
		}
		if name == bPath {
			sawB.Store(true)
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := chownProjectTree(ctx, treeRoot, treeRoot, os.Getuid(), os.Getgid())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "a ctx cancellation must surface as the walk's own error")
	assert.False(t, sawB.Load(), "no later entry may be visited once ctx is cancelled, unlike an ordinary per-entry failure")
}

// --- chownProjectTree input validation ---
//
// The full table of rejected critical-system-path names is tested directly
// against pkg/util/fsutil (TestCheckRoot_*), which is what chownProjectTree
// delegates to for this check. Tests here only cover this call site's own
// wiring, and never invoke the recursive chown on anything other than a
// t.TempDir() tree.

// TestChownProjectTree_RefusesFilesystemRootLookalike proves the guard is actually
// wired into chownProjectTree (not just defined and unused): given a
// workspace root laid out like a filesystem root, no chown must be
// attempted at all.
func TestChownProjectTree_RefusesFilesystemRootLookalike(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "etc"))
	mustWriteFile(t, filepath.Join(dir, "etc", "passwd"), "root:x:0:0:root:/root:/bin/sh\n")
	mustMkdirAll(t, filepath.Join(dir, "usr", "bin"))
	mustMkdirAll(t, filepath.Join(dir, "proc"))

	before, err := os.Lstat(filepath.Join(dir, "etc", "passwd"))
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	err = chownProjectTree(context.Background(), dir, dir, os.Getuid()+1, os.Getgid()+1)
	if !errors.Is(err, fsutil.ErrFilesystemRootLookalike) {
		t.Fatalf("chownProjectTree(%q) = %v, want it to wrap ErrFilesystemRootLookalike", dir, err)
	}

	after, err := os.Lstat(filepath.Join(dir, "etc", "passwd"))
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite refusal: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownProjectTree_AllowsOrdinaryWorkspace is a smoke test on the public
// entry point using a real, ordinary temp-dir tree.
func TestChownProjectTree_AllowsOrdinaryWorkspace(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "src"))
	mustWriteFile(t, filepath.Join(dir, "README.md"), "hello\n")

	if err := chownProjectTree(context.Background(), dir, dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownProjectTree(%q) = %v, want nil", dir, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
	_, err := acquireProvisionLock(ctx, in, t.TempDir())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context cancelled")
	assert.Less(t, elapsed, 2*time.Second, "should return promptly on context cancellation, not wait for all retries")
}

// --- acquireFileLock: fallback mutex used when no Locker is available
// (the k8s init container's normal case — it has no Hub/DB connection). ---

// makeLongHeldCrashedLock publishes a real generation, gives it a heartbeat
// file, and back-dates both the way a lock that was held for a long time and
// then crashed looks on disk: the lock directory's own mtime is the time its
// last entry was CREATED (the first heartbeat, shortly after acquisition,
// because later beats rewrite an existing file and do not touch the
// directory), and the heartbeat file's mtime is the last beat.
func makeLongHeldCrashedLock(t *testing.T, lockPath string, heldFor time.Duration) string {
	t.Helper()
	tok, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(lockPath, provisionLockHeartbeatFile), []byte(tok), 0600))
	lastBeat := time.Now().Add(-provisionLockStaleAfter - 30*time.Second)
	firstBeat := lastBeat.Add(-heldFor)
	require.NoError(t, os.Chtimes(filepath.Join(lockPath, provisionLockHeartbeatFile), lastBeat, lastBeat))
	require.NoError(t, os.Chtimes(lockPath, firstBeat, firstBeat))
	return tok
}

// withFastFileLockTiming shrinks the package-level retry/heartbeat timing
// vars for the duration of one test, restoring them on cleanup, so tests
// that need several real retry/heartbeat cycles to elapse don't have to
// wait out the multi-minute production values. Safe because no test in this
// package runs in parallel (no t.Parallel() calls) — see none needed.
func withFastFileLockTiming(t *testing.T) {
	t.Helper()
	origDelay, origHeartbeat := fileLockRetryDelay, provisionLockHeartbeatInterval
	fileLockRetryDelay = 20 * time.Millisecond
	provisionLockHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		fileLockRetryDelay = origDelay
		provisionLockHeartbeatInterval = origHeartbeat
	})
}

func TestAcquireFileLock_MutualExclusion(t *testing.T) {
	dir := t.TempDir()

	held1, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	// t.Cleanup, not just the explicit release below: if the require.Error
	// assertion after this fails (i.e. exactly the bug this test exists to
	// catch), the test exits immediately and an explicit-only release would
	// never run, leaving held1's heartbeat goroutine running into later tests.
	t.Cleanup(func() { _ = held1.release() })

	// A second, concurrent acquire attempt must not succeed while the first
	// is held. Use a short-lived context so this returns quickly instead of
	// waiting out the full retry budget.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = acquireFileLock(ctx, dir)
	require.Error(t, err, "second acquire should not succeed while the first holder is active")

	require.NoError(t, held1.release())

	// Now that it's released, acquisition should succeed again immediately.
	held2, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held2.release() })
	require.NoError(t, held2.release())
}

func TestAcquireFileLock_ConcurrentAcquirers_ExactlyOneAtATime(t *testing.T) {
	withFastFileLockTiming(t)
	dir := t.TempDir()

	const n = 8
	var holders atomic.Int32
	var maxObserved atomic.Int32
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			held, err := acquireFileLock(ctx, dir)
			if err != nil {
				errs[idx] = err
				return
			}
			cur := holders.Add(1)
			for {
				prev := maxObserved.Load()
				if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
					break
				}
			}
			// Hold briefly so overlapping acquirers actually contend.
			time.Sleep(20 * time.Millisecond)
			holders.Add(-1)
			errs[idx] = held.release()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	assert.Equal(t, int32(1), maxObserved.Load(), "more than one goroutine held the lock at once")
}

func TestAcquireFileLock_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	require.NoError(t, held.release())
}

func TestAcquireFileLock_ReclaimsStaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte("dead-owner"), 0600))

	// Back-date the lock directory well past the staleness threshold so it
	// looks abandoned (its "owner" crashed without ever beating, hence no
	// heartbeat marker — lastHeartbeat falls back to the lock dir's own mtime).
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	start := time.Now()
	held, err := acquireFileLock(context.Background(), dir)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, fileLockRetryDelay*3, "stale lock should be reclaimed promptly, not waited out via full retries")
	require.NoError(t, held.release())
}

func TestAcquireFileLock_DoesNotReclaimFreshLock(t *testing.T) {
	dir := t.TempDir()

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	// The held lock is fresh (just created) — a concurrent waiter must not
	// treat it as stale and reclaim it out from under the holder.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = acquireFileLock(ctx, dir)
	require.Error(t, err)
}

// TestAcquireFileLock_ReleaseAfterReclaim_DoesNotDeleteNewOwner proves the
// owner-id check in releaseFileLock: if the original holder is slow
// enough that a waiter reclaims its lock as stale and acquires a new one at
// the same path, the original holder's eventual (stale) release call must
// not tear down the new, legitimate lock.
func TestAcquireFileLock_ReleaseAfterReclaim_DoesNotDeleteNewOwner(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	// Simulate an original holder that acquired the lock a long time ago. It
	// has no heartbeat goroutine running (this calls releaseFileLock
	// directly instead of going through acquireFileLock), matching a holder
	// that doesn't yet know it lost the lock, exactly the case release's own
	// ownership re-check exists to protect against.
	staleToken, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	staleRelease := releaseFileLock(lockPath, staleToken)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	// A waiter reclaims it as stale and acquires a fresh lock at the same path.
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.release() })

	// The original holder finally gets around to releasing its (now stale
	// and reclaimed) lock. This must be a no-op, not a deletion of the new
	// owner's live lock.
	require.NoError(t, staleRelease())
	assert.DirExists(t, lockPath, "stale release must not delete the new owner's lock")

	require.NoError(t, held.release())
}

// TestReleaseFileLock_OwnerIDMismatch_DoesNotDeleteBackdatedSuccessor proves
// the owner-id check in releaseFileLock: a
// successor lock that happens to ALSO look stale by a naive timestamp check
// (simulating clock skew or cached attributes) must still survive a stale
// holder's release call, because what actually protects it is owner-id
// identity, not timestamps — a test that only relies on the successor's
// fresh mtime to short-circuit would never exercise that comparison at all.
//
// release never "restores" anything — see releaseFileLock's doc. It
// survives here because the stale holder's
// release targets the DETERMINISTIC destination for ITS OWN id
// (evictPathFor(lockPath, staleToken)), which the earlier reclaim below
// already occupies; the rename can therefore only fail, and a failed rename
// never touches whatever currently occupies lockPath.
func TestReleaseFileLock_OwnerIDMismatch_DoesNotDeleteBackdatedSuccessor(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	staleToken, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	staleRelease := releaseFileLock(lockPath, staleToken)

	old := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, old, old))
	require.True(t, tryReclaimIfAbandoned(dir, lockPath), "setup: a waiter should evict the now-stale-looking L1")
	tokenL2, err := tryCreateFileLock(lockPath)
	require.NoError(t, err, "setup: a successor acquires a fresh L2")

	// Back-date L2 too, simulating skew/cached attributes making it ALSO
	// look stale by timestamp alone.
	require.NoError(t, os.Chtimes(lockPath, old, old))

	require.NoError(t, staleRelease())
	current, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	require.NoError(t, err, "L2 must still exist")
	assert.Equal(t, tokenL2, string(current), "stale holder's release must not delete a back-dated successor lock")
}

// TestReleaseFileLock_SupersededBeforeRelease_DoesNotVacateSuccessor is the
// permanent regression test proving release never renames
// lockPath aside before checking whether it is still the one holding
// it. The deterministic per-id destination name means a stale release
// that reuses the SAME id as an earlier reclaim of its own generation is
// normally already blocked by simple name collision — evictPathFor(lockPath,
// tokenH) is already occupied by that earlier reclaim's own remnant. That
// remnant is not permanent, though: garbageCollectLockLitter eventually
// removes it, which frees the name back up. This test simulates that: once
// the remnant is gone, a stale holder H — calling release directly, with no
// heartbeat goroutine running to have told it otherwise — must still not
// blindly rename whatever currently occupies lockPath (a live successor S)
// to H's now-vacant destination name. release re-reads current ownership
// synchronously right before ever renaming anything, so it no-ops the
// instant it is no longer the recorded owner, regardless of whether its
// destination name happens to be free.
func TestReleaseFileLock_SupersededBeforeRelease_DoesNotVacateSuccessor(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	tokenH, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))
	// hRelease is bound to H's owner id with no heartbeat goroutine running,
	// representing a release that runs with no up-to-date signal of its own
	// loss other than its own ownership re-check.
	hRelease := releaseFileLock(lockPath, tokenH)

	// An independent reclaimer judges H's lock abandoned and evicts it, and
	// a successor S publishes a fresh, live lock in its place.
	require.True(t, tryReclaimIfAbandoned(dir, lockPath), "setup: reclaim H's stale lock")
	tokenS, err := tryCreateFileLock(lockPath)
	require.NoError(t, err, "setup: successor S acquires a fresh lock")

	// Simulate garbageCollectLockLitter having already cleaned up the
	// reclaim's remnant, freeing H's destination name back up.
	require.NoError(t, os.RemoveAll(evictPathFor(lockPath, tokenH)))

	// H, unaware it has been superseded, now releases.
	require.NoError(t, hRelease())

	assert.True(t, stillOwnsLockFile(lockPath, tokenS),
		"H's release vacated successor S's live lock even though H no longer owned it")
}

// TestReleaseFileLock_DestinationAlreadyExists_IsNotAnError proves release
// treats an already-occupied destination (os.IsExist) as the safe, expected
// outcome it is, not an error: a reclaimer having already moved this exact
// generation there before release got to it. No other test exercises
// release with its own deterministic destination pre-occupied by a genuine
// prior eviction of the SAME generation.
func TestReleaseFileLock_DestinationAlreadyExists_IsNotAnError(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	// Pre-occupy the exact deterministic destination this release will
	// target, standing in for some other remover (e.g. a reclaimer) having
	// already evicted this precise generation a moment before release got
	// to it. lockPath itself still genuinely holds `token`, so release's own
	// ownership check passes and it proceeds to the rename.
	require.NoError(t, os.Mkdir(evictPathFor(lockPath, token), 0700))

	release := releaseFileLock(lockPath, token)
	assert.NoError(t, release(), "a release whose destination was already taken by a prior removal of the same generation must be a safe no-op, not an error")
}

// TestReleaseFileLock_StillOwnedAfterTransientMisses_ActuallyReleases proves
// release always releases a lock this process still genuinely owns, with no
// other condition that can make it skip the rename: release's only guard is
// the ownership re-check, so repeated TRANSIENT heartbeat trouble (e.g.
// write errors unrelated to actual ownership) elsewhere has no bearing on
// whether a still-valid lock actually gets released here.
func TestReleaseFileLock_StillOwnedAfterTransientMisses_ActuallyReleases(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	require.NoError(t, releaseFileLock(lockPath, token)())

	assert.False(t, stillOwnsLockFile(lockPath, token), "release must actually release a lock this process still owns")
	assert.DirExists(t, evictPathFor(lockPath, token), "the release must have actually run the eviction rename, not silently no-op'd")
}

// TestReleaseFileLock_ElapsedTimeGuard_AbortsLongPause is the permanent
// regression test for release's own elapsed-time guard: its ownership check
// and its eviction rename are two separate calls, exactly like the
// reclaimer's read and rename, so a release that pauses between them for
// longer than the bound must abort rather than rename — a long enough pause
// here means the lock has already gone stale, been reclaimed, and
// (potentially) replaced by a live successor, and renaming at that point
// would vacate the successor instead of harmlessly releasing this
// generation's own, already-superseded lock.
func TestReleaseFileLock_ElapsedTimeGuard_AbortsLongPause(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(provisionLockStaleAfter + time.Second) // past the guard's bound
	}
	t.Cleanup(func() { timeNow = origNow })

	err = releaseFileLock(lockPath, token)()
	timeNow = origNow

	require.NoError(t, err, "aborting the rename must not itself be reported as an error")
	assert.True(t, stillOwnsLockFile(lockPath, token), "the lock must be untouched when release aborts on a long pause")
}

// TestAcquireFileLock_AcquisitionInvokesGarbageCollection proves an old,
// stray staging directory left behind by a crashed tryCreateFileLock gets
// swept as a side effect of a normal acquisition, not merely when
// garbageCollectLockLitter is called directly (as the dedicated GC tests
// above do).
func TestAcquireFileLock_AcquisitionInvokesGarbageCollection(t *testing.T) {
	dir := t.TempDir()
	staleStage, err := os.MkdirTemp(dir, provisionLockStagingPattern)
	require.NoError(t, err)
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(staleStage, old, old))

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	assert.NoDirExists(t, staleStage, "a normal acquisition must garbage-collect old litter, not just a direct garbageCollectLockLitter call")
}

// TestTryReclaimIfAbandoned_ElapsedTimeGuard_AbortsLongPause proves a
// reclaimer's own pause between its non-destructive staleness read and its
// rename is bounded: a reclaimer stalled for longer than the
// garbage-collection cutoff (a GC pause, CPU starvation, or an NFS stall on
// just this node) could otherwise have its destination swept out from under
// it and then rename a live successor instead, with no holder ever having
// paused at all. tryReclaimIfAbandoned aborts rather than renames once its
// own elapsed time since the read exceeds a bound well under the GC cutoff,
// using timeNow (indirected so this test can simulate the pause without
// actually waiting).
func TestTryReclaimIfAbandoned_ElapsedTimeGuard_AbortsLongPause(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(provisionLockStaleAfter + time.Second) // past the guard's bound
	}
	t.Cleanup(func() { timeNow = origNow })

	evicted := tryReclaimIfAbandoned(dir, lockPath)
	timeNow = origNow

	assert.False(t, evicted, "a reclaimer that paused past the bound must abort rather than rename")
	assert.True(t, stillOwnsLockFile(lockPath, token), "the lock must be untouched when the reclaimer aborts")
}

// TestTryReclaimIfAbandoned_ElapsedTimeGuard_AllowsShortPause is the control
// for the test above: a pause well under the bound must not block a
// legitimate reclaim.
func TestTryReclaimIfAbandoned_ElapsedTimeGuard_AllowsShortPause(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	_, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(time.Millisecond) // comfortably under the guard's bound
	}
	t.Cleanup(func() { timeNow = origNow })

	evicted := tryReclaimIfAbandoned(dir, lockPath)
	timeNow = origNow

	assert.True(t, evicted, "a short, normal pause must not block a legitimate reclaim")
}

// TestTryCreateFileLock_ElapsedTimeGuard_DiscardsAfterLongPause is the
// permanent regression test for the publisher-side counterpart to the
// reclaimer guard above: a publisher paused for too long between building
// its staging directory and publishing it produces a lock that is already
// close to stale the moment it is published (rename does not refresh the
// directory's mtime), so a concurrent reclaimer
// can evict it and a second party can acquire while the original publisher
// still believes it holds the lock. tryCreateFileLock discards its
// staging directory and returns errPublishPausedTooLong instead of
// publishing once its own elapsed time since starting exceeds this bound.
func TestTryCreateFileLock_ElapsedTimeGuard_DiscardsAfterLongPause(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(provisionLockStaleAfter) // past the guard's bound (stale/4)
	}
	t.Cleanup(func() { timeNow = origNow })

	_, err := tryCreateFileLock(lockPath)
	timeNow = origNow

	require.Error(t, err)
	entries, rdErr := os.ReadDir(dir)
	require.NoError(t, rdErr)
	assert.Empty(t, entries, "a publisher that paused past the bound must discard its staging directory, not publish it")
}

// TestTryCreateFileLock_ElapsedTimeGuard_AllowsShortPause is the control: a
// normal, short time to build the staging directory must still publish.
func TestTryCreateFileLock_ElapsedTimeGuard_AllowsShortPause(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(time.Millisecond)
	}
	t.Cleanup(func() { timeNow = origNow })

	token, err := tryCreateFileLock(lockPath)
	timeNow = origNow

	require.NoError(t, err)
	assert.True(t, stillOwnsLockFile(lockPath, token))
}

// TestTryCreateFileLock_ElapsedTimeGuard_BoundHasRealMargin proves the
// publisher guard's bound stays well under half the stale window, not merely
// under it: a pause strictly between one quarter and one half of the stale
// window must still trip the guard (a bound that only catches pauses near
// the full stale window would leave no real margin for the publish rename
// RPC itself).
func TestTryCreateFileLock_ElapsedTimeGuard_BoundHasRealMargin(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	origNow := timeNow
	start := origNow()
	calls := 0
	timeNow = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(provisionLockStaleAfter * 3 / 8) // between stale/4 and stale/2
	}
	t.Cleanup(func() { timeNow = origNow })

	_, err := tryCreateFileLock(lockPath)
	timeNow = origNow

	require.Error(t, err, "a pause of 3/8 of the stale window must still trip the publisher guard")
	assert.ErrorIs(t, err, errPublishPausedTooLong)
}

// TestAcquireFileLock_PublishesWithImmediateSynchronousHeartbeat is the
// permanent regression test for the synchronous first beat right after
// publish: with the periodic heartbeat interval set far in the future (so an
// async-only first beat could not possibly have landed yet), a freshly
// acquired lock must still have a heartbeat marker immediately, because
// acquireFileLock beats once synchronously before ever returning.
func TestAcquireFileLock_PublishesWithImmediateSynchronousHeartbeat(t *testing.T) {
	dir := t.TempDir()
	origInterval := provisionLockHeartbeatInterval
	provisionLockHeartbeatInterval = time.Hour
	t.Cleanup(func() { provisionLockHeartbeatInterval = origInterval })

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	lockPath := filepath.Join(dir, provisionFileLockName)
	_, statErr := os.Stat(filepath.Join(lockPath, provisionLockHeartbeatFile))
	assert.NoError(t, statErr, "a fresh acquisition must have a heartbeat marker immediately, not only after the first periodic interval elapses")
}

// TestAcquireFileLock_SynchronousBeatDefiniteLoss_DoesNotReturnLock proves
// acquireFileLock never returns a held lock for a generation it has just
// been told, by its own synchronous first beat, that it does not own, and
// that it retries as ordinary contention rather than failing the
// acquisition outright. This can only happen if the publish rename itself,
// or the gap between it and this beat, stalled past the publisher's own
// elapsed-time bound, reachable only through that residual, not through any
// ordinary timing. Simulated directly: right after the publish rename
// succeeds, the owner file is rewritten to a different id, standing in for
// a successor that already occupies lockPath by the time this process's own
// synchronous beat runs. Because the rewritten owner file keeps lockPath
// looking fresh and live, a retrying implementation runs out its ctx
// deadline instead of ever acquiring or giving up early, while a
// fail-outright implementation would return its own error well before that
// deadline — asserting ctx's own error, not just any error, is what
// distinguishes the two.
func TestAcquireFileLock_SynchronousBeatDefiniteLoss_DoesNotReturnLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	orig := renameFile
	fired := false
	renameFile = func(src, dst string) error {
		err := orig(src, dst)
		if !fired && err == nil && dst == lockPath && strings.Contains(src, ".stage-") {
			fired = true
			other := newFileLockToken()
			require.NoError(t, writeLockFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte(other), 0600))
		}
		return err
	}
	t.Cleanup(func() { renameFile = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	held, err := acquireFileLock(ctx, dir)
	renameFile = orig
	if err == nil {
		_ = held.release()
	}

	require.Error(t, err, "a definite loss from the synchronous first beat must never be returned as a successful acquisition")
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"must be retried as ordinary contention until the ctx deadline, not failed outright with its own error")
}

// TestAcquireFileLock_SynchronousBeatVacantLockPath_RetriesInsteadOfReturningEvictedGeneration
// proves the companion outcome of the same publish residual as the test
// above: a reclaimer evicts this generation before any successor has
// published, so when the synchronous first beat runs, lockPath is simply
// vacant rather than owned by a different id. beatOnce's own read reports
// this as ENOENT, which is ordinarily tolerated as a transient miss for the
// periodic heartbeat's benefit (an owner file can be briefly absent
// mid-eviction on a lock this process still holds) — but this call just
// published this exact generation moments ago, so a missing owner file here
// must still be treated as a definite loss and retried, rather than
// returned as a successful acquisition of a generation that is already
// gone. Unlike the mismatched-owner case above, lockPath is free once this
// happens, so the retry legitimately succeeds on a fresh generation — the
// property under test is that the lock eventually returned actually owns
// whatever now sits at lockPath, never the evicted one. Simulated directly:
// right after the publish rename succeeds, lockPath is renamed away to
// stand in for a reclaimer's eviction, with no successor publishing in its
// place.
func TestAcquireFileLock_SynchronousBeatVacantLockPath_RetriesInsteadOfReturningEvictedGeneration(t *testing.T) {
	withFastFileLockTiming(t)
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	orig := renameFile
	fired := false
	var evictedToken string
	renameFile = func(src, dst string) error {
		err := orig(src, dst)
		if !fired && err == nil && dst == lockPath && strings.Contains(src, ".stage-") {
			fired = true
			token, rerr := readOwnerToken(lockPath)
			require.NoError(t, rerr)
			evictedToken = token
			require.NoError(t, orig(lockPath, evictPathFor(lockPath, token)))
		}
		return err
	}
	t.Cleanup(func() { renameFile = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	held, err := acquireFileLock(ctx, dir)
	renameFile = orig
	require.NoError(t, err, "a vacant lockPath found by the synchronous first beat must be retried, since lockPath is actually free, not returned as a failure")
	defer func() { _ = held.release() }()

	assert.True(t, held.stillOwned(), "the retried acquisition must legitimately own whatever now sits at lockPath, not the generation that was evicted before its own synchronous beat ran")
	current, rerr := readOwnerToken(lockPath)
	require.NoError(t, rerr)
	assert.NotEqual(t, evictedToken, current, "lockPath's current owner must be the new, retried generation, not the evicted one")
}

// TestAcquireFileLock_SynchronousBeatMismatchedFollowUpRead_DoesNotReturnLock
// proves a third outcome of the same publish residual as the two tests
// above: beatOnce's own heartbeat write can fail for a reason that is NOT
// ENOENT (an ESTALE on NFS, for instance — precisely because this
// generation was evicted at that exact moment), and a successor can publish
// in the gap before the follow-up ownership read runs. That read then
// succeeds, but returns the SUCCESSOR's id, not this process's own — a
// mismatched id on a successful read is exactly as much proof of loss as
// ENOENT is, and must be treated the same way: retried, never returned as a
// successful acquisition. Simulated directly: the first heartbeat write
// inside beatOnce evicts this generation and publishes a successor as a
// side effect, then fails as if the write itself hit a transient error.
func TestAcquireFileLock_SynchronousBeatMismatchedFollowUpRead_DoesNotReturnLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	heartbeatPath := filepath.Join(lockPath, provisionLockHeartbeatFile)

	orig := writeLockFile
	fired := false
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if !fired && name == heartbeatPath {
			fired = true
			token, rerr := readOwnerToken(lockPath)
			require.NoError(t, rerr)
			require.NoError(t, os.Rename(lockPath, evictPathFor(lockPath, token)))
			_, cerr := tryCreateFileLock(lockPath) // a successor publishes in the gap
			require.NoError(t, cerr)
			return errors.New("simulated heartbeat write failure (e.g. ESTALE)")
		}
		return orig(name, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	held, err := acquireFileLock(ctx, dir)
	writeLockFile = orig
	if err == nil {
		_ = held.release()
	}

	require.Error(t, err, "a definite loss surfaced only via a mismatched follow-up read must never be returned as a successful acquisition")
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"must be retried as ordinary contention until the ctx deadline, since the successor stays fresh and live")
}

// TestAcquireFileLock_PublisherPauseGuard_RetriesInsteadOfFailing is the
// permanent regression test for acquireFileLock treating
// errPublishPausedTooLong as ordinary contention: it must retry within its
// normal wait budget, not fail the whole acquisition on the first trip of
// the publisher's own elapsed-time guard. Simulated by making the FIRST
// tryCreateFileLock attempt's own elapsed-time check trip (via timeNow) and
// the second attempt's check pass normally.
func TestAcquireFileLock_PublisherPauseGuard_RetriesInsteadOfFailing(t *testing.T) {
	withFastFileLockTiming(t)
	dir := t.TempDir()

	origNow := timeNow
	calls := 0
	timeNow = func() time.Time {
		calls++
		switch calls {
		case 1:
			return origNow() // first attempt's start
		case 2:
			return origNow().Add(provisionLockStaleAfter) // force that attempt's guard to trip
		default:
			return origNow() // second attempt: normal elapsed time
		}
	}
	t.Cleanup(func() { timeNow = origNow })

	held, err := acquireFileLock(context.Background(), dir)
	timeNow = origNow
	require.NoError(t, err, "a publisher-pause guard trip must be retried, not fail the whole acquisition")
	_ = held.release()
}

// TestAcquireFileLock_PublisherPauseGuard_RetryPacesAndRespectsCtx proves the
// errPublishPausedTooLong retry branch waits one tick in a ctx-aware select
// between attempts, exactly like plain contention: with every attempt
// tripping the guard, acquireFileLock must still return promptly once ctx is
// cancelled (not run out the full wait budget), and the number of attempts
// in the meantime must be paced by fileLockRetryDelay, not a busy retry with
// no wait at all.
func TestAcquireFileLock_PublisherPauseGuard_RetryPacesAndRespectsCtx(t *testing.T) {
	withFastFileLockTiming(t) // fileLockRetryDelay = 20ms
	dir := t.TempDir()

	origNow := timeNow
	var calls atomic.Int32
	timeNow = func() time.Time {
		// Each tryCreateFileLock attempt calls timeNow exactly twice: once
		// for its start, once for the elapsed check. Odd calls report the
		// real start; even calls report an hour later, so every attempt
		// trips the guard.
		if calls.Add(1)%2 == 1 {
			return origNow()
		}
		return origNow().Add(time.Hour)
	}
	t.Cleanup(func() { timeNow = origNow })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := acquireFileLock(ctx, dir)
	elapsed := time.Since(start)
	timeNow = origNow

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"a ctx-cancelled retry loop must surface the ctx error, not run out the full wait budget")
	assert.Less(t, elapsed, time.Second,
		"must return close to the 200ms ctx deadline, not after the full multi-minute wait budget: took %s", elapsed)

	attempts := calls.Load() / 2
	// At 20ms pacing over ~200ms, roughly 10 attempts are expected; a retry
	// with no wait between attempts would produce orders of magnitude more.
	assert.Less(t, attempts, int32(40),
		"publisher-trip retries=%d in ~200ms at 20ms pacing -- looks like a busy retry, not paced", attempts)
}

// TestAcquireFileLock_ReleaseCancelsLossContext proves
// a normal release unregisters heldLock.ctx from its parent, not just stops
// the heartbeat, so a long-lived parent context does not keep accumulating
// cancelled-but-still-registered children across many short-lived
// acquisitions. Observable directly: heldLock.ctx must report itself
// cancelled once release has returned.
func TestAcquireFileLock_ReleaseCancelsLossContext(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)

	require.Nil(t, held.ctx.Err(), "ctx must not be cancelled while the lock is still held")
	require.NoError(t, held.release())
	assert.ErrorIs(t, held.ctx.Err(), context.Canceled,
		"a normal release must cancel heldLock.ctx (via cancelLoss), not merely stop the heartbeat")
}

// TestTryReclaimIfAbandoned_ConcurrentCallers_ExactlyOneEvicts is a
// deterministic regression test: any
// number of concurrent callers contending to reclaim the SAME stale lock
// instance must produce exactly one eviction, never two. Mutual exclusion
// here rests entirely on renameFile's own atomicity (of any number of
// concurrent renames of the same source, exactly one succeeds) — there is
// no separate guard to get wrong.
func TestTryReclaimIfAbandoned_ConcurrentCallers_ExactlyOneEvicts(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	_, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	const n = 8
	var evictions atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tryReclaimIfAbandoned(dir, lockPath) {
				evictions.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), evictions.Load(), "exactly one concurrent caller should evict the stale lock")
	_, statErr := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(statErr), "the stale lock must be gone after eviction")

	// Exactly one ".evict-*" entry: the eviction is kept (not deleted) for
	// garbageCollectLockLitter to age out later — but
	// every LOSING attempt's failed rename must leave nothing of its own
	// behind (it never took the source, so there is nothing to clean up).
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var evictDirs int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".evict-") {
			evictDirs++
		}
	}
	assert.Equal(t, 1, evictDirs, "exactly one evicted-generation directory should remain, pending garbage collection")
}

// TestTryReclaimIfAbandoned_ConcurrentReclaimAndReacquire_SingleWinner goes
// one step further than the plain concurrent-eviction test: each goroutine
// that successfully evicts the stale lock
// immediately contends to create the fresh replacement too, so this exercises
// both the eviction interleaving AND the re-acquisition interleaving together.
func TestTryReclaimIfAbandoned_ConcurrentReclaimAndReacquire_SingleWinner(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	_, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	const n = 8
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !tryReclaimIfAbandoned(dir, lockPath) {
				return
			}
			if _, err := tryCreateFileLock(lockPath); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), winners.Load(), "exactly one goroutine should have both evicted the stale lock and won the contention to create its replacement")
	assert.DirExists(t, lockPath)
}

// TestAcquireFileLock_ReclaimInterleavedWithEvictAndRepublish_NoVacancy
// is the permanent regression test proving
// reclaim's own eviction rename never relocates whatever CURRENTLY occupies
// lockPath — including a live generation a concurrent publish just
// placed there — when that generation is not the one it judged abandoned:
// the destination is derived deterministically from the generation being
// removed, never a fresh name per attempt.
//
// Sequence: L1 is stale. Waiter B reads L1's owner id and judges it
// abandoned (both non-destructive, no rename yet). Before B's own eviction
// rename reaches the filesystem, waiter A independently reclaims L1 (to
// L1's own deterministic destination) and publishes a fresh, live L2. B's
// rename then executes with the exact source/destination it decided on
// before A ran — L1's destination, not L2's — so even though its source is
// still named lockPath, by the time it runs lockPath no longer holds L1 at
// all (A's publish replaced it) and A's own reclaim already took L1's
// destination name, so B's rename fails outright instead of sweeping up A's
// live L2.
func TestAcquireFileLock_ReclaimInterleavedWithEvictAndRepublish_NoVacancy(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	tokenL1, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	var heldA heldLock
	var errA error
	fired := false
	orig := renameFile
	renameFile = func(src, dst string) error {
		if !fired && src == lockPath && dst == evictPathFor(lockPath, tokenL1) {
			fired = true
			// B has judged L1 abandoned and is about to evict it, but a
			// full acquire-reclaim-republish cycle from A runs to
			// completion first. A's own reclaim call recurses into this
			// same hook with fired already true, so it falls straight
			// through to the real rename below.
			heldA, errA = acquireFileLock(context.Background(), dir)
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { renameFile = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	heldB, errB := acquireFileLock(ctx, dir)
	if errB == nil {
		t.Cleanup(func() { _ = heldB.release() })
	}

	require.NoError(t, errA, "A must acquire the lock A itself reclaimed and published")
	t.Cleanup(func() { _ = heldA.release() })

	assert.Error(t, errB, "B must not also succeed in acquiring the lock A already holds")
	assert.Nil(t, heldA.ctx.Err(), "A must not be told it lost a lock it legitimately holds")
	assert.True(t, heldA.stillOwned(), "A's lock must survive B's stale-L1 eviction attempt")
	assert.DirExists(t, lockPath, "lockPath must not have been left vacant")
}

// TestAcquireFileLock_ReclaimOfLongHeldGeneration_SurvivesConcurrentGC proves
// garbageCollectLockLitter ages an evict entry by the moment it was actually
// evicted, never by the evicted directory's own mtime (its creation/
// first-heartbeat time): for a generation held longer than roughly
// 2x the stale window before its holder crashed, the latter would make its
// evict entry already look older than the GC cutoff the instant it was
// evicted — a
// concurrent GC pass (every acquireFileLock call runs one) could then sweep
// it immediately, re-opening its destination name for a still-in-flight
// stale reclaimer to relocate a live successor published in the meantime
// instead of hitting the expected destination collision.
//
// Sequence: L1 was held for a long time before crashing. Waiter C reads L1's
// owner id and judges it abandoned (non-destructive, no rename yet).
// Before C's own eviction rename reaches the filesystem: waiter B
// independently reclaims L1 (to L1's own deterministic destination,
// recursing into this same hook, which falls straight through since fired
// is already true) and publishes a fresh, live L2; waiter E then starts an
// unrelated acquireFileLock, whose own GC pass runs over this directory.
// C's rename then executes. Property: B's live L2 is not displaced, and C
// does not report a (false) successful eviction.
func TestAcquireFileLock_ReclaimOfLongHeldGeneration_SurvivesConcurrentGC(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	tokenL1, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	hbPath := filepath.Join(lockPath, provisionLockHeartbeatFile)
	require.NoError(t, os.WriteFile(hbPath, []byte(tokenL1), 0600))
	longHeld := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(hbPath, longHeld, longHeld))
	require.NoError(t, os.Chtimes(lockPath, longHeld.Add(-time.Hour), longHeld.Add(-time.Hour)))

	var heldB heldLock
	var errB error
	fired := false
	orig := renameFile
	renameFile = func(src, dst string) error {
		if !fired && src == lockPath && dst == evictPathFor(lockPath, tokenL1) {
			fired = true
			heldB, errB = acquireFileLock(context.Background(), dir)
			ectx, ecancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			if e, eerr := acquireFileLock(ectx, dir); eerr == nil { // E: GC, then gives up
				_ = e.release()
			}
			ecancel()
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { renameFile = orig })

	cEvicted := tryReclaimIfAbandoned(dir, lockPath) // C
	renameFile = orig

	require.NoError(t, errB, "B must acquire the lock B itself reclaimed and published")
	t.Cleanup(func() { _ = heldB.release() })

	assert.False(t, cEvicted, "C must not report a successful eviction when a concurrent GC pass let it displace a live successor instead")
	assert.Nil(t, heldB.ctx.Err(), "B must not be told it lost a lock it legitimately holds")
	assert.True(t, heldB.stillOwned(), "B's lock must survive C's reclaim attempt even though an unrelated GC pass ran in between")
}

// TestTryReclaimIfAbandoned_RenameLandsOnLiveSuccessor_ReportsNoEviction is
// the only coverage for the post-rename defensive check inside
// tryReclaimIfAbandoned. It directly forces the narrow case the check exists for — the
// rename succeeds, but what it actually moved is a live, different
// generation, not the one judged stale — by vacating the destination name
// directly (standing in for whatever external event, such as garbage
// collection, could do so) rather than relying on real GC timing, which
// TestAcquireFileLock_ReclaimOfLongHeldGeneration_SurvivesConcurrentGC
// already exercises end to end (and where, now that GC is fixed, the rename
// itself fails before ever reaching this check — this test isolates the
// check on its own so it keeps being exercised regardless).
func TestTryReclaimIfAbandoned_RenameLandsOnLiveSuccessor_ReportsNoEviction(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	tokenL1, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	var tokenL2 string
	orig := renameFile
	fired := false
	renameFile = func(src, dst string) error {
		if !fired && src == lockPath && dst == evictPathFor(lockPath, tokenL1) {
			fired = true
			// Between C's non-destructive staleness read and its rename:
			// another actor evicts L1 and publishes a live L2 (recursing
			// into this same hook, which falls straight through since fired
			// is already true), and L1's own evict destination is then
			// vacated again — standing in for an external event such as
			// garbage collection — before C's rename actually runs.
			require.True(t, tryReclaimIfAbandoned(dir, lockPath))
			var err2 error
			tokenL2, err2 = tryCreateFileLock(lockPath)
			require.NoError(t, err2)
			require.NoError(t, os.RemoveAll(evictPathFor(lockPath, tokenL1)))
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { renameFile = orig })

	evicted := tryReclaimIfAbandoned(dir, lockPath) // C
	renameFile = orig

	assert.False(t, evicted, "must not report a successful eviction when the rename actually displaced a live, different generation")
	require.NotEmpty(t, tokenL2)
	assert.True(t, stillOwnsLockFile(evictPathFor(lockPath, tokenL1), tokenL2),
		"the displaced live generation must be left exactly where it landed, not deleted")
}

// TestTryReclaimIfAbandoned_SequentialInterleaving_DoesNotEvictLiveSuccessor
// reproduces a delayed-decision interleaving directly (not
// via goroutine timing): B reclaims and re-acquires first, producing a live
// L2. A later reclaim attempt re-reads the CURRENT owner id from lockPath
// (L2's, not L1's) and judges freshness on THAT id — since L2 is fresh, no
// rename is even attempted here. This is a narrower case than the
// concurrent-rename interleavings covered elsewhere (see acquireFileLock's
// correctness argument and its bounded-holder-pause assumption): it only
// shows that a SEQUENTIAL re-read, with no rename contending with it, always
// sees the current occupant rather than a cached one.
func TestTryReclaimIfAbandoned_SequentialInterleaving_DoesNotEvictLiveSuccessor(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	_, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	require.True(t, tryReclaimIfAbandoned(dir, lockPath), "B evicts the stale L1")
	tokenL2, err := tryCreateFileLock(lockPath)
	require.NoError(t, err, "B acquires a fresh, live L2")

	evicted := tryReclaimIfAbandoned(dir, lockPath)
	assert.False(t, evicted, "must not evict B's live L2")

	current, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	require.NoError(t, err, "B's lock must still be present and intact")
	assert.Equal(t, tokenL2, string(current))
}

// TestLockLooksAbandoned_IgnoresLockDirMtimeWhenHeartbeatFileExists is the
// concrete mechanism test proving staleness is judged from two
// NFS-server-assigned timestamps, never this process's own
// clock mixed with a value an explicit os.Chtimes call embedded. The lock
// directory's own mtime here is deliberately backdated — exactly what an
// explicit-time Chtimes call (an earlier, client-clock-based heartbeat
// mechanism) would have
// produced — to prove that's no longer what staleness is judged against
// once a real heartbeat marker exists.
func TestLockLooksAbandoned_IgnoresLockDirMtimeWhenHeartbeatFileExists(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	token, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	require.Equal(t, beatOK, beatOnce(lockPath, token), "a real, fresh heartbeat beat")

	old := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	assert.False(t, lockLooksAbandoned(dir, lockPath),
		"a lock with a fresh heartbeat marker must not be judged abandoned, regardless of its own directory's mtime")
}

// TestLockLooksAbandoned_MarkerlessLock_FallsBackToLockDirMtime covers the
// "crashed before the first beat" / NFS-MKDIR-retransmit case: with no
// heartbeat file at all, lastHeartbeat falls back to the lock directory's
// own (still server-assigned, from its Mkdir) mtime.
func TestLockLooksAbandoned_MarkerlessLock_FallsBackToLockDirMtime(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	require.NoError(t, os.Mkdir(lockPath, 0700)) // no owner/heartbeat file at all

	old := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	assert.True(t, lockLooksAbandoned(dir, lockPath),
		"a markerless lock (never beat) should fall back to its own mtime for staleness")
}

// TestLockLooksAbandoned_HeartbeatStatError_DoesNotFallBackToOldDirMtime
// proves lastHeartbeat falls back to the lock directory's own (old,
// first-beat) mtime ONLY when the heartbeat file does not exist, never on
// any other stat error. A transient non-ENOENT error (EIO/ETIMEDOUT on a soft mount,
// EACCES, ESTALE after a server failover — ELOOP here as a portable,
// hermetically-triggerable stand-in via a self-referential symlink) must
// not be treated as "never beaten": a live holder that has been beating
// normally, but whose heartbeat file happens to be momentarily unreadable
// for an unrelated reason, must not look abandoned by its own old
// directory mtime.
func TestLockLooksAbandoned_HeartbeatStatError_DoesNotFallBackToOldDirMtime(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	_, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	hb := filepath.Join(lockPath, provisionLockHeartbeatFile)
	require.NoError(t, os.Symlink(hb, hb)) // a symlink to itself: stat fails with ELOOP
	_, statErr := os.Stat(hb)
	require.Error(t, statErr)
	require.False(t, os.IsNotExist(statErr), "setup: this must be a non-ENOENT error")

	old := time.Now().Add(-10 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	assert.False(t, lockLooksAbandoned(dir, lockPath),
		"a non-ENOENT heartbeat stat error must not fall back to the old directory mtime")
}

// TestServerNow_ConcurrentCallersDoNotInterfere proves each serverNow call
// uses its own probe file name under the shared prefix, so concurrent
// callers cannot interfere with each other: if every call shared one fixed
// name instead, caller Y's write, stat and remove of that same name
// could land entirely between caller X's own write and its stat, making X's
// stat fail with ENOENT for a reason that has nothing to do with actual
// staleness. With several waiters polling the same tick against a crashed
// holder, this would knock every one of them out of that tick's reclaim
// attempt at once.
func TestServerNow_ConcurrentCallersDoNotInterfere(t *testing.T) {
	dir := t.TempDir()
	orig := writeLockFile
	fired := false
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		err := orig(name, data, perm)
		if !fired && strings.HasPrefix(filepath.Base(name), provisionClockProbeFile) {
			fired = true
			// Another caller's full serverNow call, including its own
			// write, stat and remove, interleaved right after this call's
			// own write.
			_, _ = serverNow(dir)
		}
		return err
	}
	t.Cleanup(func() { writeLockFile = orig })

	_, err := serverNow(dir)
	writeLockFile = orig
	assert.NoError(t, err, "a concurrent caller's own probe write/stat/remove must not interfere with this call's")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "clock-probe", "no clock-probe entry should remain after serverNow: %s", e.Name())
	}
}

// TestServerNow_WriteErrorStillRemovesProbe proves the probe file is removed
// via a defer that runs regardless of outcome, even when a write error
// occurs AFTER the probe file was already created (e.g. ENOSPC/EDQUOT on the
// data write, or any other short-write failure).
func TestServerNow_WriteErrorStillRemovesProbe(t *testing.T) {
	dir := t.TempDir()
	orig := writeLockFile
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if strings.HasPrefix(filepath.Base(name), provisionClockProbeFile) {
			_ = orig(name, nil, perm) // the file gets created before the simulated failure
			return errors.New("simulated short write")
		}
		return orig(name, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	_, err := serverNow(dir)
	writeLockFile = orig

	require.Error(t, err)
	entries, rdErr := os.ReadDir(dir)
	require.NoError(t, rdErr)
	assert.Empty(t, entries, "a probe created before a write error must still be removed")
}

// TestAcquireFileLock_HeartbeatPreventsReclaimOfLiveHolder is the regression
// test proving the heartbeat's purpose: without a heartbeat, a holder whose
// provisioning simply takes longer than provisionLockStaleAfter looks
// exactly like a crashed holder and gets reclaimed — letting a second
// provisioner run concurrently, the corruption this whole mechanism exists
// to prevent. With the heartbeat, the marker's mtime keeps refreshing for as
// long as the holder is alive, so a waiter must not be able to reclaim it
// even after waiting past the (test-scale) stale threshold.
func TestAcquireFileLock_HeartbeatPreventsReclaimOfLiveHolder(t *testing.T) {
	// Shrink both the stale threshold and the heartbeat interval (keeping
	// the same ~1:6 ratio the production defaults use) so this test can
	// actually wait PAST the stale threshold without a multi-minute sleep,
	// while still proving the heartbeat keeps refreshing often enough
	// relative to it that a live holder never looks abandoned.
	origStale, origHeartbeat, origDelay := provisionLockStaleAfter, provisionLockHeartbeatInterval, fileLockRetryDelay
	provisionLockStaleAfter = 180 * time.Millisecond
	provisionLockHeartbeatInterval = 30 * time.Millisecond
	fileLockRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		provisionLockStaleAfter, provisionLockHeartbeatInterval, fileLockRetryDelay = origStale, origHeartbeat, origDelay
	})

	dir := t.TempDir()

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	heartbeatPath := filepath.Join(dir, provisionFileLockName, provisionLockHeartbeatFile)
	require.Eventually(t, func() bool {
		_, err := os.Stat(heartbeatPath)
		return err == nil
	}, time.Second, 5*time.Millisecond, "heartbeat marker should appear after the first beat")

	initial, err := os.Stat(heartbeatPath)
	require.NoError(t, err)
	time.Sleep(provisionLockHeartbeatInterval*2 + 50*time.Millisecond)
	refreshed, err := os.Stat(heartbeatPath)
	require.NoError(t, err)
	assert.True(t, refreshed.ModTime().After(initial.ModTime()),
		"heartbeat should have refreshed its marker's mtime while held")

	// Wait well past the (shrunk) stale threshold — long enough that,
	// without a heartbeat, this lock would now look abandoned — then confirm
	// a waiter still cannot acquire it: it looks fresh, not stale.
	time.Sleep(provisionLockStaleAfter * 2)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = acquireFileLock(ctx, dir)
	require.Error(t, err, "a live, heartbeating holder must never be reclaimed")
}

// TestAcquireFileLock_HeartbeatDetectsLossAndCancelsCtx is the regression
// test proving a holder must detect losing its own
// lock (not simply assume it holds it forever once acquired), cancel the
// ctx it handed back at acquisition so in-flight provisioning work is
// killed, and its own release() must become a safe no-op rather than touch
// whatever a successor now has there.
//
// The loss is forced directly (RemoveAll + a fresh acquisition) rather than
// via the staleness path, to avoid contending with this holder's own live
// heartbeat goroutine, which would otherwise keep re-refreshing the marker
// and could undo a staleness-based setup before the test's own reclaim call
// runs.
func TestAcquireFileLock_HeartbeatDetectsLossAndCancelsCtx(t *testing.T) {
	origHeartbeat := provisionLockHeartbeatInterval
	provisionLockHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { provisionLockHeartbeatInterval = origHeartbeat })

	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	// Belt-and-suspenders: stop the heartbeat goroutine on any exit path,
	// including a t.Fatal below, not just the successful one — an
	// unstopped heartbeat goroutine would otherwise outlive this test and
	// could run concurrently with a LATER test's package-var reassignment
	// (release()/stop() blocks until the goroutine has actually exited; see
	// its doc).
	t.Cleanup(func() { _ = held.release() })

	lockPath := filepath.Join(dir, provisionFileLockName)
	require.NoError(t, os.RemoveAll(lockPath)) // force-evict, simulating an unexpected loss
	_, err = tryCreateFileLock(lockPath)       // a successor acquires
	require.NoError(t, err)

	select {
	case <-held.ctx.Done():
		// good: the heartbeat noticed the owner-id mismatch and cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not detect lock loss and cancel ctx in time")
	}

	require.NoError(t, held.release(), "release after detected loss must be a safe no-op")
	// The successor's lock must be untouched.
	assert.DirExists(t, lockPath)
}

// TestAcquireFileLock_DefiniteLossIsNotToleratedAsTransientMiss proves a
// definite owner-id mismatch is handled immediately, never like a transient
// miss (incrementing the miss counter instead of cancelling right away): the
// existing TestAcquireFileLock_HeartbeatDetectsLossAndCancelsCtx above only
// asserts detection happens SOMEWHERE within a generous 2-second bound,
// which a 3-miss-tolerance detour (well under 2s at this test's interval)
// would also satisfy. This instead measures elapsed time and requires
// detection clearly faster than provisionHeartbeatMissTolerance misses would
// take, distinguishing "immediate" from "tolerated like any other miss".
func TestAcquireFileLock_DefiniteLossIsNotToleratedAsTransientMiss(t *testing.T) {
	origHeartbeat := provisionLockHeartbeatInterval
	// 250ms, not 50ms: the distinguishing margin below needs real slack over
	// scheduler jitter on a loaded host, not just over the nominal interval
	// -- 50ms left only 50ms of slack between "immediate" and "tolerated".
	provisionLockHeartbeatInterval = 250 * time.Millisecond
	t.Cleanup(func() { provisionLockHeartbeatInterval = origHeartbeat })

	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.release() })

	lockPath := filepath.Join(dir, provisionFileLockName)
	require.NoError(t, os.RemoveAll(lockPath))
	_, err = tryCreateFileLock(lockPath) // a successor acquires

	require.NoError(t, err)
	start := time.Now()
	select {
	case <-held.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not detect lock loss and cancel ctx in time")
	}
	elapsed := time.Since(start)

	// provisionHeartbeatMissTolerance misses at this interval would take at
	// least 1 interval to even register the first miss, plus 2 more full
	// intervals before the tolerance is exhausted -- comfortably over the
	// threshold below, with real margin against scheduler jitter. A
	// definite, un-tolerated loss is detected on the very next beat instead.
	assert.Less(t, elapsed, 2*provisionLockHeartbeatInterval,
		"definite loss took %s to detect -- looks tolerated like a transient miss, not immediate", elapsed)
}

// TestAcquireFileLock_LostHolderHeartbeat_DoesNotWriteIntoSuccessor proves
// beatOnce reads the current owner BEFORE ever writing its marker: a holder
// that has already lost the lock must not refresh a successor's heartbeat
// marker. beatOnce
// reports a definite loss — never merely a tolerated miss — without writing
// anything if the owner does not match.
func TestAcquireFileLock_LostHolderHeartbeat_DoesNotWriteIntoSuccessor(t *testing.T) {
	origHeartbeat := provisionLockHeartbeatInterval
	provisionLockHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { provisionLockHeartbeatInterval = origHeartbeat })

	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	h, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = h.release() }()

	// Simulate this holder being silently superseded: its lock directory is
	// removed out from under it and a successor publishes a fresh one at the
	// same path, all before this holder's heartbeat has had a chance to
	// notice.
	require.NoError(t, os.RemoveAll(lockPath))
	_, err = tryCreateFileLock(lockPath)
	require.NoError(t, err)

	select {
	case <-h.ctx.Done():
		// good: the heartbeat detected the mismatch and cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not detect lock loss and cancel ctx in time")
	}

	_, statErr := os.Stat(filepath.Join(lockPath, provisionLockHeartbeatFile))
	assert.True(t, os.IsNotExist(statErr), "lost holder wrote a heartbeat marker into its successor's lock")
}

// TestBeatOnce_CheckThenWrite_PostWriteCheckDetectsSuccessor is the only
// coverage for beatOnce's POST-write ownership check, since
// TestAcquireFileLock_LostHolderHeartbeat_DoesNotWriteIntoSuccessor above
// only exercises the case where the loss happens before any beat is even
// attempted. This instead forces the one narrow window beatOnce's doc
// acknowledges is not fully closed: the pre-write ownership check passes
// (this generation is still genuinely current), but between that check and
// the write actually landing, this exact generation is evicted and a
// successor publishes — so the write lands in the successor's directory.
// Property: beatOnce still reports beatDefiniteLoss (via its post-write
// re-check), it just cannot prevent that one write from having happened.
func TestBeatOnce_CheckThenWrite_PostWriteCheckDetectsSuccessor(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	tH, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)

	orig := writeLockFile
	fired := false
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if !fired && filepath.Base(name) == provisionLockHeartbeatFile {
			fired = true
			// The pre-write check already passed; before the write lands,
			// this generation is evicted and a successor publishes.
			require.NoError(t, renameFile(lockPath, evictPathFor(lockPath, tH)))
			_, err := tryCreateFileLock(lockPath)
			require.NoError(t, err)
		}
		return orig(name, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	res := beatOnce(lockPath, tH)
	writeLockFile = orig

	assert.Equal(t, beatDefiniteLoss, res,
		"a beat whose write landed in a successor's directory must still be detected as definite loss via the post-write check")
}

// TestAcquireFileLock_NonEvictingContentionStillSleeps is the regression
// test proving that waiting against a
// contended-but-not-abandoned lock must pace itself via the retry delay,
// never spin through the wait budget in milliseconds. Every non-evicting
// outcome of tryReclaimIfAbandoned (here: every attempt reads the lock,
// finds it fresh, and returns without ever renaming anything) falls through
// to the same sleep-or-ctx select as plain contention.
func TestAcquireFileLock_NonEvictingContentionStillSleeps(t *testing.T) {
	withFastFileLockTiming(t)
	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err) // fresh, live lock -- never evictable
	defer func() { _ = held.release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = acquireFileLock(ctx, dir)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
		"waiting against a live, non-abandoned lock must sleep between attempts, not spin: took only %s", elapsed)
}

// TestAcquireFileLock_NonEvictingContentionStillSleeps_BoundsClaimAttempts
// closes a gap the elapsed-time check in
// TestAcquireFileLock_NonEvictingContentionStillSleeps cannot: it cannot tell a properly
// paced wait from a busy spin that keeps running past the ctx deadline
// anyway — a spin still takes at least as long as the ctx timeout, it just
// also issues far more claim attempts along the way (each one an extra NFS
// round trip). This counts tryCreateFileLock's publish rename
// (renameFile(tmpDir, lockPath)) directly and asserts an upper bound
// consistent with pacing at fileLockRetryDelay, not spinning.
func TestAcquireFileLock_NonEvictingContentionStillSleeps_BoundsClaimAttempts(t *testing.T) {
	withFastFileLockTiming(t) // fileLockRetryDelay = 20ms
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err) // fresh, live lock -- never evictable
	defer func() { _ = held.release() }()

	var claims atomic.Int32
	orig := renameFile
	renameFile = func(src, dst string) error {
		if dst == lockPath {
			claims.Add(1)
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { renameFile = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = acquireFileLock(ctx, dir)
	require.Error(t, err)

	// At 20ms pacing over ~300ms, roughly 15 claim attempts are expected; a
	// busy spin would produce orders of magnitude more in the same window.
	assert.Less(t, int(claims.Load()), 40,
		"claim attempts=%d in 300ms at 20ms pacing -- looks like a busy spin, not paced retries", claims.Load())
}

// TestAcquireFileLock_DeadlineIsWallClock proves the wait budget is a
// wall-clock deadline computed once, not an
// attempt counter that non-sleeping outcomes could exhaust in milliseconds:
// with no ctx deadline of its own, acquireFileLock must still give up once
// fileLockRetries*fileLockRetryDelay has elapsed, no sooner and not much later.
func TestAcquireFileLock_DeadlineIsWallClock(t *testing.T) {
	withFastFileLockTiming(t)
	origRetries := fileLockRetries
	fileLockRetries = 5 // 5 * 20ms = ~100ms deadline
	t.Cleanup(func() { fileLockRetries = origRetries })

	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	start := time.Now()
	_, err = acquireFileLock(context.Background(), dir) // no ctx deadline -- the budget alone must bound this
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "should not give up before the deadline elapses")
	assert.Less(t, elapsed, 2*time.Second, "should give up once the wall-clock deadline passes, not hang")
}

// TestAcquireFileLock_MarkerlessLock_SelfHeals is the regression test
// proving that a lock directory with no owner file at all (see
// markerlessOwnerToken's doc for how this can arise despite a published
// lock always having one) must still self-heal
// within the normal wait budget once it looks old enough, exactly like a
// fully-formed stale lock — lastHeartbeat's fallback to the lock
// directory's own mtime (see its doc) is what makes this possible without
// any special-casing elsewhere.
func TestAcquireFileLock_MarkerlessLock_SelfHeals(t *testing.T) {
	withFastFileLockTiming(t)
	origStale := provisionLockStaleAfter
	provisionLockStaleAfter = 100 * time.Millisecond
	t.Cleanup(func() { provisionLockStaleAfter = origStale })

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, provisionFileLockName), 0700)) // no owner file at all
	old := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(filepath.Join(dir, provisionFileLockName), old, old))

	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	require.NoError(t, held.release())
}

// TestTryReclaimIfAbandoned_NonHexOwnerContent_TreatedAsMarkerless is the
// permanent regression test proving an owner file's
// content is never used verbatim as a path segment in the evict destination.
// A stray or truncated owner file containing "/" would otherwise turn the
// intended sibling path lockPath+".evict-"+content into a path inside a
// different, likely non-existent, directory, so the rename meant to evict it
// would fail with ENOENT forever and the lock could never be reclaimed.
// isHexLockToken rejects any content that doesn't look like a real owner id,
// falling back to the markerless path instead.
func TestTryReclaimIfAbandoned_NonHexOwnerContent_TreatedAsMarkerless(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte("x/y"), 0600))
	old := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	assert.True(t, tryReclaimIfAbandoned(dir, lockPath),
		"a lock whose owner content is not a valid owner id must still be reclaimable once stale")
}

// TestTryReclaimIfAbandoned_SecondMarkerlessLock_NotStuckBehindFirst proves
// markerlessEvictSuffix disambiguates successive markerless generations
// using the lock directory's own mtime, read at the same non-destructive
// moment as the staleness judgment: without a disambiguator, every
// markerless generation would share the single literal evict destination
// ".evict-(none)", so after one markerless eviction, a second, later,
// genuinely different markerless lock at the same path would collide with
// the first's still-present evict entry (EEXIST) and be unreclaimable for
// up to the full garbage-collection window — longer than the wait budget.
func TestTryReclaimIfAbandoned_SecondMarkerlessLock_NotStuckBehindFirst(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	old := time.Now().Add(-2 * provisionLockStaleAfter)

	// First markerless generation, evicted and left behind (not yet
	// garbage-collected).
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.Chtimes(lockPath, old.Add(-time.Minute), old.Add(-time.Minute)))
	require.True(t, tryReclaimIfAbandoned(dir, lockPath), "setup: evict the first markerless generation")

	// A second, later, genuinely different markerless generation at the
	// same path.
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.Chtimes(lockPath, old, old))

	assert.True(t, tryReclaimIfAbandoned(dir, lockPath),
		"a second markerless lock must not be stuck behind the first's still-present evict entry")
}

// TestTryReclaimIfAbandoned_VacantLockPath_ReturnsFalseWithoutActing proves
// tryReclaimIfAbandoned distinguishes the two different situations
// readOwnerToken's ENOENT can mean: the owner file is missing (a real
// markerless lock directory IS there) and lockPath itself does not exist at
// all. Treating the second case as markerless too computes the shared
// fallback destination (".evict-(none)") from nothing — if a publisher then
// appears (with an old directory mtime, e.g. a publisher pause) before the
// staleness check runs, it gets evicted to that shared, generation-less
// destination and the defensive mismatch check leaves lockPath vacant
// instead of occupied by the live publisher. tryReclaimIfAbandoned now
// Lstats lockPath on ENOENT and returns false immediately if it is actually
// absent, rather than computing a markerless suffix for nothing — and, as a
// direct consequence, never even reaches the staleness check (serverNow),
// which this test confirms by asserting serverNow's own clock-probe write
// never happens: there is no window left in which a publisher could appear.
func TestTryReclaimIfAbandoned_VacantLockPath_ReturnsFalseWithoutActing(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)

	orig := writeLockFile
	probed := false
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if strings.HasPrefix(filepath.Base(name), provisionClockProbeFile) {
			probed = true
		}
		return orig(name, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	evicted := tryReclaimIfAbandoned(dir, lockPath)
	writeLockFile = orig

	assert.False(t, evicted, "nothing exists at lockPath; there is nothing to reclaim")
	assert.False(t, probed, "a vacant lockPath must short-circuit before ever judging staleness, leaving no window for a publisher to appear in")
	assert.NoDirExists(t, evictPathFor(lockPath, markerlessOwnerToken),
		"a vacant lockPath must never produce a markerless evict entry bound to no generation")
}

// TestGarbageCollectLockLitter_RemovesOldEntriesKeepsFreshAndLive proves GC
// ages an evict entry by the moment it was actually evicted, never by the
// EVICTED DIRECTORY'S OWN mtime (its creation/first-beat time). A directory
// built with a bare os.Mkdir right before the GC call always
// has a fresh mtime and so cannot distinguish the two: this test instead
// produces its "just evicted" entry by actually evicting a generation that
// was held for a long time before going stale (a real crashed-holder
// scenario), so its OWN directory mtime is old even though it was moved
// into its evict destination moments ago — exactly the case that must still
// survive an immediate GC pass. Its "old enough to sweep" entry is produced
// by a real eviction too, with only the eviction marker itself back-dated,
// to isolate "old enough since eviction" from "old since creation".
func TestGarbageCollectLockLitter_RemovesOldEntriesKeepsFreshAndLive(t *testing.T) {
	dir := t.TempDir()
	origStale := provisionLockStaleAfter
	provisionLockStaleAfter = 50 * time.Millisecond
	t.Cleanup(func() { provisionLockStaleAfter = origStale })

	livePath := filepath.Join(dir, provisionFileLockName)
	longAgo := time.Now().Add(-time.Hour)

	// A generation held for a long time before its holder crashed: its own
	// directory mtime (first-beat time) is far in the past. Evicting it
	// must still leave an evict entry that survives an IMMEDIATE GC pass —
	// the whole point of aging from the eviction marker, not the directory.
	oldHeldToken, err := tryCreateFileLock(livePath)
	require.NoError(t, err)
	hbPath := filepath.Join(livePath, provisionLockHeartbeatFile)
	require.NoError(t, os.WriteFile(hbPath, []byte(oldHeldToken), 0600))
	require.NoError(t, os.Chtimes(hbPath, longAgo, longAgo))
	require.NoError(t, os.Chtimes(livePath, longAgo.Add(-time.Hour), longAgo.Add(-time.Hour)))
	require.True(t, tryReclaimIfAbandoned(dir, livePath), "setup: evict the long-held generation")
	justEvicted := evictPathFor(livePath, oldHeldToken)
	require.DirExists(t, justEvicted, "setup: eviction must have produced an evict entry")

	// A second generation, released normally, whose eviction marker is then
	// back-dated directly: old enough since EVICTION, regardless of how
	// recently it was actually created.
	trulyOldToken, err := tryCreateFileLock(livePath)
	require.NoError(t, err)
	require.NoError(t, releaseFileLock(livePath, trulyOldToken)())
	trulyOldEvict := evictPathFor(livePath, trulyOldToken)
	require.DirExists(t, trulyOldEvict, "setup: release must have produced an evict entry")
	require.NoError(t, os.Chtimes(filepath.Join(trulyOldEvict, provisionLockEvictedAtFile), longAgo, longAgo))

	// The live lock itself must never be touched, however old it looks.
	_, err = tryCreateFileLock(livePath)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(livePath, longAgo, longAgo))

	// An unrelated directory, not under provisionFileLockName's prefix at
	// all, must never be touched regardless of age.
	unrelated := filepath.Join(dir, "not-ours")
	require.NoError(t, os.Mkdir(unrelated, 0700))
	require.NoError(t, os.Chtimes(unrelated, longAgo, longAgo))

	garbageCollectLockLitter(dir)

	assert.DirExists(t, livePath, "the live lock itself must never be swept")
	assert.DirExists(t, justEvicted, "a just-evicted long-held generation must survive immediate GC")
	assert.NoDirExists(t, trulyOldEvict, "an evict entry whose eviction marker is old enough must be swept")
	assert.DirExists(t, unrelated, "an unrelated directory must never be touched")
}

// TestGarbageCollectLockLitter_SweepsOldReclaimedEntry isolates GC of an
// entry evicted by a reclaimer (as opposed to the main GC test above, whose
// "old enough to sweep" entry comes from releaseFileLock): an entry evicted
// by a reclaimer, with its eviction marker back-dated, must also be swept
// once old enough.
func TestGarbageCollectLockLitter_SweepsOldReclaimedEntry(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	origStale := provisionLockStaleAfter
	provisionLockStaleAfter = 50 * time.Millisecond
	t.Cleanup(func() { provisionLockStaleAfter = origStale })

	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	tok, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))
	require.True(t, tryReclaimIfAbandoned(dir, lockPath), "setup: reclaim must evict and write the marker")

	evictPath := evictPathFor(lockPath, tok)
	require.DirExists(t, evictPath, "setup: reclaim must have produced an evict entry")
	longAgo := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(evictPath, provisionLockEvictedAtFile), longAgo, longAgo))

	garbageCollectLockLitter(dir)

	assert.NoDirExists(t, evictPath, "a reclaimed entry whose eviction marker is old enough must be swept, the same as a released one")
}

// TestGarbageCollectLockLitter_CutoffIsTwoStaleWindows brackets the
// garbage-collection cutoff to within +/-10% of 2x the stale window, not
// merely "some cutoff large enough to keep a just-evicted entry and sweep
// an hour-old one" the way the two tests above do: an entry whose eviction
// marker is 1.9x the stale window old sits inside the worst-case window a
// late remover of that generation could still be acting in (see
// acquireFileLock's doc, "Why the garbage-collection cutoff is 2S") and so
// must survive, while an entry at 2.1x sits outside it and must be swept.
// Runs against the PRODUCTION stale window (not shrunk) and only ever
// back-dates via os.Chtimes, never sleeps, so the margins (18s on a 180s
// window) cannot be crossed by scheduling delay between setup and the GC
// call, unlike a shrunk window with a fixed millisecond margin would be.
func TestGarbageCollectLockLitter_CutoffIsTwoStaleWindows(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	stale := provisionLockStaleAfter // production value; never shrunk in this test

	survivorTok, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, releaseFileLock(lockPath, survivorTok)())
	survivorEvict := evictPathFor(lockPath, survivorTok)
	require.DirExists(t, survivorEvict, "setup: release must have produced an evict entry")
	survivorAge := time.Now().Add(-19 * stale / 10) // 1.9S: inside the 2S cutoff
	require.NoError(t, os.Chtimes(filepath.Join(survivorEvict, provisionLockEvictedAtFile), survivorAge, survivorAge))

	sweptTok, err := tryCreateFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, releaseFileLock(lockPath, sweptTok)())
	sweptEvict := evictPathFor(lockPath, sweptTok)
	require.DirExists(t, sweptEvict, "setup: release must have produced an evict entry")
	sweptAge := time.Now().Add(-21 * stale / 10) // 2.1S: just past the 2S cutoff
	require.NoError(t, os.Chtimes(filepath.Join(sweptEvict, provisionLockEvictedAtFile), sweptAge, sweptAge))

	garbageCollectLockLitter(dir)

	assert.DirExists(t, survivorEvict, "an entry at 1.9x the stale window must still be kept: it is inside the 2S cutoff")
	assert.NoDirExists(t, sweptEvict, "an entry just past 2x the stale window must be swept")
}

// TestGarbageCollectLockLitter_EvictedAtWriteFailed_EntryNeverSwept proves a
// failed evicted-at write (reclaim or release) makes GC keep that entry
// indefinitely (never guessing at an unconfirmed eviction time), not merely
// "for a while": even with an arbitrarily small cutoff, an entry with no
// marker at all must never be collected.
func TestGarbageCollectLockLitter_EvictedAtWriteFailed_EntryNeverSwept(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	tok := makeLongHeldCrashedLock(t, lockPath, 10*time.Minute)

	origWrite := writeLockFile
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if filepath.Base(name) == provisionLockEvictedAtFile {
			return errors.New("simulated evicted-at write failure")
		}
		return origWrite(name, data, perm)
	}
	require.True(t, tryReclaimIfAbandoned(dir, lockPath))
	writeLockFile = origWrite

	evictPath := evictPathFor(lockPath, tok)
	require.DirExists(t, evictPath, "setup: the rename itself must still have succeeded")
	_, statErr := os.Stat(filepath.Join(evictPath, provisionLockEvictedAtFile))
	require.True(t, os.IsNotExist(statErr), "setup: the marker write must have failed as simulated")

	origStale := provisionLockStaleAfter
	provisionLockStaleAfter = time.Nanosecond // the smallest possible cutoff
	t.Cleanup(func() { provisionLockStaleAfter = origStale })
	time.Sleep(time.Millisecond)

	garbageCollectLockLitter(dir)

	assert.DirExists(t, evictPath, "an entry with no confirmed eviction time must never be swept, however small the cutoff")
}

// TestTryReclaimIfAbandoned_EvictedAtWriteFails_LateReclaimerStillCollides is
// the permanent regression test for the companion safety property: even
// though a failed evicted-at write leaves an entry GC can never sweep (see
// above), a LATE reclaimer that still reads the original (now-superseded)
// owner id must keep colliding with that permanent entry rather than ever
// reaching a live successor — the missing marker must never relax the
// identity-bound destination guarantee itself.
func TestTryReclaimIfAbandoned_EvictedAtWriteFails_LateReclaimerStillCollides(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	tOld := makeLongHeldCrashedLock(t, lockPath, 10*time.Minute)

	origWrite := writeLockFile
	var tS string
	orig := renameFile
	fired := false
	renameFile = func(src, dst string) error {
		if !fired && src == lockPath && dst == evictPathFor(lockPath, tOld) {
			fired = true
			writeLockFile = func(name string, data []byte, perm os.FileMode) error {
				if filepath.Base(name) == provisionLockEvictedAtFile {
					return errors.New("simulated evicted-at write failure")
				}
				return origWrite(name, data, perm)
			}
			require.True(t, tryReclaimIfAbandoned(dir, lockPath))
			writeLockFile = origWrite
			var err error
			tS, err = tryCreateFileLock(lockPath)
			require.NoError(t, err)
		}
		return orig(src, dst)
	}
	t.Cleanup(func() { renameFile = orig; writeLockFile = origWrite })

	assert.False(t, tryReclaimIfAbandoned(dir, lockPath), "a late reclaimer must still collide with the permanent (markerless-age) entry")
	renameFile = orig
	assert.True(t, stillOwnsLockFile(lockPath, tS), "the live successor must be untouched")
}

// NOTE: a waiter gives up on a live,
// heartbeating holder once its fixed budget runs out. This is a known,
// accepted limitation, not a bug this PR fixes — see fileLockRetries'
// doc for the reasoning (a caller with no ctx deadline of its own would
// otherwise wait forever against a genuinely wedged, not crashed, holder).
// TestAcquireFileLock_WaiterGivesUpOnLiveHolder_KnownLimitation below
// documents the CURRENT, intentional behavior as a regression test in its
// own right: if a future change makes waiters keep retrying past the
// budget against a live holder, this test should start failing and prompt
// an explicit decision, not a silent behavior change.
func TestAcquireFileLock_WaiterGivesUpOnLiveHolder_KnownLimitation(t *testing.T) {
	withFastFileLockTiming(t)
	origRetries := fileLockRetries
	fileLockRetries = 5 // ~100ms budget
	t.Cleanup(func() { fileLockRetries = origRetries })

	dir := t.TempDir()
	held, err := acquireFileLock(context.Background(), dir)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	// The holder stays alive (heartbeat keeps beating) for the whole test —
	// never released until the deferred call above, well after the
	// waiter's budget has expired.
	_, err = acquireFileLock(context.Background(), dir)
	require.Error(t, err, "known limitation: a waiter currently gives up once its fixed budget elapses, even against a live, heartbeating holder")
}

// TestTryCreateFileLock_OwnerWriteFailure_RemovesLockDir is a regression
// test that a real disk-full/read-only-mount fault isn't something a
// hermetic unit test can portably trigger otherwise: with writeLockFile
// made replaceable, a failure writing
// the owner marker can be forced directly, proving tryCreateFileLock
// leaves nothing behind rather than a markerless, unowned lock directory.
func TestTryCreateFileLock_OwnerWriteFailure_RemovesLockDir(t *testing.T) {
	orig := writeLockFile
	writeLockFile = func(name string, data []byte, perm os.FileMode) error {
		if filepath.Base(name) == provisionLockOwnerFile {
			return fmt.Errorf("simulated owner-write failure")
		}
		return orig(name, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	dir := t.TempDir()
	lockPath := filepath.Join(dir, provisionFileLockName)
	_, err := tryCreateFileLock(lockPath)
	require.Error(t, err)
	// Assert dir itself is empty, not just that lockPath doesn't exist: the
	// failure happens while
	// building the STAGING directory, which never gets published to
	// lockPath at all, so asserting NoDirExists(lockPath) alone is always
	// true regardless of whether the staging directory was cleaned up —
	// it never proves anything about the actual fix.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed owner-marker write must leave no staging directory (or anything else) behind")
}

// TestGitCloneViaTempDir_MoveFailure_LeavesNoPartialClone is a regression
// test for gitCloneViaTempDir's own handling of
// a moveDirContentsUp failure (as opposed to moveDirContentsUp's rollback in
// isolation, already covered by TestMoveDirContentsUp_RollsBackOnPartialFailure):
// with moveRenameFile made replaceable, a move failure can be forced mid-clone,
// proving the caller returns an error and leaves no scratch dir behind.
func TestGitCloneViaTempDir_MoveFailure_LeavesNoPartialClone(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()

	orig := moveRenameFile
	var failed atomic.Bool
	moveRenameFile = func(oldpath, newpath string) error {
		if failed.CompareAndSwap(false, true) {
			return fmt.Errorf("simulated move failure")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { moveRenameFile = orig })

	err := gitCloneWorkspace(context.Background(), ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-move-fail",
		SentinelDir: hostPath, // forces the temp-dir clone path
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}, func() bool { return true })

	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"), "no .git should remain in dest when the clone move fails")
	entries, rerr := os.ReadDir(hostPath)
	require.NoError(t, rerr)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".scion-clone-"), "leftover scratch clone dir: %s", e.Name())
	}
}

// TestProvisionShared_MoveFailure_WritesNoSentinel end-to-ends the same
// simulated failure through ProvisionShared:
// confirms no sentinel is written either, not just that gitCloneViaTempDir
// itself returns an error.
func TestProvisionShared_MoveFailure_WritesNoSentinel(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()

	orig := moveRenameFile
	var failed atomic.Bool
	moveRenameFile = func(oldpath, newpath string) error {
		if failed.CompareAndSwap(false, true) {
			return fmt.Errorf("simulated move failure")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { moveRenameFile = orig })

	err := ProvisionShared(ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-move-fail-e2e",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: hostPath,
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	})
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
}

// TestProvisionShared_LostOwnershipDuringChown_WritesNoSentinel proves
// held.stillOwned() is checked once, synchronously, immediately before the
// sentinel write, and that a failure there is actually honored. The first
// lchownFile call corrupts the file lock's own owner marker as a side
// effect, simulating this holder having been silently reclaimed and
// replaced by another node while a real chown -R (which can run for a long
// time over NFS) was still in flight, then proceeds with the real
// lchown so the rest of the walk still completes normally. ProvisionShared
// must detect the corrupted marker via the stillOwned() check and return an
// error without ever writing the sentinel.
func TestProvisionShared_LostOwnershipDuringChown_WritesNoSentinel(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()
	lockPath := filepath.Join(hostPath, provisionFileLockName)

	orig := lchownFile
	var corrupted atomic.Bool
	lchownFile = func(name string, uid, gid int) error {
		if corrupted.CompareAndSwap(false, true) {
			if err := os.WriteFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte("corrupted-by-test"), 0600); err != nil {
				return err
			}
		}
		return orig(name, uid, gid)
	}
	t.Cleanup(func() { lchownFile = orig })

	err := ProvisionShared(ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-lost-ownership-during-chown",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: hostPath,
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	})
	require.Error(t, err, "ProvisionShared must fail once it detects it lost the lock before writing the sentinel")
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile),
		"sentinel must not be written after losing the lock during chown")
}

// TestGitCloneViaTempDir_LostOwnershipBeforeMove_AbortsWithoutMoving is the
// regression test for the "clone move" ownership
// re-check: a stillOwned callback reporting loss must abort before
// moveDirContentsUp ever runs, leaving dest untouched and no scratch dir
// behind.
func TestGitCloneViaTempDir_LostOwnershipBeforeMove_AbortsWithoutMoving(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()

	err := gitCloneViaTempDir(context.Background(), ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-lost-ownership",
		SentinelDir: hostPath,
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}, func() bool { return false }) // simulate having lost the lock

	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
	entries, rerr := os.ReadDir(hostPath)
	require.NoError(t, rerr)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".scion-clone-"), "leftover scratch dir: %s", e.Name())
	}
}

// --- ProvisionShared end-to-end with no Locker: proves the filesystem
// fallback actually protects the clone/chown/sentinel sequence, not just
// the lock primitive in isolation. ---

func TestProvisionShared_NoLocker_ConcurrentSameProject_NoCorruption(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = ProvisionShared(ProvisionInput{
				Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
				ProjectID: "proj-no-locker-1",
				Mode:      store.SharingModeSharedPlain,
				Locker:    nil,
				GitClone: &api.GitCloneConfig{
					URL:    bareRepo,
					Branch: "main",
				},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}

	// The clone must have completed cleanly exactly once: a valid .git dir
	// and the tracked file present, not a partially-removed-and-reclone mess.
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))

	sentinel := filepath.Join(projectDir, ProvisionSentinelFile)
	assert.FileExists(t, sentinel)

	// The fallback lock directory must not be left behind. (assert.NoFileExists
	// would pass vacuously here since the lock is a directory, not a file,
	// so this must be NoDirExists.)
	assert.NoDirExists(t, filepath.Join(projectDir, provisionFileLockName))
}

// TestProvisionShared_NoLocker_ConcurrentSameProject_SentinelInWorkspace
// reproduces the actual k8s init container configuration: SentinelDir is
// set equal to the workspace dir itself (cmd/sciontool/commands/provision.go
// sets SentinelDir: workspace, because only the workspace dir is mounted —
// its parent isn't visible in that container's filesystem view at all). The
// fallback lock marker therefore lives INSIDE the exact directory `git
// clone` targets, which is what gitCloneViaTempDir exists to handle. This
// test would fail without it: a naive lock-dir-inside-the-clone-target
// would make every single `git clone` invocation hit git's "already exists
// and is not an empty directory" refusal, and self-healing that by deleting
// the lock out from under an in-progress holder is exactly the corruption
// this mechanism exists to prevent.
func TestProvisionShared_NoLocker_ConcurrentSameProject_SentinelInWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)

	hostPath := t.TempDir()

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = ProvisionShared(ProvisionInput{
				Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
				ProjectID:   "proj-no-locker-sentinel-in-ws",
				Mode:        store.SharingModeSharedPlain,
				Locker:      nil,
				SentinelDir: hostPath, // k8s init container: only the workspace dir is mounted.
				GitClone: &api.GitCloneConfig{
					URL:    bareRepo,
					Branch: "main",
				},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}

	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(hostPath, provisionFileLockName))

	// No leftover scratch clone directories from gitCloneViaTempDir.
	entries, err := os.ReadDir(hostPath)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".scion-clone-"), "leftover scratch clone dir: %s", e.Name())
	}

	// The lock's on-disk footprint must be excluded from git's view once a
	// real clone exists there, so every later
	// WorktreePerAgent agent's lock/unlock cycle for this same project never
	// shows up in `git status` inside the shared checkout.
	excludeData, err := os.ReadFile(filepath.Join(hostPath, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(excludeData), provisionFileLockName)
	assert.Contains(t, string(excludeData), provisionFileLockName+".*")

	// Nothing lock-related (including the
	// clock-probe file) may show up in `git status` of the shared checkout.
	out, gitErr := exec.Command("git", "-C", hostPath, "status", "--porcelain", "--ignored=no").CombinedOutput()
	require.NoError(t, gitErr, string(out))
	assert.NotContains(t, string(out), provisionFileLockName, "git status: %s", out)
}

// TestGitCloneWorkspace_NoLocker_SentinelIsParent_ClonesDirect proves
// the temp-dir clone workaround must apply only
// when the fallback lock's own directory would actually sit inside the
// clone target (SentinelDir == HostPath, the k8s init container's
// configuration). When SentinelDir is the workspace's PARENT instead — the
// broker's own host-side worktree-per-agent flow and Cloud Run both use
// this shape even with Locker == nil — the clone must go directly into
// HostPath exactly as it did before this change, with no scratch
// subdirectory ever created.
func TestGitCloneWorkspace_NoLocker_SentinelIsParent_ClonesDirect(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")
	require.NoError(t, os.MkdirAll(hostPath, 0770))

	err := gitCloneWorkspace(context.Background(), ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-direct-clone",
		Locker:    nil,
		// SentinelDir left empty -> defaults to filepath.Dir(hostPath), i.e.
		// projectDir, NOT hostPath. This is the shape that must clone direct.
		GitClone: &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}, func() bool { return true })
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))

	entries, err := os.ReadDir(hostPath)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".scion-clone-"),
			"clone should have gone directly into hostPath, not via a scratch subdir: found %s", e.Name())
	}
}

// TestProvisionShared_NoLocker_SharedPlain_SentinelPresent_SkipsLockEntirely
// is the regression test proving that for SharedPlain mode, an
// already-provisioned project must return without ever touching the lock —
// including a lock some unrelated crashed holder elsewhere left stale and
// present — not just "without waiting for it".
func TestProvisionShared_NoLocker_SharedPlain_SentinelPresent_SkipsLockEntirely(t *testing.T) {
	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")
	require.NoError(t, os.MkdirAll(hostPath, 0770))
	require.NoError(t, writeSentinel(filepath.Join(projectDir, ProvisionSentinelFile)))

	// A stale, crashed lock happens to be sitting there from something
	// unrelated. If ProvisionShared touched the lock at all for this
	// already-done SharedPlain project, it would see this and either wait
	// out fileLockRetries*fileLockRetryDelay or reclaim it — either is wrong
	// when there is nothing left to do.
	lockPath := filepath.Join(projectDir, provisionFileLockName)
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte("dead-owner"), 0600))
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	start := time.Now()
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID: "proj-already-done",
		Mode:      store.SharingModeSharedPlain,
		Locker:    nil,
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "an already-provisioned SharedPlain project must return immediately, not touch the lock at all")

	// The stale lock must be untouched — proof this path never even looked at it.
	current, err := os.ReadFile(filepath.Join(lockPath, provisionLockOwnerFile))
	require.NoError(t, err)
	assert.Equal(t, "dead-owner", string(current))
}

// TestProvisionShared_NoLocker_WorktreePerAgent_SentinelPresent_CrashedLock_SelfHeals
// covers the "crashed holder on an already-provisioned workspace" case:
// unlike SharedPlain,
// WorktreePerAgent must still take the lock (for ensureWorktree) even when
// the base clone's sentinel already exists — and if a stale, crashed lock
// happens to be sitting there, it must self-heal (reclaim) rather than fail
// the whole retry budget.
func TestProvisionShared_NoLocker_WorktreePerAgent_SentinelPresent_CrashedLock_SelfHeals(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Provision the base clone once, normally, so the sentinel exists.
	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID: "proj-crashed-lock-wt",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    nil,
		AgentID:   "agent-0",
		AgentName: "agent-0",
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}))

	// Simulate a crashed holder: a stale lock left behind after the sentinel
	// was already written (e.g. it crashed during a later agent's worktree
	// setup, or from an unrelated process).
	lockPath := filepath.Join(hostPath, provisionFileLockName)
	require.NoError(t, os.Mkdir(lockPath, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(lockPath, provisionLockOwnerFile), []byte("dead-owner"), 0600))
	stale := time.Now().Add(-2 * provisionLockStaleAfter)
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	start := time.Now()
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID: "proj-crashed-lock-wt",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    nil,
		AgentID:   "agent-1",
		AgentName: "agent-1",
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, fileLockRetryDelay*3, "a stale crashed lock on an already-provisioned workspace should self-heal promptly, not wait out the full retry budget")
	assert.DirExists(t, WorktreePath(hostPath, "agent-1"))
}

// TestGitCloneViaTempDir_RefusesWhenWorktreesNonEmpty covers the stray-
// content-clearing refusal path:
// gitCloneViaTempDir must never clear a non-empty "worktrees" dir
// out from under other agents' checkouts, even though the lock marker's own
// presence would otherwise make dest look "not empty" and eligible for the
// stray-content clear.
func TestGitCloneViaTempDir_RefusesWhenWorktreesNonEmpty(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)

	hostPath := t.TempDir()
	worktreesDir := filepath.Join(hostPath, "worktrees")
	require.NoError(t, os.MkdirAll(worktreesDir, 0770))
	require.NoError(t, os.WriteFile(filepath.Join(worktreesDir, "sentinel-file"), []byte("agent work"), 0644))

	err := gitCloneWorkspace(context.Background(), ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-worktrees-nonempty",
		Locker:      nil,
		SentinelDir: hostPath, // forces the temp-dir clone path
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}, func() bool { return true })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktrees")
	// The other agent's work must be untouched.
	assert.FileExists(t, filepath.Join(worktreesDir, "sentinel-file"))
}

// TestGitCloneViaTempDir_PreClearSurvivesConcurrentStagingDir proves
// the pre-clone stray-content clear must never remove a concurrent caller's
// in-progress staging directory. This builds its staging directory using
// provisionLockStagingPattern — the SAME constant tryCreateFileLock itself
// uses — rather than a separately-maintained literal copy of the pattern, so
// a drift between the two is exactly what this test would catch.
func TestGitCloneViaTempDir_PreClearSurvivesConcurrentStagingDir(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()

	// Simulate a concurrent caller's in-progress tryCreateFileLock staging
	// directory sitting in dest at the moment this clone's pre-clear runs.
	stagingDir, err := os.MkdirTemp(hostPath, provisionLockStagingPattern)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stagingDir, provisionLockOwnerFile), []byte("concurrent-owner"), 0600))

	err = gitCloneWorkspace(context.Background(), ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-stage-survives-clear",
		Locker:      nil,
		SentinelDir: hostPath, // forces the temp-dir clone path
		GitClone:    &api.GitCloneConfig{URL: bareRepo, Branch: "main"},
	}, func() bool { return true })

	require.NoError(t, err)
	assert.DirExists(t, stagingDir, "the pre-clone clear must not remove a concurrent caller's staging directory")
	current, readErr := os.ReadFile(filepath.Join(stagingDir, provisionLockOwnerFile))
	require.NoError(t, readErr)
	assert.Equal(t, "concurrent-owner", string(current))
}

// TestMoveDirContentsUp_RollsBackOnPartialFailure proves that if a move fails partway through, every entry already moved
// into dest must be rolled back (deleted from dest), so dest never ends up
// in a partial state. Simulated by making one destination entry a
// non-empty directory that os.Rename cannot replace, forcing that specific
// move to fail.
func TestMoveDirContentsUp_RollsBackOnPartialFailure(t *testing.T) {
	dest := t.TempDir()
	src := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0644))
	require.NoError(t, os.Mkdir(filepath.Join(src, "conflict"), 0770))
	require.NoError(t, os.WriteFile(filepath.Join(src, "conflict", "inner.txt"), []byte("x"), 0644))
	require.NoError(t, os.Mkdir(filepath.Join(src, ".git"), 0770))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644))

	// Pre-populate dest with a non-empty "conflict" dir so os.Rename onto it fails.
	require.NoError(t, os.Mkdir(filepath.Join(dest, "conflict"), 0770))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "conflict", "existing.txt"), []byte("y"), 0644))

	err := moveDirContentsUp(src, dest)
	require.Error(t, err)

	// "a.txt" was moved before the failing "conflict" entry (alphabetical
	// ReadDir order) — it must have been rolled back out of dest.
	assert.NoFileExists(t, filepath.Join(dest, "a.txt"))
	// .git must never have been moved at all: it's always attempted last,
	// and the failure on "conflict" happens before the loop ever reaches it.
	assert.NoDirExists(t, filepath.Join(dest, ".git"))
	// dest's own pre-existing "conflict" content is untouched.
	assert.FileExists(t, filepath.Join(dest, "conflict", "existing.txt"))
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

// --- IsValidJoinWorktree (consolidated onto ValidateWorktreeForBase, with the
// Lstat .git symlink front-guard) ---
//
// These cases were originally written directly against main's own
// IsRealWorktreeDir/isDirectChildOfWorktreesDir helpers before the stack's
// ValidateWorktreeForBase (admin-directory back-link proof included) was
// consolidated in as the one implementation both JOIN sites and the
// mount-time gate share. Ported one-for-one onto IsValidJoinWorktree so no
// case either side's helper covered is lost in the consolidation.

func TestIsValidJoinWorktree(t *testing.T) {
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
		if err := IsValidJoinWorktree(base, realWorktree); err != nil {
			t.Errorf("expected the freshly created worktree to be recognized as real: %v", err)
		}
	})

	t.Run("a plain file", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "not-a-dir")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := IsValidJoinWorktree(base, p); err == nil {
			t.Error("expected a plain file to be rejected")
		}
	})

	t.Run("a symlink to a directory", func(t *testing.T) {
		target := t.TempDir()
		p := filepath.Join(base, "worktrees", "a-symlink")
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if err := IsValidJoinWorktree(base, p); err == nil {
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
		if err := IsValidJoinWorktree(base, p); err == nil {
			t.Error("expected a symlink to another agent's real worktree to be rejected")
		}
	})

	t.Run("a directory with no .git at all", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "no-git")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := IsValidJoinWorktree(base, p); err == nil {
			t.Error("expected a directory with no .git to be rejected")
		}
	})

	t.Run("a directory whose .git is itself a directory", func(t *testing.T) {
		p := filepath.Join(base, "worktrees", "git-is-a-dir")
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := IsValidJoinWorktree(base, p); err == nil {
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
		if err := IsValidJoinWorktree(base, p); err == nil {
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
		if err := IsValidJoinWorktree(base, p); err == nil {
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
		if err := IsValidJoinWorktree(base, p); err == nil {
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
		if err := IsValidJoinWorktree(base, p); err == nil {
			t.Error("expected a .git file naming the base's own .git directory (rel \"..\") to be rejected")
		}
	})
}

// --- HardenedGitCommand ---
//
// pkg/runtime/common.go's narrowGitAdminMounts (Part A) is the primary
// control: a read-only bind mount over the shared base's .git
// config/hooks/info means only host-managed hooks/config/filters are ever
// honored when the broker runs git against the base. These tests exercise
// what's testable without Docker/mount-namespace access (unavailable in this
// sandbox): HardenedGitCommand's own behavior is exercised directly against
// a real base repo. A real read-only bind mount's enforcement of writes to
// the pre-existing .git/config file specifically (as opposed to creating a
// new file, e.g. under .git/hooks/) is proven only at the "correct mount
// args are generated" level (pkg/runtime/common_test.go's
// TestNarrowGitAdminMounts_HubNativeDocker) and requires a real Docker
// read-only bind mount to verify end to end (tracked acceptance item).

func TestHardenedGitCommand_RefusesCommondirRedirect(t *testing.T) {
	// A hub-native shared base is always the main working copy of its own
	// repository, so a top-level .git/commondir file is never legitimate:
	// git resolves config/hooks/refs through whatever commondir points to,
	// for the base's own gitdir as much as for any linked worktree. Its
	// presence would otherwise redirect every HardenedGitCommand
	// invocation's config/hooks resolution away from the mounted,
	// host-managed .git admin surface to an arbitrary writable location —
	// this is exactly the gap narrowGitAdminMounts' read-only mount alone
	// does not close, since it never inspects commondir.
	base := t.TempDir()
	run(t, "git", "init", "--initial-branch=main", base)
	runIn(t, base, "git", "-c", "user.name=t", "-c", "user.email=t@t.com",
		"commit", "--allow-empty", "-m", "root")

	// A redirect target with its own hook, standing in for an unverified
	// location outside the mounted admin surface.
	redirect := t.TempDir()
	if err := os.MkdirAll(filepath.Join(redirect, "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	hookScript := fmt.Sprintf("#!/bin/sh\necho hook-ran >> %s\n", marker)
	if err := os.WriteFile(filepath.Join(redirect, "hooks", "post-checkout"), []byte(hookScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(redirect, "config"), []byte("[core]\n\trepositoryformatversion = 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(redirect, "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(redirect, "refs", "heads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".git", "commondir"), []byte(redirect+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	_, err := HardenedGitCommand(context.Background(), base, "worktree", "add", "--relative-paths", "-b", "agent-x", wtPath)
	if !errors.Is(err, ErrCommondirPresent) {
		t.Fatalf("expected ErrCommondirPresent, got: %v", err)
	}
	if data, statErr := os.ReadFile(marker); statErr == nil {
		t.Errorf("expected the redirected hook to never run, marker contents: %q", data)
	}
}

func TestWorktreeUsage_UnaffectedByReadOnlyHooksAndInfo(t *testing.T) {
	// AC2-style smoke test (git-level; no docker in this sandbox — see the
	// disclosed environment limitation): with .git/hooks and .git/info
	// read-only (real for these two paths, since creating a new file only
	// needs directory write permission, which read-only expresses
	// correctly), the broker's HardenedGitCommand-driven `worktree add`
	// still succeeds, and ordinary commit + checkout in the resulting
	// worktree are unaffected.
	base := t.TempDir()
	run(t, "git", "init", "--initial-branch=main", base)
	runIn(t, base, "git", "-c", "user.name=t", "-c", "user.email=t@t.com",
		"commit", "--allow-empty", "-m", "root")

	hooksDir := filepath.Join(base, ".git", "hooks")
	infoDir := filepath.Join(base, ".git", "info")
	if err := os.Chmod(hooksDir, 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(infoDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(hooksDir, 0755)
		_ = os.Chmod(infoDir, 0755)
	})

	wtPath := filepath.Join(t.TempDir(), "wt")
	cmd, err := HardenedGitCommand(context.Background(), base, "worktree", "add", "--relative-paths", "-b", "agent-2", wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add failed: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "f.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runIn(t, wtPath, "git", "add", "f.txt")
	runIn(t, wtPath, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "-m", "agent commit")
	runIn(t, wtPath, "git", "checkout", "-b", "agent-2-work")
}

func TestHardenedGitCommand_NeutralizesFsmonitorRegardlessOfConfig(t *testing.T) {
	// core.fsmonitor is a pure .git/config vector (no on-disk file creation
	// needed), so unlike hooks/info above it cannot be blocked by directory
	// permissions — this is exactly why HardenedGitCommand clears it at the
	// invocation level (Part B) as belt-and-suspenders over Part A.
	base := t.TempDir()
	run(t, "git", "init", "--initial-branch=main", base)
	runIn(t, base, "git", "-c", "user.name=t", "-c", "user.email=t@t.com",
		"commit", "--allow-empty", "-m", "root")

	marker := filepath.Join(t.TempDir(), "marker")
	fsmonScript := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(fsmonScript,
		[]byte(fmt.Sprintf("#!/bin/sh\necho fsmonitor-ran >> %s\necho \"\"\n", marker)), 0755); err != nil {
		t.Fatal(err)
	}
	// An unexpected core.fsmonitor, however it got there.
	runIn(t, base, "git", "config", "core.fsmonitor", fsmonScript)

	cmd, err := HardenedGitCommand(context.Background(), base, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git status failed: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(marker); len(data) != 0 {
		t.Errorf("expected core.fsmonitor to be neutralized by HardenedGitCommand, but it ran: %q", data)
	}

	// Revert-check: the identical config, invoked WITHOUT the wrapper, DOES
	// fire — proving the wrapper (not something incidental) is what
	// neutralizes it.
	plain := exec.Command("git", "status", "--porcelain")
	plain.Dir = base
	if out, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("git status (unwrapped) failed: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(marker); len(data) == 0 {
		t.Error("expected core.fsmonitor to fire without the wrapper (revert-check baseline), got no marker")
	}
}

func TestHardenedGitCommand_TrustedHookAndGlobalFilterStillRun(t *testing.T) {
	// Part A only prevents a CONTAINER from writing config/hooks/info; it
	// does not and must not stop the HOST itself (e.g. `git lfs install`,
	// run by the broker operator, not a container) from doing so, and
	// HardenedGitCommand must not neutralize what it finds there. git-lfs
	// isn't available in this environment, so this stands in for it exactly
	// as instructed: filter.lfs.* configured via the GLOBAL gitconfig (the
	// way `git lfs install` actually writes it — not repo-local, which would
	// pass even with a wrongly-cleared GIT_CONFIG_GLOBAL) plus a trusted
	// post-checkout hook already present in the base's .git/hooks (as
	// git-lfs install also adds).
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv("MARKER_FILE", marker)

	// A script file (rather than an inline shell command) avoids nested-quote
	// mangling once the command string round-trips through gitconfig.
	smudgeScript := filepath.Join(t.TempDir(), "smudge.sh")
	if err := os.WriteFile(smudgeScript, []byte("#!/bin/sh\ncat >/dev/null\necho smudge-ran >> \"$MARKER_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	globalConfig := filepath.Join(home, ".gitconfig")
	globalConfigBody := fmt.Sprintf(
		"[filter \"lfs\"]\n\tsmudge = %s\n\tclean = cat\n\trequired = false\n",
		smudgeScript)
	if err := os.WriteFile(globalConfig, []byte(globalConfigBody), 0644); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	run(t, "git", "init", "--initial-branch=main", base)
	runIn(t, base, "git", "-c", "user.name=t", "-c", "user.email=t@t.com",
		"commit", "--allow-empty", "-m", "root")

	// A trusted, host-placed post-checkout hook (as `git lfs install` adds).
	hookScript := fmt.Sprintf("#!/bin/sh\necho hook-ran >> %s\n", marker)
	if err := os.WriteFile(filepath.Join(base, ".git", "hooks", "post-checkout"), []byte(hookScript), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(base, ".gitattributes"), []byte("data.bin filter=lfs -text\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "data.bin"), []byte("binary-content"), 0644); err != nil {
		t.Fatal(err)
	}
	runIn(t, base, "git", "add", ".")
	runIn(t, base, "git", "-c", "user.name=t", "-c", "user.email=t@t.com",
		"commit", "-m", "add lfs-tracked file")

	// The exact broker trigger: HardenedGitCommand-driven `git worktree add`,
	// which checks out the new worktree — running the post-checkout hook and
	// the smudge filter for data.bin.
	wtPath := filepath.Join(t.TempDir(), "wt")
	cmd, err := HardenedGitCommand(context.Background(), base, "worktree", "add", "--relative-paths", "-b", "agent-1", wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add failed: %v\n%s", err, out)
	}

	data, _ := os.ReadFile(marker)
	if !strings.Contains(string(data), "hook-ran") {
		t.Errorf("expected the trusted post-checkout hook to run, marker: %q", data)
	}
	if !strings.Contains(string(data), "smudge-ran") {
		t.Errorf("expected the trusted global (LFS-style) smudge filter to run, marker: %q", data)
	}
}

func TestHardenedGitCommand_DoesNotClobberCredentialHelperEnv(t *testing.T) {
	// pkg/util/git.go's PullSharedWorkspace authenticates via a one-shot
	// credential helper injected through GIT_CONFIG_COUNT/KEY_0/VALUE_0 env
	// vars. A caller combining that technique with HardenedGitCommand must
	// APPEND to cmd.Env (not replace it), or the GIT_COMMON_DIR pin would be
	// lost along with the ambient environment.
	base := t.TempDir()
	run(t, "git", "init", "--initial-branch=main", base)

	helper := "!f() { echo username=oauth2; echo password=test-token; }; f"
	credEnv := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=" + helper,
	}

	credCmd, err := HardenedGitCommand(context.Background(), base, "config", "--get", "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	credCmd.Env = append(credCmd.Env, credEnv...)
	out, err := credCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get credential.helper failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != helper {
		t.Errorf("expected the credential-helper env to survive alongside the wrapper's env, got %q want %q", got, helper)
	}

	pagerCmd, err := HardenedGitCommand(context.Background(), base, "config", "--get", "core.pager")
	if err != nil {
		t.Fatal(err)
	}
	pagerCmd.Env = append(pagerCmd.Env, credEnv...)
	out, err = pagerCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get core.pager failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "cat" {
		t.Errorf("expected the wrapper's core.pager=cat to survive alongside the credential-helper env, got %q", got)
	}

	commonDirCmd, err := HardenedGitCommand(context.Background(), base, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	commonDirCmd.Env = append(commonDirCmd.Env, credEnv...)
	out, err = commonDirCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --git-common-dir failed: %v\n%s", err, out)
	}
	wantCommonDir := filepath.Join(base, ".git")
	if got := strings.TrimSpace(string(out)); got != wantCommonDir {
		t.Errorf("expected the GIT_COMMON_DIR pin to survive alongside the credential-helper env, got %q want %q", got, wantCommonDir)
	}
}

// --- documented in-container git workflow limitations under a
// read-only .git/config, and the branch.autoSetupMerge=false mitigation ---

// runInGetCode runs a command in dir and returns its exit code and combined
// output without failing the test. Used where the point of the assertion is
// a specific exit code (including nonzero), not bare success.
func runInGetCode(t *testing.T, dir, name string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("%s %v (in %s): %v\n%s", name, args, dir, err, out)
	return -1, string(out)
}

// runInExpectCode runs a command in dir and fails the test if its exit code
// does not match want.
func runInExpectCode(t *testing.T, dir string, want int, name string, args ...string) {
	t.Helper()
	got, out := runInGetCode(t, dir, name, args...)
	if got != want {
		t.Errorf("%s %v (in %s): exit code = %d, want %d\n%s", name, args, dir, got, want, out)
	}
}

func TestPrepareBaseForWorktrees_DocumentedWorkflowsWithConfigUnwritable(t *testing.T) {
	// Part A mounts the shared base's .git/config read-only into the agent
	// container. That does not block plain git usage, but it does block any
	// command that needs to WRITE repo config — most notably setting up a
	// new tracking relationship. prepareBaseForWorktrees sets
	// branch.autoSetupMerge=false specifically so the single most common of
	// those (`checkout -b <local> <remote>/<branch>`, and DWIM `switch
	// <remote-branch>`) degrades to a plain untracked local branch instead
	// of failing outright. This test forces config writes to fail the same
	// way a read-only bind mount would (a pre-created .git/config.lock —
	// real mount enforcement is EROFS/EBUSY, not exercised here; see the
	// disclosed environment limitation) and asserts the resulting documented
	// supported/unsupported command list.
	ctx := context.Background()
	work := t.TempDir()
	bare := filepath.Join(work, "bare.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bare)

	remoteClone := filepath.Join(work, "remote-clone")
	run(t, "git", "clone", bare, remoteClone)
	runIn(t, remoteClone, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "--allow-empty", "-m", "root")
	runIn(t, remoteClone, "git", "push", "origin", "main")
	runIn(t, remoteClone, "git", "checkout", "-b", "feature-remote")
	runIn(t, remoteClone, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "--allow-empty", "-m", "feature")
	runIn(t, remoteClone, "git", "push", "origin", "feature-remote")

	base := filepath.Join(work, "base")
	run(t, "git", "clone", bare, base)
	if err := prepareBaseForWorktrees(ctx, base); err != nil {
		t.Fatalf("prepareBaseForWorktrees: %v", err)
	}

	wt := filepath.Join(work, "wt")
	runIn(t, base, "git", "worktree", "add", wt, "-b", "agent-branch")

	// Force config writes to fail the way a read-only-mounted .git/config
	// would: git's config write is a lock-then-rename, so pre-creating the
	// lock file makes that step fail the same way EBUSY/EROFS against a
	// real read-only mount would.
	configLock := filepath.Join(base, ".git", "config.lock")
	if err := os.WriteFile(configLock, nil, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(configLock) })

	// Mitigated by branch.autoSetupMerge=false: succeed despite config being
	// unwritable, by skipping the upstream-tracking config write entirely.
	runInExpectCode(t, wt, 0, "git", "checkout", "-b", "tracked-checkout", "origin/feature-remote")
	runInExpectCode(t, wt, 0, "git", "switch", "feature-remote") // DWIM

	// Documented as broken with config unwritable; NOT fixed by the
	// mitigation above (these all write config for reasons other than
	// initial tracking setup).
	runInExpectCode(t, wt, 1, "git", "branch", "--set-upstream-to=origin/main", "agent-branch")
	runInExpectCode(t, wt, 128, "git", "branch", "-m", "agent-branch", "agent-branch-renamed")
	// The push itself succeeds; its upstream is silently NOT recorded.
	runInExpectCode(t, wt, 0, "git", "push", "-u", "origin", "agent-branch-renamed")
	if code, _ := runInGetCode(t, wt, "git", "config", "--get", "branch.agent-branch-renamed.remote"); code == 0 {
		t.Error("expected push -u's upstream to NOT be recorded when config is unwritable (documented limitation)")
	}
	runInExpectCode(t, wt, 128, "git", "remote", "add", "extra-remote", "https://example.invalid/x.git")
	runInExpectCode(t, wt, 255, "git", "config", "local.test.key", "value")

	// Unaffected by config being unwritable.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runInExpectCode(t, wt, 0, "git", "add", "f.txt")
	runInExpectCode(t, wt, 0, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "-m", "agent commit")
	runInExpectCode(t, wt, 0, "git", "checkout", "-b", "untracked-local") // no tracking requested
	runInExpectCode(t, wt, 0, "git", "status", "--porcelain")
	runInExpectCode(t, wt, 0, "git", "fetch", "origin")
	runInExpectCode(t, wt, 0, "git", "pull", "--ff-only", "origin", "main")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("hi2"), 0644); err != nil {
		t.Fatal(err)
	}
	runInExpectCode(t, wt, 0, "git", "stash")
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
	sharers, wtPath, err := ListSharers(hostPath, "", "shared-branch")
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
	sharers, wtPath, err = ListSharers(hostPath, "", "shared-branch")
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
	sharers, _, listErr := ListSharers(hostPath, "", "agent-b")
	require.NoError(t, listErr)
	assert.Empty(t, sharers, "expected no sharer marker written on refusal")
}

// TestProvision_EnsureWorktree_RegistryNamesNonWorktree_DiscardedAndRecreated
// proves the registry read boundary's discard-and-recreate path for a
// trickery-free bad marker: a subdirectory of another agent's real worktree
// is physically in-tree but not the canonical direct-child
// "worktrees/<name>" shape, and contains neither a ".." component nor a
// symlink hop on the way from base to it, so it carries no sign of a
// deliberate path-escape attempt (see worktreePathEscapeAttempt). The
// registry read boundary (readMarker) discards the whole marker rather than
// refusing outright, ListSharers reports worktreePath="", and ProvisionShared
// falls through to create agent-c its own fresh, canonical worktree.
//
// This was originally a hard-refusal test (renamed from
// TestProvision_EnsureWorktree_RegistryNamesNonWorktree_Refused): a plain,
// trickery-free wrong-depth path is reclassified as benign/stale now that
// the read boundary distinguishes it from an escape attempt, but the
// named path itself must never be touched either way — that invariant is
// asserted explicitly below.
func TestProvision_EnsureWorktree_RegistryNamesNonWorktree_DiscardedAndRecreated(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-discarded",
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
	require.NoError(t, RegisterSharer(hostPath, "", "other-branch", nested, "agent-a"))
	nestedGitBefore, err := os.ReadFile(filepath.Join(nested, ".git"))
	require.NoError(t, err)
	nestedEntriesBefore, err := os.ReadDir(nested)
	require.NoError(t, err)

	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-registry-discarded",
		AgentID:   "agent-c",
		AgentName: "other-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err, "expected ProvisionShared to recover by creating a fresh worktree, not refuse")

	// The stale nested path must never have been touched -- no mount, no
	// write, no delete -- discard means "drop the reference," never "act on
	// what the bad marker pointed to."
	require.DirExists(t, nested, "the stale nested path must still exist, untouched")
	nestedGitAfter, err := os.ReadFile(filepath.Join(nested, ".git"))
	require.NoError(t, err)
	assert.Equal(t, nestedGitBefore, nestedGitAfter, "the stale nested path's gitfile must survive byte-identical")
	nestedEntriesAfter, err := os.ReadDir(nested)
	require.NoError(t, err)
	assert.Equal(t, len(nestedEntriesBefore), len(nestedEntriesAfter), "the stale nested path's contents must survive untouched")

	// agent-c must have gotten its own fresh, canonical worktree, and the
	// registry must now point at it instead of the stale nested path.
	wtC := WorktreePath(hostPath, "agent-c")
	require.DirExists(t, wtC, "expected a fresh canonical worktree for agent-c")
	sharers, wtPath, listErr := ListSharers(hostPath, "", "other-branch")
	require.NoError(t, listErr)
	assert.Equal(t, wtC, wtPath, "registry must record the fresh worktree, not the stale nested path")
	// The pre-existing "agent-a" sharer entry survives the degrade (keep
	// Sharers, blank only WorktreePath -- see readMarker's doc comment on
	// the data-loss regression this fixes): a real agent's refcount
	// registration must never be silently dropped just because its recorded
	// path didn't match a known shape. agent-c is added alongside it, not in
	// place of it.
	assert.ElementsMatch(t, []string{"agent-a", "agent-c"}, sharers)
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
	require.NoError(t, RegisterSharer(hostPath, "", "other-branch-2", plainDir, "agent-a"))

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

	sharers, wtPath, listErr := ListSharers(hostPath, "", "other-branch-2")
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
			require.NoError(t, RegisterSharer(hostPath, "", branch, marker, "agent-a"))

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

			// The marker now consistently refuses to read (worktreePathEscapeAttempt
			// keeps surfacing it as an error, not just on the first read), so
			// the original registration's survival is checked directly against
			// the on-disk JSON rather than through ListSharers.
			raw, readErr := os.ReadFile(sharerPath(hostPath, branch))
			require.NoError(t, readErr, "the original (bogus) marker file must still exist, untouched")
			var m sharerMarker
			require.NoError(t, json.Unmarshal(raw, &m))
			assert.Equal(t, marker, m.WorktreePath, "the original (bogus) registration must survive unchanged")
			assert.Equal(t, []string{"agent-a"}, m.Sharers, "expected no new agent added to the registry on refusal")

			// And the registry read boundary itself keeps refusing it
			// consistently, rather than silently recovering on a later read.
			_, _, listErr := ListSharers(hostPath, "", branch)
			require.Error(t, listErr, "expected ListSharers to keep refusing a non-canonical marker, not silently discard it")
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
	sharers, wtPath, listErr := ListSharers(hostPath, "", "nested-br")
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

	sharers, wtPath, listErr := ListSharers(hostPath, "", "nested-br")
	require.NoError(t, listErr)
	if wtPath == nested {
		t.Errorf("expected no marker naming the nested path %s, but the registry has one (sharers=%v)", nested, sharers)
	}
}

// TestProvision_WorktreePerAgent_OutOfTreeMarker_CreatesFreshWorktree covers
// Phase 1 acceptance: a sharer marker whose recorded WorktreePath is not
// in-tree under base must not redirect a JOINing agent's workspace. The read
// boundary (readMarker) discards the whole marker, ListSharers returns
// worktreePath="", and ProvisionShared falls through to create a fresh
// worktree for the joining agent instead of reusing the out-of-tree path.
//
// The out-of-tree marker is written for a branch that has no real git worktree
// backing it — the registry entry is the only place the (fake) association
// lives: an in-tree-but-non-worktree (or out-of-tree) recorded path must not
// redirect a JOINing agent's workspace.
func TestProvision_WorktreePerAgent_OutOfTreeMarker_CreatesFreshWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Establish the shared base checkout via an unrelated agent/branch.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-outoftree-1",
		AgentID:   "agent-setup",
		AgentName: "setup-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	branch := "shared-branch"

	// A second agent writes an out-of-tree marker directly (simulating a write
	// through the RW .git bind mount) for a branch that has no real git
	// worktree, pointing WorktreePath at a host directory outside the base
	// worktree tree.
	outside := t.TempDir()
	require.NoError(t, RegisterSharer(hostPath, "", branch, outside, "agent-c"))

	// Sanity: the out-of-tree path is never surfaced through the registry
	// API, but the sharer refcount is preserved (degrade, not discard — see
	// pkg/provision/sharers.go's readMarker; discarding it was a real
	// data-loss regression for the ProvisionAgent layout).
	sharers, wtPath, err := ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	assert.Empty(t, wtPath, "marker with an out-of-tree worktreePath must not surface it")
	assert.Equal(t, []string{"agent-c"}, sharers, "the sharer refcount must survive an out-of-tree worktreePath")

	// The second (joining) agent provisions on the same branch. It must NOT be
	// redirected to the out-of-tree external directory; it must get a fresh
	// in-tree worktree.
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-outoftree-1",
		AgentID:   "agent-b",
		AgentName: branch,
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	wtB := WorktreePath(hostPath, "agent-b")
	require.DirExists(t, wtB, "provisioning must create a fresh in-tree worktree when the marker's path is out-of-tree")

	_, wtPath, err = ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	assert.Equal(t, wtB, wtPath, "registry must record the fresh in-tree worktree, not the out-of-tree path")
	assert.NotEqual(t, outside, wtPath)
}

// TestProvision_WorktreePerAgent_RegistryDecoy_CreatesFreshWorktree covers
// Phase 3 acceptance: a registry marker whose WorktreePath is in-tree
// (base/worktrees/<name>, passing the lexical read-boundary check from Phase
// 1) but is not a genuine git worktree of base — a plain directory with no
// gitfile, or a gitfile that doesn't round-trip to base's admin dir — must
// not be joined. ValidateWorktreeForBase catches this even though the
// lexical shape is correct; ensureWorktree falls through to create a fresh
// worktree instead.
func TestProvision_WorktreePerAgent_RegistryDecoy_CreatesFreshWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")

	// Establish the shared base checkout.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-decoy-1",
		AgentID:   "agent-setup",
		AgentName: "setup-branch",
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	branch := "shared-branch"

	// Set up a decoy: a plain directory at the canonical in-tree shape with no
	// git metadata at all, and register it directly as the branch's marker
	// (instead of the fresh-worktree creation path that would normally put a
	// real worktree there).
	decoy := WorktreePath(hostPath, "decoy")
	require.NoError(t, os.MkdirAll(decoy, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(decoy, "README.md"), []byte("not a worktree"), 0o644))
	require.NoError(t, RegisterSharer(hostPath, "", branch, decoy, "agent-c"))

	// Sanity: the decoy passes the Phase 1 lexical read boundary (it IS
	// in-tree), so it is visible via ListSharers — the point of this test is
	// that ensureWorktree's full relationship check catches what the lexical
	// check alone does not.
	_, wtPath, err := ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	require.Equal(t, decoy, wtPath, "setup: decoy should pass the lexical read boundary")

	// The joining agent provisions on the same branch. It must NOT be
	// attached to the decoy; it must get a fresh, genuine worktree.
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-decoy-1",
		AgentID:   "agent-b",
		AgentName: branch,
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)

	wtB := WorktreePath(hostPath, "agent-b")
	require.FileExists(t, filepath.Join(wtB, ".git"), "provisioning must create a fresh, genuine worktree when the registry points at a decoy")

	_, wtPath, err = ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	assert.Equal(t, wtB, wtPath, "registry must record the fresh genuine worktree, not the decoy")
}

// TestProvision_WorktreePerAgent_FakeBackLink_RejectsGitDiscoveredPath covers
// Phase 3 acceptance criterion 7: git's own worktree list — not just the
// sharer marker — is a JOIN discovery source, and it can be steered by
// rewriting the admin back-link file (base/.git/worktrees/<name>/gitdir).
// That file is what "git worktree list" derives a worktree's reported path
// from, so pointing it at an existing external directory makes git itself
// report the branch as checked out there. The registry is cleared first so
// ensureWorktree falls through to the git-worktree-list discovery path
// (findWorktreeForBranch), which is the source this criterion targets.
func TestProvision_WorktreePerAgent_FakeBackLink_RejectsGitDiscoveredPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	locker := newTestLocker()
	bareRepo := initBareGitRepo(t)

	projectDir := t.TempDir()
	hostPath := filepath.Join(projectDir, "workspace")
	branch := "shared-branch"

	// Agent A creates a genuine worktree for the branch.
	err := ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-backlink-1",
		AgentID:   "agent-a",
		AgentName: branch,
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.NoError(t, err)
	wtA := WorktreePath(hostPath, "agent-a")
	require.DirExists(t, wtA)

	// Clear the registry so ensureWorktree's JOIN check falls through to the
	// git-worktree-list discovery path (findWorktreeForBranch) rather than
	// short-circuiting on the marker.
	_, _, err = UnregisterSharer(hostPath, "", branch, "agent-a")
	require.NoError(t, err)
	sharers, _, err := ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	require.Empty(t, sharers, "setup: registry must be empty so JOIN falls through to git discovery")

	// Rewrite the admin back-link (base/.git/worktrees/agent-a/gitdir) to
	// point at an existing external directory with its own (unrelated) .git
	// file. This is exactly what "git worktree list" reads to report a
	// worktree's path — after this, git itself reports the branch as checked
	// out at the external location, not at wtA.
	external := t.TempDir()
	externalGitFile := filepath.Join(external, ".git")
	require.NoError(t, os.WriteFile(externalGitFile, []byte("gitdir: /nonexistent\n"), 0o644))
	backLink := filepath.Join(hostPath, ".git", "worktrees", "agent-a", "gitdir")
	require.NoError(t, os.WriteFile(backLink, []byte(externalGitFile+"\n"), 0o644))

	// Sanity: confirm git itself now reports the external path for this branch.
	discovered, findErr := findWorktreeForBranch(context.Background(), hostPath, branch)
	require.NoError(t, findErr)
	require.Equal(t, external, discovered, "setup: git worktree list should now report the out-of-tree external path")

	// The joining agent provisions on the same branch. It must NOT be
	// attached to the git-discovered external path. Because the corrupted
	// admin metadata also makes git itself believe the branch is checked out
	// there, git refuses a fresh checkout of the same branch too (its own
	// collision guard) — so the safe, observable outcome here is a loud
	// provisioning error, not a silent join or a silent redirect. Either
	// way, the external path must never be touched or registered.
	err = ProvisionShared(ProvisionInput{
		Resolved:  ResolvedWorkspace{HostPath: hostPath, Backend: "local"},
		ProjectID: "proj-backlink-1",
		AgentID:   "agent-b",
		AgentName: branch,
		Mode:      store.SharingModeWorktreePerAgent,
		Locker:    locker,
		GitClone:  &api.GitCloneConfig{URL: bareRepo, Branch: "main", Depth: intPtr(0)},
	})
	require.Error(t, err, "provisioning must fail loudly rather than join or redirect to the git-discovered external path")
	assert.Contains(t, err.Error(), "already checked out")

	// The external path was never touched or claimed by the registry.
	entries, readErr := os.ReadDir(external)
	require.NoError(t, readErr)
	require.Len(t, entries, 1, "external dir must contain only its original unrelated .git file")
	assert.Equal(t, ".git", entries[0].Name())

	_, wtPath, err := ListSharers(hostPath, "", branch)
	require.NoError(t, err)
	assert.NotEqual(t, external, wtPath, "registry must never record the git-discovered external path")
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
	sharersA, pathA, err := ListSharers(hostPath, "", "agent-alpha")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-a"}, sharersA)
	assert.Equal(t, wtA, pathA)

	sharersB, pathB, err := ListSharers(hostPath, "", "agent-beta")
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
	sharers, _, err := ListSharers(hostPath, "", "idem-branch")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-a"}, sharers)
}

func intPtr(i int) *int { return &i }
