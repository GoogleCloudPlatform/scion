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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userDMKey(t *testing.T, a, b string) string {
	t.Helper()
	key, err := messages.DMConversationKey("user", a, "user", b)
	require.NoError(t, err)
	return key
}

// mustCreateActiveUser creates an active user with ID tid(name), for tests
// that need a real DM peer.
func mustCreateActiveUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	u := &store.User{ID: tid(name), Email: name + "@example.com", DisplayName: name,
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u.ID
}

func newMemberFanoutFixture(t *testing.T) *memberFanoutFixture {
	t.Helper()
	srv, s, alice, bob, proj := setupDemoPolicyTest(t)
	return newMemberFanoutFixtureOn(t, srv, s, alice, bob, proj)
}

// requireLiveSendAnswer asserts that rec carries exactly the answer a live
// send of key by user gets.
func requireLiveSendAnswer(t *testing.T, srv *Server, user *store.User, key string, rec *httptest.ResponseRecorder) {
	t.Helper()
	live := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages",
		map[string]interface{}{"content": "x"})
	assert.Equal(t, live.Code, rec.Code, "status must match a live send: %s", rec.Body.String())
	assert.Equal(t, live.Body.String(), rec.Body.String(), "body must match a live send")
}

// testScheduledDMDeleteRemovesScheduled checks DeleteDM on a store (run on
// Postgres from TestScheduledStore_Postgres).
func testScheduledDMDeleteRemovesScheduled(t *testing.T, sms ScheduledMessageStore) {
	t.Helper()
	ctx := context.Background()
	wcs, ok := sms.(WebChatStore)
	require.True(t, ok)
	key := "dm:agent:" + tid("dmdel-agent") + ":user:" + tid("dmdel-user")
	m := newTestScheduledRow("dmdel", "dmdel-user", time.Now().Add(time.Hour))
	m.ConversationKey = key
	_, _, err := sms.CreateScheduledMessage(ctx, m)
	require.NoError(t, err)
	keep := newTestScheduledRow("dmdel-keep", "dmdel-user", time.Now().Add(time.Hour))
	keep.ConversationKey = "dmdel-other"
	_, _, err = sms.CreateScheduledMessage(ctx, keep)
	require.NoError(t, err)
	require.NoError(t, wcs.DeleteDM(ctx, key))
	got, err := sms.GetScheduledMessage(ctx, "dmdel-user", m.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = sms.GetScheduledMessage(ctx, "dmdel-user", keep.ID)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// mustApply returns a check that a fenced store write applied.
func mustApply(t *testing.T) func(bool, error) {
	t.Helper()
	return func(ok bool, err error) {
		t.Helper()
		require.NoError(t, err)
		require.True(t, ok, "the write did not apply")
	}
}

// failRow claims a pending row and records it failed with reason.
func failRow(t *testing.T, sms ScheduledMessageStore, id, reason string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	ok, err := sms.ClaimScheduledMessage(ctx, id, at)
	require.NoError(t, err)
	require.True(t, ok)
	mustApply(t)(sms.MarkScheduledMessageFailed(ctx, id, reason, at, at))
}

func testScheduledStorePhase2Transitions(t *testing.T, sms ScheduledMessageStore) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	create := func(name, sender string) *ScheduledChatMessage {
		m := newTestScheduledRow("p2-"+name, sender, base.Add(-time.Minute))
		m.ConversationKey = "p2-conv"
		row, _, err := sms.CreateScheduledMessage(ctx, m)
		require.NoError(t, err)
		return row
	}
	get := func(sender, id string) *ScheduledChatMessage {
		row, err := sms.GetScheduledMessage(ctx, sender, id)
		require.NoError(t, err)
		return row
	}

	// Send now: only failed missed or interrupted rows, only the sender's.
	missed := create("missed", "p2-a")
	failRow(t, sms, missed.ID, ScheduledFailureMissed, base)
	ok, err := sms.SendNowScheduledMessage(ctx, "p2-b", missed.ID, base, base)
	require.NoError(t, err)
	assert.False(t, ok, "another sender cannot send it now")
	fireAt := base.Add(10 * time.Second).Add(300 * time.Millisecond)
	ok, err = sms.SendNowScheduledMessage(ctx, "p2-a", missed.ID, fireAt, base.Add(10*time.Second))
	require.NoError(t, err)
	assert.True(t, ok)
	got := get("p2-a", missed.ID)
	assert.Equal(t, ScheduledMessagePending, got.Status)
	assert.Empty(t, got.FailureReason)
	assert.Nil(t, got.ClaimedAt)
	assert.True(t, got.FireAt.Equal(base.Add(11*time.Second)), "fire time rounded up to the whole second: %v", got.FireAt)
	ok, err = sms.SendNowScheduledMessage(ctx, "p2-a", missed.ID, base, base)
	require.NoError(t, err)
	assert.False(t, ok, "a pending row is not sent now again")

	interrupted := create("interrupted", "p2-a")
	failRow(t, sms, interrupted.ID, ScheduledFailureInterrupted, base)
	ok, err = sms.SendNowScheduledMessage(ctx, "p2-a", interrupted.ID, base, base)
	require.NoError(t, err)
	assert.True(t, ok)

	noAccess := create("no-access", "p2-a")
	failRow(t, sms, noAccess.ID, ScheduledFailureNoAccess, base)
	ok, err = sms.SendNowScheduledMessage(ctx, "p2-a", noAccess.ID, base, base)
	require.NoError(t, err)
	assert.False(t, ok, "only missed and interrupted can be sent now")

	// Dismiss: failed rows only, only the sender's; then no longer listed.
	ok, err = sms.DismissScheduledMessage(ctx, "p2-b", noAccess.ID, base)
	require.NoError(t, err)
	assert.False(t, ok, "another sender cannot dismiss it")
	ok, err = sms.DismissScheduledMessage(ctx, "p2-a", noAccess.ID, base)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, ScheduledMessageCancelled, get("p2-a", noAccess.ID).Status)
	ok, err = sms.DismissScheduledMessage(ctx, "p2-a", missed.ID, base)
	require.NoError(t, err)
	assert.False(t, ok, "a pending row cannot be dismissed")
	list, err := sms.ListScheduledMessages(ctx, "p2-a", "p2-conv")
	require.NoError(t, err)
	for _, m := range list {
		assert.NotEqual(t, noAccess.ID, m.ID, "a dismissed row is not listed")
	}

	// Stuck rows: listed by claim age; marking needs the same claim.
	oldClaim := base.Add(-time.Hour).Add(123456 * time.Microsecond)
	stuck := create("stuck", "p2-c")
	ok, err = sms.ClaimScheduledMessage(ctx, stuck.ID, oldClaim)
	require.NoError(t, err)
	require.True(t, ok)
	fresh := create("fresh", "p2-c2")
	ok, err = sms.ClaimScheduledMessage(ctx, fresh.ID, base)
	require.NoError(t, err)
	require.True(t, ok)
	found, err := sms.ListStuckScheduledMessages(ctx, base.Add(-time.Minute), 10)
	require.NoError(t, err)
	var stuckRow *ScheduledChatMessage
	for i := range found {
		assert.NotEqual(t, fresh.ID, found[i].ID, "a recent claim is not stuck")
		if found[i].ID == stuck.ID {
			stuckRow = &found[i]
		}
	}
	require.NotNil(t, stuckRow)
	require.NotNil(t, stuckRow.ClaimedAt)
	ok, err = sms.MarkScheduledMessageInterrupted(ctx, stuck.ID, stuckRow.ClaimedAt.Add(time.Second), base)
	require.NoError(t, err)
	assert.False(t, ok, "a different claim is not marked")
	ok, err = sms.MarkScheduledMessageInterrupted(ctx, stuck.ID, *stuckRow.ClaimedAt, base)
	require.NoError(t, err)
	assert.True(t, ok)
	got = get("p2-c", stuck.ID)
	assert.Equal(t, ScheduledMessageFailed, got.Status)
	assert.Equal(t, ScheduledFailureInterrupted, got.FailureReason)
	ok, err = sms.MarkScheduledMessageInterrupted(ctx, stuck.ID, *stuckRow.ClaimedAt, base)
	require.NoError(t, err)
	assert.False(t, ok, "a failed row is not marked again")
	// A row released and claimed again after listing keeps its new claim.
	again := create("again", "p2-d")
	ok, err = sms.ClaimScheduledMessage(ctx, again.ID, oldClaim)
	require.NoError(t, err)
	require.True(t, ok)
	found, err = sms.ListStuckScheduledMessages(ctx, base.Add(-time.Minute), 10)
	require.NoError(t, err)
	var againRow *ScheduledChatMessage
	for i := range found {
		if found[i].ID == again.ID {
			againRow = &found[i]
		}
	}
	require.NotNil(t, againRow)
	mustApply(t)(sms.ReleaseScheduledMessage(ctx, again.ID, oldClaim, base))
	ok, err = sms.ClaimScheduledMessage(ctx, again.ID, base)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = sms.MarkScheduledMessageInterrupted(ctx, again.ID, *againRow.ClaimedAt, base)
	require.NoError(t, err)
	assert.False(t, ok, "the new claim is not marked")
	assert.Equal(t, ScheduledMessageSending, get("p2-d", again.ID).Status)
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, again.ID, "msg-again", base, base))
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, fresh.ID, "msg-fresh", base, base))

	// Final writes are fenced by the claim: a delivery whose row was marked
	// interrupted and claimed again (Send now) cannot finalize the new claim.
	fenced := create("fenced", "p2-g")
	firstClaim := base.Add(-time.Hour).Add(7 * time.Microsecond)
	ok, err = sms.ClaimScheduledMessage(ctx, fenced.ID, firstClaim)
	require.NoError(t, err)
	require.True(t, ok)
	mustApply(t)(sms.MarkScheduledMessageInterrupted(ctx, fenced.ID, firstClaim, base))
	mustApply(t)(sms.SendNowScheduledMessage(ctx, "p2-g", fenced.ID, base, base))
	ok, err = sms.ClaimScheduledMessage(ctx, fenced.ID, base)
	require.NoError(t, err)
	require.True(t, ok)
	for name, write := range map[string]func() (bool, error){
		"sent":    func() (bool, error) { return sms.MarkScheduledMessageSent(ctx, fenced.ID, "stale", firstClaim, base) },
		"failed":  func() (bool, error) { return sms.MarkScheduledMessageFailed(ctx, fenced.ID, "x", firstClaim, base) },
		"release": func() (bool, error) { return sms.ReleaseScheduledMessage(ctx, fenced.ID, firstClaim, base) },
	} {
		ok, err := write()
		require.NoError(t, err)
		assert.False(t, ok, "a stale %s write does not apply to the new claim", name)
	}
	got = get("p2-g", fenced.ID)
	assert.Equal(t, ScheduledMessageSending, got.Status)
	assert.Empty(t, got.MessageID)
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, fenced.ID, "current", base, base))
	assert.Equal(t, "current", get("p2-g", fenced.ID).MessageID)

	// A claim with nanoseconds, as production makes, is fenced by the same
	// value.
	nanos := create("nanos", "p2-h")
	nanoClaim := base.Add(123456789 * time.Nanosecond)
	mustApply(t)(sms.ClaimScheduledMessage(ctx, nanos.ID, nanoClaim))
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, nanos.ID, "nano-msg", nanoClaim, base))
	assert.Equal(t, "nano-msg", get("p2-h", nanos.ID).MessageID)
	nanosFailed := create("nanos-failed", "p2-h")
	mustApply(t)(sms.ClaimScheduledMessage(ctx, nanosFailed.ID, nanoClaim))
	mustApply(t)(sms.MarkScheduledMessageFailed(ctx, nanosFailed.ID, ScheduledFailureNoAccess, nanoClaim, base))
	nanosReleased := create("nanos-released", "p2-h")
	mustApply(t)(sms.ClaimScheduledMessage(ctx, nanosReleased.ID, nanoClaim))
	mustApply(t)(sms.ReleaseScheduledMessage(ctx, nanosReleased.ID, nanoClaim, base))

	// Purge: sent and cancelled after 7 days, failed after 30; pending and
	// sending kept whatever their age.
	day := 24 * time.Hour
	old := base.Add(-8 * day)
	ancient := base.Add(-31 * day)
	oldSent := create("old-sent", "p2-e")
	ok, err = sms.ClaimScheduledMessage(ctx, oldSent.ID, old)
	require.NoError(t, err)
	require.True(t, ok)
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, oldSent.ID, "m1", old, old))
	oldCancelled := create("old-cancelled", "p2-e")
	ok, err = sms.CancelScheduledMessage(ctx, "p2-e", oldCancelled.ID, old)
	require.NoError(t, err)
	require.True(t, ok)
	oldFailed := create("old-failed", "p2-e")
	failRow(t, sms, oldFailed.ID, ScheduledFailureNoAccess, old)
	ancientFailed := create("ancient-failed", "p2-e")
	failRow(t, sms, ancientFailed.ID, ScheduledFailureNoAccess, ancient)
	recentSent := create("recent-sent", "p2-e")
	ok, err = sms.ClaimScheduledMessage(ctx, recentSent.ID, base.Add(-6*day))
	require.NoError(t, err)
	require.True(t, ok)
	mustApply(t)(sms.MarkScheduledMessageSent(ctx, recentSent.ID, "m2", base.Add(-6*day), base.Add(-6*day)))
	oldPending := newTestScheduledRow("p2-old-pending", "p2-e", ancient)
	oldPending.ConversationKey = "p2-conv"
	oldPending.CreatedAt, oldPending.UpdatedAt = ancient, ancient
	_, _, err = sms.CreateScheduledMessage(ctx, oldPending)
	require.NoError(t, err)
	oldSending := newTestScheduledRow("p2-old-sending", "p2-e", ancient)
	oldSending.ConversationKey = "p2-conv"
	oldSending.CreatedAt, oldSending.UpdatedAt = ancient, ancient
	_, _, err = sms.CreateScheduledMessage(ctx, oldSending)
	require.NoError(t, err)
	ok, err = sms.ClaimScheduledMessage(ctx, oldSending.ID, ancient)
	require.NoError(t, err)
	require.True(t, ok)

	n, err := sms.PurgeScheduledMessages(ctx, base.Add(-7*day), base.Add(-30*day))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(3))
	assert.Nil(t, get("p2-e", oldSent.ID), "sent after 7 days purged")
	assert.Nil(t, get("p2-e", oldCancelled.ID), "cancelled after 7 days purged")
	assert.Nil(t, get("p2-e", ancientFailed.ID), "failed after 30 days purged")
	assert.NotNil(t, get("p2-e", oldFailed.ID), "failed kept for 30 days")
	assert.NotNil(t, get("p2-e", recentSent.ID), "sent kept for 7 days")
	assert.NotNil(t, get("p2-e", oldPending.ID), "pending never purged")
	assert.NotNil(t, get("p2-e", oldSending.ID), "sending never purged")

	// Delete with the conversation or the sender, whatever the status.
	other := newTestScheduledRow("p2-other-conv", "p2-e", base)
	other.ConversationKey = "p2-other"
	_, _, err = sms.CreateScheduledMessage(ctx, other)
	require.NoError(t, err)
	n, err = sms.DeleteScheduledMessagesForConversation(ctx, "p2-conv")
	require.NoError(t, err)
	assert.Greater(t, n, int64(0))
	assert.Nil(t, get("p2-e", oldSending.ID))
	assert.Nil(t, get("p2-a", missed.ID))
	assert.NotNil(t, get("p2-e", other.ID), "another conversation is kept")
	keep := newTestScheduledRow("p2-keep", "p2-f", base)
	keep.ConversationKey = "p2-other"
	_, _, err = sms.CreateScheduledMessage(ctx, keep)
	require.NoError(t, err)
	n, err = sms.DeleteScheduledMessagesForSender(ctx, "p2-e")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Nil(t, get("p2-e", other.ID))
	assert.NotNil(t, get("p2-f", keep.ID), "another sender is kept")
}

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

