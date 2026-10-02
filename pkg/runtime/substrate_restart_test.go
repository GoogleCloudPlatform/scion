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

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// wipeSubstrateAgentStateForRestart clears substrateControlTokens and
// substrateAgentRecords mid-test, simulating a runtime process restart that
// happens after one or more agents were already started through this same
// process (Run). Unlike resetSubstrateAgentStateForTest (which clears the
// maps once at test setup and restores the pre-test contents at cleanup),
// this is meant to be called after Run has already populated state, so a
// later Run/RecordlessActors call in the same test observes a process with
// no memory of what it started before. resetSubstrateAgentStateForTest's own
// t.Cleanup (from the harness that must already be in effect) still restores
// the outer state afterward, so this needs no cleanup of its own.
func wipeSubstrateAgentStateForRestart(t *testing.T) {
	t.Helper()
	substrateAgentStateMu.Lock()
	substrateControlTokens = make(map[string]string)
	substrateAgentRecords = make(map[string]*substrateAgentRecord)
	substrateAgentStateMu.Unlock()
}

// recordlessNames extracts just the names from RecordlessActors' result, for
// tests that only care about which actors were reported, not their UIDs.
func recordlessNames(actors []RecordlessActor) []string {
	if actors == nil {
		return nil
	}
	names := make([]string, len(actors))
	for i, a := range actors {
		names[i] = a.Name
	}
	return names
}

// restartTestRunConfig returns a RunConfig for projectID/agentName, digest-
// pinned and NoAuth-free like testSubstrateRunConfig, but parameterized so a
// test can run more than one agent (optionally in the same atespace, i.e.
// the same projectID) without them colliding.
func restartTestRunConfig(projectID, agentName string) RunConfig {
	cfg := testSubstrateRunConfig()
	cfg.ProjectID = projectID
	cfg.Name = agentName
	cfg.Labels = map[string]string{"scion.agent_id": agentName}
	return cfg
}

// TestSubstrateRestart_RecordlessActors_ReportsOnlyUnrecorded simulates a
// restart with a mixed atespace: one actor started before the restart (its
// record is gone) and one started after (its record is fresh, from this
// process's own Run). RecordlessActors must report only the pre-restart
// actor's name, in the atespace both projectID's agents share.
func TestSubstrateRestart_RecordlessActors_ReportsOnlyUnrecorded(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	wantAtespace := substrateAtespaceName(projectID)

	if _, err := rt.Run(context.Background(), restartTestRunConfig(projectID, "pre-restart-agent")); err != nil {
		t.Fatalf("Run(pre-restart-agent) error = %v", err)
	}

	wipeSubstrateAgentStateForRestart(t)

	if _, err := rt.Run(context.Background(), restartTestRunConfig(projectID, "post-restart-agent")); err != nil {
		t.Fatalf("Run(post-restart-agent) error = %v", err)
	}

	// This fake's CreateActor always returns the same UID (fakeActorUID)
	// regardless of actor name, so it cannot represent two distinct actors
	// by itself; override ListActors to report both by name with distinct
	// UIDs — the post-restart one under fakeActorUID (matching the record
	// Run just wrote, since substrateAgentRecords is keyed by the UID
	// CreateActor's response carries) and the pre-restart one under a UID
	// nothing was ever recorded for, exactly like two ListActors entries
	// from a real cluster after a restart would differ.
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: wantAtespace, Name: "pre-restart-agent", Uid: "uid-pre-restart"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				{Metadata: &ateapipb.ResourceMetadata{Atespace: wantAtespace, Name: "post-restart-agent", Uid: fakeActorUID},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
			},
		}, nil
	}

	atespace, actors, err := rt.RecordlessActors(context.Background(), projectID)
	names := recordlessNames(actors)
	if err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if atespace != wantAtespace {
		t.Errorf("RecordlessActors() atespace = %q, want %q", atespace, wantAtespace)
	}
	if len(names) != 1 || names[0] != "pre-restart-agent" {
		t.Errorf("RecordlessActors() names = %v, want exactly [%q]", names, "pre-restart-agent")
	}
}

