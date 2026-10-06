// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupMentionDotTest is setupSendTest with the webchat store sharing the
// ent store's database, as in production: the unread-mention query joins
// webchat_mention against the messages table.
func setupMentionDotTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project, *sql.DB) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	proj := &store.Project{ID: tid("mention-dot"), Name: "mention-dot", Slug: "mention-dot",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	if !ok || dbProvider.DB() == nil {
		t.Fatal("store does not expose DB()")
	}
	db := dbProvider.DB()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init webchat store: %v", err)
	}
	srv.SetWebChatStore(wcs)
	return srv, s, wcs, proj, db
}

// mentionDotFixture seeds topics and messages for the thread list tests.
type mentionDotFixture struct {
	t    *testing.T
	s    store.Store
	wcs  WebChatStore
	proj *store.Project
	at   time.Time
}

func (f *mentionDotFixture) topic(name string) string {
	f.t.Helper()
	id := tid("md-" + name)
	if err := f.wcs.CreateTopic(context.Background(), WebChatTopic{ID: id, ProjectID: f.proj.ID,
		Name: name, CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		f.t.Fatalf("CreateTopic %s: %v", name, err)
	}
	return id
}

// message posts a message from another user into topicID, advances the
// topic watermark, and records it as mentioning each of mentioned.
func (f *mentionDotFixture) message(topicID, name string, mentioned ...string) string {
	f.t.Helper()
	f.at = f.at.Add(time.Second)
	return f.messageAt(topicID, tid("md-msg-"+name), f.at, mentioned...)
}

// messageAt is message with an explicit ID and creation time.
func (f *mentionDotFixture) messageAt(topicID, id string, at time.Time, mentioned ...string) string {
	f.t.Helper()
	ctx := context.Background()
	msg := &store.Message{ID: id, ProjectID: f.proj.ID, Sender: "user:other@test.com",
		SenderID: tid("md-other-user"), Recipient: "thread:" + topicID, RecipientID: topicID,
		Msg: id, Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: at}
	if err := f.s.CreateMessage(ctx, msg); err != nil {
		f.t.Fatalf("CreateMessage %s: %v", id, err)
	}
	if err := f.wcs.TouchTopicActivity(ctx, topicID, msg.ID); err != nil {
		f.t.Fatalf("TouchTopicActivity: %v", err)
	}
	if err := f.wcs.RecordMentions(ctx, topicID, msg.ID, mentioned); err != nil {
		f.t.Fatalf("RecordMentions: %v", err)
	}
	return msg.ID
}

func listThreadsByID(t *testing.T, srv *Server, projectID string) map[string]chatTopicEntry {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list threads: %d %s", rec.Code, rec.Body.String())
	}
	var resp chatTopicListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]chatTopicEntry, len(resp.Threads))
	for _, e := range resp.Threads {
		out[e.ID] = e
	}
	return out
}

