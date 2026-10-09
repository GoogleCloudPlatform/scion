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

// Tests for scheduled send in direct messages (ptone/scion#3666, phase 2):
// user-to-agent and user-to-user DMs, the DM checks at fire time, and
// deletion with the DM.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scheduledDMKey(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}

func scheduledConversationPath(key string) string {
	return "/api/v1/chat/conversations/" + key + "/scheduled"
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

// A message scheduled in a DM with an agent is sent as the user at fire
// time, through the ordinary DM routing, to that agent.
func TestScheduledSend_AgentDM_DeliversAsSender(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)

	sm := f.scheduleIn(t, f.bob, key, "dm to my agent later")
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, f.project.ID, row.ProjectID, "an agent DM stores the agent's project, for cleanup only")
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, scheduledConversationPath(key), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), sm.ID)
	assert.Empty(t, f.threadMessages(t, key), "nothing in the DM before fire time")

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row = f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageSent, row.Status)
	msgs := f.threadMessages(t, key)
	require.Len(t, msgs, 1)
	assert.Equal(t, row.MessageID, msgs[0].ID)
	assert.Equal(t, f.bob.ID, msgs[0].SenderID)
	assert.Equal(t, "dm to my agent later", msgs[0].Msg)
	dispatched := f.dispatcher.getMessages()
	require.Len(t, dispatched, 1)
	assert.Equal(t, f.agent.Slug, dispatched[0].agentSlug)
	assert.False(t, dispatched[0].interrupt)
}

// A user-to-user DM has no project; the message is sent to the peer.
func TestScheduledSend_UserDM_Delivers(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	key := scheduledDMKey(t, "user", f.alice.ID, "user", f.bob.ID)
	if key != "dm:user:"+f.alice.ID+":user:"+f.bob.ID {
		key = scheduledDMKey(t, "user", f.bob.ID, "user", f.alice.ID)
	}

	sm := f.scheduleIn(t, f.bob, key, "hello alice later")
	assert.Empty(t, f.row(t, f.bob, sm.ID).ProjectID, "user DMs have no project")
	// alice is a participant but cannot see bob's scheduled message.
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, scheduledConversationPath(key), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sm.ID)

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, sm.ID).Status)
	msgs := f.threadMessages(t, key)
	require.Len(t, msgs, 1)
	assert.Equal(t, f.bob.ID, msgs[0].SenderID)
	assert.Empty(t, f.dispatcher.getMessages(), "no agent involved")
}

// Only a participant may schedule into a DM, and only the sender's rows
// are visible or changeable.
func TestScheduledSend_DM_NonParticipantRefused(t *testing.T) {
	f := newScheduledSendFixture(t)
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
	sm := f.scheduleIn(t, f.bob, key, "private dm")
	path := scheduledConversationPath(key)

	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, path, map[string]interface{}{
		"content": "x", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.alice, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doRequestAsUser(t, f.srv, f.alice, http.MethodDelete, path+"/"+sm.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, sm.ID).Status)

	// The sender can cancel it.
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, path+"/"+sm.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// The agent's messaging rules are checked again at fire time: an agent
// the sender may no longer message gets nothing, and the row fails as
// no_access.
func TestScheduledSend_AgentDM_MessageDeniedAtFire_NoAccess(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
	sm := f.scheduleIn(t, f.bob, key, "sealed later")

	agent, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	agent.MessageMode = store.MessageModeNone
	require.NoError(t, f.store.UpdateAgent(ctx, agent))

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.threadMessages(t, key))
	assert.Empty(t, f.dispatcher.getMessages())
}

// A deleted agent receives nothing: soft-deleted is recipient_gone,
// removed entirely is no_access (the same refusal as a live send).
func TestScheduledSend_AgentDM_DeletedAgent(t *testing.T) {
	t.Run("soft deleted", func(t *testing.T) {
		f := newScheduledSendFixture(t)
		ctx := context.Background()
		key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
		sm := f.scheduleIn(t, f.bob, key, "to a deleted agent")
		agent, err := f.store.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		agent.DeletedAt = time.Now()
		require.NoError(t, f.store.UpdateAgent(ctx, agent))

		assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
		row := f.row(t, f.bob, sm.ID)
		assert.Equal(t, ScheduledMessageFailed, row.Status)
		assert.Equal(t, ScheduledFailureRecipientGone, row.FailureReason)
		assert.Empty(t, f.threadMessages(t, key))
		assert.Empty(t, f.dispatcher.getMessages())
	})
	t.Run("removed", func(t *testing.T) {
		f := newScheduledSendFixture(t)
		ctx := context.Background()
		key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
		sm := f.scheduleIn(t, f.bob, key, "to a removed agent")
		require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))

		assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
		row := f.row(t, f.bob, sm.ID)
		assert.Equal(t, ScheduledMessageFailed, row.Status)
		assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
		assert.Empty(t, f.threadMessages(t, key))
		assert.Empty(t, f.dispatcher.getMessages())
	})
}

