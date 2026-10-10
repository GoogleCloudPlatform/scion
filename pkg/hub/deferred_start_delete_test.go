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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Queued start and restart intents drained for an agent that a delete holds,
// or that is soft-deleted, fail and dispatch nothing (ptone/scion#2882). The
// requester answers what the synchronous start gate answers: 409
// delete_in_progress while a delete holds the row, else 409 conflict "agent
// is deleted; restore it first" for a soft-deleted row (ptone/scion#4182).

// queuedStartCountingDispatcher counts start and restart dispatches.
type queuedStartCountingDispatcher struct {
	createAgentDispatcher
	starts   int
	restarts int
}

func (d *queuedStartCountingDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.starts++
	return nil
}

func (d *queuedStartCountingDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	d.restarts++
	return nil
}

// queuedStartIntent inserts a pending start or restart intent for agent and
// returns the row as the drain would hand it to executeDispatch.
func queuedStartIntent(t *testing.T, s store.Store, agent *store.Agent, op string) store.BrokerDispatch {
	t.Helper()
	d := store.BrokerDispatch{
		ID: uuid.NewString(), BrokerID: agent.RuntimeBrokerID, AgentID: agent.ID, Op: op,
	}
	require.NoError(t, s.InsertBrokerDispatch(context.Background(), &d))
	return d
}

func softDeleteAgentRow(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	row, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	row.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, row))
}

func TestExecDispatchStartRestart_RefusedWhileDeleting(t *testing.T) {
	type refusalCase struct {
		name string
		mark func(t *testing.T, s store.Store, agentID string)
	}
	// Every marker in blockingSeeds, so a new blocking marker is covered
	// here too, plus a soft-deleted row.
	var cases []refusalCase
	for _, seed := range blockingSeeds {
		cases = append(cases, refusalCase{seed.name, func(t *testing.T, s store.Store, id string) {
			seedAgentDeletion(t, s, id, seed)
		}})
	}
	cases = append(cases, refusalCase{"soft-deleted", softDeleteAgentRow})
	require.Len(t, cases, 6)
	for i, tc := range cases {
		for _, op := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				ctx := context.Background()
				srv, s := testServer(t)
				disp := &queuedStartCountingDispatcher{}
				srv.SetDispatcher(disp)
				agent := setupBrokerAgentInPhase(t, s, "qsd-"+op+"-"+string(rune('a'+i)), state.PhaseStopped)
				d := queuedStartIntent(t, s, agent, op)
				tc.mark(t, s, agent.ID)

				_, err := srv.executeDispatch(ctx, d)
				require.Error(t, err)
				assert.ErrorIs(t, err, store.ErrDeleteInProgress)
				assert.Zero(t, disp.starts, "no start dispatched")
				assert.Zero(t, disp.restarts, "no restart dispatched")

				// The failed row carries the delete_in_progress sentinel, so
				// the requester rebuilds the same error and answers 409
				// delete_in_progress.
				result := dispatchFailureResult(err)
				assert.Contains(t, result, ErrCodeDeleteInProgress)
				rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: op, Error: err.Error(), Result: result})
				assert.True(t, errors.Is(rebuilt, store.ErrDeleteInProgress), "rebuilt error: %v", rebuilt)

				got, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseStopped), got.Phase, "phase unchanged")
			})
		}
	}
}

// A queued start or restart for a live agent, or one whose delete failed
// with no intent outstanding, is still dispatched.
func TestExecDispatchStartRestart_DispatchesWithoutDelete(t *testing.T) {
	cases := []struct {
		name string
		mark func(t *testing.T, s store.Store, agentID string)
	}{
		{"live", func(*testing.T, store.Store, string) {}},
		{"failed delete without intent", func(t *testing.T, s store.Store, id string) {
			seedAgentDeletion(t, s, id, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})
		}},
	}
	for i, tc := range cases {
		for _, op := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				srv, s := testServer(t)
				disp := &queuedStartCountingDispatcher{}
				srv.SetDispatcher(disp)
				agent := setupBrokerAgentInPhase(t, s, "qsl-"+op+"-"+string(rune('a'+i)), state.PhaseStopped)
				d := queuedStartIntent(t, s, agent, op)
				tc.mark(t, s, agent.ID)

				_, err := srv.executeDispatch(context.Background(), d)
				require.NoError(t, err)
				if op == "start" {
					assert.Equal(t, 1, disp.starts)
				} else {
					assert.Equal(t, 1, disp.restarts)
				}
			})
		}
	}
}

