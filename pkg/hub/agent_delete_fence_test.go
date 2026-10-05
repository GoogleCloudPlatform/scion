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
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for delete dispatch fencing (ptone/scion#2906): the engine's
// notAfter, the handling of a broker's 409 stale_dispatch, and the claim
// re-check on deferred delete intents.

// setDeleteClock pins the engine's clock to now for the test.
func setDeleteClock(t *testing.T, now func() time.Time) {
	t.Helper()
	old := deleteClock
	deleteClock = now
	t.Cleanup(func() { deleteClock = old })
}

// fenceNow is a fixed engine time at second precision (notAfter goes on
// the wire as RFC3339), close to the real time so the store's own
// time-based views agree with the engine's.
func fenceNow(t *testing.T) time.Time {
	t.Helper()
	t0 := time.Now().UTC().Truncate(time.Second)
	setDeleteClock(t, func() time.Time { return t0 })
	return t0
}

const staleDispatchBody = `{"error":{"code":"stale_dispatch","message":"delete dispatch arrived after its deadline; nothing was done"}}`

func staleDispatchErr() error {
	return &brokerStatusError{StatusCode: http.StatusConflict, Body: staleDispatchBody}
}

// The engine sends notAfter = min(lease expiry, now + dispatch budget).
func TestDeleteFence_EngineSendsNotAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{"lease bound", 0},                 // default: lease 60s - 5s < budget 120s
		{"budget bound", 30 * time.Second}, // budget below the lease
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.budget != 0 {
				setDeleteKnob(t, &deleteDispatchBudget, tc.budget)
			}
			t0 := fenceNow(t)
			srv, s := testServer(t)
			client := &mockRuntimeBrokerClient{}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-send-"+map[bool]string{true: "b", false: "l"}[tc.budget != 0], state.PhaseRunning)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			require.True(t, client.deleteCalled)

			got := client.lastDeleteOpts.notAfter
			require.False(t, got.IsZero(), "the engine sent no notAfter")
			assert.False(t, got.After(t0.Add(deleteLease-deleteNotAfterMargin)), "notAfter %v past the lease expiry less the margin", got)
			assert.False(t, got.After(t0.Add(deleteDispatchBudget)), "notAfter %v past now+budget %v", got, t0.Add(deleteDispatchBudget))
			want := t0.Add(55 * time.Second) // lease 60s - margin 5s
			if tc.budget != 0 {
				want = t0.Add(tc.budget)
			}
			assert.True(t, got.Equal(want), "notAfter = %v, want %v", got, want)
		})
	}
}

func TestDeleteNotAfter_MinOfLeaseAndBudget(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// The lease bound subtracts the margin, toward refusal.
	assert.Equal(t, t0.Add(35*time.Second), deleteNotAfter(t0, t0.Add(40*time.Second)))
	assert.Equal(t, t0.Add(deleteDispatchBudget), deleteNotAfter(t0, t0.Add(deleteDispatchBudget+time.Minute)))
	assert.Equal(t, t0.Add(deleteDispatchBudget), deleteNotAfter(t0, time.Time{}))
	assert.Equal(t, 5*time.Second, deleteNotAfterMargin, "keep equal to the broker's deleteNotAfterSkew")
}

// Both transports put notAfter on the delete query only when set.
func TestDeleteAgentQuery_NotAfter(t *testing.T) {
	na := time.Date(2026, 10, 5, 12, 0, 0, 0, time.FixedZone("x", 3600))
	q, err := url.ParseQuery(deleteAgentQuery(context.Background(), "p1", DeleteAgentOptions{RunID: "r", NotAfter: na}))
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T11:00:00Z", q.Get("notAfter"), "RFC3339 in UTC")
	q, _ = url.ParseQuery(deleteAgentQuery(context.Background(), "p1", DeleteAgentOptions{RunID: "r"}))
	assert.False(t, q.Has("notAfter"), "notAfter sent without a deadline")
}

func TestHTTPRuntimeBrokerClient_DeleteAgentSendsNotAfter(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	na := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	require.NoError(t, NewHTTPRuntimeBrokerClient().DeleteAgent(context.Background(), tid("host-1"), server.URL, "a", "p", DeleteAgentOptions{NotAfter: na}))
	assert.Equal(t, "2026-10-05T12:00:00Z", gotQuery.Get("notAfter"))
}

func TestControlChannelBrokerClient_DeleteAgentSendsNotAfter(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	client := &ControlChannelBrokerClient{manager: tunnel}
	na := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	require.NoError(t, client.DeleteAgent(context.Background(), "broker-1", "unused", "a", "p", DeleteAgentOptions{NotAfter: na}))
	q, err := url.ParseQuery(tunnel.lastRequest.Query)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T12:00:00Z", q.Get("notAfter"))
}

