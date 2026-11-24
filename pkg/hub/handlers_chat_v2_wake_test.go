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
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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

// panicDispatcher panics on its first message dispatch, after the row is
// stored, then behaves normally.
type panicDispatcher struct {
	*wakeTrackingDispatcher
	mu       sync.Mutex
	panicked bool
}

func (d *panicDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	d.mu.Lock()
	first := !d.panicked
	d.panicked = true
	d.mu.Unlock()
	if first {
		panic("dispatch blew up")
	}
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm)
}

// A send that panics during dispatch, after its row is stored, leaves the
// row failed (not the optimistic "dispatched") and its idempotency key done
// via the deferred Finish: a replay answers 200 with the same message and
// that failed state, and dispatches nothing.
func TestChatV2Wake_PanicDuringDispatch_ReplayReportsInterrupted(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	disp := &panicDispatcher{wakeTrackingDispatcher: f.disp}
	f.srv.SetDispatcher(disp)
	payload := map[string]any{"content": "hello", "idempotency_key": "key-panic"}

	func() {
		defer func() { _ = recover() }() // the handler may re-panic; a middleware may also recover
		_ = doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	}()
	require.True(t, disp.panicked, "the dispatcher must have run")
	require.Equal(t, 1, f.countThreadMessages(t), "the row was stored before dispatch")

	retry := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusOK, retry.Code, "body=%s", retry.Body.String())
	resp := decodeWakeResp(t, retry)

	res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, res.Items[0].ID, resp["id"], "the replay names the stored message")
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, chatSendInterruptedReason, resp["dispatchFailureReason"])
	assert.Empty(t, f.disp.getMessageCalls(), "the replay must not dispatch")
	assert.Equal(t, 1, f.countThreadMessages(t))
}

// panickingReplyStore panics when a reply link is stored, i.e. after the
// message row is persisted but before any primary dispatch.
type panickingReplyStore struct {
	WebChatStore
}

func (panickingReplyStore) SetMessageReplyTo(context.Context, string, string) error {
	panic("reply link blew up")
}

// A panic after persistence on a primary the gates already settled keeps
// the gate's state: the interrupted mark only replaces the optimistic
// "dispatched". The deferred case is the one that needs the guard (the
// store already refuses to overwrite a failed row).
func TestChatV2Wake_PanicAfterPersist_KeepsGateState(t *testing.T) {
	cases := []struct {
		name       string
		phase      string
		reincState string
		wantState  string
		wantReason string
	}{
		{name: "deferred (reincarnating)", phase: string(state.PhaseRunning),
			reincState: store.ReincarnationStatePending, wantState: store.MessageDispatchDeferred},
		{name: "failed (unreachable)", phase: string(state.PhaseSuspended),
			wantState: store.MessageDispatchFailed, wantReason: "Agent unreachable (suspended)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := chatWakeSetup(t, tc.phase)
			if tc.reincState != "" {
				f.agent.ReincarnationState = tc.reincState
				require.NoError(t, f.s.UpdateAgent(t.Context(), f.agent))
			}
			f.srv.mu.RLock()
			wcs := f.srv.webChatStore
			f.srv.mu.RUnlock()
			f.srv.SetWebChatStore(panickingReplyStore{WebChatStore: wcs})

			// A human-sent message of this topic: a reply to it keeps the
			// default agent as primary but still stores the reply link,
			// which panics.
			replyTarget := tid("chat-wake-reply-target-" + tc.name)
			require.NoError(t, f.s.CreateMessage(t.Context(), &store.Message{
				ID: replyTarget, ProjectID: f.proj.ID, Sender: "user:dev", SenderID: DevUserID, Recipient: "thread:" + f.topic,
				Msg: "earlier", Type: "instruction", Channel: "web", ThreadID: f.topic,
				CreatedAt: time.Now().Add(-time.Minute).UTC(),
			}))

			func() {
				defer func() { _ = recover() }()
				_ = doRequest(t, f.srv, http.MethodPost, f.path(),
					map[string]any{"content": "hello", "reply_to_id": replyTarget})
			}()

			res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, res.Items, 2, "the row was stored before the panic")
			m := res.Items[0]
			require.NotEqual(t, replyTarget, m.ID, "newest row first")
			assert.Equal(t, tc.wantState, m.DispatchState, "the gate's state must survive the panic")
			if tc.wantReason != "" {
				require.NotNil(t, m.DispatchFailureReason)
				assert.Equal(t, tc.wantReason, *m.DispatchFailureReason)
			} else if m.DispatchFailureReason != nil {
				assert.Empty(t, *m.DispatchFailureReason)
			}
			assert.Empty(t, f.disp.getMessageCalls())
		})
	}
}

