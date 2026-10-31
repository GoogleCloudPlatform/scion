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

// This file covers design t1-async-create-v11.md §6 test case H-1 (the
// report answer function), one case per branch of §3.7.
package entadapter

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func beginTestLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) (*store.Agent, string) {
	t.Helper()
	a := createLaunchableAgent(t, ctx, s, projectID, slug)
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	return a, launchID
}

// TestReport_H1_UnknownStateRejected proves ApplyLaunchReport rejects a
// State outside {claim, checkpoint, progress, succeeded, failed} rather than
// silently falling through to the progress branch.
func TestReport_H1_UnknownStateRejected(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-unknown-state")

	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "bogus",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// TestReport_H1_EmptyLaunchIDRejected proves an empty LaunchID is rejected
// outright, rather than being allowed to coincide with a never-launched
// row's current.LaunchID (also "") and fall through to the active-launch
// handling as if the row had a launch it never had.
func TestReport_H1_EmptyLaunchIDRejected(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, _ := beginTestLaunch(t, ctx, s, projectID, "h1-empty-launchid")

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: "", InstanceID: "i1", State: store.LaunchReportStateProgress,
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, before.StateVersion, after.StateVersion, "a rejected empty-LaunchID report must not write anything")
}

// TestReport_H1_NoLaunchOnRowIsSuperseded proves a report against an agent
// that has never had a launch (launch_state == "") is rejected as a
// superseded/unknown launch, not treated as if the launch were active: it
// must not write anything or bump state_version.
func TestReport_H1_NoLaunchOnRowIsSuperseded(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "h1-no-launch")

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "", before.LaunchID, "setup: this agent must never have had a launch")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: "some-launch-that-never-existed", InstanceID: "i1", State: store.LaunchReportStateFailed,
		ErrorCode: "runtime_error",
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportCodeStaleLaunch, answer.Code)
	assert.Equal(t, store.LaunchReportReasonSuperseded, answer.Reason)
	assert.Equal(t, before.Phase, updated.Phase, "a report against a row with no launch must not change phase")
	assert.Equal(t, before.StateVersion, updated.StateVersion, "a report against a row with no launch must not bump state_version")

	persisted, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, before.StateVersion, persisted.StateVersion, "nothing must have been written")
	assert.Equal(t, "", persisted.LaunchError)
}

func TestReport_H1_Superseded(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, _ := beginTestLaunch(t, ctx, s, projectID, "h1-superseded")

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: "some-other-launch", InstanceID: "instance-1", State: store.LaunchReportStateProgress,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportCodeStaleLaunch, answer.Code)
	assert.Equal(t, store.LaunchReportReasonSuperseded, answer.Reason)
}

func TestReport_H1_Deleted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-deleted")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", State: store.LaunchReportStateProgress,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportReasonDeleted, answer.Reason)
}

func TestReport_H1_UnknownAgent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)
	answer, _, err := s.ApplyLaunchReport(ctx, "00000000-0000-0000-0000-000000000000", "broker-1", store.LaunchReport{
		LaunchID: "L", InstanceID: "instance-1", State: store.LaunchReportStateProgress,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportCodeUnknownLaunch, answer.Code)
}

func TestReport_H1_ForbiddenWrongBroker(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-forbidden")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.RuntimeBrokerID = "broker-A"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, "broker-B", store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", State: store.LaunchReportStateProgress,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, answer.HTTPStatus)
}

func TestReport_H1_OtherOwner(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-other-owner")

	_, err := s.MarkLaunchAccepted(ctx, a.ID, launchID, "instance-1")
	require.NoError(t, err)

	// A succeeded/failed terminal from another instance -> 409 other_owner,
	// no row change.
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-2", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportReasonOtherOwner, answer.Reason)
	assert.NotEqual(t, "running", updated.Phase, "the row must not change")
}

func TestReport_H1_HubUnreachableWithEmptyOwnerAccepted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-hub-unreachable")

	// The owner is still empty (claim never answered). A terminal from an
	// instance is accepted even though it is not yet the recorded owner.
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", State: store.LaunchReportStateFailed,
		ErrorCode: store.LaunchErrorHubUnreachable, Step: "claim",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, store.LaunchErrorHubUnreachable, updated.LaunchError)
}

func TestReport_H1_SeqDuplicate(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-dup")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1, Step: "cloning",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	before := updated.LaunchLastReportAt
	time.Sleep(5 * time.Millisecond) // ensure the store clock advances measurably

	// A terminal bypasses seq, so a lost/replayed seq on it is harmless — but
	// here we send another progress at the same seq: duplicate.
	answer, updated, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1, Step: "cloning-retry",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultDuplicate, answer.Result)
	assert.True(t, updated.LaunchLastReportAt.After(before),
		"a duplicate report must still advance launch_last_report_at, or a healthy launch would be reaped as stale")
}

func TestReport_H1_KeepaliveOnlyDoesNotChangeStepPhaseMessage(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-keepalive")

	// The first report claims ownership (design: "the first owner claim"
	// publishes), so it is not itself a keepalive. Establish ownership and a
	// step first, then send the actual keepalive as a second report.
	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1, Step: "cloning",
	})
	require.NoError(t, err)

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // ensure the store clock advances measurably

	// A higher seq with no step/phase/message change is still "applied" (not
	// duplicate) at the store layer, but a caller (the Hub
	// report handler) must treat it as a keepalive: no publish, no bump. We
	// assert the store-level facts the handler depends on: no bump, and the
	// step/message are unchanged from before.
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, before.LaunchStep, updated.LaunchStep)
	assert.Equal(t, before.Message, updated.Message)
	assert.Equal(t, before.StateVersion, updated.StateVersion)
	assert.False(t, answer.Changed, "a keepalive-only report must not report Changed")
	assert.True(t, updated.LaunchLastReportAt.After(before.LaunchLastReportAt),
		"a keepalive report must still advance launch_last_report_at, or a healthy launch would be reaped as stale")
}