// TestSubstrateRestart_RecordlessActors_NoneWhenAllRecorded is the
// refinement the design calls for: a project whose actors all still have
// records reports zero record-less actors, so the caller's not-found path
// stays idempotent instead of turning into a false conflict.
func TestSubstrateRestart_RecordlessActors_NoneWhenAllRecorded(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	if _, err := rt.Run(context.Background(), restartTestRunConfig(projectID, "still-recorded-agent")); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, actors, err := rt.RecordlessActors(context.Background(), projectID)
	names := recordlessNames(actors)
	if err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if len(names) != 0 {
		t.Errorf("RecordlessActors() = %v, want none — every actor in this atespace has a record", names)
	}
}

// TestSubstrateRestart_RecordlessActors_ExcludesGoldenAtespace covers the
// positive rule documented on RecordlessActors: an actor belonging to the
// substrate-reserved "ate-golden" atespace (a template's golden actor) is
// never counted, even if a broken or malicious ListActors response mixed it
// in alongside the project's own atespace's actors — the exclusion is by
// atespace equality, not by trusting the server to scope its response
// correctly.
func TestSubstrateRestart_RecordlessActors_ExcludesGoldenAtespace(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	wantAtespace := substrateAtespaceName(projectID)
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: wantAtespace, Name: "record-less-agent", Uid: "uid-real"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
				{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-golden", Name: "golden", Uid: "uid-golden"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}},
			},
		}, nil
	}

	atespace, actors, err := rt.RecordlessActors(context.Background(), projectID)
	names := recordlessNames(actors)
	if err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if atespace != wantAtespace {
		t.Errorf("RecordlessActors() atespace = %q, want %q", atespace, wantAtespace)
	}
	if len(names) != 1 || names[0] != "record-less-agent" {
		t.Errorf("RecordlessActors() = %v, want exactly [%q] — the ate-golden actor must never be counted", names, "record-less-agent")
	}
}

// TestSubstrateRestart_RecordlessActors_ProbeErrorIsExplicit pins that a
// ListActors failure while probing surfaces as an explicit error, never as
// an empty (i.e. "nothing record-less") result — the caller (resolveDeleteTarget/
// the stop path) must never treat a failed probe as proof of safety.
func TestSubstrateRestart_RecordlessActors_ProbeErrorIsExplicit(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	wantErr := "substrate: simulated list failure"
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return nil, errString(wantErr)
	}

	_, actors, err := rt.RecordlessActors(context.Background(), "550e8400-e29b-41d4-a716-446655440000")
	names := recordlessNames(actors)
	if err == nil {
		t.Fatal("RecordlessActors() error = nil, want an explicit error when the underlying list fails")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("RecordlessActors() error = %v, want it to wrap %q", err, wantErr)
	}
	if names != nil {
		t.Errorf("RecordlessActors() names = %v, want nil alongside the error", names)
	}
}

// errString is a trivial error type so tests can build a fixed-text error
// without importing "errors" solely for errors.New in this file.
type errString string

func (e errString) Error() string { return string(e) }

// TestSubstrateRestart_RecordlessActors_ExcludesDeletingState pins a
// same-project, no-restart false positive: a record-less actor already in
// ACTOR_STATE_DELETING must never be counted
// (Stop is Delete, and Delete's fire-and-forget DeleteActor call
// can leave the actor listed, in DELETING, for a while after the in-memory
// record is already gone — see Delete's and RecordlessActors' doc comments),
// while a record-less actor in any other state still is.
func TestSubstrateRestart_RecordlessActors_ExcludesDeletingState(t *testing.T) {
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	wantAtespace := substrateAtespaceName(projectID)
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{Metadata: &ateapipb.ResourceMetadata{Atespace: wantAtespace, Name: "deleting-agent", Uid: "uid-deleting"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING}},
				{Metadata: &ateapipb.ResourceMetadata{Atespace: wantAtespace, Name: "crashed-agent", Uid: "uid-crashed"},
					Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED}},
			},
		}, nil
	}

	atespace, actors, err := rt.RecordlessActors(context.Background(), projectID)
	names := recordlessNames(actors)
	if err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if atespace != wantAtespace {
		t.Errorf("RecordlessActors() atespace = %q, want %q", atespace, wantAtespace)
	}
	if len(names) != 1 || names[0] != "crashed-agent" {
		t.Errorf("RecordlessActors() = %v, want exactly [%q] — a DELETING record-less actor must never be counted, but any other state must", names, "crashed-agent")
	}
}

