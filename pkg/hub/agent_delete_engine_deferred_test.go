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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engine tests that drive the real deferred (cross-node) delete path, store
// fault injection, and notifications (design ptone/scion#2483 §2.3, §2.3.1).

// engineHookStore wraps the server's store to inject faults and observe the
// engine's store calls.
type engineHookStore struct {
	store.Store
	mu               sync.Mutex
	revokeErr        error
	revokeCalls      int
	revokeCtxErrs    []error
	onHasOutstanding func()
}

func (h *engineHookStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID, by, reason string) (int, error) {
	h.mu.Lock()
	h.revokeCalls++
	h.revokeCtxErrs = append(h.revokeCtxErrs, ctx.Err())
	err := h.revokeErr
	h.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return h.Store.RevokeAgentCredentialsByAgent(ctx, agentID, by, reason)
}

func (h *engineHookStore) HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error) {
	h.mu.Lock()
	hook := h.onHasOutstanding
	h.onHasOutstanding = nil
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return h.Store.HasOutstandingBrokerDispatch(ctx, agentID, op)
}

func (h *engineHookStore) setRevokeErr(err error) {
	h.mu.Lock()
	h.revokeErr = err
	h.mu.Unlock()
}

// closedEventsPublisher hands out already-closed subscription channels, to
// drive waitForDispatchDone's closed-channel exit.
type closedEventsPublisher struct{ noopEventPublisher }

func (closedEventsPublisher) Subscribe(_ ...string) (<-chan Event, func()) {
	ch := make(chan Event)
	close(ch)
	return ch, func() {}
}

// deferredDeleteFixture is a server whose dispatcher is a real
// HTTPAgentDispatcher over a broker client that always defers, so every
// delete goes through deferredDelete → deferredDataOpResult →
// waitForDispatchDone, as in production.
type deferredDeleteFixture struct {
	srv    *Server
	store  store.Store // the raw store
	hooks  *engineHookStore
	bus    *ChannelEventPublisher
	pub    *deleteRecordingPublisher
	client *mockRuntimeBrokerClient
	agent  *store.Agent
}

func newDeferredDeleteFixture(t *testing.T, suffix string, dispatchEvents EventPublisher) *deferredDeleteFixture {
	t.Helper()
	srv, s := testServer(t)
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := newDeleteRecordingPublisher(bus)
	srv.events = pub
	hooks := &engineHookStore{Store: s}
	srv.store = hooks
	client := &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	if dispatchEvents == nil {
		dispatchEvents = bus
	}
	d.SetCrossNodeDeps(dispatchEvents, NoopCommandBus{})
	srv.SetDispatcher(d)
	agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning)
	return &deferredDeleteFixture{srv: srv, store: s, hooks: hooks, bus: bus, pub: pub, client: client, agent: agent}
}

func setDeleteWaitTimeout(t *testing.T, fn func(ctx context.Context) time.Duration) {
	t.Helper()
	old := deleteWaitTimeoutFn
	deleteWaitTimeoutFn = fn
	t.Cleanup(func() { deleteWaitTimeoutFn = old })
}

func (f *deferredDeleteFixture) del(t *testing.T, query string) *deleteResult {
	t.Helper()
	r := waitDelete(t, deleteAsync(t, f.srv, "/api/v1/agents/"+f.agent.ID+query, nil), 10*time.Second)
	return &r
}

// pendingDeleteIntents lists the agent's outstanding delete intents.
func (f *deferredDeleteFixture) pendingDeleteIntents(t *testing.T) []store.BrokerDispatch {
	t.Helper()
	all, err := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
	require.NoError(t, err)
	var out []store.BrokerDispatch
	for _, d := range all {
		if d.AgentID == f.agent.ID && d.Op == brokerDispatchOpDelete {
			out = append(out, d)
		}
	}
	return out
}

