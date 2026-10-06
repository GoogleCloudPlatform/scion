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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DM wake whose resume lands after a delete won answers 409
// delete_in_progress and does not deliver, and its post-start writes keep
// the delete's phase, a finalizing row with an expired lease included
// (ptone/scion#3528). The store neutralises a refused start write and
// returns nil (StartWrite, GoogleCloudPlatform/scion#2679), so the wake
// checks for the delete itself.

// wakeStartHookDispatcher is quotaLifecycleDispatcher whose start runs hook
// once the broker start has "landed".
type wakeStartHookDispatcher struct {
	quotaLifecycleDispatcher
	hook func(agentID string)
}

func (d *wakeStartHookDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	err := d.quotaLifecycleDispatcher.DispatchAgentStart(ctx, a, task, resume)
	if d.hook != nil {
		d.hook(a.ID)
	}
	return err
}

// finalizeExpired puts the row in a finalizing delete whose lease expired:
// teardown ran, the delete holds the row until a retry or force, and the
// lease-aware guard on status reports no longer sees it. phase is the
// phase the delete's claim left (empty: unchanged).
func finalizeExpired(t *testing.T, s store.Store, id, phase string) {
	t.Helper()
	claimWakeRow(t, s, id, store.DeletionStateFinalizing, -time.Minute, phase)
}

// claimWakeRow writes a delete marker on the row: state, a lease that ends
// lease from now, and the phase the claim left (empty: unchanged; a real
// claim on a starting row writes stopping).
func claimWakeRow(t *testing.T, s store.Store, id, st string, lease time.Duration, phase string) {
	t.Helper()
	at := time.Now().Add(lease)
	fields := store.DeletionFields{State: &st, LeaseAt: &at, BumpClaim: true}
	if phase != "" {
		fields.Phase = &phase
	}
	n, err := s.UpdateAgentDeletion(context.Background(), id,
		store.DeletionPredicate{States: []string{""}, DeletedAtNull: true}, fields)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func requireWakeDeleteWon(t *testing.T, res *WakeResult, dmErr *AgentDMError, agentID string) {
	t.Helper()
	require.Nil(t, res, "no wake result: the message must not be delivered")
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus, dmErr.Message)
	assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
	assert.Equal(t, deletedWhileStartingMessage, dmErr.Message, "the message a start or restart that lost after landing answers")
	assert.Equal(t, agentID, dmErr.Details["agentId"], "the same details as the other delete_in_progress answers")
}

// reportReady posts the new container's first report (activity) after
// delay, as its heartbeat would.
func reportReady(t *testing.T, s store.Store, id string, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		assert.NoError(t, s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{Activity: "idle"}))
	}()
}

// The delete claims (and reaches finalizing, its lease since expired) while
// the resume is in flight: the starting re-assert keeps the delete's phase,
// and the wake answers 409 at once, without waiting for readiness.
func TestWake_DeleteWonDuringResume_Answers409(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-resume", 5)
	disp := &wakeStartHookDispatcher{hook: func(id string) {
		finalizeExpired(t, u.s, id, string(state.PhaseStopping))
	}}
	u.srv.SetDispatcher(disp)

	began := time.Now()
	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	requireWakeDeleteWon(t, res, dmErr, u.target.ID)
	assert.Less(t, time.Since(began), 2*time.Second, "no readiness wait")
	row := mustGetAgent(t, u.s, u.target.ID)
	assert.Equal(t, string(state.PhaseStopping), row.Phase, "the starting re-assert keeps the delete's phase")
	assert.Equal(t, store.DeletionStateFinalizing, row.DeletionState)
}

// The delete wins during the readiness wait: the agent reports activity
// (readiness), the running write keeps the delete's phase, and the wake
// answers 409 rather than WakeResumed.
func TestWake_DeleteWonDuringReadiness_Answers409(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-ready", 5)
	disp := &wakeStartHookDispatcher{hook: func(id string) {
		go func() {
			// After the starting re-assert and the first delete-won check,
			// before the first readiness poll (500ms).
			time.Sleep(150 * time.Millisecond)
			// The claim writes stopping, as a real claim on a starting row
			// does; the lease has since expired.
			finalizeExpired(t, u.s, id, string(state.PhaseStopping))
			// The new container's heartbeat repaints the phase and reports
			// activity: the lease-aware guard on status reports does not
			// hold an expired finalizing row, so both land and the agent
			// looks ready.
			assert.NoError(t, u.s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{Phase: string(state.PhaseStarting), Activity: "idle"}))
		}()
	}}
	u.srv.SetDispatcher(disp)

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	requireWakeDeleteWon(t, res, dmErr, u.target.ID)
	row := mustGetAgent(t, u.s, u.target.ID)
	assert.Equal(t, string(state.PhaseStarting), row.Phase, "the running write keeps the delete's phase")
	assert.Equal(t, store.DeletionStateFinalizing, row.DeletionState)
}

