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

// Store support for start and restart launches (ptone/scion#4669, design
// wave 3 phase D1): BeginStartLaunch, the failure-phase rule and resume
// memory on every start-kind launch end, the resume-memory clearing hook,
// and claim linking and settlement for user and restart claims. Every test
// here also runs against Postgres in the integration build (enttest).
package entadapter

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAgentInPhase seeds an agent whose stored phase is phase.
func createAgentInPhase(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug, phase string) *store.Agent {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.Phase = phase
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

// setRowPhaseAndMemory writes phase and launch_resume_phase in one
// white-box mutation. The explicit memory wins over the clearing hook, so
// a test can stage any (phase, memory) pair.
func setRowPhaseAndMemory(t *testing.T, ctx context.Context, s *AgentStore, agentID, phase, memory string) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase(phase).SetLaunchResumePhase(memory).Save(ctx)
	require.NoError(t, err)
}

// setRowMemory writes only launch_resume_phase (no phase, so the hook does
// not act).
func setRowMemory(t *testing.T, ctx context.Context, s *AgentStore, agentID, memory string) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetLaunchResumePhase(memory).Save(ctx)
	require.NoError(t, err)
}

// beginStartKindLaunch seeds an agent in the phase a start-kind launch
// begins from (starting for a start, stopped for a restart), stages the
// given resume memory, and begins the launch with prior.
func beginStartKindLaunch(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug, kind, prior, memory string) (*store.Agent, string) {
	t.Helper()
	phase := "starting"
	if kind == store.LaunchKindRestart {
		phase = "stopped"
	}
	a := createAgentInPhase(t, ctx, s, projectID, slug, phase)
	if memory != "" {
		setRowMemory(t, ctx, s, a.ID, memory)
	}
	launchID, err := s.BeginStartLaunch(ctx, a.ID, kind, time.Hour, store.BeginLaunchOptions{PriorPhase: prior})
	require.NoError(t, err)
	return a, launchID
}

// --- BeginStartLaunch (D-T1) -----------------------------------------------

func TestBeginStartLaunch_KindPhaseTable(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	phases := []string{"created", "provisioning", "cloning", "starting", "running", "stopping", "stopped", "suspended", "error", "halted"}
	accepted := map[string]map[string]bool{
		store.LaunchKindStart:   {"starting": true},
		store.LaunchKindRestart: {"starting": true, "running": true, "stopped": true},
	}
	for _, kind := range []string{store.LaunchKindStart, store.LaunchKindRestart} {
		for _, phase := range phases {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				a := createAgentInPhase(t, ctx, s, projectID, "bsl-"+kind+"-"+phase, phase)
				setAgentLaunchError(t, ctx, s, a.ID, "launch_timeout")
				setRowMemory(t, ctx, s, a.ID, "suspended")
				before, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)

				launchID, err := s.BeginStartLaunch(ctx, a.ID, kind, time.Hour,
					store.BeginLaunchOptions{PriorPhase: "suspended", RevokeOnFail: true})
				after, gerr := s.GetAgent(ctx, a.ID)
				require.NoError(t, gerr)

				if !accepted[kind][phase] {
					assert.ErrorIs(t, err, store.ErrInvalidPhase)
					assert.Empty(t, after.LaunchID, "a refused BeginStartLaunch writes nothing")
					assert.Equal(t, before.StateVersion, after.StateVersion)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, launchID, after.LaunchID)
				assert.Equal(t, store.LaunchStateActive, after.LaunchState)
				assert.Equal(t, kind, after.LaunchKind)
				assert.Equal(t, "suspended", after.LaunchPriorPhase)
				assert.True(t, after.LaunchRevokeOnFail)
				assert.Empty(t, after.LaunchError, "a new launch clears launch_error")
				assert.Equal(t, "suspended", after.LaunchResumePhase, "beginning a launch leaves the resume memory alone")
				assert.Equal(t, "starting", after.Phase)
				if kind == store.LaunchKindRestart && phase != "starting" {
					assert.Equal(t, before.StateVersion+1, after.StateVersion, "a restart that moves the phase to starting bumps state_version")
				} else {
					assert.Equal(t, before.StateVersion, after.StateVersion, "no phase change, no bump")
				}
			})
		}
	}

	t.Run("create kind is refused", func(t *testing.T) {
		a := createAgentInPhase(t, ctx, s, projectID, "bsl-create", "created")
		_, err := s.BeginStartLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour, store.BeginLaunchOptions{})
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	})
}

