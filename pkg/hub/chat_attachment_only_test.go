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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Attachment-only chat sends (ptone/scion#6146): a send with attachments
// and no text is stored with an empty body on every send path; a send
// with neither stays a 400.

// requireAttachmentOnlyStored asserts that message id is stored with an
// empty body and that history shows it, not deleted, with attachment
// attID.
func (f *sendFixture) requireAttachmentOnlyStored(t *testing.T, user *store.User, key, id, attID string) {
	t.Helper()
	stored, err := f.st.GetMessage(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "", stored.Msg)
	h := f.history(t, user, key)
	found := false
	for _, m := range h.Messages {
		if m.ID == id {
			found = true
		}
	}
	require.True(t, found, "history lists the attachment-only message")
	refs := h.MessageAttachments[id]
	require.Len(t, refs, 1)
	require.Equal(t, attID, refs[0].ID)
}

// TestWebSend_AttachmentOnly_UserDM: a user-to-user DM with an attachment
// and no text is stored (it was a 500), for an artifact upload and for an
// AttachmentStore upload.
func TestWebSend_AttachmentOnly_UserDM(t *testing.T) {
	f := newSendFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)

	id := f.uploadAs(t, f.ua, "", "photo.txt", "bytes")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	resp := f.sendResponse(t, got)
	require.Equal(t, "", resp.Content)
	require.Len(t, resp.Attachments, 1)
	f.requireBound(t, id, resp.ID)
	f.requireAttachmentOnlyStored(t, f.ua, dm, resp.ID, id)

	legacy := f.attach(t, "", f.ua.ID, "legacy.txt")
	got = f.send(t, f.ua, dm, map[string]interface{}{"content": "  ", "attachments": []string{legacy}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	resp = f.sendResponse(t, got)
	f.requireAttachmentOnlyStored(t, f.ua, dm, resp.ID, legacy)
}

// TestWebSend_AttachmentOnly_Thread: a thread message with an attachment
// and no text, addressed to no agent, is stored.
func TestWebSend_AttachmentOnly_Thread(t *testing.T) {
	f := newSendFixture(t)
	id := f.uploadAs(t, f.ua, f.projA.ID, "notes.txt", "bytes")
	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	resp := f.sendResponse(t, got)
	require.Equal(t, "", resp.Content)
	f.requireBound(t, id, resp.ID)
	f.requireAttachmentOnlyStored(t, f.ua, f.topicA, resp.ID, id)
}

// TestWebSend_AttachmentOnly_AgentRouted: an attachment-only send that
// reaches an agent (topic default agent, and an agent DM) is stored and
// dispatched with the attachment listed for delivery (it was a 400 from
// envelope validation).
func TestWebSend_AttachmentOnly_AgentRouted(t *testing.T) {
	f := newSendFixture(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	agentDM := dmKeyFor(t, "user", f.ua.ID, "agent", f.target.ID)
	for _, key := range []string{f.topicA, agentDM} {
		id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
		got := f.send(t, f.ua, key, map[string]interface{}{"content": "", "attachments": []string{id}})
		require.Equal(t, http.StatusCreated, got.status, "%s: %s", key, got.body)
		resp := f.sendResponse(t, got)
		require.Equal(t, "", resp.Content)
		f.requireBound(t, id, resp.ID)
		call := f.lastDispatch(t, f.target)
		require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1, key)
		require.Equal(t, id, call.StructuredMessage.AttachmentDeliveries[0].ArtifactID)
		require.Equal(t, resp.ID, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
		f.requireAttachmentOnlyStored(t, f.ua, key, resp.ID, id)
	}
}

// TestWebSend_NoContentNoAttachmentsRefused: a send with neither text nor
// attachments stays a 400 on every path, and nothing is stored.
func TestWebSend_NoContentNoAttachmentsRefused(t *testing.T) {
	f := newSendFixture(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	keys := []string{
		dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID),
		dmKeyFor(t, "user", f.ua.ID, "agent", f.target.ID),
		f.topicA,
	}
	for _, key := range keys {
		for _, body := range []map[string]interface{}{
			{"content": ""},
			{"content": " \n\t "},
			{"content": "", "attachments": []string{}},
		} {
			got := f.send(t, f.ua, key, body)
			require.Equal(t, http.StatusBadRequest, got.status, "%s: %s", key, got.body)
			require.Contains(t, got.body, "content or attachments required")
		}
	}
	require.Equal(t, 0, f.countStored(t, ""))
}

// TestChatHistory_AttachmentOnlyReplyPreview: a reply to an
// attachment-only message previews it as "Attachment", not as empty text.
func TestChatHistory_AttachmentOnlyReplyPreview(t *testing.T) {
	f := newSendFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "photo.txt", "bytes")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	parent := f.sendResponse(t, got)

	got = f.send(t, f.uc, dm, map[string]interface{}{"content": "nice", "reply_to_id": parent.ID})
	require.Equal(t, http.StatusCreated, got.status, got.body)

	h := f.history(t, f.uc, dm)
	preview, ok := h.ReplyPreviews[parent.ID]
	require.True(t, ok, "history carries a preview of the replied-to message")
	require.Equal(t, chatAttachmentOnlyPreview, preview.Content)
}

// TestChatHistoryMessageDeleted: the deleted flag of the extension row
// decides; an empty body counts as deleted only when the extension rows
// could not be read.
func TestChatHistoryMessageDeleted(t *testing.T) {
	now := time.Now()
	empty := store.Message{ID: "m1", Msg: ""}
	text := store.Message{ID: "m2", Msg: "hi"}
	exts := map[string]*WebChatMessageExt{"m3": {DeletedAt: &now}}
	deleted := store.Message{ID: "m3", Msg: ""}

	require.False(t, chatHistoryMessageDeleted(empty, exts, true), "attachment-only message is live")
	require.False(t, chatHistoryMessageDeleted(empty, nil, true), "no extension rows: live")
	require.False(t, chatHistoryMessageDeleted(text, exts, true))
	require.True(t, chatHistoryMessageDeleted(deleted, exts, true))
	require.True(t, chatHistoryMessageDeleted(empty, nil, false), "unknown: an empty body fails closed")
	require.False(t, chatHistoryMessageDeleted(text, nil, false))
}

// TestChatSendValidationEnvelope: an attachment-only agent send passes
// envelope validation through a copy that names its attachments; the
// message itself is unchanged, and an empty send without attachments is
// still refused.
func TestChatSendValidationEnvelope(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version, Sender: "user:a", Recipient: "agent:b",
		Type: messages.TypeInstruction, Channel: "web", ThreadID: "t",
	}
	require.Error(t, messaging.ValidateLegacyMessage(chatSendValidationEnvelope(msg, nil)))

	refs := []AttachmentRef{{ID: "att-1"}, {ID: "att-2"}}
	v := chatSendValidationEnvelope(msg, refs)
	require.NoError(t, messaging.ValidateLegacyMessage(v))
	require.Equal(t, []string{"att-1", "att-2"}, v.Attachments)
	require.Empty(t, msg.Attachments, "the message itself is not changed")

	msg.Msg = "text"
	require.Same(t, msg, chatSendValidationEnvelope(msg, refs), "a message with text is validated as is")
}

// TestChatHistory_DeletedAttachmentOnlyMessage: once an attachment-only
// message is deleted, history marks it deleted and a reply previews it as
// "[deleted]", not as an attachment.
func TestChatHistory_DeletedAttachmentOnlyMessage(t *testing.T) {
	f := newSendFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	id := f.uploadAs(t, f.ua, "", "photo.txt", "bytes")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	parent := f.sendResponse(t, got)
	got = f.send(t, f.uc, dm, map[string]interface{}{"content": "nice", "reply_to_id": parent.ID})
	require.Equal(t, http.StatusCreated, got.status, got.body)

	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodDelete, "/api/v1/chat/conversations/"+dm+"/messages/"+parent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	h := f.history(t, f.uc, dm)
	ext := h.MessageExtensions[parent.ID]
	require.NotNil(t, ext, "history carries the deleted message's extension row")
	require.NotNil(t, ext.DeletedAt)
	require.Equal(t, "[deleted]", h.ReplyPreviews[parent.ID].Content)
}

// TestBroadcast_EmptyBodyRefused: a project broadcast without text is a
// 400 on the direct and the message-broker path, for an agent and a user
// sender, and nothing is stored or dispatched (the messages store accepts
// an empty body since ptone/scion#6146).
func TestBroadcast_EmptyBodyRefused(t *testing.T) {
	f := newSendFixture(t)
	path := "/api/v1/projects/" + f.projA.ID + "/broadcast"
	body := BroadcastMessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339), Type: messages.TypeInstruction,
	}}
	check := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "msg is required")
	}
	check(f.agentRequest(t, f.sender, jsonRequest(t, path, body)))
	check(doRequestAsUser(t, f.srv, f.ua, http.MethodPost, path, body))
	f.withBrokerProxy(t)
	check(f.agentRequest(t, f.sender, jsonRequest(t, path, body)))
	require.Empty(t, f.disp.getCalls(), "nothing is dispatched")
	require.Equal(t, 0, f.countStored(t, ""), "nothing is stored")
}

