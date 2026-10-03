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

// Tests for nc-reply-recipient: the context-menu "Reply" action must route
// the reply to the original sender agent, resolved authoritatively on the
// backend from the replied-to message ID, rather than the thread default (or
// a client-supplied agent slug).
package hub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedAgentMessage persists a message as if agent had sent it into topicID,
// so a later reply can reference it via reply_to_id. Returns the message ID.
func seedAgentMessage(t *testing.T, s store.Store, proj *store.Project, topicID string, agent *store.Agent, content string) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID:        id,
		ProjectID: proj.ID,
		Sender:    "agent:" + agent.Slug,
		SenderID:  agent.ID,
		Recipient: "thread:" + topicID,
		Msg:       content,
		Type:      "chat",
		AgentID:   agent.ID,
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedAgentMessage: %v", err)
	}
	return id
}

// seedHumanMessage persists a message as if a human had sent it into
// topicID, for the "reply to a human message" case.
func seedHumanMessage(t *testing.T, s store.Store, proj *store.Project, topicID, senderID, content string) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID:        id,
		ProjectID: proj.ID,
		Sender:    "user:" + senderID,
		SenderID:  senderID,
		Recipient: "thread:" + topicID,
		Msg:       content,
		Type:      "chat",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedHumanMessage: %v", err)
	}
	return id
}

