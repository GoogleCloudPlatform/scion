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
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The count includes every unmuted thread in a readable space, member or
// not, and every unmuted DM, whose latest message is past the read
// watermark.
func TestChatUnreadCount_ThreadsAndDMs(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	me := DevUserID
	bindProjectMember(t, s, proj.ID, me)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{}, getUnreadCount(t, rec), "empty to start")

	f.thread("member-unread", me, true, true)            // counts
	f.thread("non-member-unread", me, false, true)       // counts: the rail shows it
	muted, _ := f.thread("member-muted", me, true, true) // muted
	require.NoError(t, wcs.SetMuted(ctx, me, muted, true))
	read, readMsg := f.thread("member-read", me, true, true) // read to latest
	require.NoError(t, wcs.SetReadState(ctx, me, read, readMsg))
	f.thread("member-empty", me, true, false) // no messages
	stale, _ := f.thread("member-stale-watermark", me, true, true)
	require.NoError(t, wcs.SetReadState(ctx, me, stale, api.NewUUID())) // counts

	f.dm(me) // counts
	mutedDM, _ := f.dm(me)
	require.NoError(t, wcs.SetMuted(ctx, me, mutedDM, true))
	readDM, readDMMsg := f.dm(me)
	require.NoError(t, wcs.SetReadState(ctx, me, readDM, readDMMsg))

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 4, Threads: 3, DMs: 1}, getUnreadCount(t, rec))

	// Leaving a thread does not hide it from the rail, so it still counts.
	convs, err := s.GetConversationsForPrincipal(ctx, "user", me)
	require.NoError(t, err)
	for _, c := range convs {
		require.NoError(t, s.RemoveParticipant(ctx, c.ID, "user", me))
	}
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 4, Threads: 3, DMs: 1}, getUnreadCount(t, rec))
}

// A deleted thread does not count.
func TestChatUnreadCount_DeletedThreadExcluded(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	bindProjectMember(t, s, proj.ID, DevUserID)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.thread("keeper", DevUserID, false, false) // the last thread cannot be deleted
	topicID, _ := f.thread("doomed", DevUserID, true, true)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	require.Equal(t, 1, getUnreadCount(t, rec).Threads)

	require.NoError(t, wcs.DeleteTopic(context.Background(), topicID))
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 0, getUnreadCount(t, rec).Threads)
}

// Each caller sees only their own read state, and a thread does not count
// without project read access, even with a participant row: the row is a
// listing index, not authorization.
func TestChatUnreadCount_CallerScopedAndReadGated(t *testing.T) {
	srv, s, alice, bob, proj := setupDemoPolicyTest(t)
	ctx := context.Background()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}

	// alice (project member) can read both threads; bob (not a project
	// member) holds a stale participant row on one of them.
	f.thread("alice-thread", alice.ID, true, true)
	bobThread, bobMsg := f.thread("bob-thread", bob.ID, true, true)
	f.dm(bob.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 2, Threads: 2, DMs: 0}, getUnreadCount(t, rec),
		"alice sees both threads in the project she reads, and none of bob's DMs")

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 0, DMs: 1}, getUnreadCount(t, rec),
		"bob's thread is in a project he cannot read; only his DM counts")

	// alice reading a thread changes her count and nothing for bob.
	require.NoError(t, wcs.SetReadState(ctx, alice.ID, bobThread, bobMsg))
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 1, getUnreadCount(t, rec).Threads)
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 1, getUnreadCount(t, rec).Conversations)
}

