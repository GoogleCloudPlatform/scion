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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3080 (option A): when the broker refuses a
// run-scoped delete because a different run holds the agent's name, the
// hub does not finalize the row. A plain 404, or a run mismatch naming no
// current run or the same run, stays an idempotent success.

// runMismatchEnvelope is the broker's run-mismatch 404 for a delete of
// requested; current "" omits currentRunId; details false omits all details
// (as an older broker might).
func runMismatchEnvelope(t *testing.T, requested, current string, details bool) *brokerStatusError {
	t.Helper()
	if !details {
		return brokerEnvelope(t, http.StatusNotFound, api.BrokerErrorCodeRunMismatch, nil)
	}
	d := map[string]interface{}{api.BrokerErrorDetailRunID: requested}
	if current != "" {
		d[api.BrokerErrorDetailCurrentRunID] = current
	}
	return brokerEnvelope(t, http.StatusNotFound, api.BrokerErrorCodeRunMismatch, d)
}

// deleteTransportCases are the broker answers to a delete of run-1, and
// whether each must surface as the refusal.
func deleteTransportCases(t *testing.T) []struct {
	name    string
	status  int
	body    string
	runID   string
	refused bool
	isErr   bool
} {
	mismatch := runMismatchEnvelope(t, "run-1", "run-2", true).Body
	return []struct {
		name    string
		status  int
		body    string
		runID   string
		refused bool
		isErr   bool
	}{
		{"different current run refuses", http.StatusNotFound, mismatch, "run-1", true, true},
		{"same current run is not found", http.StatusNotFound, runMismatchEnvelope(t, "run-1", "run-1", true).Body, "run-1", false, false},
		{"no current run is not found", http.StatusNotFound, runMismatchEnvelope(t, "run-1", "", true).Body, "run-1", false, false},
		{"no details is not found", http.StatusNotFound, runMismatchEnvelope(t, "run-1", "", false).Body, "run-1", false, false},
		{"plain agent_not_found", http.StatusNotFound, `{"error":{"code":"agent_not_found","message":"Agent not found"}}`, "run-1", false, false},
		{"non-JSON 404 (a proxy)", http.StatusNotFound, "not found", "run-1", false, false},
		{"delete naming no run", http.StatusNotFound, mismatch, "", false, false},
		{"other error", http.StatusInternalServerError, `{"error":{"code":"runtime_error","message":"boom"}}`, "run-1", false, true},
		{"success", http.StatusNoContent, "", "run-1", false, false},
	}
}

func assertDeleteTransportResult(t *testing.T, err error, refused, isErr bool) {
	t.Helper()
	if !isErr {
		assert.NoError(t, err)
		return
	}
	require.Error(t, err)
	assert.Equal(t, refused, errors.Is(err, ErrDeleteRunMismatch), "err = %v", err)
	if refused {
		var r *DeleteRunMismatchError
		require.True(t, errors.As(err, &r), "err = %v", err)
		assert.Equal(t, "run-1", r.RequestedRunID)
		assert.Equal(t, "run-2", r.CurrentRunID)
		assert.True(t, isBrokerStatus(err, http.StatusNotFound), "the refusal still unwraps to the broker's 404")
	}
}

func TestHTTPRuntimeBrokerClient_DeleteRunMismatch(t *testing.T) {
	for _, tc := range deleteTransportCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := NewHTTPRuntimeBrokerClient().DeleteAgent(context.Background(), tid("host-1"), server.URL, "a", "p1",
				DeleteAgentOptions{RunID: tc.runID})
			assertDeleteTransportResult(t, err, tc.refused, tc.isErr)
		})
	}
}

func TestControlChannelBrokerClient_DeleteRunMismatch(t *testing.T) {
	for _, tc := range deleteTransportCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			tunnel := &mockControlChannelTunnel{connected: true, status: tc.status, body: []byte(tc.body)}
			client := &ControlChannelBrokerClient{manager: tunnel}
			err := client.DeleteAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1",
				DeleteAgentOptions{RunID: tc.runID})
			assertDeleteTransportResult(t, err, tc.refused, tc.isErr)
		})
	}
}

// runMismatchDeleteClient answers each delete with answer(runID), a
// broker error before the transport's handling, which it then applies
// (deleteAgentError), as both transports do.
type runMismatchDeleteClient struct {
	*mockRuntimeBrokerClient
	mu     sync.Mutex
	answer func(runID string) error
	runs   []string
}

func (c *runMismatchDeleteClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	c.runs = append(c.runs, opts.RunID)
	answer := c.answer
	c.mu.Unlock()
	return deleteAgentError(answer(opts.RunID), opts.RunID)
}

func (c *runMismatchDeleteClient) deleted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.runs...)
}

