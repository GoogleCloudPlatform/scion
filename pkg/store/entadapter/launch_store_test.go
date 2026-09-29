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

//go:build !no_sqlite

// This file covers design t1-async-create-v11.md §6 test cases S-1, S-2 and
// S-3 (marked (PG) where noted; the (PG)-only assertions run identically here
// and are additionally exercised against Postgres by the integration build,
// see enttest.NewClient).
package entadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createLaunchableAgent seeds a fresh "created"-phase agent, ready for
// BeginLaunch.
func createLaunchableAgent(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) *store.Agent {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.Phase = "created"
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

// setAgentLaunchError backdates a row's launch_error directly through the
// ent client (white-box test setup: UpdateAgent never sets this column from
// the caller's struct, so tests that need a genuine non-empty launch_error
// on the row — e.g. to prove UpdateAgent's T3 clear actually clears
// something — must write it this way, not via a store.Agent field).
func setAgentLaunchError(t *testing.T, ctx context.Context, s *AgentStore, agentID, launchError string) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetLaunchError(launchError).Save(ctx)
	require.NoError(t, err)
}

// --- S-1: bump semantics ---------------------------------------------------

func TestLaunchStore_S1_ProgressWritesDoNotBump(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s1-bump")

	// BeginLaunch itself must not bump.
	preLaunch, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	vPreLaunch := preLaunch.StateVersion

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	v0 := got.StateVersion
	assert.Equal(t, vPreLaunch, v0, "BeginLaunch must not bump state_version")

	// MarkLaunchAccepted: no bump.
	accepted, err := s.MarkLaunchAccepted(ctx, a.ID, launchID, "instance-1")
	require.NoError(t, err)
	assert.Equal(t, v0, accepted.StateVersion, "MarkLaunchAccepted must not bump state_version")

	// A progress report: no bump.
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", Seq: 1,
		State: store.LaunchReportStateProgress, Phase: "provisioning", Step: "cloning-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, v0, updated.StateVersion, "a progress apply must not bump state_version")

	// Terminal apply: bumps.
	answer, updated, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", Seq: 2,
		State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Greater(t, updated.StateVersion, v0, "a terminal apply must bump state_version")

	// EndLaunch on an already-ended launch: no-op, no bump (nothing to end).
	v1 := updated.StateVersion
	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
	final, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, v1, final.StateVersion)
}

// TestLaunchStore_S1_EndLaunchActivelyEndingDoesNotBump covers a second
// no-bump case: design §3.3 lists EndLaunch as "NO bump" unconditionally,
// not just in the already-ended no-op case the other S-1 test exercises.
// This proves it for a launch EndLaunch actually transitions from active to
// ended.
func TestLaunchStore_S1_EndLaunchActivelyEndingDoesNotBump(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s1-endlaunch-active")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateActive, before.LaunchState, "setup: the launch must be active before EndLaunch ends it")

	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateEnded, after.LaunchState, "setup: EndLaunch must have actually ended the (previously active) launch")
	assert.Equal(t, before.StateVersion, after.StateVersion, "EndLaunch must never bump state_version, even when it actively ends a launch")
}

// TestLaunchStore_S1_ReaperReapBumps covers a third bump-rule case: design
// §3.3's bump rule list includes "the reaper" as a bumping writer, alongside
// terminal ApplyLaunchReport applies.
func TestLaunchStore_S1_ReaperReapBumps(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s1-reaper-bump")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	require.Len(t, result.Reaped, 1)
	assert.Greater(t, result.Reaped[0].StateVersion, before.StateVersion, "a reaper reap must bump state_version")
}

func TestLaunchStore_S1_ConcurrentStaleUpdateAfterTerminalConflicts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s1-conflict")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	// A caller loads the row before the terminal write lands.
	stale, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)

	// The stale writer's state_version no longer matches.
	stale.Message = "stale write"
	err = s.UpdateAgent(ctx, stale)
	assert.ErrorIs(t, err, store.ErrVersionConflict)
}

// --- S-2: UpdateAgent / UpdateAgentStatus running rule ----------------------

