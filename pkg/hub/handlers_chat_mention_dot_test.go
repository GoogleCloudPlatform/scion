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
	ctx := context.Background()
	f.at = f.at.Add(time.Second)
	msg := &store.Message{ID: tid("md-msg-" + name), ProjectID: f.proj.ID, Sender: "user:other@test.com",
		SenderID: tid("md-other-user"), Recipient: "thread:" + topicID, RecipientID: topicID,
		Msg: name, Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: f.at}
	if err := f.s.CreateMessage(ctx, msg); err != nil {
		f.t.Fatalf("CreateMessage %s: %v", name, err)
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
