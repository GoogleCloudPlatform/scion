//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These tests cover the agent-info lock (ptone/scion#4681). They use only
// the StatusHandler API and the lock file name, so they also run against
// the code before the lock was added, where each one fails an assertion.

// readCounter parses "<prefix>-<n>" and returns n, or -1 if absent.
func readCounter(v any, prefix string) int64 {
	s, _ := v.(string)
	var n int64
	if _, err := fmt.Sscanf(s, prefix+"-%d", &n); err != nil {
		return -1
	}
	return n
}

// TestStatusHandlerParallelWritersLoseNoField runs two StatusHandlers, as
// two container processes would (each has its own in-process mutex), on
// one agent-info.json. Writer A only sets detail.message, writer B only
// sets toolName, each to an increasing counter. Neither field may ever go
// back below the value its writer has finished writing: that would be the
// other writer writing back a stale read (a lost update).
func TestStatusHandlerParallelWritersLoseNoField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-info.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"template":"tpl","phase":"running"}`), 0o644))

	a := &StatusHandler{StatusPath: path}
	b := &StatusHandler{StatusPath: path}

	// A long lock wait (read only by test binaries, see
	// agentinfolock.TestWaitEnv) keeps both writers out of the unlocked
	// fallback however they interleave, so the result does not depend on
	// timing. The fallback is covered by the held-lock timeout test.
	//
	// With the lock this test is deterministic. Without it (the regression
	// it guards against) the failure is probabilistic: it needs two
	// read-modify-writes to overlap within n iterations per writer. With
	// tight loops that is very likely, and n is set high to make it more so,
	// but a pass is not proof the lock is present; TestUpdateHoldsLockDuringRMW
	// in pkg/agentinfolock is the deterministic check.
	t.Setenv("SCION_TEST_AGENTINFO_LOCK_WAIT", "10s")
	const n = 200
	var doneA, doneB atomic.Int64
	doneA.Store(-1)
	doneB.Store(-1)
	var lost atomic.Bool

	check := func(who string) {
		wantA, wantB := doneA.Load(), doneB.Load()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", who, err)
			return
		}
		var info map[string]any
		if err := json.Unmarshal(data, &info); err != nil {
			t.Errorf("%s: parse: %v", who, err)
			return
		}
		detail, _ := info["detail"].(map[string]any)
		gotA := readCounter(detail["message"], "a")
		gotB := readCounter(info["toolName"], "b")
		if (gotA < wantA || gotB < wantB || info["template"] != "tpl") && lost.CompareAndSwap(false, true) {
			t.Errorf("%s saw a lost update: message a-%d (finished a-%d), toolName b-%d (finished b-%d), template %v",
				who, gotA, wantA, gotB, wantB, info["template"])
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := int64(0); i < n && !lost.Load(); i++ {
			if err := a.SetMessage(fmt.Sprintf("a-%d", i)); err != nil {
				t.Errorf("A: %v", err)
				return
			}
			doneA.Store(i)
			check("A")
		}
	}()
	go func() {
		defer wg.Done()
		for i := int64(0); i < n && !lost.Load(); i++ {
			if err := b.UpdateActivity(state.ActivityExecuting, fmt.Sprintf("b-%d", i)); err != nil {
				t.Errorf("B: %v", err)
				return
			}
			doneB.Store(i)
			check("B")
		}
	}()
	wg.Wait()
	check("final")
}

// TestStatusHandlerSkipsNonRegularLockFile checks that a lock path the
// workload replaced with a symlink, FIFO or directory is not used: the
// update runs at once, unlocked, and the entry is left alone. The symlink
// points at a file this test holds locked, so following it would stall the
// update for the whole (10s) lock wait.
func TestStatusHandlerSkipsNonRegularLockFile(t *testing.T) {
	// A long lock wait: following the symlink to the held lock would stall
	// the update for 10s, far past the 5s bound below, so the check does not
	// depend on a tight wall-clock margin.
	t.Setenv("SCION_TEST_AGENTINFO_LOCK_WAIT", "10s")
	newHome := func(t *testing.T) (*StatusHandler, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "agent-info.json")
		h := &StatusHandler{StatusPath: path}
		// Baseline: a normal update creates a regular lock file.
		require.NoError(t, h.UpdateActivity(state.ActivityWorking, ""))
		fi, err := os.Lstat(path + ".lock")
		require.NoError(t, err, "a normal update must create the lock file")
		require.True(t, fi.Mode().IsRegular(), "lock file mode %v", fi.Mode())
		require.NoError(t, os.Remove(path+".lock"))
		return h, path
	}

	cases := []struct {
		name  string
		kind  string // file type word expected in the warning
		plant func(t *testing.T, lockPath string)
		check func(fi os.FileInfo) bool
	}{
		{
			name: "symlink to a held lock",
			kind: "symlink",
			plant: func(t *testing.T, lockPath string) {
				target := filepath.Join(t.TempDir(), "held")
				f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0o644)
				require.NoError(t, err)
				t.Cleanup(func() { _ = f.Close() })
				require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
				require.NoError(t, os.Symlink(target, lockPath))
			},
			check: func(fi os.FileInfo) bool { return fi.Mode()&os.ModeSymlink != 0 },
		},
		{
			name: "fifo",
			kind: "fifo",
			plant: func(t *testing.T, lockPath string) {
				require.NoError(t, syscall.Mkfifo(lockPath, 0o644))
			},
			check: func(fi os.FileInfo) bool { return fi.Mode()&os.ModeNamedPipe != 0 },
		},
		{
			name: "directory",
			kind: "directory",
			plant: func(t *testing.T, lockPath string) {
				require.NoError(t, os.Mkdir(lockPath, 0o755))
			},
			check: func(fi os.FileInfo) bool { return fi.IsDir() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, path := newHome(t)
			tc.plant(t, path+".lock")
			logPath := filepath.Join(t.TempDir(), "agent.log")
			log.SetLogPath(logPath)
			// Put back the unset default so later tests do not log to
			// this subtest's deleted temp file.
			t.Cleanup(func() { log.SetLogPath("") })

			start := time.Now()
			require.NoError(t, h.SetMessage("skipped"))
			elapsed := time.Since(start)
			assert.Less(t, elapsed, 5*time.Second, "update must not wait on a non-regular lock path")

			info := readInfo(t, path)
			detail, _ := info["detail"].(map[string]any)
			assert.Equal(t, "skipped", detail["message"])
			assert.Equal(t, string(state.ActivityWorking), info["activity"])

			fi, err := os.Lstat(path + ".lock")
			require.NoError(t, err)
			assert.True(t, tc.check(fi), "lock path changed: mode %v", fi.Mode())

			logData, err := os.ReadFile(logPath)
			require.NoError(t, err)
			assert.Contains(t, string(logData), "[WARN] agent-info lock file is not a single-link regular file ("+tc.kind+"), writing without lock: "+path+".lock")
		})
	}
}

// TestStatusHandlerHeldLockTimesOutAndStillWrites holds the lock in this
// test and checks that an update first waits for it, then gives up after a
// short bounded wait and writes anyway.
func TestStatusHandlerHeldLockTimesOutAndStillWrites(t *testing.T) {
	// Use the default 200ms wait even if the test env wait is exported.
	t.Setenv("SCION_TEST_AGENTINFO_LOCK_WAIT", "")
	path := filepath.Join(t.TempDir(), "agent-info.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"template":"tpl"}`), 0o644))

	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o666)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))

	h := &StatusHandler{StatusPath: path}
	start := time.Now()
	require.NoError(t, h.UpdatePhase(state.PhaseRunning, state.ActivityThinking, ""))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "update must wait for the held lock first")
	assert.Less(t, elapsed, 2*time.Second, "the wait must stay short and bounded")

	info := readInfo(t, path)
	assert.Equal(t, string(state.PhaseRunning), info["phase"])
	assert.Equal(t, string(state.ActivityThinking), info["activity"])
	assert.Equal(t, "tpl", info["template"])
}

func readInfo(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var info map[string]any
	require.NoError(t, json.Unmarshal(data, &info))
	return info
}