func TestLaunchStore_S2_UpdateAgentIgnoresCallerLaunchFields(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-ignore")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	fresh, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, launchID, fresh.LaunchID)
	require.Equal(t, store.LaunchStateActive, fresh.LaunchState)

	// A caller's struct claims the launch is unrelated/ended; UpdateAgent
	// must not let that struct field touch the stored launch_* columns.
	fresh.LaunchState = ""
	fresh.LaunchID = "forged"
	fresh.LaunchError = "forged-error"
	fresh.Message = "hello"
	require.NoError(t, s.UpdateAgent(ctx, fresh))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, launchID, after.LaunchID, "UpdateAgent must not let the caller's struct overwrite launch_id")
	assert.Equal(t, store.LaunchStateActive, after.LaunchState, "UpdateAgent must not let the caller's struct overwrite launch_state")
	assert.Equal(t, "", after.LaunchError, "launch_error was already empty and UpdateAgent doesn't touch it outside the running rule")
	assert.Equal(t, "hello", after.Message)
}

func TestLaunchStore_S2_RunningClearsLaunchErrorViaUpdateAgent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-clear-ua")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonFailed))

	// Give the ROW a genuine non-empty launch_error (white-box, via the ent
	// client directly) before the running write. UpdateAgent never sets
	// launch_* columns from the caller's struct, so setting got.LaunchError
	// below would silently no-op and this test would pass whether or not
	// UpdateAgent's T3 clear exists at all.
	setAgentLaunchError(t, ctx, s, a.ID, "launch_timeout")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "launch_timeout", got.LaunchError, "setup: the row must carry a non-empty launch_error before the running write")
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "", after.LaunchError, "a running write must clear launch_error (T3)")
}

func TestLaunchStore_S2_RunningEndsActiveLaunchViaUpdateAgent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-end-ua")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, after.LaunchEndReason)
	assert.Equal(t, launchID, after.LaunchID, "launch_id is kept after the launch ends")
}

func TestLaunchStore_S2_RunningEndsActiveLaunchViaUpdateAgentStatus(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-end-status")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	// Same non-vacuous setup as the UpdateAgent case above — give the row a
	// genuine non-empty launch_error first.
	setAgentLaunchError(t, ctx, s, a.ID, "broker_lost")

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, after.LaunchEndReason)
	assert.Equal(t, launchID, after.LaunchID)
	assert.Equal(t, "", after.LaunchError, "a running write must clear launch_error (T3)")
}

func TestLaunchStore_S2_RunningOnEndedLaunchLeavesItAlone(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-not-launched")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	// A stale struct that (wrongly) claims the launch is still active must
	// not resurrect it: the write is gated on the ROW's launch_state, not
	// the caller's copy.
	got.LaunchState = store.LaunchStateActive
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState, "an ended launch must stay ended")
	assert.Equal(t, store.LaunchEndReasonNotLaunched, after.LaunchEndReason, "must stay not_launched, not be overwritten to running_observed")
}

// TestLaunchStore_S2_RunningViaUpdateAgentStatusOnEndedOrNoLaunchLeavesItAlone
// is UpdateAgentStatus's counterpart to TestLaunchStore_S2_RunningOnEndedLaunchLeavesItAlone
// above (which only exercises UpdateAgent): design §6 S-2 says writing
// "running" through EITHER method leaves launch_state/launch_end_reason
// unchanged on an already-ended launch, and must not touch them at all on a
// row that never had one.
func TestLaunchStore_S2_RunningViaUpdateAgentStatusOnEndedOrNoLaunchLeavesItAlone(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// (1) An already-ended launch must stay ended, with its original reason,
	// when phase=running is written via UpdateAgentStatus.
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-status-ended")
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))

	afterEnded, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateEnded, afterEnded.LaunchState, "an ended launch must stay ended")
	assert.Equal(t, store.LaunchEndReasonNotLaunched, afterEnded.LaunchEndReason, "must stay not_launched, not be overwritten to running_observed")

	// (2) A row that has never had a launch must stay untouched: writing
	// phase=running via UpdateAgentStatus must not write launch_state='ended'
	// on a feature-off row.
	b := makeAgent(projectID, "s2-status-no-launch")
	b.Phase = "provisioning"
	require.NoError(t, s.CreateAgent(ctx, b))

	require.NoError(t, s.UpdateAgentStatus(ctx, b.ID, store.AgentStatusUpdate{Phase: "running"}))

	afterNoLaunch, err := s.GetAgent(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "", afterNoLaunch.LaunchState, "a row with no launch must not gain launch_state='ended'")
	assert.Equal(t, "", afterNoLaunch.LaunchEndReason)
}

