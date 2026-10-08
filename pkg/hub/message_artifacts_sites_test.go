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

// Behavioural tests of artifact references at each admitting site
// (ptone/scion#3222, ptone/scion#3224): what is dispatched, recorded and
// returned, beyond the source-shape guards in
// message_artifacts_coverage_test.go.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// artifactSiteFixture is a parity hub with artifacts on, the hub-rendered
// envelope on, and three artifacts: one owned by the project owner (a
// user), one owned by the sending agent, and one in another project that
// neither can read.
type artifactSiteFixture struct {
	srv        *Server
	s          store.Store
	st         artifacts.Store
	project    *store.Project
	owner      *store.User
	sender     *store.Agent
	target     *store.Agent
	dispatcher *recordingDispatcher
	userOwned  string
	agentOwned string
	unreadable string
}

func newArtifactSiteFixture(t *testing.T) *artifactSiteFixture {
	t.Helper()
	srv, s, project, sender, target, _, dispatcher, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	enableOffload(t, srv, 0, true) // envelope switch on, offload off
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	return &artifactSiteFixture{
		srv: srv, s: s, st: st, project: project, owner: owner, sender: sender, target: target, dispatcher: dispatcher,
		userOwned:  seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindUser, owner.ID, "Owner notes"),
		agentOwned: seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Agent report"),
		unreadable: seedMessageArtifact(t, st, tid("msgart-site-other"), artifacts.PrincipalKindUser, tid("msgart-site-stranger"), "Secret title"),
	}
}

func (f *artifactSiteFixture) ownerIdentity() *AuthenticatedUser {
	return NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, "member", "web")
}

func (f *artifactSiteFixture) recorded(t *testing.T, msgID string) []artifacts.MessageRef {
	t.Helper()
	got, err := f.st.ListMessageRefs(context.Background(), []string{msgID})
	require.NoError(t, err)
	return got[msgID]
}

// The non-agent-sender branch of handleAgentMessage: a user sends to an
// agent.
func TestMessageArtifactsSite_HandleAgentMessageUserSender(t *testing.T) {
	f := newArtifactSiteFixture(t)

	sm := &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type: messages.TypeInstruction, Sender: "user:" + f.owner.Email, SenderID: f.owner.ID,
		Recipient: "agent:" + f.target.Slug, RecipientID: f.target.ID, Msg: "have a look",
		Metadata: map[string]string{artifacts.MessageMetadataKey: refsValue(
			artifacts.MessageRef{ArtifactID: f.userOwned}, artifacts.MessageRef{ArtifactID: f.unreadable})},
	}
	body, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents/"+f.target.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(requestAuthCtx(req.Context(), f.ownerIdentity()))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.target.ID)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, artifactRefsWarning(1), resp.ArtifactWarning)
	require.NotEmpty(t, resp.MessageID)

	calls := dispatchesTo(f.dispatcher, f.target.ID)
	require.Len(t, calls, 1)
	want := []artifacts.MessageRef{{ArtifactID: f.userOwned}}
	assert.Equal(t, refsValue(want...), calls[0].StructuredMessage.Metadata[artifacts.MessageMetadataKey])
	assert.Contains(t, calls[0].StructuredMessage.DeliveryText, "Artifact: current - scion artifact get scion://artifact/"+f.userOwned)
	assert.NotContains(t, calls[0].StructuredMessage.DeliveryText, f.unreadable)
	assert.Equal(t, want, f.recorded(t, resp.MessageID))
}

