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
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Addressee rows for copies of a message that carries attachments stored as
// artifacts (ptone/scion#4596, design section 6.4).

// hookDispatcher is a recordingDispatcher that calls hook on every
// dispatch before recording it, and fails dispatches to failFor.
type hookDispatcher struct {
	recordingDispatcher
	hook    func(agent *store.Agent, msg *messages.StructuredMessage)
	mu      sync.Mutex
	failFor map[string]bool
}

func (d *hookDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, msg *messages.StructuredMessage) error {
	if d.hook != nil {
		d.hook(agent, msg)
	}
	d.mu.Lock()
	fail := d.failFor[agent.ID]
	d.mu.Unlock()
	if fail {
		return errors.New("broker refused the dispatch")
	}
	return d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, msg)
}

// newMentionFixture is a sendFixture with a third agent, cee, of project A
// on the same broker.
func newMentionFixture(t *testing.T) (*sendFixture, *store.Agent) {
	t.Helper()
	f := newSendFixture(t)
	c := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "cee", Slug: "cee", Phase: "running",
		OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: f.brokerID,
		MessageMode: store.MessageModeProject, Ancestry: []string{f.ua.ID}}
	require.NoError(t, f.st.CreateAgent(context.Background(), c))
	return f, c
}

// addresseeState returns the delivery state of agent a's addressee row on
// message msgID, or "" when there is none.
func addresseeState(t *testing.T, s store.Store, msgID string, a *store.Agent) string {
	t.Helper()
	rows, err := s.ListAddressees(context.Background(), msgID)
	require.NoError(t, err)
	for _, r := range rows {
		if r.PrincipalKind == "agent" && r.PrincipalID == a.ID {
			return r.DeliveryState
		}
	}
	return ""
}

// TestMentionCopy_AddresseeRowBeforeDispatch (K17): a mention copy of a
// message with an attachment reaches the dispatcher only after the
// mentioned agent's pending addressee row on the primary exists; after the
// attempt the row is delivered, and the copy lists the primary's
// attachment for delivery.
func TestMentionCopy_AddresseeRowBeforeDispatch(t *testing.T) {
	f, c := newMentionFixture(t)
	hd := &hookDispatcher{}
	var seen []string
	hd.hook = func(agent *store.Agent, msg *messages.StructuredMessage) {
		if agent.ID != c.ID || msg.Type != messages.TypeMention {
			return
		}
		primary := msg.Metadata["mention_of"]
		seen = append(seen, addresseeState(t, f.st, primary, c))
	}
	f.srv.SetDispatcher(hd)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "see this @cee", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	require.Equal(t, []string{addresseePending}, seen, "the pending row exists when the copy is dispatched")
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, primary, c))
	var copyDeliveries []messages.AttachmentDelivery
	for _, call := range hd.getCalls() {
		if call.Agent.ID == c.ID {
			copyDeliveries = call.StructuredMessage.AttachmentDeliveries
		}
	}
	require.Len(t, copyDeliveries, 1)
	require.Equal(t, primary, copyDeliveries[0].MessageID, "the copy delivers the primary's attachment")
}

// TestMentionCopy_FailedRowGrantsNothing: when the copy's delivery fails,
// the row is failed, and the read decision for the mentioned agent
// answers no.
func TestMentionCopy_FailedRowGrantsNothing(t *testing.T) {
	f, c := newMentionFixture(t)
	hd := &hookDispatcher{failFor: map[string]bool{c.ID: true}}
	f.srv.SetDispatcher(hd)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "see this @cee", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	require.Equal(t, addresseeFailed, addresseeState(t, f.st, primary, c))
	svc := f.srv.attachmentService()
	found, err := svc.MessageAttachments(context.Background(), []string{primary})
	require.NoError(t, err)
	require.Len(t, found[primary], 1)
	require.False(t, svc.MayReadAttachments(withStandingMemo(storedAgentContext(c)), primary, found[primary][0].MessageProjectID),
		"a failed row grants nothing")
}

// mentionInput builds a fan-out input for a mention of the primary message
// primaryID by f.sender, carrying the primary's bound attachments.
func mentionInput(t *testing.T, f *sendFixture, primaryID string) agentMentionFanoutInput {
	t.Helper()
	found, err := f.srv.attachmentService().MessageAttachments(context.Background(), []string{primaryID})
	require.NoError(t, err)
	var refs []AttachmentRef
	for _, info := range found[primaryID] {
		refs = append(refs, newArtifactAttachmentRef(info))
	}
	require.NotEmpty(t, refs)
	return agentMentionFanoutInput{
		Sender: f.sender,
		SenderIdent: &agentIdentityWrapper{&AgentTokenClaims{
			Claims: jwt.Claims{Subject: f.sender.ID}, ProjectID: f.sender.ProjectID, Ancestry: f.sender.Ancestry,
		}},
		Msg: "see this", Type: messages.TypeInstruction,
		ParentMessageID: primaryID, ParentAttachments: refs,
	}
}

