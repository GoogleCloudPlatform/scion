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

// Tests for the note appended to unmentioned thread replies that reach no
// agent: it is addressed to the most recent other human poster. Replies
// that reach a live default agent, and explicitly addressed replies, are
// never noted.
package hub

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedThreadMsgAt persists a thread message from sender at the given time.
func seedThreadMsgAt(t *testing.T, s store.Store, projectID, topicID, sender, senderID string, at time.Time) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID: id, ProjectID: projectID, Sender: sender, SenderID: senderID,
		Recipient: "thread:" + topicID, Msg: "earlier", Type: "chat",
		ThreadID: topicID, CreatedAt: at,
	}); err != nil {
		t.Fatalf("seedThreadMsgAt: %v", err)
	}
	return id
}

// otherAgent creates a second running agent in projectID.
func otherAgent(t *testing.T, s store.Store, projectID, slug string) *store.Agent {
	t.Helper()
	a := &store.Agent{ID: tid(slug), ProjectID: projectID, Name: slug, Slug: slug,
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUnmentionedReplyNote_Format(t *testing.T) {
	got := unmentionedReplyNote("lead", "poster")
	want := "[note to @lead: this may be a reply that did not mention the agent it was replying to, so it may need the attention of poster]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = unmentionedReplyNote("bob", "")
	want = "[note to @bob: this may be a reply that did not mention the agent it was replying to]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnmentionedReplyNote_MemberMentionToken(t *testing.T) {
	cases := []struct {
		m    chatMemberEntry
		want string
	}{
		{chatMemberEntry{ID: "1", DisplayName: "Bob Jones", Email: "bob@example.com"}, "Bob-Jones"},
		{chatMemberEntry{ID: "1", DisplayName: "Bob!", Email: "bobby@example.com"}, "bobby"},
		{chatMemberEntry{ID: "1", Email: "carol@example.com"}, "carol"},
		{chatMemberEntry{ID: "1", DisplayName: "Dan."}, ""},
	}
	for _, c := range cases {
		if got := memberMentionToken(c.m, []chatMemberEntry{c.m}); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.m, got, c.want)
		}
	}
}

// A token is only used when it resolves to the target alone: a shared
// display name falls back to the email local part, and a local part that
// is another member's hyphenated name falls back to the full email.
func TestUnmentionedReplyNote_MemberMentionTokenUnique(t *testing.T) {
	bob1 := chatMemberEntry{ID: "1", DisplayName: "Bob Jones", Email: "bob1@example.com"}
	bob2 := chatMemberEntry{ID: "2", DisplayName: "Bob Jones", Email: "bob2@example.com"}
	if got := memberMentionToken(bob1, []chatMemberEntry{bob1, bob2}); got != "bob1" {
		t.Fatalf("shared display name: got %q, want bob1", got)
	}
	ann := chatMemberEntry{ID: "3", DisplayName: "Ann Lee", Email: "ann@example.com"}
	clash := chatMemberEntry{ID: "4", DisplayName: "Ann Lee", Email: "ann-lee@example.org"}
	other := chatMemberEntry{ID: "5", DisplayName: "Ann", Email: "x@example.net"}
	if got := memberMentionToken(clash, []chatMemberEntry{ann, clash, other}); got != "ann-lee@example.org" {
		t.Fatalf("local part clash: got %q, want full email", got)
	}
	twin := chatMemberEntry{ID: "6", DisplayName: "Zed", Email: "zed@example.com"}
	twin2 := chatMemberEntry{ID: "7", DisplayName: "Zed", Email: "ZED@example.com"}
	if got := memberMentionToken(twin, []chatMemberEntry{twin, twin2}); got != "" {
		t.Fatalf("no unique token: got %q, want empty", got)
	}
}

