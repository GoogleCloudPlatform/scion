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
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// fakeProcs stands in for the processes that share a state file: each
// simulated process has a PID, and a process can be marked dead.
type fakeProcs struct {
	self int
	dead map[int]bool
}

// useFakeProcs installs a fakeProcs as the claim-ownership seams. Every
// simulated process starts alive; self starts at 100.
func useFakeProcs(t *testing.T) *fakeProcs {
	t.Helper()
	p := &fakeProcs{self: 100, dead: map[int]bool{}}
	oldPID, oldAlive := currentPID, processAlive
	currentPID = func() int { return p.self }
	processAlive = func(pid int) bool { return pid > 0 && !p.dead[pid] }
	t.Cleanup(func() { currentPID, processAlive = oldPID, oldAlive })
	return p
}

// killedHook is a session-end hook process that is killed after its
// Update (which ends the session and clears its state) and before it
// reports: its handler has no OnSessionEnd, and its store hides
// PendingReportStore so it never confirms anything.
type killedHook struct{ inner *FileSessionState }

func (k killedHook) Update(agg *telemetry.Aggregator, event *hooks.Event, apply func() (telemetry.SessionSummary, bool)) error {
	return k.inner.Update(agg, event, apply)
}

func runKilledSessionEnd(t *testing.T, store *FileSessionState, procs *fakeProcs, pid int, sessionID string) {
	t.Helper()
	saved := procs.self
	procs.self = pid
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = killedHook{store}
	if err := h.Handle(sessionEvent(hooks.EventSessionEnd, sessionID)); err != nil {
		t.Fatalf("Handle(session-end): %v", err)
	}
	procs.dead[pid] = true
	procs.self = saved
}

// hookRunAll is hookRun, returning every summary the process reported.
func hookRunAll(t *testing.T, store SessionStateStore, ev *hooks.Event) []telemetry.SessionSummary {
	t.Helper()
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = store
	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }
	if err := h.Handle(ev); err != nil {
		t.Fatalf("Handle(%s): %v", ev.Name, err)
	}
	return got
}

func readStateFile(t *testing.T, store *FileSessionState) sessionStateFile {
	t.Helper()
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var f sessionStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return f
}

// The issue's lost-report case: a session-end hook is killed after it
// clears the session's state and before it reports. The finalized summary
// stays in the file as a pending report, the shutdown check does not
// reopen the session, and the next hook process sends the report exactly
// once.
func TestPendingReport_HookKilledBeforeReportSentLaterOnce(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)

	runKilledSessionEnd(t, store, procs, 200, "s1")

	f := readStateFile(t, store)
	if f.Aggregator.Open || len(f.Pending) != 1 || f.Pending[0].Summary.SessionID != "s1" {
		t.Fatalf("after the killed hook: state = %+v, want no open session and one pending report for s1", f)
	}
	if _, ok, err := store.CloseOpenSession(""); ok || err != nil {
		t.Fatalf("CloseOpenSession = ok %v, err %v; the session must not be finalized a second time", ok, err)
	}

	var reports []telemetry.SessionSummary
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s2"),
		toolEvent("s2", "Bash"),
		sessionEvent(hooks.EventSessionEnd, "s2"),
		sessionEvent(hooks.EventSessionStart, "s3"),
	} {
		reports = append(reports, hookRunAll(t, store, ev)...)
	}

	var s1 []telemetry.SessionSummary
	for _, r := range reports {
		if r.SessionID == "s1" {
			s1 = append(s1, r)
		}
	}
	if len(s1) != 1 {
		t.Fatalf("s1 reported %d times, want exactly 1 (all reports: %+v)", len(s1), reports)
	}
	if s := s1[0]; s.TurnCount != 1 || s.ToolCalls["Bash"].Calls != 1 || s.ToolCalls["Read"].Calls != 1 {
		t.Errorf("s1 summary = %+v, want the counts of openSession", s)
	}
	if len(reports) != 2 || reports[1].SessionID != "s2" {
		t.Errorf("reports = %+v, want s1 then s2", reports)
	}
	if f := readStateFile(t, store); len(f.Pending) != 0 {
		t.Errorf("pending reports left: %+v", f.Pending)
	}
}