// TestMentions_NoTextNotCopied: a message without text and without parent
// attachments is not copied to the agents it mentions explicitly, on both
// mention paths; each mention reports why, and nothing is dispatched to
// the mentioned agent.
func TestMentions_NoTextNotCopied(t *testing.T) {
	f := newSendFixture(t)
	ctx := agentCtx(context.Background(), f.sender)
	msg := &messages.StructuredMessage{
		Version: messages.Version, Sender: "agent:" + f.sender.Slug, SenderID: f.sender.ID,
		Recipient: "agent:" + f.target.Slug, RecipientID: f.target.ID, Type: messages.TypeInstruction,
		Attachments: []string{"/scratch/shot.png"},
	}
	res := f.srv.processMentions(ctx, []string{f.aa.Slug}, f.target, msg, "", "")
	require.Len(t, res, 1)
	require.Equal(t, f.aa.Slug, res[0].Slug)
	require.Equal(t, "error", res[0].Status)
	require.Equal(t, errMentionCopyNeedsText, res[0].Error)

	res = f.srv.fanOutAgentMentions(ctx, agentMentionFanoutInput{
		Sender: f.sender, SenderIdent: GetAgentIdentityFromContext(ctx), Primary: f.target,
		Type: messages.TypeInstruction, Explicit: []string{f.aa.Slug}, ParentMessageID: "parent-message-id",
	})
	require.Len(t, res, 1)
	require.Equal(t, f.aa.Slug, res[0].Slug)
	require.Equal(t, "error", res[0].Status)
	require.Equal(t, errMentionCopyNeedsText, res[0].Error)

	for _, c := range f.disp.getCalls() {
		require.False(t, c.Agent != nil && c.Agent.ID == f.aa.ID, "nothing is dispatched to the mentioned agent")
	}
	require.Equal(t, 0, f.countStored(t, ""), "no mention copy is stored")

	// Control: with text, the mention is not refused for want of text.
	msg.Msg = "see this"
	res = f.srv.processMentions(ctx, []string{f.aa.Slug}, f.target, msg, "", "")
	require.Len(t, res, 1)
	require.NotEqual(t, errMentionCopyNeedsText, res[0].Error)
}