// TestReport_H1_StepChangeReportsChanged is the step-change counterpart to
// the keepalive test above. The first report to any launch always claims
// ownership and so always reports Changed regardless of content, so this
// establishes ownership with a first report before isolating the
// step-change behavior on a second one, and contrasts it with a third,
// pure-keepalive report.
func TestReport_H1_StepChangeReportsChanged(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-step-changed")

	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 2, Step: "cloning",
	})
	require.NoError(t, err)
	assert.True(t, answer.Changed, "a report that changes the step must report Changed")
	assert.Equal(t, "cloning", updated.LaunchStep)

	answer, updated, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 3,
	})
	require.NoError(t, err)
	assert.False(t, answer.Changed, "a pure keepalive report (no step/phase/message change) must not report Changed")
	assert.Equal(t, "cloning", updated.LaunchStep, "the step must be unchanged by the keepalive")
}

// TestReport_H1_ProgressCannotSkipToRunning proves a plain
// progress/claim/checkpoint report can never move phase to "running" or
// later — only a `succeeded` report does that, which also clears
// launch_error and ends the launch. A broker sending phase="running" via a
// progress message must not bypass the running rule.
func TestReport_H1_ProgressCannotSkipToRunning(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-progress-no-running")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1, Phase: "running",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.NotEqual(t, "running", updated.Phase, "a progress report must not be able to set phase=running")
	assert.Equal(t, store.LaunchStateActive, updated.LaunchState, "the launch must stay active — only a succeeded report ends it")
}

func TestReport_H1_SucceededPreRunning(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-succeeded")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: store.BrokerAgentConfig{Runtime: "kubernetes"}, RuntimeState: "Running",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, "running", updated.Phase)
	assert.Equal(t, "", updated.LaunchError)
	assert.Equal(t, store.LaunchStateEnded, updated.LaunchState)
	assert.Equal(t, store.LaunchEndReasonSucceeded, updated.LaunchEndReason)
	assert.Equal(t, "kubernetes", updated.Runtime)
	assert.True(t, answer.Changed, "a succeeded terminal is a real write and must report Changed")
}

func TestReport_H1_FailedPreRunning(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-failed")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "no nodes available", ErrorCode: "unschedulable",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, "error", updated.Phase)
	assert.Equal(t, "unschedulable", updated.LaunchError)
	assert.Equal(t, "pod_create: no nodes available", updated.Message)
	assert.Equal(t, store.LaunchEndReasonFailed, updated.LaunchEndReason)
	assert.True(t, answer.Changed, "a failed terminal is a real write and must report Changed")
}

// TestReport_H1_FailedPreRunningWithEmptyErrorCodeStillSetsLaunchError
// proves design §3.3's invariant that every create-launch end on an
// error/stopped phase leaves launch_error non-empty (IsIncompleteCreate
// depends on it) holds even when the broker's failed report carries no
// ErrorCode.
func TestReport_H1_FailedPreRunningWithEmptyErrorCodeStillSetsLaunchError(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-failed-no-code")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "unclassified failure",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, "error", updated.Phase)
	assert.NotEmpty(t, updated.LaunchError, "launch_error must not be left empty by a failed report with no ErrorCode")
	assert.True(t, updated.IsIncompleteCreate(), "a create launch ended with a non-empty launch_error must match the incomplete-create predicate")
}

func TestReport_H1_RunningThenProgressEndsAsCompleted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-running-progress")

	// A heartbeat/status write promotes the agent to running while the
	// launch is still active (e.g. a Docker `run -d` agent).
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	assert.Equal(t, store.LaunchStateEnded, updated.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, updated.LaunchEndReason)
	// UpdateAgentStatus already ended the launch as running_observed above,
	// so this progress report finds an already-ended launch (the ended
	// branch's RunningObserved case) and writes nothing.
	assert.False(t, answer.Changed, "a non-succeeded report against an already-ended (running_observed) launch is a no-op and must not report Changed")
}

func TestReport_H1_RunningObservedThenSucceededAppliesEcho(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-ro-succeeded")

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	require.Equal(t, store.LaunchEndReasonRunningObserved, updated.LaunchEndReason)
	phaseBefore := updated.Phase

	answer, updated, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: store.BrokerAgentConfig{Runtime: "docker"},
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	assert.True(t, answer.Changed, "the running_observed->succeeded echo is a real write and must report Changed")
	assert.Equal(t, phaseBefore, updated.Phase, "the phase must not change (preserveTerminalPhase)")
	assert.Equal(t, store.LaunchEndReasonSucceeded, updated.LaunchEndReason)
	assert.Equal(t, "docker", updated.Runtime)

	// Re-read from the store, not just the in-tx row ApplyLaunchReport
	// returned, to prove the write actually committed rather than having
	// been silently rolled back by a wrong Changed value.
	persisted, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchEndReasonSucceeded, persisted.LaunchEndReason, "the echo write must be committed, not rolled back")
	assert.Equal(t, "docker", persisted.Runtime)
}

