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

// Tests for wake-on-send in chat v2: a send to a suspended primary can ask
// the hub to offer a wake (offer_wake: 409 instead of a failed row) or to
// wake the agent and deliver the message as its first input (wake).
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatWakeFixture struct {
	srv   *Server
	s     store.Store
	proj  *store.Project
	topic string
	agent *store.Agent
	disp  *wakeTrackingDispatcher
}

// chatWakeSetup creates a project with an online broker, an agent in the
// given phase on that broker (owned by the dev user), and a topic whose
// default agent is that agent.
func chatWakeSetup(t *testing.T, phase string) *chatWakeFixture {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	broker := &store.RuntimeBroker{
		ID: tid("chat-wake-broker-" + phase), Name: "Chat Wake Broker",
		Slug: "chat-wake-broker-" + phase, Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		Status: store.BrokerStatusOnline,
	}))

	a := &store.Agent{
		ID: tid("chat-wake-" + phase), ProjectID: proj.ID, Name: "Sleepy",
		Slug: "chat-wake-" + phase, Phase: phase, RuntimeBrokerID: broker.ID,
		OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, a))

	topicID := tid("chat-wake-topic-" + phase)
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "chat-wake-" + phase,
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return &chatWakeFixture{srv: srv, s: s, proj: proj, topic: topicID, agent: a, disp: disp}
}

// memberWithoutLifecycle creates a project member who may message the
// fixture's agent (agent.message granted) but does not own it, so the
// lifecycle permission the start route requires is not held.
func (f *chatWakeFixture) memberWithoutLifecycle(t *testing.T) *store.User {
	t.Helper()
	ctx := t.Context()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	u := &store.User{
		ID: tid("chat-wake-member"), Email: "chat-wake-member@example.com",
		DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.s.CreateUser(ctx, u))
	msgAuthzAddProjectMember(t, f.s, u.ID, f.proj.ID, f.proj.Slug, store.GroupMemberRoleMember)
	msgAuthzGrantAgentMessage(t, f.s, u.ID, f.proj.ID)
	require.False(t, f.srv.agentLifecycleAllowed(ctx, userIdentityFor(u), f.agent),
		"fixture member must not hold the lifecycle permission")
	return u
}

func userIdentityFor(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

func (f *chatWakeFixture) path() string {
	return "/api/v1/chat/conversations/" + f.topic + "/messages"
}

func decodeWakeResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	return resp
}

// countThreadMessages returns how many rows the topic thread holds.
func (f *chatWakeFixture) countThreadMessages(t *testing.T) int {
	t.Helper()
	res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

// markReadySoon simulates the resumed harness reporting its first activity.
func (f *chatWakeFixture) markReadySoon() {
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = f.s.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseRunning), Activity: "idle",
		})
	}()
}

// offer_wake to a suspended primary the caller may wake answers 409 with
// canWake=true and persists nothing, so the client can ask the user.
func TestChatV2Wake_OfferWake_Suspended_Conflict(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, ErrCodeAgentNotRunning, errObj["code"])
	details, _ := errObj["details"].(map[string]any)
	require.NotNil(t, details)
	assert.Equal(t, true, details["canWake"])
	assert.Equal(t, "suspended", details["phase"])
	assert.Equal(t, f.agent.Slug, details["agentSlug"])

	assert.Equal(t, 0, f.countThreadMessages(t), "no row may be persisted")
	assert.Empty(t, f.disp.getStartCalls())
	assert.Empty(t, f.disp.getMessageCalls())
}

// offer_wake does not change a stopped primary: it is not wakeable, so the
// ordinary failed "Agent unreachable (stopped)" row is kept.
func TestChatV2Wake_OfferWake_Stopped_FailedRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, "Agent unreachable (stopped)", resp["dispatchFailureReason"])
	assert.Empty(t, f.disp.getStartCalls())
}

// offer_wake without the lifecycle permission keeps the non-wake error: a
// failed row, no 409, no resume.
func TestChatV2Wake_OfferWake_NoPermission_FailedRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	member := f.memberWithoutLifecycle(t)
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, "agent_unreachable", resp["dispatchFailureCode"])
	assert.Empty(t, f.disp.getStartCalls())
}

