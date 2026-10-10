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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

func addUsage(t *testing.T, store *FileSessionState, u telemetry.SessionUsage) bool {
	t.Helper()
	added, err := store.AddUsage(u)
	if err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	return added
}

// With native usage, the init daemon's natively derived usage accumulates
// in the persisted state between separate hook processes and reaches the
// session-end summary. Hook model-end usage and session-end token totals
// are left out, so no call is counted twice, and turn and tool counts are
// the same as without usage.
func TestAddUsage_NativeUsageAccumulatesAcrossHookProcesses(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "native")
	store := NewFileSessionState(t.TempDir())

	openSession(t, store) // s1: Bash, Read, one turn
	if !addUsage(t, store, telemetry.SessionUsage{Calls: 1, TokensInput: 100, TokensOutput: 20, TokensCached: 300}) {
		t.Fatal("usage not added to the open session")
	}

	model := sessionEvent(hooks.EventModelEnd, "s1")
	model.Data.InputTokens = 9999
	model.Data.OutputTokens = 9999
	for _, ev := range []*hooks.Event{toolEvent("s1", "Bash"), model, sessionEvent(hooks.EventAgentEnd, "s1")} {
		if s := hookRun(t, store, ev); s != nil {
			t.Fatalf("%s ended the session", ev.Name)
		}
	}
	if !addUsage(t, store, telemetry.SessionUsage{Calls: 2, TokensInput: 50, TokensOutput: 10, TokensReasoning: 4}) {
		t.Fatal("second usage not added")
	}

	end := sessionEvent(hooks.EventSessionEnd, "s1")
	end.Data.InputTokens = 8888
	end.Data.OutputTokens = 8888
	s := hookRun(t, store, end)
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if s.APICallCount != 3 || s.TokensInput != 150 || s.TokensOutput != 30 || s.TokensCached != 300 || s.TokensReasoning != 4 {
		t.Errorf("usage = calls %d in %d out %d cached %d reasoning %d, want 3/150/30/300/4",
			s.APICallCount, s.TokensInput, s.TokensOutput, s.TokensCached, s.TokensReasoning)
	}
	if s.TurnCount != 2 || s.ToolCalls["Bash"].Calls != 2 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("turns %d, tools %+v, want 2 turns, 2 Bash, 1 Read", s.TurnCount, s.ToolCalls)
	}

	// The session is over and its state removed: later usage is not added.
	if addUsage(t, store, telemetry.SessionUsage{Calls: 1}) {
		t.Error("usage added after session-end")
	}
}

// With hook usage (or no source set), model-end events still carry the
// session's usage, as before.
func TestAddUsage_HookSourceModelEndStillCounts(t *testing.T) {
	for _, source := range []string{"hooks", ""} {
		t.Run("source="+source, func(t *testing.T) {
			t.Setenv("SCION_USAGE_SOURCE", source)
			store := NewFileSessionState(t.TempDir())
			model := sessionEvent(hooks.EventModelEnd, "s1")
			model.Data.InputTokens = 10
			model.Data.OutputTokens = 3
			hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))
			hookRun(t, store, model)
			s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
			if s == nil || s.APICallCount != 1 || s.TokensInput != 10 || s.TokensOutput != 3 {
				t.Errorf("summary = %+v, want 1 call, 10 in, 3 out", s)
			}
		})
	}
}

// A harness that supplies no usage at all still reports its session, with
// zero calls and tokens and its turn and tool counts.
func TestAddUsage_NoUsageReportsZero(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "native")
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if s == nil {
		t.Fatal("session-end produced no summary")
	}
	if s.APICallCount != 0 || s.TokensInput != 0 || s.TokensOutput != 0 || s.TokensCached != 0 || s.TokensReasoning != 0 {
		t.Errorf("usage = %+v, want all zero", *s)
	}
	if s.TurnCount != 1 || s.ToolCalls["Bash"].Calls != 1 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("turns %d, tools %+v", s.TurnCount, s.ToolCalls)
	}
}

// Usage is dropped without error when there is nothing to add it to: no
// state yet, a session already closed at shutdown (the tombstone is left
// as it was), or a zero increment.
func TestAddUsage_NoOpenSession(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	if addUsage(t, store, telemetry.SessionUsage{Calls: 1}) {
		t.Error("usage added with no state")
	}
	if _, err := os.Stat(store.Path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("AddUsage created the lock file: %v", err)
	}

	openSession(t, store)
	if addUsage(t, store, telemetry.SessionUsage{}) {
		t.Error("zero usage reported as added")
	}
	if _, ok := closeOpen(t, store, ""); !ok {
		t.Fatal("open session not closed")
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if addUsage(t, store, telemetry.SessionUsage{Calls: 1}) {
		t.Error("usage added to a closed session")
	}
	after, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("tombstone changed: %s -> %s", before, after)
	}
}

// Usage added while hook processes run concurrently is neither lost nor
// loses their counts.
func TestAddUsage_ConcurrentWithHooksLosesNoCounts(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "native")
	store := NewFileSessionState(t.TempDir())
	store.lockTimeout = testLockTimeoutGenerous
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s1"))

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h := NewTelemetryHandler(nil, nil, nil)
			h.SessionState = store
			_ = h.Handle(toolEvent("s1", "Bash"))
		}()
		go func() {
			defer wg.Done()
			if _, err := store.AddUsage(telemetry.SessionUsage{Calls: 1, TokensInput: 2}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AddUsage: %v", err)
	}

	s := hookRun(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if s == nil || s.APICallCount != n || s.TokensInput != 2*n || s.ToolCalls["Bash"].Calls != n {
		t.Errorf("summary = %+v, want %d calls, %d input tokens, %d Bash calls", s, n, 2*n, n)
	}
}

// AddUsage runs as root in the init daemon, so like CloseOpenSession it
// refuses a symlinked state file instead of writing through it.
func TestAddUsage_SymlinkRefused(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, target, string(data))
	if err := os.Remove(store.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.Path); err != nil {
		t.Fatal(err)
	}
	added, err := store.AddUsage(telemetry.SessionUsage{Calls: 1})
	if added || !errors.Is(err, ErrSessionStateRefused) {
		t.Errorf("AddUsage through a symlink = %v, %v; want refused", added, err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(data) {
		t.Error("symlink target was modified")
	}
}
