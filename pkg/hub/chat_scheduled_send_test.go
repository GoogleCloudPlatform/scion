//go:build !no_sqlite

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

package hub

// Tests for scheduled send in native web chat (ptone/scion#3666, phase 1:
// topics).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scheduledSendFixture is a chat-capable server with two ordinary users
// (bob schedules, alice is a second project reader), a topic whose default
// agent is running, and a recording dispatcher.
type scheduledSendFixture struct {
	srv        *Server
	store      store.Store
	wcs        WebChatStore
	sms        ScheduledMessageStore
	db         *sql.DB
	alice, bob *store.User
	project    *store.Project
	agent      *store.Agent
	topicID    string
	dispatcher *brokerMockDispatcher
}

func setScheduledSendExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.ChatScheduledSend, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

func newScheduledSendFixture(t *testing.T) *scheduledSendFixture {
	t.Helper()
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	// alice owns the project; bob is an ordinary project member.
	addProjectMemberWithRole(t, s, project, bob.ID, store.GroupMemberRoleMember)

	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // one shared in-memory database (see setupSendTest)
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := &store.Agent{
		ID:        tid("sched-agent"),
		ProjectID: project.ID,
		Name:      "sched-agent",
		Slug:      "sched-agent",
		Phase:     "running",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := tid("sched-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    project.ID,
		Name:         "sched-topic",
		CreatedBy:    alice.ID,
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agent.Slug,
	}))
	setTopicConversationID(t, db, s, topicID, project.ID)

	setScheduledSendExperiment(t, srv, true)

	return &scheduledSendFixture{
		srv: srv, store: s, wcs: wcs, sms: scheduledMessageStoreFrom(wcs), db: db,
		alice: alice, bob: bob, project: project, agent: agent, topicID: topicID,
		dispatcher: dispatcher,
	}
}

func (f *scheduledSendFixture) scheduledPath() string {
	return "/api/v1/chat/conversations/" + f.topicID + "/scheduled"
}

// schedule creates a scheduled message as user and returns it.
func (f *scheduledSendFixture) schedule(t *testing.T, user *store.User, content string, fireAt time.Time) scheduledMessageResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content":         content,
		"fire_at":         fireAt.UTC().Format(time.RFC3339Nano),
		"idempotency_key": "idem-" + content,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func (f *scheduledSendFixture) list(t *testing.T, user *store.User) []scheduledMessageResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.scheduledPath(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		ScheduledMessages []scheduledMessageResponse `json:"scheduledMessages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.ScheduledMessages
}

func (f *scheduledSendFixture) row(t *testing.T, user *store.User, id string) *ScheduledChatMessage {
	t.Helper()
	row, err := f.sms.GetScheduledMessage(context.Background(), user.ID, id)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row
}

// topicMessages returns every persisted message in the fixture topic.
func (f *scheduledSendFixture) topicMessages(t *testing.T) []store.Message {
	t.Helper()
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{ThreadID: f.topicID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return rows.Items
}

func (f *scheduledSendFixture) historyContains(t *testing.T, user *store.User, needle string) bool {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return strings.Contains(rec.Body.String(), needle)
}

func (f *scheduledSendFixture) searchContains(t *testing.T, user *store.User, needle string) bool {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/search?q="+url.QueryEscape(needle), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Results []json.RawMessage `json:"results"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return len(body.Results) > 0 || strings.Contains(rec.Body.String(), needle)
}

// collectEvents drains ch until no event arrives for a short while.
func collectEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, evt)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func scheduledEventActions(t *testing.T, evts []Event) []string {
	t.Helper()
	var actions []string
	for _, e := range evts {
		var se ChatScheduledEvent
		require.NoError(t, json.Unmarshal(e.Data, &se))
		actions = append(actions, se.Action)
	}
	return actions
}

// ---------------------------------------------------------------------------
// Experiment gate
// ---------------------------------------------------------------------------

func TestScheduledSend_ExperimentRegistered(t *testing.T) {
	exp, ok := experiments.Default().Lookup(experiments.ChatScheduledSend)
	require.True(t, ok)
	assert.False(t, exp.Default, "default off")
	assert.True(t, exp.HasLayer(experiments.LayerWeb))
	assert.True(t, exp.HasLayer(experiments.LayerServer))
	assert.Equal(t, "ptone/scion#3666", exp.Issue)
}

func TestScheduledSend_ExperimentOff_EndpointsNotFound(t *testing.T) {
	f := newScheduledSendFixture(t)
	setScheduledSendExperiment(t, f.srv, false)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content": "hi", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, f.scheduledPath(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.scheduledPath()+"/"+tid("any"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScheduledSend_ExperimentOff_PendingHeld(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "held while off", fireAt)

	setScheduledSendExperiment(t, f.srv, false)
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status, "held, not sent and not failed")
	assert.Empty(t, f.topicMessages(t))

	setScheduledSendExperiment(t, f.srv, true)
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, sm.ID).Status)
}

// ---------------------------------------------------------------------------
// Visibility
// ---------------------------------------------------------------------------

