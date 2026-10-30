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

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Phase 2 of starred messages: agent and human DMs, labels, deletion,
// promotion and lifecycle cleanup through the real handlers.

// dmConversation creates the direct conversation of a DM key (the read
// switch lists DM history by it) and returns its ID.
func (f *starFixture) dmConversation(t *testing.T, key string) string {
	t.Helper()
	id := uuid.NewString()
	at := time.Now().UTC().Add(-2 * time.Hour)
	require.NoError(t, f.s.CreateConversation(context.Background(), &store.Conversation{
		ID: id, Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active", LastActivityAt: at, CreatedAt: at,
	}))
	return id
}

// dmMsg stores a native DM message (projectID must be a UUID: production
// stores a DM between users under uuid.Nil) with thread ID threadID (the DM key; ""
// for a modern row matched only by its conversation) and conversation
// convID ("" for a legacy row). It is already dispatched, as a delivered
// DM message is: a pending one would make a promotion answer 409.
func (f *starFixture) dmMsg(t *testing.T, threadID, convID, projectID, sender, senderID, text, channel string, at time.Time) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateMessage(context.Background(), &store.Message{
		ID: id, ProjectID: projectID, Sender: sender, SenderID: senderID, Recipient: "chat",
		Msg: text, Type: "chat", Channel: channel, ThreadID: threadID, ConversationID: convID, CreatedAt: at.UTC(),
		DispatchState: store.MessageDispatchDispatched,
	}))
	return id
}

// starDMs is a member's agent DM (with agent A of project A) and human DM
// (with the owner), each with a direct conversation.
type starDMs struct {
	agentKey, agentConv string
	humanKey, humanConv string
}

func (f *starFixture) memberDMs(t *testing.T) starDMs {
	t.Helper()
	d := starDMs{agentKey: f.dm(t, f.member, "agent", f.agentA.ID), humanKey: f.dm(t, f.member, "user", f.owner)}
	d.agentConv = f.dmConversation(t, d.agentKey)
	d.humanConv = f.dmConversation(t, d.humanKey)
	return d
}

func decodeStarItem(t *testing.T, rec interface {
	Result() *http.Response
}) starItemResponse {
	t.Helper()
	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, res.StatusCode)
	var item starItemResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&item))
	return item
}

