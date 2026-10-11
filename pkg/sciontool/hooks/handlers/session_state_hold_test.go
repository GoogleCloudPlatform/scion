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
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// fakeClock drives pendingNow, the clock of claims and holds.
type fakeClock struct{ now time.Time }

func useFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)}
	old := pendingNow
	pendingNow = func() time.Time { return c.now }
	t.Cleanup(func() { pendingNow = old })
	return c
}

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newHoldingStore is the state store of a hook process of a harness whose
// usage source is native.
func newHoldingStore(t *testing.T) *FileSessionState {
	t.Helper()
	t.Setenv("SCION_USAGE_SOURCE", "native")
	store := NewFileSessionState(t.TempDir())
	store.HoldReports = true
	return store
}

// endHeld runs a session-end hook for sessionID that must hold its report:
// nothing is sent.
func endHeld(t *testing.T, store *FileSessionState, sessionID string) {
	t.Helper()
	if sent := hookRunAll(t, store, sessionEvent(hooks.EventSessionEnd, sessionID)); len(sent) != 0 {
		t.Fatalf("session-end of %s sent %d report(s), want the report held", sessionID, len(sent))
	}
}

func lateUsage(calls, in, out int64) telemetry.SessionUsage {
	return telemetry.SessionUsage{Calls: calls, TokensInput: in, TokensOutput: out}
}

func mustAddUsage(t *testing.T, store *FileSessionState, u telemetry.SessionUsage, want bool) {
	t.Helper()
	added, err := store.AddUsage(u)
	if err != nil || added != want {
		t.Fatalf("AddUsage(%+v) = %v, %v; want %v, nil", u, added, err, want)
	}
}

func claimDue(t *testing.T, store *FileSessionState) []telemetry.SessionSummary {
	t.Helper()
	got, err := store.ClaimDueHeldReportsNoFollow()
	if err != nil {
		t.Fatalf("ClaimDueHeldReportsNoFollow: %v", err)
	}
	return got
}

func completeAll(t *testing.T, store *FileSessionState, summaries []telemetry.SessionSummary) {
	t.Helper()
	if err := store.CompleteReportsNoFollow(summaries...); err != nil {
		t.Fatalf("CompleteReportsNoFollow: %v", err)
	}
}

func stateFileGone(t *testing.T, store *FileSessionState) bool {
	t.Helper()
	_, err := os.Stat(store.Path)
	return errors.Is(err, os.ErrNotExist)
}

// The issue's case: a native harness exports its usage after session-end.
// The session-end hook holds the report, the late usage is added to it, and
// the init daemon's held-report check sends that single report once the
// quiet period after the last usage has passed.
func TestHeldReport_LateUsageIsInTheSingleReportSent(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)

	t0 := clock.now
	endHeld(t, store, "s1")
	f := readStateFile(t, store)
	if len(f.Pending) != 1 {
		t.Fatalf("pending = %+v, want one held report", f.Pending)
	}
	r := f.Pending[0]
	if r.ClaimPID != 0 || !r.HoldUntil.Equal(t0.Add(nativeReportQuietPeriod)) || !r.HoldCap.Equal(t0.Add(nativeReportMaxHold)) {
		t.Fatalf("held report = %+v, want unclaimed, held until t0+%s, cap t0+%s", r, nativeReportQuietPeriod, nativeReportMaxHold)
	}

	clock.advance(10 * time.Second)
	mustAddUsage(t, store, lateUsage(2, 100, 50), true)
	clock.advance(5 * time.Second)
	mustAddUsage(t, store, lateUsage(1, 10, 5), true)
	// The quiet period restarts at the last usage: due at t0+15s+45s.
	if got := readStateFile(t, store).Pending[0].HoldUntil; !got.Equal(t0.Add(15*time.Second + nativeReportQuietPeriod)) {
		t.Fatalf("HoldUntil = %v, want t0+60s", got)
	}

	clock.advance(nativeReportQuietPeriod - time.Second) // t0+59s
	if got := claimDue(t, store); len(got) != 0 {
		t.Fatalf("held report claimed before it was due: %+v", got)
	}
	procs.self = 300 // the init daemon
	clock.advance(time.Second)
	sent := claimDue(t, store)
	if len(sent) != 1 || sent[0].SessionID != "s1" {
		t.Fatalf("due claim = %+v, want the report of s1", sent)
	}
	if sent[0].APICallCount != 3 || sent[0].TokensInput != 110 || sent[0].TokensOutput != 55 {
		t.Fatalf("sent summary = %+v, want the late usage (3 calls, 110 in, 55 out) in it", sent[0])
	}
	completeAll(t, store, sent)
	if !stateFileGone(t, store) {
		t.Fatalf("state file still present after the report was sent: %+v", readStateFile(t, store))
	}

	// Nothing is left to send by anyone.
	procs.self = 400
	if got := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2")); len(got) != 0 {
		t.Fatalf("a later hook sent %d report(s), want none", len(got))
	}
	if got := claimDue(t, store); len(got) != 0 {
		t.Fatalf("a second due claim returned %+v", got)
	}
}

