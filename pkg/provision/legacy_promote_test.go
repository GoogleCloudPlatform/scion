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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyRootLitter places, in the workspace root, what images from before
// the state directory left there: an evict entry whose eviction is long
// past, a freshly evicted one, an old staging directory and a live lock.
// It returns their paths.
func legacyRootLitter(t *testing.T, ws string) (agedEvict, freshEvict, agedStage, live string) {
	t.Helper()
	longAgo := time.Now().Add(-time.Hour)
	agedEvict = filepath.Join(ws, provisionFileLockName+".evict-aged")
	freshEvict = filepath.Join(ws, provisionFileLockName+".evict-fresh")
	agedStage = filepath.Join(ws, provisionFileLockName+".stage-aged")
	live = filepath.Join(ws, provisionFileLockName)
	for _, d := range []string{agedEvict, freshEvict, agedStage, live} {
		require.NoError(t, os.Mkdir(d, 0o770))
	}
	for _, d := range []string{agedEvict, freshEvict} {
		require.NoError(t, os.WriteFile(filepath.Join(d, provisionLockEvictedAtFile), nil, 0o660))
	}
	require.NoError(t, os.Chtimes(filepath.Join(agedEvict, provisionLockEvictedAtFile), longAgo, longAgo))
	require.NoError(t, os.Chtimes(agedStage, longAgo, longAgo))
	require.NoError(t, os.WriteFile(filepath.Join(live, provisionLockOwnerFile), []byte("0123456789abcdef0123456789abcdef"), 0o660))
	require.NoError(t, os.Chtimes(live, longAgo, longAgo))
	return agedEvict, freshEvict, agedStage, live
}

// makeLegacyMarked turns a workspace current code provisioned into one a
// pre-state-dir image provisioned: the sentinel only in the workspace root,
// with its git exclude line next to other lines.
func makeLegacyMarked(t *testing.T, ws string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile)))
	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))
	require.NoError(t, appendGitExclude(ws, "/keep-me"))
	require.NoError(t, appendGitExclude(ws, legacyExcludeLine))
	require.NoError(t, appendGitExclude(ws, "/keep-me-too"))
}

func readExcludeLines(t *testing.T, ws string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	require.NoError(t, err)
	return strings.Split(string(data), "\n")
}

// assertPromoted checks the AC-L5/AC-L6 end state of a promoted workspace.
func assertPromoted(t *testing.T, ws string) {
	t.Helper()
	assert.FileExists(t, filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile))
	assert.NoFileExists(t, filepath.Join(ws, ProvisionSentinelFile))
	lines := readExcludeLines(t, ws)
	assert.NotContains(t, lines, legacyExcludeLine)
	assert.Contains(t, lines, "/keep-me")
	assert.Contains(t, lines, "/keep-me-too")
}

// AC-L5, shared-plain: the workspace-root marker is promoted to the state
// directory, removed with its exclude line, and the next start does no
// clone and no recursive chown. A second run changes nothing. AC-L6: aged
// workspace-root litter goes, the live lock and fresh litter stay.
func TestPromoteLegacyMarker_SharedPlain(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	ws, stateDir := stateDirLayout(t)
	require.NoError(t, ProvisionShared(stateDirInput(ws, stateDir, origin, store.SharingModeSharedPlain)))
	makeLegacyMarked(t, ws)
	agedEvict, freshEvict, agedStage, live := legacyRootLitter(t, ws)

	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.Equal(t, LegacyPromotion{Promoted: true, MarkerRemoved: true, ExcludeRemoved: true, LitterRemoved: 2}, res)
	assertPromoted(t, ws)
	assert.NoDirExists(t, agedEvict)
	assert.NoDirExists(t, agedStage)
	assert.DirExists(t, freshEvict, "fresh litter is kept")
	assert.DirExists(t, live, "a live workspace-root lock is never removed")
	assert.FileExists(t, filepath.Join(live, provisionLockOwnerFile))
	for _, e := range mustReadDir(t, ws) {
		assert.False(t, strings.HasPrefix(e.Name(), provisionClockProbeFile), "clock probe written into the workspace: %s", e.Name())
	}

	// The next start: no clone (the URL does not exist) and no chown.
	clones := countGitClones(t)
	paths := recordLchown(t, nil)
	missing := filepath.Join(t.TempDir(), "missing.git")
	AllowLocalCloneFixture(t, missing)
	require.NoError(t, ProvisionShared(stateDirInput(ws, stateDir, missing, store.SharingModeSharedPlain)))
	assert.Equal(t, 0, clones())
	assert.Empty(t, *paths, "a promoted workspace is not chowned again")

	// Idempotent: a second run changes nothing.
	again, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.False(t, again.Changed(), "%+v", again)
	assert.DirExists(t, live)
}

