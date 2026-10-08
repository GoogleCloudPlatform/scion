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

package handlers

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// openSession leaves the store with an open session s1 holding one Bash
// call, one Read call and one turn, as hook processes would.
func openSession(t *testing.T, store *FileSessionState) {
	t.Helper()
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s1"),
		toolEvent("s1", "Bash"),
		toolEvent("s1", "Read"),
		sessionEvent(hooks.EventAgentEnd, "s1"),
	} {
		if s := hookRun(t, store, ev); s != nil {
			t.Fatalf("%s ended the session", ev.Name)
		}
	}
}

func closeOpen(t *testing.T, store *FileSessionState, errMsg string) (telemetry.SessionSummary, bool) {
	t.Helper()
	s, ok, err := store.CloseOpenSession(errMsg)
	if err != nil {
		t.Fatalf("CloseOpenSession: %v", err)
	}
	return s, ok
}

// A session still open at shutdown is finalized once, with its ID and
// counts. The file becomes a tombstone, so a second shutdown check and a
// session-end hook still in flight both report nothing; the next session
// starts clean.
func TestCloseOpenSession_OpenSessionClosedOnce(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)

	s, ok := closeOpen(t, store, "")
	if !ok {
		t.Fatal("open session not returned")
	}
	if s.SessionID != "s1" || s.Status != "completed" || s.TurnCount != 1 ||
		s.ToolCalls["Bash"].Calls != 1 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("summary = %+v", s)
	}
	if s.StartedAt.IsZero() || s.EndedAt.Before(s.StartedAt) {
		t.Errorf("times = %v..%v", s.StartedAt, s.EndedAt)
	}

	if _, ok := closeOpen(t, store, ""); ok {
		t.Error("closed session returned a second time")
	}

	// In-flight hooks for the closed session: ignored, nothing reported.
	if got := hookRun(t, store, toolEvent("s1", "Bash")); got != nil {
		t.Errorf("tool-end reported %+v", *got)
	}
	if got := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1")); got != nil {
		t.Errorf("late session-end reported the closed session again: %+v", *got)
	}
	if _, ok := closeOpen(t, store, ""); ok {
		t.Error("late hook events reopened the closed session")
	}

	// A new session replaces the tombstone and counts from zero.
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s2"))
	hookRun(t, store, toolEvent("s2", "Grep"))
	got := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s2"))
	if got == nil || got.SessionID != "s2" || got.TurnCount != 0 ||
		got.ToolCalls["Grep"].Calls != 1 || got.ToolCalls["Bash"].Calls != 0 {
		t.Errorf("next session summary = %+v", got)
	}
}

// The tombstone keeps the state file's ownership and mode: it is written in
// place, not replaced by a root-created file.
func TestCloseOpenSession_TombstoneWrittenInPlace(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	before, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := closeOpen(t, store, ""); !ok {
		t.Fatal("open session not returned")
	}
	after, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Errorf("state file replaced or mode changed: same=%v mode=%o", os.SameFile(before, after), after.Mode().Perm())
	}
}

// A session-start with the closed session's ID (a resumed harness session)
// starts a new segment rather than being ignored.
func TestCloseOpenSession_SameIDSessionStartAfterCloseCountsAgain(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	if _, ok := closeOpen(t, store, ""); !ok {
		t.Fatal("open session not returned")
	}
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))
	hookRun(t, store, toolEvent("s1", "Edit"))
	got := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if got == nil || got.ToolCalls["Edit"].Calls != 1 || got.ToolCalls["Bash"].Calls != 0 {
		t.Errorf("resumed segment summary = %+v", got)
	}
}

func TestCloseOpenSession_ErrorStatus(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	s, ok := closeOpen(t, store, "Agent crashed with exit code 2")
	if !ok || s.Status != "error" {
		t.Errorf("ok=%v status=%q, want error", ok, s.Status)
	}
}