// endIntent claims a dispatch intent (as an owning node would) and ends it.
func endIntent(t *testing.T, s store.Store, id string, ok bool) {
	t.Helper()
	ctx := context.Background()
	claimed, err := s.ClaimBrokerDispatch(ctx, id, "test-owner")
	require.NoError(t, err)
	require.True(t, claimed)
	if ok {
		require.NoError(t, s.CompleteBrokerDispatch(ctx, id, ""))
	} else {
		require.NoError(t, s.FailBrokerDispatch(ctx, id, "gave up"))
	}
}

func requireInDoubt(t *testing.T, f *deferredDeleteFixture, r *deleteResult) {
	t.Helper()
	require.Equal(t, http.StatusBadGateway, r.rec.Code, r.rec.Body.String())
	_, details := errorBody(t, r.rec)
	assert.Equal(t, store.DeletionCodeInDoubt, details["deletionCode"])
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeInDoubt, got.DeletionCode)
	assert.Equal(t, string(state.PhaseStopping), got.Phase, "in_doubt does not roll back to a live phase")
	assert.True(t, got.DeletedAt.IsZero())
}

// Acceptance (m), (x) engine path, (z): a deferred delete whose intent is
// still outstanding becomes in_doubt, whichever wait exit fires. Start then
// answers 409 until the intent is terminal, and a retry DELETE completes.
func TestAgentDeleteEngine_DeferredOutstandingIsInDoubt(t *testing.T) {
	for _, exit := range []string{"timer", "ctx.Done", "closed channel"} {
		t.Run(exit, func(t *testing.T) {
			var dispatchEvents EventPublisher
			switch exit {
			case "timer":
				// The wait's timer shorter than the engine ctx.
				setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
			case "ctx.Done":
				setDeleteKnob(t, &deleteDispatchBudget, 200*time.Millisecond)
				setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
			case "closed channel":
				setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
				dispatchEvents = closedEventsPublisher{}
			}
			f := newDeferredDeleteFixture(t, "indoubt-"+map[string]string{"timer": "t", "ctx.Done": "c", "closed channel": "x"}[exit], dispatchEvents)

			r := f.del(t, "")
			requireInDoubt(t, f, r)
			intents := f.pendingDeleteIntents(t)
			require.Len(t, intents, 1, "the intent is still outstanding")

			// (z): no expiresAt, and the banner persists past the 15m TTL.
			got := mustGetAgent(t, f.store, f.agent.ID)
			view := store.ComputeAgentDeletion(got, time.Now())
			require.NotNil(t, view)
			assert.Nil(t, view.ExpiresAt)
			later := store.ComputeAgentDeletion(got, time.Now().Add(16*time.Minute))
			require.NotNil(t, later)
			assert.Equal(t, store.DeletionCodeInDoubt, later.Code)

			// (m)/(x): start is blocked while the intent is outstanding.
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
			requireDeleteInProgress(t, rec)

			// The intent ends; a retry DELETE (broker now answers directly)
			// completes.
			endIntent(t, f.store, intents[0].ID, false)
			f.client.returnErr = nil
			r = f.del(t, "")
			require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			assert.True(t, agentGone(t, f.store, f.agent.ID))
		})
	}
}