// AC-L5, worktree-per-agent with live worktrees: after the promotion the
// next start reuses the base (no clone, no base re-preparation by clone, no
// recursive chown of the shared checkout) and the worktrees are untouched.
func TestPromoteLegacyMarker_WorktreePerAgentLiveWorktrees(t *testing.T) {
	ws := provisionTwoMountedWorktrees(t)
	work := filepath.Join(WorktreePath(ws, "agent-1"), "unpushed.txt")
	require.NoError(t, os.WriteFile(work, []byte("work"), 0o644))
	makeLegacyMarked(t, ws)

	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.True(t, res.Promoted)
	assertPromoted(t, ws)

	clones := countGitClones(t)
	paths := recordLchown(t, nil)
	missing := filepath.Join(t.TempDir(), "missing.git")
	AllowLocalCloneFixture(t, missing)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(ws, missing, "agent-1", "agent-one")))
	assert.Equal(t, 0, clones())
	for _, p := range *paths {
		assert.NotEqual(t, filepath.Join(ws, "README.md"), p, "the shared checkout was chowned again")
		assert.False(t, strings.HasPrefix(p, filepath.Join(ws, ".git", "objects")), "objects chowned again: %s", p)
	}
	for _, agent := range []string{"agent-1", "agent-2"} {
		assert.True(t, IsRealWorktreeDir(WorktreePath(ws, agent), ws), agent)
	}
	data, err := os.ReadFile(work)
	require.NoError(t, err)
	assert.Equal(t, "work", string(data))

	again, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.False(t, again.Changed(), "%+v", again)
}

// A workspace with nothing to promote takes no lock: the step returns at
// once even while another holder has the state-directory lock.
func TestPromoteLegacyMarker_NothingToDoTakesNoLock(t *testing.T) {
	ws := newWorkspaceTree(t)
	held, err := acquireFileLock(t.Context(), ProjectStateDir(ws))
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	res, err := PromoteLegacyMarker(ctx, ws)
	require.NoError(t, err)
	assert.False(t, res.Changed())
}

// A missing workspace is nothing to do; a relative or unclean path is
// refused.
func TestPromoteLegacyMarker_PathChecks(t *testing.T) {
	res, err := PromoteLegacyMarker(t.Context(), filepath.Join(t.TempDir(), "absent", "workspace"))
	require.NoError(t, err)
	assert.False(t, res.Changed())
	for _, p := range []string{"", "relative/workspace", "/a/../workspace"} {
		_, err := PromoteLegacyMarker(t.Context(), p)
		assert.Error(t, err, p)
	}
}

// A marker that is not a regular file (a symlink) never promotes: the
// state-directory sentinel is not written, and the link itself is removed
// (never its target).
func TestPromoteLegacyMarker_SymlinkMarkerNotPromoted(t *testing.T) {
	ws := newWorkspaceTree(t)
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(ws, ProvisionSentinelFile)))

	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.False(t, res.Promoted)
	assert.True(t, res.MarkerRemoved)
	assert.NoFileExists(t, filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile))
	assert.FileExists(t, target)
}

// An existing state-directory sentinel is kept as it is; the
// workspace-root marker is still removed.
func TestPromoteLegacyMarker_StateSentinelAlreadyPresent(t *testing.T) {
	ws := newWorkspaceTree(t)
	require.NoError(t, MarkProvisioned(ProjectStateDir(ws)))
	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))

	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.Equal(t, LegacyPromotion{MarkerRemoved: true}, res)
	assert.FileExists(t, filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile))
	assert.NoFileExists(t, filepath.Join(ws, ProvisionSentinelFile))
}

// #5836 / AC-L6: the recurring lock-litter GC runs on the shared-plain
// pre-lock fast path too (every open of the state directory, not only a
// contended acquisition). It removes aged litter in the state directory,
// never the live lock or fresh litter, and never touches the workspace root.
func TestProvisionShared_FastPathSweepsStateDirLitter(t *testing.T) {
	ws, stateDir := stateDirLayout(t)
	require.NoError(t, MarkProvisioned(stateDir))
	agedEvict, freshEvict, agedStage, live := legacyRootLitter(t, stateDir)
	rootAged, _, _, rootLive := legacyRootLitter(t, ws)

	in := stateDirInput(ws, stateDir, filepath.Join(t.TempDir(), "missing.git"), store.SharingModeSharedPlain)
	in.GitClone = nil
	require.NoError(t, ProvisionShared(in))

	assert.NoDirExists(t, agedEvict)
	assert.NoDirExists(t, agedStage)
	assert.DirExists(t, freshEvict)
	assert.DirExists(t, live, "the live lock is never removed")
	assert.DirExists(t, rootAged, "the recurring GC never touches the workspace root")
	assert.DirExists(t, rootLive)
}

