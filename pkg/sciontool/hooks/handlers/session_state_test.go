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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"golang.org/x/sys/unix"
)

// hookRun handles one event the way a `sciontool hook` process does: with a
// new handler whose state lives in the store. It returns the summary if the
// event ended the session.
func hookRun(t *testing.T, store SessionStateStore, ev *hooks.Event) *telemetry.SessionSummary {
	t.Helper()
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = store
	var got *telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = &s }
	if err := h.Handle(ev); err != nil {
		t.Fatalf("Handle(%s): %v", ev.Name, err)
	}
	return got
}

func toolEvent(sessionID, tool string) *hooks.Event {
	ev := sessionEvent(hooks.EventToolEnd, sessionID)
	ev.Data.ToolName = tool
	return ev
}

func TestFileSessionState_CarriesCountsAcrossHandlers(t *testing.T) {
	store := NewFileSessionState(t.TempDir())

	model := sessionEvent(hooks.EventModelEnd, "s1")
	model.Data.InputTokens = 10
	model.Data.OutputTokens = 3
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s1"),
		toolEvent("s1", "Bash"),
		model,
		sessionEvent(hooks.EventAgentEnd, "s1"),
	} {
		if s := hookRun(t, store, ev); s != nil {
			t.Fatalf("%s ended the session", ev.Name)
		}
	}

	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("state file mode = %o, want 600", mode)
	}

	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if s.SessionID != "s1" || s.TurnCount != 1 || s.APICallCount != 1 ||
		s.TokensInput != 10 || s.TokensOutput != 3 || s.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("summary = %+v", *s)
	}
	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Errorf("state file not removed after session-end: %v", err)
	}
}

// Parallel hook processes (for example PostToolUse for parallel tool calls)
// must serialize on the lock and lose no counts.
func TestFileSessionState_ConcurrentRunsLoseNoCounts(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine gets its own store value, as separate
			// processes would, so each opens its own lock file description.
			hookRun(t, &FileSessionState{Path: store.Path}, toolEvent("s1", "Bash"))
		}()
	}
	wg.Wait()

	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if got := s.ToolCalls["Bash"].Calls; got != n {
		t.Errorf("Bash calls = %d, want %d", got, n)
	}
}

func TestFileSessionState_UnusableFileStartsFresh(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"wrong version": `{"version":99,"aggregator":{"session_id":"old","open":true,"turn_count":7}}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := NewFileSessionState(t.TempDir())
			if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.Path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			hookRun(t, store, toolEvent("s2", "Read"))
			s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s2"))
			if s == nil {
				t.Fatal("session-end produced no summary")
			}
			if s.SessionID != "s2" || s.TurnCount != 0 || s.ToolCalls["Read"].Calls != 1 {
				t.Errorf("summary = %+v, want fresh session s2 with one Read", *s)
			}
		})
	}
}

// A session-start with a new ID replaces a stale, unreported session.
func TestFileSessionState_NewSessionStartResetsStaleState(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "old"))
	hookRun(t, store, toolEvent("old", "Bash"))
	hookRun(t, store, sessionEvent(hooks.EventAgentEnd, "old"))

	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "new"))
	hookRun(t, store, toolEvent("new", "Read"))
	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "new"))
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if s.SessionID != "new" || s.TurnCount != 0 || s.ToolCalls["Bash"].Calls != 0 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("summary = %+v, want only the new session's counts", *s)
	}
}

// When the lock cannot be taken, the event is still counted in memory and
// the hook does not fail.
func TestFileSessionState_LockUnavailableStillApplies(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(store.Path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	agg := telemetry.NewAggregator()
	applied := 0
	err = store.Update(agg, toolEvent("s1", "Bash"), func() bool { applied++; return false })
	if err == nil {
		t.Error("Update succeeded while the lock was held elsewhere")
	}
	if applied != 1 {
		t.Errorf("apply called %d times, want 1", applied)
	}
	if _, statErr := os.Stat(store.Path); !os.IsNotExist(statErr) {
		t.Errorf("state written without the lock: %v", statErr)
	}
}

// A session that never got its session-end (harness killed) must not be
// merged into the next session when that session's session-start is also
// missed: any event with a different session ID discards the stale state.
func TestFileSessionState_DifferentSessionIDDiscardsStaleStateWithoutStart(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "old"))
	hookRun(t, store, toolEvent("old", "Bash"))
	hookRun(t, store, sessionEvent(hooks.EventAgentEnd, "old"))
	// No session-end for "old", and no session-start for "new".

	hookRun(t, store, toolEvent("new", "Read"))
	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "new"))
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if s.SessionID != "new" {
		t.Errorf("SessionID = %q, want new", s.SessionID)
	}
	if s.TurnCount != 0 || s.ToolCalls["Bash"].Calls != 0 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("summary = %+v, want only the new session's counts", *s)
	}
}

// Events without a session ID cannot be attributed elsewhere, so they keep
// counting into the persisted session.
func TestFileSessionState_EventWithoutSessionIDKeepsState(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))
	hookRun(t, store, toolEvent("", "Bash"))
	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if s == nil || s.SessionID != "s1" || s.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("summary = %+v, want s1 with one Bash", s)
	}
}

// If the lock cannot be taken on session-end, the summary would be built
// without the session's persisted counts, so it is not reported.
func TestFileSessionState_LockUnavailableOnSessionEndSkipsReport(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))
	hookRun(t, store, toolEvent("s1", "Bash"))

	held, err := os.OpenFile(store.Path+".lock", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	if s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1")); s != nil {
		t.Errorf("reported summary %+v while the state was unavailable", *s)
	}
	if _, err := os.Stat(store.Path); err != nil {
		t.Errorf("state file should be left in place: %v", err)
	}
}