// A broker's 409 stale_dispatch, over either transport, is recognised.
func TestIsStaleDeleteDispatch(t *testing.T) {
	assert.True(t, isStaleDeleteDispatch(staleDispatchErr()))
	assert.True(t, isStaleDeleteDispatch(errStaleDeleteDispatch))
	assert.False(t, isStaleDeleteDispatch(&brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict"}}`}))
	assert.False(t, isStaleDeleteDispatch(&brokerStatusError{StatusCode: http.StatusInternalServerError, Body: staleDispatchBody}))

	tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusConflict, body: []byte(staleDispatchBody)}
	err := (&ControlChannelBrokerClient{manager: tunnel}).DeleteAgent(context.Background(), "b", "", "a", "p", DeleteAgentOptions{})
	assert.True(t, isStaleDeleteDispatch(err), "control channel 409 stale_dispatch: %v", err)
}

// A 409 stale_dispatch is not acted on: never finalized (not even with
// force), the row stays, and the claim is abandoned.
func TestDeleteFence_StaleDispatchIsNotActedOn(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "force"}[force], func(t *testing.T) {
			srv, s := testServer(t)
			client := &mockRuntimeBrokerClient{returnErr: staleDispatchErr()}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-stale-"+map[bool]string{false: "p", true: "f"}[force], state.PhaseRunning)

			path := "/api/v1/agents/" + agent.ID
			if force {
				path += "?force=true"
			}
			rec := doRequest(t, srv, http.MethodDelete, path, nil)
			require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())
			require.True(t, client.deleteCalled)

			got := mustGetAgent(t, s, agent.ID)
			assert.True(t, got.DeletedAt.IsZero(), "the row was soft-deleted")
			assert.NotEqual(t, store.DeletionStateFinalizing, got.DeletionState, "the delete was finalized")
			view := store.ComputeAgentDeletion(got, time.Now())
			require.NotNil(t, view)
			assert.Equal(t, store.DeletionCodeAbandoned, view.Code, "the claim reads abandoned")
			_, details := errorBody(t, rec)
			assert.Equal(t, store.DeletionCodeAbandoned, details["deletionCode"])
			assert.Contains(t, rec.Body.String(), "after its deadline", "a stale-specific message")
		})
	}
}

// DispatchAgentDelete callers other than the engine send no notAfter.
func TestDeleteFence_NonEngineCallersOmitNotAfter(t *testing.T) {
	_, s := testServer(t)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	agent := setupBrokerAgentInPhase(t, s, "fence-nonengine", state.PhaseRunning)
	require.NoError(t, d.DispatchAgentDelete(context.Background(), agent, true, true, false, time.Time{}))
	require.True(t, client.deleteCalled)
	assert.True(t, client.lastDeleteOpts.notAfter.IsZero(), "notAfter = %v, want none", client.lastDeleteOpts.notAfter)
}