// The same lost report, picked up by the init daemon's shutdown check
// instead: it is returned once, and once confirmed it is gone for both the
// next check and later hooks.
func TestPendingReport_HookKilledBeforeReportSentAtShutdownOnce(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	runKilledSessionEnd(t, store, procs, 200, "s1")

	procs.self = 1 // the init daemon
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil || len(got) != 1 || got[0].SessionID != "s1" || got[0].TurnCount != 1 {
		t.Fatalf("shutdown check = %+v, %v; want the s1 summary once", got, err)
	}
	if again, err := store.CloseOpenSessionAndClaimPending(""); err != nil || len(again) != 0 {
		t.Fatalf("second check while the first is sending = %+v, %v; want nothing", again, err)
	}
	if err := store.CompleteReportsNoFollow(got...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		t.Errorf("state file left after the last report was confirmed: %v", err)
	}

	procs.self = 300
	if r := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2")); len(r) != 0 {
		t.Errorf("later hook reported %+v, want nothing", r)
	}
}

// A report whose sender is still running is left to it: neither a hook
// nor the shutdown check sends it too, and the sender's confirmation
// removes it. Pending reports survive the other writers of the file.
func TestPendingReport_LiveSenderNotDuplicated(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)

	procs.self = 200
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = killedHook{store} // reports later, by hand
	if err := h.Handle(sessionEvent(hooks.EventSessionEnd, "s1")); err != nil {
		t.Fatal(err)
	}

	procs.self = 300
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s2"),
		toolEvent("s2", "Bash"),
	} {
		if r := hookRunAll(t, store, ev); len(r) != 0 {
			t.Fatalf("hook reported %+v while the sender is alive", r)
		}
	}
	if _, err := store.AddUsage(telemetry.SessionUsage{Calls: 1, TokensInput: 5}); err != nil {
		t.Fatal(err)
	}
	procs.self = 1
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil || len(got) != 1 || got[0].SessionID != "s2" {
		t.Fatalf("shutdown check = %+v, %v; want only the open s2", got, err)
	}
	if err := store.CompleteReportsNoFollow(got...); err != nil {
		t.Fatal(err)
	}

	f := readStateFile(t, store)
	if len(f.Pending) != 1 || f.Pending[0].Summary.SessionID != "s1" || f.Pending[0].ClaimPID != 200 {
		t.Fatalf("pending = %+v, want s1 still claimed by its sender", f.Pending)
	}
	procs.self = 200
	if err := store.CompleteReport(f.Pending[0].Summary); err != nil {
		t.Fatal(err)
	}
	if f := readStateFile(t, store); len(f.Pending) != 0 || !f.Closed {
		t.Errorf("state = %+v, want only the s2 tombstone", f)
	}
}

// The init daemon is killed after its shutdown check closed a session and
// before it reported it. Its claim is by PID 1, which is alive again after
// the restart, so the startup clear releases it, and the first hook of the
// new run sends the report once.
func TestPendingReport_ShutdownReportKilledSentAfterRestart(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)

	procs.self = 1
	if got, err := store.CloseOpenSessionAndClaimPending(""); err != nil || len(got) != 1 {
		t.Fatalf("shutdown check = %+v, %v", got, err)
	}
	// Killed here: no report, no confirmation. Restart:
	cleared, err := store.ClearSessionTombstone()
	if err != nil || !cleared {
		t.Fatalf("ClearSessionTombstone = %v, %v", cleared, err)
	}

	procs.self = 400
	var s1 int
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s1"), // resumed with the same ID
		toolEvent("s1", "Bash"),
	} {
		for _, r := range hookRunAll(t, store, ev) {
			if r.SessionID == "s1" {
				s1++
			}
		}
	}
	if s1 != 1 {
		t.Fatalf("first segment of s1 reported %d times, want 1", s1)
	}
	f := readStateFile(t, store)
	if !f.Aggregator.Open || f.Aggregator.SessionID != "s1" || f.Aggregator.ToolCalls["Bash"].Calls != 1 || len(f.Pending) != 0 {
		t.Errorf("state = %+v, want only the resumed s1 segment", f)
	}
}

// A stale claim (its PID reused by an unrelated live process) is not
// honoured forever.
func TestPendingReport_StaleClaimReclaimed(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	procs.self = 200
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = killedHook{store}
	if err := h.Handle(sessionEvent(hooks.EventSessionEnd, "s1")); err != nil {
		t.Fatal(err)
	}

	procs.self = 300
	oldNow := pendingNow
	t.Cleanup(func() { pendingNow = oldNow })
	pendingNow = func() time.Time { return time.Now().Add(pendingClaimTTL + time.Second) }
	r := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2"))
	if len(r) != 1 || r[0].SessionID != "s1" {
		t.Errorf("reports = %+v, want the s1 report reclaimed", r)
	}
}