// The GC lists first and probes the server clock only when there is
// litter: a state directory with nothing to sweep gets no new entry.
func TestGarbageCollectLockLitter_NothingToSweepWritesNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, MarkProvisioned(dir))
	written := false
	orig := writeLockFile
	writeLockFile = func(path string, data []byte, perm os.FileMode) error {
		written = true
		return orig(path, data, perm)
	}
	t.Cleanup(func() { writeLockFile = orig })

	garbageCollectLockLitter(dir)
	assert.False(t, written, "no clock probe without litter")
	entries := mustReadDir(t, dir)
	require.Len(t, entries, 1)
	assert.Equal(t, ProvisionSentinelFile, entries[0].Name())
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return entries
}

// legacyMarkedTree is a workspace as a pre-state-dir image left it: no
// state-directory sentinel, the marker in the workspace root and its git
// exclude line between two other lines.
func legacyMarkedTree(t *testing.T) string {
	t.Helper()
	ws := newWorkspaceTree(t)
	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))
	info := filepath.Join(ws, ".git", "info")
	require.NoError(t, os.MkdirAll(info, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(info, "exclude"), []byte("/keep-me\n"+legacyExcludeLine+"\n/keep-me-too\n"), 0o644))
	return ws
}

// assertNotPromoted checks that a promotion attempt changed nothing: no
// state-directory sentinel, the workspace-root marker and its exclude line
// still there.
func assertNotPromoted(t *testing.T, ws string) {
	t.Helper()
	assert.NoFileExists(t, filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile), "no state-directory sentinel")
	assert.FileExists(t, filepath.Join(ws, ProvisionSentinelFile), "the workspace-root marker is untouched")
	assert.Contains(t, readExcludeLines(t, ws), legacyExcludeLine, "the exclude line is untouched")
}

// The promotion runs under the project's provisioning locks: while a
// current provisioner holds the state-directory lock, or the project-root
// lock, the promotion waits, and when its wait ends it fails and writes
// nothing (no sentinel; the marker and its exclude line stay). Once the
// lock is released, the promotion goes ahead.
func TestPromoteLegacyMarker_WaitsForProvisioningLock(t *testing.T) {
	withFastFileLockTiming(t)
	origRetries := fileLockRetries
	fileLockRetries = 5 // a lock budget of about 100ms
	t.Cleanup(func() { fileLockRetries = origRetries })

	for _, tc := range []struct {
		name string
		dir  func(ws string) string
	}{
		{"state-directory lock", ProjectStateDir},
		{"project-root lock", filepath.Dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := legacyMarkedTree(t)
			held, err := acquireFileLock(t.Context(), tc.dir(ws))
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, err = PromoteLegacyMarker(ctx, ws)
			require.Error(t, err, "the promotion must not run while the lock is held")
			assertNotPromoted(t, ws)

			require.NoError(t, held.release())
			res, err := PromoteLegacyMarker(t.Context(), ws)
			require.NoError(t, err)
			assert.True(t, res.Promoted, "after the release the promotion goes ahead")
			assertPromoted(t, ws)
		})
	}
}

// The state-directory sentinel is irreversible: when the provisioning lock
// is lost right before it would be written, the promotion fails and writes
// no sentinel, and leaves the marker and its exclude line in place.
func TestPromoteLegacyMarker_LostLockWritesNoSentinel(t *testing.T) {
	ws := legacyMarkedTree(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	orig := legacyPromoteBeforeSentinel
	legacyPromoteBeforeSentinel = func(context.Context) { cancel() }
	t.Cleanup(func() { legacyPromoteBeforeSentinel = orig })

	_, err := PromoteLegacyMarker(ctx, ws)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provisioning lock lost")
	assertNotPromoted(t, ws)
}

// A directory named like the marker is never removed, so it is not
// something to promote: the lock-free pre-check ignores it (no lock taken
// at every start), while a marker file or a symlink of that name counts.
func TestLegacyPromotionNeeded_DirectoryMarkerIgnored(t *testing.T) {
	ws := newWorkspaceTree(t)
	marker := filepath.Join(ws, ProvisionSentinelFile)
	require.NoError(t, os.Mkdir(marker, 0o755))
	assert.False(t, LegacyPromotionNeeded(ws), "a directory named like the marker is nothing to do")
	res, err := PromoteLegacyMarker(t.Context(), ws)
	require.NoError(t, err)
	assert.Equal(t, LegacyPromotion{}, res)
	assert.DirExists(t, marker, "the directory is left alone")

	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "target"), marker))
	assert.True(t, LegacyPromotionNeeded(ws), "a symlink named like the marker is removed, so it counts")
}
