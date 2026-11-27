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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestConversationMessageAttachment_GetMessagePayload(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	// Seed a direct conversation between user ua and agent aa.
	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))

	// Delivered message with an artifact-backed attachment AND a legacy AttachmentStore attachment.
	msg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "see attached",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, msg))

	const body = "delivered payload bytes"
	sum := sha256.Sum256([]byte(body))
	wantSHA := hex.EncodeToString(sum[:])

	artAttID := f.uploadAs(t, f.ua, f.projA.ID, "report.txt", body)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, artAttID, artifacts.PrincipalKindUser, f.ua.ID, msg.ID, f.projA.ID))

	// Also link a legacy AttachmentStore attachment to verify D5 excludes it.
	legacyAtt := AttachmentMeta{
		ID:         uuid.NewString(),
		Filename:   "legacy.txt",
		MimeType:   "text/plain",
		Size:       6,
		UploadedBy: f.ua.ID,
		ProjectID:  f.projA.ID,
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, f.srv.webChatStore.CreateAttachment(ctx, legacyAtt))
	require.NoError(t, f.srv.webChatStore.LinkAttachmentToMessage(ctx, msg.ID, legacyAtt.ID))

	// Deferred message with an artifact-backed attachment: should populate BOTH Attachments and DeferredAttachments.
	defMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "deferred msg",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDeferred,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, defMsg))
	defAttID := f.uploadAs(t, f.ua, f.projA.ID, "My Report.txt", "deferred bytes")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, defAttID, artifacts.PrincipalKindUser, f.ua.ID, defMsg.ID, f.projA.ID))

	// Read delivered message as agent aa.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+msg.ID, nil)
	req = req.WithContext(storedAgentContext(f.aa))
	rec := httptest.NewRecorder()
	f.srv.handleConversationRoutes(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got store.Message
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Attachments, 1, "only artifact-backed attachment should be listed (D5)")
	assert.Equal(t, artAttID, got.Attachments[0].ID)
	assert.Equal(t, "report.txt", got.Attachments[0].Name)
	assert.Equal(t, int64(len(body)), got.Attachments[0].Size)
	assert.Equal(t, "text/plain", got.Attachments[0].ContentType)
	assert.Equal(t, wantSHA, got.Attachments[0].SHA256)
	assert.Equal(t, artifacts.FormatAttachmentRef(artAttID), got.Attachments[0].Ref)
	assert.Empty(t, got.DeferredAttachments)

	// Read deferred message as user ua: both Attachments and DeferredAttachments populated.
	reqDef := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+defMsg.ID, nil)
	reqDef = reqDef.WithContext(contextWithIdentity(context.Background(), NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))))
	recDef := httptest.NewRecorder()
	f.srv.handleConversationRoutes(recDef, reqDef)
	require.Equal(t, http.StatusOK, recDef.Code, recDef.Body.String())

	var gotDef store.Message
	require.NoError(t, json.Unmarshal(recDef.Body.Bytes(), &gotDef))
	require.Len(t, gotDef.Attachments, 1)
	assert.Equal(t, defAttID, gotDef.Attachments[0].ID)
	assert.Equal(t, "My Report.txt", gotDef.Attachments[0].Name)
	require.Len(t, gotDef.DeferredAttachments, 1)
	assert.Equal(t, "My_Report.txt", gotDef.DeferredAttachments[0].Name)
	assert.Equal(t, int64(len("deferred bytes")), gotDef.DeferredAttachments[0].Size)
}