func TestListThreads_HasUnreadMention(t *testing.T) {
	srv, s, wcs, proj, _ := setupMentionDotTest(t)
	ctx := context.Background()
	f := &mentionDotFixture{t: t, s: s, wcs: wcs, proj: proj, at: time.Now().UTC().Add(-time.Hour)}
	other := tid("md-someone-else")

	mentioned := f.topic("mentioned")
	f.message(mentioned, "mentioned-1", DevUserID)

	read := f.topic("read")
	readMsg := f.message(read, "read-1", DevUserID)
	if err := wcs.SetReadState(ctx, DevUserID, read, readMsg); err != nil {
		t.Fatal(err)
	}

	// The mention was read; a newer plain message is unread but not a mention.
	readThenNew := f.topic("read-then-new")
	rtn := f.message(readThenNew, "rtn-1", DevUserID)
	if err := wcs.SetReadState(ctx, DevUserID, readThenNew, rtn); err != nil {
		t.Fatal(err)
	}
	f.message(readThenNew, "rtn-2")

	// Read up to a plain message, then mentioned after the watermark.
	newMention := f.topic("new-mention")
	nm := f.message(newMention, "nm-1")
	if err := wcs.SetReadState(ctx, DevUserID, newMention, nm); err != nil {
		t.Fatal(err)
	}
	f.message(newMention, "nm-2", DevUserID)

	someoneElse := f.topic("someone-else")
	f.message(someoneElse, "se-1", other)
	// Another user's read state must not matter either way.
	if err := wcs.SetReadState(ctx, other, mentioned, ""); err != nil {
		t.Fatal(err)
	}

	plain := f.topic("plain")
	f.message(plain, "plain-1")

	muted := f.topic("muted")
	f.message(muted, "muted-1", DevUserID)
	if err := wcs.SetMuted(ctx, DevUserID, muted, true); err != nil {
		t.Fatal(err)
	}

	msgDeleted := f.topic("msg-deleted")
	f.message(msgDeleted, "md-1")
	gone := f.message(msgDeleted, "md-2", DevUserID)
	if err := wcs.SetMessageDeleted(ctx, gone, time.Now()); err != nil {
		t.Fatal(err)
	}

	// mark-unread on a single-message thread leaves an empty watermark.
	emptyWatermark := f.topic("empty-watermark")
	f.message(emptyWatermark, "ew-1", DevUserID)
	if err := wcs.SetReadState(ctx, DevUserID, emptyWatermark, ""); err != nil {
		t.Fatal(err)
	}

	// A watermark naming no stored message counts as no watermark.
	lostWatermark := f.topic("lost-watermark")
	f.message(lostWatermark, "lw-1", DevUserID)
	if err := wcs.SetReadState(ctx, DevUserID, lostWatermark, tid("md-no-such-message")); err != nil {
		t.Fatal(err)
	}

	deleted := f.topic("deleted")
	f.message(deleted, "deleted-1", DevUserID)
	if err := wcs.DeleteTopic(ctx, deleted); err != nil {
		t.Fatal(err)
	}

	got := listThreadsByID(t, srv, proj.ID)
	want := map[string]struct{ unread, mention bool }{
		mentioned:   {true, true},
		read:        {false, false},
		readThenNew: {true, false},
		newMention:  {true, true},
		someoneElse: {true, false},
		plain:       {true, false},
		muted:       {true, true}, // the rail hides it; the data is unchanged
		msgDeleted:  {true, false},

		emptyWatermark: {true, true},
		lostWatermark:  {true, true},
	}
	for id, w := range want {
		e, ok := got[id]
		if !ok {
			t.Fatalf("thread %s missing from list", id)
		}
		if e.HasUnread != w.unread || e.HasUnreadMention != w.mention {
			t.Errorf("thread %s: hasUnread=%v hasUnreadMention=%v; want %v %v",
				e.Name, e.HasUnread, e.HasUnreadMention, w.unread, w.mention)
		}
	}
	if _, ok := got[deleted]; ok {
		t.Errorf("deleted thread listed")
	}

	// Deleting a topic or message removes its mention rows.
	keys, err := wcs.UnreadMentionKeys(ctx, DevUserID, []string{deleted, msgDeleted, mentioned})
	if err != nil {
		t.Fatal(err)
	}
	if keys[deleted] || keys[msgDeleted] || !keys[mentioned] {
		t.Errorf("UnreadMentionKeys = %v; want only %s", keys, mentioned)
	}

	// Reading the thread clears the mention along with the unread dot.
	if err := wcs.SetReadState(ctx, DevUserID, newMention, tid("md-msg-nm-2")); err != nil {
		t.Fatal(err)
	}
	if e := listThreadsByID(t, srv, proj.ID)[newMention]; e.HasUnread || e.HasUnreadMention {
		t.Errorf("after read: hasUnread=%v hasUnreadMention=%v; want false false",
			e.HasUnread, e.HasUnreadMention)
	}
}

// UnreadMentionKeys only consults the keys it is given, so a mention in a
// thread outside the caller's listed set never surfaces.
func TestUnreadMentionKeys_RestrictedToGivenKeys(t *testing.T) {
	_, s, wcs, proj, _ := setupMentionDotTest(t)
	ctx := context.Background()
	f := &mentionDotFixture{t: t, s: s, wcs: wcs, proj: proj, at: time.Now().UTC().Add(-time.Hour)}
	listed := f.topic("listed")
	hidden := f.topic("hidden")
	f.message(listed, "l-1")
	f.message(hidden, "h-1", DevUserID)

	keys, err := wcs.UnreadMentionKeys(ctx, DevUserID, []string{listed})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("UnreadMentionKeys = %v; want none", keys)
	}
}