// cancelDuringWakeDispatcher cancels the request context when the resume
// dispatch starts, as a dropped client connection would, and replays the
// send from inside the resume and the message dispatch.
type cancelDuringWakeDispatcher struct {
	*wakeTrackingDispatcher
	t           *testing.T
	f           *chatWakeFixture
	payload     map[string]any
	cancel      context.CancelFunc
	mu          sync.Mutex
	replayCodes []int
	replayBody  []string
}

func (d *cancelDuringWakeDispatcher) replay() {
	rec := doRequest(d.t, d.f.srv, http.MethodPost, d.f.path(), d.payload)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replayCodes = append(d.replayCodes, rec.Code)
	d.replayBody = append(d.replayBody, rec.Body.String())
}

func (d *cancelDuringWakeDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.cancel()
	d.replay()
	return d.wakeTrackingDispatcher.DispatchAgentStart(ctx, agent, prompt, cont)
}

func (d *cancelDuringWakeDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	d.replay()
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm)
}

// A client that drops its connection during a wake does not abort it: the
// wake, persist and dispatch run to their end while a retry is told the
// send is in progress, and a retry afterwards gets the final outcome, with
// one wake and one dispatch in all.
func TestChatV2Wake_ClientCancelDuringWake_ReplayGetsOutcome(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	payload := map[string]any{"content": "hello", "wake": true, "idempotency_key": "key-cancel"}
	reqCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	disp := &cancelDuringWakeDispatcher{
		wakeTrackingDispatcher: f.disp, t: t, f: f, payload: payload, cancel: cancel,
	}
	f.srv.SetDispatcher(disp)
	f.markReadySoon()

	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, f.path(), bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	f.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	require.Error(t, reqCtx.Err(), "the request context was cancelled during the wake")

	disp.mu.Lock()
	codes, bodies := append([]int(nil), disp.replayCodes...), append([]string(nil), disp.replayBody...)
	disp.mu.Unlock()
	require.Len(t, codes, 2, "replays from the resume and the message dispatch")
	for i, code := range codes {
		assert.Equal(t, http.StatusConflict, code, "replay %d: %s", i, bodies[i])
		assert.Contains(t, bodies[i], ErrCodeSendInProgress)
	}

	retry := doRequest(t, f.srv, http.MethodPost, f.path(), payload)
	require.Equal(t, http.StatusOK, retry.Code, "body=%s", retry.Body.String())
	resp := decodeWakeResp(t, retry)
	assert.Equal(t, "dispatched", resp["dispatchState"])
	res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, res.Items[0].ID, resp["id"])
	assert.Equal(t, store.MessageDispatchDispatched, res.Items[0].DispatchState)
	assert.Len(t, f.disp.getStartCalls(), 1, "one wake")
	assert.Len(t, f.disp.getMessageCalls(), 1, "one dispatch")
}

// stallCreateMessageStore blocks CreateMessage until its context ends, or
// for at most stallFor, simulating a store call that hangs.
type stallCreateMessageStore struct {
	store.Store
	stallFor time.Duration
}

func (s *stallCreateMessageStore) CreateMessage(ctx context.Context, m *store.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.stallFor):
		return context.DeadlineExceeded
	}
}