func TestChatUnreadCount_MethodNotAllowed(t *testing.T) {
	srv, _, _, _ := setupSharedChatTest(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// The count is a constant number of read-state queries however many
// conversations the caller has, batched at the configured size.
func TestChatUnreadCount_BatchesReadStates(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	srv.chatSpacesBatch = chatSpacesBatchSizes{topics: 1, readStates: 2}
	bindProjectMember(t, s, proj.ID, DevUserID)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	for i := 0; i < 3; i++ {
		f.thread("t"+string(rune('a'+i)), DevUserID, true, true)
	}
	f.dm(DevUserID)
	f.dm(DevUserID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 5, Threads: 3, DMs: 2}, getUnreadCount(t, rec))
}

// A user who is not a project member gains no thread membership when
// mentioned by email, and cannot read the project, so the thread never
// reaches their count.
func TestChatUnreadCount_NonMemberMentionedByEmail(t *testing.T) {
	srv, s, alice, bob, proj := setupDemoPolicyTest(t)
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	topicID, _ := f.thread("alice-thread", alice.ID, false, false)
	convID := topicConversationID(t, wcs, topicID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "hi @" + bob.Email})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// The sender's row is written by the same background pass that would
	// have written bob's.
	waitUserParticipant(t, s, convID, alice.ID)
	require.Never(t, func() bool { return isUserParticipant(t, s, convID, bob.ID) },
		200*time.Millisecond, 20*time.Millisecond, "a non-member mentioned by email must not become a member")

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{}, getUnreadCount(t, rec))
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 0, getUnreadCount(t, rec).Threads, "the sender's own message is read")
}

// latestMessagesFaultStore fails the batched latest-message reads while its
// switch is armed.
type latestMessagesFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

var errLatestMessagesFault = errors.New("injected latest-messages fault")

func (s *latestMessagesFaultStore) LatestMessagesByThreadIDs(ctx context.Context, ids []string, opts store.LatestMessageOptions) (map[string]*store.Message, error) {
	if s.fault.Active() {
		return nil, errLatestMessagesFault
	}
	return s.Store.LatestMessagesByThreadIDs(ctx, ids, opts)
}

func (s *latestMessagesFaultStore) LatestMessagesByConversationIDs(ctx context.Context, ids []string, opts store.LatestMessageOptions) (map[string]*store.Message, error) {
	if s.fault.Active() {
		return nil, errLatestMessagesFault
	}
	return s.Store.LatestMessagesByConversationIDs(ctx, ids, opts)
}

// A failed DM read fails the count, as a failed thread read does, rather
// than reporting every DM as read.
func TestChatUnreadCount_DMReadErrorFails(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *latestMessagesFaultStore {
		return &latestMessagesFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	proj := &store.Project{ID: api.NewUUID(), Name: "dm-fault", Slug: "dm-fault", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.dm(DevUserID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	require.Equal(t, 1, getUnreadCount(t, rec).DMs)

	fault.Arm()
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())

	// The DM list keeps degrading instead of failing.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// Threads in several projects are counted the same at any batch size.
func TestChatUnreadCount_ProjectOrderDeterministic(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	srv.chatSpacesBatch = chatSpacesBatchSizes{topics: 1, readStates: 1}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		p := &store.Project{ID: api.NewUUID(), Name: fmt.Sprintf("p%d", i), Slug: fmt.Sprintf("p%d", i), Created: time.Now(), Updated: time.Now()}
		require.NoError(t, s.CreateProject(ctx, p))
		bindProjectMember(t, s, p.ID, DevUserID)
		pf := &unreadFixture{t: t, s: s, wcs: wcs, proj: p}
		pf.thread("t", DevUserID, true, true)
	}
	bindProjectMember(t, s, proj.ID, DevUserID)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.thread("home", DevUserID, true, true)

	for i := 0; i < 3; i++ {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
		assert.Equal(t, chatUnreadCountResponse{Conversations: 4, Threads: 4}, getUnreadCount(t, rec))
	}
}

// A thread in a template project is not counted: the spaces endpoint
// leaves templates out of the rail, so the badge would count a thread the
// user cannot find. The badge's thread count equals the rail's rollup.
func TestChatUnreadCount_TemplateProjectExcludedAgreesWithSpaces(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	me := DevUserID
	bindProjectMember(t, s, proj.ID, me)

	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.thread("unread", me, true, true)
	f.thread("unread-non-member", me, false, true)
	muted, _ := f.thread("muted", me, true, true)
	require.NoError(t, wcs.SetMuted(ctx, me, muted, true))
	read, readMsg := f.thread("read", me, true, true)
	require.NoError(t, wcs.SetReadState(ctx, me, read, readMsg))

	tmpl := &store.Project{
		ID: api.NewUUID(), Name: "tmpl", Slug: "tmpl",
		Labels:  map[string]string{store.LabelTemplate: "true"},
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, tmpl))
	bindProjectMember(t, s, tmpl.ID, me) // a member, and still left out
	tf := &unreadFixture{t: t, s: s, wcs: wcs, proj: tmpl}
	tf.thread("in-template", me, true, true)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	got := getUnreadCount(t, rec)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 2, Threads: 2}, got)
	assert.Equal(t, spacesUnreadTotal(t, srv), got.Threads,
		"badge threads must equal the rail's unread rollup")
}

