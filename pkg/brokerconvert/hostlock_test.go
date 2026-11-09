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

//go:build !windows

package brokerconvert

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostLock_ExclusiveAndReleased(t *testing.T) {
	g := t.TempDir()
	h, err := AcquireHostLock(g)
	require.NoError(t, err)

	_, err = AcquireHostLock(g)
	assert.ErrorIs(t, err, ErrHostLockHeld)

	h.Release()
	h.Release() // idempotent
	h2, err := AcquireHostLock(g)
	require.NoError(t, err)
	h2.Release()

	fi, err := os.Lstat(HostLockPath(g))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// Named mutant target: server start runs while the lock is held. Test (b):
// a server start refuses while a convert operation holds the lock.
func TestCheckServerStart_LockHeld(t *testing.T) {
	g := t.TempDir()
	h, err := AcquireHostLock(g)
	require.NoError(t, err)
	defer h.Release()

	s, err := AcquireServerStartLock(g)
	assert.ErrorIs(t, err, ErrHostLockHeld)
	assert.Nil(t, s)
}

// Test (a): a convert operation refuses while a server start holds the lock
// (from its check until its listeners are bound).
func TestAcquireHostLock_RefusesWhileServerStartHoldsIt(t *testing.T) {
	g := t.TempDir()
	s, err := AcquireServerStartLock(g)
	require.NoError(t, err)

	_, err = AcquireHostLock(g)
	assert.ErrorIs(t, err, ErrHostLockHeld)

	// After the bind the start releases it, and convert can take it (its
	// server-running check then finds the server: test (c), in cmd).
	s.Release()
	h, err := AcquireHostLock(g)
	require.NoError(t, err)
	h.Release()
}

func TestCheckServerStart_JournalStates(t *testing.T) {
	t.Run("no journal passes", func(t *testing.T) {
		s, err := AcquireServerStartLock(t.TempDir())
		require.NoError(t, err)
		s.Release()
	})
	t.Run("a started item refuses and names resume and rollback", func(t *testing.T) {
		g := t.TempDir()
		j := newTestJournal(t)
		j.Settings.State = StepStarted
		require.NoError(t, SaveJournal(g, j, testNow))
		_, err := AcquireServerStartLock(g)
		assert.ErrorIs(t, err, ErrConvertInterrupted)
		assert.Contains(t, err.Error(), "convert --apply")
		assert.Contains(t, err.Error(), "convert --rollback")
		// The refusal releases the lock.
		h, err := AcquireHostLock(g)
		require.NoError(t, err)
		h.Release()
	})
	t.Run("an applied journal is the switch and passes", func(t *testing.T) {
		g := t.TempDir()
		j := newTestJournal(t)
		j.State = JournalApplied
		j.Settings.State = StepDone
		j.Switch.State = StepDone
		j.Register = []RegisterItem{{Key: "k", ItemStatus: ItemStatus{State: StepDone}}}
		j.Projects = []ProjectItem{{ProjectID: "p", ItemStatus: ItemStatus{State: StepDone}}}
		require.NoError(t, SaveJournal(g, j, testNow))
		s, err := AcquireServerStartLock(g)
		require.NoError(t, err)
		s.Release()
	})
	t.Run("an unreadable journal fails closed", func(t *testing.T) {
		g := t.TempDir()
		require.NoError(t, os.MkdirAll(ConvertDir(g), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(ConvertDir(g), JournalFileName), []byte("{"), 0o600))
		_, err := AcquireServerStartLock(g)
		assert.ErrorIs(t, err, ErrJournalUnreadable)
	})
}

func TestHostLock_RefusesLinkAtLockPath(t *testing.T) {
	g := t.TempDir()
	require.NoError(t, os.MkdirAll(ConvertDir(g), 0o700))
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	require.NoError(t, os.Symlink(target, HostLockPath(g)))
	_, err := AcquireHostLock(g)
	assert.Error(t, err)
}