// A store call that stalls after the wake ends the send at the overall
// deadline with a failure, and releases the idempotency key.
func TestChatV2Wake_StalledStoreAfterWake_EndsAtDeadline(t *testing.T) {
	budget := 3 * time.Second
	orig := chatWakeSendBudget
	chatWakeSendBudget = func(int) time.Duration { return budget }
	t.Cleanup(func() { chatWakeSendBudget = orig })

	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.markReadySoon()
	f.srv.store = &stallCreateMessageStore{Store: f.s, stallFor: 20 * time.Second}

	start := time.Now()
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true, "idempotency_key": "key-stall"})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	assert.Less(t, elapsed, budget+5*time.Second, "send must end at the overall deadline")
	assert.Len(t, f.disp.getStartCalls(), 1, "the wake ran before the stall")
	assert.Empty(t, f.disp.getMessageCalls())

	_, begin := f.srv.chatIdempotency.Begin(DevUserID, "key-stall")
	assert.Equal(t, IdempotencyNew, begin, "the key must be released after the failed send")
}

// A chat send with wake and interrupt to a suspended agent that is still
// initializing when the wake wait ends is delivered as an ordinary
// (buffered) message; an agent that reports working keeps the interrupt.
func TestChatV2Interrupt_WakeInitializingIsBuffered(t *testing.T) {
	orig := wakeReadyTimeout
	wakeReadyTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { wakeReadyTimeout = orig })

	for _, tc := range []struct {
		activity      string
		wantInterrupt bool
	}{
		{"initializing", false},
		{"working", true},
	} {
		t.Run(tc.activity, func(t *testing.T) {
			f := chatWakeSetup(t, string(state.PhaseSuspended))
			go func() {
				time.Sleep(200 * time.Millisecond)
				_ = f.s.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
					Phase: string(state.PhaseRunning), Activity: tc.activity,
				})
			}()
			rec := doRequest(t, f.srv, http.MethodPost, f.path(),
				map[string]any{"content": "stop and look", "wake": true, "interrupt": true})
			require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

			msgs := f.disp.getMessageCalls()
			require.Len(t, msgs, 1, "the message is delivered")
			assert.Equal(t, tc.wantInterrupt, msgs[0].Interrupt)
		})
	}
}

// --- Auto-resume progress events (ptone/scion#9015, PR 2) ---

type progressTimeline struct {
	mu     sync.Mutex
	events []string
}

func (p *progressTimeline) add(entry string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, entry)
}

func (p *progressTimeline) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

type progressRecordingPublisher struct {
	EventPublisher
	timeline *progressTimeline
	progress []struct {
		userID string
		evt    ChatSendProgressEvent
	}
	mu sync.Mutex
}

func (p *progressRecordingPublisher) PublishChatSendProgress(ctx context.Context, userID string, evt ChatSendProgressEvent) {
	p.mu.Lock()
	p.progress = append(p.progress, struct {
		userID string
		evt    ChatSendProgressEvent
	}{userID: userID, evt: evt})
	p.mu.Unlock()
	p.timeline.add("progress:" + evt.Stage)
	p.EventPublisher.PublishChatSendProgress(ctx, userID, evt)
}

func (p *progressRecordingPublisher) PublishUserMessage(ctx context.Context, msg *store.Message, attachments []AttachmentRef, artifactRefs []artifacts.MessageRef, opts ...ChatEventOption) {
	p.timeline.add("publish:" + msg.DispatchState)
	p.EventPublisher.PublishUserMessage(ctx, msg, attachments, artifactRefs, opts...)
}

type progressTimelineDispatcher struct {
	*wakeTrackingDispatcher
	timeline          *progressTimeline
	onFirstMessageErr func(agent *store.Agent) error
	firstMsgDone      bool
	mu                sync.Mutex
}

func (d *progressTimelineDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.timeline.add("broker:start")
	return d.wakeTrackingDispatcher.DispatchAgentStart(ctx, agent, prompt, cont)
}

