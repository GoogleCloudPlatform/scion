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

package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
)

// setVSCodeLockWait shortens the vscode lock wait for one test.
func setVSCodeLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := vscodeLockWait
	vscodeLockWait = d
	t.Cleanup(func() { vscodeLockWait = prev })
}

// The vscode directory lock wait is bounded. With the
// lock held (in-process: filelock locks are per open file, so a second
// Acquire waits like another process would), a second lockVSCodeDir fails
// after the wait with an error wrapping filelock.ErrTimeout instead of
// blocking forever. Control: after release it is taken at once.
func TestLockVSCodeDir_TimesOut(t *testing.T) {
	setVSCodeLockWait(t, 50*time.Millisecond)
	p := vscodePaths{home: t.TempDir()}
	require.NoError(t, ensureSSHDir(p.home, vscodeDirRel))
	unlock, err := lockVSCodeDir(p.lockFile())
	require.NoError(t, err)

	start := time.Now()
	_, err = lockVSCodeDir(p.lockFile())
	require.Error(t, err)
	assert.True(t, errors.Is(err, filelock.ErrTimeout), "%v", err)
	assert.Less(t, time.Since(start), 30*time.Second, "the wait is bounded")

	unlock()
	unlock2, err := lockVSCodeDir(p.lockFile())
	require.NoError(t, err, "control: the released lock is taken")
	unlock2()
}

// Contention: a waiter blocked on a held lock gets it once the holder
// releases within the wait (run on Windows too, where the lock is
// LockFileEx).
func TestLockVSCodeDir_WaitsForRelease(t *testing.T) {
	setVSCodeLockWait(t, 10*time.Second)
	p := vscodePaths{home: t.TempDir()}
	require.NoError(t, ensureSSHDir(p.home, vscodeDirRel))
	unlock, err := lockVSCodeDir(p.lockFile())
	require.NoError(t, err)
	got := make(chan error, 1)
	go func() {
		u, err := lockVSCodeDir(p.lockFile())
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the waiter got the lock while it was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter did not get the released lock")
	}
}

// The default wait is two minutes.
func TestLockVSCodeDir_DefaultWait(t *testing.T) {
	assert.Equal(t, 2*time.Minute, vscodeLockWait)
}