func TestConversationMessageAttachment_DownloadAuthz(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	// DM between user ua and agent aa (project A).
	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	dmMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "dm file",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, dmMsg))
	const dmPayload = "dm attachment secret content"
	dmAttID := f.uploadAs(t, f.ua, f.projA.ID, "dm.txt", dmPayload)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, dmAttID, artifacts.PrincipalKindUser, f.ua.ID, dmMsg.ID, f.projA.ID))

	// Link a legacy AttachmentStore row to dmMsg to test D5 404.
	legacyAtt := AttachmentMeta{
		ID:         uuid.NewString(),
		Filename:   "legacy.bin",
		MimeType:   "application/octet-stream",
		Size:       4,
		UploadedBy: f.ua.ID,
		ProjectID:  f.projA.ID,
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, f.srv.webChatStore.CreateAttachment(ctx, legacyAtt))
	require.NoError(t, f.srv.webChatStore.LinkAttachmentToMessage(ctx, dmMsg.ID, legacyAtt.ID))

	// A second conversation + message + attachment to test cross-conversation and unlinked-attachment refusals.
	otherConvID := seedGroupConversation(t, f.st, f.projA.ID, "other-room")
	otherMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "other room msg",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: otherConvID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, otherMsg))
	otherAttID := f.uploadAs(t, f.ua, f.projA.ID, "other.txt", "other room bytes")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, otherAttID, artifacts.PrincipalKindUser, f.ua.ID, otherMsg.ID, f.projA.ID))

	// Cross-project DM between agent aa (projA) and agent ab (projB).
	ab := &store.Agent{
		ID:        uuid.NewString(),
		ProjectID: f.projB.ID,
		Name:      "ab",
		Slug:      "ab",
		Phase:     "running",
		OwnerID:   f.ub.ID,
		CreatedBy: f.ub.ID,
	}
	require.NoError(t, f.st.CreateAgent(ctx, ab))
	xpDM := seedDirectConversation(t, f.st, dmKeyFor(t, "agent", f.aa.ID, "agent", ab.ID))
	xpMsg := &store.Message{
		ID:                 uuid.NewString(),
		ProjectID:          f.projA.ID,
		Sender:             "agent:aa",
		SenderID:           f.aa.ID,
		Recipient:          "agent:ab",
		RecipientID:        ab.ID,
		SenderProjectID:    &f.projA.ID,
		RecipientProjectID: &f.projB.ID,
		Msg:                "cross-project dm",
		Type:               "instruction",
		Channel:            "web",
		ConversationID:     xpDM.ID,
		ThreadID:           xpDM.ExternalRef,
		CreatedAt:          time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, xpMsg))
	xpAttID := f.uploadAs(t, f.ua, "", "xp.txt", "cross-project bytes")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, xpAttID, artifacts.PrincipalKindUser, f.ua.ID, xpMsg.ID, ""))

	userCtx := func(u *store.User) context.Context {
		return contextWithIdentity(context.Background(), NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)))
	}
	doDownload := func(callCtx context.Context, convID, msgID, attID, query string) *httptest.ResponseRecorder {
		u := "/api/v1/conversations/" + convID + "/messages/" + msgID + "/attachments/" + attID
		if query != "" {
			u += "?" + query
		}
		req := httptest.NewRequest(http.MethodGet, u, nil).WithContext(callCtx)
		rec := httptest.NewRecorder()
		f.srv.handleConversationRoutes(rec, req)
		return rec
	}

	// 1. User session reads DM attachment (including ?stream=1).
	rec := doDownload(userCtx(f.ua), dm.ID, dmMsg.ID, dmAttID, "stream=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, dmPayload, rec.Body.String())

	// 2. Scoped UAT with inbox:read + agent:read succeeds.
	uatOK := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "inbox:read", "agent:read")
	rec = doDownload(contextWithIdentity(context.Background(), uatOK), dm.ID, dmMsg.ID, dmAttID, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, dmPayload, rec.Body.String())

	// 3. Scoped UAT without inbox:read is denied.
	uatNoInbox := selfToken(t, f.ua.ID, projectBoundary(f.projA.ID), "agent:read")
	rec = doDownload(contextWithIdentity(context.Background(), uatNoInbox), dm.ID, dmMsg.ID, dmAttID, "")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// 4. Agent with real SCION_AUTH_TOKEN (without project:artifact:read) succeeds!
	agentToken, err := f.srv.GenerateAgentToken(
		f.aa.ID, f.projA.ID, nil, "",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentNotify},
	)
	require.NoError(t, err)
	agentReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+dmMsg.ID+"/attachments/"+dmAttID, nil)
	agentReq.Header.Set("Authorization", "Bearer "+agentToken)
	agentRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(agentRec, agentReq)
	require.Equal(t, http.StatusOK, agentRec.Code, agentRec.Body.String())
	assert.Equal(t, dmPayload, agentRec.Body.String())

	// 5. Cross-project DM with cross_project_messaging_enabled OFF is refused for agent.
	f.srv.operationalSettings.Store(nil)
	rec = doDownload(storedAgentContext(f.aa), xpDM.ID, xpMsg.ID, xpAttID, "")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// 6. Identical 404 body for all post-step-2 refusals / not-found cases:
	//    (a) unknown attachment UUID
	//    (b) attachment linked to a different message
	//    (c) legacy AttachmentStore attachment id linked to this message (D5)
	recUnknown := doDownload(userCtx(f.ua), dm.ID, dmMsg.ID, uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, recUnknown.Code)

	recWrongMsg := doDownload(userCtx(f.ua), dm.ID, dmMsg.ID, otherAttID, "")
	require.Equal(t, http.StatusNotFound, recWrongMsg.Code)
	assert.Equal(t, recUnknown.Body.String(), recWrongMsg.Body.String(), "unlinked attachment must return identical 404 body")

	recLegacy := doDownload(userCtx(f.ua), dm.ID, dmMsg.ID, legacyAtt.ID, "")
	require.Equal(t, http.StatusNotFound, recLegacy.Code)
	assert.Equal(t, recUnknown.Body.String(), recLegacy.Body.String(), "legacy AttachmentStore id must return identical 404 body (D5)")

	// 7. Message in another conversation returns 404.
	recWrongConv := doDownload(userCtx(f.ua), otherConvID, dmMsg.ID, dmAttID, "")
	assert.Equal(t, http.StatusNotFound, recWrongConv.Code)

	// 8. Non-GET method returns 405.
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+dm.ID+"/messages/"+dmMsg.ID+"/attachments/"+dmAttID, nil).WithContext(userCtx(f.ua))
	postRec := httptest.NewRecorder()
	f.srv.handleConversationRoutes(postRec, postReq)
	assert.Equal(t, http.StatusMethodNotAllowed, postRec.Code)
}