func TestScheduledSend_PendingVisibleOnlyToSender(t *testing.T) {
	f := newScheduledSendFixture(t)
	const secret = "pending-secret-text-zq81"
	sm := f.schedule(t, f.bob, secret, time.Now().Add(time.Hour))

	// The sender sees it.
	mine := f.list(t, f.bob)
	require.Len(t, mine, 1)
	assert.Equal(t, sm.ID, mine[0].ID)
	assert.Equal(t, secret, mine[0].Content)
	assert.Equal(t, ScheduledMessagePending, mine[0].Status)

	// A second user does not, through the list, history or search.
	assert.Empty(t, f.list(t, f.alice))
	assert.False(t, f.historyContains(t, f.alice, secret))
	assert.False(t, f.searchContains(t, f.alice, secret))
	// Nor does the sender through history or search: pending text is not a message.
	assert.False(t, f.historyContains(t, f.bob, secret))
	assert.False(t, f.searchContains(t, f.bob, secret))

	// Nothing reached the messages table or the agent.
	assert.Empty(t, f.topicMessages(t))
	assert.Empty(t, f.dispatcher.getMessages())

	// Another user cannot cancel it.
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodDelete, f.scheduledPath()+"/"+sm.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

func TestScheduledSend_FiresAsSenderToDefaultAgent(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	bobEvents, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsubBob()
	aliceEvents, unsubAlice := f.srv.events.Subscribe("user." + f.alice.ID + ".chat.>")
	defer unsubAlice()

	sm := f.schedule(t, f.bob, "scheduled hello", time.Now().Add(2*time.Minute))
	// Move the fire time close so the test can wait for it in real time
	// (the API requires at least 60 s of lead time).
	fireAt := time.Now().Add(1500 * time.Millisecond).UTC().Truncate(time.Second).Add(time.Second)
	_, err := f.db.ExecContext(ctx, `UPDATE webchat_scheduled_message SET fire_at = ? WHERE id = ?`,
		sqliteScheduledTime(fireAt), sm.ID)
	require.NoError(t, err)

	// Not due yet: nothing happens.
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(-time.Second)))
	assert.Empty(t, f.topicMessages(t))

	// Due: one sweep tick delivers it.
	time.Sleep(time.Until(fireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageSent, row.Status)
	require.NotEmpty(t, row.MessageID)

	msgs := f.topicMessages(t)
	require.Len(t, msgs, 1)
	msg := msgs[0]
	assert.Equal(t, row.MessageID, msg.ID)
	assert.Equal(t, "scheduled hello", msg.Msg)
	assert.Equal(t, f.bob.ID, msg.SenderID)
	assert.Equal(t, "user:"+f.bob.Email, msg.Sender, "attributed to the sender, not a system identity")
	assert.Equal(t, f.agent.ID, msg.AgentID)
	assert.False(t, msg.CreatedAt.Before(fireAt), "created at fire time, not at schedule time")

	dispatched := f.dispatcher.getMessages()
	require.Len(t, dispatched, 1)
	assert.Equal(t, f.agent.Slug, dispatched[0].agentSlug)
	assert.False(t, dispatched[0].interrupt, "a scheduled send is never an interrupt")

	// It is now ordinary history for everyone with access, and no longer pending.
	assert.True(t, f.historyContains(t, f.alice, "scheduled hello"))
	assert.Empty(t, f.list(t, f.bob))

	// Only the sender was told about the scheduled message.
	assert.Equal(t, []string{"created", "sending", "sent"}, scheduledEventActions(t, collectEvents(bobEvents)))
	for _, e := range collectEvents(aliceEvents) {
		assert.NotContains(t, e.Subject, "scheduled")
	}

	// A later sweep does not send it again.
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
	assert.Len(t, f.topicMessages(t), 1)
}

func TestScheduledSend_SweepTickWithinTenSeconds(t *testing.T) {
	assert.LessOrEqual(t, scheduledSendTick, 10*time.Second)
}

func TestScheduledSend_CancelBeforeFire_NeverSent(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "cancel me", fireAt)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.scheduledPath()+"/"+sm.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, ScheduledMessageCancelled, f.row(t, f.bob, sm.ID).Status)
	assert.Empty(t, f.list(t, f.bob))

	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
	assert.Empty(t, f.topicMessages(t))
	assert.Empty(t, f.dispatcher.getMessages())

	// Cancelling again is harmless.
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.scheduledPath()+"/"+sm.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestScheduledSend_CancelAfterClaim_Conflict(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	sm := f.schedule(t, f.bob, "claimed first", time.Now().Add(2*time.Minute))

	ok, err := f.sms.ClaimScheduledMessage(ctx, sm.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.scheduledPath()+"/"+sm.ID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, ScheduledMessageSending, f.row(t, f.bob, sm.ID).Status)
}

// Cancel and claim race on the same row: exactly one of them wins, every
// time.
func TestScheduledSend_CancelRacingClaim_ExactlyOneOutcome(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testCancelClaimRace(t, sms[0], sms[1], "race")
}

// testCancelClaimRace races a cancel through one store handle against a
// claim through another on the same row, many times: exactly one wins.
func testCancelClaimRace(t *testing.T, a, b ScheduledMessageStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		m := newTestScheduledRow(fmt.Sprintf("%s-%d", prefix, i), "user-1", time.Now().Add(-time.Second))
		_, _, err := a.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)

		start := make(chan struct{})
		results := make(chan bool, 2)
		go func() {
			<-start
			ok, err := a.CancelScheduledMessage(ctx, "user-1", m.ID, time.Now())
			assert.NoError(t, err)
			results <- ok
		}()
		go func() {
			<-start
			ok, err := b.ClaimScheduledMessage(ctx, m.ID, time.Now())
			assert.NoError(t, err)
			results <- ok
		}()
		close(start)
		r1, r2 := <-results, <-results
		assert.True(t, r1 != r2, "exactly one of cancel and claim must win (iteration %d)", i)
	}
}

