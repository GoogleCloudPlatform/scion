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
	return newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
}

func newScheduledSendFixtureOn(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, project *store.Project) *scheduledSendFixture {
	t.Helper()
	ctx := context.Background()
	// alice owns the project; bob is an ordinary project member.
	addProjectMemberWithRole(t, s, project, bob.ID, store.GroupMemberRoleMember)

	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	db := openTestMemorySQLite(t, "sqlite3")
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
		_, _, err := sms[0].CreateScheduledMessage(ctx, newTestScheduledRow(fmt.Sprintf("rep-%d", i), fmt.Sprintf("rep-user-%d", i), time.Now().Add(-time.Second)))
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
	// The due list has one row per sender (its oldest); the sender's next
	// due row comes from NextDueScheduledMessage.
	due, err := sms.ListDueScheduledMessages(ctx, base.Add(250*time.Millisecond), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, early.ID, due[0].ID)
	gotLate, err := sms.GetScheduledMessage(ctx, "user-a", late.ID)
	require.NoError(t, err)
	assert.True(t, gotLate.FireAt.Equal(base), "rounded up to the whole second")
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
	next, err := sms.NextDueScheduledMessage(ctx, "user-a", base.Add(250*time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, late.ID, next.ID, "the sender's next due row")
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
	testScheduledStoreDuePerSender(t, sms[0])
	testCancelClaimRace(t, sms[0], sms[1], "pg-race")
	testOneSenderTwoReplicas(t, sms[0], sms[1], "pg-one-sender", 30)

	const n = 40
	for i := 0; i < n; i++ {
		_, _, err := sms[0].CreateScheduledMessage(ctx, newTestScheduledRow(fmt.Sprintf("pg-rep-%d", i), fmt.Sprintf("user-pg-%d", i), time.Now().Add(-time.Second)))
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
				if !strings.HasPrefix(m.SenderUserID, "user-pg-") {
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
	assert.Less(t, time.Since(start), 2*time.Second, "returned at the caller's deadline")
	// The cut-short delivery still finalizes the row on its own context.
	assert.Eventually(t, func() bool {
		st := f.row(t, f.bob, sm.ID).Status
		return st == ScheduledMessageSent || st == ScheduledMessageFailed
	}, scheduledFinalizeTimeout, 20*time.Millisecond, "never left in sending")
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
			got := scheduledSendCaller(rec, req.WithContext(ctx), ActionCreate)
			if tc.allowed {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
				assert.Equal(t, http.StatusForbidden, rec.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Review round 4
// ---------------------------------------------------------------------------

// senderBlockingDispatcher blocks dispatches for messages from the listed
// senders until release is closed (or the context ends).
type senderBlockingDispatcher struct {
	brokerMockDispatcher
	blocked map[string]bool
	release chan struct{}
}

func (d *senderBlockingDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, msg string, interrupt bool, sm *messages.StructuredMessage) error {
	if sm != nil && d.blocked[sm.SenderID] {
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, a, msg, interrupt, sm)
}

// Two senders with many due messages behind a slow broker do not keep a
// third sender's message from being listed and sent: the due list has one
// row per sender.
func TestScheduledSend_HeavySendersDoNotStarveOthers(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()

	carol := &store.User{ID: tid("user-carol"), Email: "carol@test.com", DisplayName: "Carol",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, f.store.CreateUser(ctx, carol))
	addProjectMemberWithRole(t, f.store, f.project, carol.ID, store.GroupMemberRoleMember)
	carolAgent := &store.Agent{ID: tid("sched-carol-agent"), ProjectID: f.project.ID, Name: "carol-agent",
		Slug: "carol-agent", Phase: "running", OwnerID: carol.ID, CreatedBy: carol.ID}
	require.NoError(t, f.store.CreateAgent(ctx, carolAgent))
	carolTopic := tid("sched-topic-carol")
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{ID: carolTopic, ProjectID: f.project.ID, Name: "carol",
		CreatedBy: carol.ID, CreatedAt: time.Now().UTC(), DefaultAgent: carolAgent.Slug}))
	setTopicConversationID(t, f.db, f.store, carolTopic, f.project.ID)

	disp := &senderBlockingDispatcher{blocked: map[string]bool{f.bob.ID: true, f.alice.ID: true}, release: make(chan struct{})}
	f.srv.SetDispatcher(disp)

	// bob and alice each have 26 due messages, older than carol's.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 26; i++ {
		for _, sender := range []string{f.bob.ID, f.alice.ID} {
			m := newTestScheduledRow(fmt.Sprintf("heavy-%s-%d", sender, i), sender, base.Add(time.Duration(i)*time.Second))
			m.ConversationKey = f.topicID
			_, _, err := f.sms.CreateScheduledMessage(ctx, m)
			require.NoError(t, err)
		}
	}
	carolRow := func(key string, at time.Time) *ScheduledChatMessage {
		m := newTestScheduledRow(key, carol.ID, at)
		m.ConversationKey = carolTopic
		_, _, err := f.sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
		return m
	}
	c1 := carolRow("carol-1", base.Add(time.Minute))

	now := time.Now()
	first := make(chan int, 1)
	go func() { first <- f.srv.sweepScheduledMessages(ctx, now) }()
	// Sent, and carol's worker has finished (it is out of inFlight), so the
	// next sweep can give her a slot.
	require.Eventually(t, func() bool {
		if f.row(t, carol, c1.ID).Status != ScheduledMessageSent {
			return false
		}
		rt := f.srv.scheduledRuntime()
		rt.mu.Lock()
		defer rt.mu.Unlock()
		_, busy := rt.inFlight[carol.ID]
		return !busy
	}, 5*time.Second, 5*time.Millisecond, "carol's message is listed and sent despite 52 older ones")

	// A later sweep, while bob and alice are still blocked. carol-2 falls
	// due after the first sweep's "now", so only the later sweep sees it.
	c2 := carolRow("carol-2", now.Add(time.Second))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, now.Add(2*time.Second)))
	assert.Equal(t, ScheduledMessageSent, f.row(t, carol, c2.ID).Status)

	close(disp.release)
	// No sender waited for a worker, so bob's and alice's workers did not
	// yield: the first sweep delivers all 52 plus carol's first.
	assert.Equal(t, 53, <-first)
	f.srv.waitScheduledDeliveries()
	for i := 0; i < 20; i++ {
		if f.srv.sweepScheduledMessages(ctx, now.Add(2*time.Second)) == 0 {
			break
		}
	}
	for _, u := range []string{f.bob.ID, f.alice.ID, carol.ID} {
		n, err := f.sms.CountActiveScheduledMessages(ctx, u)
		require.NoError(t, err)
		assert.Equal(t, 0, n, "all delivered eventually")
	}
}

// Stopping cuts deliveries short; a claimed row whose checks are cut short
// goes back to pending (with a released event), it does not fail.
func TestScheduledSend_AbortBeforeSend_Released(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "aborted", fireAt)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()

	f.srv.scheduledRuntime().abort()
	ok, err := f.sms.ClaimScheduledMessage(context.Background(), sm.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	released := f.srv.fireScheduledMessage(context.Background(), f.sms, f.row(t, f.bob, sm.ID))

	assert.True(t, released)
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, []string{"released"}, scheduledEventActions(t, collectEvents(events)))
}

