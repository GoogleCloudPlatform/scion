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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestStripClientAttachmentRefs(t *testing.T) {
	assert.Nil(t, stripClientAttachmentRefs(nil))

	md := map[string]string{"keep": "me"}
	assert.Equal(t, md, stripClientAttachmentRefs(md), "a map without the key is returned as is")

	in := map[string]string{"keep": "me", attachmentsMetadataKey: `[{"id":"x"}]`}
	out := stripClientAttachmentRefs(in)
	assert.Equal(t, map[string]string{"keep": "me"}, out)
	assert.Contains(t, in, attachmentsMetadataKey, "the caller's map is not changed")
}

// clientAttachmentMetadata stores two attachments the sending agent did not
// ingest (a direct-message upload and a file of another project) and
// returns metadata naming them the way the hub names attachments.
func clientAttachmentMetadata(t *testing.T, srv *Server) (map[string]string, []string) {
	t.Helper()
	ctx := context.Background()
	srv.mu.RLock()
	wcs := srv.webChatStore
	srv.mu.RUnlock()
	require.NotNil(t, wcs)
	var refs []AttachmentRef
	var ids []string
	for _, a := range []AttachmentMeta{
		{ID: tid("client-md-dm-upload"), ProjectID: "", Filename: "dm.txt", MimeType: "text/plain", Size: 1, UploadedBy: tid("client-md-someone")},
		{ID: tid("client-md-other-project"), ProjectID: tid("client-md-other-proj"), Filename: "b.txt", MimeType: "text/plain", Size: 1, UploadedBy: tid("client-md-someone")},
	} {
		a.CreatedAt = time.Now().UTC()
		require.NoError(t, wcs.CreateAttachment(ctx, a))
		refs = append(refs, AttachmentRef{ID: a.ID, Name: a.Filename, MimeType: a.MimeType, Size: a.Size})
		ids = append(ids, a.ID)
	}
	encoded, ok := attachmentRefsMetadata(refs)
	require.True(t, ok)
	return map[string]string{attachmentsMetadataKey: encoded, "keep": "me"}, ids
}

func postRefOutbound(t *testing.T, srv *Server, sender *store.Agent, identity Identity, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(requestAuthCtx(r.Context(), identity))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, r, sender.ID)
	return rr
}

// Broker delivery to a user links attachments from message metadata; an
// agent cannot name attachments there itself.
func TestAgentOutbound_ClientAttachmentMetadataNotLinked(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher, _ := paritySetup(t)
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	md, attachmentIDs := clientAttachmentMetadata(t, srv)

	rr := postRefOutbound(t, srv, sender, tokenBackedSender(t, s, sender), OutboundMessageRequest{
		Recipient: "user:" + owner.Email,
		Msg:       "files for you",
		Type:      "instruction",
		Metadata:  md,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var row *store.Message
	require.Eventually(t, func() bool {
		rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: sender.ID, RecipientID: owner.ID}, store.ListOptions{Limit: 10})
		if err != nil || len(rows.Items) != 1 {
			return false
		}
		row = &rows.Items[0]
		return true
	}, 5*time.Second, 20*time.Millisecond, "the broker path must persist the message")

	linked, err := srv.webChatStore.GetAttachmentsByMessage(context.Background(), row.ID)
	require.NoError(t, err)
	assert.Empty(t, linked, "attachments named in caller metadata are never linked")
	for _, id := range attachmentIDs {
		ids, err := srv.webChatStore.ListMessageIDsForAttachment(context.Background(), id, 20)
		require.NoError(t, err)
		assert.Empty(t, ids)
	}
}