// A thread in a project the caller cannot read is never listed, so its
// mention records cannot reach the caller.
func TestListThreads_HasUnreadMention_InvisibleProject(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	if !ok {
		t.Fatal("store does not expose DB()")
	}
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatal(err)
	}
	srv.SetWebChatStore(wcs)
	f := &mentionDotFixture{t: t, s: s, wcs: wcs, proj: project, at: time.Now().UTC().Add(-time.Hour)}
	topic := f.topic("private")
	f.message(topic, "p-1", bob.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/spaces/"+project.ID+"/threads", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("non-member listed threads: %s", rec.Body.String())
	}

	// The member who can see it gets the dot for their own mention.
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/spaces/"+project.ID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member list: %d %s", rec.Code, rec.Body.String())
	}
	var resp chatTopicListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Threads) != 1 || !resp.Threads[0].HasUnreadMention {
		t.Fatalf("member threads = %+v; want one with hasUnreadMention", resp.Threads)
	}
}

// Sending a thread message records a mention row only for each resolved
// human project member, excluding the sender, unknown names and registered
// non-members, even when the mentioned user muted the thread.
func TestSendThreadMessage_RecordsHumanMentions(t *testing.T) {
	srv, s, wcs, proj, db := setupMentionDotTest(t)
	srv.SetDispatcher(&brokerMockDispatcher{})
	ctx := context.Background()
	alice := addHumanMember(t, s, proj.ID, "alice.smith@test.com", "Alice Smith")
	bob := addHumanMember(t, s, proj.ID, "bob@test.com", "Bob")
	// A registered user who is not a project member gets no row.
	carol := &store.User{ID: tid("md-carol"), Email: "carol@test.com", DisplayName: "Carol",
		Role: "member", Status: "active", Created: time.Now()}
	if err := s.CreateUser(ctx, carol); err != nil {
		t.Fatal(err)
	}
	topicID := tid("md-send")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "send",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	if err := wcs.SetMuted(ctx, alice.ID, topicID, true); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@alice-smith, @carol and @ghost please look"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	var users []string
	rows, err := db.QueryContext(ctx,
		`SELECT user_id FROM webchat_mention WHERE conversation_key = ?`, topicID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	if len(users) != 1 || users[0] != alice.ID {
		t.Fatalf("mention rows = %v; want only %s", users, alice.ID)
	}
	for _, tc := range []struct {
		user string
		want bool
	}{{alice.ID, true}, {bob.ID, false}, {carol.ID, false}, {DevUserID, false}} {
		keys, err := wcs.UnreadMentionKeys(ctx, tc.user, []string{topicID})
		if err != nil {
			t.Fatal(err)
		}
		if keys[topicID] != tc.want {
			t.Errorf("user %s: unread mention = %v; want %v", tc.user, keys[topicID], tc.want)
		}
	}
}

func TestMentionedHumanIDs(t *testing.T) {
	members := []chatMemberEntry{
		{ID: "u1", Kind: "user", DisplayName: "Alice Smith", Email: "alice@test.com"},
		{ID: "u2", Kind: "user", DisplayName: "Bob", Email: "robert@test.com"},
	}
	got := mentionedHumanIDs(members, []string{"Alice-Smith", "robert", "alice", "ghost", "bob"}, "u2")
	if len(got) != 1 || got[0] != "u1" {
		t.Fatalf("mentionedHumanIDs = %v; want [u1]", got)
	}
}

// Messages with the same created time are ordered by ID, matching the
// read watermark's monotonic guard and ListMessages.
func TestUnreadMentionKeys_SameCreatedAtTieBreaksByID(t *testing.T) {
	srv, s, wcs, proj, _ := setupMentionDotTest(t)
	ctx := context.Background()
	f := &mentionDotFixture{t: t, s: s, wcs: wcs, proj: proj, at: time.Now().UTC().Add(-time.Hour)}
	lo, hi := tid("md-tie-a"), tid("md-tie-b")
	if lo > hi {
		lo, hi = hi, lo
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	// Read up to lo; the mention on hi (same instant) is after it.
	after := f.topic("tie-after")
	f.messageAt(after, lo, at)
	f.messageAt(after, hi, at, DevUserID)
	if err := wcs.SetReadState(ctx, DevUserID, after, lo); err != nil {
		t.Fatal(err)
	}

	// Read up to hi; the mention on lo (same instant) is before it, and a
	// later plain message keeps the thread unread.
	before := f.topic("tie-before")
	lo2, hi2 := tid("md-tie2-a"), tid("md-tie2-b")
	if lo2 > hi2 {
		lo2, hi2 = hi2, lo2
	}
	f.messageAt(before, lo2, at, DevUserID)
	f.messageAt(before, hi2, at)
	f.messageAt(before, tid("md-tie2-later"), at.Add(time.Second))
	if err := wcs.SetReadState(ctx, DevUserID, before, hi2); err != nil {
		t.Fatal(err)
	}

	keys, err := wcs.UnreadMentionKeys(ctx, DevUserID, []string{after, before})
	if err != nil {
		t.Fatal(err)
	}
	if !keys[after] || keys[before] {
		t.Fatalf("UnreadMentionKeys = %v; want only %s", keys, after)
	}
	got := listThreadsByID(t, srv, proj.ID)
	if e := got[after]; !e.HasUnread || !e.HasUnreadMention {
		t.Errorf("tie-after: %+v; want unread mention", e)
	}
	if e := got[before]; !e.HasUnread || e.HasUnreadMention {
		t.Errorf("tie-before: %+v; want unread without mention", e)
	}
}

// A human thread message routed to the topic's default agent still
// records the human @mentions in it: "@agent do X, cc @alice" gives Alice
// the mention dot.
func TestSendAgentRouted_RecordsHumanMentions(t *testing.T) {
	srv, s, wcs, proj, db := setupMentionDotTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	ctx := context.Background()
	alice := addHumanMember(t, s, proj.ID, "alice.smith@test.com", "Alice Smith")
	a := &store.Agent{ID: tid("md-default-agent"), ProjectID: proj.ID, Name: "Builder", Slug: "md-builder",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("md-routed")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "routed",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@md-builder do X, cc @alice-smith"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if len(d.getMessages()) == 0 {
		t.Fatal("expected the message to be routed to the default agent")
	}

	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice list: %d %s", rec.Code, rec.Body.String())
	}
	var resp chatTopicListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range resp.Threads {
		if e.ID == topicID {
			found = true
			if !e.HasUnread || !e.HasUnreadMention {
				t.Errorf("alice thread = %+v; want unread mention", e)
			}
		}
	}
	if !found {
		t.Fatalf("thread %s not listed for alice: %+v", topicID, resp.Threads)
	}
	if keys, err := wcs.UnreadMentionKeys(ctx, DevUserID, []string{topicID}); err != nil || keys[topicID] {
		t.Errorf("sender recorded as mentioned: %v %v", keys, err)
	}
}

// An agent's thread message on the direct delivery path records the human
// @mentions in it before it is published.
func TestAgentThreadMessage_DirectPathRecordsHumanMentions(t *testing.T) {
	srv, s, wcs, proj, db := setupMentionDotTest(t)
	ctx := context.Background()
	alice := addHumanMember(t, s, proj.ID, "alice.smith@test.com", "Alice Smith")
	a := &store.Agent{ID: tid("md-poster-agent"), ProjectID: proj.ID, Name: "Poster", Slug: "md-poster",
		Phase: "running"}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("md-agent-thread")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "agent-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	convID := def162GroupConv(t, s, proj.ID, topicID)

	rr := postOutboundConvRef(t, srv, proj.ID, a.ID, "done, @alice-smith and @ghost", "conv:"+convID)
	if rr.Code != http.StatusOK {
		t.Fatalf("outbound: %d %s", rr.Code, rr.Body.String())
	}

	rows, err := db.QueryContext(ctx,
		`SELECT user_id, conversation_key FROM webchat_mention WHERE user_id = ?`, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var u, k string
		if err := rows.Scan(&u, &k); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	if len(got) != 1 || got[0] != topicID {
		t.Fatalf("alice mention rows = %v; want [%s]", got, topicID)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webchat_mention`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("mention rows = %d; want 1 (no row for @ghost)", total)
	}
	keys, err := wcs.UnreadMentionKeys(ctx, alice.ID, []string{topicID})
	if err != nil || !keys[topicID] {
		t.Errorf("alice unread mention = %v %v; want true", keys, err)
	}
}

// memberListCountingStore counts ListProjectMembers calls.
type memberListCountingStore struct {
	store.Store
	calls atomic.Int32
}

func (c *memberListCountingStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	c.calls.Add(1)
	return c.Store.ListProjectMembers(ctx, projectID)
}

// Recording human @mentions on a routed send reuses the member list the
// send already resolves for mention translation: neither an @agent-only
// send nor one that also cc's a person adds a member lookup. Only names
// that matched no agent are recorded.
func TestSendAgentRouted_MentionRecordingAddsNoMemberLookup(t *testing.T) {
	srv, s, wcs, proj, db := setupMentionDotTest(t)
	srv.SetDispatcher(&brokerMockDispatcher{})
	ctx := context.Background()
	alice := addHumanMember(t, s, proj.ID, "alice.smith@test.com", "Alice Smith")
	for _, slug := range []string{"md-first", "md-second"} {
		if err := s.CreateAgent(ctx, &store.Agent{ID: tid("md-agent-" + slug), ProjectID: proj.ID,
			Name: slug, Slug: slug, Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}); err != nil {
			t.Fatal(err)
		}
	}
	topicID := tid("md-agent-only")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "agent-only",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	counting := &memberListCountingStore{Store: s}
	srv.store = counting

	send := func(content string) {
		t.Helper()
		counting.calls.Store(0)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
			map[string]string{"content": content})
		if rec.Code != http.StatusCreated {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
		// The one pre-existing lookup for mention translation.
		if n := counting.calls.Load(); n != 1 {
			t.Errorf("%q: ListProjectMembers called %d times; want 1", content, n)
		}
	}
	mentionRows := func() []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT user_id FROM webchat_mention`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		return out
	}

	send("@md-first please ask @md-second")
	if got := mentionRows(); len(got) != 0 {
		t.Errorf("agent-only send recorded %v; want none", got)
	}

	send("@md-first please ask @md-second, cc @alice-smith")
	if got := mentionRows(); len(got) != 1 || got[0] != alice.ID {
		t.Errorf("mention rows = %v; want [%s]", got, alice.ID)
	}
}

func TestUnresolvedMentionNames(t *testing.T) {
	got := unresolvedMentionNames([]messages.MentionResult{
		{Slug: "agent-a", Status: "delivered"},
		{Slug: "alice", Status: "not_found"},
		{Slug: "agent-capped", Status: "error"},
		{Slug: "bob", Status: "not_found"},
	})
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("unresolvedMentionNames = %v; want [alice bob]", got)
	}
}

// The failed-message retention job hard-deletes messages; it also drops
// the mention rows of messages that no longer exist.
func TestFailedMessageRetention_PurgesOrphanMentions(t *testing.T) {
	srv, s, wcs, proj, db := setupMentionDotTest(t)
	srv.config.FailedMessageRetentionDays = 1
	ctx := context.Background()
	topicID := tid("md-retention")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "retention",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	newMsg := func(name string, at time.Time) string {
		m := &store.Message{ID: tid("md-ret-" + name), ProjectID: proj.ID, Sender: "user:dev@localhost",
			SenderID: DevUserID, Recipient: "agent:a", Msg: name, Type: messages.TypeChat, Channel: "web",
			ThreadID: topicID, DispatchState: store.MessageDispatchFailed, CreatedAt: at}
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := wcs.RecordMentions(ctx, topicID, m.ID, []string{tid("md-ret-user")}); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	old := newMsg("old", time.Now().Add(-48*time.Hour))
	kept := newMsg("kept", time.Now())

	srv.failedMessageRetentionHandler()(ctx)

	if _, err := s.GetMessage(ctx, old); err == nil {
		t.Fatal("old failed message was not purged")
	}
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT message_id FROM webchat_mention`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != kept {
		t.Fatalf("mention rows = %v; want only %s", got, kept)
	}
}