func TestReport_H1_RunningObservedThenFailedStillCompletes(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-ro-failed")

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultCompleted, answer.Result, "failed after running_observed is completed, not applied — no row change")
	assert.Equal(t, before.StateVersion, updated.StateVersion)
	assert.Equal(t, before.LaunchEndReason, updated.LaunchEndReason)
}

func TestReport_H1_StoppedDuringLaunch(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-stopped")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "stopped"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportReasonStopped, answer.Reason)
	assert.Equal(t, store.LaunchErrorLaunchStopped, updated.LaunchError)
	assert.Equal(t, "stopped during launch", updated.Message)
	assert.Equal(t, store.LaunchEndReasonFailed, updated.LaunchEndReason)
	assert.True(t, answer.Changed, "ending the launch on a wind-down phase is a real write and must report Changed")
}

func TestReport_H1_ErrorByAnotherWriter_ProgressGets409(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-error-writer-progress")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	got.Message = "some other failure"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchEndReasonFailed, answer.Reason)
	assert.Equal(t, store.LaunchErrorAgentError, updated.LaunchError)
	assert.Equal(t, "some other failure", updated.Message, "the existing message is kept for a non-refining report")
	assert.True(t, answer.Changed, "ending the launch on the phase=error branch is a real write and must report Changed")
}

func TestReport_H1_ErrorByAnotherWriter_FailedRefinesAndApplies(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-error-writer-failed")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	got.Message = "some other failure"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "crash loop", ErrorCode: "crash_loop",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result, "a failed report on error-by-another-writer is applied, not completed")
	assert.Equal(t, "crash_loop", updated.LaunchError)
	assert.Equal(t, "pod_create: crash loop", updated.Message)
	assert.True(t, answer.Changed, "ending the launch on the phase=error branch is a real write and must report Changed")
}

// TestReport_H1_ErrorByAnotherWriter_FailedWithEmptyErrorCodeStillSetsLaunchError
// is the PhaseError-branch counterpart to the pre-running empty-ErrorCode
// test above: design §3.3's non-empty-launch_error invariant must hold here
// too, and must not overwrite a launch_error the row already carries.
func TestReport_H1_ErrorByAnotherWriter_FailedWithEmptyErrorCodeStillSetsLaunchError(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-error-writer-failed-no-code")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "unclassified failure",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.NotEmpty(t, updated.LaunchError, "launch_error must not be left empty by a failed report with no ErrorCode")
	assert.True(t, updated.IsIncompleteCreate())
}

// TestReport_H1_FailedConfirmingReapedLaunchRefinesLaunchError covers the
// ended-branch refine case: the reaper already timed a launch out (moving
// phase to error), and a failed report then arrives confirming the same
// failure. It must be applied (refining launch_error/message) rather than
// rejected as stale, and the write must be a real, committed one.
func TestReport_H1_FailedConfirmingReapedLaunchRefinesLaunchError(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-refine-after-reap")

	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonTimedOut))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	require.NoError(t, s.UpdateAgent(ctx, got))

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "crash loop", ErrorCode: "crash_loop",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result, "a failed report confirming a reaper-timed-out launch is applied, not completed")
	assert.True(t, answer.Changed, "the refine write is a real write and must report Changed")
	assert.Equal(t, "crash_loop", updated.LaunchError)
	assert.Equal(t, "pod_create: crash loop", updated.Message)

	// Re-read from the store to prove the refine write actually committed
	// rather than having been silently rolled back by a wrong Changed value.
	persisted, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "crash_loop", persisted.LaunchError, "the refine write must be committed, not rolled back")
	assert.Equal(t, "pod_create: crash loop", persisted.Message)
	assert.Equal(t, store.LaunchEndReasonTimedOut, persisted.LaunchEndReason, "the end reason itself is not changed by the refine")
}

// The "SSE carries launch" / H-4 client-contract assertions (AgentLaunch,
// ComputeAgentLaunch) live in P1a-ii: that type is part of the H-4 client
// contract the P1a/P1a-ii split assigns to P1a-ii, with its own tests
// (active/ended, clamp-at-0, ceiling rounding), not half-tested here.

// --- ApplyLaunchReport failed-path reservation release ---------------------

// testQuotaBroker is the Runtime Broker the launch-end reservation tests
// record on the agent row and scope the reservation to.
const testQuotaBroker = "broker-1"

// setAgentRuntimeBroker records broker on agent a's row through the ent
// client (white-box: the launch tests' agents are created with no broker)
// and on the caller's struct, which the tests' reports are sent from (a
// report from any other broker is refused).
func setAgentRuntimeBroker(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, broker string) {
	t.Helper()
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetRuntimeBrokerID(broker).Save(ctx)
	require.NoError(t, err)
	a.RuntimeBrokerID = broker
}