// abortingTopicStore aborts the sweeper's deliveries during the send's own
// topic lookup (the second GetTopic of a fire), after the fire-time checks
// passed.
type abortingTopicStore struct {
	WebChatStore
	ScheduledMessageStore
	srv   *Server
	calls atomic.Int32
}

func (w *abortingTopicStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	if w.calls.Add(1) == 2 {
		w.srv.scheduledRuntime().abort()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return w.WebChatStore.GetTopic(ctx, id)
}

// A send refused because it was cut short is recorded as a delivery error,
// never as an access problem.
func TestScheduledSend_AbortDuringSend_DeliveryError(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "cut short", fireAt)
	f.srv.SetWebChatStore(&abortingTopicStore{WebChatStore: f.wcs, ScheduledMessageStore: f.sms, srv: f.srv})

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, got.Status)
	assert.Equal(t, ScheduledFailureDeliveryError, got.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

// The runtime is single-use: after a stop, start and sweeps do nothing.
func TestScheduledSend_NoWorkAfterStop(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "after stop", fireAt)
	f.srv.stopScheduledSendSweeper(context.Background())
	f.srv.startScheduledSendSweeper(context.Background())
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)
	f.srv.waitScheduledDeliveries()
}

// The due list has one row per sender (the oldest); the next one is
// fetched per sender.
func TestScheduledStore_SQLite_DuePerSender(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testScheduledStoreDuePerSender(t, sms[0])
}

