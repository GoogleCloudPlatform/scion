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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The delete fence (ptone/scion#2906) applies to every per-run delete the
// dispatch sends, including the previous runs' (ptone/scion#3097): each
// carries the engine's notAfter, and the broker refusing a late one as
// stale is not acted on.

// fenceRecordingClient records each delete's options and refuses the
// delete of staleRun with 409 stale_dispatch.
type fenceRecordingClient struct {
	*mockRuntimeBrokerClient
	staleRun string

	mu      sync.Mutex
	deletes []DeleteAgentOptions
}

func (c *fenceRecordingClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, opts)
	if c.staleRun != "" && opts.RunID == c.staleRun {
		return staleDispatchErr()
	}
	return nil
}

func (c *fenceRecordingClient) sent() []DeleteAgentOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DeleteAgentOptions(nil), c.deletes...)
}

func runsOf(opts []DeleteAgentOptions) []string {
	runs := make([]string, 0, len(opts))
	for _, o := range opts {
		runs = append(runs, o.RunID)
	}
	return runs
}

// The engine's delete of an agent with a previous run sends notAfter on
// the previous run's delete as on the current run's, and a late
// previous-run delete refused as stale abandons the claim rather than
// finalizing.
func TestDeleteFence_PreviousRunDeleteCarriesNotAfter(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "in time", true: "late previous run"}[late], func(t *testing.T) {
			t0 := fenceNow(t)
			srv, s := testServer(t)
			client := &fenceRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
			if late {
				client.staleRun = "run-1"
			}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-prev-"+map[bool]string{false: "ok", true: "late"}[late], state.PhaseRunning)
			ctx := context.Background()
			_, err := s.SetAgentRunID(ctx, agent.ID, "run-1", nil)
			require.NoError(t, err)
			_, err = s.SetAgentRunID(ctx, agent.ID, "run-2", nil)
			require.NoError(t, err)
			require.Equal(t, []string{"run-1"}, mustGetAgent(t, s, agent.ID).PreviousRunIDs)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			sent := client.sent()
			require.Equal(t, []string{"run-2", "run-1"}, runsOf(sent), "the current run, then the previous run")
			want := t0.Add(deleteLease - deleteNotAfterMargin)
			for _, o := range sent {
				assert.True(t, o.NotAfter.Equal(want), "run %s: notAfter = %v, want %v", o.RunID, o.NotAfter, want)
			}

			if !late {
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				return
			}
			require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())
			got := mustGetAgent(t, s, agent.ID)
			assert.True(t, got.DeletedAt.IsZero(), "the row was soft-deleted")
			assert.NotEqual(t, store.DeletionStateFinalizing, got.DeletionState, "the delete was finalized")
			view := store.ComputeAgentDeletion(got, time.Now())
			require.NotNil(t, view)
			assert.Equal(t, store.DeletionCodeAbandoned, view.Code, "the claim reads abandoned")
			assert.Contains(t, rec.Body.String(), "after its deadline", "a stale-specific message")
		})
	}
}

// The executing node fences an intent's previous-run deletes with the
// deadline it computes at send time; an intent whose claim is no longer
// current sends no delete at all, for any run.
func TestDeleteFence_ExecDeferredDeletePreviousRunsFenced(t *testing.T) {
	ctx := context.Background()
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current claim", true: "stale claim"}[stale], func(t *testing.T) {
			fenceNow(t)
			srv, s := testServer(t)
			client := &fenceRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-exec-prev", state.PhaseRunning)
			_, err := s.SetAgentRunID(ctx, agent.ID, "run-cur", nil)
			require.NoError(t, err)
			seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
			var claimAdj int64
			if stale {
				// A later claim took the row; the intent is the earlier one's.
				seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
				claimAdj = -1
			}
			row := mustGetAgent(t, s, agent.ID)
			args, err := MarshalDispatchArgs(&DeleteDispatchArgs{
				Claim:          row.DeletionClaim + claimAdj,
				PreviousRunIDs: []string{"run-old", "run-prev"},
			})
			require.NoError(t, err)

			_, execErr := srv.execDispatchDelete(ctx, store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: args})
			if stale {
				require.Error(t, execErr)
				assert.ErrorIs(t, execErr, errStaleDeleteDispatch)
				assert.Empty(t, client.sent(), "a stale intent reached the broker")
				return
			}
			require.NoError(t, execErr)
			sent := client.sent()
			require.Equal(t, []string{"run-cur", "run-prev", "run-old"}, runsOf(sent))
			want := row.DeletionLeaseAt.Add(-deleteNotAfterMargin)
			for _, o := range sent {
				assert.True(t, o.NotAfter.Equal(want), "run %s: notAfter = %v, want %v", o.RunID, o.NotAfter, want)
			}
		})
	}
}