// A session the session-end hook already finalized is not returned.
func TestCloseOpenSession_AlreadyReportedByHook(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	if s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1")); s == nil {
		t.Fatal("hook session-end produced no summary")
	}
	if s, ok := closeOpen(t, store, ""); ok {
		t.Errorf("already-reported session returned again: %+v", s)
	}
}

func TestCloseOpenSession_NoState(t *testing.T) {
	t.Run("no directory", func(t *testing.T) {
		store := NewFileSessionState(filepath.Join(t.TempDir(), "missing"))
		if _, ok := closeOpen(t, store, ""); ok {
			t.Error("returned a session without any state")
		}
	})
	t.Run("lock but no state file", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		mustMkdir(t, filepath.Dir(store.Path))
		mustWrite(t, store.Path+".lock", "")
		if _, ok := closeOpen(t, store, ""); ok {
			t.Error("returned a session without a state file")
		}
	})
	t.Run("state file but no lock file", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		openSession(t, store)
		if err := os.Remove(store.Path + ".lock"); err != nil {
			t.Fatal(err)
		}
		if _, ok := closeOpen(t, store, ""); ok {
			t.Error("returned a session without taking the hook lock")
		}
		if _, err := os.Lstat(store.Path + ".lock"); !os.IsNotExist(err) {
			t.Errorf("lock file created by the shutdown check: %v", err)
		}
	})
	t.Run("corrupt state", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		openSession(t, store)
		mustWrite(t, store.Path, "{not json")
		if _, ok, err := store.CloseOpenSession(""); ok || err == nil {
			t.Errorf("ok=%v err=%v, want an error and no session", ok, err)
		}
	})
}

// A symlink anywhere on the path is refused, never followed, and its target
// is left untouched.
func TestCloseOpenSession_SymlinksRefused(t *testing.T) {
	// targetState is a valid open-session file outside the agent's directory.
	writeTarget := func(t *testing.T) (dir string, state string) {
		t.Helper()
		src := NewFileSessionState(t.TempDir())
		openSession(t, src)
		return filepath.Dir(src.Path), src.Path
	}
	unchanged := func(t *testing.T, path string, want []byte) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("symlink target %s modified", path)
		}
	}

	t.Run("state file", func(t *testing.T) {
		_, target := writeTarget(t)
		want, _ := os.ReadFile(target)
		store := NewFileSessionState(t.TempDir())
		mustMkdir(t, filepath.Dir(store.Path))
		mustWrite(t, store.Path+".lock", "")
		if err := os.Symlink(target, store.Path); err != nil {
			t.Fatal(err)
		}
		_, ok, err := store.CloseOpenSession("")
		if ok || !errors.Is(err, ErrSessionStateRefused) {
			t.Errorf("ok=%v err=%v, want ErrSessionStateRefused", ok, err)
		}
		unchanged(t, target, want)
	})
	t.Run("lock file", func(t *testing.T) {
		dir, _ := writeTarget(t)
		store := NewFileSessionState(t.TempDir())
		mustMkdir(t, filepath.Dir(store.Path))
		if err := os.Symlink(filepath.Join(dir, SessionStateFileName+".lock"), store.Path+".lock"); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, store.Path, "unused")
		_, ok, err := store.CloseOpenSession("")
		if ok || !errors.Is(err, ErrSessionStateRefused) {
			t.Errorf("ok=%v err=%v, want ErrSessionStateRefused", ok, err)
		}
	})
	t.Run("state directory", func(t *testing.T) {
		dir, target := writeTarget(t)
		want, _ := os.ReadFile(target)
		home := t.TempDir()
		if err := os.Symlink(dir, filepath.Join(home, ".scion")); err != nil {
			t.Fatal(err)
		}
		store := NewFileSessionState(home)
		_, ok, err := store.CloseOpenSession("")
		if ok || !errors.Is(err, ErrSessionStateRefused) {
			t.Errorf("ok=%v err=%v, want ErrSessionStateRefused", ok, err)
		}
		unchanged(t, target, want)
	})
}