func TestLaunchStore_S2_BeginLaunchInvalidPhase(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "s2-invalid-phase")
	a.Phase = "running"
	require.NoError(t, s.CreateAgent(ctx, a))

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	assert.ErrorIs(t, err, store.ErrInvalidPhase)
}

// TestLaunchStore_S2_BeginLaunchRejectsNonCreateKind proves BeginLaunch's
// kind must be store.LaunchKindCreate in P1a; start and restart are P6 and
// must be rejected, not silently accepted and stored.
func TestLaunchStore_S2_BeginLaunchRejectsNonCreateKind(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-kind-reject")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindStart, 5*time.Minute)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "", after.LaunchID, "a rejected BeginLaunch must not write anything")
}

func TestLaunchStore_S2_RunningEndsActiveLaunchInsideWithTx(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	composite := NewCompositeStore(client)

	projectID := uuid.NewString()
	require.NoError(t, composite.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "test-project-withtx",
		Slug: "test-project-withtx",
	}))

	a := makeAgent(projectID, "s2-withtx")
	a.Phase = "created"
	require.NoError(t, composite.CreateAgent(ctx, a))

	launchID, err := composite.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	// A running write via UpdateAgent inside store.WithTx must end the active
	// launch without a nested-transaction error.
	err = composite.WithTx(ctx, func(tx store.Store) error {
		got, err := tx.GetAgent(ctx, a.ID)
		if err != nil {
			return err
		}
		got.Phase = "running"
		return tx.UpdateAgent(ctx, got)
	})
	require.NoError(t, err)

	after, err := composite.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, after.LaunchEndReason)
	assert.Equal(t, launchID, after.LaunchID)
}

// TestLaunchStore_S2_RunningWriteRollsBackTogetherOnSecondStatementFailure
// proves the CAS update and the conditional end-active-launch update inside
// updateAgentRunningTx commit or roll back together: an injected failure
// between the two statements must leave the phase write undone, along with
// the caller's copy of state_version.
func TestLaunchStore_S2_RunningWriteRollsBackTogetherOnSecondStatementFailure(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s2-atomic-fail")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateActive, before.LaunchState, "setup: an active launch is required to force the transactional path")

	injected := errors.New("injected failure between the CAS and the end-launch update")
	agentRunningTxFailureHook = func() error { return injected }
	t.Cleanup(func() { agentRunningTxFailureHook = nil })

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	err = s.UpdateAgent(ctx, got)
	require.ErrorIs(t, err, injected)
	assert.Equal(t, before.StateVersion, got.StateVersion, "UpdateAgent must not advance the caller's copy of state_version when the transaction fails")

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Phase, after.Phase, "the phase write must roll back along with the failed second statement")
	assert.Equal(t, before.StateVersion, after.StateVersion, "state_version must not advance when the transaction fails")
	assert.Equal(t, store.LaunchStateActive, after.LaunchState, "the launch must still be active; the end-launch write must roll back too")
}

