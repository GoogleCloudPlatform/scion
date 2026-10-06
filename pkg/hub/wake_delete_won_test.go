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
	st := store.DeletionStateFinalizing
	at := time.Now().Add(-time.Minute)
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
	assert.Equal(t, agentID, dmErr.Details["agentId"], "the same details as the other delete_in_progress answers")
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
			finalizeExpired(t, u.s, id, "")
			// The new container's first report: the lease-aware guard does
			// not hold an expired finalizing row, so it lands.
			assert.NoError(t, u.s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{Activity: "idle"}))
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