// Acceptance (m): a deferred intent that ends failed gives an ordinary
// runtime_error rollback.
func TestAgentDeleteEngine_DeferredFailedIntentRollsBack(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
	f := newDeferredDeleteFixture(t, "deffail", nil)

	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			all, _ := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
			for _, d := range all {
				if d.AgentID == f.agent.ID {
					if ok, _ := f.store.ClaimBrokerDispatch(context.Background(), d.ID, "test-owner"); ok {
						_ = f.store.FailBrokerDispatch(context.Background(), d.ID, "owner refused")
					}
					f.bus.PublishDispatchDone(context.Background(), d.ID)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	r := f.del(t, "")
	require.Equal(t, http.StatusBadGateway, r.rec.Code, r.rec.Body.String())
	_, details := errorBody(t, r.rec)
	assert.Equal(t, store.DeletionCodeRuntimeError, details["deletionCode"])
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "an ordinary rollback restores the prior phase")
}

// Acceptance (m): an intent that reaches done after the wait exits but
// before the classification runs → finalize: credentials revoked, row
// deleted.
func TestAgentDeleteEngine_DeferredDoneAfterWaitFinalizes(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	f := newDeferredDeleteFixture(t, "deflate", nil)
	f.hooks.onHasOutstanding = func() {
		for _, d := range f.pendingDeleteIntents(t) {
			endIntent(t, f.store, d.ID, true)
		}
	}

	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	f.hooks.mu.Lock()
	assert.Equal(t, 1, f.hooks.revokeCalls, "credentials revoked")
	f.hooks.mu.Unlock()
}

// Acceptance (t): in_doubt → a retry whose tunnel call fails (500) is
// in_doubt again, because the older intent is still outstanding; start
// stays 409; force deletes the row.
func TestAgentDeleteEngine_InDoubtRetryStaysInDoubtUntilForce(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	f := newDeferredDeleteFixture(t, "retry500", nil)

	requireInDoubt(t, f, f.del(t, ""))

	f.client.returnErr = &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}
	requireInDoubt(t, f, f.del(t, ""))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	requireDeleteInProgress(t, rec)

	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
}

// Acceptance (y): only a ctx carrying withDeleteWaitBudget changes the
// deferred delete wait; every other caller keeps the 15s default.
func TestDeleteWaitTimeoutFn_DefaultAndBudget(t *testing.T) {
	assert.Equal(t, dispatchDeleteTimeout, deleteWaitTimeoutFn(context.Background()))
	assert.Equal(t, 15*time.Second, deleteWaitTimeoutFn(context.Background()))
	ctx := withDeleteWaitBudget(context.Background(), 90*time.Second)
	assert.Equal(t, 90*time.Second, deleteWaitTimeoutFn(ctx))
	assert.Equal(t, dispatchDeleteTimeout, deleteWaitTimeoutFn(withDeleteWaitBudget(context.Background(), 0)))
}

// Acceptance (r), (u): a revoke failure publishes failed/revoke_failed at
// once, answers 502, and leaves the row live in finalizing: start → 409,
// stop → 200, no expiresAt. A retry DELETE finalizes without re-dispatching.
func TestAgentDeleteEngine_RevokeFailure(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	hooks := &engineHookStore{Store: s}
	srv.store = hooks
	hooks.setRevokeErr(errors.New("credential store down"))
	agent := setupBrokerAgentInPhase(t, s, "revoke", state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeRevokeFailed, details["deletionCode"])

	got := mustGetAgent(t, s, agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "the row stays live")
	assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	assert.False(t, got.DeletionActive(time.Now()), "abandoned immediately, not after the lease")
	view := store.ComputeAgentDeletion(got, time.Now())
	require.NotNil(t, view)
	assert.Equal(t, store.DeletionCodeRevokeFailed, view.Code)
	assert.Nil(t, view.ExpiresAt, "(u) no expiresAt")
	evs := pub.snapshot()
	last := evs[len(evs)-1]
	require.NotNil(t, last.deletion)
	assert.Equal(t, store.DeletionCodeRevokeFailed, last.deletion.Code)
	assert.Zero(t, pub.count("deleted"))

	// (u): start blocked, stop a no-op.
	requireDeleteInProgress(t, doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.DeletionStateFinalizing, mustGetAgent(t, s, agent.ID).DeletionState)

	hooks.setRevokeErr(nil)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 1, disp.callCount(), "the finalizing re-claim skips the dispatch")
	assert.True(t, agentGone(t, s, agent.ID))
}