// TestLaunchStore_S2_RunningTxEndLaunchGuardKeepsRaceEndedReason proves the
// transactional path's end-launch UPDATE is genuinely gated on the row's
// launch_state at write time, not on the probe's earlier read: if a
// concurrent EndLaunch (which never bumps state_version) lands between the
// probe finding the launch active and the transaction actually starting,
// the running write must still succeed, but must leave the launch's
// already-recorded end reason alone rather than overwriting it with
// running_observed.
func TestLaunchStore_S2_RunningTxEndLaunchGuardKeepsRaceEndedReason(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := createLaunchableAgent(t, ctx, s, projectID, "s2-tx-guard-race")
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	// agentRunningTxEnterHook fires after the probe has found the launch
	// active but before s.client.Tx(ctx) opens, so EndLaunch's own separate
	// transaction runs cleanly first, with no nested-tx or lock issue.
	agentRunningTxEnterHook = func() {
		require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
	}
	t.Cleanup(func() { agentRunningTxEnterHook = nil })

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", after.Phase, "the phase write itself must still succeed")
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, after.LaunchEndReason, "the end-launch UPDATE's launch_state='active' guard must see the row already ended and no-op, not overwrite the recorded reason with running_observed")
}

// withAgentRunningTxEnterTripwire installs agentRunningTxEnterHook as a
// call-recording tripwire for the duration of the test: the returned bool
// starts false and is set true the moment UpdateAgent's running branch
// decides to enter the transactional path at all. Unlike
// agentRunningTxFailureHook (which only fires once inside the transaction,
// after its CAS has already matched a row), this is the only hook that can
// prove a stale-version or not-found running write never got there.
func withAgentRunningTxEnterTripwire(t *testing.T) *bool {
	t.Helper()
	called := false
	agentRunningTxEnterHook = func() { called = true }
	t.Cleanup(func() { agentRunningTxEnterHook = nil })
	return &called
}

// TestLaunchStore_S2_RunningWriteOnNoLaunchRowStaysOnFastPath proves the
// fast path stays inert for a row that has never had a launch, using
// withAgentRunningTxEnterTripwire's hook as a tripwire that must NOT fire
// for (a) a successful running write, (b) a stale-version running write
// (ErrVersionConflict), or (c) a running write on a non-existent row
// (ErrNotFound) — none of these may ever reach the transactional path.
// A positive control then proves the tripwire is live: a running write on a
// row with a genuinely active launch DOES enter it.
func TestLaunchStore_S2_RunningWriteOnNoLaunchRowStaysOnFastPath(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	entered := withAgentRunningTxEnterTripwire(t)

	// (a) A normal running write on a row that never had a launch
	// (launch_state stays "") must succeed via the single-statement fast
	// path alone.
	a := makeAgent(projectID, "s2-fastpath-inert-ok")
	a.Phase = "provisioning"
	require.NoError(t, s.CreateAgent(ctx, a))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))
	assert.False(t, *entered, "a successful running write on a row with no launch must stay on the single-statement fast path")

	// (b) A stale-version running write on such a row must return
	// ErrVersionConflict without ever taking the transactional path either.
	*entered = false
	b := makeAgent(projectID, "s2-fastpath-inert-conflict")
	b.Phase = "provisioning"
	require.NoError(t, s.CreateAgent(ctx, b))
	stale, err := s.GetAgent(ctx, b.ID)
	require.NoError(t, err)

	fresh, err := s.GetAgent(ctx, b.ID)
	require.NoError(t, err)
	fresh.Message = "concurrent write"
	require.NoError(t, s.UpdateAgent(ctx, fresh)) // bumps state_version out from under `stale`

	stale.Phase = "running"
	err = s.UpdateAgent(ctx, stale)
	assert.ErrorIs(t, err, store.ErrVersionConflict)
	assert.False(t, *entered, "a stale-version running write on a row with no launch must not take the transactional path")

	// (c) A running write on a non-existent row must return ErrNotFound
	// without taking the transactional path either.
	*entered = false
	ghost := makeAgent(projectID, "s2-fastpath-inert-notfound")
	ghost.ID = uuid.NewString()
	ghost.Phase = "running"
	err = s.UpdateAgent(ctx, ghost)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.False(t, *entered, "a running write on a non-existent row must not take the transactional path")

	// Positive control: a row that genuinely has an active launch DOES enter
	// the transactional path, proving the tripwire is live rather than
	// permanently disconnected. It also pins the shape of that path: the
	// probe runs (before_probe), but the row is already active-launch on the
	// very first read, so there is no "it cleared in between" race to retry
	// into — the retry (before_retry) must not run. Ignoring the probe's
	// launch_state (so this row would needlessly retry the fast path before
	// the tx) would pass every other test in this file but fail this one.
	*entered = false
	var racePoints []string
	withAgentRunningRaceHook(t, func(point string) { racePoints = append(racePoints, point) })
	c := createLaunchableAgent(t, ctx, s, projectID, "s2-fastpath-active-control")
	_, err = s.BeginLaunch(ctx, c.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	gotC, err := s.GetAgent(ctx, c.ID)
	require.NoError(t, err)
	gotC.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, gotC))
	assert.True(t, *entered, "a running write on a row with an active launch must take the transactional path")
	assert.Contains(t, racePoints, "before_probe", "the probe must run for an active-launch row")
	assert.NotContains(t, racePoints, "before_retry", "an active-launch row must go straight to the tx after the probe, never retrying the fast path")
}

