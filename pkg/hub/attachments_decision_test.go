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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// One shared read decision for attachments stored as artifacts
// (ptone/scion#4596, design section 2).

// storedAgentContext returns a context whose caller is agent a, classified as the
// agent credential class.
func storedAgentContext(a *store.Agent) context.Context {
	return contextWithIdentity(context.Background(), &storedAgentIdentity{agent: a})
}

// decisionFixture is a refFixture with the experiment on, a second agent
// of project A that is party to nothing, and one attachment bound to each
// of: a DM between alice and agent aa, and a project B group message
// addressed to agent aa.
type decisionFixture struct {
	*refFixture
	art        artifacts.Store
	ab         *store.Agent
	dmMsg      string
	dmFile     string
	groupMsg   string
	groupFile  string
	unknownGet refAnswer
}

func newDecisionFixture(t *testing.T) *decisionFixture {
	t.Helper()
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	d := &decisionFixture{refFixture: f, art: st}
	d.ab = &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "ab", Slug: "ab",
		Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.ua.ID}
	require.NoError(t, f.st.CreateAgent(ctx, d.ab))

	dm := seedDirectConversation(t, f.st, dmKeyFor(t, "user", f.ua.ID, "agent", f.aa.ID))
	d.dmMsg = d.storeMessage(t, &store.Message{ProjectID: f.projA.ID, Sender: "user:alice", SenderID: f.ua.ID,
		Recipient: "agent:aa", RecipientID: f.aa.ID, ConversationID: dm.ID, ThreadID: dm.ExternalRef})
	d.dmFile = f.uploadAs(t, f.ua, f.projA.ID, "dm.txt", "dm with an agent")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, d.dmFile, artifacts.PrincipalKindUser, f.ua.ID, d.dmMsg, f.projA.ID))

	group := seedGroupConversation(t, f.st, f.projB.ID, "b-room")
	d.groupMsg = d.storeMessage(t, &store.Message{ProjectID: f.projB.ID, Sender: "user:alice", SenderID: f.ua.ID,
		Recipient: "agent:aa", RecipientID: f.aa.ID, ConversationID: group})
	d.groupFile = f.uploadAs(t, f.ua, "", "group.txt", "group of another project")
	require.NoError(t, f.srv.attachmentService().BindAttachment(ctx, d.groupFile, artifacts.PrincipalKindUser, f.ua.ID, d.groupMsg, f.projB.ID))

	d.unknownGet = f.download(t, f.ua, uuid.NewString())
	require.Equal(t, http.StatusNotFound, d.unknownGet.status)
	return d
}

func (d *decisionFixture) storeMessage(t *testing.T, msg *store.Message) string {
	t.Helper()
	msg.ID = uuid.NewString()
	msg.Msg = "with a file"
	msg.Type = "instruction"
	msg.Channel = "web"
	msg.CreatedAt = time.Now().UTC()
	require.NoError(t, d.st.CreateMessage(context.Background(), msg))
	return msg.ID
}

// decide asks the shared list decision for message msgID as the caller of
// ctx.
func (d *decisionFixture) decide(ctx context.Context, msgID string) bool {
	svc := d.srv.attachmentService()
	found, err := svc.MessageAttachments(ctx, []string{msgID})
	if err != nil || len(found[msgID]) == 0 {
		return false
	}
	return svc.MayReadAttachments(withStandingMemo(ctx), msgID, found[msgID][0].MessageProjectID)
}

// TestPermitsMessageRead_AgentNotInStandingRefused (K11a): the agent class
// of PermitsMessageRead holds only while the agent is in good standing.
func TestPermitsMessageRead_AgentNotInStandingRefused(t *testing.T) {
	d := newDecisionFixture(t)
	host := newArtifactHost(d.srv)
	ctx := storedAgentContext(d.aa)
	require.NoError(t, d.srv.agentStanding(context.Background(), d.aa.ID), "precondition: aa is in standing")
	require.True(t, host.PermitsMessageRead(ctx))
	require.True(t, d.decide(ctx, d.dmMsg), "a DM party in standing reads")

	holdAgentFor(t, d.st, d.aa.ID, d.projA.ID, d.ua.ID)
	ctx = storedAgentContext(d.aa)
	require.False(t, host.PermitsMessageRead(ctx), "a held agent's credential reads no message attachment")
	require.False(t, d.decide(ctx, d.dmMsg))
}