// scheduleIn creates a scheduled message in conversation key as user and
// moves its fire time to now, so the next sweep delivers it.
func (f *scheduledSendFixture) scheduleIn(t *testing.T, user *store.User, key, content string) scheduledMessageResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, scheduledConversationPath(key), map[string]interface{}{
		"content":         content,
		"fire_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "idem-" + content,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, err := f.db.ExecContext(context.Background(), `UPDATE webchat_scheduled_message SET fire_at = ? WHERE id = ?`,
		sqliteScheduledTime(time.Now().UTC().Truncate(time.Second)), resp.ID)
	require.NoError(t, err)
	return resp
}

func (f *scheduledSendFixture) threadMessages(t *testing.T, key string) []store.Message {
	t.Helper()
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{ThreadID: key}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return rows.Items
}

// insertScheduled stores a pending row for user in the fixture topic,
// without the create endpoint's time window.
func (f *scheduledSendFixture) insertScheduled(t *testing.T, user *store.User, name string, fireAt time.Time) *ScheduledChatMessage {
	t.Helper()
	m := newTestScheduledRow(name, user.ID, fireAt)
	m.ConversationKey = f.topicID
	m.ProjectID = f.project.ID
	m.Content = "content " + name
	row, existed, err := f.sms.CreateScheduledMessage(context.Background(), m)
	require.NoError(t, err)
	require.False(t, existed)
	return row
}