// seedBrokerQuotaReservation records testQuotaBroker as a's Runtime Broker
// (row and struct), creates a max_agents_per_broker limit definition and an
// active reservation for a scoped to that broker, and returns the limit
// definition ID for a HasActiveReservation check. A launch end releases only
// a reservation on the agent's own broker (ptone/scion#5137).
func seedBrokerQuotaReservation(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent) (quota *QuotaStore, limitDefID string) {
	t.Helper()
	setAgentRuntimeBroker(t, ctx, s, a, testQuotaBroker)
	quota = NewQuotaStore(s.client)
	ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
	})
	require.NoError(t, err)
	_, err = quota.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld.ID, SubjectID: testQuotaBroker, ScopeType: "broker", ScopeID: testQuotaBroker,
		ResourceID: a.ID, Reserved: 1,
	})
	require.NoError(t, err)
	has, err := quota.HasActiveReservation(ctx, ld.ID, a.ID)
	require.NoError(t, err)
	require.True(t, has, "setup: the reservation must be active before the failed report")
	return quota, ld.ID
}

func TestReport_H1_FailedPreRunning_ReleasesReservation(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-release-prerunning")
	quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)

	_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "boom", ErrorCode: "runtime_error",
	})
	require.NoError(t, err)

	has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "a failed pre-running report must release the max_agents_per_broker reservation")
}

func TestReport_H1_ErrorByAnotherWriter_FailedReleasesReservation(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-release-error-writer")
	quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = "error"
	got.Message = "some other failure"
	require.NoError(t, s.UpdateAgent(ctx, got))

	_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "crash loop", ErrorCode: "crash_loop",
	})
	require.NoError(t, err)

	has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "a failed report on the error-by-another-writer branch must release the reservation")
}

// TestReport_H1_FailedPreRunning_ReleasesReservationOnPGWithSingleConnection
// is the Postgres counterpart to TestReport_H1_FailedPreRunning_ReleasesReservation:
// with MaxOpenConns=1, this can only complete if
// releaseBrokerQuotaTx reuses ApplyLaunchReport's own transaction/connection
// rather than opening a second one on s.client, which would deadlock against
// the exhausted pool.
func TestReport_H1_FailedPreRunning_ReleasesReservationOnPGWithSingleConnection(t *testing.T) {
	ctx := context.Background()
	s, projectID := newPGTestAgentStore(t, 1)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-release-prerunning-pg-single-conn")
	quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "boom", ErrorCode: "runtime_error",
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result, "the report must complete within budget on a single-connection pool")

	has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "a failed pre-running report must release the reservation through its own transaction, not a second connection")
}

// --- additional H-1 cases ---------------------------------------------------

func TestReport_H1_MarkLaunchAcceptedSetsOwnerOnFirstClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-owner-claim")

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "", before.LaunchOwner, "setup: the owner must start empty")

	accepted, err := s.MarkLaunchAccepted(ctx, a.ID, launchID, "instance-1")
	require.NoError(t, err)
	assert.Equal(t, "instance-1", accepted.LaunchOwner, "the first claim must set launch_owner")

	// A second claim from a different instance does not steal ownership.
	accepted2, err := s.MarkLaunchAccepted(ctx, a.ID, launchID, "instance-2")
	require.NoError(t, err)
	assert.Equal(t, "instance-1", accepted2.LaunchOwner, "MarkLaunchAccepted must not overwrite an existing owner")
}

// TestReport_H1_MarkLaunchAcceptedOnSupersededLaunchIsNoOp covers
// MarkLaunchAccepted's no-op path: a launchID that no longer matches the
// row's current launch (already superseded by a new BeginLaunch) must not
// write anything, and returns the current row so the caller can detect the
// mismatch itself.
func TestReport_H1_MarkLaunchAcceptedOnSupersededLaunchIsNoOp(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, staleLaunchID := beginTestLaunch(t, ctx, s, projectID, "h1-accept-superseded")

	require.NoError(t, s.EndLaunch(ctx, a.ID, staleLaunchID, store.LaunchEndReasonSuperseded))
	newLaunchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, staleLaunchID, newLaunchID, "setup: a fresh BeginLaunch must mint a new launch ID")

	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	accepted, err := s.MarkLaunchAccepted(ctx, a.ID, staleLaunchID, "stale-instance")
	require.NoError(t, err)
	assert.Equal(t, newLaunchID, accepted.LaunchID, "the no-op must return the row's current (new) launch, not write the stale one")
	assert.Equal(t, "", accepted.LaunchOwner, "a no-op accept for a superseded launch must not claim ownership of the new one")
	assert.Equal(t, before.StateVersion, accepted.StateVersion, "MarkLaunchAccepted's no-op path must not bump state_version")
}

func TestReport_H1_ForbiddenAfterReassignment(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-reassigned")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.RuntimeBrokerID = "broker-original"
	require.NoError(t, s.UpdateAgent(ctx, got))

	// A report from the original broker is accepted.
	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, "broker-original", store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)

	// The agent is reassigned to a different broker (e.g. broker affinity
	// reap). The original broker's late report is now 403, including after
	// the reassignment -- not just for a broker that was never assigned.
	got2, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got2.RuntimeBrokerID = "broker-new"
	require.NoError(t, s.UpdateAgent(ctx, got2))

	answer, _, err = s.ApplyLaunchReport(ctx, a.ID, "broker-original", store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, answer.HTTPStatus, "a report from the pre-reassignment broker must be forbidden")
}

func TestReport_H1_SucceededAfterReapIsTimedOut(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-succeeded-after-reap")

	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonTimedOut))
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("error").SetLaunchError(store.LaunchErrorLaunchTimeout).Save(ctx)
	require.NoError(t, err)

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchEndReasonTimedOut, answer.Reason)
	assert.Equal(t, "error", updated.Phase, "the row stays error/launch_timeout; a late succeeded gets no grace (design §3.10)")
	assert.Equal(t, store.LaunchErrorLaunchTimeout, updated.LaunchError)
}