func (d *progressTimelineDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, msg string, interrupt bool, structured *messages.StructuredMessage) error {
	d.mu.Lock()
	first := !d.firstMsgDone
	d.firstMsgDone = true
	hook := d.onFirstMessageErr
	d.mu.Unlock()
	if first && hook != nil {
		d.timeline.add("broker:message-idle-suspending")
		if err := hook(agent); err != nil {
			return err
		}
	}
	d.timeline.add("broker:message")
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, msg, interrupt, structured)
}

func enableIdleSuspendForWakeTest(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.IdleSuspend {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func suspendAgentWithReason(t *testing.T, s store.Store, agent *store.Agent, reason state.SuspendReason) {
	t.Helper()
	require.NoError(t, s.UpdateAgentStatus(t.Context(), agent.ID, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(reason),
	}))
	refreshed, err := s.GetAgent(t.Context(), agent.ID)
	require.NoError(t, err)
	*agent = *refreshed
}

func TestChatV2AutoWake_EmitsResumingThenSendingProgressEvents(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)
	suspendAgentWithReason(t, f.s, f.agent, state.SuspendReasonIdle)

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&progressTimelineDispatcher{
		wakeTrackingDispatcher: f.disp,
		timeline:               tl,
	})
	f.markReadySoon()

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "wake and run", "idempotency_key": "idem-auto-1"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	assert.Equal(t, []string{
		"progress:resuming",
		"broker:start",
		"progress:sending",
		"publish:pending",
		"broker:message",
		"publish:dispatched",
	}, tl.snapshot(), "progress events and broker dispatch must follow strict order")

	require.Len(t, recPub.progress, 2)
	assert.Equal(t, DevUserID, recPub.progress[0].userID)
	assert.Equal(t, f.topic, recPub.progress[0].evt.ConversationKey)
	assert.Equal(t, "idem-auto-1", recPub.progress[0].evt.IdempotencyKey)
	assert.Equal(t, ChatSendProgressResuming, recPub.progress[0].evt.Stage)
	assert.Equal(t, ChatSendProgressSending, recPub.progress[1].evt.Stage)
}

func TestChatV2AutoWake_FailedWakeLeavesNoRowAndEmitsResuming(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)
	suspendAgentWithReason(t, f.s, f.agent, state.SuspendReasonIdle)

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.disp.startReturnErr = assert.AnError
	f.srv.SetDispatcher(&progressTimelineDispatcher{
		wakeTrackingDispatcher: f.disp,
		timeline:               tl,
	})

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "should not persist", "idempotency_key": "idem-fail-1"})
	require.GreaterOrEqual(t, rec.Code, 500, "body=%s", rec.Body.String())

	assert.Equal(t, []string{"progress:resuming", "broker:start"}, tl.snapshot(),
		"failed wake must emit resuming, never sending or publish")
	assert.Equal(t, 0, f.countThreadMessages(t), "failed wake must leave no message row behind")
}

func TestChatV2AutoWake_RunningAgentEmitsNoResumingEvent(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&progressTimelineDispatcher{
		wakeTrackingDispatcher: f.disp,
		timeline:               tl,
	})

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "ordinary send", "idempotency_key": "idem-run-1"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	assert.Empty(t, recPub.progress, "running agent must not emit send-progress events")
	assert.Equal(t, []string{"publish:pending", "broker:message", "publish:dispatched"}, tl.snapshot(),
		"pre-dispatch pending publish must precede broker call and settled publish must follow broker acceptance")
}