func (f *scheduledSendFixture) rowPath(id, suffix string) string {
	return f.scheduledPath() + "/" + id + suffix
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

// memberFanoutFixture is a project owned by alice with a thread whose
// conversation has these user participants:
//   - carol: project member, active participant (receives)
//   - dave: project member, left the thread (does not receive)
//   - bob: active participant row but not a project member (does not receive)
//   - sam: project member, active participant, suspended (does not receive)
//
// and erin, a project member who is not a participant (does not receive).
type memberFanoutFixture struct {
	srv                          *Server
	s                            store.Store
	wcs                          WebChatStore
	ep                           *ChannelEventPublisher
	proj                         *store.Project
	alice, bob, carol, dave, sam *store.User
	erin                         *store.User
	topicID, convID              string
}

func newMemberFanoutFixtureOn(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, proj *store.Project) *memberFanoutFixture {
	t.Helper()
	ctx := context.Background()

	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	member := func(name string) *store.User {
		u := &store.User{
			ID: api.NewUUID(), Email: name + "@test.com", DisplayName: name,
			Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		addProjectMemberWithRole(t, s, proj, u.ID, store.GroupMemberRoleMember)
		return u
	}
	f := &memberFanoutFixture{
		srv: srv, s: s, wcs: wcs, ep: ep, proj: proj, alice: alice, bob: bob,
		carol: member("carol"), dave: member("dave"), sam: member("sam"), erin: member("erin"),
	}

	f.topicID = api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: f.topicID, ProjectID: proj.ID, Name: "private plans",
		CreatedBy: alice.ID, CreatedAt: time.Now().UTC(),
	}))
	f.convID = topicConversationID(t, wcs, f.topicID)
	for _, u := range []*store.User{f.carol, f.dave, f.bob, f.sam} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.convID, PrincipalKind: "user", PrincipalID: u.ID, Role: "member",
		}))
	}
	require.NoError(t, s.RemoveParticipant(ctx, f.convID, "user", f.dave.ID))
	f.sam.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, f.sam))
	return f
}

func scheduledConversationPath(key string) string {
	return "/api/v1/chat/conversations/" + key + "/scheduled"
}

// threadMessage is a stored-looking web message in the fixture's thread.
func (f *memberFanoutFixture) threadMessage(content string) *store.Message {
	return &store.Message{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Sender: "user:" + f.alice.Email, SenderID: f.alice.ID,
		Msg: content, Type: "chat", Channel: "web", ThreadID: f.topicID, ConversationID: f.convID,
		CreatedAt: time.Now().UTC(),
	}
}
