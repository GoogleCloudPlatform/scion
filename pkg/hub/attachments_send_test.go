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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Sending attachments stored as artifacts by id (ptone/scion#4596, design
// sections 6.2 and 6.4, decision D2).

// sendFixture is a refFixture with the experiment on, a runtime broker, two
// running agents of project A placed on it (sender and target), and a
// recording dispatcher.
type sendFixture struct {
	*refFixture
	art            artifacts.Store
	brokerID       string
	sender, target *store.Agent
	disp           *recordingDispatcher
}

func newSendFixture(t *testing.T) *sendFixture {
	t.Helper()
	f := newRefFixture(t)
	art := enableAttachmentArtifacts(t, f)
	ctx := context.Background()
	brokerID := uuid.NewString()
	require.NoError(t, f.st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "send-broker", Slug: "send-broker", Status: store.BrokerStatusOnline,
	}))
	mk := func(slug string) *store.Agent {
		a := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: slug, Slug: slug, Phase: "running",
			OwnerID: f.ua.ID, CreatedBy: f.ua.ID, RuntimeBrokerID: brokerID,
			MessageMode: store.MessageModeProject, Ancestry: []string{f.ua.ID}}
		require.NoError(t, f.st.CreateAgent(ctx, a))
		return a
	}
	sf := &sendFixture{refFixture: f, art: art, brokerID: brokerID, sender: mk("snd"), target: mk("tgt")}
	sf.disp = &recordingDispatcher{}
	f.srv.SetDispatcher(sf.disp)
	return sf
}

// withBrokerProxy installs a message broker proxy with an in-process bus
// (and a spy spoke and the web channel spoke), so agent-to-user sends take
// the broker path.
func (f *sendFixture) withBrokerProxy(t *testing.T) *MessageBrokerProxy {
	t.Helper()
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "spy", Bus: &spyBrokerBus{}},
		{Name: "web", Bus: NewWebChannelBus(slog.Default(), f.wcs)},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, f.st, events, func() AgentDispatcher { return f.disp }, slog.Default())
	proxy.webChatStore = f.wcs
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)
	return proxy
}

// upload uploads one file as agent a and returns its id.
func (f *sendFixture) upload(t *testing.T, a *store.Agent, name, content string) string {
	t.Helper()
	return uploadedAttachmentID(t, f.agentUpload(t, a, name, content), name)
}

// jsonRequest builds a JSON POST to path.
func jsonRequest(t *testing.T, path string, body interface{}) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// outbound sends req as agent a on its outbound-message route.
func (f *sendFixture) outbound(t *testing.T, a *store.Agent, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	return f.agentRequest(t, a, jsonRequest(t, "/api/v1/agents/"+a.ID+"/outbound-message", req))
}

// message sends text with attachment ids from agent sender to agent
// target on the target's message route.
func (f *sendFixture) message(t *testing.T, sender, target *store.Agent, text string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.agentRequest(t, sender, jsonRequest(t, "/api/v1/agents/"+target.ID+"/message", f.messageBody(target, text, ids)))
}

func (f *sendFixture) messageBody(target *store.Agent, text string, ids []string) MessageRequest {
	return MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Recipient: "agent:" + target.Slug, RecipientID: target.ID,
			Msg: text, Type: messages.TypeInstruction,
		},
		AttachmentIDs: ids,
	}
}

// requireBound asserts that attachment id is bound to messageID.
func (f *sendFixture) requireBound(t *testing.T, id, messageID string) {
	t.Helper()
	requireMessageScoped(t, f.art, id, messageID)
}

// requireUnbound asserts that attachment id is still an upload.
func (f *sendFixture) requireUnbound(t *testing.T, id string) {
	t.Helper()
	a, err := f.art.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind, "the upload is not bound")
}

// lastDispatch returns the last recorded dispatch to agent a.
func (f *sendFixture) lastDispatch(t *testing.T, a *store.Agent) dispatchCall {
	t.Helper()
	calls := f.disp.getCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Agent != nil && calls[i].Agent.ID == a.ID {
			return calls[i]
		}
	}
	t.Fatalf("no dispatch to %s", a.Slug)
	return dispatchCall{}
}

