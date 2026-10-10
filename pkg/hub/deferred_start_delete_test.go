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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Queued start and restart intents drained for an agent that a delete holds,
// or that is soft-deleted, fail with delete_in_progress and dispatch nothing
// (ptone/scion#2882).

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