// wake resumes the suspended primary (continue=true), waits for readiness
// and then delivers the message as its first input.
func TestChatV2Wake_Wake_Suspended_ResumesThenDelivers(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.markReadySoon()
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "wake up please", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "dispatched", resp["dispatchState"])

	starts := f.disp.getStartCalls()
	require.Len(t, starts, 1, "the agent must be resumed exactly once")
	assert.True(t, starts[0].Continue, "wake must resume the previous session")
	msgs := f.disp.getMessageCalls()
	require.Len(t, msgs, 1)
	assert.Equal(t, f.agent.ID, msgs[0].AgentID)

	got, err := f.s.GetAgent(t.Context(), f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	m, err := f.s.GetMessage(t.Context(), resp["id"].(string))
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState)
}

// wake without the lifecycle permission is refused with 403 before any
// resume, and nothing is persisted.
func TestChatV2Wake_Wake_NoPermission_Forbidden(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	member := f.memberWithoutLifecycle(t)
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, f.disp.getStartCalls())
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// A failed resume returns the wake error and persists nothing, so the
// client keeps the draft.
func TestChatV2Wake_Wake_ResumeFails_NoRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.disp.startReturnErr = assert.AnError
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Failed to wake agent")
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// wake to a running primary is a no-op: no resume, ordinary delivery.
func TestChatV2Wake_Wake_Running_NoResume(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, f.disp.getStartCalls())
	assert.Len(t, f.disp.getMessageCalls(), 1)
}