// Default agent, another agent posted last: delivery to the default agent
// only, with the body unchanged in storage and in the dispatch.
func TestUnmentionedReplyNote_DefaultAgentOtherPosterUnchanged(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topicID, def := unreachableTestSetup(t, "running", false, d)
	poster := otherAgent(t, s, def.ProjectID, "note-poster")
	now := time.Now().UTC()
	seedThreadMsgAt(t, s, def.ProjectID, topicID, "agent:"+def.Slug, def.ID, now.Add(-2*time.Minute))
	seedThreadMsgAt(t, s, def.ProjectID, topicID, "agent:"+poster.Slug, poster.ID, now.Add(-time.Minute))

	code, resp, m := unreachableSend(t, srv, s, topicID, "looks good")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	if m == nil || m.Msg != "looks good" || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected unchanged dispatched row, got %+v", m)
	}
	if resp["content"] != "looks good" {
		t.Fatalf("response content: got %v", resp["content"])
	}
	msgs := d.getMessages()
	if len(msgs) != 1 || msgs[0].agentSlug != def.Slug {
		t.Fatalf("expected one dispatch to %s, got %+v", def.Slug, msgs)
	}
	if msgs[0].structured == nil || msgs[0].structured.Msg != "looks good" {
		t.Fatalf("dispatched body changed: %+v", msgs[0].structured)
	}
}

// No note when the default agent posted last, or no agent posted at all.
func TestUnmentionedReplyNote_DefaultAgentNoNote(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seedPoster bool
	}{{"default is last poster", true}, {"no agent poster", false}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &brokerMockDispatcher{}
			srv, s, topicID, def := unreachableTestSetup(t, "running", false, d)
			if tc.seedPoster {
				poster := otherAgent(t, s, def.ProjectID, "note-poster")
				now := time.Now().UTC()
				seedThreadMsgAt(t, s, def.ProjectID, topicID, "agent:"+poster.Slug, poster.ID, now.Add(-2*time.Minute))
				seedThreadMsgAt(t, s, def.ProjectID, topicID, "agent:"+def.Slug, def.ID, now.Add(-time.Minute))
			}
			_, _, m := unreachableSend(t, srv, s, topicID, "hello")
			if m == nil || m.Msg != "hello" || m.DispatchState != store.MessageDispatchDispatched {
				t.Fatalf("expected unchanged dispatched row, got %+v", m)
			}
			if msgs := d.getMessages(); len(msgs) != 1 || msgs[0].structured.Msg != "hello" {
				t.Fatalf("expected one unchanged dispatch, got %+v", msgs)
			}
		})
	}
}

// Any explicit mention or reply-to target leaves the reply unchanged.
func TestUnmentionedReplyNote_ExplicitTargetsUnchanged(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topicID, def := unreachableTestSetup(t, "running", false, d)
	poster := otherAgent(t, s, def.ProjectID, "note-poster")
	posterMsg := seedThreadMsgAt(t, s, def.ProjectID, topicID, "agent:"+poster.Slug, poster.ID,
		time.Now().UTC().Add(-time.Minute))

	for _, content := range []string{
		"@" + poster.Slug + " please check",
		"please check @" + poster.Slug,
		"thanks @" + def.Slug,
		"cc @nobody-here",
	} {
		_, _, m := unreachableSend(t, srv, s, topicID, content)
		if m == nil || m.Msg != content {
			t.Fatalf("%q: expected unchanged body, got %+v", content, m)
		}
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "on it", "reply_to_id": posterMsg})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "note to") {
		t.Fatalf("reply to agent must not be noted: %s", rec.Body.String())
	}
	for _, dm := range d.getMessages() {
		if strings.Contains(dm.structured.Msg, "note to") {
			t.Fatalf("explicitly targeted dispatch was noted: %q", dm.structured.Msg)
		}
	}
}

// No default agent, another human posted: the note mentions that human
// and names the agent poster, no agent is invoked, and the row reads
// dispatched because it is now addressed to a person.
func TestUnmentionedReplyNote_NoDefaultMentionsRecentHuman(t *testing.T) {
	srv, s, topicID, a, d, projectID := noRecipientSetupProject(t)
	bob := addHumanMember(t, s, projectID, "bob@example.com", "Bob Jones")
	carol := addHumanMember(t, s, projectID, "carol@example.com", "Carol")
	seedThreadMsgAt(t, s, projectID, topicID, "user:bob@example.com", bob.ID, time.Now().UTC().Add(-time.Minute))

	code, resp, m := unreachableSend(t, srv, s, topicID, "thanks, sounds good")
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	want := "thanks, sounds good\n\n" + unmentionedReplyNote("Bob-Jones", a.Slug)
	if m == nil || m.Msg != want {
		t.Fatalf("stored body: got %+v, want %q", m, want)
	}
	if m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected dispatched, got %q", m.DispatchState)
	}
	if v, ok := resp["dispatchState"]; ok && v != store.MessageDispatchDispatched {
		t.Fatalf("expected no no_recipient in response, got %v", v)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
	waitMentionNotified(t, s, bob.ID)
	assertNotNotified(t, s, carol.ID, DevUserID)
}