func outboundToOwner(t *testing.T, f *artifactSiteFixture, refs ...artifacts.MessageRef) map[string]any {
	t.Helper()
	body, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.owner.Email,
		Msg:       "report attached",
		Type:      "instruction",
		Metadata:  map[string]string{artifacts.MessageMetadataKey: refsValue(refs...)},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.sender.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(requestAuthCtx(req.Context(), tokenBackedSender(t, f.s, f.sender)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.sender.ID)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

// handleAgentOutboundMessage to a user, web-chat direct branch (no broker
// proxy): the handler persists the row and records the refs itself.
func TestMessageArtifactsSite_OutboundToUserDirect(t *testing.T) {
	f := newArtifactSiteFixture(t)
	if bp := f.srv.GetMessageBrokerProxy(); bp != nil {
		bp.Stop()
	}
	f.srv.SetMessageBrokerProxy(nil)

	resp := outboundToOwner(t, f, artifacts.MessageRef{ArtifactID: f.agentOwned, Seq: 1}, artifacts.MessageRef{ArtifactID: f.unreadable})
	assert.Equal(t, artifactRefsWarning(1), resp["artifact_warning"])
	msgID, _ := resp["message_id"].(string)
	require.NotEmpty(t, msgID)
	assert.Equal(t, []artifacts.MessageRef{{ArtifactID: f.agentOwned, Seq: 1}}, f.recorded(t, msgID))
}

// handleAgentOutboundMessage to a user, broker branch: deliverToUser
// persists the row and records the refs because the admission step set the
// flag.
func TestMessageArtifactsSite_OutboundToUserBroker(t *testing.T) {
	f := newArtifactSiteFixture(t)
	wireWebBrokerForMentionTests(t, f.srv, f.s, f.project.ID, f.dispatcher)
	f.srv.GetMessageBrokerProxy().recordArtifactRefs = f.srv.recordMessageArtifacts

	resp := outboundToOwner(t, f, artifacts.MessageRef{ArtifactID: f.agentOwned}, artifacts.MessageRef{ArtifactID: f.unreadable})
	assert.Equal(t, artifactRefsWarning(1), resp["artifact_warning"])

	var rowID string
	require.Eventually(t, func() bool {
		rows, err := f.s.ListMessages(context.Background(), store.MessageFilter{SenderID: f.sender.ID, RecipientID: f.owner.ID}, store.ListOptions{Limit: 10})
		if err != nil || len(rows.Items) != 1 {
			return false
		}
		rowID = rows.Items[0].ID
		return len(f.recorded(t, rowID)) == 1
	}, 5*time.Second, 20*time.Millisecond, "deliverToUser must persist the row and record the admitted ref")
	assert.Equal(t, []artifacts.MessageRef{{ArtifactID: f.agentOwned}}, f.recorded(t, rowID))
}

// deliverToUser records refs only behind ArtifactRefsAdmitted. A message
// that carries the key without the flag (from any other publisher) is
// persisted without refs, and the shared message is not modified.
func TestMessageArtifactsSite_DeliverToUserRequiresAdmittedFlag(t *testing.T) {
	f := newArtifactSiteFixture(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(nil, f.s, events, func() AgentDispatcher { return f.dispatcher }, slog.Default())
	var mu sync.Mutex
	var recordedFor []string
	proxy.recordArtifactRefs = func(_ context.Context, messageID string, refs []artifacts.MessageRef) {
		mu.Lock()
		defer mu.Unlock()
		recordedFor = append(recordedFor, messageID)
	}

	value := refsValue(artifacts.MessageRef{ArtifactID: f.unreadable})
	newMsg := func(flag bool) *messages.StructuredMessage {
		return &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type: messages.TypeInstruction, Sender: "agent:" + f.sender.Slug, SenderID: f.sender.ID,
			Recipient: "user:" + f.owner.Email, RecipientID: f.owner.ID, Msg: "hello",
			Metadata:             map[string]string{"keep": "me", artifacts.MessageMetadataKey: value},
			ArtifactRefsAdmitted: flag,
		}
	}

	forged := newMsg(false)
	proxy.deliverToUser(context.Background(), f.project.ID, "topic", forged)
	mu.Lock()
	assert.Empty(t, recordedFor, "a key without the admitted flag must never be recorded")
	mu.Unlock()
	assert.Equal(t, value, forged.Metadata[artifacts.MessageMetadataKey], "the shared message must not be modified")
	assert.Equal(t, "me", forged.Metadata["keep"])

	admitted := newMsg(true)
	proxy.deliverToUser(context.Background(), f.project.ID, "topic", admitted)
	mu.Lock()
	assert.Len(t, recordedFor, 1, "with the flag set the refs are recorded")
	mu.Unlock()
}

func newChatV2WebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	if srv.webChatStore != nil {
		return srv.webChatStore
	}
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// chat v2 sendAgentRouted: the primary gets the admitted refs; the mention
// copy carries none; the send response returns the sender's views and the
// warning; the refs are recorded. Then the history route returns them for
// the viewer, and not for a soft-deleted message.
func TestMessageArtifactsSite_ChatV2SendAndHistory(t *testing.T) {
	f := newArtifactSiteFixture(t)
	wcs := newChatV2WebChatStore(t, f.srv, f.s)
	key, err := messages.DMConversationKey("agent", f.target.ID, "user", f.owner.ID)
	require.NoError(t, err)
	user := f.ownerIdentity()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(requestAuthCtx(req.Context(), user))
	rr := httptest.NewRecorder()
	clientMD := map[string]string{artifacts.MessageMetadataKey: refsValue(
		artifacts.MessageRef{ArtifactID: f.userOwned}, artifacts.MessageRef{ArtifactID: f.unreadable})}
	msgID := f.srv.sendAgentRouted(rr, req, key, f.project.ID, user, "notes @"+f.sender.Slug, f.owner.Email,
		[]*store.Agent{f.target, f.sender}, []string{f.sender.Slug}, nil, nil, time.Now(), "", clientMD, chatSendOptions{})
	require.NotEmpty(t, msgID, "%d: %s", rr.Code, rr.Body.String())

	var resp chatMessageResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, artifactRefsWarning(1), resp.ArtifactWarning)
	require.Len(t, resp.Artifacts, 1)
	assert.True(t, resp.Artifacts[0].Available)
	assert.Equal(t, "Owner notes", resp.Artifacts[0].Title)
	assert.NotContains(t, rr.Body.String(), "Secret title")

	want := refsValue(artifacts.MessageRef{ArtifactID: f.userOwned})
	primary := dispatchesTo(f.dispatcher, f.target.ID)
	require.Len(t, primary, 1)
	assert.Equal(t, want, primary[0].StructuredMessage.Metadata[artifacts.MessageMetadataKey])
	assert.Contains(t, primary[0].StructuredMessage.DeliveryText, "Artifact: current - scion artifact get scion://artifact/"+f.userOwned)
	mention := dispatchesTo(f.dispatcher, f.sender.ID)
	require.Len(t, mention, 1, "the mentioned agent gets a copy")
	assert.NotContains(t, mention[0].StructuredMessage.Metadata, artifacts.MessageMetadataKey, "mention copies carry no refs")
	assert.Equal(t, []artifacts.MessageRef{{ArtifactID: f.userOwned}}, f.recorded(t, msgID))

	history := func() chatHistoryResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
		req = req.WithContext(requestAuthCtx(req.Context(), user))
		rr := httptest.NewRecorder()
		f.srv.handleConversationHistory(rr, req, key)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var h chatHistoryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &h))
		return h
	}
	h := history()
	require.Len(t, h.MessageArtifacts[msgID], 1)
	assert.True(t, h.MessageArtifacts[msgID][0].Available)
	assert.Equal(t, "Owner notes", h.MessageArtifacts[msgID][0].Title)

	require.NoError(t, wcs.SetMessageDeleted(context.Background(), msgID, time.Now()))
	h = history()
	assert.NotContains(t, h.MessageArtifacts, msgID, "a soft-deleted message shows no refs")
}