// TestSubstrateRestart_NewAgentAfterWipe_FullyManageable covers the design's
// "a post-restart NEW agent is fully manageable" case: an agent started by
// this same process AFTER the in-memory state was wiped (i.e. after the
// point a restart would have happened) has a fresh record, and its Delete/
// Stop succeed exactly like any other agent this process created — a
// restart in this process's past does not taint agents it starts from then
// on.
func TestSubstrateRestart_NewAgentAfterWipe_FullyManageable(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	// An agent from "before" the restart, to prove the wipe actually
	// happened and this test isn't vacuously true.
	if _, err := rt.Run(context.Background(), restartTestRunConfig("550e8400-e29b-41d4-a716-446655440000", "before-restart")); err != nil {
		t.Fatalf("Run(before-restart) error = %v", err)
	}
	wipeSubstrateAgentStateForRestart(t)

	deleteID, err := rt.Run(context.Background(), restartTestRunConfig("660e8400-e29b-41d4-a716-446655440000", "post-restart-delete"))
	if err != nil {
		t.Fatalf("Run(post-restart-delete) error = %v", err)
	}
	stopID, err := rt.Run(context.Background(), restartTestRunConfig("770e8400-e29b-41d4-a716-446655440000", "post-restart-stop"))
	if err != nil {
		t.Fatalf("Run(post-restart-stop) error = %v", err)
	}

	before := len(rec.list())
	if err := rt.Delete(context.Background(), deleteID); err != nil {
		t.Errorf("Delete(%q) error = %v, want a post-restart agent to delete cleanly", deleteID, err)
	}
	if err := rt.Stop(context.Background(), stopID); err != nil {
		t.Errorf("Stop(%q) error = %v, want a post-restart agent to stop cleanly", stopID, err)
	}
	after := rec.list()

	deleteActorCalls := 0
	for _, c := range after[before:] {
		if c == "DeleteActor" {
			deleteActorCalls++
		}
	}
	// Stop is Delete (substrate-runtime.md §9), so each of the two
	// calls above issues its own DeleteActor RPC: 2 total.
	if deleteActorCalls != 2 {
		t.Errorf("DeleteActor calls after Delete+Stop = %d, want 2 (one per call, both succeeding cleanly)", deleteActorCalls)
	}
}

// TestSubstrateRestart_ExecAfterWipe_ExplicitNoTokenError pins Exec's
// existing behaviour for a specific agent that HAD a cached control token
// before a restart: after the wipe, Exec must fail with an explicit error
// naming the cause, never silently no-op or succeed with stale
// credentials — the token cannot be recovered (bootstrap is one-shot), so
// failing loudly is correct, not a regression to guard against fixing.
func TestSubstrateRestart_ExecAfterWipe_ExplicitNoTokenError(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	substrateAgentStateMu.Lock()
	_, hadToken := substrateControlTokens[id]
	substrateAgentStateMu.Unlock()
	if !hadToken {
		t.Fatal("test setup: Run() did not cache a control token")
	}

	wipeSubstrateAgentStateForRestart(t)

	_, err = rt.Exec(context.Background(), id, []string{"true"})
	if err == nil {
		t.Fatal("Exec() after a restart wipe: error = nil, want an explicit no-token error")
	}
	if !strings.Contains(err.Error(), "no control token cached") {
		t.Errorf("Exec() error = %v, want it to name the missing control token", err)
	}
}

// TestSubstrateRestart_LogsAfterWipe_ExplicitErrorNeverEmptySuccess pins
// that GetLogs — unlike Exec — does not depend on substrateAgentRecords or
// substrateControlTokens at all (it returns ErrLogsNotSupported
// unconditionally, without resolving the id or making any call), so a
// restart by itself never silently empties out its result. It never
// surfaces ("", nil).
func TestSubstrateRestart_LogsAfterWipe_ExplicitErrorNeverEmptySuccess(t *testing.T) {
	rec := &callRecorder{}
	rt, _, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	id, err := rt.Run(context.Background(), testSubstrateRunConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wipeSubstrateAgentStateForRestart(t)

	logs, err := rt.GetLogs(context.Background(), id)
	if !errors.Is(err, ErrLogsNotSupported) {
		t.Fatalf("GetLogs() after a restart wipe: error = %v, want errors.Is(err, ErrLogsNotSupported)", err)
	}
	if logs != "" {
		t.Errorf("GetLogs() logs = %q, want empty alongside a non-nil error", logs)
	}
}