func TestChatV2AutoWake_SendProgressScopedToSenderOnly(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)
	suspendAgentWithReason(t, f.s, f.agent, state.SuspendReasonIdle)

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)
	f.markReadySoon()

	senderCh, unsubSender := pub.Subscribe("user." + DevUserID + ".send-progress")
	defer unsubSender()
	senderChatCh, unsubSenderChat := pub.Subscribe("user." + DevUserID + ".chat.>")
	defer unsubSenderChat()
	projectCh, unsubProject := pub.Subscribe("project." + f.proj.ID + ".>")
	defer unsubProject()
	otherCh, unsubOther := pub.Subscribe("user.other-user.>")
	defer unsubOther()

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "private progress", "idempotency_key": "idem-scope-1"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var senderStages []string
	deadline := time.After(300 * time.Millisecond)
collectSender:
	for len(senderStages) < 2 {
		select {
		case evt := <-senderCh:
			var payload ChatSendProgressEvent
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			senderStages = append(senderStages, payload.Stage)
		case <-deadline:
			break collectSender
		}
	}
	assert.Equal(t, []string{ChatSendProgressResuming, ChatSendProgressSending}, senderStages)

	// Confirm no send-progress leaked onto user.<self>.chat.> (old web),
	// project.>, or another user's stream.
	drain := time.After(100 * time.Millisecond)
drainLoop:
	for {
		select {
		case evt := <-senderChatCh:
			assert.NotContains(t, evt.Subject, "send-progress",
				"send-progress must not publish under user.<id>.chat.>")
		case evt := <-projectCh:
			assert.NotContains(t, evt.Subject, "send-progress")
		case evt := <-otherCh:
			t.Fatalf("unexpected event on other user's stream: %s", evt.Subject)
		case <-drain:
			break drainLoop
		}
	}
}

func TestChatV2AutoWake_MidDispatchSyncAutoWakeEmitsResumingThenSending(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&progressTimelineDispatcher{
		wakeTrackingDispatcher: f.disp,
		timeline:               tl,
		onFirstMessageErr: func(agent *store.Agent) error {
			// Simulate the agent suspending between lookup and dispatch.
			suspendAgentWithReason(t, f.s, agent, state.SuspendReasonIdle)
			f.markReadySoon()
			return &errAgentIdleSuspending{agent: agent}
		},
	})

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "mid-dispatch wake", "idempotency_key": "idem-mid-1"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	assert.Equal(t, []string{
		"publish:pending",
		"broker:message-idle-suspending",
		"progress:resuming",
		"broker:start",
		"progress:sending",
		"broker:message",
		"publish:dispatched",
	}, tl.snapshot(), "mid-dispatch syncAutoWakeHook must emit resuming -> sending before retry and settled publish")
}

// TestChatV2Wake_ExplicitWakeEmitsNoProgressEvents pins L3 (kills audit
// survivor m5): an explicit wake (wake:true on a user-suspended agent) must
// never emit auto-resume send-progress events, since the web shows "Waking".
func TestChatV2Wake_ExplicitWakeEmitsNoProgressEvents(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	enableIdleSuspendForWakeTest(t, f.srv)
	suspendAgentWithReason(t, f.s, f.agent, state.SuspendReasonUser)

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&progressTimelineDispatcher{
		wakeTrackingDispatcher: f.disp,
		timeline:               tl,
	})
	f.markReadySoon()

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "explicit wake", "wake": true, "idempotency_key": "idem-explicit-wake-1"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	assert.Empty(t, recPub.progress, "explicit wake must emit no send-progress events")
	assert.Equal(t, []string{"broker:start", "publish:pending", "broker:message", "publish:dispatched"}, tl.snapshot())
}

type hookDispatchDispatcher struct {
	*wakeTrackingDispatcher
	onDispatch func(ctx context.Context, agent *store.Agent) error
}

func (d *hookDispatchDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, msg string, interrupt bool, structured *messages.StructuredMessage) error {
	if d.onDispatch != nil {
		if err := d.onDispatch(ctx, agent); err != nil {
			return err
		}
	}
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, msg, interrupt, structured)
}