// TestMentionCopy_StoppedMentionWritesNoRow (K19): an unauthorized
// mention, a suppressed mention (pair limiter full) and a mention whose
// conversation resolution fails each record no addressee row, and the
// read decision for the target answers no.
func TestMentionCopy_StoppedMentionWritesNoRow(t *testing.T) {
	f, c := newMentionFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "primary", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	ctx := context.Background()
	decide := func(a *store.Agent) bool {
		svc := f.srv.attachmentService()
		found, err := svc.MessageAttachments(ctx, []string{primary})
		require.NoError(t, err)
		return svc.MayReadAttachments(withStandingMemo(storedAgentContext(a)), primary, found[primary][0].MessageProjectID)
	}

	t.Run("unauthorized", func(t *testing.T) {
		sealed := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "sealed", Slug: "sealed", Phase: "running",
			OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: f.brokerID, MessageMode: store.MessageModeNone, Ancestry: []string{f.ua.ID}}
		require.NoError(t, f.st.CreateAgent(ctx, sealed))
		out := f.srv.deliverAgentMentionCopy(ctx, ctx, mentionInput(t, f, primary), sealed, "body")
		require.Equal(t, "unauthorized", out.Status, out.Error)
		require.Equal(t, "", addresseeState(t, f.st, primary, sealed))
		require.False(t, decide(sealed))
	})
	t.Run("suppressed", func(t *testing.T) {
		for f.srv.mentionPairLimiter.Reserve(f.sender.ID, c.ID) {
		}
		out := f.srv.deliverAgentMentionCopy(ctx, ctx, mentionInput(t, f, primary), c, "body")
		require.Equal(t, "suppressed", out.Status, out.Error)
		require.Equal(t, "", addresseeState(t, f.st, primary, c))
		require.False(t, decide(c))
	})
	t.Run("conversation resolution fails", func(t *testing.T) {
		d := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "dee", Slug: "dee", Phase: "running",
			OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: f.brokerID, MessageMode: store.MessageModeProject, Ancestry: []string{f.ua.ID}}
		require.NoError(t, f.st.CreateAgent(ctx, d))
		f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": true}`)) // write-deny on
		f.faults.failUpsertConv = true
		f.fault.Arm()
		out := f.srv.deliverAgentMentionCopy(ctx, ctx, mentionInput(t, f, primary), d, "body")
		require.Equal(t, "error", out.Status, out.Error)
		require.Equal(t, "", addresseeState(t, f.st, primary, d))
		require.False(t, decide(d))
	})
}

// TestWebMentionCopy_DMSenderMentionsOtherAgentGetsFile: a session user in
// a DM with agent P attaches a file and mentions agent C; C is recorded as
// an addressee and its copy lists the file for delivery.
func TestWebMentionCopy_DMSenderMentionsOtherAgentGetsFile(t *testing.T) {
	f, c := newMentionFixture(t)
	dm := dmKeyFor(t, "user", f.ua.ID, "agent", f.target.ID)
	id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "hello @cee", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	resp := f.sendResponse(t, got)
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, resp.ID, c))
	call := f.lastDispatch(t, c)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, resp.ID, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
}

// TestMentionCopy_OutboundAgentDMDeliversToMentioned: on the outbound
// route, an agent-to-agent DM that @mentions another agent delivers the
// primary's attachment with the mention copy, and records the mentioned
// agent as a delivered addressee.
func TestMentionCopy_OutboundAgentDMDeliversToMentioned(t *testing.T) {
	f, c := newMentionFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "agent:" + f.target.Slug, Msg: "see this @cee",
		Type: messages.TypeInstruction, AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	call := f.lastDispatch(t, c)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, primary, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, primary, c))
}

// TestMentionCopy_DefaultAgentWakeDeliversAttachment: an agent's group post
// with an attachment wakes the group's default agent, whose copy delivers
// the attachment and records it as an addressee via the default agent.
func TestMentionCopy_DefaultAgentWakeDeliversAttachment(t *testing.T) {
	f, c := newMentionFixture(t)
	ctx := context.Background()
	convID := seedGroupConversation(t, f.st, f.projA.ID, "wake-room")
	conv, err := f.st.GetConversation(ctx, convID)
	require.NoError(t, err)
	conv.DefaultAgentID = &c.ID
	require.NoError(t, f.st.UpdateConversation(ctx, conv))
	grantAgentProjectAccess(t, f.st, f.sender.ID, f.projA.ID)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{ConversationRef: "conv:" + convID, Msg: "status for the room",
		AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	call := f.lastDispatch(t, c)
	require.Equal(t, "default_agent", call.StructuredMessage.Metadata["mention_position"])
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	rows, err := f.st.ListAddressees(ctx, primary)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "default-agent", rows[0].Via)
	require.Equal(t, addresseeDelivered, rows[0].DeliveryState)
}

// TestAttachmentAddressee_DeliveredRowKept: a second copy for an agent
// that already received one leaves its delivered row as it is, even when
// the second copy fails.
func TestAttachmentAddressee_DeliveredRowKept(t *testing.T) {
	f, c := newMentionFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "see this @cee", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	primary := messageIDOf(t, rec)
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, primary, c))
	row := f.srv.recordAttachmentAddressee(context.Background(), primary, c.ID, "default-agent")
	require.Empty(t, row, "nothing is tracked for a second copy")
	f.srv.finishAttachmentAddressee(context.Background(), row, errors.New("second copy failed"))
	require.Equal(t, addresseeDelivered, addresseeState(t, f.st, primary, c))
}