// queuedRefusalAnswer runs a queued op intent for agent through the drain,
// records the failure on the dispatch row as the executing node does,
// rebuilds the error as the requesting node does and returns the answer the
// requester writes for it.
func queuedRefusalAnswer(t *testing.T, srv *Server, s store.Store, agent *store.Agent, d store.BrokerDispatch) (*httptest.ResponseRecorder, ErrorResponse) {
	t.Helper()
	_, err := srv.executeDispatch(context.Background(), d)
	require.Error(t, err)
	rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: d.Op, Error: err.Error(), Result: dispatchFailureResult(err)})
	rec := httptest.NewRecorder()
	require.True(t, srv.writeStartClaimError(context.Background(), rec, rebuilt, agent.ID), "rebuilt error not answered: %v", rebuilt)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec, body
}

// A queued start or restart of a soft-deleted agent gets the synchronous
// start gate's answer (agentDeletedRefusal), not delete_in_progress
// (ptone/scion#4182). A row a delete holds still answers
// delete_in_progress, also when it is soft-deleted too (finalizing), as
// the gate does: its delete check comes first.
func TestExecDispatchStartRestart_SoftDeletedAnswersLikeStartGate(t *testing.T) {
	want := agentDeletedRefusal("")
	cases := []struct {
		name     string
		mark     func(t *testing.T, s store.Store, agentID string)
		wantCode string
		wantMsg  string
	}{
		{"soft-deleted", softDeleteAgentRow, want.Code, want.Message},
		{"deleting", func(t *testing.T, s store.Store, id string) {
			seedAgentDeletion(t, s, id, seedLiveDeleting)
		}, ErrCodeDeleteInProgress, deleteInProgressRefusal("").Message},
		{"soft-deleted and finalizing", func(t *testing.T, s store.Store, id string) {
			softDeleteAgentRow(t, s, id)
			seedAgentDeletion(t, s, id, seedExpiredFinalize)
		}, ErrCodeDeleteInProgress, deleteInProgressRefusal("").Message},
	}
	for i, tc := range cases {
		for _, op := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				srv, s := testServer(t)
				disp := &queuedStartCountingDispatcher{}
				srv.SetDispatcher(disp)
				agent := setupBrokerAgentInPhase(t, s, "qsg-"+op+"-"+string(rune('a'+i)), state.PhaseStopped)
				d := queuedStartIntent(t, s, agent, op)
				tc.mark(t, s, agent.ID)

				rec, body := queuedRefusalAnswer(t, srv, s, agent, d)
				assert.Equal(t, http.StatusConflict, rec.Code)
				assert.Equal(t, tc.wantCode, body.Error.Code)
				assert.Equal(t, tc.wantMsg, body.Error.Message)
				assert.Equal(t, agent.ID, body.Error.Details["agentId"])
				assert.Zero(t, disp.starts+disp.restarts, "nothing dispatched")
			})
		}
	}
}

// The agent-deleted envelope field round-trips on its own and alongside the
// hub sentinels, and an envelope without it rebuilds nothing new.
func TestDispatchFailureResult_AgentDeleted(t *testing.T) {
	execErr := fmt.Errorf("queued start not applied: %w: %w", errQueuedStartAgentDeleted, store.ErrDeleteInProgress)
	result := dispatchFailureResult(execErr)
	env := decodeDispatchFailure(result)
	require.NotNil(t, env)
	assert.True(t, env.AgentDeleted)
	assert.Equal(t, []string{"delete_in_progress"}, env.HubErrors)
	rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: "start", Error: execErr.Error(), Result: result})
	assert.ErrorIs(t, rebuilt, errQueuedStartAgentDeleted)
	assert.ErrorIs(t, rebuilt, store.ErrDeleteInProgress)

	alone := dispatchFailureResult(errQueuedStartAgentDeleted)
	require.NotEmpty(t, alone, "the field alone makes the envelope non-empty")
	assert.ErrorIs(t, dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: alone}), errQueuedStartAgentDeleted)

	held := dispatchFailureResult(fmt.Errorf("x: %w", store.ErrDeleteInProgress))
	assert.NotContains(t, held, "agentDeleted")
	assert.False(t, errors.Is(dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: held}), errQueuedStartAgentDeleted))
}