func TestBeginLaunch_CreateResetsStartColumns(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createAgentInPhase(t, ctx, s, projectID, "bl-reset", "starting")
	_, err := s.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, time.Hour,
		store.BeginLaunchOptions{PriorPhase: "stopped", RevokeOnFail: true})
	require.NoError(t, err)

	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("created").Save(ctx)
	require.NoError(t, err)
	_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchKindCreate, got.LaunchKind)
	assert.Empty(t, got.LaunchPriorPhase, "a create launch carries no prior phase")
	assert.False(t, got.LaunchRevokeOnFail, "a create launch carries no start revoke decision")
}

// --- Failure phase rule and memory write (D-T2, D-T12 e2) -------------------

// endStartLaunchByFailure ends the agent's active launch with a failure of
// the given kind: a broker failed report, a reaper deadline reap, or a
// reaper staleness reap.
func endStartLaunchByFailure(t *testing.T, ctx context.Context, s *AgentStore, a *store.Agent, launchID, how string) {
	t.Helper()
	switch how {
	case "report_failed":
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
			Step: "pod_create", Message: "boom", ErrorCode: "runtime_error",
		})
		require.NoError(t, err)
	case "reaper_deadline":
		setAgentLaunchDeadline(t, ctx, s, a.ID, readStoreNow(t, ctx, s).Add(-time.Second))
		res, err := s.RunLaunchReaperTick(ctx, testReaperParams)
		require.NoError(t, err)
		require.Len(t, res.Reaped, 1)
	case "reaper_lost":
		_, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
		require.NoError(t, err)
		setLaunchReaperArmedSince(t, ctx, s, readStoreNow(t, ctx, s).Add(-time.Hour))
		setAgentLastReportAt(t, ctx, s, a.ID, readStoreNow(t, ctx, s).Add(-time.Hour))
		res, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
		require.NoError(t, err)
		require.Len(t, res.Reaped, 1)
	default:
		t.Fatalf("unknown failure %q", how)
	}
}

func TestStartLaunch_FailurePhaseRule(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		kind, prior           string
		wantPhase, wantMemory string
	}{
		{store.LaunchKindRestart, "running", "stopped", ""},
		{store.LaunchKindRestart, "stopped", "stopped", ""},
		{store.LaunchKindStart, "stopped", "stopped", ""},
		{store.LaunchKindStart, "error", "error", ""},
		{store.LaunchKindStart, "suspended", "error", "suspended"},
		{store.LaunchKindStart, "created", "error", "created"},
		{store.LaunchKindStart, "provisioning", "error", "provisioning"},
		{store.LaunchKindStart, "", "error", ""},
	}
	for _, how := range []string{"report_failed", "reaper_deadline", "reaper_lost"} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%s/prior=%s", how, tc.kind, tc.prior), func(t *testing.T) {
				s, projectID := newTestAgentStore(t)
				// A stale memory from an earlier failure: every failure end
				// rewrites it.
				a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "fpr", tc.kind, tc.prior, "created")
				quota, ldID := seedBrokerQuotaReservation(t, ctx, s, a)

				endStartLaunchByFailure(t, ctx, s, a, launchID, how)

				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				assert.Equal(t, tc.wantPhase, got.Phase, "end phase")
				assert.Equal(t, tc.wantMemory, got.LaunchResumePhase, "resume memory")
				assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
				assert.NotEmpty(t, got.LaunchError, "a failure end leaves a launch error")
				has, err := quota.HasActiveReservation(ctx, ldID, a.ID)
				require.NoError(t, err)
				assert.False(t, has, "a failure end releases the broker reservation")
			})
		}
	}
}

