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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the phase 1a-2 delete engine (design ptone/scion#2483 §2.1,
// §2.3, §2.3.1, §2.4). Each test names the §8 phase 1a acceptance letters it
// covers.

// --- helpers ---

// setDeleteKnob overrides a timing knob for the test.
func setDeleteKnob(t *testing.T, knob *time.Duration, v time.Duration) {
	t.Helper()
	old := *knob
	*knob = v
	t.Cleanup(func() { *knob = old })
}

// recordedAgentEvent is one agent event as a subscriber would see it.
type recordedAgentEvent struct {
	kind     string // "status" | "deleted"
	phase    string
	activity string
	deletion *store.DeletionInfo
}

// deleteRecordingPublisher records agent status and deleted events (with the
// deletion view the real publisher would compute) and forwards everything to
// an inner publisher.
type deleteRecordingPublisher struct {
	EventPublisher
	mu     sync.Mutex
	events []recordedAgentEvent
}

func newDeleteRecordingPublisher(inner EventPublisher) *deleteRecordingPublisher {
	return &deleteRecordingPublisher{EventPublisher: inner}
}

func (p *deleteRecordingPublisher) PublishAgentStatus(ctx context.Context, a *store.Agent) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "status", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.EventPublisher.PublishAgentStatus(ctx, a)
}

func (p *deleteRecordingPublisher) PublishAgentDeleted(ctx context.Context, agentID, projectID string) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{kind: "deleted"})
	p.mu.Unlock()
	p.EventPublisher.PublishAgentDeleted(ctx, agentID, projectID)
}

func (p *deleteRecordingPublisher) snapshot() []recordedAgentEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedAgentEvent(nil), p.events...)
}