// TestReplyRecipient_RoutesToOriginalSender: replying to an agent's message
// dispatches to that agent, even though the thread's default agent is a
// different agent entirely.
func TestReplyRecipient_RoutesToOriginalSender(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("reply-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "reply-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	defaultAgent := &store.Agent{ID: tid("reply-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{sender, defaultAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("reply-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original message from sender")

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != sender.Slug {
		t.Fatalf("dispatched to %q, want original sender %q (not thread default %q)",
			dispatched[0].agentSlug, sender.Slug, defaultAgent.Slug)
	}
}

// TestReplyRecipient_LeadingMentionOverridesReplyTarget: a leading @mention
// in the reply body wins over both the reply-to-agent override and the
// thread default.
func TestReplyRecipient_LeadingMentionOverridesReplyTarget(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("reply-mention-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "reply-mention-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	mentioned := &store.Agent{ID: tid("reply-mentioned"), ProjectID: proj.ID, Name: "Mentioned", Slug: "reply-mentioned",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{sender, mentioned} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("reply-mention-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-mention-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original message from sender")

	body := map[string]string{"content": "@reply-mentioned please take this", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != mentioned.Slug {
		t.Fatalf("dispatched to %q, want leading-mentioned agent %q (not reply sender %q)",
			dispatched[0].agentSlug, mentioned.Slug, sender.Slug)
	}
}

// TestReplyRecipient_ReplyToHumanMessage_Unchanged: replying to a human's
// message keeps existing behavior — routed to the thread default agent.
func TestReplyRecipient_ReplyToHumanMessage_Unchanged(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	defaultAgent := &store.Agent{ID: tid("reply-human-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-human-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, defaultAgent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("reply-human-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-human-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedHumanMessage(t, s, proj, topicID, DevUserID, "hello from a human")

	body := map[string]string{"content": "replying to the human", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != defaultAgent.Slug {
		t.Fatalf("dispatched to %q, want thread default %q (unchanged for human reply target)",
			dispatched[0].agentSlug, defaultAgent.Slug)
	}
}

// TestReplyRecipient_SuspendedSender_Unreachable: replying to a message from
// an agent that is now suspended reports agent_unreachable, and does not
// fall back to the thread default.
func TestReplyRecipient_SuspendedSender_Unreachable(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("reply-suspended-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "reply-suspended-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	defaultAgent := &store.Agent{ID: tid("reply-suspended-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-suspended-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{sender, defaultAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("reply-suspended-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-suspended-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original message from sender")

	// Suspend the sender after the original message was sent.
	sender.Phase = "suspended"
	if err := s.UpdateAgent(ctx, sender); err != nil {
		t.Fatalf("UpdateAgent (suspend): %v", err)
	}

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected dispatchState=failed, got %v", resp)
	}
	if resp["dispatchFailureCode"] != dispatchFailureCodeAgentUnreachable {
		t.Fatalf("expected dispatchFailureCode=%q, got %v", dispatchFailureCodeAgentUnreachable, resp)
	}
	if len(dispatcher.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches, got %d", len(dispatcher.getMessages()))
	}
}

// TestReplyRecipient_DeletedSender_Unreachable: replying to a message from an
// agent that has since been soft-deleted reports agent_unreachable and does
// not silently fall back to the thread default.
func TestReplyRecipient_DeletedSender_Unreachable(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("reply-deleted-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "reply-deleted-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	defaultAgent := &store.Agent{ID: tid("reply-deleted-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-deleted-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{sender, defaultAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("reply-deleted-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-deleted-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original message from sender")

	// Soft-delete the sender after the original message was sent.
	sender.DeletedAt = time.Now()
	if err := s.UpdateAgent(ctx, sender); err != nil {
		t.Fatalf("UpdateAgent (delete): %v", err)
	}

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected dispatchState=failed, got %v", resp)
	}
	if resp["dispatchFailureCode"] != dispatchFailureCodeAgentUnreachable {
		t.Fatalf("expected dispatchFailureCode=%q, got %v", dispatchFailureCodeAgentUnreachable, resp)
	}
	if len(dispatcher.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches (must not fall back to thread default), got %d", len(dispatcher.getMessages()))
	}
}

// TestReplyRecipient_CrossConversationReplyToID_Ignored: a reply_to_id
// pointing at a message from a different conversation must not be honored —
// routing falls back to the current thread's ordinary rules (its default
// agent here).
func TestReplyRecipient_CrossConversationReplyToID_Ignored(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	otherAgent := &store.Agent{ID: tid("reply-other-conv-agent"), ProjectID: proj.ID, Name: "OtherConvAgent", Slug: "reply-other-conv-agent",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	defaultAgent := &store.Agent{ID: tid("reply-cross-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-cross-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{otherAgent, defaultAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	otherTopicID := tid("reply-other-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: otherTopicID, ProjectID: proj.ID, Name: "other-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateTopic (other): %v", err)
	}
	setTopicConversationID(t, db, s, otherTopicID, proj.ID)
	foreignMsgID := seedAgentMessage(t, s, proj, otherTopicID, otherAgent, "message in another conversation")

	topicID := tid("reply-cross-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "this-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "replying with a foreign reply_to_id", "reply_to_id": foreignMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != defaultAgent.Slug {
		t.Fatalf("dispatched to %q, want thread default %q (cross-conversation reply_to_id must be ignored, not routed to %q)",
			dispatched[0].agentSlug, defaultAgent.Slug, otherAgent.Slug)
	}
}

// TestReplyRecipient_ForeignProjectSender_FallsThroughToDefault: a review R1
// regression guard (nc-reply-recipient review). A message from an agent in a
// different project ends up in a project-A topic (e.g. via a planted or
// mis-derived thread_id on agent outbound persistence). Replying to it must
// NOT dispatch to the foreign-project agent — it must fall through to the
// thread's own default agent, mirroring the DEF-31 foreignProjectDefault
// handling of the topic-default path.
func TestReplyRecipient_ForeignProjectSender_FallsThroughToDefault(t *testing.T) {
	srv, s, wcs, projA, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	projB := &store.Project{ID: tid("reply-foreign-projB"), Name: "reply-foreign-projB", Slug: "reply-foreign-projb",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, projB); err != nil {
		t.Fatalf("CreateProject(projB): %v", err)
	}

	foreignSender := &store.Agent{ID: tid("reply-foreign-sender"), ProjectID: projB.ID, Name: "Foreign", Slug: "reply-foreign-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("CreateAgent(foreignSender): %v", err)
	}
	defaultAgent := &store.Agent{ID: tid("reply-foreign-default"), ProjectID: projA.ID, Name: "Default", Slug: "reply-foreign-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, defaultAgent); err != nil {
		t.Fatalf("CreateAgent(defaultAgent): %v", err)
	}

	topicID := tid("reply-foreign-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: projA.ID, Name: "reply-foreign-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, projA.ID)

	// A project-B agent's message lands in the project-A topic (the
	// precondition the review's probe relied on).
	origMsgID := seedAgentMessage(t, s, projA, topicID, foreignSender, "message from a foreign-project agent")

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != defaultAgent.Slug {
		t.Fatalf("dispatched to %q, want thread default %q (must not route to foreign-project sender %q)",
			dispatched[0].agentSlug, defaultAgent.Slug, foreignSender.Slug)
	}
}

// TestReplyRecipient_HardDeletedSender_Unreachable: a review R2 regression
// guard (nc-reply-recipient review). Replying to a message whose sender
// agent record has been hard-deleted entirely (store.ErrNotFound, not just
// soft-deleted) reports agent_unreachable and does not silently fall back to
// the thread default.
func TestReplyRecipient_HardDeletedSender_Unreachable(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("reply-harddeleted-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "reply-harddeleted-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	defaultAgent := &store.Agent{ID: tid("reply-harddeleted-default"), ProjectID: proj.ID, Name: "Default", Slug: "reply-harddeleted-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{sender, defaultAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("reply-harddeleted-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "reply-harddeleted-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original message from sender")

	// Hard-delete the sender's agent record entirely (not a soft delete):
	// GetAgent(sender.ID) now returns store.ErrNotFound.
	if err := s.DeleteAgent(ctx, sender.ID); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp["dispatchState"] != "failed" {
		t.Fatalf("expected dispatchState=failed, got %v", resp)
	}
	if resp["dispatchFailureCode"] != dispatchFailureCodeAgentUnreachable {
		t.Fatalf("expected dispatchFailureCode=%q, got %v", dispatchFailureCodeAgentUnreachable, resp)
	}
	if len(dispatcher.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches (must not fall back to thread default), got %d", len(dispatcher.getMessages()))
	}
}

// TestReplyRecipient_EmptyProjectID_ForeignAgent_NotDispatched: a review round-2
// R1 regression guard (nc-reply-recipient review-2). A user-user DM has no
// project (resolveProjectFromDMKey returns "" for it), so the project guard in
// resolveReplyTarget must fail closed on projectID == "" rather than skip the
// check. This plants a foreign-project agent's message in a user-user DM
// thread (the same precondition the review's probe used) and replies to it:
// the override must not select the foreign agent, and the send must not
// dispatch to it.
func TestReplyRecipient_EmptyProjectID_ForeignAgent_NotDispatched(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	projB := &store.Project{ID: tid("reply-empty-proj-projB"), Name: "reply-empty-proj-projB", Slug: "reply-empty-proj-projb",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, projB); err != nil {
		t.Fatalf("CreateProject(projB): %v", err)
	}
	foreignSender := &store.Agent{ID: tid("reply-empty-proj-sender"), ProjectID: projB.ID, Name: "Foreign", Slug: "reply-empty-proj-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("CreateAgent(foreignSender): %v", err)
	}

	// A user-user DM key: resolveProjectFromDMKey returns "" for this shape
	// (it only resolves a project for the dm:agent:... shape).
	peerID := tid("reply-empty-proj-peer")
	dmKey, err := messages.DMConversationKey("user", DevUserID, "user", peerID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	setDMConversationID(t, s, dmKey, proj.ID)

	// A project-B agent's message lands in this DM thread (planted, mirroring
	// the topic-path precondition review-1 already accepted as reachable).
	origMsgID := seedAgentMessage(t, s, projB, dmKey, foreignSender, "message from a foreign-project agent")

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Type != messages.TypeReply {
		t.Fatalf("expected type %q (human-to-human, no override), got %q", messages.TypeReply, resp.Type)
	}
	if resp.DispatchFailureCode != "" || resp.DispatchState == "failed" {
		t.Fatalf("expected no dispatch failure (empty projectID must not turn into a 500 or unreachable report), got state=%q code=%q",
			resp.DispatchState, resp.DispatchFailureCode)
	}
	if len(dispatcher.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches (must not route to foreign-project agent %q), got %d",
			foreignSender.Slug, len(dispatcher.getMessages()))
	}
}

// TestReplyRecipient_SoftDeletedForeignProjectSender_FallsThroughToDefault: a
// review round-2 R2 regression guard (nc-reply-recipient review-2). Mirrors
// TestReplyRecipient_ForeignProjectSender_FallsThroughToDefault but for the
// soft-deleted branch of resolveReplyTarget: a soft-deleted foreign-project
// agent's message sits in a project-A topic. Replying to it must fall through
// to the thread default rather than reporting agent_unreachable with the
// foreign agent's slug/ID (which is what round-1 R1 asked to prevent).
func TestReplyRecipient_SoftDeletedForeignProjectSender_FallsThroughToDefault(t *testing.T) {
	srv, s, wcs, projA, db := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	projB := &store.Project{ID: tid("reply-sd-foreign-projB"), Name: "reply-sd-foreign-projB", Slug: "reply-sd-foreign-projb",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, projB); err != nil {
		t.Fatalf("CreateProject(projB): %v", err)
	}

	foreignSender := &store.Agent{ID: tid("reply-sd-foreign-sender"), ProjectID: projB.ID, Name: "Foreign", Slug: "reply-sd-foreign-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("CreateAgent(foreignSender): %v", err)
	}
	defaultAgent := &store.Agent{ID: tid("reply-sd-foreign-default"), ProjectID: projA.ID, Name: "Default", Slug: "reply-sd-foreign-default",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, defaultAgent); err != nil {
		t.Fatalf("CreateAgent(defaultAgent): %v", err)
	}

	topicID := tid("reply-sd-foreign-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: projA.ID, Name: "reply-sd-foreign-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: defaultAgent.Slug}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, projA.ID)

	origMsgID := seedAgentMessage(t, s, projA, topicID, foreignSender, "message from a soft-deleted foreign-project agent")

	// Soft-delete the foreign sender after the original message was sent, so
	// resolveReplyTarget's soft-deleted branch (not the live branch) applies.
	foreignSender.DeletedAt = time.Now()
	if err := s.UpdateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("UpdateAgent (delete): %v", err)
	}

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp["dispatchFailureCode"] != nil {
		t.Fatalf("expected no dispatchFailureCode (must fall through to thread default, not report unreachable with the foreign agent's identity), got %v", resp)
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != defaultAgent.Slug {
		t.Fatalf("dispatched to %q, want thread default %q (must not surface soft-deleted foreign-project sender %q)",
			dispatched[0].agentSlug, defaultAgent.Slug, foreignSender.Slug)
	}
}

// TestReplyRecipient_EmptyProjectID_SoftDeletedForeignSender_NotDispatched: a
// review round-3 Optional regression guard (nc-reply-recipient review-3).
// Combines TestReplyRecipient_EmptyProjectID_ForeignAgent_NotDispatched's
// user-user DM (empty projectID) with
// TestReplyRecipient_SoftDeletedForeignProjectSender_FallsThroughToDefault's
// soft-deleted sender, so it exercises resolveReplyTarget's soft-deleted
// branch under an empty projectID specifically. Without this test, reverting
// only that branch's guard to the round-2 form
// (`if projectID != "" && replyAgent.ProjectID != projectID`) leaves every
// TestReplyRecipient_* test passing (review-3 mutation M6): the empty-
// projectID test above only exercises the live-sender branch, and the
// soft-deleted test above only uses a non-empty projectID. A soft-deleted
// foreign-project sender's message in a user-user DM must fall through to
// human-to-human, not be reported agent_unreachable with the foreign agent's
// identity.
func TestReplyRecipient_EmptyProjectID_SoftDeletedForeignSender_NotDispatched(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	projB := &store.Project{ID: tid("reply-empty-sd-projB"), Name: "reply-empty-sd-projB", Slug: "reply-empty-sd-projb",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, projB); err != nil {
		t.Fatalf("CreateProject(projB): %v", err)
	}
	foreignSender := &store.Agent{ID: tid("reply-empty-sd-sender"), ProjectID: projB.ID, Name: "Foreign", Slug: "reply-empty-sd-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("CreateAgent(foreignSender): %v", err)
	}

	// A user-user DM key: resolveProjectFromDMKey returns "" for this shape
	// (it only resolves a project for the dm:agent:... shape).
	peerID := tid("reply-empty-sd-peer")
	dmKey, err := messages.DMConversationKey("user", DevUserID, "user", peerID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	setDMConversationID(t, s, dmKey, proj.ID)

	// A project-B agent's message lands in this DM thread (planted, mirroring
	// the live-branch empty-projectID test's precondition).
	origMsgID := seedAgentMessage(t, s, projB, dmKey, foreignSender, "message from a soft-deleted foreign-project agent")

	// Soft-delete the foreign sender after the original message was sent, so
	// resolveReplyTarget's soft-deleted branch (not the live branch) applies.
	foreignSender.DeletedAt = time.Now()
	if err := s.UpdateAgent(ctx, foreignSender); err != nil {
		t.Fatalf("UpdateAgent (delete): %v", err)
	}

	body := map[string]string{"content": "replying to you", "reply_to_id": origMsgID}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Type != messages.TypeReply {
		t.Fatalf("expected type %q (human-to-human, no override), got %q", messages.TypeReply, resp.Type)
	}
	if resp.DispatchFailureCode != "" || resp.DispatchState == "failed" {
		t.Fatalf("expected no dispatch failure (must not report the soft-deleted foreign-project sender %q as unreachable), got state=%q code=%q",
			foreignSender.Slug, resp.DispatchState, resp.DispatchFailureCode)
	}
	if len(dispatcher.getMessages()) != 0 {
		t.Fatalf("expected zero dispatches (must not route to foreign-project agent %q), got %d",
			foreignSender.Slug, len(dispatcher.getMessages()))
	}
}