func TestStartLaunch_EndsClearMemory(t *testing.T) {
	ctx := context.Background()

	t.Run("succeeded clears it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-ok", store.LaunchKindStart, "suspended", "suspended")
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
		assert.Empty(t, got.LaunchResumePhase, "a successful start ends the memory in the same write")
	})

	t.Run("running_observed clears it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-ro", store.LaunchKindStart, "suspended", "")
		setRowPhaseAndMemory(t, ctx, s, a.ID, "running", "suspended")
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateProgress,
		})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
		assert.Empty(t, got.LaunchResumePhase)
	})

	t.Run("stopped during launch clears it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-stop", store.LaunchKindStart, "suspended", "")
		setRowPhaseAndMemory(t, ctx, s, a.ID, "stopped", "suspended")
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateProgress,
		})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchErrorLaunchStopped, got.LaunchError)
		assert.Empty(t, got.LaunchResumePhase)
	})

	t.Run("not_launched leaves it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-nl", store.LaunchKindStart, "suspended", "suspended")
		require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "suspended", got.LaunchResumePhase, "not_launched writes no phase and keeps the memory for the holder's rollback")
	})

	// Q3(a): another writer moved the row to error while the launch was
	// active; the launch's failed end applies the start-kind rule.
	for _, tc := range []struct {
		kind, prior           string
		wantPhase, wantMemory string
	}{
		{store.LaunchKindStart, "suspended", "error", "suspended"},
		{store.LaunchKindStart, "stopped", "stopped", ""},
		{store.LaunchKindRestart, "running", "stopped", ""},
	} {
		t.Run("active error end/"+tc.kind+"/prior="+tc.prior, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-err", tc.kind, tc.prior, "")
			setRowPhaseAndMemory(t, ctx, s, a.ID, "error", "created")
			_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
			})
			require.NoError(t, err)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantMemory, got.LaunchResumePhase)
		})
	}

	// Q3(a) with a non-failed report: the launch still ends failed, so the
	// start-kind rule applies (a restart ends stopped).
	t.Run("active error end/non-failed report/restart", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "end-err-nf", store.LaunchKindRestart, "running", "")
		setRowPhaseAndMemory(t, ctx, s, a.ID, "error", "")
		_, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateProgress,
		})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason)
		assert.Equal(t, "stopped", got.Phase)
	})

	// Q3(b): a wind-down reap writes no phase; it clears the memory unless
	// the row is in error, where the prior's memory applies.
	for _, tc := range []struct {
		kind, prior, phase string
		wantMemory         string
	}{
		{store.LaunchKindStart, "suspended", "stopped", ""},
		{store.LaunchKindStart, "suspended", "error", "suspended"},
		{store.LaunchKindRestart, "running", "error", ""},
	} {
		t.Run("wind-down/"+tc.kind+"/"+tc.phase, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, _ := beginStartKindLaunch(t, ctx, s, projectID, "end-wd", tc.kind, tc.prior, "")
			setRowPhaseAndMemory(t, ctx, s, a.ID, tc.phase, "created")
			_, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			setLaunchReaperArmedSince(t, ctx, s, readStoreNow(t, ctx, s).Add(-time.Hour))
			setAgentLastReportAt(t, ctx, s, a.ID, readStoreNow(t, ctx, s).Add(-time.Hour))
			res, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			require.Len(t, res.Reaped, 1)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.phase, got.Phase, "a wind-down reap leaves the phase alone")
			assert.Equal(t, tc.wantMemory, got.LaunchResumePhase)
		})
	}
}

// TestStartLaunch_SucceededClearsPriorExit pins that a start launch's
// succeeded report, the new run's first running write, clears the previous
// run's exit code and reason left on the starting row (ptone/scion#5384).
func TestStartLaunch_SucceededClearsPriorExit(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "start-clears-exit", store.LaunchKindStart, "stopped", "")
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetExitCode(137).SetExitReason("crashed").Save(ctx)
	require.NoError(t, err)

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase)
	assert.Nil(t, got.ExitCode, "the succeeded report clears the previous run's exit code")
	assert.Empty(t, got.ExitReason, "the succeeded report clears the previous run's exit reason")
}