func testScheduledStoreDuePerSender(t *testing.T, sms ScheduledMessageStore) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	add := func(key, sender string, at time.Time) *ScheduledChatMessage {
		m := newTestScheduledRow(key, sender, at)
		_, _, err := sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
		return m
	}
	a1 := add("dps-a1", "dps-a", base)
	add("dps-a2", "dps-a", base.Add(time.Second))
	add("dps-a3", "dps-a", base.Add(2*time.Second))
	b1 := add("dps-b1", "dps-b", base.Add(3*time.Second))
	add("dps-c-future", "dps-c", time.Now().Add(time.Hour))

	due, err := sms.ListDueScheduledMessages(ctx, time.Now(), 10)
	require.NoError(t, err)
	var ids []string
	for _, d := range due {
		if strings.HasPrefix(d.SenderUserID, "dps-") {
			ids = append(ids, d.ID)
		}
	}
	assert.Equal(t, []string{a1.ID, b1.ID}, ids)

	next, err := sms.NextDueScheduledMessage(ctx, "dps-a", time.Now())
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, a1.ID, next.ID)
	ok, err := sms.ClaimScheduledMessage(ctx, a1.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	next, err = sms.NextDueScheduledMessage(ctx, "dps-a", time.Now())
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, tid("sched-dps-a2"), next.ID)
	next, err = sms.NextDueScheduledMessage(ctx, "dps-c", time.Now())
	require.NoError(t, err)
	assert.Nil(t, next, "not due yet")
}

// ---------------------------------------------------------------------------
// Review round 5
// ---------------------------------------------------------------------------

// A refusal on a delivery that was cut short is a delivery error; the same
// refusal on a live delivery keeps its meaning.
func TestScheduledSend_RefusalReason(t *testing.T) {
	live, cut := false, true
	assert.Equal(t, ScheduledFailureNoAccess, scheduledRefusalReason(live, chatSendForbidden()))
	assert.Equal(t, ScheduledFailureDeliveryError, scheduledRefusalReason(cut, chatSendForbidden()))
	assert.Equal(t, ScheduledFailureDeliveryError, scheduledRefusalReason(cut, chatSendNotFound("Thread")))
}

// stallAbortPropagation makes the runtime's abort never reach a delivery
// context, standing in for the context.AfterFunc goroutine not having run
// yet. Only a direct read of the abort can then cut the delivery short.
func stallAbortPropagation(srv *Server) {
	rt := srv.scheduledRuntime()
	rt.mu.Lock()
	rt.afterAbort = func(context.Context, func()) func() bool {
		return func() bool { return true }
	}
	rt.mu.Unlock()
}

// A delivery started after the runtime was aborted runs no checks, sends
// nothing and goes back to pending, even when the abort has not reached
// its context (ptone/scion#3827).
func TestScheduledSend_AbortBeforeFire_PropagationStalled_Released(t *testing.T) {
	f := newScheduledSendFixture(t)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "aborted, cancel not landed", fireAt)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()

	stallAbortPropagation(f.srv)
	f.srv.scheduledRuntime().abort()
	ok, err := f.sms.ClaimScheduledMessage(context.Background(), sm.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	released := f.srv.fireScheduledMessage(context.Background(), f.sms, f.row(t, f.bob, sm.ID))

	assert.True(t, released)
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, []string{"released"}, scheduledEventActions(t, collectEvents(events)))
}

// abortOnProjectCallStore aborts the scheduled-send runtime during the
// at-th GetProject made by a scheduled delivery while armed, without
// waiting for the abort to reach the delivery context. Call 1 is the
// fire-time check; call 2 is the send's own authorization. With foreign
// set, that call answers a project the sender has no access to.
type abortOnProjectCallStore struct {
	store.Store
	fault   *storeFaultSwitch
	srv     atomic.Pointer[Server]
	at      int32
	foreign bool
	calls   atomic.Int32
}

func (w *abortOnProjectCallStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	exec, _ := ExecutorContextFromContext(ctx)
	if !w.fault.Active() || exec.Kind != scheduledSendClientType || w.calls.Add(1) != w.at {
		return w.Store.GetProject(ctx, id)
	}
	w.srv.Load().scheduledRuntime().abort()
	p, err := w.Store.GetProject(ctx, id)
	if err != nil || !w.foreign {
		return p, err
	}
	other := *p
	other.ID = tid("foreign-project")
	other.OwnerID = tid("someone-else")
	return &other, nil
}