// No default agent and no agent poster: the agent clause is omitted.
func TestUnmentionedReplyNote_NoDefaultNoAgentPoster(t *testing.T) {
	srv, s, topicID, d, projectID := humanOnlyTopic(t, "dev")
	bob := addHumanMember(t, s, projectID, "bob@example.com", "Bob Jones")
	seedThreadMsgAt(t, s, projectID, topicID, "user:bob@example.com", bob.ID, time.Now().UTC().Add(-time.Minute))

	_, _, m := unreachableSend(t, srv, s, topicID, "ok")
	want := "ok\n\n" + unmentionedReplyNote("Bob-Jones", "")
	if m == nil || m.Msg != want || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("got %+v, want body %q dispatched", m, want)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// No default agent and no other human poster (only the sender, or a
// non-member): no note, still no_recipient.
func TestUnmentionedReplyNote_NoFallbackKeepsNoRecipient(t *testing.T) {
	srv, s, topicID, _, d, projectID := noRecipientSetupProject(t)
	now := time.Now().UTC()
	seedThreadMsgAt(t, s, projectID, topicID, "user:dev", DevUserID, now.Add(-2*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "user:stranger", "not-a-member", now.Add(-time.Minute))

	_, _, m := unreachableSend(t, srv, s, topicID, "thanks")
	if m == nil || m.Msg != "thanks" || m.DispatchState != store.MessageDispatchNoRecipient {
		t.Fatalf("expected unchanged no_recipient row, got %+v", m)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// humanOnlyTopic creates a topic with no default agent and no messages,
// created by createdBy.
func humanOnlyTopic(t *testing.T, createdBy string) (*Server, store.Store, string, *brokerMockDispatcher, string) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	topicID := tid("norcpt-human-topic")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "humans",
		CreatedBy: createdBy, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return srv, s, topicID, d, proj.ID
}

// waitMentionNotified waits for the chat notifier (wired by
// SetWebChatStore) to store a notification for userID.
func waitMentionNotified(t *testing.T, s store.Store, userID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := s.GetNotifications(t.Context(), store.SubscriberTypeUser, userID, false)
		if err != nil {
			t.Fatalf("GetNotifications: %v", err)
		}
		if len(n) == 1 {
			return
		}
		if len(n) > 1 || time.Now().After(deadline) {
			t.Fatalf("expected one notification for %s, got %d", userID, len(n))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNotNotified checks that none of userIDs has a notification. Call it
// after waitMentionNotified: the notifier handles one message's mentions
// in a single goroutine.
func assertNotNotified(t *testing.T, s store.Store, userIDs ...string) {
	t.Helper()
	for _, id := range userIDs {
		n, err := s.GetNotifications(t.Context(), store.SubscriberTypeUser, id, false)
		if err != nil {
			t.Fatalf("GetNotifications: %v", err)
		}
		if len(n) != 0 {
			t.Fatalf("expected no notification for %s, got %d", id, len(n))
		}
	}
}

// Two members share a display name: the note mentions the intended one by
// email local part and only that person is notified.
func TestUnmentionedReplyNote_DuplicateDisplayNames(t *testing.T) {
	srv, s, topicID, _, _, projectID := noRecipientSetupProject(t)
	bob1 := addHumanMember(t, s, projectID, "bob1@example.com", "Bob Jones")
	bob2 := addHumanMember(t, s, projectID, "bob2@example.com", "Bob Jones")
	now := time.Now().UTC()
	seedThreadMsgAt(t, s, projectID, topicID, "user:bob2@example.com", bob2.ID, now.Add(-2*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "user:bob1@example.com", bob1.ID, now.Add(-time.Minute))

	_, _, m := unreachableSend(t, srv, s, topicID, "thanks")
	want := "thanks\n\n" + unmentionedReplyNote("bob1", "norcpt-agent")
	if m == nil || m.Msg != want || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("got %+v, want body %q dispatched", m, want)
	}
	waitMentionNotified(t, s, bob1.ID)
	assertNotNotified(t, s, bob2.ID, DevUserID)
}

// With no other human poster, the note falls back to the thread creator.
// A creator who is the sender is no fallback: still no_recipient.
func TestUnmentionedReplyNote_ThreadCreatorFallback(t *testing.T) {
	t.Run("creator is another member", func(t *testing.T) {
		srv, s, wcs, proj, db := setupSendTest(t)
		d := &brokerMockDispatcher{}
		srv.SetDispatcher(d)
		owner := addHumanMember(t, s, proj.ID, "owner@example.com", "Olive Owner")
		topicID := tid("creator-topic")
		if err := wcs.CreateTopic(t.Context(), WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "c",
			CreatedBy: owner.ID, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		setTopicConversationID(t, db, s, topicID, proj.ID)

		_, _, m := unreachableSend(t, srv, s, topicID, "hi")
		want := "hi\n\n" + unmentionedReplyNote("Olive-Owner", "")
		if m == nil || m.Msg != want || m.DispatchState != store.MessageDispatchDispatched {
			t.Fatalf("got %+v, want body %q dispatched", m, want)
		}
		if n := len(d.getMessages()); n != 0 {
			t.Fatalf("expected no agent dispatch, got %d", n)
		}
		waitMentionNotified(t, s, owner.ID)
	})
	t.Run("creator is the sender", func(t *testing.T) {
		srv, s, topicID, _, _ := humanOnlyTopic(t, DevUserID)
		_, _, m := unreachableSend(t, srv, s, topicID, "hi")
		if m == nil || m.Msg != "hi" || m.DispatchState != store.MessageDispatchNoRecipient {
			t.Fatalf("expected unchanged no_recipient row, got %+v", m)
		}
	})
}

// A mention that resolves to nobody (a typo) addresses no one, so with no
// default agent the reply still gets the note rather than being dropped.
func TestUnmentionedReplyNote_UnresolvedMentionGetsNote(t *testing.T) {
	srv, s, topicID, a, d, projectID := noRecipientSetupProject(t)
	bob := addHumanMember(t, s, projectID, "bob@example.com", "Bob Jones")
	seedThreadMsgAt(t, s, projectID, topicID, "user:bob@example.com", bob.ID, time.Now().UTC().Add(-time.Minute))

	_, _, m := unreachableSend(t, srv, s, topicID, "cc @nobody-here")
	want := "cc @nobody-here\n\n" + unmentionedReplyNote("Bob-Jones", a.Slug)
	if m == nil || m.Msg != want || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("got %+v, want body %q dispatched", m, want)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
	waitMentionNotified(t, s, bob.ID)
}

// Two human and two agent posters: the note picks the most recent of each.
func TestUnmentionedReplyNote_MostRecentPosters(t *testing.T) {
	srv, s, topicID, d, projectID := humanOnlyTopic(t, "dev")
	older := otherAgent(t, s, projectID, "older-agent")
	newer := otherAgent(t, s, projectID, "newer-agent")
	ann := addHumanMember(t, s, projectID, "ann@example.com", "Ann")
	ben := addHumanMember(t, s, projectID, "ben@example.com", "Ben")
	now := time.Now().UTC()
	seedThreadMsgAt(t, s, projectID, topicID, "agent:"+older.Slug, older.ID, now.Add(-5*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "user:ann@example.com", ann.ID, now.Add(-4*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "agent:"+newer.Slug, newer.ID, now.Add(-3*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "user:ben@example.com", ben.ID, now.Add(-2*time.Minute))
	seedThreadMsgAt(t, s, projectID, topicID, "user:dev", DevUserID, now.Add(-time.Minute))

	_, _, m := unreachableSend(t, srv, s, topicID, "ok")
	want := "ok\n\n" + unmentionedReplyNote("Ben", newer.Slug)
	if m == nil || m.Msg != want || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("got %+v, want body %q dispatched", m, want)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
	waitMentionNotified(t, s, ben.ID)
	assertNotNotified(t, s, ann.ID)
}