// TestStartLaunch_ReportsOutsidePreRunningSucceededKeepExit pins the
// launch reports that leave the exit fields alone: a succeeded report on a
// row already running, and a failed report on a starting row.
func TestStartLaunch_ReportsOutsidePreRunningSucceededKeepExit(t *testing.T) {
	cases := []struct {
		name      string
		rowPhase  string
		state     string
		wantPhase string
	}{
		{"succeeded on a running row", "running", store.LaunchReportStateSucceeded, "running"},
		{"failed on a starting row", "starting", store.LaunchReportStateFailed, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "report-keeps-exit", store.LaunchKindStart, "starting", "")
			uid, err := parseUUID(a.ID)
			require.NoError(t, err)
			_, err = s.client.Agent.UpdateOneID(uid).SetPhase(tc.rowPhase).SetExitCode(137).SetExitReason("crashed").Save(ctx)
			require.NoError(t, err)

			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: tc.state,
				Step: "pod_create", Message: "boom", ErrorCode: "runtime_error",
			})
			require.NoError(t, err)
			require.Equal(t, store.LaunchReportResultApplied, answer.Result)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
			require.NotNil(t, got.ExitCode)
			assert.Equal(t, 137, *got.ExitCode)
			assert.Equal(t, "crashed", got.ExitReason)
		})
	}
}

// TestStartLaunch_LateFailedAfterStoppedReapRefines pins that a broker's
// failed report after the reaper ended a restart launch stopped still
// refines the launch error (the reaper's failure phase for a restart is
// stopped, not error).
func TestStartLaunch_LateFailedAfterStoppedReapRefines(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "late-refine", store.LaunchKindRestart, "running", "")
	endStartLaunchByFailure(t, ctx, s, a, launchID, "reaper_deadline")

	answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "image_pull_failed",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Equal(t, "image_pull_failed", got.LaunchError)
}

// TestStartLaunch_LateFailedAfterWindDownErrorRefines pins that a broker's
// failed report after a wind-down reap ended a start-kind launch on a row
// another writer had moved to error is applied (and refines the error), as
// it is for a create launch, even when the launch's own failure phase is
// stopped (a restart, or a start from stopped).
func TestStartLaunch_LateFailedAfterWindDownErrorRefines(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ kind, prior string }{
		{store.LaunchKindRestart, "running"},
		{store.LaunchKindStart, "stopped"},
	} {
		t.Run(tc.kind+"/prior="+tc.prior, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "late-wd", tc.kind, tc.prior, "")
			setRowPhaseAndMemory(t, ctx, s, a.ID, "error", "")
			_, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			setLaunchReaperArmedSince(t, ctx, s, readStoreNow(t, ctx, s).Add(-time.Hour))
			setAgentLastReportAt(t, ctx, s, a.ID, readStoreNow(t, ctx, s).Add(-time.Hour))
			res, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			require.Len(t, res.Reaped, 1)
			require.Equal(t, "error", res.Reaped[0].Phase, "a wind-down reap leaves the error phase")

			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "image_pull_failed",
			})
			require.NoError(t, err)
			assert.Equal(t, store.LaunchReportResultApplied, answer.Result, "the late failed report is applied, as for a create")
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, "image_pull_failed", got.LaunchError)
		})
	}
}

// TestStartLaunch_LateFailedAfterWindDownStoppedIsStale pins that a wind-down
// reap of a start-kind row the user stopped is not mistaken for the
// reaper's own failure end, even when the launch's failure phase is
// stopped (a restart, or a start from stopped): a late failed report gets
// 409 stale_launch, as it would for a create launch or before the reap.
func TestStartLaunch_LateFailedAfterWindDownStoppedIsStale(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ kind, prior string }{
		{store.LaunchKindRestart, "running"},
		{store.LaunchKindStart, "stopped"},
	} {
		t.Run(tc.kind+"/prior="+tc.prior, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "late-wd-stop", tc.kind, tc.prior, "")
			setRowPhaseAndMemory(t, ctx, s, a.ID, "stopped", "")
			_, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			setLaunchReaperArmedSince(t, ctx, s, readStoreNow(t, ctx, s).Add(-time.Hour))
			setAgentLastReportAt(t, ctx, s, a.ID, readStoreNow(t, ctx, s).Add(-time.Hour))
			res, err := s.RunLaunchReaperTick(ctx, wideWindowReaperParams)
			require.NoError(t, err)
			require.Len(t, res.Reaped, 1)
			require.Equal(t, "stopped", res.Reaped[0].Phase, "a wind-down reap leaves the stopped phase")

			answer, _, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "image_pull_failed",
			})
			require.NoError(t, err)
			assert.Equal(t, http.StatusConflict, answer.HTTPStatus, "a late failed report after a user stop is stale")
			assert.Equal(t, store.LaunchReportCodeStaleLaunch, answer.Code)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, store.LaunchErrorLaunchStopped, got.LaunchError, "the stop's launch error stands")
		})
	}
}