// The agent-to-agent path of the outbound route drops the key as well, and
// keeps the rest of the caller's metadata.
func TestAgentOutbound_AgentToAgentClientAttachmentMetadataRemoved(t *testing.T) {
	srv, s, _, sender, target, convID, dispatcher, _ := paritySetup(t)
	newChatV2WebChatStore(t, srv, s)
	md, _ := clientAttachmentMetadata(t, srv)

	rr := postRefOutbound(t, srv, sender, tokenBackedSender(t, s, sender), OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             "files for you",
		Type:            "instruction",
		Metadata:        md,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.NotContains(t, calls[0].StructuredMessage.Metadata, attachmentsMetadataKey)
	assert.Equal(t, "me", calls[0].StructuredMessage.Metadata["keep"])
}

func TestAgentOutbound_ThreadIDOfOtherProjectTopicMatchesMissing(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	wcs := attachWebChatStore(t, srv, s)
	setupThreadTestChannels(t, srv, s, project, "web")

	other := &store.Project{ID: tid("outbound-ref-other-project"), Name: "Other", Slug: "outbound-ref-other",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, other))
	otherTopic := tid("outbound-ref-other-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: otherTopic, ProjectID: other.ID, Name: "elsewhere", CreatedBy: user.ID, CreatedAt: time.Now(),
	}))
	otherConvID, err := wcs.GetTopicConversationIDInProject(ctx, other.ID, otherTopic)
	require.NoError(t, err)
	require.NotEmpty(t, otherConvID)
	partsBefore, err := s.ListParticipants(ctx, otherConvID)
	require.NoError(t, err)
	before := countProjectConversations(t, s, project.ID)

	send := func(threadID string) refAnswer {
		rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
			Recipient: "user:" + user.Email,
			Msg:       "to a thread",
			ThreadID:  threadID,
			Channel:   "web",
		})
		return refAnswer{status: rr.Code, body: strings.ReplaceAll(rr.Body.String(), threadID, "<thread>")}
	}
	missing := send(tid("outbound-ref-unknown-thread"))
	require.Equal(t, http.StatusUnprocessableEntity, missing.status, missing.body)
	requireSameAnswer(t, missing, send(otherTopic))

	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "no conversation row is created")
	partsAfter, err := s.ListParticipants(ctx, otherConvID)
	require.NoError(t, err)
	assert.Equal(t, len(partsBefore), len(partsAfter), "no participant row is added to the other project's conversation")
	assertOnlyControlMessage(t, srv, s, project, agent, user)
}

func seedGroupConversation(t *testing.T, s store.Store, projectID, name string) string {
	t.Helper()
	pid := projectID
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "group", Surface: "native", ExternalRef: "group:" + projectID + ":" + name,
		ProjectID: &pid, DriftState: "active",
	})
	require.NoError(t, err)
	return conv.ID
}

func participantCount(t *testing.T, s store.Store, convID string) int {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), convID)
	require.NoError(t, err)
	return len(parts)
}

func postMessageWithConv(t *testing.T, srv *Server, sender Identity, target *store.Agent, convID string) refAnswer {
	t.Helper()
	body, err := json.Marshal(MessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "x:" + sender.ID(), SenderID: sender.ID(),
		Recipient: "agent:" + target.Slug, RecipientID: target.ID,
		Msg: "into a group", Type: messages.TypeInstruction, ConversationID: convID,
	}})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(contextWithIdentity(r.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, r, target.ID)
	return refAnswer{status: rr.Code, body: rr.Body.String()}
}

func TestAgentMessage_GroupConversationRequiresSenderReadAccess(t *testing.T) {
	f := acceptanceSetup(t)
	groupB := seedGroupConversation(t, f.store, f.projectB, "general")
	before := participantCount(t, f.store, groupB)

	// An agent of project A whose message project B's inbound policy admits.
	senderA := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	missing := postMessageWithConv(t, f.srv, senderA, f.hubAgentB, tid("group-ref-unknown-conv"))
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	requireSameAnswer(t, missing, postMessageWithConv(t, f.srv, senderA, f.hubAgentB, groupB))
	assert.Equal(t, before, participantCount(t, f.store, groupB), "no participant row is written")

	// A user with no role in project B gets the same answer.
	userA := NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, f.ownerA.Role, string(ClientTypeWeb))
	missingForUser := postMessageWithConv(t, f.srv, userA, f.hubAgentB, tid("group-ref-unknown-conv"))
	require.Equal(t, http.StatusBadRequest, missingForUser.status, missingForUser.body)
	requireSameAnswer(t, missingForUser, postMessageWithConv(t, f.srv, userA, f.hubAgentB, groupB))
	assert.Equal(t, before, participantCount(t, f.store, groupB), "no participant row is written")

	// Project B's owner can post into it.
	userB := NewAuthenticatedUser(f.ownerB.ID, f.ownerB.Email, f.ownerB.DisplayName, f.ownerB.Role, string(ClientTypeWeb))
	ok := postMessageWithConv(t, f.srv, userB, f.hubAgentB, groupB)
	require.Equal(t, http.StatusOK, ok.status, ok.body)
	assert.Greater(t, participantCount(t, f.store, groupB), before)
}

func TestAgentOutbound_GroupConversationOfOtherProjectMatchesMissing(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	other := &store.Project{ID: tid("group-out-other-project"), Name: "Other", Slug: "group-out-other",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, other))
	groupOther := seedGroupConversation(t, s, other.ID, "general")
	before := participantCount(t, s, groupOther)

	send := func(convID string) refAnswer {
		rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "into a group", convID)
		return refAnswer{status: rr.Code, body: rr.Body.String()}
	}
	missing := send(tid("group-out-unknown-conv"))
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	requireSameAnswer(t, missing, send(groupOther))
	assert.Equal(t, before, participantCount(t, s, groupOther), "no participant row is written")
}