// The originating node records the engine's claim on a deferred intent; a
// stale refusal from the executing node reaches the engine as
// not-acted-on (abandoned), not as an ordinary runtime_error rollback.
func TestDeleteFence_DeferredIntentCarriesClaim_StaleIsAbandoned(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
	f := newDeferredDeleteFixture(t, "fence-deferred", nil)

	claims := make(chan int64, 1)
	go func() {
		assert.Eventually(t, func() bool {
			intents := f.pendingDeleteIntents(t)
			if len(intents) == 0 {
				return false
			}
			d := intents[0]
			args, err := UnmarshalDeleteArgs(d.Args)
			if err != nil {
				return false
			}
			claims <- args.Claim
			if ok, _ := f.store.ClaimBrokerDispatch(context.Background(), d.ID, "test-owner"); ok {
				_ = f.store.FailBrokerDispatch(context.Background(), d.ID, "dispatch delete: "+errStaleDeleteDispatch.Error())
			}
			f.bus.PublishDispatchDone(context.Background(), d.ID)
			return true
		}, 5*time.Second, 10*time.Millisecond, "the delete intent was never written")
	}()

	r := f.del(t, "")
	require.NotEqual(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	got := mustGetAgent(t, f.store, f.agent.ID)
	select {
	case c := <-claims:
		assert.Equal(t, got.DeletionClaim, c, "the intent carries the engine's claim")
		assert.NotZero(t, c)
	default:
		t.Fatal("no intent seen")
	}
	assert.True(t, got.DeletedAt.IsZero())
	view := store.ComputeAgentDeletion(got, time.Now())
	require.NotNil(t, view)
	assert.Equal(t, store.DeletionCodeAbandoned, view.Code)
}

// The executing node re-reads the row: an intent whose claim is no longer
// the current, live claim is dropped without dispatching; a current one is
// dispatched with notAfter computed from the row's lease at send time.
func TestDeleteFence_ExecDeferredDeleteChecksClaim(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name        string
		seed        deleteSeed
		claimAdj    int64 // intent claim = row claim + claimAdj
		softDeleted bool
		wantSend    bool
		// wantNotAfter computes the expected deadline from t0 and the row.
		wantNotAfter func(t0 time.Time, row *store.Agent) time.Time
	}
	leaseBound := func(_ time.Time, row *store.Agent) time.Time {
		return row.DeletionLeaseAt.Add(-deleteNotAfterMargin)
	}
	budgetBound := func(t0 time.Time, _ *store.Agent) time.Time { return t0.Add(deleteDispatchBudget) }
	for _, c := range []tc{
		{name: "current live claim", seed: seedLiveDeleting, wantSend: true, wantNotAfter: leaseBound},
		// The engine's wait ended with this intent outstanding: it still
		// runs (design §2.3.1, follow-up 3), bounded by the budget.
		{name: "claim failed in_doubt", seed: deleteSeed{name: "in_doubt", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}, wantSend: true, wantNotAfter: budgetBound},
		{name: "in_doubt but soft-deleted", seed: deleteSeed{name: "in_doubt", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}, softDeleted: true},
		{name: "live claim but soft-deleted", seed: seedLiveDeleting, softDeleted: true},
		{name: "in_doubt of an older claim", seed: deleteSeed{name: "in_doubt", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}, claimAdj: -1},
		{name: "newer claim on the row", seed: seedLiveDeleting, claimAdj: -1},
		{name: "lease lapsed", seed: deleteSeed{name: "lapsed", state: store.DeletionStateDeleting, leaseIn: -time.Minute}},
		{name: "claim failed", seed: deleteSeed{name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}},
		{name: "claim finalizing", seed: deleteSeed{name: "finalizing", state: store.DeletionStateFinalizing, leaseIn: time.Minute}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t0 := fenceNow(t)
			srv, s := testServer(t)
			client := &mockRuntimeBrokerClient{}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-exec", state.PhaseRunning)
			seedAgentDeletion(t, s, agent.ID, c.seed)
			if c.claimAdj < 0 {
				// A later claim took the row (claim 2); the intent is claim 1's.
				seedAgentDeletion(t, s, agent.ID, c.seed)
			}
			if c.softDeleted {
				past := time.Now().Add(-time.Minute)
				n, err := s.UpdateAgentDeletion(ctx, agent.ID, store.DeletionPredicate{}, store.DeletionFields{DeletedAt: &past})
				require.NoError(t, err)
				require.Equal(t, 1, n)
			}
			row := mustGetAgent(t, s, agent.ID)
			args, err := MarshalDispatchArgs(&DeleteDispatchArgs{Claim: row.DeletionClaim + c.claimAdj})
			require.NoError(t, err)

			_, execErr := srv.execDispatchDelete(ctx, store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: args})
			if !c.wantSend {
				require.Error(t, execErr)
				assert.ErrorIs(t, execErr, errStaleDeleteDispatch)
				assert.True(t, staleDeleteDispatchFromText(execErr.Error()), "the row error text keeps the stale marker")
				assert.False(t, client.deleteCalled, "a stale intent reached the broker")
				return
			}
			require.NoError(t, execErr)
			require.True(t, client.deleteCalled)
			want := c.wantNotAfter(t0, row)
			assert.True(t, client.lastDeleteOpts.notAfter.Equal(want), "notAfter = %v, want %v", client.lastDeleteOpts.notAfter, want)
		})
	}
	t.Run("no claim (not from the engine)", func(t *testing.T) {
		srv, s := testServer(t)
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
		agent := setupBrokerAgentInPhase(t, s, "fence-exec-nc", state.PhaseRunning)
		_, err := srv.execDispatchDelete(ctx, store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: `{"deleteFiles":true}`})
		require.NoError(t, err)
		assert.True(t, client.deleteCalled)
		assert.True(t, client.lastDeleteOpts.notAfter.IsZero())
	})
	t.Run("broker refuses as stale", func(t *testing.T) {
		srv, s := testServer(t)
		client := &mockRuntimeBrokerClient{returnErr: staleDispatchErr()}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
		agent := setupBrokerAgentInPhase(t, s, "fence-exec-br", state.PhaseRunning)
		seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
		row := mustGetAgent(t, s, agent.ID)
		args, _ := MarshalDispatchArgs(&DeleteDispatchArgs{Claim: row.DeletionClaim})
		_, err := srv.execDispatchDelete(ctx, store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: args})
		require.Error(t, err)
		assert.True(t, staleDeleteDispatchFromText(err.Error()), "error text %q lacks the stale marker", err.Error())
	})
}