// TestAttachmentReadDecision_EveryReturnPath: the by-id download, history
// and the shared decision answer the same for every caller class. Every
// download refusal is byte-equal to the unknown-id answer.
func TestAttachmentReadDecision_EveryReturnPath(t *testing.T) {
	d := newDecisionFixture(t)
	dmKey := dmKeyFor(t, "user", d.ua.ID, "agent", d.aa.ID)
	federated := NewFederatedUserIdentity("https://issuer.decision.test", "dec-fed", d.ua.Email, "Fed", "member", nil)
	brokerOBO := contextWithCredentialContext(
		contextWithIdentity(context.Background(), NewAuthenticatedUser(d.ua.ID, d.ua.Email, d.ua.DisplayName, d.ua.Role, string(ClientTypeWeb))),
		CredentialContext{Kind: CredentialKindBroker})
	token := selfToken(t, d.ua.ID, hubBoundary(), "project:read", "inbox:read", "agent:read")
	visitor := NewVisitorIdentity(uuid.NewString(), "https://issuer.decision.test", "dec-visitor", "visitor@decision.test", "Visitor")

	// Session reader and non-reader: download and history.
	require.Equal(t, http.StatusOK, d.download(t, d.ua, d.dmFile).status, "session reader downloads")
	require.Len(t, d.history(t, d.ua, dmKey).MessageAttachments[d.dmMsg], 1, "session reader's history lists it")
	requireSameAnswer(t, d.unknownGet, d.download(t, d.uc, d.dmFile))
	require.False(t, d.decide(d.sessionContext(d.uc), d.dmMsg), "session non-reader")

	for _, tc := range []struct {
		name     string
		ctx      context.Context
		identity Identity
		msgID    string
		file     string
		permits  bool
		want     bool
	}{
		{name: "agent DM party", ctx: storedAgentContext(d.aa), msgID: d.dmMsg, permits: true, want: true},
		{name: "agent addressee of another project's group message", ctx: storedAgentContext(d.aa), msgID: d.groupMsg, permits: true, want: true},
		{name: "agent non-addressee", ctx: storedAgentContext(d.ab), msgID: d.dmMsg, permits: true, want: false},
		{name: "agent non-addressee, other project", ctx: storedAgentContext(d.ab), msgID: d.groupMsg, permits: true, want: false},
		{name: "user access token, reader (section 9)", identity: token, msgID: d.dmMsg, permits: true, want: true},
		{name: "federated user", identity: federated, msgID: d.dmMsg, file: d.dmFile, want: false},
		{name: "project visitor", identity: visitor, msgID: d.dmMsg, file: d.dmFile, want: false},
		{name: "broker acting for a user", ctx: brokerOBO, msgID: d.dmMsg, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = contextWithIdentity(context.Background(), tc.identity)
			}
			require.Equal(t, tc.permits, newArtifactHost(d.srv).PermitsMessageRead(ctx), "credential class (PermitsMessageRead)")
			if !tc.permits {
				// The second layer: MayReadMessage dispatches on the credential
				// class too, so it refuses on its own.
				require.False(t, newArtifactHost(d.srv).MayReadMessage(ctx, tc.msgID, nil), "credential class (MayReadMessage)")
			}
			require.Equal(t, tc.want, d.decide(ctx, tc.msgID), "list decision")
			merged := d.srv.mergeArtifactAttachments(ctx, []string{tc.msgID}, nil)
			require.Equal(t, tc.want, len(merged[tc.msgID]) == 1, "history merge")
			if tc.file != "" {
				requireSameAnswer(t, d.unknownGet, d.downloadAs(t, tc.identity, tc.file))
			}
		})
	}
}