func TestChatStars_AgentAndHumanDMs(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			d := f.memberDMs(t)
			now := time.Now()
			agentMsg := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "agent:"+f.agentA.Slug, f.agentA.ID, "from the  agent", "web", now.Add(-time.Hour))
			// A human DM message stored with a project: its project is never
			// a human DM star's project. Its body is blanked after the insert
			// (the message store refuses an empty text): an empty stored body
			// is what the star excerpt shows as "Attachment".
			humanMsg := f.dmMsg(t, d.humanKey, d.humanConv, f.projB, "user:"+f.owner+"@test.com", f.owner, "to be blanked", "web", now.Add(-time.Minute))
			require.NoError(t, f.wcs.UpdateMessageContent(context.Background(), humanMsg, ""))
			thread := f.threadMsg(t, f.topicA, f.projA, "thread", now)

			ch, unsub := f.ep.Subscribe("user.*.chat.stars")
			defer unsub()
			decodeStarPut(t, f.put(t, f.member, agentMsg, d.agentKey))
			decodeStarPut(t, f.put(t, f.member, humanMsg, d.humanKey))
			decodeStarPut(t, f.put(t, f.member, thread, f.topicA))
			assert.Equal(t, []string{"user." + f.member + ".chat.stars", "user." + f.member + ".chat.stars", "user." + f.member + ".chat.stars"},
				starEvents(t, ch), "own subject only")

			// Stored project: the peer agent's for an agent DM, none for a
			// human DM.
			rec, err := f.stars.GetMessageStar(context.Background(), f.member, agentMsg)
			require.NoError(t, err)
			assert.Equal(t, f.projA, rec.ProjectID)
			rec, err = f.stars.GetMessageStar(context.Background(), f.member, humanMsg)
			require.NoError(t, err)
			assert.Equal(t, "", rec.ProjectID)

			// One state batch across the three conversations.
			assert.ElementsMatch(t, []string{agentMsg, humanMsg, thread},
				decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(agentMsg, humanMsg, thread), nil)))

			page := decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(d.agentKey), nil))
			require.Len(t, page.Items, 1)
			it := page.Items[0]
			assert.Equal(t, "dm", it.ConversationKind)
			assert.Equal(t, d.agentKey, it.ConversationKey)
			assert.Equal(t, f.projA, it.ProjectID)
			assert.Equal(t, "Inbox A", it.ProjectName)
			assert.Equal(t, f.agentA.Name, it.ConversationName)
			assert.Equal(t, f.agentA.Name, it.SenderName)
			assert.Equal(t, "from the agent", it.Excerpt)
			assert.Equal(t, "/chat/dm/"+strings.ReplaceAll(d.agentKey, ":", "%3A")+"#msg-"+agentMsg, it.Permalink)

			owner, err := f.s.GetUser(context.Background(), f.owner)
			require.NoError(t, err)
			ownerLabel := starUserLabel(owner)
			item := decodeStarItem(t, f.session(t, f.member, http.MethodGet, starPath(humanMsg), nil))
			assert.Equal(t, "dm", item.ConversationKind)
			assert.Equal(t, d.humanKey, item.ConversationKey)
			assert.Empty(t, item.ProjectID, "project fields are omitted for a human DM")
			assert.Empty(t, item.ProjectName)
			assert.Equal(t, ownerLabel, item.ConversationName)
			assert.Equal(t, ownerLabel, item.SenderName)
			assert.Equal(t, "Attachment", item.Excerpt, "an attachment-only message")
			raw := f.session(t, f.member, http.MethodGet, starPath(humanMsg), nil).Body.String()
			assert.NotContains(t, raw, "projectId")
			assert.NotContains(t, raw, "projectName")

			// The other DM participant sees only their own (empty) collection.
			assert.Empty(t, decodeStarPage(t, f.session(t, f.owner, http.MethodGet, starListPath(d.humanKey), nil)).Items)
			assert.Empty(t, decodeStarState(t, f.session(t, f.owner, http.MethodGet, starStatePath(humanMsg), nil)))

			// A non-participant gets the shared 404 on every route and an
			// empty state, even holding an association stored directly.
			_, err = f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, NULL, ?)`,
				f.admin, humanMsg, d.humanKey, sqliteStarTime(time.Now()))
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, f.put(t, f.admin, humanMsg, d.humanKey).Code)
			assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starListPath(d.humanKey), nil).Code)
			assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starPath(humanMsg), nil).Code)
			assert.Empty(t, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(humanMsg), nil)))
		})
	}
}

// TestChatStars_DMWrongConversationOrChannel: a message can be starred
// only under its own conversation's key and channel.
func TestChatStars_DMWrongConversationOrChannel(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			d := f.memberDMs(t)
			now := time.Now()
			human := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "human", "web", now)
			agent := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "user:"+f.member, f.member, "agent dm", "web", now)
			telegram := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "user:"+f.member, f.member, "other channel", "telegram", now)
			thread := f.threadMsg(t, f.topicA, f.projA, "thread", now)
			for name, c := range map[string]struct{ msg, key string }{
				"human DM message under the agent DM": {human, d.agentKey},
				"agent DM message under the human DM": {agent, d.humanKey},
				"thread message under a DM":           {thread, d.agentKey},
				"DM message under a thread":           {agent, f.topicA},
				"another channel":                     {telegram, d.agentKey},
			} {
				assert.Equal(t, http.StatusNotFound, f.put(t, f.member, c.msg, c.key).Code, name)
			}
			assert.Empty(t, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(human, agent, telegram, thread), nil)))
		})
	}
}

// TestChatStars_DMTokenBoundaries: inbox:write/inbox:read apply to DMs; an
// agent DM needs the agent's project inside a project boundary (and
// agent:read); a human DM needs a hub boundary; DELETE removes only a row
// whose stored project is inside the boundary, always with 204.
func TestChatStars_DMTokenBoundaries(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	d := f.memberDMs(t)
	agentMsg := f.dmMsg(t, d.agentKey, d.agentConv, f.projA, "user:"+f.member, f.member, "a", "web", time.Now())
	humanMsg := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "h", "web", time.Now())
	agentBody := map[string]string{"conversationKey": d.agentKey}
	humanBody := map[string]string{"conversationKey": d.humanKey}

	hubWriter := f.mint(t, f.member, hubBoundary(), "inbox:write", "agent:read")
	hubReader := f.mint(t, f.member, hubBoundary(), "inbox:read", "agent:read")
	projAWriter := f.mint(t, f.member, projectBoundary(f.projA), "inbox:write", "agent:read")
	projAReader := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "agent:read")
	// A project-boundary token can be minted only for a project its holder belongs to: give the member a role
	// in project B, whose boundary does not hold agent A's project.
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projB, store.ProjectRoleMember)
	projBWriter := f.mint(t, f.member, projectBoundary(f.projB), "inbox:write", "agent:read")

	assert.Equal(t, http.StatusForbidden, f.call(t, hubReader, http.MethodPut, starPath(agentMsg), agentBody).Code, "PUT needs inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, hubWriter, http.MethodGet, starPath(agentMsg), nil).Code, "reads need inbox:read")

	decodeStarPut(t, f.call(t, projAWriter, http.MethodPut, starPath(agentMsg), agentBody))
	assert.Equal(t, http.StatusNotFound, f.call(t, projBWriter, http.MethodPut, starPath(agentMsg), agentBody).Code, "agent's project outside the boundary")
	assert.Equal(t, http.StatusNotFound, f.call(t, projAWriter, http.MethodPut, starPath(humanMsg), humanBody).Code, "a human DM needs a hub boundary")
	decodeStarPut(t, f.call(t, hubWriter, http.MethodPut, starPath(humanMsg), humanBody))

	// Reads follow the same rule.
	assert.Equal(t, http.StatusOK, f.call(t, projAReader, http.MethodGet, starPath(agentMsg), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAReader, http.MethodGet, starPath(humanMsg), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAReader, http.MethodGet, starListPath(d.humanKey), nil).Code)
	assert.Equal(t, []string{agentMsg}, decodeStarState(t, f.call(t, projAReader, http.MethodGet, starStatePath(agentMsg, humanMsg), nil)))
	assert.ElementsMatch(t, []string{agentMsg, humanMsg}, decodeStarState(t, f.call(t, hubReader, http.MethodGet, starStatePath(agentMsg, humanMsg), nil)))

	// DELETE: a project token removes only a row whose stored project is in
	// its boundary; the human DM row (no project) stays, with the same 204.
	assert.Equal(t, http.StatusNoContent, f.call(t, projAWriter, http.MethodDelete, starPath(humanMsg), nil).Code)
	got, err := f.stars.GetMessageStar(ctx, f.member, humanMsg)
	require.NoError(t, err)
	assert.NotNil(t, got, "out-of-boundary DELETE removes nothing")
	assert.Equal(t, http.StatusNoContent, f.call(t, projBWriter, http.MethodDelete, starPath(agentMsg), nil).Code)
	got, err = f.stars.GetMessageStar(ctx, f.member, agentMsg)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, http.StatusNoContent, f.call(t, projAWriter, http.MethodDelete, starPath(agentMsg), nil).Code)
	assert.Equal(t, http.StatusNoContent, f.call(t, hubWriter, http.MethodDelete, starPath(humanMsg), nil).Code)
	for _, id := range []string{agentMsg, humanMsg} {
		got, err := f.stars.GetMessageStar(ctx, f.member, id)
		require.NoError(t, err)
		assert.Nil(t, got, id)
	}
}

// TestChatStars_SenderLabels pins the neutral label contract: names from
// the batch-loaded records, "User"/"Agent" for a blank name or a missing
// sender ID, "Deleted user"/"Deleted agent" for a record that no longer
// exists, "Someone" for an unknown sender, and never the raw address.
func TestChatStars_SenderLabels(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	blank := uuid.NewString()
	require.NoError(t, f.s.CreateUser(ctx, &store.User{ID: blank, Email: blank + "@test.com", DisplayName: "  ", Role: store.UserRoleMember, Status: "active"}))
	// A blank name falls back to the slug. Ent refuses an empty name, so a
	// whitespace-only one exercises the same TrimSpace path.
	unnamed := &store.Agent{ID: uuid.NewString(), Slug: "slug-only", Name: "   ", ProjectID: f.projA}
	require.NoError(t, f.s.CreateAgent(ctx, unnamed))
	softDeleted := &store.Agent{ID: uuid.NewString(), Slug: "gone-agent", Name: "Retired Agent", ProjectID: f.projA}
	require.NoError(t, f.s.CreateAgent(ctx, softDeleted))
	softDeleteAgentForTest(t, f.s, softDeleted.ID)

	cases := []struct{ sender, senderID, want string }{
		{"user:" + f.owner + "@test.com", f.owner, ""}, // filled below: the display name
		{"user:" + blank + "@test.com", blank, starLabelUser},
		{"user:someone@example.test", uuid.NewString(), starLabelDeletedUser},
		{"user:someone@example.test", "", starLabelUser},
		{"agent:" + f.agentA.Slug, f.agentA.ID, f.agentA.Name},
		{"agent:slug-only", unnamed.ID, "slug-only"},
		{"agent:gone-agent", softDeleted.ID, "Retired Agent"},
		{"agent:never-was", uuid.NewString(), starLabelDeletedAgent},
		{"agent:never-was", "", starLabelAgent},
		// No usable stable ID (not a canonical UUID): the neutral kind
		// label, never "Deleted ...".
		{"user:someone@example.test", "not-a-uuid", starLabelUser},
		{"user:someone@example.test", strings.ToUpper(f.owner), starLabelUser},
		{"agent:never-was", strings.ToUpper(f.agentA.ID), starLabelAgent},
		{"agent:never-was", "{" + f.agentA.ID + "}", starLabelAgent},
		{"system", "", starLabelSomeone},
	}
	owner, err := f.s.GetUser(ctx, f.owner)
	require.NoError(t, err)
	cases[0].want = starUserLabel(owner)
	require.NotEqual(t, starLabelDeletedUser, cases[0].want)

	conv := topicConversationID(t, f.wcs, f.topicA)
	for i, c := range cases {
		id := uuid.NewString()
		require.NoError(t, f.s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: f.projA, Sender: c.sender, SenderID: c.senderID, Recipient: "chat", Msg: "label " + fmt.Sprint(i),
			Type: "chat", Channel: "web", ThreadID: f.topicA, ConversationID: conv, CreatedAt: time.Now().UTC(),
		}))
		decodeStarPut(t, f.put(t, f.admin, id, f.topicA))
		rec := f.session(t, f.admin, http.MethodGet, starPath(id), nil)
		item := decodeStarItem(t, rec)
		assert.Equal(t, c.want, item.SenderName, "%q/%q", c.sender, c.senderID)
		assert.NotContains(t, f.session(t, f.admin, http.MethodGet, starPath(id), nil).Body.String(), "example.test", "no raw address")
	}
}

// failingLabelStore fails the batched user lookup once armed: a lookup
// failure must be a 500, never a "Deleted user" label.
type failingLabelStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *failingLabelStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	if s.fault.Active() {
		return nil, errFailingStarStore
	}
	return s.Store.GetUsersByIDs(ctx, ids)
}

func TestChatStars_LabelLookupFailureIs500(t *testing.T) {
	f := newStarFixture(t, true)
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *failingLabelStore {
		return &failingLabelStore{Store: inner, fault: sw}
	})
	msg := f.threadMsg(t, f.topicA, f.projA, "labels", time.Now())
	decodeStarPut(t, f.put(t, f.admin, msg, f.topicA))
	fault.Arm()
	assertStar500(t, f.session(t, f.admin, http.MethodGet, starPath(msg), nil), "detail, label lookup")
	assertStar500(t, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA), nil), "list, label lookup")
}

// TestChatStars_MessageDeleteClearsStarsAndInvalidatesOwners: deleting a
// message through the chat route removes every owner's star in the same
// transaction and tells exactly those owners, on their own subjects, to
// refetch. With the experiment off the stars still go, silently.
func TestChatStars_MessageDeleteClearsStarsAndInvalidatesOwners(t *testing.T) {
	for _, experimentOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("experiment=%t", experimentOn), func(t *testing.T) {
			f := newStarFixture(t, true)
			ctx := context.Background()
			msg := f.threadMsg(t, f.topicA, f.projA, "will be deleted", time.Now().Add(-time.Minute))
			kept := f.threadMsg(t, f.topicA, f.projA, "kept", time.Now().Add(-2*time.Minute))
			decodeStarPut(t, f.put(t, f.admin, msg, f.topicA))
			decodeStarPut(t, f.put(t, f.member, msg, f.topicA))
			decodeStarPut(t, f.put(t, f.owner, kept, f.topicA))
			setStarsExperiment(t, f.srv, experimentOn, true)
			ch, unsub := f.ep.Subscribe("user.*.chat.stars")
			defer unsub()

			rec := f.session(t, f.admin, http.MethodDelete, chatConversationPath(f.topicA, "messages/"+msg), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			for _, u := range []string{f.admin, f.member} {
				got, err := f.stars.GetMessageStar(ctx, u, msg)
				require.NoError(t, err)
				assert.Nil(t, got, "no association survives the deletion")
			}
			got, err := f.stars.GetMessageStar(ctx, f.owner, kept)
			require.NoError(t, err)
			assert.NotNil(t, got)
			if experimentOn {
				assert.ElementsMatch(t, []string{"user." + f.admin + ".chat.stars", "user." + f.member + ".chat.stars"}, starEvents(t, ch))
			} else {
				assert.Empty(t, starEvents(t, ch), "no invalidation while the experiment is off")
			}
			if experimentOn {
				// Nothing of it can be read back or re-added.
				assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starPath(msg), nil).Code)
				assert.Empty(t, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(msg), nil)))
				assert.Equal(t, http.StatusNotFound, f.put(t, f.member, msg, f.topicA).Code)
			}
		})
	}
}

// TestChatStars_PromotionKeepsStarsAndRechecksAccess: promoting an agent
// DM moves its stars, modern (conversation-ID) and legacy (thread-ID)
// rows, to the new thread with the same identity and save time; the
// owner is told to refetch, the detail points at the thread, the stale DM
// key lists nothing, and the thread's project access now decides.
func TestChatStars_PromotionKeepsStarsAndRechecksAccess(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	d := f.memberDMs(t)
	// The modern row is matched only by its conversation ID (no thread ID),
	// the legacy row only by its thread ID: promotion's two arms.
	modern := f.dmMsg(t, "", d.agentConv, f.projA, "user:"+f.member, f.member, "modern", "web", time.Now().Add(-time.Hour))
	legacy := f.dmMsg(t, d.agentKey, "", f.projA, "user:"+f.member, f.member, "legacy", "web", time.Now().Add(-2*time.Hour))
	first := decodeStarPut(t, f.put(t, f.member, modern, d.agentKey))
	// The legacy row is in history only under the thread-ID selector (the
	// read switch off), so its star is stored with that selector.
	legacyRec, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: legacy, ProjectID: f.projA,
		Selector: StarSelector{ConversationKey: d.agentKey, Channel: "web", ThreadID: d.agentKey}})
	require.NoError(t, err)

	ch, unsub := f.ep.Subscribe("user.*.chat.stars")
	defer unsub()
	rec := f.session(t, f.member, http.MethodPost, chatConversationPath(d.agentKey, "promote"), map[string]string{"name": "Promoted stars"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var promoted promoteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &promoted))
	assert.Equal(t, []string{"user." + f.member + ".chat.stars"}, starEvents(t, ch), "the DM's user refetches")

	for id, at := range map[string]time.Time{modern: first.StarredAt, legacy: legacyRec.StarredAt} {
		item := decodeStarItem(t, f.session(t, f.member, http.MethodGet, starPath(id), nil))
		assert.Equal(t, promoted.ID, item.ConversationKey, id)
		assert.Equal(t, "thread", item.ConversationKind)
		assert.Equal(t, f.projA, item.ProjectID)
		assert.Equal(t, "Promoted stars", item.ConversationName)
		assert.True(t, item.StarredAt.Equal(at), "save time kept")
		assert.Equal(t, "/chat/space/"+f.projA+"/thread/"+promoted.ID+"#msg-"+id, item.Permalink)
	}
	assert.ElementsMatch(t, []string{modern, legacy},
		starPageIDs(decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(promoted.ID), nil))))
	// The stale DM key no longer lists or accepts them.
	assert.Empty(t, decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(d.agentKey), nil)).Items)
	assert.Equal(t, http.StatusNotFound, f.put(t, f.member, modern, d.agentKey).Code)

	// Former DM participation does not confer project read: without the
	// project role the stars are hidden (retained), and they return with it.
	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.member)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projA {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
		}
	}
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starPath(modern), nil).Code)
	assert.Empty(t, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(modern, legacy), nil)))
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projA, store.ProjectRoleMember)
	assert.ElementsMatch(t, []string{modern, legacy},
		decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(modern, legacy), nil)))
}

// TestChatStars_LifecycleCleanup: topic deletion, user deletion and
// project deletion remove the matching associations.
func TestChatStars_LifecycleCleanup(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	inA := f.threadMsg(t, f.topicA, f.projA, "in A", time.Now())
	inB := f.threadMsg(t, f.topicB, f.projB, "in B", time.Now())
	spare := f.topic(t, f.projA) // a project keeps at least one thread
	inSpare := f.threadMsg(t, spare, f.projA, "in spare", time.Now())
	decodeStarPut(t, f.put(t, f.admin, inA, f.topicA))
	decodeStarPut(t, f.put(t, f.member, inA, f.topicA))
	decodeStarPut(t, f.put(t, f.admin, inB, f.topicB))
	decodeStarPut(t, f.put(t, f.member, inSpare, spare))

	has := func(user, msg string) bool {
		t.Helper()
		rec, err := f.stars.GetMessageStar(ctx, user, msg)
		require.NoError(t, err)
		return rec != nil
	}

	rec := f.session(t, f.admin, http.MethodDelete, "/api/v1/chat/topics/"+url.PathEscape(f.topicA), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, has(f.admin, inA), "topic deletion")
	assert.False(t, has(f.member, inA), "topic deletion")
	assert.True(t, has(f.member, inSpare))

	f.srv.removeUserScopedData(ctx, f.member)
	assert.False(t, has(f.member, inSpare), "user deletion")

	f.srv.executePostDeletionEffects(ctx, f.projB, &store.Project{ID: f.projB, Name: "Inbox B", GitRemote: "https://example.test/b.git"}, deletionEffectInputs{})
	assert.False(t, has(f.admin, inB), "project deletion")
}

// TestChatStars_UpkeepPass: the server's upkeep pass walks the table in
// bounded batches from where it stopped, purges permanently gone targets,
// keeps an inactive user's stars, and never overlaps itself.
func TestChatStars_UpkeepPass(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "kept", time.Now())
	decodeStarPut(t, f.put(t, f.member, msg, f.topicA))
	// A suspended user is not a deleted one.
	member, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	member.Status = store.UserStatusSuspended
	require.NoError(t, f.s.UpdateUser(ctx, member))
	gone := uuid.NewString()
	_, err = f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
		gone, msg, f.topicA, f.projA, sqliteStarTime(time.Now()))
	require.NoError(t, err)
	// Upkeep runs while the experiment is off.
	setStarsExperiment(t, f.srv, false, true)

	rt := &f.srv.messageStarUpkeep
	rt.busy.Store(true)
	assert.False(t, f.srv.messageStarUpkeepPass(ctx), "a pass in progress makes another a no-op")
	rt.busy.Store(false)

	require.True(t, f.srv.messageStarUpkeepPass(ctx))
	got, err := f.stars.GetMessageStar(ctx, gone, msg)
	require.NoError(t, err)
	assert.Nil(t, got, "a deleted owner's star is purged")
	got, err = f.stars.GetMessageStar(ctx, f.member, msg)
	require.NoError(t, err)
	assert.NotNil(t, got, "an inactive owner's star is kept")
	rt.mu.Lock()
	next := rt.next
	rt.mu.Unlock()
	assert.Equal(t, StarCleanupPosition{}, next, "fewer rows than a batch: the next pass starts over")
}

// TestChatStars_MessageDeleteNonCanonicalID: a delete whose path spells the
// message UUID in upper case still removes every star of the message and
// keys the soft delete by the canonical ID, so the star reads see it as
// deleted.
func TestChatStars_MessageDeleteNonCanonicalID(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "delete me loudly", time.Now().Add(-time.Minute))
	decodeStarPut(t, f.put(t, f.admin, msg, f.topicA))
	decodeStarPut(t, f.put(t, f.member, msg, f.topicA))
	ch, unsub := f.ep.Subscribe("user.*.chat.stars")
	defer unsub()

	rec := f.session(t, f.admin, http.MethodDelete, chatConversationPath(f.topicA, "messages/"+strings.ToUpper(msg)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		MessageID string `json:"messageId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, msg, resp.MessageID, "the canonical ID")

	for _, u := range []string{f.admin, f.member} {
		got, err := f.stars.GetMessageStar(ctx, u, msg)
		require.NoError(t, err)
		assert.Nil(t, got, "no association survives the deletion")
	}
	assert.ElementsMatch(t, []string{"user." + f.admin + ".chat.stars", "user." + f.member + ".chat.stars"}, starEvents(t, ch))
	exts, err := f.wcs.GetMessageExts(ctx, []string{msg})
	require.NoError(t, err)
	require.NotNil(t, exts[msg], "the extension row is keyed by the canonical ID")
	assert.NotNil(t, exts[msg].DeletedAt)
}