func TestReport_H1_NotLaunchedGets409(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-not-launched")

	require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, answer.Reason)
}

func TestReport_H1_RunningPhaseSucceededAppliesEcho(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-running-succeeded")

	// A status write promotes phase=running through UpdateAgentStatus, which
	// (design §3.3) ends the launch as running_observed in the same write --
	// so to reach applyLaunchReportActive's PhaseRunning+succeeded branch
	// with the launch still ACTIVE, write phase=running directly via the ent
	// client (white-box), bypassing that rule, the same way a docker
	// `run -d` agent's heartbeat could race a checkpoint's write ordering.
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("running").Save(ctx)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateActive, before.LaunchState, "setup: the launch must still be active")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded, AgentConfig: store.BrokerAgentConfig{Runtime: "docker"},
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.Equal(t, "running", updated.Phase, "preserveTerminalPhase: the phase must not change")
	assert.Equal(t, store.LaunchStateEnded, updated.LaunchState)
	assert.Equal(t, store.LaunchEndReasonSucceeded, updated.LaunchEndReason)
	assert.Equal(t, "docker", updated.Runtime)
	assert.True(t, answer.Changed, "a succeeded terminal on the active PhaseRunning branch is a real write and must report Changed")
}

// TestReport_H1_RunningPhaseNonSucceededEndsAsRunningObserved is the
// non-succeeded counterpart to the echo test above: with phase=running
// reached while the launch is still ACTIVE (the same white-box race), a
// non-terminal or failed report must still end the launch as
// running_observed, and that is a real write.
func TestReport_H1_RunningPhaseNonSucceededEndsAsRunningObserved(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "h1-running-active-progress")

	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("running").Save(ctx)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateActive, before.LaunchState, "setup: the launch must still be active")

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	assert.Equal(t, store.LaunchStateEnded, updated.LaunchState)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, updated.LaunchEndReason)
	assert.True(t, answer.Changed, "ending an active launch as running_observed is a real write and must report Changed")
}

// TestReport_BrokerProgressSequenceUpdatesLaunchStep replays the reports an
// async Kubernetes launch's broker sends (ptone/scion#4670 phase E1b): claim,
// a pre-create checkpoint, then a progress report carrying a runtime step,
// then a keepalive at the same seq. The progress step must become the
// agent's launch step (the SSE/REST launch view) and report Changed so the
// hub publishes it; the keepalive must not undo it.
func TestReport_BrokerProgressSequenceUpdatesLaunchStep(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "e1b-progress-step")

	report := func(seq int64, state, step, message string) (store.LaunchReportAnswer, store.Agent) {
		t.Helper()
		answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: state, Seq: seq, Step: step, Message: message,
		})
		require.NoError(t, err)
		require.Zero(t, answer.HTTPStatus, "report %s seq %d answered %+v", state, seq, answer)
		return answer, updated
	}

	_, claimed := report(1, store.LaunchReportStateClaim, "", "")
	_, updated := report(2, store.LaunchReportStateCheckpoint, "pod_create", "")
	assert.Equal(t, "pod_create", updated.LaunchStep)

	answer, updated := report(3, store.LaunchReportStateProgress, "container_create", "preparing the container (image pull, volume mounts)")
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.True(t, answer.Changed, "a progress report with a new step must report Changed, or the hub does not publish it")
	assert.Equal(t, "container_create", updated.LaunchStep)
	assert.Equal(t, "preparing the container (image pull, volume mounts)", updated.Message)
	assert.Equal(t, claimed.Phase, updated.Phase, "a progress report must not move the phase")
	assert.Equal(t, store.LaunchStateActive, updated.LaunchState, "a progress report must not end the launch")

	// The keepalive reuses the current seq.
	answer, _ = report(3, store.LaunchReportStateProgress, "", "")
	assert.Equal(t, store.LaunchReportResultDuplicate, answer.Result)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	view := store.ComputeAgentLaunch(got, time.Now())
	require.NotNil(t, view, "an active launch must have a launch view")
	assert.Equal(t, "container_create", view.Step)
}

// --- a failed report after running_observed (ptone/scion#5368) -------------
//
// Unless a test says otherwise, the launch ended running_observed through a
// heartbeat-shaped status write (phase running, no activity), the report
// names the row's run, and the row holds no claim, delete or reincarnation.

const roRunID = "run-current"

// updateRow applies a white-box mutation to the agent row through the ent
// client (test setup for columns the store API does not set directly).
func updateRow(t *testing.T, ctx context.Context, s *AgentStore, agentID string, mutate func(u *ent.AgentUpdateOne)) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	u := s.client.Agent.UpdateOneID(uid)
	mutate(u)
	_, err = u.Save(ctx)
	require.NoError(t, err)
}

// endRunningObserved records run roRunID on the row and ends its active
// launch running_observed with a heartbeat-shaped status write, then clears
// the activity the test fixture created the agent with.
func endRunningObserved(t *testing.T, ctx context.Context, s *AgentStore, agentID string) {
	t.Helper()
	updateRow(t, ctx, s, agentID, func(u *ent.AgentUpdateOne) { u.SetRunID(roRunID) })
	require.NoError(t, s.UpdateAgentStatus(ctx, agentID, store.AgentStatusUpdate{Phase: "running"}))
	updateRow(t, ctx, s, agentID, func(u *ent.AgentUpdateOne) { u.SetActivity("") })
	got, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateEnded, got.LaunchState, "setup: the launch must have ended")
	require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason, "setup: the launch must have ended running_observed")
	require.Equal(t, "running", got.Phase)
	require.Equal(t, roRunID, got.RunID)
	require.Empty(t, got.Activity)
}