func newAbortOnProjectCallFixture(t *testing.T, at int32, foreign bool) (*scheduledSendFixture, *abortOnProjectCallStore, *storeFaultSwitch) {
	t.Helper()
	srv, s, alice, bob, project, wrapped, fault := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, fault *storeFaultSwitch) *abortOnProjectCallStore {
			return &abortOnProjectCallStore{Store: inner, fault: fault, at: at, foreign: foreign}
		})
	wrapped.srv.Store(srv)
	return newScheduledSendFixtureOn(t, srv, s, alice, bob, project), wrapped, fault
}

// An abort during the fire-time checks releases the row even when it has
// not reached the delivery context by the time the checks finish: the
// gate after the checks reads the abort itself (ptone/scion#3827).
func TestScheduledSend_AbortDuringChecks_PropagationStalled_Released(t *testing.T) {
	f, wrapped, fault := newAbortOnProjectCallFixture(t, 1, false)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "aborted in checks", fireAt)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()

	stallAbortPropagation(f.srv)
	fault.Arm()
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	assert.Equal(t, int32(1), wrapped.calls.Load(), "aborted inside the checks' GetProject, no send")
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessagePending, got.Status)
	assert.Empty(t, got.FailureReason)
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, []string{"sending", "released"}, scheduledEventActions(t, collectEvents(events)))
}

// A send refused after an abort that has not reached the delivery context
// is still recorded as a delivery error, not as an access problem.
func TestScheduledSend_AbortDuringSend_PropagationStalled_DeliveryError(t *testing.T) {
	f, wrapped, fault := newAbortOnProjectCallFixture(t, 2, true)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "aborted in send", fireAt)

	stallAbortPropagation(f.srv)
	fault.Arm()
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	assert.GreaterOrEqual(t, wrapped.calls.Load(), int32(2), "aborted inside the send's GetProject")
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, got.Status)
	assert.Equal(t, ScheduledFailureDeliveryError, got.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

// abortOnProjectStore aborts the scheduled-send runtime during the first
// GetProject made by a scheduled delivery while armed, and still answers
// it: the fire-time checks then finish their store reads and only the
// access decision sees the cut-short context. Other callers (background
// work of the test server) are not counted.
type abortOnProjectStore struct {
	store.Store
	fault *storeFaultSwitch
	srv   atomic.Pointer[Server]
	calls atomic.Int32
}

func (w *abortOnProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	exec, _ := ExecutorContextFromContext(ctx)
	if w.fault.Active() && exec.Kind == scheduledSendClientType && w.calls.Add(1) == 1 {
		w.srv.Load().scheduledRuntime().abort()
		// The abort reaches the delivery context through context.AfterFunc,
		// which runs on its own goroutine: wait until it has.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return w.Store.GetProject(context.WithoutCancel(ctx), id)
	}
	return w.Store.GetProject(ctx, id)
}

// A delivery cut short after the fire-time store reads (so only the
// access decision sees the cancelled context) goes back to pending with a
// released event; it does not fail with a misleading reason.
func TestScheduledSend_AbortAfterCheckReads_Released(t *testing.T) {
	srv, s, alice, bob, project, wrapped, fault := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, fault *storeFaultSwitch) *abortOnProjectStore {
			return &abortOnProjectStore{Store: inner, fault: fault}
		})
	wrapped.srv.Store(srv)
	f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "cut after checks", fireAt)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()

	fault.Arm()
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), fireAt.Add(time.Second)))
	assert.Equal(t, int32(1), wrapped.calls.Load(), "aborted inside the checks' GetProject")
	got := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessagePending, got.Status)
	assert.Empty(t, got.FailureReason)
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, []string{"sending", "released"}, scheduledEventActions(t, collectEvents(events)))
}

// slowDispatcher delays every dispatch a little.
type slowDispatcher struct {
	brokerMockDispatcher
	delay time.Duration
}

func (d *slowDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, msg string, interrupt bool, sm *messages.StructuredMessage) error {
	time.Sleep(d.delay)
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, a, msg, interrupt, sm)
}

