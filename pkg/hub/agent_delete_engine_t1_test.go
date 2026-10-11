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

package hub

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Delete engine × T1 async-create interleaving (design ptone/scion#2483
// §2.3 C1/C2, acceptance (aa), (bb), (cc)), plus P0 review N3.

// launchingAgent creates a broker agent in phase with an active create
// launch whose deadline is timeout away.
func launchingAgent(t *testing.T, s store.Store, suffix string, phase state.Phase, timeout time.Duration) (*store.Agent, string) {
	t.Helper()
	agent := setupBrokerAgentInPhase(t, s, suffix, phase)
	launchID, err := s.BeginLaunch(context.Background(), agent.ID, store.LaunchKindCreate, timeout)
	require.NoError(t, err)
	return mustGetAgent(t, s, agent.ID), launchID
}

func launchReport(t *testing.T, s store.Store, a *store.Agent, launchID, reportState string, seq int64) store.LaunchReportAnswer {
	t.Helper()
	ans, _, err := s.ApplyLaunchReport(context.Background(), a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "inst-1", Seq: seq, State: reportState,
		Phase: string(state.PhaseStarting), Step: "pulling",
	})
	require.NoError(t, err)
	return ans
}

// Acceptance (aa)(i), (ii): a claim on a created/provisioning row with an
// active launch moves it to stopping and dispatches a broker delete. A
// later launch report gets 409 stopped, ends the launch launch_stopped, and
// never moves the phase. The delete then completes (an incomplete create is
// hard-deleted).
func TestAgentDeleteEngine_T1_ReportDuringDeleteIsStopped(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseProvisioning} {
		for _, report := range []string{store.LaunchReportStateProgress, store.LaunchReportStateSucceeded} {
			t.Run(string(phase)+"/"+report, func(t *testing.T) {
				srv, s, pub, disp := engineTestServer(t)
				entered, release := make(chan struct{}), make(chan struct{})
				disp.setFn(blockingDelete(entered, release, nil))
				agent, launchID := launchingAgent(t, s, "t1rep-"+string(phase)[:4]+report[:4], phase, 5*time.Minute)

				ch := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
				waitClosed(t, entered, 5*time.Second, "broker delete dispatch")

				got := mustGetAgent(t, s, agent.ID)
				assert.Equal(t, string(state.PhaseStopping), got.Phase, "C1: claim sets stopping")
				evs := pub.snapshot()
				require.NotEmpty(t, evs)
				assert.Equal(t, string(state.PhaseStopping), evs[0].phase)

				ans := launchReport(t, s, agent, launchID, report, 1)
				assert.Equal(t, http.StatusConflict, ans.HTTPStatus)
				assert.Equal(t, store.LaunchReportReasonStopped, ans.Reason)
				got = mustGetAgent(t, s, agent.ID)
				assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
				assert.Equal(t, store.LaunchErrorLaunchStopped, got.LaunchError)
				assert.Equal(t, string(state.PhaseStopping), got.Phase, "the report never moves the phase")
				assert.Equal(t, store.DeletionStateDeleting, got.DeletionState)

				close(release)
				r := waitDelete(t, ch, 10*time.Second)
				require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
				assert.Equal(t, 1, disp.callCount())
				assert.True(t, agentGone(t, s, agent.ID), "(bb): an incomplete create is hard-deleted")
			})
		}
	}
}

// Acceptance (aa)(i), reaper half: a reaper tick past the launch deadline
// while the delete holds the row in stopping does not set error.
func TestAgentDeleteEngine_T1_ReaperDuringDeleteDoesNotSetError(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseProvisioning} {
		t.Run(string(phase), func(t *testing.T) {
			srv, s, _, disp := engineTestServer(t)
			entered, release := make(chan struct{}), make(chan struct{})
			disp.setFn(blockingDelete(entered, release, nil))
			agent, _ := launchingAgent(t, s, "t1reap-"+string(phase)[:4], phase, 10*time.Millisecond)

			ch := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
			waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
			time.Sleep(30 * time.Millisecond) // past the launch deadline

			_, err := s.RunLaunchReaperTick(context.Background(), store.ReaperParams{
				KeepaliveInterval: 15 * time.Second, ReaperInterval: 15 * time.Second,
			})
			require.NoError(t, err)
			got := mustGetAgent(t, s, agent.ID)
			assert.NotEqual(t, string(state.PhaseError), got.Phase)
			assert.Equal(t, string(state.PhaseStopping), got.Phase)
			assert.Equal(t, store.DeletionStateDeleting, got.DeletionState)

			close(release)
			r := waitDelete(t, ch, 10*time.Second)
			require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			assert.True(t, agentGone(t, s, agent.ID))
		})
	}
}