// messageIDOf decodes the message id of a send answer.
func messageIDOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		MessageID  string `json:"message_id"`
		MessageID2 string `json:"messageId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	if resp.MessageID != "" {
		return resp.MessageID
	}
	require.NotEmpty(t, resp.MessageID2, rec.Body.String())
	return resp.MessageID2
}

// TestAgentSend_BindsOwnUpload_AgentDM: an agent-to-agent send binds the
// sender's upload to the stored message, and the dispatch to the target
// lists it for delivery.
func TestAgentSend_BindsOwnUpload_AgentDM(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "report.txt", "agent bytes")
	rec := f.message(t, f.sender, f.target, "see report", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgID := messageIDOf(t, rec)
	f.requireBound(t, id, msgID)
	call := f.lastDispatch(t, f.target)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	d := call.StructuredMessage.AttachmentDeliveries[0]
	require.Equal(t, id, d.ArtifactID)
	require.Equal(t, msgID, d.MessageID)
	require.Equal(t, "report.txt", d.Name)
	require.NotEmpty(t, d.SHA256)
	a, err := f.art.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, a.MessageProjectID)
	require.Equal(t, "", *a.MessageProjectID, "an agent-agent DM has no project")
}

// TestAgentSend_BindsOwnUpload_Direct: an agent-to-user send on the direct
// path (no broker proxy) binds the upload to the stored row.
func TestAgentSend_BindsOwnUpload_Direct(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "report.txt", "agent bytes")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "for you", AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgID := messageIDOf(t, rec)
	f.requireBound(t, id, msgID)
	_, err := f.st.GetMessage(context.Background(), msgID)
	require.NoError(t, err, "the bound message is the stored row")
}

// TestAgentSend_BindsOwnUpload_Broker: on the broker path the handler
// stores the message itself (decision D2), binds the upload to that row,
// and deliverToUser stores no second row.
func TestAgentSend_BindsOwnUpload_Broker(t *testing.T) {
	f := newSendFixture(t)
	f.withBrokerProxy(t)
	id := f.upload(t, f.sender, "report.txt", "agent bytes")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "for you via broker", AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgID := messageIDOf(t, rec)
	f.requireBound(t, id, msgID)
	require.Equal(t, 1, f.countStored(t, "for you via broker"), "the handler stored the row")
	require.Never(t, func() bool { return f.countStored(t, "for you via broker") != 1 }, 500*time.Millisecond, 25*time.Millisecond,
		"deliverToUser stores no second row")
}

// TestAgentSend_RefusesForeignUpload_UserUpload and _OtherAgentUpload
// (K6): an agent naming a user's upload or another agent's upload gets the
// unknown-id answer, and the upload stays unbound.
func TestAgentSend_RefusesForeignUpload_UserUpload(t *testing.T) {
	f := newSendFixture(t)
	userUpload := f.uploadAs(t, f.ua, f.projA.ID, "user.txt", "user bytes")
	requireUnknownIDAnswer(t, f, userUpload)
	f.requireUnbound(t, userUpload)
}

func TestAgentSend_RefusesForeignUpload_OtherAgentUpload(t *testing.T) {
	f := newSendFixture(t)
	other := f.upload(t, f.target, "other.txt", "other agent bytes")
	requireUnknownIDAnswer(t, f, other)
	f.requireUnbound(t, other)
}

// requireUnknownIDAnswer asserts that the sender naming id on an agent
// send gets the same 400 as for an unknown id.
func requireUnknownIDAnswer(t *testing.T, f *sendFixture, id string) {
	t.Helper()
	unknown := uuid.NewString()
	missing := f.message(t, f.sender, f.target, "x", unknown)
	require.Equal(t, http.StatusBadRequest, missing.Code, missing.Body.String())
	require.Contains(t, missing.Body.String(), "is not available in this conversation")
	got := f.message(t, f.sender, f.target, "x", id)
	require.Equal(t, missing.Code, got.Code, got.Body.String())
	require.Equal(t, strings.ReplaceAll(missing.Body.String(), unknown, "ID"), strings.ReplaceAll(got.Body.String(), id, "ID"))
}

// TestAgentSend_RefusesAlreadyBound: an upload already sent cannot be sent
// again; the answer is that of an unknown id.
func TestAgentSend_RefusesAlreadyBound(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "once.txt", "once")
	require.Equal(t, http.StatusOK, f.message(t, f.sender, f.target, "first", id).Code)
	requireUnknownIDAnswer(t, f, id)
}

// TestAgentSend_UnknownIDSameAnswer: unknown ids, malformed ids and
// foreign uploads share one answer.
func TestAgentSend_UnknownIDSameAnswer(t *testing.T) {
	f := newSendFixture(t)
	requireUnknownIDAnswer(t, f, "not-a-uuid")
}

// TestAgentSend_ExperimentOffRefusesIDs: with the experiment off every id
// is refused like an unknown one, and nothing is bound.
func TestAgentSend_ExperimentOffRefusesIDs(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	setAttachmentExperiments(t, f.srv, false, false)
	requireUnknownIDAnswer(t, f, id)
	f.requireUnbound(t, id)
}

// TestAgentSend_BroadcastRefusesIDs: attachment_ids are refused on a
// project broadcast and on a hub group send.
func TestAgentSend_BroadcastRefusesIDs(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.agentRequest(t, f.sender, jsonRequest(t, "/api/v1/projects/"+f.projA.ID+"/broadcast", BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Msg: "all", Type: messages.TypeInstruction},
		AttachmentIDs: []string{id},
	}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "attachment_ids")

	body := f.messageBody(f.target, "group", []string{id})
	body.StructuredMessage.Recipient = "group[" + f.target.Slug + "," + f.aa.Slug + "]"
	body.StructuredMessage.RecipientID = ""
	rec = f.agentRequest(t, f.sender, jsonRequest(t, "/api/v1/agents/"+f.target.ID+"/message", body))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "attachment_ids")
	f.requireUnbound(t, id)
}

// TestSend_DuplicateAttachmentNamesRefused (K15): two attachments with one
// name are refused, on an agent send (artifact plus artifact) and on web
// send (artifact plus artifact, artifact plus legacy).
func TestSend_DuplicateAttachmentNamesRefused(t *testing.T) {
	f := newSendFixture(t)
	a := f.upload(t, f.sender, "Report.txt", "one")
	b := f.upload(t, f.sender, "report.TXT", "two")
	rec := f.message(t, f.sender, f.target, "two files", a, b)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "two attachments are named")
	f.requireUnbound(t, a)

	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	w1 := f.uploadAs(t, f.ua, "", "notes.txt", "one")
	w2 := f.uploadAs(t, f.ua, "", "NOTES.txt", "two")
	got := f.send(t, f.ua, dm, map[string]interface{}{"content": "x", "attachments": []string{w1, w2}})
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.Contains(t, got.body, "two attachments are named")
	legacy := f.attach(t, "", f.ua.ID, "notes.txt")
	got = f.send(t, f.ua, dm, map[string]interface{}{"content": "x", "attachments": []string{w1, legacy}})
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.Contains(t, got.body, "two attachments are named")
}

// setDeliveryBudget sets messaging.attachment_delivery_max_bytes.
func setDeliveryBudget(t *testing.T, srv *Server, n int64) {
	t.Helper()
	srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"attachment_delivery_max_bytes": `+strings.TrimSpace(jsonInt(n))+`}`))
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestSend_OverDeliveryBudgetRefused (K25): an agent send whose
// attachments exceed the delivery budget is refused at send time, and
// nothing is bound; with the budget at 0, delivery is off.
func TestSend_OverDeliveryBudgetRefused(t *testing.T) {
	f := newSendFixture(t)
	id := f.upload(t, f.sender, "big.txt", "0123456789")
	setDeliveryBudget(t, f.srv, 5)
	rec := f.message(t, f.sender, f.target, "big", id)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the limit for delivery to agents is")
	f.requireUnbound(t, id)
	setDeliveryBudget(t, f.srv, 0)
	rec = f.message(t, f.sender, f.target, "off", id)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "attachment delivery is off")
}