// More senders with long batches than there are workers take turns: each
// worker yields after scheduledSendYieldAfter messages while another
// sender is waiting (here the fifth heavy sender and the light sender),
// senders that yielded are listed last, and a sender with one message is
// delivered within two sweeps.
func TestScheduledSend_ManyHeavySendersRotate(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	f.srv.SetDispatcher(&slowDispatcher{delay: 20 * time.Millisecond})

	// Five heavy senders. Rows are inserted directly; bob may message the
	// topic's agent, so every heavy row is bob's or a stand-in sender that
	// fails its fire-time checks (still claimed and finalized like any row).
	heavy := []string{f.bob.ID}
	for i := 1; i < scheduledSendWorkers+1; i++ {
		u := &store.User{ID: tid(fmt.Sprintf("heavy-user-%d", i)), Email: fmt.Sprintf("heavy%d@test.com", i),
			DisplayName: "Heavy", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
		require.NoError(t, f.store.CreateUser(ctx, u))
		addProjectMemberWithRole(t, f.store, f.project, u.ID, store.GroupMemberRoleMember)
		heavy = append(heavy, u.ID)
	}
	require.Len(t, heavy, scheduledSendWorkers+1)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3*scheduledSendYieldAfter; i++ {
		for j, sender := range heavy {
			m := newTestScheduledRow(fmt.Sprintf("rot-%d-%d", j, i), sender, base.Add(time.Duration(i)*time.Second))
			m.ConversationKey = f.topicID
			_, _, err := f.sms.CreateScheduledMessage(ctx, m)
			require.NoError(t, err)
		}
	}
	light := newTestScheduledRow("rot-light", f.alice.ID, base.Add(time.Minute))
	light.ConversationKey = f.topicID
	_, _, err := f.sms.CreateScheduledMessage(ctx, light)
	require.NoError(t, err)

	now := time.Now()
	for sweep := 1; sweep <= 2; sweep++ {
		f.srv.sweepScheduledMessages(ctx, now)
		if f.row(t, f.alice, light.ID).Status != ScheduledMessagePending {
			break
		}
	}
	assert.NotEqual(t, ScheduledMessagePending, f.row(t, f.alice, light.ID).Status,
		"the light sender is delivered within two sweeps")
	for _, sender := range heavy {
		n, err := f.sms.CountActiveScheduledMessages(ctx, sender)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, scheduledSendYieldAfter, "each heavy sender yielded after %d", scheduledSendYieldAfter)
	}
}

// drainSender runs the sweeper's per-sender loop against one store handle:
// list the sender's oldest due row, then claim and fetch the next until
// none is left, recording every successful claim.
func drainSender(ctx context.Context, t *testing.T, st ScheduledMessageStore, sender string, claims chan<- string) {
	due, err := st.ListDueScheduledMessages(ctx, time.Now(), 100)
	if !assert.NoError(t, err) {
		return
	}
	var row *ScheduledChatMessage
	for i := range due {
		if due[i].SenderUserID == sender {
			row = &due[i]
			break
		}
	}
	attempted := map[string]bool{}
	for row != nil && !attempted[row.ID] {
		attempted[row.ID] = true
		ok, err := st.ClaimScheduledMessage(ctx, row.ID, time.Now())
		assert.NoError(t, err)
		if ok {
			claims <- row.ID
		}
		next, err := st.NextDueScheduledMessage(ctx, sender, time.Now())
		if !assert.NoError(t, err) {
			return
		}
		row = next
	}
}