// TestChatV2_SettledPublishReReadsDeliveredState pins M1: if the broker
// confirms delivery synchronously while DispatchAgentMessage is still running,
// the post-dispatch settled publish must re-read the store and publish
// "delivered" rather than a stale "dispatched" after "delivered".
func TestChatV2_SettledPublishReReadsDeliveredState(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&hookDispatchDispatcher{
		wakeTrackingDispatcher: f.disp,
		onDispatch: func(ctx context.Context, agent *store.Agent) error {
			msgID := dispatchMessageIDFromContext(ctx)
			require.NotEmpty(t, msgID)
			ok := f.srv.applyBrokerMessageDeliveryOutcome(ctx, agent.RuntimeBrokerID, messageDeliveryConfirmation{
				MessageID: msgID,
				AgentID:   agent.ID,
				ProjectID: agent.ProjectID,
			})
			require.True(t, ok, "broker delivery confirmation should mark row delivered")
			return nil
		},
	})

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "fast broker delivery", "idempotency_key": "idem-m1-delivered"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	assert.Equal(t, store.MessageDispatchDelivered, resp["dispatchState"])

	events := tl.snapshot()
	require.NotEmpty(t, events)
	assert.Equal(t, "publish:delivered", events[len(events)-1], "final publish must be delivered, got %v", events)
	assert.NotContains(t, events, "publish:dispatched", "stale dispatched must never be published after delivered: %v", events)
}

// TestChatV2_SettledPublishReReadsFailedState pins M1 (failed-wins): if the
// store already transitioned the row to "failed" before the settled publish
// runs, the post-dispatch publish must publish "failed", never a stale
// "dispatched".
func TestChatV2_SettledPublishReReadsFailedState(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))

	tl := &progressTimeline{}
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	recPub := &progressRecordingPublisher{EventPublisher: pub, timeline: tl}
	f.srv.SetEventPublisher(recPub)
	f.srv.SetDispatcher(&hookDispatchDispatcher{
		wakeTrackingDispatcher: f.disp,
		onDispatch: func(ctx context.Context, _ *store.Agent) error {
			msgID := dispatchMessageIDFromContext(ctx)
			require.NotEmpty(t, msgID)
			require.NoError(t, f.srv.markFailed(ctx, msgID, "session closed"))
			return nil
		},
	})

	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "fast async failure", "idempotency_key": "idem-m1-failed"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	assert.Equal(t, store.MessageDispatchFailed, resp["dispatchState"])

	events := tl.snapshot()
	require.NotEmpty(t, events)
	assert.Equal(t, "publish:failed", events[len(events)-1], "final publish must be failed, got %v", events)
	assert.NotContains(t, events, "publish:dispatched", "stale dispatched must never be published after failed: %v", events)
}