type runMismatchFixture struct {
	srv    *Server
	store  store.Store
	client *runMismatchDeleteClient
	agent  *store.Agent
}

// newRunMismatchFixture is an agent in phase on a broker answering through
// client, with the row's run run-a and, when prev is set, the previous
// run run-p (ptone/scion#3097).
func newRunMismatchFixture(t *testing.T, suffix string, phase state.Phase, prev bool) *runMismatchFixture {
	t.Helper()
	srv, s := testServer(t)
	client := &runMismatchDeleteClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, answer: func(string) error { return nil }}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, suffix, phase)
	ctx := context.Background()
	if prev {
		_, err := s.SetAgentRunID(ctx, agent.ID, "run-p")
		require.NoError(t, err)
	}
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-a")
	require.NoError(t, err)
	got := mustGetAgent(t, s, agent.ID)
	require.Equal(t, "run-a", got.RunID)
	if prev {
		require.Equal(t, []string{"run-p"}, got.PreviousRunIDs)
	}
	return &runMismatchFixture{srv: srv, store: s, client: client, agent: got}
}

func (f *runMismatchFixture) del(t *testing.T, query string) deleteResult {
	t.Helper()
	return waitDelete(t, deleteAsync(t, f.srv, "/api/v1/agents/"+f.agent.ID+query, nil), 10*time.Second)
}

// requireNotFinalized checks a refused delete: 409 conflict naming both
// runs, the row kept, failed with code conflict, and phase restored.
func requireNotFinalized(t *testing.T, s store.Store, agentID string, r deleteResult, wantPhase state.Phase) {
	t.Helper()
	require.Equal(t, http.StatusConflict, r.rec.Code, r.rec.Body.String())
	code, details := errorBody(t, r.rec)
	assert.Equal(t, ErrCodeConflict, code)
	assert.Equal(t, store.DeletionCodeConflict, details["deletionCode"])
	assert.Contains(t, r.rec.Body.String(), "holds run run-b of this agent, not run run-a")
	got := mustGetAgent(t, s, agentID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is not soft-deleted")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
	assert.Equal(t, string(wantPhase), got.Phase, "the prior phase is restored")
}

// The current run's delete is refused (run-b holds the name): the row is
// kept, with or without force, and no previous-run
// delete is sent.
func TestAgentDelete_CurrentRunMismatch_NotFinalized(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"plain", ""},
		{"force", "?force=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunMismatchFixture(t, "rm-cur-"+tc.name, state.PhaseRunning, true)
			f.client.answer = func(runID string) error {
				return runMismatchEnvelope(t, runID, "run-b", true)
			}
			requireNotFinalized(t, f.store, f.agent.ID, f.del(t, tc.query), state.PhaseRunning)
			assert.Equal(t, []string{"run-a"}, f.client.deleted(), "no previous-run delete after the refusal")
		})
	}
}

// A created row with no launch (the best-effort dispatch, ptone/scion#2635)
// is refused too.
func TestAgentDelete_CurrentRunMismatch_BestEffortNotFinalized(t *testing.T) {
	f := newRunMismatchFixture(t, "rm-created", state.PhaseCreated, false)
	f.client.answer = func(runID string) error { return runMismatchEnvelope(t, runID, "run-b", true) }
	requireNotFinalized(t, f.store, f.agent.ID, f.del(t, ""), state.PhaseCreated)
}

// A retry is refused again while the broker holds run-b, and finalizes
// once it does not.
func TestAgentDelete_CurrentRunMismatch_RetryAfterDriftResolved(t *testing.T) {
	f := newRunMismatchFixture(t, "rm-retry", state.PhaseRunning, false)
	f.client.answer = func(runID string) error { return runMismatchEnvelope(t, runID, "run-b", true) }
	requireNotFinalized(t, f.store, f.agent.ID, f.del(t, ""), state.PhaseRunning)
	requireNotFinalized(t, f.store, f.agent.ID, f.del(t, ""), state.PhaseRunning)

	f.client.answer = func(string) error { return nil }
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
}

