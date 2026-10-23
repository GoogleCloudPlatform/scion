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
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Delivery of attachments stored as artifacts into the recipient agent's
// home: plan, capabilities and redemption (ptone/scion#4596, section 8).

// enableBrokerDelivery makes the fixture's broker advertise the attachment
// delivery capability.
func (f *sendFixture) enableBrokerDelivery(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	b, err := f.st.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{AttachmentDelivery: true}
	require.NoError(t, f.st.UpdateRuntimeBroker(ctx, b))
	require.True(t, f.srv.brokerReceivesAttachments(ctx, f.brokerID))
}

// sendWithFile sends one uploaded file from the sender to the target and
// returns the message id and the dispatch the target got.
func (f *sendFixture) sendWithFile(t *testing.T, name, content string) (string, dispatchCall) {
	t.Helper()
	id := f.upload(t, f.sender, name, content)
	rec := f.message(t, f.sender, f.target, "see file", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return messageIDOf(t, rec), f.lastDispatch(t, f.target)
}

// issue issues the capabilities of call's plan as the dispatcher would.
func (f *sendFixture) issue(call dispatchCall) []messages.WireAttachment {
	return f.srv.attachmentDelivery().Issue(context.Background(), call.Agent, call.Agent.RuntimeBrokerID, call.StructuredMessage.AttachmentPlan)
}

// tokenOf returns the capability token of a wire attachment's URL.
func tokenOf(t *testing.T, w messages.WireAttachment) string {
	t.Helper()
	require.True(t, strings.HasPrefix(w.URL, attachmentDeliveriesPrefix), w.URL)
	return strings.TrimPrefix(w.URL, attachmentDeliveriesPrefix)
}

// redeem redeems token as broker brokerID (an empty id: the session of a
// user, a credential that is not a broker's) through the registered route
// handler.
func (f *sendFixture) redeem(t *testing.T, brokerID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, attachmentDeliveriesPrefix+token, nil)
	ctx := req.Context()
	if brokerID != "" {
		ctx = contextWithIdentity(ctx, NewBrokerIdentity(brokerID))
	} else {
		ctx = f.sessionContext(f.ua)
	}
	rec := httptest.NewRecorder()
	f.srv.guarded(attachmentDeliveriesPrefix, f.srv.handleAttachmentDelivery)(rec, req.WithContext(ctx))
	return rec
}

// TestDeliveryName_Reduced (K12): stored names are reduced to
// [A-Za-z0-9._-], at most 120 runes, never starting with '.' or '-'.
func TestDeliveryName_Reduced(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	for _, name := range []string{"a/b.txt", "../../etc/passwd", "line\nbreak.txt", "$(rm -rf).sh", "back`tick`.md",
		".hidden", "-rf", "résumé.pdf", strings.Repeat("x", 300), "", "normal-name_1.txt"} {
		got := reduceAttachmentName(name)
		require.Regexp(t, safe, got, "%q", name)
		require.False(t, strings.HasPrefix(got, ".") || strings.HasPrefix(got, "-"), "%q -> %q", name, got)
		require.LessOrEqual(t, len([]rune(got)), maxAttachmentNameRunes, "%q", name)
	}
	require.Equal(t, "normal-name_1.txt", reduceAttachmentName("normal-name_1.txt"))

	// End to end: the wire name and path carry the reduced name.
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	_, call := f.sendWithFile(t, "my notes (1).txt", "x")
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.Regexp(t, safe, wire[0].Name)
	require.Equal(t, "my_notes__1_.txt", wire[0].Name)
	require.True(t, strings.HasSuffix(wire[0].Path, "/"+wire[0].Name))
}