// TestAttachmentDecision_AgentAddresseeRows: a pending or delivered
// addressee row grants an agent read; a failed row grants nothing.
func TestAttachmentDecision_AgentAddresseeRows(t *testing.T) {
	d := newDecisionFixture(t)
	ctx := context.Background()
	require.False(t, d.decide(storedAgentContext(d.ab), d.dmMsg), "precondition: ab is no addressee")
	row := &store.MessageAddressee{ID: uuid.NewString(), MessageID: d.dmMsg, PrincipalKind: "agent",
		PrincipalID: d.ab.ID, Via: "body-mention", DeliveryState: "pending"}
	require.NoError(t, d.st.AddAddressee(ctx, row))
	require.True(t, d.decide(storedAgentContext(d.ab), d.dmMsg), "pending row")
	require.NoError(t, d.st.UpdateDeliveryState(ctx, row.ID, "delivered", nil))
	require.True(t, d.decide(storedAgentContext(d.ab), d.dmMsg), "delivered row")
	reason := "failed"
	require.NoError(t, d.st.UpdateDeliveryState(ctx, row.ID, "failed", &reason))
	require.False(t, d.decide(storedAgentContext(d.ab), d.dmMsg), "a failed row grants nothing")
}

// countingAttachmentStore counts ListMessageAttachments calls.
type countingAttachmentStore struct {
	artifacts.Store
	lists int
}

func (c *countingAttachmentStore) ListMessageAttachments(ctx context.Context, ids []string) (map[string][]artifacts.AttachmentRow, error) {
	c.lists++
	return c.Store.ListMessageAttachments(ctx, ids)
}

// TestHistory_DecisionOnlyForMessagesWithAttachments: a history page runs
// one batched attachment query, then the message lookup of the decision
// only for the message ids that have attachments.
func TestHistory_DecisionOnlyForMessagesWithAttachments(t *testing.T) {
	d := newDecisionFixture(t)
	counting := &countingAttachmentStore{Store: d.art}
	d.srv.SetArtifactStore(counting)
	// GetMessage lookups are counted by the fixture's fault store, which
	// newRefFixture installed before any setup ran.
	before := d.faults.messageGets(d.dmMsg)
	plain := []string{uuid.NewString(), uuid.NewString()}
	merged := d.srv.mergeArtifactAttachments(d.sessionContext(d.ua), append([]string{d.dmMsg}, plain...), nil)
	require.Len(t, merged[d.dmMsg], 1)
	require.Equal(t, 1, counting.lists, "one batched attachment query per page")
	require.Equal(t, 1, d.faults.messageGets(d.dmMsg)-before, "one decision for the message with attachments")
	for _, id := range plain {
		require.Zero(t, d.faults.messageGets(id), "no decision for a message without attachments")
	}
}

// TestSendEcho_FilteredByDecision: the send echo carries the bound refs for
// a reader; for a sender that does not read its own message it carries no
// artifact ref, and the send still succeeds.
func TestSendEcho_FilteredByDecision(t *testing.T) {
	d := newDecisionFixture(t)
	id := d.uploadAs(t, d.ua, d.projA.ID, "echo.txt", "echo")
	resp := d.sendWith(t, d.ua, d.topicA, id)
	require.Len(t, resp.Attachments, 1, "a reader's echo carries the ref")

	// A sender who no longer reads the message: filtered.
	ref := newArtifactAttachmentRef(artifacts.AttachmentInfo{ID: id, Name: "echo.txt"})
	legacy := AttachmentRef{ID: uuid.NewString(), Name: "legacy.txt"}
	got := d.srv.filterReadableAttachments(d.sessionContext(d.uc), resp.ID, []AttachmentRef{legacy, ref})
	require.Equal(t, []AttachmentRef{legacy}, got, "a non-reader's echo keeps only legacy refs")
	got = d.srv.filterReadableAttachments(d.sessionContext(d.ua), resp.ID, []AttachmentRef{legacy, ref})
	require.Len(t, got, 2)
}

// TestMayReadMessage_ProjectIsNotAReadInput: for sessions and agents the
// message project the host is given is not a read input: readers and
// non-readers get the same answer whatever value is passed, so the value
// cannot widen (or narrow) their read.
func TestMayReadMessage_ProjectIsNotAReadInput(t *testing.T) {
	d := newDecisionFixture(t)
	host := newArtifactHost(d.srv)
	empty, other, own := "", uuid.NewString(), d.projA.ID
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"session reader", d.sessionContext(d.ua), true},
		{"session non-reader", d.sessionContext(d.uc), false},
		{"agent reader", storedAgentContext(d.aa), true},
		{"agent non-reader", storedAgentContext(d.ab), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range []*string{nil, &empty, &other, &own} {
				require.Equal(t, tc.want, host.MayReadMessage(withStandingMemo(tc.ctx), d.dmMsg, p), "project %v", p)
			}
		})
	}
}