// TestStartLaunch_HaltedPriorRemembered pins the halted prior (ruling Q4,
// after ptone/scion#3643 added the phase): a failed start from halted ends
// error and remembers halted, for a failed report and for a reaper
// timeout, so EffectiveRestingPhase and launch.resumePhase show halted;
// a later halted write ends the memory.
func TestStartLaunch_HaltedPriorRemembered(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"report_failed", "reaper_deadline"} {
		t.Run(how, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginStartKindLaunch(t, ctx, s, projectID, "halted-prior", store.LaunchKindStart, "halted", "")
			endStartLaunchByFailure(t, ctx, s, a, launchID, how)

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, "error", got.Phase, "a failed start from halted ends error, so a phase-only wait ends")
			assert.Equal(t, "halted", got.LaunchResumePhase, "and remembers halted")
			assert.Equal(t, "halted", store.EffectiveRestingPhase(got))
			l := store.ComputeAgentLaunch(got, time.Now())
			require.NotNil(t, l)
			assert.Equal(t, "halted", l.ResumePhase)

			uid, err := parseUUID(a.ID)
			require.NoError(t, err)
			_, err = s.client.Agent.UpdateOneID(uid).SetPhase("halted").Save(ctx)
			require.NoError(t, err)
			got, err = s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Empty(t, got.LaunchResumePhase, "a halted write ends the memory")
		})
	}
}

// --- Resume-memory clearing hook (D-T12 e) ----------------------------------

func TestClearLaunchResumePhaseHook(t *testing.T) {
	ctx := context.Background()
	clears := map[string]bool{
		"running": true, "stopped": true, "stopping": true, "suspended": true,
		"created": true, "provisioning": true, "cloning": true,
		"halted": true, "starting": false, "error": false,
	}
	// write runs one phase write of agent uid through client, by UpdateOne
	// or by bulk Update.
	type writer func(ctx context.Context, c *ent.Client, a *store.Agent, phase string) error
	updateOne := func(ctx context.Context, c *ent.Client, a *store.Agent, phase string) error {
		uid, err := parseUUID(a.ID)
		if err != nil {
			return err
		}
		_, err = c.Agent.UpdateOneID(uid).SetPhase(phase).Save(ctx)
		return err
	}
	bulk := func(ctx context.Context, c *ent.Client, a *store.Agent, phase string) error {
		uid, err := parseUUID(a.ID)
		if err != nil {
			return err
		}
		_, err = c.Agent.Update().Where(entagent.IDEQ(uid)).SetPhase(phase).Save(ctx)
		return err
	}
	// via runs w through the store's own (entc-opened) client, or through
	// the launch writers' transaction client (newTxClient).
	type via func(t *testing.T, s *AgentStore, w func(c *ent.Client) error)
	storeClient := func(t *testing.T, s *AgentStore, w func(c *ent.Client) error) {
		require.NoError(t, w(s.client))
	}
	txClient := func(t *testing.T, s *AgentStore, w func(c *ent.Client) error) {
		ltx, err := s.beginLaunchTx(ctx)
		require.NoError(t, err)
		defer ltx.cleanup()
		if err := w(ltx.client); err != nil {
			_ = ltx.tx.Rollback()
			require.NoError(t, err)
		}
		require.NoError(t, ltx.tx.Commit())
	}

	for viaName, v := range map[string]via{"entc": storeClient, "newTxClient": txClient} {
		for wName, w := range map[string]writer{"UpdateOne": updateOne, "Update": bulk} {
			for phase, wantClear := range clears {
				t.Run(viaName+"/"+wName+"/"+phase, func(t *testing.T) {
					s, projectID := newTestAgentStore(t)
					a := createAgentInPhase(t, ctx, s, projectID, "hook", "error")
					setRowMemory(t, ctx, s, a.ID, "suspended")
					v(t, s, func(c *ent.Client) error { return w(ctx, c, a, phase) })
					got, err := s.GetAgent(ctx, a.ID)
					require.NoError(t, err)
					if wantClear {
						assert.Empty(t, got.LaunchResumePhase, "a %s write ends the memory", phase)
					} else {
						assert.Equal(t, "suspended", got.LaunchResumePhase, "a %s write keeps the memory", phase)
					}
				})
			}
		}
		t.Run(viaName+"/explicit set wins", func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := createAgentInPhase(t, ctx, s, projectID, "hook-explicit", "starting")
			uid, err := parseUUID(a.ID)
			require.NoError(t, err)
			v(t, s, func(c *ent.Client) error {
				_, err := c.Agent.UpdateOneID(uid).SetPhase("stopped").SetLaunchResumePhase("created").Save(ctx)
				return err
			})
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, "created", got.LaunchResumePhase)
		})
		t.Run(viaName+"/write without phase keeps it", func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := createAgentInPhase(t, ctx, s, projectID, "hook-nophase", "error")
			setRowMemory(t, ctx, s, a.ID, "suspended")
			uid, err := parseUUID(a.ID)
			require.NoError(t, err)
			v(t, s, func(c *ent.Client) error {
				_, err := c.Agent.UpdateOneID(uid).SetMessage("hello").Save(ctx)
				return err
			})
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, "suspended", got.LaunchResumePhase)
		})
	}

	t.Run("UpdateAgentStatus running write clears it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "hook-status", "error")
		setRowMemory(t, ctx, s, a.ID, "suspended")
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.LaunchResumePhase)
	})

	t.Run("UpdateAgent never writes it and its stopped write clears it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "hook-update", "error")
		setRowMemory(t, ctx, s, a.ID, "suspended")
		cur, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		cur.LaunchResumePhase = "provisioning" // ignored: UpdateAgent never writes it
		cur.Message = "touched"
		require.NoError(t, s.UpdateAgent(ctx, cur))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "suspended", got.LaunchResumePhase, "a whole-row write of phase error keeps the stored memory")

		got.Phase = "stopped"
		require.NoError(t, s.UpdateAgent(ctx, got))
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.LaunchResumePhase)
	})
}

