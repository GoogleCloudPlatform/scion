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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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
	fireAt := time.Now().Add(2 * time.Second)
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

	fireAt := time.Now().Add(2 * time.Second)
	sm := f.schedule(t, f.bob, "scheduled hello", fireAt)

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
	assert.Equal(t, []string{"created", "sent"}, scheduledEventActions(t, collectEvents(bobEvents)))
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
	fireAt := time.Now().Add(2 * time.Second)
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
	sm := f.schedule(t, f.bob, "claimed first", time.Now().Add(2*time.Second))

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
	fireAt := time.Now().Add(2 * time.Second)
	f.schedule(t, f.bob, "only once", fireAt)

	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- f.srv.sweepScheduledMessages(ctx, fireAt) }()
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
	fireAt := time.Now().Add(2 * time.Second)
	sm := f.schedule(t, f.bob, "after losing access", fireAt)

	// bob is removed from the project after scheduling.
	membersGroup, err := f.store.GetGroupBySlug(ctx, "project:"+f.project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, f.bob.ID))
	_, err = f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.bob.ID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, f.scheduledPath(), nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "bob has lost read access")

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.topicMessages(t), "nothing posted")
	assert.Empty(t, f.dispatcher.getMessages())
}

func TestScheduledSend_SenderSuspended_FailsSenderInactive(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Second)
	sm := f.schedule(t, f.bob, "suspended sender", fireAt)

	u, err := f.store.GetUser(ctx, f.bob.ID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, u))

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureSenderInactive, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

func TestScheduledSend_TopicDeleted_FailsConversationGone(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Second)
	sm := f.schedule(t, f.bob, "topic gone", fireAt)

	_, err := f.db.ExecContext(ctx, `UPDATE webchat_topic SET deleted_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), f.topicID)
	require.NoError(t, err)

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt))
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
	fireAt := time.Now().Add(2 * time.Second)
	sm := f.schedule(t, f.bob, "to alice's agent", fireAt)

	// The default agent changes to one bob may not message.
	other := &store.Agent{
		ID: tid("sched-alice-agent"), ProjectID: f.project.ID, Name: "alice-agent", Slug: "alice-agent",
		Phase: "running", OwnerID: f.alice.ID, CreatedBy: f.alice.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, other))
	_, err := f.db.ExecContext(ctx, `UPDATE webchat_topic SET default_agent = ? WHERE id = ?`, other.Slug, f.topicID)
	require.NoError(t, err)

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt))
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
	fireAt := time.Now().Add(2 * time.Second)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content":     "reply later",
		"fire_at":     fireAt.UTC().Format(time.RFC3339Nano),
		"reply_to_id": tid("no-such-message"),
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt))
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
	base := time.Now().UTC().Truncate(time.Millisecond)

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
	// with and without fractional seconds must compare correctly.
	early := newTestScheduledRow("t2", "user-a", base.Add(-2*time.Second).Truncate(time.Second))
	late := newTestScheduledRow("t3", "user-a", base.Add(-time.Second).Add(500*time.Millisecond))
	for _, r := range []*ScheduledChatMessage{early, late} {
		_, _, err := sms.CreateScheduledMessage(ctx, r)
		require.NoError(t, err)
	}
	due, err := sms.ListDueScheduledMessages(ctx, base, 10)
	require.NoError(t, err)
	require.Len(t, due, 2)
	assert.Equal(t, early.ID, due[0].ID)
	assert.Equal(t, late.ID, due[1].ID)

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
	require.Len(t, list, 1)
	assert.Equal(t, late.ID, list[0].ID)
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