// TestMentionCopy_NoTextWithParentAttachmentsDelivered: an agent's message
// without text that mentions an agent explicitly is copied when the copy
// delivers the parent's attachments: the mention is delivered and the copy
// lists the attachment.
func TestMentionCopy_NoTextWithParentAttachmentsDelivered(t *testing.T) {
	f, c := newMentionFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "primary", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)

	in := mentionInput(t, f, primary)
	in.Msg = ""
	in.Primary = f.target
	in.Explicit = []string{c.Slug}
	res := f.srv.fanOutAgentMentions(context.Background(), in)
	require.Len(t, res, 1)
	require.Equal(t, c.Slug, res[0].Slug)
	require.Equal(t, "delivered", res[0].Status, res[0].Error)
	call := f.lastDispatch(t, c)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, id, call.StructuredMessage.AttachmentDeliveries[0].ArtifactID)
	require.Equal(t, primary, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
}

// TestMentionFanout_NoTextKeepsResolution: on a message without text or
// parent attachments, the fan-out still resolves names as usual: the
// primary never appears, a human address is dropped, an unknown name is
// not_found, and only the agent that would have been delivered to reports
// the no-text error (nothing is dispatched).
func TestMentionFanout_NoTextKeepsResolution(t *testing.T) {
	f := newSendFixture(t)
	ctx := agentCtx(context.Background(), f.sender)
	res := f.srv.fanOutAgentMentions(ctx, agentMentionFanoutInput{
		Sender: f.sender, SenderIdent: GetAgentIdentityFromContext(ctx), Primary: f.target,
		Type:     messages.TypeInstruction,
		Explicit: []string{f.target.Slug, "someone@example.com", "no-such-agent", f.aa.Slug},
	})
	got := map[string]messages.MentionResult{}
	for _, r := range res {
		got[r.Slug] = r
	}
	require.Len(t, res, 2, "%+v", res)
	require.Equal(t, "not_found", got["no-such-agent"].Status)
	require.Equal(t, "error", got[f.aa.Slug].Status)
	require.Equal(t, errMentionCopyNeedsText, got[f.aa.Slug].Error)
	for _, c := range f.disp.getCalls() {
		require.False(t, c.Agent != nil && c.Agent.ID == f.aa.ID, "nothing is dispatched to the mentioned agent")
	}
}

// TestAgentSend_AttachmentOnlyWithMention: an agent sends a message with no
// text, an inline path attachment, an uploaded attachment and an explicit
// mention, through the message route: the message is stored with an empty
// body, and the mention copy is delivered with the uploaded attachment.
func TestAgentSend_AttachmentOnlyWithMention(t *testing.T) {
	f, c := newMentionFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	body := f.messageBody(f.target, "", []string{id})
	body.StructuredMessage.Attachments = []string{"/scion-volumes/scratchpad/.attachments/snd/m1/notes.txt"}
	body.Mentions = []string{c.Slug}
	rec := f.agentRequest(t, f.sender, jsonRequest(t, "/api/v1/agents/"+f.target.ID+"/message", body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	stored, err := f.st.GetMessage(context.Background(), primary)
	require.NoError(t, err)
	require.Equal(t, "", stored.Msg)
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, primary, c))
	call := f.lastDispatch(t, c)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, id, call.StructuredMessage.AttachmentDeliveries[0].ArtifactID)
	require.Equal(t, primary, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
}