// Two hub replicas on one database: concurrent sweeps through two store
// handles claim every due row exactly once. SQLite stands in for Postgres
// here (no Postgres in the local environment); the claim is the same
// single conditional UPDATE on both dialects.
func TestScheduledSend_TwoReplicas_EachRowClaimedOnce(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	ctx := context.Background()
	const n = 40
	for i := 0; i < n; i++ {
		_, _, err := sms[0].CreateScheduledMessage(ctx, newTestScheduledRow(fmt.Sprintf("rep-%d", i), "user-1", time.Now().Add(-time.Second)))
		require.NoError(t, err)
	}
	claims := make(chan string, 2*n)
	done := make(chan struct{})
	for r := 0; r < 2; r++ {
		go func(st ScheduledMessageStore) {
			defer func() { done <- struct{}{} }()
			due, err := st.ListDueScheduledMessages(ctx, time.Now(), 100)
			if !assert.NoError(t, err) {
				return
			}
			for _, m := range due {
				ok, err := st.ClaimScheduledMessage(ctx, m.ID, time.Now())
				assert.NoError(t, err)
				if ok {
					claims <- m.ID
				}
			}
		}(sms[r])
	}
	<-done
	<-done
	close(claims)
	seen := map[string]int{}
	for id := range claims {
		seen[id]++
	}
	assert.Len(t, seen, n, "every row claimed")
	for id, c := range seen {
		assert.Equal(t, 1, c, "row %s claimed more than once", id)
	}
}

// Two sweeps of the same due message at once deliver it once.
func TestScheduledSend_ConcurrentSweeps_OneDelivery(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	f.schedule(t, f.bob, "only once", fireAt)

	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)) }()
	}
	total := <-results + <-results
	assert.Equal(t, 1, total)
	assert.Len(t, f.topicMessages(t), 1)
	assert.Len(t, f.dispatcher.getMessages(), 1)
}

// ---------------------------------------------------------------------------
// Fire-time checks
// ---------------------------------------------------------------------------

func TestScheduledSend_SenderLosesProjectRead_FailsNoAccess(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "after losing access", fireAt)

	// bob is removed from the project after scheduling.
	membersGroup, err := f.store.GetGroupBySlug(ctx, "project:"+f.project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, f.bob.ID))
	_, err = f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.bob.ID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, f.scheduledPath(), nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "bob has lost read access")

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.topicMessages(t), "nothing posted")
	assert.Empty(t, f.dispatcher.getMessages())
}

func TestScheduledSend_SenderSuspended_FailsSenderInactive(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "suspended sender", fireAt)

	u, err := f.store.GetUser(ctx, f.bob.ID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, u))

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureSenderInactive, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