// Acceptance (k), first half: a dispatch that uses its whole budget still
// revokes credentials, on a step budget of its own.
func TestAgentDeleteEngine_FullDispatchBudgetStillRevokes(t *testing.T) {
	setDeleteKnob(t, &deleteDispatchBudget, 300*time.Millisecond)
	srv, s, _, disp := engineTestServer(t)
	hooks := &engineHookStore{Store: s}
	srv.store = hooks
	disp.setFn(func(ctx context.Context, _ *store.Agent) error {
		dl, ok := ctx.Deadline()
		require.True(t, ok)
		time.Sleep(time.Until(dl) - 20*time.Millisecond)
		return nil
	})
	agent := setupBrokerAgentInPhase(t, s, "budget", state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Equal(t, 1, hooks.revokeCalls)
	assert.NoError(t, hooks.revokeCtxErrs[0], "the revoke ran on its own budget")
}

// stallingMessageDispatcher blocks notification delivery until release.
type stallingMessageDispatcher struct {
	recordingDispatcher
	release chan struct{}
}

func (d *stallingMessageDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, msg string, interrupt bool, sm *messages.StructuredMessage) error {
	<-d.release
	return d.recordingDispatcher.DispatchAgentMessage(ctx, a, msg, interrupt, sm)
}

// deletedSubscription subscribes subscriber (an agent slug, or a user ID
// when user is true) to DELETED on agent.
func deletedSubscription(t *testing.T, s store.Store, agent *store.Agent, subscriberType, subscriberID string) {
	t.Helper()
	require.NoError(t, s.CreateNotificationSubscription(context.Background(), &store.NotificationSubscription{
		ID: api.NewUUID(), Scope: store.SubscriptionScopeAgent, AgentID: agent.ID,
		SubscriberType: subscriberType, SubscriberID: subscriberID, ProjectID: agent.ProjectID,
		TriggerActivities: []string{"DELETED"}, CreatedAt: time.Now().Add(-time.Minute), CreatedBy: "test",
	}))
}

// Acceptance (f): a hard delete with an agent-scoped subscription produces
// exactly one DELETED notification, and the notification survives the
// agent row's removal. A soft delete produces exactly one.
func TestAgentDeleteEngine_DeletedNotifications(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
	}{{"hard", 0}, {"soft", time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			srv.notificationDispatcher = NewNotificationDispatcher(s, srv.events, func() AgentDispatcher { return &recordingDispatcher{} }, slog.Default())
			agent := setupBrokerAgentInPhase(t, s, "notif-"+tc.name, state.PhaseRunning)
			deletedSubscription(t, s, agent, store.SubscriberTypeUser, "watcher-"+tc.name)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			require.Eventually(t, func() bool {
				n, _ := s.GetNotifications(context.Background(), store.SubscriberTypeUser, "watcher-"+tc.name, false)
				return len(n) == 1
			}, 5*time.Second, 20*time.Millisecond)
			time.Sleep(100 * time.Millisecond)
			n, err := s.GetNotifications(context.Background(), store.SubscriberTypeUser, "watcher-"+tc.name, false)
			require.NoError(t, err)
			require.Len(t, n, 1, "exactly one")
			assert.Equal(t, "DELETED", n[0].Status)
		})
	}
}

// Acceptance (k), second half: a notification subscriber that stalls does
// not hold up the delete: delivery is asynchronous.
func TestAgentDeleteEngine_StalledNotificationDoesNotBlockDelete(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	stall := &stallingMessageDispatcher{release: make(chan struct{})}
	srv.notificationDispatcher = NewNotificationDispatcher(s, srv.events, func() AgentDispatcher { return stall }, slog.Default())
	agent := setupBrokerAgentInPhase(t, s, "stall", state.PhaseRunning)
	subscriber := &store.Agent{
		ID: tid("stall-sub"), Slug: "stall-sub", Name: "Stall Sub", ProjectID: agent.ProjectID,
		RuntimeBrokerID: agent.RuntimeBrokerID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(context.Background(), subscriber))
	deletedSubscription(t, s, agent, store.SubscriberTypeAgent, subscriber.Slug)

	start := time.Now()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(start), 3*time.Second, "the stalled subscriber does not delay the answer")
	assert.True(t, agentGone(t, s, agent.ID))

	close(stall.release)
	require.Eventually(t, func() bool { return len(stall.getCalls()) == 1 }, 5*time.Second, 20*time.Millisecond)
}