// beginRunningObservedLaunch begins a create launch on a fresh agent and
// ends it running_observed (endRunningObserved).
func beginRunningObservedLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) (*store.Agent, string) {
	t.Helper()
	a, launchID := beginTestLaunch(t, ctx, s, projectID, slug)
	endRunningObserved(t, ctx, s, a.ID)
	return a, launchID
}

// roFailedReport is the broker's failed report for launchID, naming runID.
func roFailedReport(launchID, runID string) store.LaunchReport {
	return store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "workspace_sync", Message: "launch timed out before the agent started",
		ErrorCode: "launch_timeout", RunID: runID,
	}
}

// assertROApplied checks that the failed report ended the launch failed.
func assertROApplied(t *testing.T, ctx context.Context, s *AgentStore, agentID string, answer store.LaunchReportAnswer) {
	t.Helper()
	assert.Equal(t, 0, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.True(t, answer.Changed)
	got, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason, "the applied failure must be committed")
	assert.Equal(t, "launch_timeout", got.LaunchError)
}

// assertROCompletedUnchanged checks that the report was answered completed
// and the row was not written.
func assertROCompletedUnchanged(t *testing.T, ctx context.Context, s *AgentStore, before *store.Agent, answer store.LaunchReportAnswer) {
	t.Helper()
	assert.Equal(t, 0, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportResultCompleted, answer.Result)
	assert.False(t, answer.Changed)
	got, err := s.GetAgent(ctx, before.ID)
	require.NoError(t, err)
	assert.Equal(t, before.StateVersion, got.StateVersion, "the row must not be written")
	assert.Equal(t, before.LaunchEndReason, got.LaunchEndReason)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.LaunchError, got.LaunchError)
}

func TestApplyLaunchReport_FailedAfterRunningObserved_RecordsLaunchError(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-records")
	quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	assert.True(t, answer.Changed)
	assert.Equal(t, "error", updated.Phase)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, "launch_timeout", got.LaunchError)
	assert.Equal(t, "workspace_sync", got.LaunchStep)
	assert.Equal(t, "workspace_sync: launch timed out before the agent started", got.Message)
	assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason)
	assert.Greater(t, got.StateVersion, before.StateVersion)

	has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "the applied failure must release the broker quota reservation")
}

func TestApplyLaunchReport_FailedAfterRunningObserved_PhaseError_Applied(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-phase-error")
	// The broker's failed Start writes error to its agent-info, and a
	// heartbeat relays it before the report lands.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "error"}))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "error", got.Phase)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assertROApplied(t, ctx, s, a.ID, answer)
}

func TestApplyLaunchReport_FailedAfterRunningObserved_StartKind_UsesFailureEndPhase(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	// A start from stopped restores stopped on a failure end.
	a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "ro-start-kind", store.LaunchKindStart, "stopped", "")
	endRunningObserved(t, ctx, s, a.ID)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assertROApplied(t, ctx, s, a.ID, answer)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase, "a start-kind launch ends in its failure phase")
}

func TestApplyLaunchReport_FailedAfterRunningObserved_TerminalActivityIsNotEvidence(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-terminal-activity")
	// A terminal activity left by an earlier run.
	updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetActivity("crashed") })

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assertROApplied(t, ctx, s, a.ID, answer)
}

func TestApplyLaunchReport_FailedAfterRunningObserved_RunFence(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cases := []struct {
		name    string
		runID   string
		applied bool
	}{
		{"no run ID (older broker)", "", false},
		{"an older run", "run-older", false},
		{"the current run", roRunID, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-run-fence-"+string(rune('a'+i)))
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, c.runID))
			require.NoError(t, err)
			if c.applied {
				assertROApplied(t, ctx, s, a.ID, answer)
			} else {
				assertROCompletedUnchanged(t, ctx, s, before, answer)
			}
		})
	}
}

func TestApplyLaunchReport_FailedAfterRunningObserved_LiveActivity_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	for i, activity := range []string{"initializing", "working"} {
		t.Run(activity, func(t *testing.T) {
			a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-live-activity-"+string(rune('a'+i)))
			updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetActivity(activity) })
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
			require.NoError(t, err)
			assertROCompletedUnchanged(t, ctx, s, before, answer)
		})
	}
}

func TestApplyLaunchReport_FailedAfterRunningObserved_BrokerRestarted_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-broker-restarted")
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	r := roFailedReport(launchID, roRunID)
	r.ErrorCode = store.LaunchErrorBrokerRestarted
	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, r)
	require.NoError(t, err)
	assertROCompletedUnchanged(t, ctx, s, before, answer)
}

func TestApplyLaunchReport_FailedAfterRunningObserved_PhaseNotRunningOrError_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	for _, phase := range []string{"created", "provisioning", "cloning", "starting", "stopped", "stopping", "suspended", "halted"} {
		t.Run(phase, func(t *testing.T) {
			a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-phase-"+phase)
			updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetPhase(phase) })
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, phase, before.Phase)
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
			require.NoError(t, err)
			assertROCompletedUnchanged(t, ctx, s, before, answer)
		})
	}
}