// A held report has no claimer (ClaimPID 0), but it is not abandoned: a hook
// process must not claim it before HoldUntil. Once it is due, the first
// hook process sends it, and only that one.
func TestHeldReport_NotClaimableByHooksBeforeDue(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")

	r := readStateFile(t, store).Pending[0]
	if r.abandoned(clock.now) || r.abandoned(r.HoldUntil.Add(-time.Nanosecond)) {
		t.Fatal("a held report counts as abandoned before HoldUntil")
	}
	if !r.abandoned(r.HoldUntil) {
		t.Fatal("a held report does not count as abandoned at HoldUntil")
	}

	procs.self = 201
	if got, err := store.ClaimAbandonedReports(); err != nil || len(got) != 0 {
		t.Fatalf("ClaimAbandonedReports before due = %+v, %v; want nothing", got, err)
	}
	if got := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2")); len(got) != 0 {
		t.Fatalf("hook before due sent %+v", got)
	}

	clock.advance(nativeReportQuietPeriod)
	procs.self = 202
	got := hookRunAll(t, store, toolEvent("s2", "Bash"))
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("hook after due sent %+v, want the report of s1 once", got)
	}
	procs.self = 203
	if got := hookRunAll(t, store, toolEvent("s2", "Read")); len(got) != 0 {
		t.Fatalf("a second hook sent %+v, want nothing", got)
	}
	if got := claimDue(t, store); len(got) != 0 {
		t.Fatalf("the daemon claimed %+v after a hook sent it", got)
	}
}

// At shutdown the init daemon sends a held report at once, without waiting
// for it to be due, and nobody sends it again.
func TestHeldReport_ShutdownSendsAtOnce(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")
	clock.advance(time.Second)
	mustAddUsage(t, store, lateUsage(4, 40, 4), true)

	procs.self = 300
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil || len(got) != 1 || got[0].SessionID != "s1" || got[0].APICallCount != 4 {
		t.Fatalf("shutdown check = %+v, %v; want the held report of s1 with its late usage", got, err)
	}
	// Claimed: late usage no longer goes into it.
	mustAddUsage(t, store, lateUsage(1, 1, 1), false)
	completeAll(t, store, got)
	if !stateFileGone(t, store) {
		t.Fatalf("state file still present: %+v", readStateFile(t, store))
	}
	clock.advance(nativeReportMaxHold)
	if got := claimDue(t, store); len(got) != 0 {
		t.Fatalf("due claim after shutdown sent it = %+v", got)
	}
}

// A new session that starts during the quiet period takes the usage that
// arrives while it is open (the usage carries no session ID), the earlier
// report stays held, and each segment is sent exactly once.
func TestHeldReport_NewSessionDuringQuietPeriod(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")

	clock.advance(5 * time.Second)
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s2"))
	mustAddUsage(t, store, lateUsage(7, 70, 7), true)
	f := readStateFile(t, store)
	if !f.Aggregator.Open || f.Aggregator.SessionID != "s2" || f.Aggregator.APICallCount != 7 {
		t.Fatalf("open session = %+v, want s2 with the 7 calls", f.Aggregator)
	}
	if len(f.Pending) != 1 || f.Pending[0].Summary.APICallCount != 0 || !f.Pending[0].held() {
		t.Fatalf("pending = %+v, want the report of s1 still held and without the usage", f.Pending)
	}

	clock.advance(5 * time.Second)
	endHeld(t, store, "s2")
	if p := readStateFile(t, store).Pending; len(p) != 2 || !p[0].held() || !p[1].held() {
		t.Fatalf("pending = %+v, want two held reports", p)
	}

	clock.advance(nativeReportQuietPeriod)
	procs.self = 300
	sent := claimDue(t, store)
	if len(sent) != 2 || sent[0].SessionID != "s1" || sent[1].SessionID != "s2" || sent[1].APICallCount != 7 {
		t.Fatalf("due claim = %+v, want s1 and s2 (with 7 calls) once each", sent)
	}
	completeAll(t, store, sent)
	if got := claimDue(t, store); len(got) != 0 {
		t.Fatalf("second due claim = %+v", got)
	}
	procs.self = 400
	if got := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s3")); len(got) != 0 {
		t.Fatalf("a later hook sent %+v", got)
	}
}