// TestWebSend_OverDeliveryBudgetRefused: a web send that reaches an agent
// is refused over the budget; a send between users is not.
func TestWebSend_OverDeliveryBudgetRefused(t *testing.T) {
	f := newSendFixture(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	id := f.uploadAs(t, f.ua, f.projA.ID, "big.txt", "0123456789")
	setDeliveryBudget(t, f.srv, 5)
	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "big", "attachments": []string{id}})
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.Contains(t, got.body, "the limit for delivery to agents is")
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)
	other := f.uploadAs(t, f.ua, "", "big2.txt", "0123456789")
	f.sendWith(t, f.ua, dm, other)
}

// TestHumanCLISend_SessionBindsOwnUpload: a session user sends
// attachment_ids through POST /api/v1/agents/{id}/message: the upload is
// bound, the dispatch lists it for delivery, and another user's upload is
// refused like an unknown id.
func TestHumanCLISend_SessionBindsOwnUpload(t *testing.T) {
	f := newSendFixture(t)
	id := f.uploadAs(t, f.ua, f.projA.ID, "cli.txt", "from the cli")
	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", f.messageBody(f.target, "from cli", []string{id}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	msgID := messageIDOf(t, rec)
	f.requireBound(t, id, msgID)
	call := f.lastDispatch(t, f.target)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, id, call.StructuredMessage.AttachmentDeliveries[0].ArtifactID)

	foreign := f.uploadAs(t, f.ub, f.projB.ID, "bob.txt", "bob")
	unknown := uuid.NewString()
	missing := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", f.messageBody(f.target, "x", []string{unknown}))
	got := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", f.messageBody(f.target, "x", []string{foreign}))
	require.Equal(t, http.StatusBadRequest, missing.Code, missing.Body.String())
	require.Equal(t, missing.Code, got.Code)
	require.Equal(t, strings.ReplaceAll(missing.Body.String(), unknown, "ID"), strings.ReplaceAll(got.Body.String(), foreign, "ID"))
}