// TestChatV2_AgentRoutedPublishesPendingAtPersistTimeForCoMembersAndOtherTabs
// pins M2: co-members and the sender's other tabs receive the message promptly
// at persist time (with dispatchState="pending") BEFORE the synchronous broker
// dispatch returns, and receive the settled state ("dispatched") once dispatch
// finishes.
func TestChatV2_AgentRoutedPublishesPendingAtPersistTimeForCoMembersAndOtherTabs(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	coMember := f.memberWithoutLifecycle(t)

	// Record co-member as an existing thread member so fan-out targets them.
	f.srv.recordThreadMembers(t.Context(), threadMembership{
		ProjectID: f.proj.ID,
		ThreadKey: f.topic,
		UserID:    coMember.ID,
	})

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)

	projCh, unsubProj := pub.Subscribe("project." + f.proj.ID + ".chat.message")
	defer unsubProj()
	memberCh, unsubMember := pub.Subscribe("user." + coMember.ID + ".chat.message")
	defer unsubMember()

	inDispatch := make(chan struct{})
	releaseDispatch := make(chan struct{})
	f.srv.SetDispatcher(&hookDispatchDispatcher{
		wakeTrackingDispatcher: f.disp,
		onDispatch: func(_ context.Context, _ *store.Agent) error {
			close(inDispatch)
			<-releaseDispatch
			return nil
		},
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doRequest(t, f.srv, http.MethodPost, f.path(),
			map[string]any{"content": "visible while dispatching", "idempotency_key": "idem-m2-prompt"})
	}()

	// Wait until broker dispatch is blocked in flight.
	select {
	case <-inDispatch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentMessage")
	}

	// Both the project chat stream (co-members / sender's other tabs) and the
	// off-project co-member fan-out stream must receive the pending event BEFORE
	// dispatch is released.
	readState := func(ch <-chan Event, label string) UserMessageEvent {
		t.Helper()
		select {
		case evt := <-ch:
			var payload UserMessageEvent
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			return payload
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s event", label)
			return UserMessageEvent{}
		}
	}

	projPending := readState(projCh, "project pre-dispatch pending")
	assert.Equal(t, store.MessageDispatchPending, projPending.DispatchState)
	assert.Equal(t, "visible while dispatching", projPending.Msg)

	memberPending := readState(memberCh, "co-member pre-dispatch pending")
	assert.Equal(t, store.MessageDispatchPending, memberPending.DispatchState)
	assert.Equal(t, projPending.ID, memberPending.ID)

	// Release broker dispatch and confirm both streams receive the settled event.
	close(releaseDispatch)
	rec := <-done
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	projSettled := readState(projCh, "project settled")
	assert.Equal(t, store.MessageDispatchDispatched, projSettled.DispatchState)
	assert.Equal(t, projPending.ID, projSettled.ID)

	memberSettled := readState(memberCh, "co-member settled")
	assert.Equal(t, store.MessageDispatchDispatched, memberSettled.DispatchState)
	assert.Equal(t, projPending.ID, memberSettled.ID)
}

// delayEnsureParticipantStore delays EnsureParticipant calls while armed so
// a test can deterministically exercise fast dispatch vs. thread-membership
// recording ordering without relying on scheduler luck.
type delayEnsureParticipantStore struct {
	store.Store
	fault       *storeFaultSwitch
	dispatchEnd <-chan struct{}
	postDelay   time.Duration
	maxWait     time.Duration
}

func (w *delayEnsureParticipantStore) EnsureParticipant(ctx context.Context, p *store.ConversationParticipant) error {
	if w.fault.Active() {
		select {
		case <-w.dispatchEnd:
			if w.postDelay > 0 {
				time.Sleep(w.postDelay)
			}
		case <-time.After(w.maxWait):
		}
	}
	return w.Store.EnsureParticipant(ctx, p)
}

// TestChatV2_AgentRoutedFastDispatch_NewlyMentionedMemberReceivesPendingThenSettled
// pins L1 (ptone/scion#9015): when an agent-routed send @mentions a project
// member who is not yet a thread participant and broker dispatch finishes
// rapidly, the newly mentioned member must deterministically receive the
// pending copy followed by the settled ("dispatched") copy on
// user.<id>.chat.message (never only pending, and never settled before
// pending).
func TestChatV2_AgentRoutedFastDispatch_NewlyMentionedMemberReceivesPendingThenSettled(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))

	dispatchEnd := make(chan struct{})
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *delayEnsureParticipantStore {
		return &delayEnsureParticipantStore{
			Store:       inner,
			fault:       sw,
			dispatchEnd: dispatchEnd,
			postDelay:   30 * time.Millisecond,
			maxWait:     150 * time.Millisecond,
		}
	})

	// Create a project member who is NOT yet a participant of f.topic.
	mentioned := f.memberWithoutLifecycle(t)
	topic, err := f.srv.webChatStore.GetTopic(t.Context(), f.topic)
	require.NoError(t, err)
	require.False(t, isUserParticipant(t, f.s, topic.ConversationID, mentioned.ID),
		"mentioned user must not be a thread participant before the send")

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)

	memberCh, unsubMember := pub.Subscribe("user." + mentioned.ID + ".chat.message")
	defer unsubMember()

	var closeOnce sync.Once
	f.srv.SetDispatcher(&hookDispatchDispatcher{
		wakeTrackingDispatcher: f.disp,
		onDispatch: func(_ context.Context, _ *store.Agent) error {
			closeOnce.Do(func() { close(dispatchEnd) })
			return nil
		},
	})

	fault.Arm()
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "fast dispatch hello @Member", "idempotency_key": "idem-l1-fast"})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	sentID := decodeWakeResp(t, rec)["id"].(string)

	readMemberEvent := func(label string) UserMessageEvent {
		t.Helper()
		select {
		case evt := <-memberCh:
			var payload UserMessageEvent
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			return payload
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s event on user.%s.chat.message", label, mentioned.ID)
			return UserMessageEvent{}
		}
	}

	first := readMemberEvent("pending")
	assert.Equal(t, sentID, first.ID)
	assert.Equal(t, store.MessageDispatchPending, first.DispatchState,
		"newly mentioned member must receive pending first")

	second := readMemberEvent("settled")
	assert.Equal(t, sentID, second.ID)
	assert.Equal(t, store.MessageDispatchDispatched, second.DispatchState,
		"newly mentioned member must receive settled dispatched copy after pending")

	assert.True(t, isUserParticipant(t, f.s, topic.ConversationID, mentioned.ID),
		"mentioned user must be recorded as a thread participant")
}