// The other participant of a user DM is checked again at fire time: a
// suspended or removed user gets nothing.
func TestScheduledSend_UserDM_PeerGone_NoAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f *scheduledSendFixture, peer *store.User)
	}{
		{"suspended", func(t *testing.T, f *scheduledSendFixture, peer *store.User) {
			u, err := f.store.GetUser(context.Background(), peer.ID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(context.Background(), u))
		}},
		{"removed", func(t *testing.T, f *scheduledSendFixture, peer *store.User) {
			require.NoError(t, f.store.DeleteUser(context.Background(), peer.ID))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScheduledSendFixture(t)
			ctx := context.Background()
			peer := &store.User{ID: tid("sched-dm-peer-" + tc.name), Email: "peer-" + tc.name + "@test.com",
				DisplayName: "Peer", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
			require.NoError(t, f.store.CreateUser(ctx, peer))
			key := scheduledDMKey(t, "user", f.bob.ID, "user", peer.ID)
			if key != "dm:user:"+f.bob.ID+":user:"+peer.ID {
				key = scheduledDMKey(t, "user", peer.ID, "user", f.bob.ID)
			}
			sm := f.scheduleIn(t, f.bob, key, "to a peer that goes away "+tc.name)
			tc.change(t, f, peer)

			assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
			row := f.row(t, f.bob, sm.ID)
			assert.Equal(t, ScheduledMessageFailed, row.Status)
			assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
			assert.Empty(t, f.threadMessages(t, key))
		})
	}
}

// Send now and dismiss work in DMs; Send now runs the DM checks again.
func TestScheduledSend_DM_SendNowAndDismiss(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
	now := time.Now().UTC()
	missed := newTestScheduledRow("dm-missed", f.bob.ID, now.Add(-2*time.Hour))
	missed.ConversationKey = key
	_, _, err := f.sms.CreateScheduledMessage(ctx, missed)
	require.NoError(t, err)
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, now))
	assert.Equal(t, ScheduledFailureMissed, f.row(t, f.bob, missed.ID).FailureReason)

	path := scheduledConversationPath(key) + "/" + missed.ID
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, path+"/send-now", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "not a participant")
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path+"/send-now", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	time.Sleep(time.Until(f.row(t, f.bob, missed.ID).FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, missed.ID).Status)
	assert.Len(t, f.threadMessages(t, key), 1)

	failed := newTestScheduledRow("dm-failed", f.bob.ID, now)
	failed.ConversationKey = key
	_, _, err = f.sms.CreateScheduledMessage(ctx, failed)
	require.NoError(t, err)
	failRow(t, f.sms, failed.ID, ScheduledFailureNoAccess, now)
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, scheduledConversationPath(key)+"/"+failed.ID+"/dismiss", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, ScheduledMessageCancelled, f.row(t, f.bob, failed.ID).Status)
}