// TestWebSend_SetsDeliveries: a web send to an agent sets the bound
// attachments on the dispatched message (unit check of the bind site).
func TestWebSend_SetsDeliveries(t *testing.T) {
	f := newSendFixture(t)
	require.NoError(t, f.wcs.UpdateTopic(context.Background(), f.topicA, TopicUpdate{DefaultAgent: &f.target.Slug}))
	id := f.uploadAs(t, f.ua, f.projA.ID, "web.txt", "web bytes")
	resp := f.sendWith(t, f.ua, f.topicA, id)
	call := f.lastDispatch(t, f.target)
	require.Len(t, call.StructuredMessage.AttachmentDeliveries, 1)
	require.Equal(t, id, call.StructuredMessage.AttachmentDeliveries[0].ArtifactID)
	require.Equal(t, resp.ID, call.StructuredMessage.AttachmentDeliveries[0].MessageID)
}

// TestSanitizeCrossProjectObserver_ClearsDeliveries: observer copies carry
// no delivery list.
func TestSanitizeCrossProjectObserver_ClearsDeliveries(t *testing.T) {
	msg := &messages.StructuredMessage{AttachmentDeliveries: []messages.AttachmentDelivery{{ArtifactID: "a"}}}
	sanitizeCrossProjectObserver(msg)
	require.Nil(t, msg.AttachmentDeliveries)
}

// countStored counts the stored messages whose body is text.
func (f *sendFixture) countStored(t *testing.T, text string) int {
	t.Helper()
	res, err := f.st.ListMessages(context.Background(), store.MessageFilter{}, store.ListOptions{Limit: 500})
	require.NoError(t, err)
	n := 0
	for _, m := range res.Items {
		if m.Msg == text {
			n++
		}
	}
	return n
}

// sendResponse decodes the chat send answer got.
func (f *sendFixture) sendResponse(t *testing.T, got refAnswer) chatMessageResponse {
	t.Helper()
	var resp chatMessageResponse
	require.NoError(t, json.Unmarshal([]byte(got.body), &resp), got.body)
	return resp
}

// TestAgentSend_CrossProjectIDsAllowedPathsRefused: where messaging allows
// a cross-project send, attachment ids are accepted (they carry no host
// path) while path attachments keep the cross-project refusal
// (ptone/scion#1687).
func TestAgentSend_CrossProjectIDsAllowedPathsRefused(t *testing.T) {
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
	sender := mk("hubsnd", f.projA.ID, f.ua.ID)
	other := mk("bee", f.projB.ID, f.ub.ID)
	enableCPM(t, f.srv, f.st)

	id := f.upload(t, sender, "cross.txt", "cross-project bytes")
	rec := f.message(t, sender, other, "across projects", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireBound(t, id, messageIDOf(t, rec))

	body := f.messageBody(other, "with a path", nil)
	body.StructuredMessage.Attachments = []string{"/scion-volumes/scratchpad/.attachments/x/y/file.txt"}
	rec = f.agentRequest(t, sender, jsonRequest(t, "/api/v1/agents/"+other.ID+"/message", body))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "cross-project attachment")
}

// TestSend_OverDeliveryBudgetRefused_MentionOnUserSend: an agent send to a
// user whose body @mentions an agent reaches that agent through a mention
// copy, so it must fit the delivery budget; the same send without an agent
// mention does not.
func TestSend_OverDeliveryBudgetRefused_MentionOnUserSend(t *testing.T) {
	f := newSendFixture(t)
	setDeliveryBudget(t, f.srv, 5)
	id := f.upload(t, f.sender, "big.txt", "0123456789")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "for you and @" + f.target.Slug,
		AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the limit for delivery to agents is")
	f.requireUnbound(t, id)
	rec = f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "for you only",
		AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestHumanCLISend_OverDeliveryBudgetRefused: the human CLI site checks
// the delivery budget too.
func TestHumanCLISend_OverDeliveryBudgetRefused(t *testing.T) {
	f := newSendFixture(t)
	setDeliveryBudget(t, f.srv, 5)
	id := f.uploadAs(t, f.ua, f.projA.ID, "big.txt", "0123456789")
	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", f.messageBody(f.target, "big", []string{id}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the limit for delivery to agents is")
}

// TestAttachmentProbe_DeliveryBudget: the probe's artifact answer carries
// the delivery budget.
func TestAttachmentProbe_DeliveryBudget(t *testing.T) {
	f := newSendFixture(t)
	setDeliveryBudget(t, f.srv, 4096)
	rec := doRequestAsUser(t, f.srv, f.ua, http.MethodGet, chatAttachmentsPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got attachmentUploadProbe
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "artifact", got.Uploads)
	require.NotNil(t, got.DeliveryMaxBytes)
	require.Equal(t, int64(4096), *got.DeliveryMaxBytes)
}