// TestChatStars_MessageEditNonCanonicalID: an edit whose path spells the
// message UUID in upper case keys the edited marker by the canonical ID, so
// the star detail shows the message as edited, with its new text.
func TestChatStars_MessageEditNonCanonicalID(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "before the edit", time.Now().Add(-time.Minute))
	decodeStarPut(t, f.put(t, f.admin, msg, f.topicA))
	edits, unsub := f.ep.Subscribe("project." + f.projA + ".chat.message.edited")
	defer unsub()

	rec := f.session(t, f.admin, http.MethodPut, chatConversationPath(f.topicA, "messages/"+strings.ToUpper(msg)),
		map[string]string{"content": "after the edit"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		MessageID string `json:"messageId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, msg, resp.MessageID, "the canonical ID")

	item := decodeStarItem(t, f.session(t, f.admin, http.MethodGet, starPath(msg), nil))
	assert.True(t, item.Edited, "the edited marker is keyed by the canonical ID")
	assert.Equal(t, "after the edit", item.Excerpt)

	// The edited event carries the canonical ID too.
	got := collectEvents(edits)
	require.Len(t, got, 1)
	var evt map[string]any
	require.NoError(t, json.Unmarshal(got[0].Data, &evt))
	assert.Equal(t, msg, evt["messageId"], "the edited event names the canonical message ID")
}

// TestChatStars_DMKeyMustBeCanonical pins that the star routes accept a DM
// only under its canonical key (messages.DMConversationKey: sorted slots,
// canonical IDs), so a stored DM key is always the external ref of the DM's
// native conversation (the form the read switch and the Starred rail look
// up). Under the swapped-slot key, PUT, the star list and the starred-only
// history each answer 400 and nothing is stored; under the canonical key,
// PUT, the list, the state route and DELETE work. The state route's
// handling of a non-canonical key already in storage is
// TestChatStars_StateLeavesOutNonCanonicalStoredDMKey's.
func TestChatStars_DMKeyMustBeCanonical(t *testing.T) {
	f := newStarFixture(t, true)
	d := f.memberDMs(t)
	msg := f.dmMsg(t, d.humanKey, d.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "h", "web", time.Now())
	parts := strings.Split(d.humanKey, ":")
	require.Len(t, parts, 5)
	swapped := strings.Join([]string{"dm", parts[3], parts[4], parts[1], parts[2]}, ":")
	require.NotEqual(t, d.humanKey, swapped)
	require.True(t, validDMKey(swapped), "the swapped key is well formed, only not canonical")

	assert.Equal(t, http.StatusBadRequest, f.put(t, f.member, msg, swapped).Code, "PUT under the swapped key")
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.member, http.MethodGet, starListPath(swapped), nil).Code, "list")
	assert.Equal(t, http.StatusBadRequest,
		f.session(t, f.member, http.MethodGet, chatConversationPath(swapped, "messages")+"?starred=true", nil).Code, "starred-only history")
	got, err := f.stars.GetMessageStar(context.Background(), f.member, msg)
	require.NoError(t, err)
	assert.Nil(t, got, "nothing stored under the swapped key")

	// The canonical key works.
	decodeStarPut(t, f.put(t, f.member, msg, d.humanKey))
	assert.Len(t, decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(d.humanKey), nil)).Items, 1)
	assert.Equal(t, []string{msg}, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(msg), nil)))
	assert.Equal(t, http.StatusNoContent, f.session(t, f.member, http.MethodDelete, starPath(msg), nil).Code)
}

// TestChatStars_StateLeavesOutNonCanonicalStoredDMKey pins that a legacy
// star stored under a DM key that is not canonical (the star routes refuse
// such a key, so it is inserted directly) is left out of a state batch,
// alone or mixed with another conversation, under both read-switch
// settings, instead of failing the batch with 400.
func TestChatStars_StateLeavesOutNonCanonicalStoredDMKey(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		f := newStarFixture(t, readSwitch)
		d := f.memberDMs(t)
		parts := strings.Split(d.humanKey, ":")
		require.Len(t, parts, 5)
		swapped := strings.Join([]string{"dm", parts[3], parts[4], parts[1], parts[2]}, ":")
		require.NotEqual(t, d.humanKey, swapped)
		legacy := f.dmMsg(t, swapped, d.humanConv, uuid.Nil.String(), "user:"+f.member, f.member, "legacy", "web", time.Now())
		_, _, err := f.stars.AddMessageStar(context.Background(), f.member, StarTarget{MessageID: legacy,
			Selector: StarSelector{ConversationKey: swapped, Channel: "web", ThreadID: swapped}})
		require.NoError(t, err)
		thread := f.threadMsg(t, f.topicA, f.projA, "t", time.Now())
		decodeStarPut(t, f.put(t, f.member, thread, f.topicA))

		assert.Empty(t, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(legacy), nil)),
			"switch %t: a batch of only the legacy star answers, without it", readSwitch)
		assert.Equal(t, []string{thread}, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(legacy, thread), nil)),
			"switch %t: a mixed batch keeps the other conversation's answer", readSwitch)
	}
}