// TestEntClientConstructorsRegisterResumeHook scans the non-test sources of
// pkg/ent/entc and pkg/store/entadapter for ent.NewClient calls and
// requires each to register ClearLaunchResumePhaseHook within the next few
// lines, so a new agent-writing client cannot skip the resume-memory rule.
// The schema-snapshot recorder in pkg/store/enttest is migration-only and
// out of scope.
func TestEntClientConstructorsRegisterResumeHook(t *testing.T) {
	dirs := []string{".", filepath.Join("..", "..", "ent", "entc")}
	found := 0
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			lines := strings.Split(string(raw), "\n")
			for i, line := range lines {
				if !strings.Contains(line, "ent.NewClient(") || strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				found++
				end := i + 4
				if end > len(lines) {
					end = len(lines)
				}
				registered := false
				for _, l := range lines[i:end] {
					code := strings.TrimSpace(l)
					if !strings.HasPrefix(code, "//") && strings.Contains(code, ".Use(") && strings.Contains(code, "ClearLaunchResumePhaseHook") {
						registered = true
					}
				}
				assert.True(t, registered,
					"%s:%d builds an *ent.Client without a .Use(... ClearLaunchResumePhaseHook ...) call in the next lines", f, i+1)
			}
		}
	}
	assert.GreaterOrEqual(t, found, 4, "the scan must see the three entc openers and newTxClient")
}

// --- Claim linking and settlement for user and restart claims (D-T2) --------

