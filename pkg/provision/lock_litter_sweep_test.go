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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releasedLockEvict acquires and releases the provisioning lock in dir, as
// every lock user does, and returns the ".evict-*" entry the release
// leaves.
func releasedLockEvict(t *testing.T, dir string) string {
	t.Helper()
	held, err := acquireFileLockWithin(context.Background(), dir, testLockWait)
	require.NoError(t, err)
	require.NoError(t, held.release())
	var evicts []string
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".evict-") {
			evicts = append(evicts, e.Name())
		}
	}
	require.Len(t, evicts, 1, "a release leaves exactly one .evict entry")
	return filepath.Join(dir, evicts[0])
}

func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, old, old))
}

// A release always leaves an ".evict-*" entry. It is kept while younger
// than the cutoff (pending reports the time left) and swept once its
// eviction marker is older.
func TestSweepLockLitter_EvictAgedByMarker(t *testing.T) {
	dir := t.TempDir()
	evict := releasedLockEvict(t, dir)

	left, err := SweepLockLitter(dir)
	require.NoError(t, err)
	assert.DirExists(t, evict, "a young .evict entry is kept")
	assert.Greater(t, left.Pending, time.Duration(0))
	assert.LessOrEqual(t, left.Pending, LockLitterMinAge())
	assert.Equal(t, 0, left.Unaged)

	// The directory's own mtime does not count: only the marker's.
	backdate(t, evict, 24*time.Hour)
	_, err = SweepLockLitter(dir)
	require.NoError(t, err)
	assert.DirExists(t, evict, "the entry's own mtime never ages it")

	backdate(t, filepath.Join(evict, provisionLockEvictedAtFile), LockLitterMinAge()+time.Minute)
	left, err = SweepLockLitter(dir)
	require.NoError(t, err)
	assert.NoDirExists(t, evict)
	assert.Equal(t, LockLitter{}, left)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing else is left, not even a clock probe")
}

// An ".evict-*" entry without its marker is never guessed at: kept and
// counted as unaged, with no pending wait (waiting would not age it).
func TestSweepLockLitter_EvictWithoutMarkerUnaged(t *testing.T) {
	dir := t.TempDir()
	evict := filepath.Join(dir, provisionFileLockName+".evict-abc")
	require.NoError(t, os.Mkdir(evict, 0o755))
	backdate(t, evict, 24*time.Hour)

	left, err := SweepLockLitter(dir)
	require.NoError(t, err)
	assert.DirExists(t, evict)
	assert.Equal(t, LockLitter{Unaged: 1}, left)
}

// The exported sweep's cutoff is LockLitterMinAge: litter just past a
// shorter age is kept by it, and removed by the test seam with that age.
func TestSweepLockLitter_CutoffIsFixed(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, provisionFileLockName+".stage-x")
	require.NoError(t, os.Mkdir(stage, 0o755))
	backdate(t, stage, time.Minute)

	_, err := SweepLockLitter(dir)
	require.NoError(t, err)
	assert.DirExists(t, stage, "younger than LockLitterMinAge")
	_, err = sweepLockLitter(dir, 30*time.Second)
	require.NoError(t, err)
	assert.NoDirExists(t, stage)
}

// Staging directories and leaked clock probes are aged from their own
// mtime; the live lock and anything not under the lock's prefix are never
// touched.
func TestSweepLockLitter_OtherLitterAndUntouched(t *testing.T) {
	dir := t.TempDir()
	oldStage := filepath.Join(dir, provisionFileLockName+".stage-old")
	youngStage := filepath.Join(dir, provisionFileLockName+".stage-young")
	oldProbe := filepath.Join(dir, provisionClockProbeFile+"-old")
	live := filepath.Join(dir, provisionFileLockName)
	other := filepath.Join(dir, "workspace")
	for _, d := range []string{oldStage, youngStage, live, other} {
		require.NoError(t, os.Mkdir(d, 0o755))
	}
	require.NoError(t, os.WriteFile(oldProbe, []byte("1"), 0o600))
	for _, p := range []string{oldStage, oldProbe, live, other} {
		backdate(t, p, 24*time.Hour)
	}

	left, err := SweepLockLitter(dir)
	require.NoError(t, err)
	assert.NoDirExists(t, oldStage)
	assert.NoFileExists(t, oldProbe)
	assert.DirExists(t, youngStage)
	assert.DirExists(t, live, "the live lock is never swept")
	assert.DirExists(t, other, "entries outside the lock's prefix are never swept")
	assert.Greater(t, left.Pending, time.Duration(0))
}

// A missing directory, or one without litter, is nothing to do.
func TestSweepLockLitter_Nothing(t *testing.T) {
	left, err := SweepLockLitter(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	assert.Equal(t, LockLitter{}, left)

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "workspace"), 0o755))
	left, err = SweepLockLitter(dir)
	require.NoError(t, err)
	assert.Equal(t, LockLitter{}, left)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no clock probe is written when there is no litter")
}

// RemoveAgentWorkspaceOutcome reports whether a workspace was moved aside.
func TestRemoveAgentWorkspaceOutcome(t *testing.T) {
	agentDir := t.TempDir()
	removed, err := RemoveAgentWorkspaceOutcome(context.Background(), agentDir, testLockWait)
	require.NoError(t, err)
	assert.False(t, removed, "no workspace")

	require.NoError(t, os.Mkdir(filepath.Join(agentDir, AgentWorkspaceDir), 0o755))
	removed, err = RemoveAgentWorkspaceOutcome(context.Background(), agentDir, testLockWait)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoDirExists(t, filepath.Join(agentDir, AgentWorkspaceDir))

	removed, err = RemoveAgentWorkspaceOutcome(context.Background(), filepath.Join(agentDir, "missing"), testLockWait)
	require.NoError(t, err)
	assert.False(t, removed, "no agent directory")
}
