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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Live chat events and artifact references (ptone/scion#3758): a chat event
// carries the message's recorded references as metadata.artifacts (ids and
// pinned versions only), and only on the chat subjects, whose audience is
// the audience of web chat history. Clients resolve the references through
// history under their own credential.

// receiveEvent returns the next event on ch, failing after a second.
func receiveEvent(t *testing.T, ch <-chan Event, subject string) Event {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(time.Second):
		t.Fatalf("no event on %s", subject)
		return Event{}
	}
}

// eventArtifacts decodes an event's metadata.artifacts; "" when absent.
func eventArtifacts(t *testing.T, data []byte) string {
	t.Helper()
	var payload UserMessageEvent
	require.NoError(t, json.Unmarshal(data, &payload))
	if payload.Metadata == nil {
		return ""
	}
	return payload.Metadata.Artifacts
}

// TestPublishUserMessage_ArtifactRefsOnChatSubjectsOnly pins the subject
// set in both directions: references are on project.<id>.chat.message,
// user.<id>.chat.dm and user.<id>.chat.message, and on no other subject a
// user message is published to (withChatArtifactRefs).
func TestPublishUserMessage_ArtifactRefsOnChatSubjectsOnly(t *testing.T) {
	id := "5f1c2d3e-0000-4000-8000-0000000000aa"
	refs := []artifacts.MessageRef{{ArtifactID: id, Seq: 2}}
	want := artifacts.EncodeMessageRefs(refs)
	userID := "11111111-0000-4000-8000-000000000001"
	agentID := "22222222-0000-4000-8000-000000000002"

	type publish func(pub *ChannelEventPublisher, refs []artifacts.MessageRef)
	topic := func(pub *ChannelEventPublisher, refs []artifacts.MessageRef) {
		pub.PublishUserMessage(context.Background(), &store.Message{
			ID: "m-topic", ProjectID: "p1", Sender: "agent:coder", SenderID: agentID,
			Recipient: "user:alice", RecipientID: userID, AgentID: agentID, Msg: "see this",
			Channel: "web", ThreadID: "33333333-0000-4000-8000-000000000003", CreatedAt: time.Now(),
		}, nil, refs)
	}
	dmKey := "dm:agent:" + agentID + ":user:" + userID
	dm := func(pub *ChannelEventPublisher, refs []artifacts.MessageRef) {
		pub.PublishUserMessage(context.Background(), &store.Message{
			ID: "m-dm", ProjectID: "p1", Sender: "agent:coder", SenderID: agentID,
			Recipient: "user:alice", RecipientID: userID, AgentID: agentID, Msg: "see this",
			Channel: "web", ThreadID: dmKey, CreatedAt: time.Now(),
		}, nil, refs)
	}
	member := func(pub *ChannelEventPublisher, refs []artifacts.MessageRef) {
		pub.PublishChatMemberMessage(context.Background(), &store.Message{
			ID: "m-member", ProjectID: "p1", Sender: "agent:coder", SenderID: agentID,
			Recipient: "user:alice", RecipientID: userID, AgentID: agentID, Msg: "see this",
			Channel: "web", ThreadID: "33333333-0000-4000-8000-000000000003", CreatedAt: time.Now(),
		}, nil, refs, []string{"u2"})
	}

	for _, tc := range []struct {
		name    string
		publish publish
		carry   []string // subjects that carry the references
		omit    []string // subjects that get the message without them
	}{
		{
			name:    "topic thread",
			publish: topic,
			carry:   []string{"project.p1.chat.message"},
			omit:    []string{"user." + userID + ".message", "project.p1.user.message", "agent." + agentID + ".message"},
		},
		{
			name:    "dm",
			publish: dm,
			carry:   []string{"user." + userID + ".chat.dm", "user." + agentID + ".chat.dm"},
			omit:    []string{"user." + userID + ".message", "project.p1.user.message", "agent." + agentID + ".message"},
		},
		{
			name:    "member fan-out",
			publish: member,
			carry:   []string{"user.u2.chat.message"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, withRefs := range []bool{true, false} {
				pub := NewChannelEventPublisher()
				chans := map[string]<-chan Event{}
				for _, subj := range append(append([]string{}, tc.carry...), tc.omit...) {
					ch, unsub := pub.Subscribe(subj)
					t.Cleanup(unsub)
					chans[subj] = ch
				}
				var r []artifacts.MessageRef
				if withRefs {
					r = refs
				}
				tc.publish(pub, r)
				for _, subj := range tc.carry {
					evt := receiveEvent(t, chans[subj], subj)
					if withRefs {
						assert.Equal(t, want, eventArtifacts(t, evt.Data), subj)
					} else {
						assert.NotContains(t, string(evt.Data), `"metadata"`, subj)
					}
				}
				for _, subj := range tc.omit {
					evt := receiveEvent(t, chans[subj], subj)
					assert.NotContains(t, string(evt.Data), `"metadata"`, subj)
					assert.NotContains(t, string(evt.Data), id, subj)
				}
				pub.Close()
			}
		})
	}
}