func TestScheduledSend_TopicDeleted_FailsConversationGone(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "topic gone", fireAt)

	_, err := f.db.ExecContext(ctx, `UPDATE webchat_topic SET deleted_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), f.topicID)
	require.NoError(t, err)

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureConversationGone, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

// The sender may not message the topic's current default agent at fire
// time: the message fails with no_access and nothing is posted.
func TestScheduledSend_AgentMessageDenied_FailsNoAccess(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "to alice's agent", fireAt)

	// The default agent changes to one bob may not message.
	other := &store.Agent{
		ID: tid("sched-alice-agent"), ProjectID: f.project.ID, Name: "alice-agent", Slug: "alice-agent",
		Phase: "running", OwnerID: f.alice.ID, CreatedBy: f.alice.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, other))
	_, err := f.db.ExecContext(ctx, `UPDATE webchat_topic SET default_agent = ? WHERE id = ?`, other.Slug, f.topicID)
	require.NoError(t, err)

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
	assert.Empty(t, f.dispatcher.getMessages())
}

// A reply-to target that is not in this conversation is dropped at fire
// time; the message is still sent.
func TestScheduledSend_ReplyToOutsideConversationDropped(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content":     "reply later",
		"fire_at":     fireAt.UTC().Format(time.RFC3339Nano),
		"reply_to_id": tid("no-such-message"),
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	msgs := f.topicMessages(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, "reply later", msgs[0].Msg)
	assert.NotEqual(t, "reply", msgs[0].Type)
}

// ---------------------------------------------------------------------------
// Create validation and caller checks
// ---------------------------------------------------------------------------

func TestScheduledSend_CreateIdempotent(t *testing.T) {
	f := newScheduledSendFixture(t)
	body := map[string]interface{}{
		"content":         "once",
		"fire_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "same-key",
	}
	rec1 := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	rec2 := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var a, b scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &a))
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &b))
	assert.Equal(t, a.ID, b.ID)
	assert.Len(t, f.list(t, f.bob), 1)
}

func TestScheduledSend_CreateValidation(t *testing.T) {
	f := newScheduledSendFixture(t)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"empty content", map[string]interface{}{"content": "  ", "fire_at": future}},
		{"past fire_at", map[string]interface{}{"content": "x", "fire_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}},
		{"bad fire_at", map[string]interface{}{"content": "x", "fire_at": "tomorrow"}},
		{"attachments", map[string]interface{}{"content": "x", "fire_at": future, "attachments": []string{"a"}}},
		{"too long", map[string]interface{}{"content": strings.Repeat("x", 16001), "fire_at": future}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	assert.Empty(t, f.list(t, f.bob))
}

func TestScheduledSend_FireAtWithOffsetStoredUTC(t *testing.T) {
	f := newScheduledSendFixture(t)
	zone := time.FixedZone("UTC+2", 2*3600)
	fireAt := time.Now().Add(time.Hour).In(zone).Truncate(time.Second)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content": "offset", "fire_at": fireAt.Format(time.RFC3339),
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.FireAt.Equal(fireAt))
	assert.Contains(t, rec.Body.String(), fireAt.UTC().Format("2006-01-02T15:04:05")+"Z")
}

func TestScheduledSend_DMRejected(t *testing.T) {
	f := newScheduledSendFixture(t)
	dmKey := "dm:agent:" + f.agent.ID + ":user:" + f.bob.ID
	path := "/api/v1/chat/conversations/" + dmKey + "/scheduled"
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, map[string]interface{}{
		"content": "dm later", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestScheduledSend_OutsiderRefused(t *testing.T) {
	f := newScheduledSendFixture(t)
	outsider := &store.User{
		ID: tid("sched-outsider"), Email: "outsider@test.com", DisplayName: "Outsider",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(context.Background(), outsider))
	rec := doRequestAsUser(t, f.srv, outsider, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content": "x", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = doRequestAsUser(t, f.srv, outsider, http.MethodGet, f.scheduledPath(), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestScheduledSend_ScopedTokenRefused(t *testing.T) {
	f := newScheduledSendFixture(t)
	bob := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	scoped := NewScopedUserIdentity(bob, f.project.ID, []string{"project:read"})
	rec := doRequestAsIdentity(t, f.srv, scoped, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content": "x", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, f.list(t, f.bob))
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

func newTestScheduledRow(key, sender string, fireAt time.Time) *ScheduledChatMessage {
	now := time.Now().UTC()
	return &ScheduledChatMessage{
		ID:              tid("sched-" + key),
		SenderUserID:    sender,
		ConversationKey: tid("topic"),
		Content:         "content " + key,
		IdempotencyKey:  key,
		FireAt:          fireAt,
		Status:          ScheduledMessagePending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// openScheduledStorePair opens two independent webchat store handles on one
// file-backed SQLite database, as two hub replicas would share one database.
func openScheduledStorePair(t *testing.T) ([2]ScheduledMessageStore, string) {
	t.Helper()
	path := t.TempDir() + "/webchat.db"
	dsn := "file:" + path + "?_busy_timeout=10000&_journal_mode=WAL"
	var out [2]ScheduledMessageStore
	for i := range out {
		db, err := sql.Open("sqlite3", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		wcs := NewWebChatStore(db, "sqlite3")
		require.NoError(t, wcs.Init())
		out[i] = scheduledMessageStoreFrom(wcs)
		require.NotNil(t, out[i])
	}
	return out, dsn
}

func TestScheduledStore_SQLite_Transitions(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testScheduledStoreTransitions(t, sms[0])
}

func testScheduledStoreTransitions(t *testing.T, sms ScheduledMessageStore) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Create, idempotent create, sender filter.
	m := newTestScheduledRow("t1", "user-a", base.Add(time.Minute))
	row, existed, err := sms.CreateScheduledMessage(ctx, m)
	require.NoError(t, err)
	assert.False(t, existed)
	assert.True(t, row.FireAt.Equal(m.FireAt))
	dup := newTestScheduledRow("t1", "user-a", base.Add(time.Hour))
	row2, existed, err := sms.CreateScheduledMessage(ctx, dup)
	require.NoError(t, err)
	assert.True(t, existed)
	assert.Equal(t, row.ID, row2.ID)

	got, err := sms.GetScheduledMessage(ctx, "user-b", m.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "another sender cannot read it")
	ok, err := sms.CancelScheduledMessage(ctx, "user-b", m.ID, base)
	require.NoError(t, err)
	assert.False(t, ok, "another sender cannot cancel it")

	// Due ordering: only rows at or before now, oldest first. Fire times
	// are stored rounded up to the whole second, so a fractional fire time
	// is never due early.
	early := newTestScheduledRow("t2", "user-a", base.Add(-2*time.Second))
	late := newTestScheduledRow("t3", "user-a", base.Add(-time.Second).Add(500*time.Millisecond))
	notYet := newTestScheduledRow("t4", "user-a", base.Add(time.Second))
	for _, r := range []*ScheduledChatMessage{early, late, notYet} {
		_, _, err := sms.CreateScheduledMessage(ctx, r)
		require.NoError(t, err)
	}
	due, err := sms.ListDueScheduledMessages(ctx, base.Add(250*time.Millisecond), 10)
	require.NoError(t, err)
	require.Len(t, due, 2)
	assert.Equal(t, early.ID, due[0].ID)
	assert.Equal(t, late.ID, due[1].ID)
	assert.True(t, due[1].FireAt.Equal(base), "rounded up to the whole second")
	early2 := newTestScheduledRow("t5", "user-c", base.Add(1500*time.Millisecond))
	_, _, err = sms.CreateScheduledMessage(ctx, early2)
	require.NoError(t, err)
	due, err = sms.ListDueScheduledMessages(ctx, base.Add(1900*time.Millisecond), 10)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, early2.ID, d.ID, "not due before its (rounded-up) fire time")
	}
	n, err := sms.CountActiveScheduledMessages(ctx, "user-a")
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	byKey, err := sms.GetScheduledMessageByIdempotencyKey(ctx, "user-a", "t3")
	require.NoError(t, err)
	require.NotNil(t, byKey)
	assert.Equal(t, late.ID, byKey.ID)
	byKey, err = sms.GetScheduledMessageByIdempotencyKey(ctx, "user-b", "t3")
	require.NoError(t, err)
	assert.Nil(t, byKey)

	// Claim is exclusive; release returns it; sent and failed are final.
	ok, err = sms.ClaimScheduledMessage(ctx, early.ID, base)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = sms.ClaimScheduledMessage(ctx, early.ID, base)
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, sms.ReleaseScheduledMessage(ctx, early.ID, base))
	got, err = sms.GetScheduledMessage(ctx, "user-a", early.ID)
	require.NoError(t, err)
	assert.Equal(t, ScheduledMessagePending, got.Status)
	assert.Nil(t, got.ClaimedAt)

	ok, err = sms.ClaimScheduledMessage(ctx, early.ID, base)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, sms.MarkScheduledMessageSent(ctx, early.ID, "msg-1", base))
	got, err = sms.GetScheduledMessage(ctx, "user-a", early.ID)
	require.NoError(t, err)
	assert.Equal(t, ScheduledMessageSent, got.Status)
	assert.Equal(t, "msg-1", got.MessageID)
	ok, err = sms.CancelScheduledMessage(ctx, "user-a", early.ID, base)
	require.NoError(t, err)
	assert.False(t, ok, "a sent message cannot be cancelled")

	ok, err = sms.ClaimScheduledMessage(ctx, late.ID, base)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, sms.MarkScheduledMessageFailed(ctx, late.ID, ScheduledFailureNoAccess, base))
	got, err = sms.GetScheduledMessage(ctx, "user-a", late.ID)
	require.NoError(t, err)
	assert.Equal(t, ScheduledMessageFailed, got.Status)
	assert.Equal(t, ScheduledFailureNoAccess, got.FailureReason)

	// Cancel a pending one.
	ok, err = sms.CancelScheduledMessage(ctx, "user-a", m.ID, base)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = sms.ClaimScheduledMessage(ctx, m.ID, base.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "a cancelled message cannot be claimed")

	// The list shows pending, sending and failed only, for that sender and conversation.
	list, err := sms.ListScheduledMessages(ctx, "user-a", late.ConversationKey)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, late.ID, list[0].ID)
	assert.Equal(t, notYet.ID, list[1].ID)
	list, err = sms.ListScheduledMessages(ctx, "user-b", late.ConversationKey)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestScheduledStore_SQLite_MigrationRecorded(t *testing.T) {
	_, dsn := openScheduledStorePair(t)
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM webchat_migrations WHERE name = ?`, scheduledMessageTableMigration).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestScheduledStore_Postgres runs the store transitions and the two-handle
// claim race against Postgres when SCION_TEST_POSTGRES_DSN is set, in a
// throwaway schema.
func TestScheduledStore_Postgres(t *testing.T) {
	dsn := requirePostgresDSN(t)
	ctx := context.Background()
	schema := fmt.Sprintf("sched_send_test_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema

	open := func() *sql.DB {
		db := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	// Init's thread_id backfill reads messages; give it a minimal table.
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
    id uuid PRIMARY KEY, created timestamptz NOT NULL, channel text, thread_id text,
    sender text, sender_id text, recipient text, recipient_id text)`)
	require.NoError(t, err)

	var sms [2]ScheduledMessageStore
	for i := range sms {
		wcs := NewWebChatStore(open(), "postgres")
		require.NoError(t, wcs.Init())
		sms[i] = scheduledMessageStoreFrom(wcs)
	}
	testScheduledStoreTransitions(t, sms[0])
	testCancelClaimRace(t, sms[0], sms[1], "pg-race")

	const n = 40
	for i := 0; i < n; i++ {
		_, _, err := sms[0].CreateScheduledMessage(ctx, newTestScheduledRow(fmt.Sprintf("pg-rep-%d", i), "user-pg", time.Now().Add(-time.Second)))
		require.NoError(t, err)
	}
	claims := make(chan string, 2*n)
	done := make(chan struct{})
	for r := 0; r < 2; r++ {
		go func(st ScheduledMessageStore) {
			defer func() { done <- struct{}{} }()
			due, err := st.ListDueScheduledMessages(ctx, time.Now(), 100)
			if !assert.NoError(t, err) {
				return
			}
			for _, m := range due {
				if m.SenderUserID != "user-pg" {
					continue
				}
				ok, err := st.ClaimScheduledMessage(ctx, m.ID, time.Now())
				assert.NoError(t, err)
				if ok {
					claims <- m.ID
				}
			}
		}(sms[r])
	}
	<-done
	<-done
	close(claims)
	seen := map[string]int{}
	for id := range claims {
		seen[id]++
	}
	assert.Len(t, seen, n)
	for id, c := range seen {
		assert.Equal(t, 1, c, "row %s claimed more than once", id)
	}
}

// ---------------------------------------------------------------------------
// Review round 1: shutdown, transient errors, panic, limits
// ---------------------------------------------------------------------------

// A shutdown (cancelled sweeper context) after a row was claimed must not
// strand it in sending: delivery runs on a detached, bounded context.
func TestScheduledSend_ShutdownAfterClaim_StillDelivered(t *testing.T) {
	f := newScheduledSendFixture(t)
	sm := f.schedule(t, f.bob, "claimed then shutdown", time.Now().Add(2*time.Minute))

	ok, err := f.sms.ClaimScheduledMessage(context.Background(), sm.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	row := f.row(t, f.bob, sm.ID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.srv.fireScheduledMessage(ctx, f.sms, row)

	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageSent, got.Status, "never left in sending")
	assert.Len(t, f.topicMessages(t), 1)
}

// A sweep whose context is already cancelled claims nothing; the row stays
// pending for the next replica or restart.
func TestScheduledSend_SweepAfterShutdown_ClaimsNothing(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "not claimed", fireAt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)
	assert.Empty(t, f.topicMessages(t))
}

// The sweeper stops when asked and the stop does not hang without work.
func TestScheduledSend_StopSweeper(t *testing.T) {
	f := newScheduledSendFixture(t)
	f.srv.startScheduledSendSweeper(context.Background())
	stopped := make(chan struct{})
	go func() {
		f.srv.stopScheduledSendSweeper(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopScheduledSendSweeper did not return")
	}
	f.srv.stopScheduledSendSweeper(context.Background()) // idempotent
}

// flakyTopicWebChatStore fails GetTopic a set number of times with a store
// error, to drive the fire path's transient branch.
type flakyTopicWebChatStore struct {
	WebChatStore
	ScheduledMessageStore
	failures atomic.Int32
}

func (w *flakyTopicWebChatStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	if w.failures.Add(-1) >= 0 {
		return nil, errors.New("database unavailable")
	}
	return w.WebChatStore.GetTopic(ctx, id)
}

// A store error during the fire-time checks releases the claim: the row is
// pending again (nothing was sent) and the next sweep sends it.
func TestScheduledSend_TransientCheckError_ReleasedThenSent(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "retry me", fireAt)

	flaky := &flakyTopicWebChatStore{WebChatStore: f.wcs, ScheduledMessageStore: f.sms}
	flaky.failures.Store(1)
	f.srv.SetWebChatStore(flaky)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessagePending, got.Status)
	assert.Nil(t, got.ClaimedAt)
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, []string{"sending", "released"}, scheduledEventActions(t, collectEvents(events)),
		"the client learns the message is pending again")

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, sm.ID).Status)
	assert.Len(t, f.topicMessages(t), 1)
}

// panickingDispatcher panics on every agent message.
type panickingDispatcher struct{ brokerMockDispatcher }

func (d *panickingDispatcher) DispatchAgentMessage(context.Context, *store.Agent, string, bool, *messages.StructuredMessage) error {
	panic("dispatcher exploded")
}

// A panic during delivery marks the row failed (never back to pending, so
// it is not sent twice) and does not take down the sweeper.
func TestScheduledSend_PanicDuringDelivery_Failed(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "panic", fireAt)
	f.srv.SetDispatcher(&panickingDispatcher{})

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, got.Status)
	assert.Equal(t, ScheduledFailureDeliveryError, got.FailureReason)
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)))
}

func TestScheduledSend_CreateTimeAndLengthLimits(t *testing.T) {
	f := newScheduledSendFixture(t)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"under 60 s", map[string]interface{}{"content": "x", "fire_at": time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)}},
		{"beyond 90 days", map[string]interface{}{"content": "x", "fire_at": time.Now().Add(91 * 24 * time.Hour).UTC().Format(time.RFC3339)}},
		{"long reply_to_id", map[string]interface{}{"content": "x", "fire_at": future, "reply_to_id": strings.Repeat("r", 129)}},
		{"long idempotency_key", map[string]interface{}{"content": "x", "fire_at": future, "idempotency_key": strings.Repeat("k", 256)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	assert.Empty(t, f.list(t, f.bob))
}

// A sender can have at most 50 pending messages; the 51st create is
// refused, while a retry of an existing create still answers 200.
func TestScheduledSend_PendingCap(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	for i := 0; i < scheduledMaxActivePerSender; i++ {
		m := newTestScheduledRow(fmt.Sprintf("cap-%d", i), f.bob.ID, time.Now().Add(time.Hour))
		m.ConversationKey = f.topicID
		_, _, err := f.sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
	}
	body := map[string]interface{}{
		"content": "one too many", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "cap-new",
	}
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeScheduledLimit)

	// A retry of an earlier create is answered from the existing row.
	body["idempotency_key"] = "cap-0"
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Another user is not affected.
	body["idempotency_key"] = "alice-1"
	rec = doRequestAsUser(t, f.srv, f.alice, http.MethodPost, f.scheduledPath(), body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Once one is cancelled there is room again.
	ok, err := f.sms.CancelScheduledMessage(ctx, f.bob.ID, tid("sched-cap-0"), time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	body["idempotency_key"] = "cap-new"
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// Reusing an idempotency key in another conversation is refused instead of
// answering with the first conversation's row.
func TestScheduledSend_IdempotencyKeyReusedInOtherConversation(t *testing.T) {
	f := newScheduledSendFixture(t)
	other := tid("sched-topic-2")
	require.NoError(t, f.wcs.CreateTopic(context.Background(), WebChatTopic{
		ID: other, ProjectID: f.project.ID, Name: "sched-topic-2", CreatedBy: f.alice.ID, CreatedAt: time.Now().UTC(),
	}))
	body := map[string]interface{}{
		"content": "x", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "idempotency_key": "shared",
	}
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/chat/conversations/"+other+"/scheduled", body)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// SQLite stores the canonical webchat time text (as the other webchat_*
// tables do, and as the stored-timestamp normalizer expects); fire times
// have no fractional part.
func TestScheduledStore_SQLite_CanonicalTimeText(t *testing.T) {
	sms, dsn := openScheduledStorePair(t)
	ctx := context.Background()
	fireAt := time.Date(2026, 10, 8, 7, 0, 0, 750_000_000, time.UTC)
	m := newTestScheduledRow("canon", "user-a", fireAt)
	m.CreatedAt = time.Date(2026, 10, 7, 10, 0, 0, 120_000_000, time.UTC)
	m.UpdatedAt = m.CreatedAt
	_, _, err := sms[0].CreateScheduledMessage(ctx, m)
	require.NoError(t, err)

	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var fire, created string
	require.NoError(t, db.QueryRow(`SELECT fire_at, created_at FROM webchat_scheduled_message WHERE id = ?`, m.ID).Scan(&fire, &created))
	assert.Equal(t, "2026-10-08T07:00:01Z", fire, "rounded up, never early")
	assert.Equal(t, "2026-10-07T10:00:00.12Z", created)
}

// blockingDispatcher blocks every agent dispatch until its context ends,
// like a broker that keeps answering "not reachable yet".
type blockingDispatcher struct{ brokerMockDispatcher }

func (d *blockingDispatcher) DispatchAgentMessage(ctx context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	<-ctx.Done()
	return ctx.Err()
}

// When the delivery bound runs out during the send, the row is still
// finalized (its final write has its own context): sent or failed, never
// sending.
func TestScheduledSend_DeliveryBudgetExpires_RowFinalized(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "slow broker", fireAt)
	f.srv.SetDispatcher(&blockingDispatcher{})

	saved := scheduledDeliveryBudget
	scheduledDeliveryBudget = 300 * time.Millisecond
	t.Cleanup(func() { scheduledDeliveryBudget = saved })

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	got := f.row(t, f.bob, sm.ID)
	assert.Contains(t, []string{ScheduledMessageSent, ScheduledMessageFailed}, got.Status, "never left in sending")
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Minute)), "not sent again")
}

// The delivery bound is never shorter than a live send's worst case.
func TestScheduledSend_DeliveryBudgetCoversLiveWorstCase(t *testing.T) {
	worst := time.Duration(1+messages.MaxMentionRecipients) * chatWakeDeliveryBudget
	assert.Greater(t, scheduledDeliveryBudget, worst)
}

// Stopping the sweeper is bounded even when callers pass no deadline (as
// production does): after the grace period the delivery in progress is cut
// short and the row is still finalized, sent or failed.
func TestScheduledSend_StopCutsSlowDeliveryShort(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "slow at shutdown", fireAt)
	f.srv.SetDispatcher(&blockingDispatcher{})
	savedGrace := scheduledStopGrace
	scheduledStopGrace = 200 * time.Millisecond
	t.Cleanup(func() { scheduledStopGrace = savedGrace })

	f.srv.startScheduledSendSweeper(context.Background())
	go f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second))
	require.Eventually(t, func() bool {
		return f.row(t, f.bob, sm.ID).Status == ScheduledMessageSending
	}, 5*time.Second, 20*time.Millisecond)

	start := time.Now()
	f.srv.stopScheduledSendSweeper(context.Background())
	assert.Less(t, time.Since(start), scheduledStopGrace+scheduledFinalizeTimeout)
	assert.Contains(t, []string{ScheduledMessageSent, ScheduledMessageFailed}, f.row(t, f.bob, sm.ID).Status)
}

// A caller's shutdown deadline shorter than the grace period is honoured.
func TestScheduledSend_StopHonoursShutdownDeadline(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "slow, short deadline", fireAt)
	f.srv.SetDispatcher(&blockingDispatcher{})
	go f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second))
	require.Eventually(t, func() bool {
		return f.row(t, f.bob, sm.ID).Status == ScheduledMessageSending
	}, 5*time.Second, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	f.srv.stopScheduledSendSweeper(ctx)
	assert.Less(t, time.Since(start), 100*time.Millisecond+scheduledFinalizeTimeout)
	assert.Contains(t, []string{ScheduledMessageSent, ScheduledMessageFailed}, f.row(t, f.bob, sm.ID).Status)
}

// selectiveDispatcher blocks dispatches to one agent until release is
// closed; others are accepted at once.
type selectiveDispatcher struct {
	brokerMockDispatcher
	slowSlug string
	release  chan struct{}
}

func (d *selectiveDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, msg string, interrupt bool, sm *messages.StructuredMessage) error {
	if a.Slug == d.slowSlug {
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, a, msg, interrupt, sm)
}

// One sender's slow batch does not hold up another sender: the other
// sender's message is delivered while the slow one is still in progress,
// in the same sweep and in a later one. Each sender has at most one
// message in delivery.
func TestScheduledSend_SlowSenderDoesNotBlockOthers(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	// alice writes in a second topic whose default agent is hers.
	aliceAgent := &store.Agent{
		ID: tid("sched-fast-agent"), ProjectID: f.project.ID, Name: "fast-agent", Slug: "fast-agent",
		Phase: "running", OwnerID: f.alice.ID, CreatedBy: f.alice.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, aliceAgent))
	topic2 := tid("sched-topic-fast")
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: topic2, ProjectID: f.project.ID, Name: "fast", CreatedBy: f.alice.ID,
		CreatedAt: time.Now().UTC(), DefaultAgent: aliceAgent.Slug,
	}))
	setTopicConversationID(t, f.db, f.store, topic2, f.project.ID)
	disp := &selectiveDispatcher{slowSlug: f.agent.Slug, release: make(chan struct{})}
	f.srv.SetDispatcher(disp)

	fireAt := time.Now().Add(2 * time.Minute)
	bob1 := f.schedule(t, f.bob, "bob one", fireAt)
	bob2 := f.schedule(t, f.bob, "bob two", fireAt)
	aliceRow := func(content string) scheduledMessageResponse {
		rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/chat/conversations/"+topic2+"/scheduled",
			map[string]interface{}{"content": content, "fire_at": fireAt.UTC().Format(time.RFC3339Nano), "idempotency_key": content})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var r scheduledMessageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
		return r
	}
	a1 := aliceRow("alice one")

	first := make(chan int, 1)
	go func() { first <- f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)) }()
	require.Eventually(t, func() bool {
		return f.row(t, f.alice, a1.ID).Status == ScheduledMessageSent
	}, 5*time.Second, 20*time.Millisecond, "alice's message is not held up by bob's slow one")
	// bob's first message is claimed (its worker may start after alice's).
	require.Eventually(t, func() bool {
		return f.row(t, f.bob, bob1.ID).Status == ScheduledMessageSending ||
			f.row(t, f.bob, bob2.ID).Status == ScheduledMessageSending
	}, 5*time.Second, 20*time.Millisecond)
	statuses := []string{f.row(t, f.bob, bob1.ID).Status, f.row(t, f.bob, bob2.ID).Status}
	assert.ElementsMatch(t, []string{ScheduledMessageSending, ScheduledMessagePending}, statuses,
		"one message per sender in delivery")

	// A later tick, while bob's delivery is still in progress.
	a2 := aliceRow("alice two")
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(2*time.Second)), "bob is skipped while in delivery")
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.alice, a2.ID).Status)

	close(disp.release)
	assert.Equal(t, 3, <-first, "alice one, bob one and bob two")
	f.srv.waitScheduledDeliveries()
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, bob1.ID).Status)
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, bob2.ID).Status)
}

// experimentTogglingDispatcher turns the experiment off on its first
// dispatch.
type experimentTogglingDispatcher struct {
	brokerMockDispatcher
	t   *testing.T
	srv *Server
	off sync.Once
}

func (d *experimentTogglingDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, msg string, interrupt bool, sm *messages.StructuredMessage) error {
	d.off.Do(func() { setScheduledSendExperiment(d.t, d.srv, false) })
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, a, msg, interrupt, sm)
}

// The experiment is checked before each claim: turning it off during a
// batch holds the rest of the batch.
func TestScheduledSend_ExperimentTurnedOffMidBatch_RestHeld(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	first := f.schedule(t, f.bob, "first", fireAt)
	second := f.schedule(t, f.bob, "second", fireAt)
	f.srv.SetDispatcher(&experimentTogglingDispatcher{t: t, srv: f.srv})

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	sent, held := f.row(t, f.bob, first.ID), f.row(t, f.bob, second.ID)
	if sent.Status == ScheduledMessagePending {
		sent, held = held, sent
	}
	assert.Equal(t, ScheduledMessageSent, sent.Status)
	assert.Equal(t, ScheduledMessagePending, held.Status)
}

// A 404 from sendChatMessage at fire time is a delivery error: the checks
// just before it proved the conversation exists.
func TestScheduledSend_FailureMapping(t *testing.T) {
	assert.Equal(t, ScheduledFailureNoAccess, scheduledFailureFromSendError(chatSendForbidden()))
	assert.Equal(t, ScheduledFailureDeliveryError, scheduledFailureFromSendError(chatSendNotFound("Thread")))
	assert.Equal(t, ScheduledFailureDeliveryError, scheduledFailureFromSendError(
		newChatSendError(http.StatusInternalServerError, "INTERNAL", "x", nil)))
}

// doScheduledRequestWithCredential serves a request as identity with the given
// credential context, bypassing authentication (as doRequestAsIdentity).
func doScheduledRequestWithCredential(t *testing.T, srv *Server, identity Identity, cred CredentialContext, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx := contextWithCredentialContext(contextWithIdentity(req.Context(), identity), cred)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// Only an interactive session may use the scheduled-message routes: a
// broker request on behalf of the user is refused on create, list and
// cancel; the same user with an interactive credential is accepted
// (control). Fails if the credential-kind check in scheduledSendCaller is
// removed.
func TestScheduledSend_BrokerOnBehalfOfRefused(t *testing.T) {
	f := newScheduledSendFixture(t)
	bob := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, "integration")
	broker := CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"}
	body := func(k string) map[string]interface{} {
		return map[string]interface{}{
			"content": "x", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "idempotency_key": k,
		}
	}
	existing := f.schedule(t, f.bob, "existing", time.Now().Add(time.Hour))

	rec := doScheduledRequestWithCredential(t, f.srv, bob, broker, http.MethodPost, f.scheduledPath(), body("broker"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "create: %s", rec.Body.String())
	rec = doScheduledRequestWithCredential(t, f.srv, bob, broker, http.MethodGet, f.scheduledPath(), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "list: %s", rec.Body.String())
	rec = doScheduledRequestWithCredential(t, f.srv, bob, broker, http.MethodDelete, f.scheduledPath()+"/"+existing.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "cancel: %s", rec.Body.String())
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, existing.ID).Status)
	assert.Len(t, f.list(t, f.bob), 1)

	interactive := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	rec = doScheduledRequestWithCredential(t, f.srv, interactive, CredentialContext{Kind: CredentialKindInteractive},
		http.MethodPost, f.scheduledPath(), body("interactive"))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// scheduledSendCaller, which guards create, list and cancel alike, accepts
// interactive and dev sessions only, and refuses scoped tokens and
// federated identities whatever their credential kind. Fails if any of
// those checks is removed.
func TestScheduledSend_CallerGuard(t *testing.T) {
	user := NewAuthenticatedUser(tid("guard-user"), "g@test.com", "G", "member", string(ClientTypeWeb))
	scoped := NewScopedUserIdentity(user, tid("guard-project"), []string{"project:read"})
	fed := NewFederatedUserIdentity("https://issuer.example", "sub-1", "g@test.com", "G", "member", nil)
	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		allowed  bool
	}{
		{"interactive session", user, CredentialKindInteractive, true},
		{"dev session", user, CredentialKindDev, true},
		{"broker on behalf of the user", user, CredentialKindBroker, false},
		{"no credential context", user, "", false},
		{"scoped access token", scoped, CredentialKindInteractive, false},
		{"federated identity", fed, CredentialKindInteractive, false},
		{"federated identity, federation credential", fed, CredentialKindFederation, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/x/scheduled", nil)
			ctx := contextWithIdentity(req.Context(), tc.identity)
			if tc.kind != "" {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			}
			rec := httptest.NewRecorder()
			got := scheduledSendCaller(rec, req.WithContext(ctx))
			if tc.allowed {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
				assert.Equal(t, http.StatusForbidden, rec.Code)
			}
		})
	}
}