// TestChatV2_AgentRoutedPanicBetweenPublishes_PublishesFailedToProjectAndMembers
// pins the panic path of L1 (ptone/scion#9015): if a panic occurs between the
// pre-dispatch pending publish and the post-dispatch settled publish, both
// project subscribers and off-project thread members must receive the settled
// "failed" copy rather than staying stuck at "pending".
func TestChatV2_AgentRoutedPanicBetweenPublishes_PublishesFailedToProjectAndMembers(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	coMember := f.memberWithoutLifecycle(t)
	f.srv.recordThreadMembers(t.Context(), threadMembership{
		ProjectID: f.proj.ID,
		ThreadKey: f.topic,
		UserID:    coMember.ID,
	})

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)

	projCh, unsubProj := pub.Subscribe("project." + f.proj.ID + ".chat.message")
	defer unsubProj()
	memberCh, unsubMember := pub.Subscribe("user." + coMember.ID + ".chat.message")
	defer unsubMember()

	disp := &panicDispatcher{wakeTrackingDispatcher: f.disp}
	f.srv.SetDispatcher(disp)

	func() {
		defer func() { _ = recover() }()
		_ = doRequest(t, f.srv, http.MethodPost, f.path(),
			map[string]any{"content": "panic mid-dispatch", "idempotency_key": "idem-l1-panic"})
	}()
	require.True(t, disp.panicked, "dispatcher must have panicked")

	readEvent := func(ch <-chan Event, label string) UserMessageEvent {
		t.Helper()
		select {
		case evt := <-ch:
			var payload UserMessageEvent
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			return payload
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s event", label)
			return UserMessageEvent{}
		}
	}

	projPending := readEvent(projCh, "project pending")
	assert.Equal(t, store.MessageDispatchPending, projPending.DispatchState)

	projFailed := readEvent(projCh, "project failed after panic")
	assert.Equal(t, projPending.ID, projFailed.ID)
	assert.Equal(t, store.MessageDispatchFailed, projFailed.DispatchState)
	assert.Equal(t, chatSendInterruptedReason, projFailed.DispatchFailureReason)

	memberPending := readEvent(memberCh, "member pending")
	assert.Equal(t, projPending.ID, memberPending.ID)
	assert.Equal(t, store.MessageDispatchPending, memberPending.DispatchState)

	memberFailed := readEvent(memberCh, "member failed after panic")
	assert.Equal(t, projPending.ID, memberFailed.ID)
	assert.Equal(t, store.MessageDispatchFailed, memberFailed.DispatchState)
	assert.Equal(t, chatSendInterruptedReason, memberFailed.DispatchFailureReason)
}