// TestConversationMessageAttachment_AgentE2E validates Phase 1 end to end:
// an agent authenticated with a real SCION_AUTH_TOKEN (lacking
// project:artifact:read) calls hubclient Conversations().GetMessage and
// Conversations().DownloadAttachment against a live httptest hub, verifying
// metadata, sha256, and downloaded bytes.
func TestConversationMessageAttachment_AgentE2E(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	msg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "please inspect the attached spec",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, msg))

	const payload = "# Phase 1 E2E Spec\nverified end-to-end bytes\n"
	sum := sha256.Sum256([]byte(payload))
	wantSHA := hex.EncodeToString(sum[:])

	attID := f.uploadAs(t, f.ua, f.projA.ID, "spec.md", payload)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, attID, artifacts.PrincipalKindUser, f.ua.ID, msg.ID, f.projA.ID))

	// Mint a real SCION_AUTH_TOKEN without project:artifact:read.
	agentToken, err := f.srv.GenerateAgentToken(
		f.aa.ID, f.projA.ID, nil, "",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentNotify},
	)
	require.NoError(t, err)

	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()

	client, err := hubclient.New(ts.URL, hubclient.WithAgentToken(agentToken))
	require.NoError(t, err)

	gotMsg, err := client.Conversations().GetMessage(ctx, dm.ID, msg.ID)
	require.NoError(t, err)
	require.Len(t, gotMsg.Attachments, 1)
	att := gotMsg.Attachments[0]
	assert.Equal(t, attID, att.ID)
	assert.Equal(t, "spec.md", att.Name)
	assert.Equal(t, int64(len(payload)), att.Size)
	assert.Equal(t, wantSHA, att.SHA256)
	assert.Equal(t, artifacts.FormatAttachmentRef(attID), att.Ref)

	rc, err := client.Conversations().DownloadAttachment(ctx, dm.ID, msg.ID, att.ID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	downloaded, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, payload, string(downloaded))
	gotSum := sha256.Sum256(downloaded)
	assert.Equal(t, att.SHA256, hex.EncodeToString(gotSum[:]))
}