// TestDeliveryCapability_IssuedOnlyToReaders (K24): a recipient that fails
// the read decision gets no plan entry it could see and no capability.
func TestDeliveryCapability_IssuedOnlyToReaders(t *testing.T) {
	f, c := newMentionFixture(t)
	f.enableBrokerDelivery(t)
	msgID, call := f.sendWithFile(t, "x.txt", "x")
	require.Len(t, f.issue(call), 1, "the recipient reads it")
	deliveries := call.StructuredMessage.AttachmentDeliveries
	require.Len(t, deliveries, 1)
	// c is neither a party of the DM nor an addressee.
	plan := f.srv.attachmentDelivery().Plan(context.Background(), c, deliveries)
	require.Len(t, plan, 1)
	require.Equal(t, messages.AttachmentWithheld, plan[0].State, "withheld from a non-reader")
	require.Empty(t, f.srv.attachmentDelivery().Issue(context.Background(), c, f.brokerID, plan), "no capability for a non-reader")
	// Given the reader's deliverable plan, Issue still asks the decision for
	// the non-reader and issues nothing.
	wire := f.srv.attachmentDelivery().Issue(context.Background(), c, f.brokerID, call.StructuredMessage.AttachmentPlan)
	require.Len(t, wire, 1)
	require.Empty(t, wire[0].URL, "the decision, not the plan, gates the capability")
	text := messaging.RenderDeliveryText(messaging.RenderDeliveryInput{MessageID: msgID, Msg: &messages.StructuredMessage{Msg: "hi"}, Plan: plan})
	require.NotContains(t, text, "x.txt", "the non-reader's envelope lists nothing")
}

// TestDeliveryCapability_RecipientNotInStandingGetsNone (K11b): a
// recipient that is not in good standing gets no capability.
func TestDeliveryCapability_RecipientNotInStandingGetsNone(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	_, call := f.sendWithFile(t, "x.txt", "x")
	require.Len(t, f.issue(call), 1)
	holdAgentFor(t, f.st, f.target.ID, f.projA.ID, f.ua.ID)
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.Empty(t, wire[0].URL, "a recipient out of standing gets no capability")
}

// TestPlan_OldBrokerGetsNotDeliveredLine (K29): a broker that does not
// advertise the capability gets no capability, and the envelope tells the
// recipient the file was not delivered.
func TestPlan_OldBrokerGetsNotDeliveredLine(t *testing.T) {
	f := newSendFixture(t)
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": true}`))
	_, call := f.sendWithFile(t, "report.txt", "x")
	plan := call.StructuredMessage.AttachmentPlan
	require.Len(t, plan, 1)
	require.Equal(t, messages.AttachmentUndeliverableRuntime, plan[0].State)
	require.Empty(t, f.issue(call))
	require.Contains(t, call.StructuredMessage.DeliveryText, "Attachment not delivered: report.txt")
	require.Contains(t, call.StructuredMessage.DeliveryText, "the recipient's runtime cannot receive attachments")
}

// TestPlan_EnvelopeAndIssueAgree: the paths the envelope lists are the
// paths of the issued entries; a decision that turns to no between render
// and dispatch gives an entry without a URL.
func TestPlan_EnvelopeAndIssueAgree(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": true}`))
	_, call := f.sendWithFile(t, "report.txt", "x")
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.NotEmpty(t, wire[0].URL)
	require.Contains(t, call.StructuredMessage.DeliveryText, wire[0].Path, "the envelope lists the issued path")
	holdAgentFor(t, f.st, f.target.ID, f.projA.ID, f.ua.ID)
	wire = f.issue(call)
	require.Len(t, wire, 1)
	require.Empty(t, wire[0].URL, "a changed decision issues no capability")
	require.Equal(t, call.StructuredMessage.AttachmentPlan[0].InboxPath, wire[0].Path)
}