// Without a pending report, today's behaviour holds: the session-end hook
// reports once and leaves no state file.
func TestPendingReport_NormalSessionEndLeavesNothing(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	r := hookRunAll(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if len(r) != 1 || r[0].SessionID != "s1" {
		t.Fatalf("reports = %+v", r)
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		t.Errorf("state file left: %v", err)
	}
}

// Two segments of one session (resumed with the same session ID) can be
// pending at once. Segment A's sender is still alive when segment B starts,
// so B's first hook leaves A alone; then A's sender is killed. The shutdown
// check must return both segments' reports: B's must not have replaced A's,
// and confirming one must not remove the other.
func TestPendingReport_TwoSegmentsOfOneSessionBothSent(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store) // segment A of s1: 1 turn, Bash and Read

	procs.self = 200
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = killedHook{store} // A's sender, still sending
	if err := h.Handle(sessionEvent(hooks.EventSessionEnd, "s1")); err != nil {
		t.Fatal(err)
	}

	// Segment B of s1 starts later, while A's sender is alive.
	time.Sleep(5 * time.Millisecond) // B's start time differs from A's
	procs.self = 300
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "s1"),
		toolEvent("s1", "Grep"),
	} {
		if r := hookRunAll(t, store, ev); len(r) != 0 {
			t.Fatalf("hook reported %+v while A's sender is alive", r)
		}
	}
	procs.dead[200] = true // A's sender is killed

	procs.self = 1
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("shutdown check returned %d summaries, want 2 (both segments of s1): %+v", len(got), got)
	}
	var a, b *telemetry.SessionSummary
	for i := range got {
		switch {
		case got[i].ToolCalls["Bash"].Calls == 1 && got[i].ToolCalls["Read"].Calls == 1:
			a = &got[i]
		case got[i].ToolCalls["Grep"].Calls == 1:
			b = &got[i]
		}
	}
	if a == nil || b == nil || a.SessionID != "s1" || b.SessionID != "s1" || a.StartedAt.Equal(b.StartedAt) {
		t.Fatalf("summaries = %+v, want segment A and segment B of s1 with different start times", got)
	}

	// Confirming B alone leaves A pending; confirming A then clears it.
	if err := store.CompleteReportsNoFollow(*b); err != nil {
		t.Fatal(err)
	}
	if f := readStateFile(t, store); len(f.Pending) != 1 || !f.Pending[0].Summary.StartedAt.Equal(a.StartedAt) {
		t.Fatalf("after confirming B: pending = %+v, want only A", f.Pending)
	}
	if err := store.CompleteReportsNoFollow(*a); err != nil {
		t.Fatal(err)
	}
	if f := readStateFile(t, store); len(f.Pending) != 0 {
		t.Errorf("pending left: %+v", f.Pending)
	}
}

// A session-end with no prior state finalizes a summary whose StartedAt
// comes straight from time.Now, so it carries a monotonic clock reading and
// the local zone. The pending copy went through JSON and has neither. The
// hook's confirmation must still match it (time.Equal, not ==), or the
// report would stay pending and be sent a second time.
func TestPendingReport_InMemoryStartTimeMatchesStoredReport(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	r := hookRunAll(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if len(r) != 1 || r[0].SessionID != "s1" {
		t.Fatalf("reports = %+v, want one for s1", r)
	}
	if r[0].StartedAt == r[0].StartedAt.Round(0) {
		t.Fatal("setup: the in-memory StartedAt has no monotonic reading, so this test proves nothing")
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		f := readStateFile(t, store)
		t.Fatalf("pending report not cleared by the hook's confirmation: %+v", f.Pending)
	}
}

// The init daemon confirms every report it sent in one call; each of them
// must be cleared, not only the first.
func TestPendingReport_CompleteReportsNoFollowClearsAll(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	for i, id := range []string{"s1", "s2", "s3"} {
		// Started by a handler that cannot claim pending reports, so the
		// earlier killed senders' reports stay pending for the shutdown.
		h := NewTelemetryHandler(nil, nil, nil)
		h.SessionState = killedHook{store}
		if err := h.Handle(sessionEvent(hooks.EventSessionStart, id)); err != nil {
			t.Fatal(err)
		}
		runKilledSessionEnd(t, store, procs, 200+i, id)
	}

	procs.self = 1
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil || len(got) != 3 {
		t.Fatalf("shutdown check = %d summaries, %v; want 3", len(got), err)
	}
	if err := store.CompleteReportsNoFollow(got...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(store.Path); !os.IsNotExist(err) {
		f := readStateFile(t, store)
		t.Fatalf("pending reports left after confirming all of them: %+v", f.Pending)
	}
}