// TestConversationMessageAttachment_ListMessagesPayload validates Phase 2
// (ptone/scion#9579, #9607): GET /api/v1/conversations/{id}/messages populates
// attachments[] and deferredAttachments in one pass, enforces MayReadAttachments,
// and omits legacy AttachmentStore attachments (D5).
func TestConversationMessageAttachment_ListMessagesPayload(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	t0 := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)

	// 1. Plain message with no attachments.
	plainMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "plain text",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      t0,
	}
	require.NoError(t, f.st.CreateMessage(ctx, plainMsg))

	// 2. Delivered message with an artifact-backed attachment AND a legacy AttachmentStore attachment.
	deliveredMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "delivered with attachment",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      t0.Add(time.Minute),
	}
	require.NoError(t, f.st.CreateMessage(ctx, deliveredMsg))

	const deliveredBody = "list delivered payload"
	delSum := sha256.Sum256([]byte(deliveredBody))
	wantDeliveredSHA := hex.EncodeToString(delSum[:])

	artAttID := f.uploadAs(t, f.ua, f.projA.ID, "summary.txt", deliveredBody)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, artAttID, artifacts.PrincipalKindUser, f.ua.ID, deliveredMsg.ID, f.projA.ID))

	// Link a legacy AttachmentStore row to verify D5 omits it on ListMessages too.
	legacyAtt := AttachmentMeta{
		ID:         uuid.NewString(),
		Filename:   "legacy-list.txt",
		MimeType:   "text/plain",
		Size:       11,
		UploadedBy: f.ua.ID,
		ProjectID:  f.projA.ID,
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, f.srv.webChatStore.CreateAttachment(ctx, legacyAtt))
	require.NoError(t, f.srv.webChatStore.LinkAttachmentToMessage(ctx, deliveredMsg.ID, legacyAtt.ID))

	// 3. Deferred message with an artifact-backed attachment: must carry BOTH attachments and deferredAttachments.
	deferredMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "deferred with attachment",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDeferred,
		CreatedAt:      t0.Add(2 * time.Minute),
	}
	require.NoError(t, f.st.CreateMessage(ctx, deferredMsg))

	const deferredBody = "deferred list payload"
	defSum := sha256.Sum256([]byte(deferredBody))
	wantDeferredSHA := hex.EncodeToString(defSum[:])

	defAttID := f.uploadAs(t, f.ua, f.projA.ID, "My Deferred File.txt", deferredBody)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, defAttID, artifacts.PrincipalKindUser, f.ua.ID, deferredMsg.ID, f.projA.ID))

	// Read via GET /api/v1/conversations/{id}/messages as agent aa using a real
	// SCION_AUTH_TOKEN lacking project:artifact:read.
	agentToken, err := f.srv.GenerateAgentToken(
		f.aa.ID, f.projA.ID, nil, "",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentNotify},
	)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var listResp struct {
		Items []store.Message `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	byID := make(map[string]store.Message, len(listResp.Items))
	for _, item := range listResp.Items {
		byID[item.ID] = item
	}
	require.Len(t, byID, 3)

	// Plain message has neither field.
	gotPlain := byID[plainMsg.ID]
	assert.Empty(t, gotPlain.Attachments)
	assert.Empty(t, gotPlain.DeferredAttachments)

	// Delivered message has artifact-backed attachment only (legacy omitted) and no deferredAttachments.
	gotDelivered := byID[deliveredMsg.ID]
	require.Len(t, gotDelivered.Attachments, 1, "only artifact-backed attachment should be listed (D5)")
	assert.Equal(t, artAttID, gotDelivered.Attachments[0].ID)
	assert.Equal(t, "summary.txt", gotDelivered.Attachments[0].Name)
	assert.Equal(t, int64(len(deliveredBody)), gotDelivered.Attachments[0].Size)
	assert.Equal(t, "text/plain", gotDelivered.Attachments[0].ContentType)
	assert.Equal(t, wantDeliveredSHA, gotDelivered.Attachments[0].SHA256)
	assert.Equal(t, artifacts.FormatAttachmentRef(artAttID), gotDelivered.Attachments[0].Ref)
	assert.Empty(t, gotDelivered.DeferredAttachments)

	// Deferred message carries BOTH attachments (original name) and deferredAttachments (reduced name).
	gotDeferred := byID[deferredMsg.ID]
	require.Len(t, gotDeferred.Attachments, 1)
	assert.Equal(t, defAttID, gotDeferred.Attachments[0].ID)
	assert.Equal(t, "My Deferred File.txt", gotDeferred.Attachments[0].Name)
	assert.Equal(t, int64(len(deferredBody)), gotDeferred.Attachments[0].Size)
	assert.Equal(t, "text/plain", gotDeferred.Attachments[0].ContentType)
	assert.Equal(t, wantDeferredSHA, gotDeferred.Attachments[0].SHA256)
	assert.Equal(t, artifacts.FormatAttachmentRef(defAttID), gotDeferred.Attachments[0].Ref)
	require.Len(t, gotDeferred.DeferredAttachments, 1)
	assert.Equal(t, "My_Deferred_File.txt", gotDeferred.DeferredAttachments[0].Name)
	assert.Equal(t, int64(len(deferredBody)), gotDeferred.DeferredAttachments[0].Size)

	// MayReadAttachments gating: a non-reader (outsider agent in projB) gets
	// neither Attachments nor DeferredAttachments from noteMessageAttachments.
	outsider := &store.Agent{
		ID:        uuid.NewString(),
		ProjectID: f.projB.ID,
		Name:      "outsider",
		Slug:      "outsider",
		Phase:     "running",
		OwnerID:   f.ub.ID,
		CreatedBy: f.ub.ID,
	}
	require.NoError(t, f.st.CreateAgent(ctx, outsider))
	items := []store.Message{*deliveredMsg, *deferredMsg}
	f.srv.noteMessageAttachments(storedAgentContext(outsider), items)
	assert.Empty(t, items[0].Attachments, "non-reader must not receive attachments")
	assert.Empty(t, items[1].Attachments, "non-reader must not receive attachments on deferred message")
	assert.Empty(t, items[1].DeferredAttachments, "non-reader must not receive deferredAttachments")
}

func TestConversationMessageAttachment_PublishedAs(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	msg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "check published ref",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, msg))

	const body = "published attachment body"
	sum := sha256.Sum256([]byte(body))
	bodySHA := hex.EncodeToString(sum[:])
	attID := f.uploadAs(t, f.ua, f.projA.ID, "spec.md", body)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, attID, artifacts.PrincipalKindUser, f.ua.ID, msg.ID, f.projA.ID))

	// Seed two published artifacts referencing attID:
	// 1. readableArt in projA (ua can read it)
	// 2. secretArt in projB owned by ub (ua cannot read it)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedPublished := func(scopeRef, ownerRef string, createdAt time.Time) string {
		artID := uuid.NewString()
		verID := uuid.NewString()
		a := &artifacts.Artifact{
			ID:         artID,
			ScopeKind:  artifacts.ScopeKindProject,
			ScopeRef:   scopeRef,
			OwnerKind:  artifacts.PrincipalKindUser,
			OwnerRef:   ownerRef,
			Title:      "spec.md",
			CurrentSeq: 1,
			CreatedAt:  createdAt,
			UpdatedAt:  createdAt,
		}
		v := &artifacts.Version{
			ID:                 verID,
			ArtifactID:         artID,
			Seq:                1,
			Kind:               artifacts.VersionKindPublish,
			EntryPath:          "spec.md",
			TotalBytes:         int64(len(body)),
			FileCount:          1,
			CreatedByKind:      artifacts.PrincipalKindUser,
			CreatedByRef:       ownerRef,
			CreatedAt:          createdAt,
			State:              artifacts.VersionStateReady,
			SourceAttachmentID: attID,
			SourceMessageID:    msg.ID,
		}
		files := []artifacts.File{{VersionID: verID, Path: "spec.md", Size: int64(len(body)), SHA256: bodySHA, MediaType: "text/markdown"}}
		require.NoError(t, f.srv.ArtifactStore().CreatePublished(ctx, a, v, files, nil))
		return artID
	}

	readableArtID := seedPublished(f.projA.ID, f.ua.ID, now)
	_ = seedPublished(f.projB.ID, f.ub.ID, now.Add(time.Second))

	uaCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb)))

	fetchGetMessage := func() store.Message {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+msg.ID, nil).WithContext(uaCtx)
		rec := httptest.NewRecorder()
		f.srv.handleConversationRoutes(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.Message
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	fetchListMessages := func() []store.Message {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages", nil).WithContext(uaCtx)
		rec := httptest.NewRecorder()
		f.srv.handleConversationRoutes(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp struct {
			Items []store.Message `json:"items"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Items
	}

	// 1. With hub.artifacts enabled: publishedAs contains ONLY readableArtID (secretArt omitted).
	setServerExperimentDefault(t, f.srv, experiments.Artifacts, true)
	require.True(t, f.srv.experimentEnabled(experiments.Artifacts))

	gotMsg := fetchGetMessage()
	require.Len(t, gotMsg.Attachments, 1)
	assert.Equal(t, []string{artifacts.FormatRef(readableArtID, 0)}, gotMsg.Attachments[0].PublishedAs)

	listed := fetchListMessages()
	require.Len(t, listed, 1)
	require.Len(t, listed[0].Attachments, 1)
	assert.Equal(t, []string{artifacts.FormatRef(readableArtID, 0)}, listed[0].Attachments[0].PublishedAs)

	// 2. With hub.artifacts disabled (hub.attachment_artifacts still ready): publishedAs is omitted.
	setServerExperimentDefault(t, f.srv, experiments.Artifacts, false)
	require.False(t, f.srv.experimentEnabled(experiments.Artifacts))

	gotDisabled := fetchGetMessage()
	require.Len(t, gotDisabled.Attachments, 1)
	assert.Empty(t, gotDisabled.Attachments[0].PublishedAs)

	listedDisabled := fetchListMessages()
	require.Len(t, listedDisabled, 1)
	require.Len(t, listedDisabled[0].Attachments, 1)
	assert.Empty(t, listedDisabled[0].Attachments[0].PublishedAs)
}