// Today's behaviour is kept for every 404 that is not a refusal: the row is
// finalized.
func TestAgentDelete_RunMismatchWithoutDifferentCurrent_Finalizes(t *testing.T) {
	for i, tc := range []struct {
		name   string
		answer func(t *testing.T, runID string) error
	}{
		{"same current run", func(t *testing.T, runID string) error { return runMismatchEnvelope(t, runID, runID, true) }},
		{"empty current run", func(t *testing.T, runID string) error { return runMismatchEnvelope(t, runID, "", true) }},
		{"no details", func(t *testing.T, runID string) error { return runMismatchEnvelope(t, runID, "", false) }},
		{"plain not found", func(t *testing.T, _ string) error {
			return brokerEnvelope(t, http.StatusNotFound, "agent_not_found", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunMismatchFixture(t, fmt.Sprintf("rm-fin-%d", i), state.PhaseRunning, false)
			f.client.answer = func(runID string) error { return tc.answer(t, runID) }
			r := f.del(t, "")
			require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			assert.True(t, agentGone(t, f.store, f.agent.ID))
		})
	}
}

// A previous run's delete answered with the refusal is expected (the
// current run may hold the name) and counts as success: the row is
// finalized after both runs were sent.
func TestAgentDelete_PreviousRunMismatch_Finalizes(t *testing.T) {
	f := newRunMismatchFixture(t, "rm-prev", state.PhaseRunning, true)
	f.client.answer = func(runID string) error {
		if runID == "run-p" {
			return runMismatchEnvelope(t, runID, "run-a", true)
		}
		return nil
	}
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	assert.Equal(t, []string{"run-a", "run-p"}, f.client.deleted())
}

// A refusal recorded on a failed cross-node delete intent reaches the
// engine as the refusal: 409 conflict and the row kept, not a success and
// not a plain runtime_error.
func TestAgentDelete_DeferredRunMismatch_NotFinalized(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
	f := newDeferredDeleteFixture(t, "rm-deferred", nil)
	_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, "run-a")
	require.NoError(t, err)

	// What the executing node records when its DispatchAgentDelete is
	// refused (execDispatchDelete wraps it).
	execErr := fmt.Errorf("dispatch delete: %w", deleteAgentError(runMismatchEnvelope(t, "run-a", "run-b", true), "run-a"))
	require.ErrorIs(t, execErr, ErrDeleteRunMismatch)
	result := dispatchFailureResult(execErr)
	require.NotEmpty(t, result)

	go func() {
		assert.Eventually(t, func() bool {
			all, _ := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
			for _, d := range all {
				if d.AgentID == f.agent.ID {
					if ok, _ := f.store.ClaimBrokerDispatch(context.Background(), d.ID, "test-owner"); ok {
						_ = f.store.FailBrokerDispatch(context.Background(), d.ID, execErr.Error(), result)
					}
					f.bus.PublishDispatchDone(context.Background(), d.ID)
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "the delete intent was never written")
	}()

	r := f.del(t, "")
	requireNotFinalized(t, f.store, f.agent.ID, *r, state.PhaseRunning)
}

// deferredDeleteError rebuilds the refusal from a failed row's broker
// error, using the run the broker names; anything else is unchanged.
func TestDeferredDeleteError(t *testing.T) {
	row := func(err error) error {
		return dispatchFailureError(&store.BrokerDispatch{Op: "delete", Error: err.Error(), Result: dispatchFailureResult(err)})
	}
	refused := deferredDeleteError(row(runMismatchEnvelope(t, "run-x", "run-y", true)))
	var r *DeleteRunMismatchError
	require.True(t, errors.As(refused, &r), "err = %v", refused)
	assert.Equal(t, "run-x", r.RequestedRunID)
	assert.Equal(t, "run-y", r.CurrentRunID)

	for name, err := range map[string]error{
		"same run":     row(runMismatchEnvelope(t, "run-x", "run-x", true)),
		"no current":   row(runMismatchEnvelope(t, "run-x", "", true)),
		"other status": row(brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", nil)),
		"no envelope":  errors.New("dispatch delete failed: owner refused"),
	} {
		t.Run(name, func(t *testing.T) {
			got := deferredDeleteError(err)
			assert.Same(t, err, got)
			assert.False(t, errors.Is(got, ErrDeleteRunMismatch))
		})
	}
	assert.NoError(t, deferredDeleteError(nil))
}

// A move's localOnly cleanup finalizes no row: the refusal (another run,
// such as the moved agent's new one, holds the name) is a success there.
func TestDispatchAgentDeleteLocalOnly_RunMismatchIsSuccess(t *testing.T) {
	d, client, agent := newMoveDispatchFixture(t, &store.BrokerCapabilities{AgentMove: true})
	client.returnErr = deleteAgentError(runMismatchEnvelope(t, "run-1", "run-2", true), "run-1")
	require.ErrorIs(t, client.returnErr, ErrDeleteRunMismatch)
	assert.NoError(t, d.DispatchAgentDeleteLocalOnly(context.Background(), agent))

	client.returnErr = &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}
	assert.Error(t, d.DispatchAgentDeleteLocalOnly(context.Background(), agent), "other errors still fail")
}

func TestRefuseDeleteRunMismatch(t *testing.T) {
	for _, tc := range []struct{ force, bestEffort bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		assert.True(t, refuseDeleteRunMismatch(tc.force, tc.bestEffort), "force=%v bestEffort=%v", tc.force, tc.bestEffort)
	}
}