// testOneSenderTwoReplicas: one sender with n due rows, two store handles
// running the sweeper's per-sender loop at once; every row is claimed
// exactly once.
func testOneSenderTwoReplicas(t *testing.T, a, b ScheduledMessageStore, sender string, n int) {
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, _, err := a.CreateScheduledMessage(ctx, newTestScheduledRow(fmt.Sprintf("%s-%d", sender, i), sender, time.Now().Add(-time.Minute)))
		require.NoError(t, err)
	}
	claims := make(chan string, 2*n)
	done := make(chan struct{})
	for _, st := range []ScheduledMessageStore{a, b} {
		go func(st ScheduledMessageStore) {
			defer func() { done <- struct{}{} }()
			drainSender(ctx, t, st, sender, claims)
		}(st)
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

func TestScheduledSend_TwoReplicas_OneSenderManyRows(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testOneSenderTwoReplicas(t, sms[0], sms[1], "one-sender", 30)
}

// ---------------------------------------------------------------------------
// Review round 6
// ---------------------------------------------------------------------------

// With free workers and no other sender waiting, a worker does not yield:
// one sender's 12 due messages all go out in one sweep.
func TestScheduledSend_NoYieldWithoutContention(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 12; i++ {
		m := newTestScheduledRow(fmt.Sprintf("burst-%d", i), f.bob.ID, base.Add(time.Duration(i)*time.Second))
		m.ConversationKey = f.topicID
		_, _, err := f.sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
	}
	assert.Equal(t, 12, f.srv.sweepScheduledMessages(ctx, time.Now()))
	n, err := f.sms.CountActiveScheduledMessages(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Len(t, f.topicMessages(t), 12)
}

// A yielded mark is dropped once the sender has nothing due (the due list
// is complete), and kept while the list may have been cut off at the batch
// limit.
func TestScheduledSend_YieldedMarkPruned(t *testing.T) {
	rt := &scheduledSendRuntime{inFlight: map[string]struct{}{}, yielded: map[string]struct{}{}}
	rt.yielded["gone"] = struct{}{}
	rt.yielded["still-due"] = struct{}{}
	due := []ScheduledChatMessage{{ID: "1", SenderUserID: "still-due"}, {ID: "2", SenderUserID: "other"}}

	out := rt.yieldedLast(due)
	assert.Equal(t, []string{"2", "1"}, []string{out[0].ID, out[1].ID}, "yielded sender listed last")
	assert.NotContains(t, rt.yielded, "gone")
	assert.Contains(t, rt.yielded, "still-due")

	rt.yielded["gone"] = struct{}{}
	full := make([]ScheduledChatMessage, scheduledSendBatch)
	for i := range full {
		full[i] = ScheduledChatMessage{ID: fmt.Sprint(i), SenderUserID: fmt.Sprintf("s%d", i)}
	}
	rt.yieldedLast(full)
	assert.Contains(t, rt.yielded, "gone", "kept when the list may be cut off")
}

// twoServerSQLite is two full hub servers sharing one SQLite hub database
// and one SQLite webchat database, with one sender (bob) who may message the
// topic's default agent and n due scheduled messages.
type twoServerSQLite struct {
	a, b    twoServerReplica
	bob     *store.User
	topicID string
	sms     ScheduledMessageStore
	rowIDs  []string // oldest first
}

type twoServerReplica struct {
	srv *Server
	st  store.Store
	wcs WebChatStore
	db  *sql.DB
}

func newTwoServerSQLite(t *testing.T, n int) *twoServerSQLite {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	// No shared cache and a busy timeout on both files: the two servers
	// write concurrently through separate connections.
	hubDSN := "file:" + dir + "/hub.db?_pragma=busy_timeout(10000)"
	webchatDSN := "file:" + dir + "/webchat.db?_busy_timeout=10000&_journal_mode=WAL"
	newReplica := func() twoServerReplica {
		st, err := newTestStoreAt(t, hubDSN)
		require.NoError(t, err)
		srv, st := testServerWithStore(t, st)
		db, err := sql.Open("sqlite3", webchatDSN)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		wcs := NewWebChatStore(db, "sqlite3")
		require.NoError(t, wcs.Init())
		srv.SetWebChatStore(wcs)
		srv.SetDispatcher(&brokerMockDispatcher{})
		setScheduledSendExperiment(t, srv, true)
		return twoServerReplica{srv: srv, st: st, wcs: wcs, db: db}
	}
	f := &twoServerSQLite{a: newReplica(), b: newReplica()}

	_, bob, project := setupDemoPolicyOn(t, f.a.srv, f.a.st)
	addProjectMemberWithRole(t, f.a.st, project, bob.ID, store.GroupMemberRoleMember)
	agent := &store.Agent{ID: tid("two-srv-agent"), ProjectID: project.ID, Name: "two-srv-agent", Slug: "two-srv-agent",
		Phase: "running", OwnerID: bob.ID, CreatedBy: bob.ID}
	require.NoError(t, f.a.st.CreateAgent(ctx, agent))
	f.topicID = tid("two-srv-topic")
	require.NoError(t, f.a.wcs.CreateTopic(ctx, WebChatTopic{ID: f.topicID, ProjectID: project.ID, Name: "two-srv",
		CreatedBy: bob.ID, CreatedAt: time.Now().UTC(), DefaultAgent: agent.Slug}))
	setTopicConversationID(t, f.a.db, f.a.st, f.topicID, project.ID)
	f.bob = bob

	f.sms = scheduledMessageStoreFrom(f.a.wcs)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		m := newTestScheduledRow(fmt.Sprintf("two-srv-%d", i), bob.ID, base.Add(time.Duration(i)*time.Second))
		m.ConversationKey = f.topicID
		_, _, err := f.sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
		f.rowIDs = append(f.rowIDs, m.ID)
	}
	return f
}

// assertAllDeliveredOnce checks n delivered messages, n dispatches across
// both servers, and nothing left pending or sending.
func (f *twoServerSQLite) assertAllDeliveredOnce(t *testing.T, n int, dispatches int) {
	t.Helper()
	ctx := context.Background()
	msgs, err := f.a.st.ListMessages(ctx, store.MessageFilter{ThreadID: f.topicID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, msgs.Items, n, "one delivered message per scheduled message")
	assert.Equal(t, n, dispatches, "one dispatch per scheduled message")
	active, err := f.sms.CountActiveScheduledMessages(ctx, f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, active)
}

// Two hub servers sharing one SQLite database run the real sweeper on one
// sender's due messages at the same time: every message is delivered
// exactly once.
func TestScheduledSend_TwoServersOneSQLiteFile_OneSenderManyRows(t *testing.T) {
	const n = 20
	f := newTwoServerSQLite(t, n)
	dispA, dispB := &brokerMockDispatcher{}, &brokerMockDispatcher{}
	f.a.srv.SetDispatcher(dispA)
	f.b.srv.SetDispatcher(dispB)

	ctx := context.Background()
	now := time.Now()
	var claimedA, claimedB int
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; claimedA = f.a.srv.sweepScheduledMessages(ctx, now) }()
	go func() { defer wg.Done(); <-start; claimedB = f.b.srv.sweepScheduledMessages(ctx, now) }()
	close(start)
	wg.Wait()
	t.Logf("claims per server: a=%d b=%d", claimedA, claimedB)
	assert.Equal(t, n, claimedA+claimedB, "each message claimed by exactly one server")
	f.assertAllDeliveredOnce(t, n, len(dispA.getMessages())+len(dispB.getMessages()))
}

// Deterministic two-server run: server a claims the sender's oldest
// message and is held in its dispatch; server b's sweep then skips that
// claimed message and delivers all the others; once a is released it
// finishes its one message and claims nothing more.
func TestScheduledSend_TwoServersOneSQLiteFile_SecondServerTakesTheRest(t *testing.T) {
	const n = 10
	f := newTwoServerSQLite(t, n)
	dispA := &senderBlockingDispatcher{blocked: map[string]bool{f.bob.ID: true}, release: make(chan struct{})}
	dispB := &brokerMockDispatcher{}
	f.a.srv.SetDispatcher(dispA)
	f.b.srv.SetDispatcher(dispB)
	ctx := context.Background()
	now := time.Now()

	resultA := make(chan int, 1)
	go func() { resultA <- f.a.srv.sweepScheduledMessages(ctx, now) }()
	require.Eventually(t, func() bool {
		row, err := f.sms.GetScheduledMessage(ctx, f.bob.ID, f.rowIDs[0])
		return err == nil && row != nil && row.Status == ScheduledMessageSending
	}, 5*time.Second, 5*time.Millisecond, "server a holds the oldest message")

	assert.Equal(t, n-1, f.b.srv.sweepScheduledMessages(ctx, now), "server b delivers every other message")
	row, err := f.sms.GetScheduledMessage(ctx, f.bob.ID, f.rowIDs[0])
	require.NoError(t, err)
	assert.Equal(t, ScheduledMessageSending, row.Status, "server b left a's claimed message alone")

	close(dispA.release)
	assert.Equal(t, 1, <-resultA, "server a finishes its one message and claims nothing more")
	f.assertAllDeliveredOnce(t, n, len(dispA.getMessages())+len(dispB.getMessages()))
}

// Stopping honours the caller's deadline in the wait after the abort too:
// a delivery that does not finish after being cut short does not hold the
// stop for the full finalize timeout.
func TestScheduledSend_StopHonoursDeadlineAfterAbort(t *testing.T) {
	f := newScheduledSendFixture(t)
	rt := f.srv.scheduledRuntime()
	rt.running.Add(1) // stands in for a delivery that never finishes
	t.Cleanup(rt.running.Done)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	f.srv.stopScheduledSendSweeper(ctx)
	assert.Less(t, time.Since(start), scheduledFinalizeTimeout/2, "returned at the caller's deadline, not after the finalize timeout")
	assert.Error(t, rt.abortCtx.Err(), "deliveries were cut short")
}