// Control: with no delete the wake resumes and the agent runs.
func TestWake_NoDelete_Resumes(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-none", 5)
	disp := &wakeStartHookDispatcher{hook: func(id string) {
		go func() {
			time.Sleep(150 * time.Millisecond)
			assert.NoError(t, u.s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{Activity: "idle"}))
		}()
	}}
	u.srv.SetDispatcher(disp)

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, WakeResumed, res.Outcome)
	assert.Equal(t, string(state.PhaseRunning), mustGetAgent(t, u.s, u.target.ID).Phase)
}

// A live delete claim during the readiness wait (deleting, a live lease,
// phase stopping, as the claim writes it) ends the wait on the unexpected
// phase. That is the delete's answer: 409, not a 502 runtime error, and no
// readiness-failure message is written to the row the delete holds.
func TestWake_LiveClaimDuringReadiness_Answers409(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-live", 5)
	u.srv.SetDispatcher(&wakeStartHookDispatcher{hook: func(id string) {
		go func() {
			time.Sleep(150 * time.Millisecond)
			claimWakeRow(t, u.s, id, store.DeletionStateDeleting, time.Minute, string(state.PhaseStopping))
		}()
	}})

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	requireWakeDeleteWon(t, res, dmErr, u.target.ID)
	row := mustGetAgent(t, u.s, u.target.ID)
	assert.Equal(t, string(state.PhaseStopping), row.Phase)
	assert.NotEqual(t, "Failed to become ready after wake", row.Message, "the delete's row is left alone")
}

// The row is deleted during the readiness wait: 409, not a 502.
func TestWake_RowDeletedDuringReadiness_Answers409(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-gone", 5)
	u.srv.SetDispatcher(&wakeStartHookDispatcher{hook: func(id string) {
		go func() {
			time.Sleep(150 * time.Millisecond)
			assert.NoError(t, u.s.DeleteAgent(context.Background(), id))
		}()
	}})

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	requireWakeDeleteWon(t, res, dmErr, u.target.ID)
}

// A failed delete leaves the agent live: the wake resumes it.
func TestWake_FailedDelete_StillWakes(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-failed", 5)
	st, code := store.DeletionStateFailed, store.DeletionCodeRuntimeError
	failedAt := time.Now()
	n, err := u.s.UpdateAgentDeletion(context.Background(), u.target.ID,
		store.DeletionPredicate{States: []string{""}, DeletedAtNull: true},
		store.DeletionFields{State: &st, FailedAt: &failedAt, Code: &code, BumpClaim: true})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	target := mustGetAgent(t, u.s, u.target.ID)
	u.srv.SetDispatcher(&wakeStartHookDispatcher{hook: func(id string) {
		reportReady(t, u.s, id, 150*time.Millisecond)
	}})

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), target)
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, WakeResumed, res.Outcome)
	assert.Equal(t, string(state.PhaseRunning), mustGetAgent(t, u.s, u.target.ID).Phase)
}

// A deleting row whose lease lapsed (the engine died) reads as failed: the
// agent is live, and the wake resumes it.
func TestWake_LapsedDelete_StillWakes(t *testing.T) {
	u := newWakeQuotaFixture(t, "wake-dw-lapsed", 5)
	u.srv.SetDispatcher(&wakeStartHookDispatcher{hook: func(id string) {
		claimWakeRow(t, u.s, id, store.DeletionStateDeleting, -time.Minute, "")
		reportReady(t, u.s, id, 150*time.Millisecond)
	}})

	res, dmErr := u.srv.wakeAgentForDM(context.Background(), u.target)
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, WakeResumed, res.Outcome)
	assert.Equal(t, string(state.PhaseRunning), mustGetAgent(t, u.s, u.target.ID).Phase)
}