// railUnreadDMs counts the DMs the rail's Unread DMs list shows: the
// entries of GET /api/v1/chat/dms with hasUnread set that are neither muted
// nor with a deleted agent (isBadgeUnreadDM on the web).
func railUnreadDMs(t *testing.T, srv *Server) int {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp chatDMListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	n := 0
	for _, dm := range resp.DMs {
		if dm.HasUnread && !dm.Muted && !dm.PeerDeleted {
			n++
		}
	}
	return n
}

// The badge total equals what the rail shows as unread: the space unread
// rollups plus the Unread DMs list. Covers threads the caller is not a
// participant of, threads in another member project, in a template project
// and in a readable project the caller is not a member of, and DMs with a
// user, with an agent in another project and with a hard- and a
// soft-deleted agent, along with muted and read conversations of each kind.
func TestChatUnreadCount_BadgeEqualsRail(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	me := DevUserID
	bindProjectMember(t, s, proj.ID, me)

	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.thread("member", me, true, true)
	f.thread("non-member", me, false, true)
	mutedThread, _ := f.thread("muted", me, true, true)
	require.NoError(t, wcs.SetMuted(ctx, me, mutedThread, true))
	readThread, readThreadMsg := f.thread("read", me, false, true)
	require.NoError(t, wcs.SetReadState(ctx, me, readThread, readThreadMsg))

	other := &store.Project{ID: api.NewUUID(), Name: "other", Slug: "other", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, other))
	bindProjectMember(t, s, other.ID, me)
	of := &unreadFixture{t: t, s: s, wcs: wcs, proj: other}
	of.thread("elsewhere", me, false, true)

	// A project the caller (a hub admin) can read but is not a member of.
	foreign := &store.Project{ID: api.NewUUID(), Name: "foreign", Slug: "foreign", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, foreign))
	ff := &unreadFixture{t: t, s: s, wcs: wcs, proj: foreign}
	ff.thread("not-mine", me, true, true)

	tmpl := &store.Project{
		ID: api.NewUUID(), Name: "tmpl", Slug: "tmpl",
		Labels:  map[string]string{store.LabelTemplate: "true"},
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, tmpl))
	tf := &unreadFixture{t: t, s: s, wcs: wcs, proj: tmpl}
	tf.thread("in-template", me, true, true)

	newAgent := func(slug string) string {
		id := api.NewUUID()
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Slug: slug, Name: slug, ProjectID: other.ID, Phase: "stopped",
			CreatedBy: me, OwnerID: me,
		}))
		return id
	}
	f.dm(me)                              // user peer
	f.dmWith(me, "agent", newAgent("a1")) // agent in another project
	gone := newAgent("gone")
	f.dmWith(me, "agent", gone)
	require.NoError(t, s.DeleteAgent(ctx, gone)) // hard-deleted agent: left out
	soft := newAgent("soft")
	f.dmWith(me, "agent", soft)
	softDeleteChatPeerAgent(t, s, soft) // soft-deleted agent: left out
	mutedDM, _ := f.dm(me)
	require.NoError(t, wcs.SetMuted(ctx, me, mutedDM, true))
	readDM, readDMMsg := f.dmWith(me, "agent", newAgent("a2"))
	require.NoError(t, wcs.SetReadState(ctx, me, readDM, readDMMsg))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	got := getUnreadCount(t, rec)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 5, Threads: 3, DMs: 2}, got,
		"threads: member, non-member, elsewhere; DMs: the user and the live agent")
	assert.Equal(t, spacesUnreadTotal(t, srv), got.Threads, "threads must equal the space rollups")
	assert.Equal(t, railUnreadDMs(t, srv), got.DMs, "DMs must equal the Unread DMs list")
	assert.Equal(t, got.Threads+got.DMs, got.Conversations)
}