// Acceptance (aa)(iii): the launch ends during the delete, then the dispatch
// fails → rollback restores stopped (not the prior provisioning), and the
// row is an incomplete create. (bb): a retry with retention on hard-deletes
// it, and the name can be reused.
func TestAgentDeleteEngine_T1_LaunchEndedThenDispatchFails(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	entered, release := make(chan struct{}), make(chan struct{})
	disp.setFn(blockingDelete(entered, release, errors.New("broker boom")))
	agent, launchID := launchingAgent(t, s, "t1iii", state.PhaseProvisioning, 5*time.Minute)

	ch := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
	waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
	ans := launchReport(t, s, agent, launchID, store.LaunchReportStateProgress, 1)
	require.Equal(t, http.StatusConflict, ans.HTTPStatus)

	close(release)
	r := waitDelete(t, ch, 10*time.Second)
	require.Equal(t, http.StatusBadGateway, r.rec.Code, r.rec.Body.String())
	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "C2: the launch ended, so stopped, not provisioning")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.True(t, got.IsIncompleteCreate())

	// (bb): retry with retention on → hard delete; the name is free again.
	disp.setFn(nil)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, agent.ID))
	_, err := s.GetAgentBySlug(context.Background(), agent.ProjectID, agent.Slug)
	assert.ErrorIs(t, err, store.ErrNotFound)
	reuse := &store.Agent{
		ID: tid("t1iii-reuse"), Slug: agent.Slug, Name: agent.Name, ProjectID: agent.ProjectID,
		RuntimeBrokerID: agent.RuntimeBrokerID, Phase: string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(context.Background(), reuse), "create with the same name succeeds")
}

// Acceptance (aa)(iv): the dispatch fails while the launch is still active →
// rollback restores the prior phase and the launch continues.
func TestAgentDeleteEngine_T1_DispatchFailsLaunchActive(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseProvisioning} {
		t.Run(string(phase), func(t *testing.T) {
			srv, s, _, disp := engineTestServer(t)
			disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
			agent, launchID := launchingAgent(t, s, "t1iv-"+string(phase)[:4], phase, 5*time.Minute)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			got := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(phase), got.Phase, "prior phase restored")
			assert.Equal(t, store.LaunchStateActive, got.LaunchState)
			assert.Equal(t, launchID, got.LaunchID)

			// The launch continues: a report is applied.
			ans := launchReport(t, s, got, launchID, store.LaunchReportStateProgress, 1)
			assert.Zero(t, ans.HTTPStatus, "report accepted after rollback")
		})
	}
}

// Acceptance (bb): with retention on, an ordinary agent is still
// soft-deleted, and force still hard-deletes.
func TestAgentDeleteEngine_T1_OrdinaryAgentSoftForceHard(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour

	ordinary := setupBrokerAgentInPhase(t, s, "bbsoft", state.PhaseRunning)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+ordinary.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got := mustGetAgent(t, s, ordinary.ID)
	assert.False(t, got.DeletedAt.IsZero(), "soft-deleted")

	forced := setupBrokerAgentInPhase(t, s, "bbforce", state.PhaseRunning)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+forced.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, forced.ID))
}

// Acceptance (cc): a row that is both mid-delete and an incomplete create
// gets 409 delete_in_progress from start (the delete gate runs first).
func TestAgentDeleteEngine_T1_StartGateOrder(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	disp.setFn(blockingDelete(entered, release, nil))
	agent, launchID := launchingAgent(t, s, "t1cc", state.PhaseProvisioning, 5*time.Minute)

	ch := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
	waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
	launchReport(t, s, agent, launchID, store.LaunchReportStateProgress, 1)
	got := mustGetAgent(t, s, agent.ID)
	require.True(t, got.IsIncompleteCreate())
	require.Equal(t, store.DeletionStateDeleting, got.DeletionState)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	requireDeleteInProgress(t, rec)

	close(release)
	r := waitDelete(t, ch, 10*time.Second)
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
}

// P0 review N3 (EM-approved ptone/scion#2635 interpretation): a dispatch
// succeeded but the follow-up write was lost to a ctx cancel, leaving the
// row created with no launch. A later delete must still dispatch to the
// broker (best-effort, when reachable) so the container is not orphaned,
// and the event sequence keeps phase created (acceptance (a)).
func TestAgentDeleteEngine_CreatedRowAfterLostWriteStillDispatches(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "p0n3", state.PhaseCreated)

	// The create dispatch succeeded but its request ctx was cancelled
	// before updateAgentAfterDispatch could record it.
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, disp.DispatchAgentStart(ctx, agent, "", false))
	cancel()
	require.Error(t, srv.updateAgentAfterDispatch(ctx, agent))
	require.Equal(t, string(state.PhaseCreated), mustGetAgent(t, s, agent.ID).Phase)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 1, disp.callCount(), "the broker delete was dispatched")

	evs := pub.snapshot()
	require.Len(t, evs, 2)
	assert.Equal(t, "status", evs[0].kind)
	assert.Equal(t, string(state.PhaseCreated), evs[0].phase, "no spurious phase change")
	require.NotNil(t, evs[0].deletion)
	assert.Equal(t, store.DeletionStateDeleting, evs[0].deletion.State)
	assert.Equal(t, "deleted", evs[1].kind)
}