// beginClaimedRunningObservedLaunch takes a create claim, begins a create
// launch (which links the claim to it) and ends it running_observed.
func beginClaimedRunningObservedLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) (*store.Agent, string) {
	t.Helper()
	a := createLaunchableAgent(t, ctx, s, projectID, slug)
	_, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
	require.NoError(t, err)
	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	endRunningObserved(t, ctx, s, a.ID)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotEmpty(t, got.StartClaimID, "setup: a status-written running_observed end keeps the claim")
	require.Equal(t, launchID, got.StartClaimLaunchID, "setup: the claim is linked to the launch")
	return a, launchID
}

func TestApplyLaunchReport_SucceededAfterRunningObserved_SettlesLinkedClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	succeeded := func(launchID string) store.LaunchReport {
		return store.LaunchReport{LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded}
	}

	t.Run("a claim linked to this launch is released", func(t *testing.T) {
		a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-succ-linked")
		answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, succeeded(launchID))
		require.NoError(t, err)
		assert.Equal(t, store.LaunchReportResultCompleted, answer.Result)
		assert.True(t, answer.Changed)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchEndReasonSucceeded, got.LaunchEndReason)
		assert.Empty(t, got.StartClaimID, "the succeeded report releases the claim the status-ended launch kept")
		assert.Empty(t, got.StartClaimLaunchID)
	})
	t.Run("a claim for another launch is kept", func(t *testing.T) {
		a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-succ-unlinked")
		updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetStartClaimLaunchID("launch-later") })
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, succeeded(launchID))
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchEndReasonSucceeded, got.LaunchEndReason)
		assert.Equal(t, before.StartClaimID, got.StartClaimID, "a claim not linked to this launch is not this report's to settle")
	})
	t.Run("a resend after the release writes nothing", func(t *testing.T) {
		a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-succ-resend")
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, succeeded(launchID))
		require.NoError(t, err)
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, succeeded(launchID))
		require.NoError(t, err)
		assert.False(t, answer.Changed)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, before.StateVersion, got.StateVersion)
	})
}

func TestApplyLaunchReport_FailedAfterRunningObserved_UnlinkedStartClaim_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("a claim for another launch refuses", func(t *testing.T) {
		a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-claim-unlinked")
		updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetStartClaimLaunchID("launch-later") })
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
		require.NoError(t, err)
		assertROCompletedUnchanged(t, ctx, s, before, answer)
	})
	t.Run("a claim linked to this launch applies", func(t *testing.T) {
		a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-claim-linked")
		answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
		require.NoError(t, err)
		assertROApplied(t, ctx, s, a.ID, answer)
	})
	t.Run("no claim applies", func(t *testing.T) {
		a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-claim-none")
		answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
		require.NoError(t, err)
		assertROApplied(t, ctx, s, a.ID, answer)
	})
}

func TestApplyLaunchReport_FailedAfterRunningObserved_DeleteHold_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cases := []struct {
		name    string
		state   string
		leaseAt time.Time
	}{
		{"deleting with an active lease", store.DeletionStateDeleting, time.Now().Add(time.Hour)},
		{"finalizing with an expired lease", store.DeletionStateFinalizing, time.Now().Add(-time.Hour)},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-delete-hold-"+string(rune('a'+i)))
			updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) {
				u.SetDeletionState(c.state).SetDeletionLeaseAt(c.leaseAt)
			})
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
			require.NoError(t, err)
			assertROCompletedUnchanged(t, ctx, s, before, answer)
		})
	}
}

func TestApplyLaunchReport_FailedAfterRunningObserved_ReincarnationInFlight_Completed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cases := []struct {
		state   string
		applied bool
	}{
		{store.ReincarnationStatePending, false},
		{store.ReincarnationStateStopping, false},
		{store.ReincarnationStateProvisioning, false},
		{store.ReincarnationStateStarting, false},
		{store.ReincarnationStateFailed, true},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-reincarnation-"+c.state)
			// The phase stays running, as it does while a pending
			// reincarnation waits for its worker.
			updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetReincarnationState(c.state) })
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, "running", before.Phase)
			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
			require.NoError(t, err)
			if c.applied {
				assertROApplied(t, ctx, s, a.ID, answer)
			} else {
				assertROCompletedUnchanged(t, ctx, s, before, answer)
			}
		})
	}
}

func TestApplyLaunchReport_FailedAfterRunningObserved_ResendAfterApplied_409Failed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-resend")
	r := roFailedReport(launchID, roRunID)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, r)
	require.NoError(t, err)
	if !assert.Equal(t, store.LaunchReportResultApplied, answer.Result, "the first send applies") {
		return
	}
	afterFirst, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	answer, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, r)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportCodeStaleLaunch, answer.Code)
	assert.Equal(t, store.LaunchEndReasonFailed, answer.Reason)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, afterFirst.StateVersion, got.StateVersion, "the resend must not write")
}

func TestApplyLaunchReport_FailedAfterRunningObserved_HeartbeatEndedClaimSettles(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginClaimedRunningObservedLaunch(t, ctx, s, projectID, "ro-claim-settles")

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assertROApplied(t, ctx, s, a.ID, answer)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.StartClaimID, "the applied failure releases the claim the heartbeat-ended launch kept")
}