// offer_wake from a caller without message permission is refused by the
// message authorization (403 message_denied) before any wake offer.
func TestChatV2Wake_OfferWake_NoMessagePermission_Denied(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	ctx := t.Context()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	u := &store.User{
		ID: tid("chat-wake-nomsg"), Email: "chat-wake-nomsg@example.com",
		DisplayName: "No Message", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.s.CreateUser(ctx, u))
	msgAuthzAddProjectMember(t, f.s, u.ID, f.proj.ID, f.proj.Slug, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, u, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	resp := decodeWakeResp(t, rec)
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, ErrCodeMessageDenied, errObj["code"])
	assert.NotContains(t, rec.Body.String(), "canWake")
	assert.Empty(t, f.disp.getStartCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// deadlineRecorder is a ResponseRecorder that supports SetWriteDeadline,
// as a real connection does, and records every deadline set.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines = append(r.deadlines, t)
	return nil
}

// ctxDeadlineDispatcher records the deadline of the resume dispatch ctx.
type ctxDeadlineDispatcher struct {
	*wakeTrackingDispatcher
	mu            sync.Mutex
	startDeadline time.Time
	startHasDL    bool
}

func (d *ctxDeadlineDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.mu.Lock()
	d.startDeadline, d.startHasDL = ctx.Deadline()
	d.mu.Unlock()
	return d.wakeTrackingDispatcher.DispatchAgentStart(ctx, agent, prompt, cont)
}

// A wake request gets a write deadline past the server-wide WriteTimeout
// that outlasts the bounded resume and delivery, and the resume itself is
// bounded, so the client always receives the outcome.
func TestChatV2Wake_Wake_ExtendsWriteDeadlineAndBoundsResume(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	disp := &ctxDeadlineDispatcher{wakeTrackingDispatcher: f.disp}
	f.srv.SetDispatcher(disp)
	f.markReadySoon()

	body, err := json.Marshal(map[string]any{"content": "hello", "wake": true})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, f.path(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// The extended deadline reached the connection through every
	// middleware wrapper, and ends after resume plus delivery.
	rec.mu.Lock()
	deadlines := append([]time.Time(nil), rec.deadlines...)
	rec.mu.Unlock()
	require.Len(t, deadlines, 1, "the wake path must set the write deadline once")
	assert.False(t, deadlines[0].Before(start.Add(chatWakeWriteBudget(1))))
	assert.Greater(t, chatWakeWriteBudget(1), chatWakeResumeBudget+chatWakeDeliveryBudget)
	assert.Greater(t, chatWakeWriteBudget(1), 60*time.Second, "must outlast the default WriteTimeout")

	disp.mu.Lock()
	defer disp.mu.Unlock()
	require.True(t, disp.startHasDL, "the resume dispatch must be bounded")
	assert.False(t, disp.startDeadline.After(time.Now().Add(chatWakeResumeBudget)))
}

// A plain send never touches the write deadline.
func TestChatV2Wake_PlainSend_LeavesWriteDeadline(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	body, err := json.Marshal(map[string]any{"content": "hello", "offer_wake": true})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, f.path(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, rec.deadlines)
}

// postRecorded posts a send through a deadline-recording writer.
func (f *chatWakeFixture) postRecorded(t *testing.T, payload map[string]any) *deadlineRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, f.path(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The wake write budget grows with each recipient's dispatch budget.
func TestChatWakeWriteBudget_ScalesWithRecipients(t *testing.T) {
	assert.Equal(t, chatWakeResumeBudget+chatWakeDeliveryBudget+chatWakeWriteSlack, chatWakeWriteBudget(1))
	assert.Equal(t, chatWakeWriteBudget(1), chatWakeWriteBudget(0))
	assert.Equal(t, chatWakeWriteBudget(1)+2*chatWakeDeliveryBudget, chatWakeWriteBudget(3))
}

// A wake send that also @mentions a secondary gets a deadline covering
// both recipients' dispatch budgets.
func TestChatV2Wake_Wake_DeadlineCoversMentionFanOut(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	second := &store.Agent{
		ID: tid("chat-wake-second"), ProjectID: f.proj.ID, Name: "Second",
		Slug: "chat-wake-second", Phase: string(state.PhaseRunning),
		RuntimeBrokerID: f.agent.RuntimeBrokerID, OwnerID: DevUserID, CreatedBy: DevUserID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.s.CreateAgent(t.Context(), second))
	f.markReadySoon()

	start := time.Now()
	rec := f.postRecorded(t, map[string]any{"content": "hi @chat-wake-second", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	require.Len(t, rec.deadlines, 1)
	assert.False(t, rec.deadlines[0].Before(start.Add(chatWakeWriteBudget(2))),
		"deadline %v must cover two dispatch budgets", rec.deadlines[0].Sub(start))
	assert.Len(t, f.disp.getMessageCalls(), 2, "primary and mention are both delivered")
}

// The woken message is dated at delivery, not at request time.
func TestChatV2Wake_Wake_DatesMessageAfterWake(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.markReadySoon()
	start := time.Now()
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	m, err := f.s.GetMessage(t.Context(), decodeWakeResp(t, rec)["id"].(string))
	require.NoError(t, err)
	// The readiness poll ticks every 500ms, so the wake takes at least that.
	assert.False(t, m.CreatedAt.Before(start.Add(400*time.Millisecond)),
		"created %v after request start", m.CreatedAt.Sub(start))
}

// A retry with the idempotency key of a send still running is told so
// (409 send_in_progress) and sends nothing.
func TestChatV2Wake_IdempotencyKeyInFlight_Conflict(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	_, begin := f.srv.chatIdempotency.Begin(DevUserID, "key-in-flight")
	require.Equal(t, IdempotencyNew, begin)

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true, "idempotency_key": "key-in-flight"})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeSendInProgress)
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// A retry after the send finished returns the same message and its
// stored dispatch outcome, without sending again.
func TestChatV2Wake_IdempotencyRetry_ReturnsOutcome(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.markReadySoon()
	payload := map[string]any{"content": "hello", "wake": true, "idempotency_key": "key-retry"}
	first := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	firstID := decodeWakeResp(t, first)["id"]

	retry := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusOK, retry.Code, "body=%s", retry.Body.String())
	resp := decodeWakeResp(t, retry)
	assert.Equal(t, firstID, resp["id"])
	assert.Equal(t, "dispatched", resp["dispatchState"])
	assert.Len(t, f.disp.getStartCalls(), 1)
	assert.Len(t, f.disp.getMessageCalls(), 1)
	assert.Equal(t, 1, f.countThreadMessages(t))
}

// A send that ends without a message (a failed wake) releases its key, so
// a retry may try again.
func TestChatV2Wake_IdempotencyKeyReleasedAfterFailedWake(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.disp.startReturnErr = assert.AnError
	payload := map[string]any{"content": "hello", "wake": true, "idempotency_key": "key-failed"}
	rec := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())

	_, begin := f.srv.chatIdempotency.Begin(DevUserID, "key-failed")
	assert.Equal(t, IdempotencyNew, begin, "the key must be free after a send without a message")
}

// A replay of a send whose dispatch failed reports that failure, not a
// delivery.
func TestChatV2Wake_IdempotencyReplay_ReportsFailedDispatch(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	f.disp.msgReturnErr = assert.AnError
	payload := map[string]any{"content": "hello", "idempotency_key": "key-failed-dispatch"}
	first := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	firstResp := decodeWakeResp(t, first)
	require.Equal(t, "failed", firstResp["dispatchState"])

	retry := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusOK, retry.Code, "body=%s", retry.Body.String())
	resp := decodeWakeResp(t, retry)
	assert.Equal(t, firstResp["id"], resp["id"])
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, firstResp["dispatchFailureReason"], resp["dispatchFailureReason"])
	assert.Len(t, f.disp.getMessageCalls(), 1, "the replay must not dispatch again")
}