func (p *deleteRecordingPublisher) count(kind string) int {
	n := 0
	for _, e := range p.snapshot() {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// engineStubDispatcher is a goroutine-safe delete dispatcher whose behaviour
// is a per-test function.
type engineStubDispatcher struct {
	createAgentDispatcher
	mu      sync.Mutex
	calls   int
	softArg []bool
	fn      func(ctx context.Context, a *store.Agent) error
}

func (d *engineStubDispatcher) DispatchAgentDelete(ctx context.Context, a *store.Agent, _, _, soft bool, _ time.Time) error {
	d.mu.Lock()
	d.calls++
	d.softArg = append(d.softArg, soft)
	fn := d.fn
	d.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, a)
}

func (d *engineStubDispatcher) DispatchAgentStart(_ context.Context, a *store.Agent, _ string, _ bool) error {
	a.Phase = string(state.PhaseRunning)
	return nil
}

func (d *engineStubDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }

func (d *engineStubDispatcher) setFn(fn func(ctx context.Context, a *store.Agent) error) {
	d.mu.Lock()
	d.fn = fn
	d.mu.Unlock()
}

func (d *engineStubDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// blockingDelete returns a dispatch fn that signals entered and blocks
// until release is closed (or ctx ends), then returns err.
func blockingDelete(entered chan<- struct{}, release <-chan struct{}, err error) func(ctx context.Context, a *store.Agent) error {
	var once sync.Once
	return func(ctx context.Context, _ *store.Agent) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// deleteResult is an async DELETE's answer.
type deleteResult struct {
	rec     *httptest.ResponseRecorder
	elapsed time.Duration
}

func deleteAsync(t *testing.T, srv *Server, path string, headers map[string]string) <-chan deleteResult {
	t.Helper()
	out := make(chan deleteResult, 1)
	go func() {
		start := time.Now()
		rec := doRequestHeaders(t, srv, http.MethodDelete, path, nil, headers)
		out <- deleteResult{rec: rec, elapsed: time.Since(start)}
	}()
	return out
}

func waitDelete(t *testing.T, ch <-chan deleteResult, within time.Duration) deleteResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("DELETE did not answer within %s", within)
		return deleteResult{}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// errorBody decodes the standard error envelope.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Details
}

// agentGone reports whether the row is hard-deleted.
func agentGone(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetAgent(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	require.NoError(t, err)
	return false
}

func mustGetAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

// ownerDeleteRequest makes agentID owned by a fresh user and returns a
// DELETE request authenticated as that user on ctx (the resource-owner
// bypass authorizes ActionDelete).
func ownerDeleteRequest(t *testing.T, s store.Store, ctx context.Context, agentID string) *http.Request {
	t.Helper()
	userID := tid("owner-" + agentID)
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))
	a := mustGetAgent(t, s, agentID)
	a.OwnerID = userID
	a.CreatedBy = userID
	require.NoError(t, s.UpdateAgent(context.Background(), a))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	return req.WithContext(contextWithIdentity(ctx, NewAuthenticatedUser(userID, userID+"@test.com", "Owner", "member", "cli")))
}

// engineTestServer is a test server with a recording publisher over a real
// channel bus and a stub dispatcher.
func engineTestServer(t *testing.T) (*Server, store.Store, *deleteRecordingPublisher, *engineStubDispatcher) {
	t.Helper()
	srv, s := testServer(t)
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := newDeleteRecordingPublisher(bus)
	srv.events = pub
	disp := &engineStubDispatcher{}
	srv.SetDispatcher(disp)
	return srv, s, pub, disp
}

// --- (a): event sequence ---

// Acceptance (a), R1: hard, soft, force and created deletes produce exactly
// status{deletion:deleting} → deleted. A running agent shows stopping; a
// stopped agent keeps stopped; a created agent keeps created (EM-approved
// ptone/scion#2635 interpretation). The soft finish emits no stopped status.
func TestAgentDeleteEngine_EventSequence(t *testing.T) {
	cases := []struct {
		name      string
		phase     state.Phase
		retention time.Duration
		query     string
		wantPhase string
		wantSoft  bool
	}{
		{"hard running", state.PhaseRunning, 0, "", string(state.PhaseStopping), false},
		{"soft running", state.PhaseRunning, time.Hour, "", string(state.PhaseStopping), true},
		{"force running", state.PhaseRunning, time.Hour, "?force=true", string(state.PhaseStopping), false},
		{"hard stopped", state.PhaseStopped, 0, "", string(state.PhaseStopped), false},
		{"soft stopped", state.PhaseStopped, time.Hour, "", string(state.PhaseStopped), true},
		{"created", state.PhaseCreated, 0, "", string(state.PhaseCreated), false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, pub, disp := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			agent := setupBrokerAgentInPhase(t, s, "seq-"+string(rune('a'+i)), tc.phase)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+tc.query, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, 1, disp.callCount())

			evs := pub.snapshot()
			require.Len(t, evs, 2, "exactly status{deleting} then deleted: %+v", evs)
			assert.Equal(t, "status", evs[0].kind)
			require.NotNil(t, evs[0].deletion)
			assert.Equal(t, store.DeletionStateDeleting, evs[0].deletion.State)
			assert.Equal(t, tc.wantSoft, evs[0].deletion.Soft)
			assert.Equal(t, tc.wantPhase, evs[0].phase)
			assert.Equal(t, "deleted", evs[1].kind)

			if tc.wantSoft {
				got := mustGetAgent(t, s, agent.ID)
				assert.False(t, got.DeletedAt.IsZero())
				assert.Equal(t, string(state.PhaseStopped), got.Phase)
				assert.Equal(t, store.DeletionStateNone, got.DeletionState, "the soft finish clears the marker")
				assert.Empty(t, got.DeletionPrior)
				assert.Empty(t, got.DeletionRequest)
				assert.Nil(t, got.DeletionLeaseAt)
			} else {
				assert.True(t, agentGone(t, s, agent.ID))
			}
		})
	}
}

// --- (b), (p): broker failure and fast answers ---

// Acceptance (b), (p): a broker 502 rolls back to the prior phase with
// status{prior, failed, runtime_error} and HTTP 502, within the stub's
// latency (noop publisher: no event can wake anyone). A later stop clears
// the failed marker.
func TestAgentDeleteEngine_BrokerFailureRollsBack(t *testing.T) {
	srv, s := testServer(t)
	events := &deleteCountingEventPublisher{}
	srv.events = events
	disp := &engineStubDispatcher{}
	srv.SetDispatcher(disp)
	disp.setFn(func(context.Context, *store.Agent) error {
		time.Sleep(50 * time.Millisecond)
		return errors.New("broker boom")
	})
	agent := setupBrokerAgentInPhase(t, s, "rollback", state.PhaseRunning)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{Activity: "working"}))

	start := time.Now()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(start), 3*time.Second, "answered within the stub's latency, not at deleteSyncWait")
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeRuntimeError, code)
	assert.Equal(t, store.DeletionCodeRuntimeError, details["deletionCode"])
	assert.Contains(t, rec.Body.String(), "broker boom")
	assert.Zero(t, events.Count())

	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "prior phase restored")
	assert.Equal(t, "working", got.Activity, "prior activity restored")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
	view := store.ComputeAgentDeletion(got, time.Now())
	require.NotNil(t, view)
	assert.NotNil(t, view.ExpiresAt, "an ordinary failure expires from view")
	assert.Nil(t, store.ComputeAgentDeletion(got, time.Now().Add(store.DeletionDisplayTTL+time.Second)), "and clears after the TTL")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = mustGetAgent(t, s, agent.ID)
	assert.Equal(t, store.DeletionStateNone, got.DeletionState, "stop clears the failed marker")
}