// TestPlan_PlainMessageGetsPathLines and TestPlan_LegacyEnvelopeGetsPaths.
func TestPlan_PlainMessageGetsPathLines(t *testing.T) {
	plan := []messages.AttachmentPlanEntry{
		{Name: "a.txt", InboxPath: "~/.scion/inbox/m1/a.txt", State: messages.AttachmentDeliverable},
		{Name: "b.txt", InboxPath: "~/.scion/inbox/m1/b.txt", State: messages.AttachmentUndeliverableRuntime, Delivery: messages.AttachmentDelivery{Size: 2048}},
		{Name: "c.txt", State: messages.AttachmentWithheld},
	}
	text := messaging.RenderDeliveryText(messaging.RenderDeliveryInput{MessageID: "m1", Msg: &messages.StructuredMessage{Msg: "body", Plain: true}, Plan: plan})
	require.Contains(t, text, "Attachment: ~/.scion/inbox/m1/a.txt")
	require.Contains(t, text, "Attachment not delivered: b.txt (2.0 KiB): the recipient's runtime cannot receive attachments. Ask the sender to resend it.")
	require.NotContains(t, text, "c.txt")
}

func TestPlan_LegacyEnvelopeGetsPaths(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	// The envelope switch is off: the broker formats the legacy envelope
	// from the structured message.
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": false}`))
	_, call := f.sendWithFile(t, "report.txt", "x")
	require.Empty(t, call.StructuredMessage.DeliveryText)
	// The dispatched message carries no inbox path; the copy the dispatcher
	// sends to the broker carries the deliverable one.
	path := call.StructuredMessage.AttachmentPlan[0].InboxPath
	require.NotContains(t, call.StructuredMessage.Attachments, path)
	require.Contains(t, legacyAttachmentCopy(call.StructuredMessage).Attachments, path)
}

// TestDeliveryCapability_ObserverCopyGetsNone (K7): an observer copy of a
// cross-project message carries no delivery list.
func TestDeliveryCapability_ObserverCopyGetsNone(t *testing.T) {
	msg := &messages.StructuredMessage{
		AttachmentDeliveries: []messages.AttachmentDelivery{{ArtifactID: uuid.NewString()}},
		AttachmentPlan:       []messages.AttachmentPlanEntry{{State: messages.AttachmentDeliverable}},
	}
	sanitizeCrossProjectObserver(msg)
	require.Nil(t, msg.AttachmentDeliveries)
	require.Nil(t, msg.AttachmentPlan)
}

// TestDeliveryEnvelope_PathsForIssuedOnly: only deliverable entries are
// listed as paths.
func TestDeliveryEnvelope_PathsForIssuedOnly(t *testing.T) {
	msg := &messages.StructuredMessage{Msg: "body"}
	messaging.ApplyAttachmentPlanLegacy(msg, []messages.AttachmentPlanEntry{
		{Name: "a.txt", InboxPath: "~/.scion/inbox/m/a.txt", State: messages.AttachmentDeliverable},
		{Name: "w.txt", InboxPath: "~/.scion/inbox/m/w.txt", State: messages.AttachmentWithheld},
	})
	require.Equal(t, []string{"~/.scion/inbox/m/a.txt"}, msg.Attachments)
	require.Equal(t, "body", msg.Msg)
}

// fixtureToken issues one capability for a sent file and returns it with
// the message id.
func fixtureToken(t *testing.T, f *sendFixture) (string, string) {
	t.Helper()
	f.enableBrokerDelivery(t)
	msgID, call := f.sendWithFile(t, "report.txt", "file bytes")
	wire := f.issue(call)
	require.Len(t, wire, 1)
	return tokenOf(t, wire[0]), msgID
}

// TestDeliveryRedemption_UniformRefusal and the single checks (K21-K23):
// the right broker reads the bytes once; a wrong broker, no broker
// credential, a tampered token, an expired token, a second use and a
// deleted message all get the same 404.
func TestDeliveryRedemption_UniformRefusal(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	require.Equal(t, http.StatusNotFound, unknown.Code)
	require.Equal(t, unknown.Body.String(), f.redeem(t, "", token).Body.String(), "a credential that is not a broker's")
	ok := f.redeem(t, f.brokerID, token)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	require.Equal(t, "file bytes", ok.Body.String())
	second := f.redeem(t, f.brokerID, token)
	require.Equal(t, unknown.Code, second.Code)
	require.Equal(t, unknown.Body.String(), second.Body.String())
}