func TestApplyLaunchReport_FailedAfterRunningObserved_SucceededEnd_StillCompleted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginTestLaunch(t, ctx, s, projectID, "ro-succeeded-end")
	updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetRunID(roRunID) })
	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetActivity("") })
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchEndReasonSucceeded, before.LaunchEndReason)

	answer, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	assertROCompletedUnchanged(t, ctx, s, before, answer)
}

func TestApplyLaunchReport_FailedAfterRunningObserved_OtherLaunchID_Superseded(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, _ := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-other-launch")
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport("launch-other", roRunID))
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, answer.HTTPStatus)
	assert.Equal(t, store.LaunchReportReasonSuperseded, answer.Reason)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, before.StateVersion, got.StateVersion)
}

// TestApplyLaunchReportEnded_OnlyNewBranchEndsFailed pins the invariant the
// broker's "409 failed means this run's failure was applied" rule relies
// on: once a launch has ended running_observed or succeeded, no report but
// an eligible failed report on running_observed ends it failed.
func TestApplyLaunchReportEnded_OnlyNewBranchEndsFailed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	reports := []struct {
		name     string
		report   func(launchID string) store.LaunchReport
		eligible bool
		// mutate, when set, changes the row after the launch has ended.
		mutate func(u *ent.AgentUpdateOne)
	}{
		{"claim", func(l string) store.LaunchReport {
			return store.LaunchReport{LaunchID: l, InstanceID: "i1", State: store.LaunchReportStateClaim, Seq: 10}
		}, false, nil},
		{"checkpoint", func(l string) store.LaunchReport {
			return store.LaunchReport{LaunchID: l, InstanceID: "i1", State: store.LaunchReportStateCheckpoint, Seq: 11}
		}, false, nil},
		{"progress", func(l string) store.LaunchReport {
			return store.LaunchReport{LaunchID: l, InstanceID: "i1", State: store.LaunchReportStateProgress, Seq: 12, Step: "home_sync"}
		}, false, nil},
		{"succeeded", func(l string) store.LaunchReport {
			return store.LaunchReport{LaunchID: l, InstanceID: "i1", State: store.LaunchReportStateSucceeded}
		}, false, nil},
		{"failed without a run ID", func(l string) store.LaunchReport { return roFailedReport(l, "") }, false, nil},
		{"failed for another run", func(l string) store.LaunchReport { return roFailedReport(l, "run-older") }, false, nil},
		{"failed broker_restarted", func(l string) store.LaunchReport {
			r := roFailedReport(l, roRunID)
			r.ErrorCode = store.LaunchErrorBrokerRestarted
			return r
		}, false, nil},
		{"failed with live activity", func(l string) store.LaunchReport { return roFailedReport(l, roRunID) }, false,
			func(u *ent.AgentUpdateOne) { u.SetActivity("working") }},
		{"failed on a stopped row", func(l string) store.LaunchReport { return roFailedReport(l, roRunID) }, false,
			func(u *ent.AgentUpdateOne) { u.SetPhase("stopped") }},
		{"failed, eligible", func(l string) store.LaunchReport { return roFailedReport(l, roRunID) }, true, nil},
	}
	ends := []string{store.LaunchEndReasonRunningObserved, store.LaunchEndReasonSucceeded}

	n := 0
	for _, end := range ends {
		for _, rc := range reports {
			n++
			slug := "ro-invariant-" + string(rune('a'+n))
			t.Run(end+"/"+rc.name, func(t *testing.T) {
				var a *store.Agent
				var launchID string
				if end == store.LaunchEndReasonRunningObserved {
					a, launchID = beginRunningObservedLaunch(t, ctx, s, projectID, slug)
				} else {
					a, launchID = beginTestLaunch(t, ctx, s, projectID, slug)
					updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetRunID(roRunID) })
					_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
						LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
					})
					require.NoError(t, err)
					updateRow(t, ctx, s, a.ID, func(u *ent.AgentUpdateOne) { u.SetActivity("") })
				}
				if rc.mutate != nil {
					updateRow(t, ctx, s, a.ID, rc.mutate)
				}
				_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, rc.report(launchID))
				require.NoError(t, err)
				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				if rc.eligible && end == store.LaunchEndReasonRunningObserved {
					assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason, "the eligible failed report ends a running_observed launch failed")
				} else {
					assert.NotEqual(t, store.LaunchEndReasonFailed, got.LaunchEndReason, "no other report may end a completed launch failed")
				}
			})
		}
	}
}

// TestApplyLaunchReport_FailedAfterRunningObserved_ReleasesReservationOnPGWithSingleConnection
// is the Postgres counterpart of the quota assertion in
// TestApplyLaunchReport_FailedAfterRunningObserved_RecordsLaunchError: with
// MaxOpenConns=1 it completes only if the failure end after
// running_observed releases the reservation in ApplyLaunchReport's own
// transaction. It skips without a Postgres test database.
func TestApplyLaunchReport_FailedAfterRunningObserved_ReleasesReservationOnPGWithSingleConnection(t *testing.T) {
	ctx := context.Background()
	s, projectID := newPGTestAgentStore(t, 1)
	a, launchID := beginRunningObservedLaunch(t, ctx, s, projectID, "ro-release-pg-single-conn")
	quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, roFailedReport(launchID, roRunID))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result, "the report must complete within budget on a single-connection pool")

	has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "the failure end must release the reservation through its own transaction, not a second connection")
}