// Acceptance (b): the published failure status carries the restored prior
// phase and the failed view.
func TestAgentDeleteEngine_BrokerFailurePublishesFailedStatus(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
	agent := setupBrokerAgentInPhase(t, s, "rollback-ev", state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)

	evs := pub.snapshot()
	require.Len(t, evs, 2, "%+v", evs)
	assert.Equal(t, string(state.PhaseStopping), evs[0].phase)
	assert.Equal(t, store.DeletionStateDeleting, evs[0].deletion.State)
	assert.Equal(t, string(state.PhaseRunning), evs[1].phase)
	require.NotNil(t, evs[1].deletion)
	assert.Equal(t, store.DeletionStateFailed, evs[1].deletion.State)
	assert.Equal(t, store.DeletionCodeRuntimeError, evs[1].deletion.Code)
	assert.Zero(t, pub.count("deleted"))
}

// A broker 409 is a conflict, answered 409 with code conflict.
func TestAgentDeleteEngine_BrokerConflictIs409(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	disp.setFn(func(context.Context, *store.Agent) error {
		return &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"message":"ambiguous"}}`}
	})
	agent := setupBrokerAgentInPhase(t, s, "conflict", state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}

// Acceptance (p): with the noop publisher, a fast stub delete answers 204
// within the stub's latency.
func TestAgentDeleteEngine_FastDeleteAnswersPromptlyWithNoopPublisher(t *testing.T) {
	srv, s := testServer(t)
	events := &deleteCountingEventPublisher{}
	srv.events = events
	disp := &engineStubDispatcher{}
	disp.setFn(func(context.Context, *store.Agent) error { time.Sleep(50 * time.Millisecond); return nil })
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "fast", state.PhaseRunning)

	start := time.Now()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, 1, events.Count())
	assert.True(t, agentGone(t, s, agent.ID))
}

// --- (i): 202 ---

// Acceptance (i): with a blocking stub, DELETE answers 202 {agentId,
// deletion} at deleteSyncWait, and deleted follows when the stub returns.
// "Prefer: wait=N" can only lower the wait.
func TestAgentDeleteEngine_SlowDeleteAnswers202(t *testing.T) {
	setDeleteKnob(t, &deleteSyncWait, 300*time.Millisecond)
	srv, s, pub, disp := engineTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	disp.setFn(blockingDelete(entered, release, nil))
	agent := setupBrokerAgentInPhase(t, s, "slow", state.PhaseRunning)

	// Prefer: wait=60 must not raise the 300ms wait.
	res := waitDelete(t, deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, map[string]string{"Prefer": "wait=60"}), 5*time.Second)
	require.Equal(t, http.StatusAccepted, res.rec.Code, res.rec.Body.String())
	assert.Less(t, res.elapsed, 3*time.Second)
	var body agentDeleteAcceptedResponse
	require.NoError(t, json.Unmarshal(res.rec.Body.Bytes(), &body))
	assert.Equal(t, agent.ID, body.AgentID)
	require.NotNil(t, body.Deletion)
	assert.Equal(t, store.DeletionStateDeleting, body.Deletion.State)
	assert.Zero(t, pub.count("deleted"))

	close(release)
	require.Eventually(t, func() bool { return agentGone(t, s, agent.ID) }, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, 1, pub.count("deleted"))
}

func TestDeleteSyncWaitFor(t *testing.T) {
	setDeleteKnob(t, &deleteSyncWait, 20*time.Second)
	for _, tc := range []struct {
		prefer string
		want   time.Duration
	}{
		{"", 20 * time.Second},
		{"wait=5", 5 * time.Second},
		{"wait=60", 20 * time.Second},
		{"respond-async, wait=0", 0},
		{"wait=junk", 20 * time.Second},
	} {
		r := httptest.NewRequest(http.MethodDelete, "/", nil)
		if tc.prefer != "" {
			r.Header.Set("Prefer", tc.prefer)
		}
		assert.Equal(t, tc.want, deleteSyncWaitFor(r), tc.prefer)
	}
}

// --- (j), (g), (q): joiners ---

// watchJoin returns a channel closed when a request enters the join path
// (subscribed, first re-read undecided). It proves the request joined and
// lets a test release the owner only after that.
func watchJoin(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	old := joinAgentDeletionHook
	joinAgentDeletionHook = func(string) { once.Do(func() { close(ch) }) }
	t.Cleanup(func() { joinAgentDeletionHook = old })
	return ch
}

// Acceptance (j), (g): a second DELETE (plain or force) during a live delete
// joins it and gets the owner's outcome, never 409.
func TestAgentDeleteEngine_SecondDeleteJoins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownerErr error
		want     int
		query    string
	}{
		{"success", nil, http.StatusNoContent, ""},
		{"failure", errors.New("broker boom"), http.StatusBadGateway, ""},
		{"force joins success", nil, http.StatusNoContent, "?force=true"},
		{"force joins failure", errors.New("broker boom"), http.StatusBadGateway, "?force=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, pub, disp := engineTestServer(t)
			entered, release := make(chan struct{}), make(chan struct{})
			disp.setFn(blockingDelete(entered, release, tc.ownerErr))
			agent := setupBrokerAgentInPhase(t, s, "join", state.PhaseRunning)

			owner := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
			waitClosed(t, entered, 5*time.Second, "owner dispatch")
			joined := watchJoin(t)
			joiner := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID+tc.query, nil)
			waitClosed(t, joined, 5*time.Second, "joiner subscribed and re-read")
			close(release)

			o := waitDelete(t, owner, 5*time.Second)
			j := waitDelete(t, joiner, 5*time.Second)
			assert.Equal(t, tc.want, o.rec.Code, o.rec.Body.String())
			assert.Equal(t, tc.want, j.rec.Code, j.rec.Body.String())
			assert.Equal(t, 1, disp.callCount(), "the joiner must not dispatch")
			if tc.ownerErr == nil {
				assert.Equal(t, 1, pub.count("deleted"))
			}
		})
	}
}

// Acceptance (j): a joiner whose deadline passes answers 202.
func TestAgentDeleteEngine_JoinerAnswers202AtDeadline(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	disp.setFn(blockingDelete(entered, release, nil))
	agent := setupBrokerAgentInPhase(t, s, "join202", state.PhaseRunning)

	owner := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
	waitClosed(t, entered, 5*time.Second, "owner dispatch")
	j := waitDelete(t, deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, map[string]string{"Prefer": "wait=1"}), 5*time.Second)
	assert.Equal(t, http.StatusAccepted, j.rec.Code, j.rec.Body.String())
	// Let the owner finish before the test (and its DB) ends, so no engine
	// outlives the test.
	close(release)
	assert.Equal(t, http.StatusNoContent, waitDelete(t, owner, 5*time.Second).rec.Code)
}

// Acceptance (q): a joiner on a publisher that drops every event (noop:
// Subscribe returns a nil channel) still resolves through the 1s row poll.
func TestAgentDeleteEngine_JoinerResolvesByPollWithDroppingPublisher(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"deleted", nil, http.StatusNoContent},
		{"failed", errors.New("broker boom"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDeleteKnob(t, &deleteJoinPollInterval, 100*time.Millisecond)
			srv, s := testServer(t)
			srv.events = &deleteCountingEventPublisher{}
			disp := &engineStubDispatcher{}
			srv.SetDispatcher(disp)
			entered, release := make(chan struct{}), make(chan struct{})
			disp.setFn(blockingDelete(entered, release, tc.err))
			agent := setupBrokerAgentInPhase(t, s, "dropjoin", state.PhaseRunning)

			owner := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
			waitClosed(t, entered, 5*time.Second, "owner dispatch")
			joined := watchJoin(t)
			joiner := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
			waitClosed(t, joined, 5*time.Second, "joiner subscribed and re-read")
			close(release)
			assert.Equal(t, tc.want, waitDelete(t, owner, 5*time.Second).rec.Code)
			j := waitDelete(t, joiner, 5*time.Second)
			assert.Equal(t, tc.want, j.rec.Code, j.rec.Body.String())
		})
	}
}

// --- (s): row removed between load and claim ---

// Acceptance (s): a concurrent hard delete removes the row between the
// handler's load and its claim → 204.
func TestAgentDeleteEngine_RowGoneBeforeClaim(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "gone", state.PhaseRunning)
	req := ownerDeleteRequest(t, s, context.Background(), agent.ID)
	stale := mustGetAgent(t, s, agent.ID)
	require.NoError(t, s.DeleteAgent(context.Background(), agent.ID))

	rec := httptest.NewRecorder()
	srv.performAgentDelete(rec, req, stale)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Zero(t, disp.callCount())
}

// --- (d): request ctx canceled ---

// Acceptance (d): canceling the request mid-dispatch does not stop the
// delete: the engine runs detached and completes.
func TestAgentDeleteEngine_RequestCancelStillCompletes(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var dispatchCtxErr error
	var mu sync.Mutex
	disp.setFn(func(ctx context.Context, a *store.Agent) error {
		close(entered)
		<-release
		mu.Lock()
		dispatchCtxErr = ctx.Err()
		mu.Unlock()
		return nil
	})
	agent := setupBrokerAgentInPhase(t, s, "cancel", state.PhaseRunning)

	ctx, cancel := context.WithCancel(context.Background())
	req := ownerDeleteRequest(t, s, ctx, agent.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.performAgentDelete(httptest.NewRecorder(), req, mustGetAgent(t, s, agent.ID))
	}()
	waitClosed(t, entered, 5*time.Second, "dispatch")
	cancel()
	waitClosed(t, done, 5*time.Second, "handler return after cancel")
	close(release)

	require.Eventually(t, func() bool { return agentGone(t, s, agent.ID) }, 5*time.Second, 20*time.Millisecond)
	mu.Lock()
	assert.NoError(t, dispatchCtxErr, "the dispatch ctx is detached from the request")
	mu.Unlock()
	assert.Equal(t, 1, pub.count("deleted"))
}

// --- (v): panic ---

// Acceptance (v): an engine that panics mid-step → the requester gets 502
// abandoned promptly, and the row reads abandoned.
func TestAgentDeleteEngine_PanicAbandonsPromptly(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	disp.setFn(func(context.Context, *store.Agent) error { panic("boom") })
	agent := setupBrokerAgentInPhase(t, s, "panic", state.PhaseRunning)

	start := time.Now()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(start), 3*time.Second)
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeAbandoned, details["deletionCode"])

	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletionActive(time.Now()), "the lease was dropped at once")
	assert.Equal(t, store.DeletionCodeAbandoned, got.DeletionEffectiveCode(time.Now()))
	evs := pub.snapshot()
	require.NotEmpty(t, evs)
	last := evs[len(evs)-1]
	require.NotNil(t, last.deletion)
	assert.Equal(t, store.DeletionCodeAbandoned, last.deletion.Code)

	// A retry re-claims the abandoned row and completes.
	disp.setFn(nil)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, agent.ID))
}

// --- (g), (h): force on failed, re-claim of abandoned rows ---

// Acceptance (g): force on a failed row claims it and completes despite a
// broker error.
func TestAgentDeleteEngine_ForceOnFailedRow(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
	agent := setupBrokerAgentInPhase(t, s, "forcefail", state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, agent.ID))
	assert.Equal(t, 1, pub.count("deleted"))
}

// Acceptance (h): a lease-expired deleting row re-claims with dispatch and
// keeps the original DeletionPrior; a lease-expired finalizing row
// re-claims and skips the dispatch.
func TestAgentDeleteEngine_ReclaimAbandoned(t *testing.T) {
	seedAbandoned := func(t *testing.T, s store.Store, id, st string) {
		t.Helper()
		past := time.Now().Add(-time.Minute)
		stopping := string(state.PhaseStopping)
		prior := `{"phase":"running","activity":"working"}`
		n, err := s.UpdateAgentDeletion(context.Background(), id, store.DeletionPredicate{}, store.DeletionFields{
			State: &st, BumpClaim: true, LeaseAt: &past, StartedAt: &past, Prior: &prior, Phase: &stopping,
		})
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}

	t.Run("deleting keeps prior", func(t *testing.T) {
		srv, s, _, disp := engineTestServer(t)
		disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
		agent := setupBrokerAgentInPhase(t, s, "reclaim-del", state.PhaseRunning)
		seedAbandoned(t, s, agent.ID, store.DeletionStateDeleting)
		require.Equal(t, store.DeletionCodeAbandoned, mustGetAgent(t, s, agent.ID).DeletionEffectiveCode(time.Now()))

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assert.Equal(t, 1, disp.callCount(), "a deleting re-claim dispatches")
		got := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, `{"phase":"running","activity":"working"}`, got.DeletionPrior, "the original prior is kept")
		assert.Equal(t, string(state.PhaseRunning), got.Phase, "rollback restores the original prior, not stopping")
		assert.Equal(t, "working", got.Activity)
		assert.EqualValues(t, 2, got.DeletionClaim)
	})

	t.Run("finalizing skips dispatch", func(t *testing.T) {
		srv, s, pub, disp := engineTestServer(t)
		agent := setupBrokerAgentInPhase(t, s, "reclaim-fin", state.PhaseRunning)
		seedAbandoned(t, s, agent.ID, store.DeletionStateFinalizing)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Zero(t, disp.callCount(), "a finalizing re-claim skips the dispatch")
		assert.True(t, agentGone(t, s, agent.ID))
		assert.Equal(t, 1, pub.count("deleted"))
	})
}