// softDeleteChatPeerAgent marks agentID soft-deleted.
func softDeleteChatPeerAgent(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	a.DeletedAt = time.Now().UTC()
	require.NoError(t, s.UpdateAgent(ctx, a))
}

// A DM with an agent that has since been deleted (hard or soft) is kept and
// still listed by GET /api/v1/chat/dms, flagged peerDeleted, but it is left
// out of the badge and of the rail's Unread DMs list. Its data is untouched:
// the history loads and the read state is not changed. A live agent's DM
// is not flagged.
func TestChatUnreadCount_DeletedAgentDMExcluded(t *testing.T) {
	for _, mode := range []string{"hard", "soft"} {
		t.Run(mode, func(t *testing.T) {
			srv, s, wcs, proj := setupSharedChatTest(t)
			ctx := context.Background()
			me := DevUserID
			f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}

			newAgent := func(slug string) string {
				id := api.NewUUID()
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID: id, Slug: slug, Name: slug, ProjectID: proj.ID, Phase: "stopped",
					CreatedBy: me, OwnerID: me,
				}))
				return id
			}
			agentID := newAgent("gone")
			key, msgID := f.dmWith(me, "agent", agentID)
			liveKey, _ := f.dmWith(me, "agent", newAgent("live"))
			if mode == "hard" {
				require.NoError(t, s.DeleteAgent(ctx, agentID))
			} else {
				softDeleteChatPeerAgent(t, s, agentID)
			}

			rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var dms chatDMListResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dms))
			byKey := map[string]chatDMEntry{}
			for _, d := range dms.DMs {
				byKey[d.ConversationKey] = d
			}
			require.Contains(t, byKey, key, "the DM is still listed")
			assert.True(t, byKey[key].PeerDeleted)
			assert.True(t, byKey[key].HasUnread, "its read state is untouched")
			assert.False(t, byKey[liveKey].PeerDeleted)
			assert.Equal(t, 1, railUnreadDMs(t, srv), "only the live agent's DM")
			assert.Equal(t, chatUnreadCountResponse{Conversations: 1, DMs: 1},
				getUnreadCount(t, doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)))

			rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
			require.Equal(t, http.StatusOK, rec.Code, "history: %s", rec.Body.String())
			var hist struct {
				Messages []struct {
					ID string `json:"id"`
				} `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hist))
			require.Len(t, hist.Messages, 1)
			assert.Equal(t, msgID, hist.Messages[0].ID)
		})
	}
}

// A failed agent lookup flags no DM as deleted: the list keeps every DM
// countable rather than hiding live agents' DMs.
func TestChatDMs_PeerLookupFailureFlagsNothing(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *agentLookupFaultStore {
		return &agentLookupFaultStore{Store: inner, fault: fault}
	})
	ctx := context.Background()
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	agentID := api.NewUUID()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a", Name: "a", ProjectID: proj.ID, Phase: "stopped",
		CreatedBy: DevUserID, OwnerID: DevUserID,
	}))
	f.dmWith(DevUserID, "agent", agentID)
	fault.Arm()

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var dms chatDMListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dms))
	require.Len(t, dms.DMs, 1)
	assert.False(t, dms.DMs[0].PeerDeleted)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "the strict count fails instead")
}

// agentLookupFaultStore fails the batched agent lookup once armed.
type agentLookupFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *agentLookupFaultStore) GetAgentsByIDsIncludingDeleted(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	if s.fault.Active() {
		return nil, errors.New("injected agent lookup fault")
	}
	return s.Store.GetAgentsByIDsIncludingDeleted(ctx, ids)
}

// Explicit membership decides which projects count, for everyone: a hub
// admin who can read every project counts only the projects they are a
// member of. A thread in a readable non-member project is not counted on
// the badge nor in that space's rollup (the space is still listed, with
// unreadTracked false), and the thread list reports it read, while a
// mention of the caller there still shows. Template projects stay out even
// for a member.
func TestChatUnreadCount_ExplicitMemberProjectsOnly(t *testing.T) {
	srv, s, wcs, mine := setupSharedChatTest(t)
	ctx := context.Background()
	me := DevUserID
	bindProjectMember(t, s, mine.ID, me)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: mine}
	f.thread("mine", me, false, true)

	foreign := &store.Project{ID: api.NewUUID(), Name: "foreign", Slug: "foreign", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, foreign))
	ff := &unreadFixture{t: t, s: s, wcs: wcs, proj: foreign}
	plain, _ := ff.thread("plain", me, true, true)
	mentioned, _ := ff.thread("mentioned", me, true, false)
	// A real message mentioning the caller: the mention lookup joins on it.
	mentionMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: foreign.ID, Sender: "user:someone", SenderID: api.NewUUID(),
		Recipient: "user:me", RecipientID: me,
		Msg: "hi @me", Type: messages.TypeInstruction, Channel: "web", ThreadID: mentioned,
		ConversationID: topicConversationID(t, wcs, mentioned), CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(ctx, mentionMsg))
	require.NoError(t, wcs.TouchTopicActivity(ctx, mentioned, mentionMsg.ID))
	require.NoError(t, wcs.RecordMentions(ctx, mentioned, mentionMsg.ID, []string{me}))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 1}, getUnreadCount(t, rec))

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var spaces chatSpacesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &spaces))
	bySpace := map[string]chatSpaceEntry{}
	for _, sp := range spaces.Spaces {
		bySpace[sp.ProjectID] = sp
	}
	require.Contains(t, bySpace, foreign.ID, "the rail still lists the non-member space")
	assert.Equal(t, chatSpaceEntry{ProjectID: foreign.ID, ProjectName: "foreign", ProjectSlug: "foreign",
		ThreadCount: 2, UnreadCount: 0, UnreadTracked: false, LastActivityAt: bySpace[foreign.ID].LastActivityAt},
		bySpace[foreign.ID])
	assert.True(t, bySpace[mine.ID].UnreadTracked)
	assert.Equal(t, 1, bySpace[mine.ID].UnreadCount)
	assert.Equal(t, spacesUnreadTotal(t, srv), 1)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+foreign.ID+"/threads", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var threads chatTopicListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &threads))
	byID := map[string]chatTopicEntry{}
	for _, th := range threads.Threads {
		byID[th.ID] = th
	}
	assert.False(t, byID[plain].HasUnread, "no unread dot in a non-member space")
	assert.False(t, byID[plain].HasUnreadMention)
	assert.False(t, byID[mentioned].HasUnread)
	assert.True(t, byID[mentioned].HasUnreadMention, "a mention of the caller still shows")

	// Becoming a member starts counting.
	bindProjectMember(t, s, foreign.ID, me)
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 3, Threads: 3}, getUnreadCount(t, rec))
	assert.Equal(t, spacesUnreadTotal(t, srv), 3)
}

// An agent DM row with no peer ID is never looked up, so it is not taken
// for a deleted agent: the list does not flag it and both the badge and
// the rail's Unread DMs list count it.
func TestChatUnreadCount_AgentDMWithoutPeerIDCounted(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	me := DevUserID
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	key, _ := f.dmWith(me, "agent", api.NewUUID())
	require.NoError(t, wcs.UpsertDM(ctx, WebChatDM{ConversationKey: key, ParticipantID: me, PeerKind: "agent"}))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var dms chatDMListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dms))
	require.Len(t, dms.DMs, 1)
	assert.Empty(t, dms.DMs[0].PeerID)
	assert.False(t, dms.DMs[0].PeerDeleted)
	assert.Equal(t, 1, railUnreadDMs(t, srv))
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, DMs: 1},
		getUnreadCount(t, doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)))
}