func TestDeliveryRedemption_WrongBrokerRefused(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, uuid.NewString(), token)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
	require.Equal(t, http.StatusOK, f.redeem(t, f.brokerID, token).Code, "the capability was not spent by the refusal")
}

func TestDeliveryRedemption_TamperedTokenRefused(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	payload, mac, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	require.NoError(t, err)
	var c deliveryCapability
	require.NoError(t, json.Unmarshal(raw, &c))
	c.AgentID = uuid.NewString()
	forged, _ := json.Marshal(c)
	tampered := base64.RawURLEncoding.EncodeToString(forged) + "." + mac
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, tampered)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

func TestDeliveryRedemption_ExpiredRefused(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	_, call := f.sendWithFile(t, "report.txt", "x")
	plan := call.StructuredMessage.AttachmentPlan[0]
	token, err := f.srv.signDeliveryCapability(deliveryCapability{
		ArtifactID: plan.Delivery.ArtifactID, MessageID: plan.Delivery.MessageID, AgentID: f.target.ID,
		BrokerID: f.brokerID, Exp: time.Now().Add(-time.Second).Unix(), Nonce: "expired-nonce",
	})
	require.NoError(t, err)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, token)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

func TestDeliveryRedemption_SecondUseRefused(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	require.Equal(t, http.StatusOK, f.redeem(t, f.brokerID, token).Code)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, token)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

func TestDeliveryRedemption_DeletedMessageRefused(t *testing.T) {
	f := newSendFixture(t)
	token, msgID := fixtureToken(t, f)
	// The message is deleted while its attachment is still bound: the
	// live-message check refuses.
	require.NoError(t, f.wcs.SetMessageDeleted(context.Background(), msgID, time.Now()))
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, token)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

// TestDeliveryRedemption_ConcurrentSecondUseRefused: two parallel
// redemptions of one token: exactly one gets the bytes, the other the
// uniform 404.
func TestDeliveryRedemption_ConcurrentSecondUseRefused(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = f.redeem(t, f.brokerID, token)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, r := range results {
		if r.Code == http.StatusOK {
			ok++
			require.Equal(t, "file bytes", r.Body.String())
			continue
		}
		require.Equal(t, http.StatusNotFound, r.Code)
		require.Equal(t, unknown.Body.String(), r.Body.String())
	}
	require.Equal(t, 1, ok, "exactly one redemption serves the file")
}

// TestDeliveryCapability_FreshPerDispatch: two dispatch attempts through
// the HTTP dispatcher carry different capabilities for the same rendered
// text.
func TestDeliveryCapability_FreshPerDispatch(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	_, call := f.sendWithFile(t, "report.txt", "x")
	client := &wireCaptureClient{}
	d := NewHTTPAgentDispatcherWithClient(f.st, client, false, slog.Default())
	d.SetAttachmentIssuer(f.srv.attachmentDelivery())
	ctx := context.Background()
	require.NoError(t, d.DispatchAgentMessage(ctx, f.target, call.Message, false, call.StructuredMessage))
	require.NoError(t, d.DispatchAgentMessage(ctx, f.target, call.Message, false, call.StructuredMessage))
	require.Len(t, client.wires, 2)
	require.Len(t, client.wires[0], 1)
	require.Len(t, client.wires[1], 1)
	require.NotEqual(t, client.wires[0][0].URL, client.wires[1][0].URL, "each attempt carries a fresh capability")
	require.Equal(t, client.texts[0], client.texts[1], "the rendered text is the same")
}