// withAgentRunningRaceHook installs agentRunningRaceHook for the duration of
// the test, so a test can land a concurrent write in the exact window
// between UpdateAgent's own autocommitted statements a real race would.
func withAgentRunningRaceHook(t *testing.T, fn func(point string)) {
	t.Helper()
	agentRunningRaceHook = fn
	t.Cleanup(func() { agentRunningRaceHook = nil })
}

// TestLaunchStore_S2_RunningWriteRetryRaceEndLaunchSucceedsOnFastPath proves
// a concurrent EndLaunch landing between the fast path's first miss and the
// probe — the narrow race the retry exists for — must still resolve on the
// retried fast path, never the transactional one.
func TestLaunchStore_S2_RunningWriteRetryRaceEndLaunchSucceedsOnFastPath(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	entered := withAgentRunningTxEnterTripwire(t)

	a := createLaunchableAgent(t, ctx, s, projectID, "s2-race-endlaunch")
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	withAgentRunningRaceHook(t, func(point string) {
		if point == "before_probe" {
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
		}
	})

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))
	assert.False(t, *entered, "a race ended by a concurrent EndLaunch must resolve on the retried fast path, not the transactional one")

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", after.Phase)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, after.LaunchEndReason, "EndLaunch's reason must not be overwritten by the running write")
}

// TestLaunchStore_S2_RunningWriteRetryRaceVersionBumpReturnsConflictViaTx
// proves an unrelated concurrent write that bumps state_version in the
// window between the probe and the retry must surface as
// ErrVersionConflict, and that it is updateAgentRunningTx's own CAS (not a
// value returned before entering the transactional path) that detects it:
// if that CAS miss were swallowed, this test would see a nil error instead.
func TestLaunchStore_S2_RunningWriteRetryRaceVersionBumpReturnsConflictViaTx(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	entered := withAgentRunningTxEnterTripwire(t)

	a := createLaunchableAgent(t, ctx, s, projectID, "s2-race-versionbump")
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	withAgentRunningRaceHook(t, func(point string) {
		switch point {
		case "before_probe":
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
		case "before_retry":
			// An unrelated concurrent write bumps state_version without
			// touching launch_state.
			fresh, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			fresh.Message = "concurrent unrelated write"
			require.NoError(t, s.UpdateAgent(ctx, fresh))
		}
	})

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	err = s.UpdateAgent(ctx, got)
	assert.ErrorIs(t, err, store.ErrVersionConflict)
	assert.True(t, *entered, "a version conflict discovered only after the retry must still be classified inside the transactional path")
}

// TestLaunchStore_S2_RunningWriteRetryRaceRelaunchEndsNewLaunchViaTx proves
// an EndLaunch followed by a BeginLaunch landing in the retry window
// (neither bumps state_version) must still succeed, ending the newly
// re-activated launch as running_observed atomically with the phase write
// via the transactional path.
func TestLaunchStore_S2_RunningWriteRetryRaceRelaunchEndsNewLaunchViaTx(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	entered := withAgentRunningTxEnterTripwire(t)

	a := createLaunchableAgent(t, ctx, s, projectID, "s2-race-relaunch")
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	var newLaunchID string
	withAgentRunningRaceHook(t, func(point string) {
		switch point {
		case "before_probe":
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
		case "before_retry":
			var err error
			newLaunchID, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
			require.NoError(t, err)
		}
	})

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "running"
	require.NoError(t, s.UpdateAgent(ctx, got))
	assert.True(t, *entered, "a re-activated launch discovered only after the retry must be ended inside the transactional path")

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotEmpty(t, newLaunchID, "setup: the race hook must have re-launched")
	assert.Equal(t, "running", after.Phase)
	assert.Equal(t, newLaunchID, after.LaunchID, "the row must carry the re-activated launch's ID")
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, after.LaunchEndReason, "the re-activated launch must be ended as running_observed by the phase=running write")
}