// replayProbeDispatcher replays the send from inside dispatch, i.e. while
// the row is persisted but its dispatch has not ended, then fails.
type replayProbeDispatcher struct {
	*wakeTrackingDispatcher
	t          *testing.T
	f          *chatWakeFixture
	payload    map[string]any
	replayCode int
	replayBody string
}

func (d *replayProbeDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	if d.replayCode == 0 {
		rec := doRequest(d.t, d.f.srv, http.MethodPost, d.f.path(), d.payload)
		d.replayCode, d.replayBody = rec.Code, rec.Body.String()
	}
	_ = d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm)
	return assert.AnError
}

// A replay while the original's dispatch is still running is told the
// send is in progress (not a premature "dispatched"); once the dispatch
// has failed, a replay reports that failure with the same reason.
func TestChatV2Wake_IdempotencyReplayDuringFailingDispatch(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	payload := map[string]any{"content": "hello", "idempotency_key": "key-race"}
	disp := &replayProbeDispatcher{wakeTrackingDispatcher: f.disp, t: t, f: f, payload: payload}
	f.srv.SetDispatcher(disp)

	first := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	firstResp := decodeWakeResp(t, first)
	require.Equal(t, "failed", firstResp["dispatchState"])

	assert.Equal(t, http.StatusConflict, disp.replayCode, "replay during dispatch: %s", disp.replayBody)
	assert.Contains(t, disp.replayBody, ErrCodeSendInProgress)

	after := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusOK, after.Code, "body=%s", after.Body.String())
	resp := decodeWakeResp(t, after)
	assert.Equal(t, firstResp["id"], resp["id"])
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, firstResp["dispatchFailureReason"], resp["dispatchFailureReason"])
	assert.Len(t, f.disp.getMessageCalls(), 1, "neither replay may dispatch")
}

// The message is noted against the key as soon as the row is stored, so
// ending the send without Record (as a panic mid-dispatch would, via the
// deferred Finish) makes the key done rather than releasing it.
func TestChatV2Wake_IdempotencyPersistedBeforeDispatch(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	var persistedID string
	disp := &persistProbeDispatcher{wakeTrackingDispatcher: f.disp, probe: func() {
		f.srv.chatIdempotency.mu.Lock()
		defer f.srv.chatIdempotency.mu.Unlock()
		entry := f.srv.chatIdempotency.entries[idempotencyCacheKey{senderID: DevUserID, idempotencyKey: "key-early"}]
		persistedID = entry.messageID
		assert.False(t, entry.done, "the key must stay in flight during dispatch")
	}}
	f.srv.SetDispatcher(disp)
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "idempotency_key": "key-early"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, decodeWakeResp(t, rec)["id"], persistedID, "message noted before dispatch")
}

// persistProbeDispatcher runs probe from inside dispatch.
type persistProbeDispatcher struct {
	*wakeTrackingDispatcher
	probe func()
}

func (d *persistProbeDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	d.probe()
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm)
}