// wireCaptureClient records the wire attachments and text of each message
// dispatch.
type wireCaptureClient struct {
	mockRuntimeBrokerClient
	wires [][]messages.WireAttachment
	texts []string
}

func (c *wireCaptureClient) MessageAgent(ctx context.Context, _, _, _, _, message string, _ bool, msg *messages.StructuredMessage) error {
	c.wires = append(c.wires, dispatchAttachmentsFromContext(ctx))
	c.texts = append(c.texts, message+"|"+msg.DeliveryText)
	return nil
}

// TestWebSend_AgentRecipientGetsFile (K32) and
// TestWebSend_MentionedAgentGetsFile: a web send to an agent plans and
// issues the file for the primary; a mentioned agent gets it through its
// addressee row.
func TestWebSend_AgentRecipientGetsFile(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": true}`))
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
	f.sendWith(t, f.ua, f.topicA, id)
	call := f.lastDispatch(t, f.target)
	require.Len(t, call.StructuredMessage.AttachmentPlan, 1)
	require.Contains(t, call.StructuredMessage.DeliveryText, call.StructuredMessage.AttachmentPlan[0].InboxPath,
		"the primary's envelope lists the inbox path")
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.NotEmpty(t, wire[0].URL)
}

func TestWebSend_MentionedAgentGetsFile(t *testing.T) {
	f, c := newMentionFixture(t)
	f.enableBrokerDelivery(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "hello @cee", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	call := f.lastDispatch(t, c)
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.NotEmpty(t, wire[0].URL)
}

// TestDeliveryCapability_MentionCopyAddresseeGetsFile (K2): agent A DMs
// agent P with an attachment and @C; C's copy lands in its own A-C
// conversation, so C reads the primary's attachment only through its
// addressee row.
func TestDeliveryCapability_MentionCopyAddresseeGetsFile(t *testing.T) {
	f, c := newMentionFixture(t)
	f.enableBrokerDelivery(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "see this @cee", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call := f.lastDispatch(t, c)
	require.Len(t, call.StructuredMessage.AttachmentPlan, 1)
	require.Equal(t, messages.AttachmentDeliverable, call.StructuredMessage.AttachmentPlan[0].State)
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.NotEmpty(t, wire[0].URL, "the addressee gets a capability")
}

// TestDeliveryCapability_CrossProjectRecipientGetsFile and
// TestAttachmentDecision_PeerOfAddresseeRefused: an agent in another
// project that is the DM recipient gets the file; another agent of its
// project does not, and cannot redeem the recipient's capability.
func TestDeliveryCapability_CrossProjectRecipientGetsFile(t *testing.T) {
	f, bee, peer := crossProjectDeliveryFixture(t)
	id := f.upload(t, f.sender, "cross.txt", "cross bytes")
	rec := f.message(t, f.sender, bee, "across", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call := f.lastDispatch(t, bee)
	wire := f.issue(call)
	require.Len(t, wire, 1)
	require.NotEmpty(t, wire[0].URL)
	ok := f.redeem(t, f.brokerID, tokenOf(t, wire[0]))
	require.Equal(t, http.StatusOK, ok.Code)
	require.Equal(t, "cross bytes", ok.Body.String())
	_ = peer
}

func TestAttachmentDecision_PeerOfAddresseeRefused(t *testing.T) {
	f, bee, peer := crossProjectDeliveryFixture(t)
	id := f.upload(t, f.sender, "cross.txt", "cross bytes")
	rec := f.message(t, f.sender, bee, "across", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call := f.lastDispatch(t, bee)
	plan := f.srv.attachmentDelivery().Plan(context.Background(), peer, call.StructuredMessage.AttachmentDeliveries)
	require.Equal(t, messages.AttachmentWithheld, plan[0].State, "the recipient's project peer does not read it")
	// A capability issued to the recipient names it; presenting it for the
	// peer changes the signed content, so it does not verify.
	wire := f.issue(call)
	token := tokenOf(t, wire[0])
	payload, mac, _ := strings.Cut(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var c deliveryCapability
	require.NoError(t, json.Unmarshal(raw, &c))
	c.AgentID = peer.ID
	forged, _ := json.Marshal(c)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, base64.RawURLEncoding.EncodeToString(forged)+"."+mac)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

// crossProjectDeliveryFixture is a sendFixture with cross-project
// messaging on, a hub-mode sender of project A and two hub-mode agents of
// project B (bee, the recipient, and peer), with broker delivery on.
func crossProjectDeliveryFixture(t *testing.T) (*sendFixture, *store.Agent, *store.Agent) {
	t.Helper()
	f := newSendFixture(t)
	ctx := context.Background()
	_, err := f.st.UpdateProjectMessagingPolicy(ctx, f.projB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)
	mk := func(slug, projectID, owner string) *store.Agent {
		a := &store.Agent{ID: uuid.NewString(), ProjectID: projectID, Name: slug, Slug: slug, Phase: "running",
			OwnerID: owner, CreatedBy: owner, RuntimeBrokerID: f.brokerID,
			MessageMode: store.MessageModeHub, Ancestry: []string{owner}}
		require.NoError(t, f.st.CreateAgent(ctx, a))
		return a
	}
	f.sender = mk("xsnd", f.projA.ID, f.ua.ID)
	bee := mk("xbee", f.projB.ID, f.ub.ID)
	peer := mk("xpeer", f.projB.ID, f.ub.ID)
	enableCPM(t, f.srv, f.st)
	f.enableBrokerDelivery(t)
	return f, bee, peer
}

// TestCatchUp_DeferredAttachmentsListedAsNotDelivered (D15): a deferred
// message's attachments are listed on the conversation messages route to
// a reader, and to nobody else.
func TestCatchUp_DeferredAttachmentsListedAsNotDelivered(t *testing.T) {
	f, c := newMentionFixture(t)
	ctx := context.Background()
	// The target is reincarnating: the message is stored deferred.
	tgt, err := f.st.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	markReincarnating(t, f.st, tgt)
	id := f.upload(t, f.sender, "late.txt", "late bytes")
	rec := f.message(t, f.sender, f.target, "while you were away", id)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	msgID := messageIDOf(t, rec)
	m, err := f.st.GetMessage(ctx, msgID)
	require.NoError(t, err)
	require.Equal(t, store.MessageDispatchDeferred, m.DispatchState)

	items := []store.Message{*m}
	f.srv.noteDeferredAttachments(storedAgentContext(f.target), items)
	require.Equal(t, []store.DeferredAttachment{{Name: "late.txt", Size: int64(len("late bytes"))}}, items[0].DeferredAttachments)
	line := messaging.AttachmentNotDeliveredLine(items[0].DeferredAttachments[0].Name, items[0].DeferredAttachments[0].Size, "the message was deferred")
	require.Equal(t, "Attachment not delivered: late.txt (10 bytes): the message was deferred. Ask the sender to resend it.", line)

	other := []store.Message{*m}
	f.srv.noteDeferredAttachments(storedAgentContext(c), other)
	require.Nil(t, other[0].DeferredAttachments, "a non-reader gets no field")

	// Through the conversation messages route catch-up reads.
	require.NotEmpty(t, m.ConversationID)
	list := f.agentRequest(t, f.target, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+m.ConversationID+"/messages", nil))
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	require.Contains(t, list.Body.String(), `"deferredAttachments":[{"name":"late.txt","size":10}]`)
}

// markReincarnating puts agent a mid-reincarnation, so messages to it are
// stored deferred and never dispatched.
func markReincarnating(t *testing.T, s store.Store, a *store.Agent) {
	t.Helper()
	a.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// TestDeliveryRedemption_RetiredAttachmentRefused: an attachment no longer
// bound (retired with its message) is refused with the uniform 404.
func TestDeliveryRedemption_RetiredAttachmentRefused(t *testing.T) {
	f := newSendFixture(t)
	token, msgID := fixtureToken(t, f)
	f.srv.retireMessageAttachments(context.Background(), msgID)
	unknown := f.redeem(t, f.brokerID, "not-a-token")
	got := f.redeem(t, f.brokerID, token)
	require.Equal(t, http.StatusNotFound, got.Code)
	require.Equal(t, unknown.Body.String(), got.Body.String())
}

// countingGetAttachmentStore counts attachment lookups.
type countingGetAttachmentStore struct {
	artifacts.Store
	mu   sync.Mutex
	gets int
}

func (c *countingGetAttachmentStore) GetAttachment(ctx context.Context, id string) (*artifacts.Artifact, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Store.GetAttachment(ctx, id)
}

// TestDeliveryRedemption_LoserReadsNothing: of two parallel redemptions of
// one token, the one that loses the nonce is answered without the
// attachment being looked up.
func TestDeliveryRedemption_LoserReadsNothing(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	counting := &countingGetAttachmentStore{Store: f.art}
	f.srv.SetArtifactStore(counting)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.redeem(t, f.brokerID, token).Code
		}(i)
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusNotFound}, codes)
	counting.mu.Lock()
	defer counting.mu.Unlock()
	require.Equal(t, 1, counting.gets, "only the winning redemption looks the attachment up")
}

// TestSetDispatcher_InstallsAttachmentIssuer: the server wires its delivery
// component into an HTTP dispatcher, so production dispatches issue
// capabilities.
func TestSetDispatcher_InstallsAttachmentIssuer(t *testing.T) {
	f := newSendFixture(t)
	d := NewHTTPAgentDispatcherWithClient(f.st, &wireCaptureClient{}, false, slog.Default())
	f.srv.SetDispatcher(d)
	_, ok := d.attachmentIssuer.(*attachmentDelivery)
	require.True(t, ok, "SetDispatcher installs the attachment issuer")
}

// TestPlan_LegacyCopiesCarryOnlyTheirOwnPaths: with the envelope switch
// off, a web send that @mentions an agent: the messages the hub dispatches
// carry no inbox paths; the copy the dispatcher sends for each carries its
// own plan's deliverable paths once; a withheld entry puts none in it.
func TestPlan_LegacyCopiesCarryOnlyTheirOwnPaths(t *testing.T) {
	f, c := newMentionFixture(t)
	f.enableBrokerDelivery(t)
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"conversation_envelope_switch": false}`))
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "hello @cee", "attachments": []string{id}})
	require.Equal(t, http.StatusCreated, got.status, got.body)
	primary := f.lastDispatch(t, f.target)
	require.Empty(t, primary.StructuredMessage.Attachments, "the dispatched message is not changed by the plan")
	sent := legacyAttachmentCopy(primary.StructuredMessage)
	require.Equal(t, []string{primary.StructuredMessage.AttachmentPlan[0].InboxPath}, sent.Attachments)
	require.Empty(t, primary.StructuredMessage.Attachments, "the copy does not alias the message")
	mention := f.lastDispatch(t, c)
	require.Empty(t, mention.StructuredMessage.Attachments, "the mention copy does not inherit the primary's paths")
	sentMention := legacyAttachmentCopy(mention.StructuredMessage)
	require.Equal(t, []string{mention.StructuredMessage.AttachmentPlan[0].InboxPath}, sentMention.Attachments, "its own path, once")

	// A withheld entry puts no path in the copy sent.
	withheld := *mention.StructuredMessage
	withheld.AttachmentPlan = []messages.AttachmentPlanEntry{{Name: "web.txt", InboxPath: "~/.scion/inbox/x/web.txt", State: messages.AttachmentWithheld}}
	require.Empty(t, legacyAttachmentCopy(&withheld).Attachments)
}