// --- S-3: predicate truth tables --------------------------------------------

func TestLaunchStore_S3_InFlightPredicate(t *testing.T) {
	cases := []struct {
		name        string
		launchState string
		phase       string
		deletedAt   bool
		want        bool
	}{
		{"active+provisioning", store.LaunchStateActive, "provisioning", false, true},
		{"active+created", store.LaunchStateActive, "created", false, true},
		{"active+cloning", store.LaunchStateActive, "cloning", false, true},
		{"active+starting", store.LaunchStateActive, "starting", false, true},
		{"active+running", store.LaunchStateActive, "running", false, false},
		{"active+error", store.LaunchStateActive, "error", false, false},
		{"ended+provisioning", store.LaunchStateEnded, "provisioning", false, false},
		{"active+provisioning+deleted", store.LaunchStateActive, "provisioning", true, false},
		{"empty+provisioning", "", "provisioning", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &store.Agent{LaunchState: tc.launchState, Phase: tc.phase}
			if tc.deletedAt {
				a.DeletedAt = time.Now()
			}
			assert.Equal(t, tc.want, a.IsInFlight())
		})
	}
}

func TestLaunchStore_S3_IncompleteCreatePredicate(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		phase       string
		launchState string
		launchError string
		deletedAt   bool
		want        bool
	}{
		{"active create wind-down, stopped", store.LaunchKindCreate, "stopped", store.LaunchStateActive, "", false, true},
		{"active create wind-down, error", store.LaunchKindCreate, "error", store.LaunchStateActive, "", false, true},
		{"ended create with error", store.LaunchKindCreate, "error", store.LaunchStateEnded, "launch_timeout", false, true},
		{"ended create with error, stopped phase", store.LaunchKindCreate, "stopped", store.LaunchStateEnded, "launch_stopped", false, true},
		{"ended create, no error: agent ran once", store.LaunchKindCreate, "error", store.LaunchStateEnded, "", false, false},
		{"running phase never matches", store.LaunchKindCreate, "running", store.LaunchStateActive, "launch_timeout", false, false},
		{"provisioning phase never matches", store.LaunchKindCreate, "provisioning", store.LaunchStateActive, "", false, false},
		{"start kind never matches in P1a", store.LaunchKindStart, "error", store.LaunchStateEnded, "launch_timeout", false, false},
		{"deleted row never matches", store.LaunchKindCreate, "error", store.LaunchStateEnded, "launch_timeout", true, false},
		{"suspended wind-down", store.LaunchKindCreate, "suspended", store.LaunchStateActive, "", false, true},
		{"stopping wind-down", store.LaunchKindCreate, "stopping", store.LaunchStateActive, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &store.Agent{LaunchKind: tc.kind, Phase: tc.phase, LaunchState: tc.launchState, LaunchError: tc.launchError}
			if tc.deletedAt {
				a.DeletedAt = time.Now()
			}
			assert.Equal(t, tc.want, a.IsIncompleteCreate())
		})
	}
}

// TestLaunchStore_S3_RanOnceNeverMatchesAgain proves the "an agent that ran
// once and was then stopped or errored never matches" rule end to end
// through the real write paths (UpdateAgentStatus's running clear), not just
// the pure predicate function.
func TestLaunchStore_S3_RanOnceNeverMatchesAgain(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "s3-ran-once")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"}))

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.False(t, after.IsIncompleteCreate(), "an agent that has run once must never match the incomplete-create predicate again")
}