// A FIFO or a hard link at the state path is refused without blocking.
func TestCloseOpenSession_NonRegularRefused(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		mustMkdir(t, filepath.Dir(store.Path))
		mustWrite(t, store.Path+".lock", "")
		if err := syscall.Mkfifo(store.Path, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, _, err := store.CloseOpenSession(""); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrSessionStateRefused) {
				t.Errorf("err = %v, want ErrSessionStateRefused", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CloseOpenSession blocked on a FIFO")
		}
	})
	t.Run("hard link", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		openSession(t, store)
		if err := os.Link(store.Path, filepath.Join(t.TempDir(), "other")); err != nil {
			t.Skipf("hard link not possible here: %v", err)
		}
		_, ok, err := store.CloseOpenSession("")
		if ok || !errors.Is(err, ErrSessionStateRefused) {
			t.Errorf("ok=%v err=%v, want ErrSessionStateRefused", ok, err)
		}
	})
}

// blockingStore delays the session-end hook's apply, which runs under the
// state lock, until release is closed.
type blockingStore struct {
	inner   *FileSessionState
	inApply chan struct{}
	release chan struct{}
}

func (b *blockingStore) Update(agg *telemetry.Aggregator, event *hooks.Event, apply func() bool) error {
	return b.inner.Update(agg, event, func() bool {
		close(b.inApply)
		<-b.release
		return apply()
	})
}