// TestLiveChatArtifacts_ChatV2SendMatchesHistory: a web chat send publishes
// the recorded references on the chat subject and nowhere else, as ids and
// versions only. Resolved through history (the client's refresh), the
// reader sees the chip; for a viewer who cannot read the artifact, the
// history view is byte for byte the view the event's references give
// without resolving anything, so the event adds nothing history does not
// already show them, and unreadable stays the same as missing.
func TestLiveChatArtifacts_ChatV2SendMatchesHistory(t *testing.T) {
	f := newArtifactSiteFixture(t)
	newChatV2WebChatStore(t, f.srv, f.s)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.events = pub

	key, err := messages.DMConversationKey("agent", f.target.ID, "user", f.owner.ID)
	require.NoError(t, err)
	dmCh, unsubDM := pub.Subscribe("user." + f.owner.ID + ".chat.dm")
	t.Cleanup(unsubDM)
	agentCh, unsubAgent := pub.Subscribe("agent." + f.target.ID + ".message")
	t.Cleanup(unsubAgent)

	user := f.ownerIdentity()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", nil)
	req = req.WithContext(requestAuthCtx(req.Context(), user))
	rr := httptest.NewRecorder()
	clientMD := map[string]string{artifacts.MessageMetadataKey: refsValue(
		artifacts.MessageRef{ArtifactID: f.userOwned, Seq: 1}, artifacts.MessageRef{ArtifactID: f.unreadable})}
	msgID := writeChatSendOutcome(rr)(f.srv.sendAgentRouted(req.Context(), key, f.project.ID, user, "notes", f.owner.Email,
		[]*store.Agent{f.target}, nil, nil, nil, time.Now(), "", clientMD, chatSendOptions{}))
	require.NotEmpty(t, msgID, "%d: %s", rr.Code, rr.Body.String())

	// The chat subject carries exactly the recorded (admitted) references;
	// the agent subject carries none.
	recorded := f.recorded(t, msgID)
	require.Equal(t, []artifacts.MessageRef{{ArtifactID: f.userOwned, Seq: 1}}, recorded)
	dmEvt := receiveEvent(t, dmCh, "chat.dm")
	assert.Equal(t, artifacts.EncodeMessageRefs(recorded), eventArtifacts(t, dmEvt.Data))
	for _, leak := range []string{"Owner notes", "ownerRef", "ownerName", "title", "available"} {
		assert.NotContains(t, string(dmEvt.Data), leak)
	}
	agentEvt := receiveEvent(t, agentCh, "agent message")
	assert.NotContains(t, string(agentEvt.Data), f.userOwned)

	// The reader's refresh through history shows the chip.
	hreq := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages?limit=5", nil)
	hreq = hreq.WithContext(requestAuthCtx(hreq.Context(), user))
	hrr := httptest.NewRecorder()
	f.srv.handleConversationHistory(hrr, hreq, key)
	require.Equal(t, http.StatusOK, hrr.Code, hrr.Body.String())
	var h chatHistoryResponse
	require.NoError(t, json.Unmarshal(hrr.Body.Bytes(), &h))
	require.Len(t, h.MessageArtifacts[msgID], 1)
	assert.True(t, h.MessageArtifacts[msgID][0].Available)
	assert.Equal(t, "Owner notes", h.MessageArtifacts[msgID][0].Title)

	// A viewer who cannot read the artifact: history's view of the message
	// is byte for byte the unresolved view of the event's references.
	refs, dropped := artifacts.ParseMessageRefs(eventArtifacts(t, dmEvt.Data))
	require.Zero(t, dropped)
	unresolved := make([]chatArtifactRef, len(refs))
	for i, r := range refs {
		unresolved[i] = chatArtifactRef{RefView: artifacts.RefView{Ref: r.String(), ID: r.ArtifactID, Seq: r.Seq}}
	}
	stranger := requestAuthCtx(context.Background(),
		NewAuthenticatedUser(tid("live-stranger"), "stranger@test.example", "Stranger", "member", "web"))
	got, err := json.Marshal(f.srv.messageArtifactViews(stranger, []string{msgID})[msgID])
	require.NoError(t, err)
	wantJSON, err := json.Marshal(unresolved)
	require.NoError(t, err)
	assert.Equal(t, string(wantJSON), string(got))
}
