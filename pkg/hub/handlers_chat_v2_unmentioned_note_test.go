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

// Tests for the note appended to unmentioned thread replies: it is
// addressed to the default agent (delivery unchanged) when another agent
// posted last, or to the most recent other human poster when no agent
// receives the reply. Explicitly addressed replies are never noted.
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
	want := "[note to @lead: this may be a reply that did not mention the agent it was replying to, so it may need the attention of @poster]"
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
		{chatMemberEntry{DisplayName: "Bob Jones", Email: "bob@example.com"}, "Bob-Jones"},
		{chatMemberEntry{DisplayName: "Bob!", Email: "bobby@example.com"}, "bobby"},
		{chatMemberEntry{Email: "carol@example.com"}, "carol"},
		{chatMemberEntry{DisplayName: "Dan."}, ""},
	}
	for _, c := range cases {
		if got := memberMentionToken(c.m); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.m, got, c.want)
		}
	}
}

// Default agent, another agent posted last: delivery to the default agent
// only, and both the stored body and the dispatched body carry the note.
func TestUnmentionedReplyNote_DefaultAgentOtherPoster(t *testing.T) {
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
	want := "looks good\n\n" + unmentionedReplyNote(def.Slug, poster.Slug)
	if m == nil || m.Msg != want {
		t.Fatalf("stored body: got %+v, want %q", m, want)
	}
	if m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected dispatched, got %q", m.DispatchState)
	}
	if resp["content"] != want {
		t.Fatalf("response content: got %v", resp["content"])
	}
	msgs := d.getMessages()
	if len(msgs) != 1 || msgs[0].agentSlug != def.Slug {
		t.Fatalf("expected one dispatch to %s, got %+v", def.Slug, msgs)
	}
	if msgs[0].structured == nil || msgs[0].structured.Msg != want {
		t.Fatalf("dispatched body: got %+v, want %q", msgs[0].structured, want)
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
}

// No default agent and no agent poster: the agent clause is omitted.
func TestUnmentionedReplyNote_NoDefaultNoAgentPoster(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	projectID := proj.ID
	topicID := tid("norcpt-human-topic")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{ID: topicID, ProjectID: projectID, Name: "humans",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, projectID)
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