// An in_doubt intent's deadline is bounded by the executing ctx's deadline
// when that comes first.
func TestDeferredDeleteDeadline_InDoubtUsesCtxDeadline(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	row := &store.Agent{DeletionClaim: 3, DeletionState: store.DeletionStateFailed, DeletionCode: store.DeletionCodeInDoubt}
	got, ok := deferredDeleteDeadline(context.Background(), row, 3, t0)
	require.True(t, ok)
	assert.Equal(t, t0.Add(deleteDispatchBudget), got)

	dl := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	defer cancel()
	got, ok = deferredDeleteDeadline(ctx, row, 3, time.Now())
	require.True(t, ok)
	assert.True(t, got.Equal(dl), "notAfter = %v, want the ctx deadline %v", got, dl)
}

// renewalHookStore observes the engine's lease renewals. The first renewal
// fails (failFirst) or goes through; later ones are held until release is
// closed, so exactly one renewal lands before the dispatch. SwapRunIntent
// (the engine's run-intent write, just before the dispatch computes
// notAfter) waits for that first renewal.
type renewalHookStore struct {
	store.Store
	failFirst bool
	first     chan struct{} // closed once the first renewal returned
	release   chan struct{}
	mu        sync.Mutex
	renewals  int
}

func isRenewal(set store.DeletionFields) bool {
	return set.KeepUpdated && set.LeaseAt != nil && set.State == nil && !set.BumpClaim
}

func (h *renewalHookStore) UpdateAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, set store.DeletionFields) (int, error) {
	if !isRenewal(set) {
		return h.Store.UpdateAgentDeletion(ctx, id, pred, set)
	}
	h.mu.Lock()
	h.renewals++
	n := h.renewals
	h.mu.Unlock()
	if n > 1 {
		<-h.release
		return h.Store.UpdateAgentDeletion(ctx, id, pred, set)
	}
	defer close(h.first)
	if h.failFirst {
		return 0, errInjectedDeletionWrite
	}
	return h.Store.UpdateAgentDeletion(ctx, id, pred, set)
}

func (h *renewalHookStore) SwapRunIntent(ctx context.Context, agentID string, intent store.RunIntent) (store.RunIntent, time.Time, error) {
	select {
	case <-h.first:
	case <-time.After(10 * time.Second):
	}
	return h.Store.SwapRunIntent(ctx, agentID, intent)
}

// releasingClient records the dispatch's notAfter, then lets held renewals
// through so the engine can stop its renewal goroutine.
type releasingClient struct {
	mockRuntimeBrokerClient
	release chan struct{}
	once    sync.Once
}

func (c *releasingClient) DeleteAgent(ctx context.Context, brokerID, endpoint, agentID, projectID string, opts DeleteAgentOptions) error {
	err := c.mockRuntimeBrokerClient.DeleteAgent(ctx, brokerID, endpoint, agentID, projectID, opts)
	c.once.Do(func() { close(c.release) })
	return err
}

// notAfter tracks the lease the engine last wrote: a renewal that took
// moves it forward, a failed one does not.
func TestDeleteFence_NotAfterTracksRenewals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failFirst bool
		want      time.Duration // from t0
	}{
		// Claim at t0 (lease t0+60s); the renewal runs at t0+20s and
		// writes t0+80s, so notAfter = t0+80s-5s.
		{"renewal took", false, 75 * time.Second},
		// The renewal failed: the row still holds t0+60s.
		{"renewal failed", true, 55 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDeleteKnob(t, &deleteLeaseRenewInterval, 5*time.Millisecond)
			t0 := time.Now().UTC().Truncate(time.Second)
			var clockMu sync.Mutex
			now := t0
			setDeleteClock(t, func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now })

			srv, s := testServer(t)
			release := make(chan struct{})
			hooks := &renewalHookStore{Store: s, failFirst: tc.failFirst, first: make(chan struct{}), release: release}
			srv.store = hooks
			client := &releasingClient{release: release}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "fence-renew-"+map[bool]string{true: "f", false: "ok"}[tc.failFirst], state.PhaseRunning)

			plan, err := srv.claimAgentDeletion(context.Background(), agent.ID, agentDeleteParams{})
			require.NoError(t, err)
			require.NotNil(t, plan)
			clockMu.Lock()
			now = t0.Add(20 * time.Second)
			clockMu.Unlock()
			select {
			case <-srv.runAgentDeletion(context.Background(), plan):
			case <-time.After(10 * time.Second):
				t.Fatal("the engine never finished")
			}

			require.True(t, client.deleteCalled)
			want := t0.Add(tc.want)
			got := client.lastDeleteOpts.notAfter
			assert.True(t, got.Equal(want), "notAfter = %v, want %v", got, want)
		})
	}
}