// A hook process handling session-end holds the lock while the shutdown
// check runs. The check waits for the lock, then finds the session already
// finalized: exactly one report, from the hook.
func TestCloseOpenSession_HookHoldingLockNoDoubleReport(t *testing.T) {
	dir := t.TempDir()
	openSession(t, NewFileSessionState(dir))

	hookStore := &blockingStore{inner: NewFileSessionState(dir), inApply: make(chan struct{}), release: make(chan struct{})}
	hookDone := make(chan *telemetry.SessionSummary, 1)
	go func() {
		h := NewTelemetryHandler(nil, nil, nil)
		h.SessionState = hookStore
		var got *telemetry.SessionSummary
		h.OnSessionEnd = func(s telemetry.SessionSummary) { got = &s }
		_ = h.Handle(sessionEvent(hooks.EventSessionEnd, "s1"))
		hookDone <- got
	}()
	<-hookStore.inApply

	shutdownStore := NewFileSessionState(dir)
	shutdownStore.lockTimeout = testLockTimeoutGenerous
	type result struct {
		ok  bool
		err error
	}
	shutdownDone := make(chan result, 1)
	go func() {
		_, ok, err := shutdownStore.CloseOpenSession("")
		shutdownDone <- result{ok, err}
	}()

	select {
	case r := <-shutdownDone:
		t.Fatalf("shutdown check finished while the hook held the lock: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(hookStore.release)

	if got := <-hookDone; got == nil || got.SessionID != "s1" || got.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("hook summary = %+v, want s1 with its counts", got)
	}
	r := <-shutdownDone
	if r.err != nil || r.ok {
		t.Errorf("shutdown check ok=%v err=%v, want nothing to report", r.ok, r.err)
	}
}

// A lock held past the wait bound makes the check give up without reading
// or changing the state.
func TestCloseOpenSession_LockTimeout(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	store.lockTimeout = testLockTimeoutShort
	held, err := os.OpenFile(store.Path+".lock", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.Path)
	start := time.Now()
	if _, ok, err := store.CloseOpenSession(""); ok || err == nil {
		t.Errorf("ok=%v err=%v, want a lock timeout", ok, err)
	}
	if elapsed := time.Since(start); elapsed >= testLockElapsedMax {
		t.Errorf("waited %s, want about %s", elapsed, testLockTimeoutShort)
	}
	after, _ := os.ReadFile(store.Path)
	if string(before) != string(after) {
		t.Error("state changed without the lock")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A harness with no session-start (for example one resumed by conversation
// ID) reuses the session ID after a restart. Once init clears the previous
// shutdown's tombstone at startup, the resumed segment is counted and
// reported at the next shutdown, with only its own counts.
func TestClearSessionTombstone_ResumeWithSameIDReportedAgain(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, toolEvent("c1", "Bash"))
	hookRun(t, store, sessionEvent(hooks.EventAgentEnd, "c1"))
	if s, ok := closeOpen(t, store, ""); !ok || s.SessionID != "c1" {
		t.Fatalf("first segment: ok=%v summary=%+v", ok, s)
	}

	// Restart: init clears the tombstone before the harness starts.
	cleared, err := store.ClearSessionTombstone()
	if err != nil || !cleared {
		t.Fatalf("ClearSessionTombstone = %v, %v; want true, nil", cleared, err)
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		t.Errorf("tombstone still present: %v", err)
	}

	hookRun(t, store, toolEvent("c1", "Read"))
	hookRun(t, store, sessionEvent(hooks.EventAgentEnd, "c1"))
	s, ok := closeOpen(t, store, "")
	if !ok {
		t.Fatal("resumed segment with the same ID not reported")
	}
	if s.SessionID != "c1" || s.TurnCount != 1 || s.ToolCalls["Read"].Calls != 1 || s.ToolCalls["Bash"].Calls != 0 {
		t.Errorf("resumed segment summary = %+v, want only its own counts", s)
	}
}

// ClearSessionTombstone touches only a tombstone.
func TestClearSessionTombstone_LeavesOtherStateAlone(t *testing.T) {
	t.Run("no state", func(t *testing.T) {
		store := NewFileSessionState(filepath.Join(t.TempDir(), "missing"))
		if cleared, err := store.ClearSessionTombstone(); cleared || err != nil {
			t.Errorf("= %v, %v; want false, nil", cleared, err)
		}
	})
	t.Run("open session", func(t *testing.T) {
		store := NewFileSessionState(t.TempDir())
		openSession(t, store)
		before, _ := os.ReadFile(store.Path)
		if cleared, err := store.ClearSessionTombstone(); cleared || err != nil {
			t.Errorf("= %v, %v; want false, nil", cleared, err)
		}
		if after, _ := os.ReadFile(store.Path); string(after) != string(before) {
			t.Error("open session state changed")
		}
	})
	t.Run("symlinked state file", func(t *testing.T) {
		src := NewFileSessionState(t.TempDir())
		openSession(t, src)
		if _, ok := closeOpen(t, src, ""); !ok {
			t.Fatal("setup: no session closed")
		}
		store := NewFileSessionState(t.TempDir())
		mustMkdir(t, filepath.Dir(store.Path))
		mustWrite(t, store.Path+".lock", "")
		if err := os.Symlink(src.Path, store.Path); err != nil {
			t.Fatal(err)
		}
		cleared, err := store.ClearSessionTombstone()
		if cleared || !errors.Is(err, ErrSessionStateRefused) {
			t.Errorf("= %v, %v; want false, ErrSessionStateRefused", cleared, err)
		}
		if _, err := os.Lstat(store.Path); err != nil {
			t.Errorf("symlink removed: %v", err)
		}
		if _, err := os.Stat(src.Path); err != nil {
			t.Errorf("symlink target removed: %v", err)
		}
	})
}

// A session without an ID is never tombstoned: nothing is returned (the
// Hub would refuse it), its state is removed as on the hook path, and later
// ID-less events open and count a new session.
func TestCloseOpenSession_EmptySessionIDNotTombstoned(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, toolEvent("", "Bash"))
	if s, ok := closeOpen(t, store, ""); ok {
		t.Fatalf("empty-ID session returned: %+v", s)
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		t.Fatalf("empty-ID state left behind: %v", err)
	}

	hookRun(t, store, toolEvent("", "Read"))
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("later ID-less event not recorded: %v", err)
	}
	file, err := decodeSessionState(data)
	if err != nil {
		t.Fatal(err)
	}
	if file.Closed || !file.Aggregator.Open || file.Aggregator.ToolCalls["Read"].Calls != 1 || file.Aggregator.ToolCalls["Bash"].Calls != 0 {
		t.Errorf("state after later ID-less event = %+v", file)
	}
}