// Usage is never added to a report that has been claimed: the report is
// sent with exactly the content it had when it was claimed.
func TestHeldReport_NoUsageAddedAfterClaim(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")
	clock.advance(nativeReportQuietPeriod)

	procs.self = 300
	sent := claimDue(t, store)
	if len(sent) != 1 {
		t.Fatalf("due claim = %+v", sent)
	}
	mustAddUsage(t, store, lateUsage(9, 9, 9), false)
	if p := readStateFile(t, store).Pending; len(p) != 1 || p[0].Summary.APICallCount != 0 || p[0].ClaimPID != 300 || p[0].held() {
		t.Fatalf("pending = %+v, want the claimed report unchanged", p)
	}
}

// Late usage keeps moving HoldUntil, but never past HoldCap.
func TestHeldReport_LateUsageCannotPostponePastCap(t *testing.T) {
	useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	t0 := clock.now
	endHeld(t, store, "s1")
	for i := 0; i < 10; i++ {
		clock.advance(30 * time.Second)
		if clock.now.Before(t0.Add(nativeReportMaxHold)) {
			mustAddUsage(t, store, lateUsage(1, 1, 1), true)
		}
		r := readStateFile(t, store).Pending
		if len(r) == 0 {
			break
		}
		if r[0].HoldUntil.After(t0.Add(nativeReportMaxHold)) {
			t.Fatalf("HoldUntil = %v, past the cap t0+%s", r[0].HoldUntil, nativeReportMaxHold)
		}
	}
	clock.now = t0.Add(nativeReportMaxHold)
	if got := claimDue(t, store); len(got) != 1 {
		t.Fatalf("due claim at the cap = %+v, want the report", got)
	}
}

// After a restart the harness that could still send usage for a held report
// is gone: the startup clear releases the hold, and the report is sent once,
// without waiting.
func TestHeldReport_RestartReleasesTheHold(t *testing.T) {
	procs := useFakeProcs(t)
	useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")

	if _, err := store.ClearSessionTombstone(); err != nil {
		t.Fatalf("ClearSessionTombstone: %v", err)
	}
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if strings.Contains(string(data), "hold_") {
		t.Fatalf("state after the startup clear still holds: %s", data)
	}
	mustAddUsage(t, store, lateUsage(1, 1, 1), false)

	procs.self = 201
	got := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2"))
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("first hook after restart sent %+v, want the report of s1", got)
	}
	procs.self = 202
	if got := hookRunAll(t, store, toolEvent("s2", "Bash")); len(got) != 0 {
		t.Fatalf("second hook sent %+v", got)
	}
}

// The init daemon dies after it claimed a due held report and before it
// confirmed it: the report is sent once more, by the first hook after the
// restart (a resend the Hub absorbs, as for any dead sender).
func TestHeldReport_DaemonDiesAfterClaim(t *testing.T) {
	procs := useFakeProcs(t)
	clock := useFakeClock(t)
	store := newHoldingStore(t)
	openSession(t, store)
	endHeld(t, store, "s1")
	clock.advance(nativeReportQuietPeriod)

	procs.self = 300
	if got := claimDue(t, store); len(got) != 1 {
		t.Fatalf("due claim = %+v", got)
	}
	procs.dead[300] = true
	if _, err := store.ClearSessionTombstone(); err != nil {
		t.Fatalf("ClearSessionTombstone: %v", err)
	}
	procs.self = 201
	if got := hookRunAll(t, store, sessionEvent(hooks.EventSessionStart, "s2")); len(got) != 1 {
		t.Fatalf("first hook after restart sent %+v, want the report once", got)
	}
	procs.self = 202
	if got := hookRunAll(t, store, toolEvent("s2", "Bash")); len(got) != 0 {
		t.Fatalf("second hook sent %+v", got)
	}
}

// legacyPendingReport and legacySessionStateFile are the state file's
// encoding before held reports existed.
type legacyPendingReport struct {
	Summary   telemetry.SessionSummary `json:"summary"`
	ClaimPID  int                      `json:"claim_pid,omitempty"`
	ClaimedAt time.Time                `json:"claimed_at,omitempty"`
}