func TestStartClaim_LaunchEndSettlement_PairedKinds(t *testing.T) {
	ctx := context.Background()
	pairs := []struct {
		claim store.StartClaimKind
		kind  string
	}{
		{store.StartClaimUser, store.LaunchKindStart},
		{store.StartClaimRestart, store.LaunchKindRestart},
	}
	for _, p := range pairs {
		for _, reason := range []string{
			store.LaunchEndReasonSucceeded, store.LaunchEndReasonRunningObserved, store.LaunchEndReasonFailed,
			store.LaunchEndReasonTimedOut, store.LaunchEndReasonLost,
		} {
			t.Run(string(p.claim)+"/"+reason, func(t *testing.T) {
				s, projectID := newTestAgentStore(t)
				a := newClaimAgent(t, ctx, s, projectID, "paired")
				c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", p.claim, "", testClaimTTL)
				require.NoError(t, err)
				if p.kind == store.LaunchKindStart {
					setRowPhaseAndMemory(t, ctx, s, a.ID, "starting", "")
				}
				launchID, err := s.BeginStartLaunch(ctx, a.ID, p.kind, time.Hour, store.BeginLaunchOptions{PriorPhase: "stopped"})
				require.NoError(t, err)
				linked, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				require.Equal(t, launchID, linked.StartClaimLaunchID, "the paired claim is linked to the launch")

				require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, reason))
				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				release, demote := store.LaunchEndClaimSettlement(reason)
				switch {
				case release:
					assert.Empty(t, got.StartClaimID, "end reason %s releases the %s claim", reason, p.claim)
				case demote:
					assert.Equal(t, c.ID, got.StartClaimID)
					assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "end reason %s demotes", reason)
				default:
					t.Fatalf("reason %s settles nothing", reason)
				}
			})
		}

		t.Run(string(p.claim)+"/not_launched leaves it live", func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := newClaimAgent(t, ctx, s, projectID, "paired-nl")
			c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", p.claim, "", testClaimTTL)
			require.NoError(t, err)
			if p.kind == store.LaunchKindStart {
				setRowPhaseAndMemory(t, ctx, s, a.ID, "starting", "")
			}
			launchID, err := s.BeginStartLaunch(ctx, a.ID, p.kind, time.Hour, store.BeginLaunchOptions{PriorPhase: "stopped"})
			require.NoError(t, err)
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched))
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, c.ID, got.StartClaimID)
			assert.Equal(t, store.StartClaimLive, got.StartClaimState)
		})
	}

	// The backstop settles an ended start launch's user claim the launch's
	// end left alone (here: an end written outside the launch writers).
	t.Run("SettleEndedLaunchClaim settles a user claim", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := newClaimAgent(t, ctx, s, projectID, "paired-backstop")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
		require.NoError(t, err)
		setRowPhaseAndMemory(t, ctx, s, a.ID, "starting", "")
		_, err = s.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, time.Hour, store.BeginLaunchOptions{PriorPhase: "stopped"})
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
		mid, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, store.LaunchEndReasonRunningObserved, mid.LaunchEndReason)
		require.Equal(t, c.ID, mid.StartClaimID, "UpdateAgentStatus's running_observed end does not settle the claim")

		changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
		require.NoError(t, err)
		assert.True(t, changed)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.StartClaimID)
	})

	// Strict pairing: a claim of another kind is never adopted.
	for _, tc := range []struct {
		claim store.StartClaimKind
		kind  string
	}{
		{store.StartClaimWake, store.LaunchKindStart},
		{store.StartClaimRestart, store.LaunchKindStart},
		{store.StartClaimUser, store.LaunchKindRestart},
		{store.StartClaimRecovery, store.LaunchKindRestart},
	} {
		t.Run("unpaired/"+string(tc.claim)+"/"+tc.kind, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := newClaimAgent(t, ctx, s, projectID, "unpaired")
			c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", tc.claim, "", testClaimTTL)
			require.NoError(t, err)
			if tc.kind == store.LaunchKindStart {
				setRowPhaseAndMemory(t, ctx, s, a.ID, "starting", "")
			}
			launchID, err := s.BeginStartLaunch(ctx, a.ID, tc.kind, time.Hour, store.BeginLaunchOptions{PriorPhase: "stopped"})
			require.NoError(t, err)
			linked, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Empty(t, linked.StartClaimLaunchID, "a %s claim is not linked to a %s launch", tc.claim, tc.kind)

			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonFailed))
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, c.ID, got.StartClaimID, "the launch's end leaves an unpaired claim alone")
			assert.Equal(t, store.StartClaimLive, got.StartClaimState)
		})
	}
}

// TestStartKindHelpers_NilRow pins the defensive nil checks on the
// start-kind helpers (upstream review): a nil row is neither a start-kind
// launch nor a holder of a paired claim.
func TestStartKindHelpers_NilRow(t *testing.T) {
	got := true
	if assert.NotPanics(t, func() { got = isStartKindLaunch(nil) }, "isStartKindLaunch(nil)") {
		assert.False(t, got)
	}
	for _, kind := range []string{store.LaunchKindCreate, store.LaunchKindStart, store.LaunchKindRestart} {
		got = true
		if assert.NotPanics(t, func() { got = claimPairsWithLaunch(nil, kind) }, "claimPairsWithLaunch(nil, %s)", kind) {
			assert.False(t, got, kind)
		}
	}
}