// TestConversationMessageAttachment_MayReadAttachmentsRefused pins the
// MayReadAttachments check inside artifacts.Service.ServeMessageAttachment
// (ptone/scion#9579): when authorizeConversationMessageRead admits a caller
// (here, a DM party agent) but MayReadAttachments refuses (the agent is held
// and therefore not in good standing), the download route must refuse with the
// same uniform 404 Attachment body as an unknown attachment ID.
func TestConversationMessageAttachment_MayReadAttachmentsRefused(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	ctx := context.Background()

	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	msg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      f.projA.ID,
		Sender:         "user:alice",
		SenderID:       f.ua.ID,
		Recipient:      "agent:aa",
		RecipientID:    f.aa.ID,
		Msg:            "standing-gated attachment",
		Type:           "instruction",
		Channel:        "web",
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.st.CreateMessage(ctx, msg))

	const payload = "standing-gated attachment bytes"
	attID := f.uploadAs(t, f.ua, f.projA.ID, "secret.txt", payload)
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, attID, artifacts.PrincipalKindUser, f.ua.ID, msg.ID, f.projA.ID))

	doDownload := func(att string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+msg.ID+"/attachments/"+att, nil)
		req = req.WithContext(storedAgentContext(f.aa))
		rec := httptest.NewRecorder()
		f.srv.handleConversationRoutes(rec, req)
		return rec
	}

	// Precondition: agent in good standing can download the attachment.
	require.NoError(t, f.srv.agentStanding(ctx, f.aa.ID), "precondition: aa is in standing")
	recOK := doDownload(attID)
	require.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())
	assert.Equal(t, payload, recOK.Body.String())

	// Reference 404 body for an unknown attachment ID.
	recUnknown := doDownload(uuid.NewString())
	require.Equal(t, http.StatusNotFound, recUnknown.Code)

	// Place an active hold on agent aa so agentStanding fails while
	// authorizeConversationMessageRead still admits the DM party.
	_, err := f.st.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           f.aa.ID,
		ProjectID:         f.projA.ID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   f.ua.ID,
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "system",
		ActorID:           "hub",
		CorrelationID:     "test-may-read-attachments",
	}})
	require.NoError(t, err)
	require.Error(t, f.srv.agentStanding(ctx, f.aa.ID), "held agent must fail agentStanding")

	// Confirm authorizeConversationMessageRead still admits the message read
	// (200 OK with attachments omitted by noteMessageAttachments), isolating
	// MayReadAttachments in ServeMessageAttachment as the download gate.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dm.ID+"/messages/"+msg.ID, nil)
	getReq = getReq.WithContext(storedAgentContext(f.aa))
	getRec := httptest.NewRecorder()
	f.srv.handleConversationRoutes(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var gotMsg store.Message
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &gotMsg))
	assert.Empty(t, gotMsg.Attachments)

	// Download must be refused by MayReadAttachments with the uniform 404 body.
	recRefused := doDownload(attID)
	require.Equal(t, http.StatusNotFound, recRefused.Code, "held agent download must return 404")
	assert.Equal(t, recUnknown.Body.String(), recRefused.Body.String(), "MayReadAttachments refusal must return identical 404 body")
}