type legacySessionStateFile struct {
	Version    int                       `json:"version"`
	Aggregator telemetry.AggregatorState `json:"aggregator"`
	Closed     bool                      `json:"closed,omitempty"`
	Pending    []legacyPendingReport     `json:"pending,omitempty"`
}

// assertLegacyBytes checks that the state file is byte for byte what the
// encoding before held reports produced for the same content.
func assertLegacyBytes(t *testing.T, store *FileSessionState, step string) {
	t.Helper()
	data, err := os.ReadFile(store.Path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("%s: reading state: %v", step, err)
	}
	var legacy legacySessionStateFile
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatalf("%s: decoding state: %v", step, err)
	}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("%s: encoding: %v", step, err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("%s: state file differs from the legacy encoding\n got: %s\nwant: %s", step, data, want)
	}
}

// A harness whose usage source is not native is unchanged: session-end
// sends at once, Update returns no hold, usage that arrives after
// session-end is not added anywhere, nothing waits for a quiet period, and
// every state file written is byte for byte the legacy encoding.
func TestHeldReport_NonNativeUnchanged(t *testing.T) {
	for _, source := range []string{"", "hooks"} {
		t.Run("source="+source, func(t *testing.T) {
			procs := useFakeProcs(t)
			clock := useFakeClock(t)
			t.Setenv("SCION_USAGE_SOURCE", source)
			store := NewFileSessionState(t.TempDir())
			if store.HoldReports {
				t.Fatal("a new store holds reports by default")
			}

			openSession(t, store)
			assertLegacyBytes(t, store, "open")

			// A session-end hook killed before it reports leaves a
			// pending report claimed by itself, as before.
			runKilledSessionEnd(t, store, procs, 200, "s1")
			assertLegacyBytes(t, store, "killed session-end")
			if p := readStateFile(t, store).Pending; len(p) != 1 || p[0].ClaimPID != 200 || p[0].held() {
				t.Fatalf("pending = %+v, want one report claimed by 200 and not held", p)
			}
			mustAddUsage(t, store, lateUsage(1, 1, 1), false)
			if got := claimDue(t, store); len(got) != 0 {
				t.Fatalf("the daemon's held check claimed %+v", got)
			}

			// The next session's session-end sends at once (no clock
			// advance), and the abandoned report goes with it.
			procs.self = 201
			var sent []telemetry.SessionSummary
			for _, ev := range []*hooks.Event{
				sessionEvent(hooks.EventSessionStart, "s2"),
				toolEvent("s2", "Bash"),
				sessionEvent(hooks.EventSessionEnd, "s2"),
			} {
				sent = append(sent, hookRunAll(t, store, ev)...)
				assertLegacyBytes(t, store, ev.Name)
			}
			if len(sent) != 2 || sent[0].SessionID != "s1" || sent[1].SessionID != "s2" {
				t.Fatalf("sent = %+v, want s1 (abandoned) then s2 (at session-end)", sent)
			}
			if !stateFileGone(t, store) {
				t.Fatalf("state left after both sends: %+v", readStateFile(t, store))
			}
			clock.advance(nativeReportMaxHold)
			if got := claimDue(t, store); len(got) != 0 {
				t.Fatalf("held check claimed %+v", got)
			}
		})
	}
}

// Update returns the hold sentinel only with HoldReports, and only for the
// event that ends the session.
func TestFileSessionState_UpdateHoldSentinel(t *testing.T) {
	useFakeProcs(t)
	useFakeClock(t)
	for _, hold := range []bool{false, true} {
		store := NewFileSessionState(t.TempDir())
		store.HoldReports = hold
		var errs []error
		for _, ev := range []*hooks.Event{
			sessionEvent(hooks.EventSessionStart, "s1"),
			sessionEvent(hooks.EventSessionEnd, "s1"),
		} {
			agg := telemetry.NewAggregator()
			errs = append(errs, store.Update(agg, ev, func() (telemetry.SessionSummary, bool) {
				if update := aggregatorUpdates[ev.Name]; update != nil {
					return update(agg, ev)
				}
				return noSummary()
			}))
		}
		if errs[0] != nil {
			t.Errorf("hold=%v: session-start Update = %v, want nil", hold, errs[0])
		}
		if got := errors.Is(errs[1], ErrSessionReportHeld); got != hold {
			t.Errorf("hold=%v: session-end Update = %v, want held %v", hold, errs[1], hold)
		}
	}
}