// Deleting a DM deletes its scheduled messages.
func TestScheduledSend_DMDelete_DeletesScheduled(t *testing.T) {
	f := newScheduledSendFixture(t)
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
	sm := f.scheduleIn(t, f.bob, key, "doomed dm")
	topic := f.schedule(t, f.bob, "topic survives", time.Now().Add(time.Hour))
	require.NoError(t, f.wcs.DeleteDM(context.Background(), key))
	got, err := f.sms.GetScheduledMessage(context.Background(), f.bob.ID, sm.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.NotNil(t, f.row(t, f.bob, topic.ID), "other conversations are kept")
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

func TestScheduledStore_SQLite_DMDelete(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testScheduledDMDeleteRemovesScheduled(t, sms[0])
}

// dmPeerFaultStore fails the DM peer lookups of a scheduled delivery with
// a store error (not "not found") while armed.
type dmPeerFaultStore struct {
	store.Store
	fault  *storeFaultSwitch
	peerID string
}

func (w *dmPeerFaultStore) faulty(ctx context.Context, id string) bool {
	exec, _ := ExecutorContextFromContext(ctx)
	return w.fault.Active() && exec.Kind == scheduledSendClientType && id == w.peerID
}

func (w *dmPeerFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if w.faulty(ctx, id) {
		return nil, errors.New("injected store error")
	}
	return w.Store.GetAgent(ctx, id)
}

func (w *dmPeerFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if w.faulty(ctx, id) {
		return nil, errors.New("injected store error")
	}
	return w.Store.GetUser(ctx, id)
}

// A store error while checking the DM peer at fire time hands the row back
// to pending (claim cleared); nothing is sent.
func TestScheduledSend_DM_PeerLookupError_Released(t *testing.T) {
	for _, peerKind := range []string{"agent", "user"} {
		t.Run(peerKind, func(t *testing.T) {
			srv, s, alice, bob, project, wrapped, fault := setupDemoPolicyTestWithFault(t,
				func(inner store.Store, fault *storeFaultSwitch) *dmPeerFaultStore {
					return &dmPeerFaultStore{Store: inner, fault: fault}
				})
			f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
			ctx := context.Background()
			key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
			wrapped.peerID = f.agent.ID
			if peerKind == "user" {
				key = scheduledDMKey(t, "user", f.alice.ID, "user", f.bob.ID)
				if key != "dm:user:"+f.alice.ID+":user:"+f.bob.ID {
					key = scheduledDMKey(t, "user", f.bob.ID, "user", f.alice.ID)
				}
				wrapped.peerID = f.alice.ID
			}
			sm := f.scheduleIn(t, f.bob, key, "peer lookup fails "+peerKind)
			fault.Arm()

			assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
			row := f.row(t, f.bob, sm.ID)
			assert.Equal(t, ScheduledMessagePending, row.Status)
			assert.Nil(t, row.ClaimedAt)
			assert.Empty(t, f.threadMessages(t, key))
			assert.Empty(t, f.dispatcher.getMessages())
		})
	}
}

// Send now runs the DM checks again before sending: an agent that stopped
// accepting the sender's messages after Send now gets nothing.
func TestScheduledSend_DM_SendNowRunsDMChecks(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
	now := time.Now().UTC()
	m := newTestScheduledRow("dm-sn-checks", f.bob.ID, now.Add(-2*time.Hour))
	m.ConversationKey = key
	_, _, err := f.sms.CreateScheduledMessage(ctx, m)
	require.NoError(t, err)
	failRow(t, f.sms, m.ID, ScheduledFailureMissed, now)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, scheduledConversationPath(key)+"/"+m.ID+"/send-now", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agent, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	agent.MessageMode = store.MessageModeNone
	require.NoError(t, f.store.UpdateAgent(ctx, agent))

	time.Sleep(time.Until(f.row(t, f.bob, m.ID).FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.threadMessages(t, key))
	assert.Empty(t, f.dispatcher.getMessages())
}

// A DM message that failed because the peer changed stays listed for its
// sender, who can dismiss it. A non-participant still gets the same
// refusal as a live send.
func TestScheduledSend_DM_FailedRowListedAfterPeerChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *scheduledSendFixture) string
	}{
		{"agent stops accepting messages", func(t *testing.T, f *scheduledSendFixture) string {
			key := scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID)
			f.scheduleIn(t, f.bob, key, "agent changes")
			agent, err := f.store.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			agent.MessageMode = store.MessageModeNone
			require.NoError(t, f.store.UpdateAgent(context.Background(), agent))
			return key
		}},
		{"peer user suspended", func(t *testing.T, f *scheduledSendFixture) string {
			peer := &store.User{ID: tid("sched-dm-list-peer"), Email: "list-peer@test.com",
				DisplayName: "Peer", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
			require.NoError(t, f.store.CreateUser(context.Background(), peer))
			key := scheduledDMKey(t, "user", f.bob.ID, "user", peer.ID)
			if key != "dm:user:"+f.bob.ID+":user:"+peer.ID {
				key = scheduledDMKey(t, "user", peer.ID, "user", f.bob.ID)
			}
			f.scheduleIn(t, f.bob, key, "peer changes")
			peer.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(context.Background(), peer))
			return key
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScheduledSendFixture(t)
			key := tc.setup(t, f)
			assert.Equal(t, 1, f.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))

			rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, scheduledConversationPath(key), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body struct {
				ScheduledMessages []scheduledMessageResponse `json:"scheduledMessages"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.ScheduledMessages, 1)
			failed := body.ScheduledMessages[0]
			assert.Equal(t, ScheduledMessageFailed, failed.Status)
			assert.Equal(t, ScheduledFailureNoAccess, failed.FailureReason)

			rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, scheduledConversationPath(key)+"/"+failed.ID+"/dismiss", nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			rec = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, scheduledConversationPath(key), nil)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.NotContains(t, rec.Body.String(), failed.ID)
		})
	}
}

// For a non-participant or a malformed key, the DM list answers exactly
// what a live send answers.
func TestScheduledSend_DM_ListRefusalsMatchLiveSend(t *testing.T) {
	f := newScheduledSendFixture(t)
	outsider := &store.User{ID: tid("sched-dm-outsider"), Email: "dm-outsider@test.com",
		DisplayName: "Out", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, f.store.CreateUser(context.Background(), outsider))
	for _, key := range []string{
		scheduledDMKey(t, "agent", f.agent.ID, "user", f.bob.ID), // not a participant
		"dm:agent:not-a-uuid", // malformed
	} {
		list := doRequestAsUser(t, f.srv, outsider, http.MethodGet, scheduledConversationPath(key), nil)
		live := doRequestAsUser(t, f.srv, outsider, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages",
			map[string]interface{}{"content": "hi"})
		assert.Equal(t, live.Code, list.Code, key)
		assert.Equal(t, live.Body.String(), list.Body.String(), key)
		assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, list.Code)
	}
}
