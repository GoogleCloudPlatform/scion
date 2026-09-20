//go:build !no_sqlite

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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Foreign attachment limits and publication boundary tests (#1687)
//
// Acceptance criteria:
//   AC-1: Foreign attachment sends produce a clear denial before blobs,
//         message rows, resume or recipient publication; same-project
//         attachments retain their behavior.
//   AC-2: Allowed foreign text DMs reach the intended agent; unrelated
//         members of either project receive no body, preview or attachment
//         metadata through tested event/observer sinks.
//   AC-3: Observer publication cannot cause a second agent dispatch.
//   AC-4: Any required management-view classification is actually wired
//         and tested.
//   AC-5: Same-project native and legacy group publication still works.
// ---------------------------------------------------------------------------

// spyBrokerBus records all published messages for inspection.
type spyBrokerBus struct {
	mu     sync.Mutex
	events []spyBrokerEvent
}

type spyBrokerEvent struct {
	topic string
	msg   *messages.StructuredMessage
}

func (b *spyBrokerBus) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Deep copy the message to prevent mutation after capture.
	cp := *msg
	if msg.Metadata != nil {
		cp.Metadata = make(map[string]string, len(msg.Metadata))
		for k, v := range msg.Metadata {
			cp.Metadata[k] = v
		}
	}
	if msg.Attachments != nil {
		cp.Attachments = make([]string, len(msg.Attachments))
		copy(cp.Attachments, msg.Attachments)
	}
	b.events = append(b.events, spyBrokerEvent{topic: topic, msg: &cp})
	return nil
}

func (b *spyBrokerBus) Subscribe(_ string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	return &spyNoopSub{}, nil
}

func (b *spyBrokerBus) Close() error { return nil }

func (b *spyBrokerBus) getEvents() []spyBrokerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]spyBrokerEvent, len(b.events))
	copy(result, b.events)
	return result
}

type spyNoopSub struct{}

func (s *spyNoopSub) Unsubscribe() error { return nil }

// foreignAttachSetup creates two projects, two agents (one per project),
// a DM conversation between them, and enables cross-project messaging.
func foreignAttachSetup(t *testing.T) (
	srv *Server, s store.Store,
	projectA, projectB *store.Project,
	agentA, agentB *store.Agent,
	convID string, dispatcher *recordingDispatcher,
	spyBus *spyBrokerBus,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("fa-owner"),
		Email:   "fa-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	projectA = &store.Project{
		ID:        tid("fa-project-a"),
		Name:      "fa-project-a",
		Slug:      "fa-project-a",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB = &store.Project{
		ID:   tid("fa-project-b"),
		Name: "fa-project-b",
		Slug: "fa-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("fa-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fa-broker",
		Slug:   "fa-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA = &store.Agent{
		ID:              tid("fa-agent-a"),
		Name:            "fa-agent-a",
		Slug:            "fa-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB = &store.Agent{
		ID:              tid("fa-agent-b"),
		Name:            "fa-agent-b",
		Slug:            "fa-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation between the agents.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	convID = conv.ID

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Set up a spy broker bus so we can capture observer publications.
	spyBus = &spyBrokerBus{}
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "spy", Bus: spyBus},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	enableCPM(t, srv, s)

	return srv, s, projectA, projectB, agentA, agentB, convID, dispatcher, spyBus
}

// sendOutboundDMWithAttachments sends an outbound message with optional attachments.
func sendOutboundDMWithAttachments(t *testing.T, srv *Server, sender *store.Agent, convID, projectID, msgText string, attachments []string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msgText,
		Type:            "instruction",
		Attachments:     attachments,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// sendInboundAgentMessage sends a message through the inbound handleAgentMessage path.
func sendInboundAgentMessage(t *testing.T, srv *Server, senderAgent, targetAgent *store.Agent, msgText string, attachments []string) *httptest.ResponseRecorder {
	t.Helper()

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + senderAgent.Slug,
		SenderID:    senderAgent.ID,
		Recipient:   "agent:" + targetAgent.Slug,
		RecipientID: targetAgent.ID,
		Msg:         msgText,
		Attachments: attachments,
	}

	reqBody, err := json.Marshal(MessageRequest{
		StructuredMessage: sm,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: senderAgent.ID},
		ProjectID: senderAgent.ProjectID,
		Ancestry:  senderAgent.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, targetAgent.ID)
	return rr
}

// ---------------------------------------------------------------------------
// AC-1: Foreign attachment sends denied before side effects
// ---------------------------------------------------------------------------

func TestForeignAttach_OutboundPath_CrossProject_Denied(t *testing.T) {
	srv, s, projectA, _, agentA, _, convID, dispatcher, _ := foreignAttachSetup(t)

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"cross-project with attachment", []string{"/tmp/secret.txt"})

	// Must be rejected with 422 and the unsupported_capability code.
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"cross-project attachment DM must be rejected; body: %s", rr.Body.String())

	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeUnsupportedCapability, resp.Error.Code)
	assert.Equal(t, string(MessageDenialCrossProjectAttachUnsupported), resp.Error.Details["reason"])

	// Zero message rows.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected send must produce zero message rows")

	// Zero dispatch calls.
	assert.Empty(t, dispatcher.getCalls(), "rejected send must produce zero dispatch calls")
}

func TestForeignAttach_InboundPath_CrossProject_Denied(t *testing.T) {
	srv, s, _, _, agentA, agentB, _, _, _ := foreignAttachSetup(t)

	rr := sendInboundAgentMessage(t, srv, agentA, agentB,
		"inbound with attachment", []string{"/tmp/secret.txt"})

	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"cross-project attachment via inbound path must be rejected; body: %s", rr.Body.String())

	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeUnsupportedCapability, resp.Error.Code)
	assert.Equal(t, string(MessageDenialCrossProjectAttachUnsupported), resp.Error.Details["reason"])

	// Zero message rows.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected send must produce zero message rows")
}

// ---------------------------------------------------------------------------
// AC-1 (positive): Same-project attachments retained
// ---------------------------------------------------------------------------

func TestForeignAttach_SameProject_Attachments_Allowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("fa-same-proj"),
		Name: "fa-same-proj",
		Slug: "fa-same-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("fa-same-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fa-same-broker",
		Slug:   "fa-same-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender := &store.Agent{
		ID:              tid("fa-same-sender"),
		Name:            "same-sender",
		Slug:            "same-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target := &store.Agent{
		ID:              tid("fa-same-target"),
		Name:            "same-target",
		Slug:            "same-target",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	rr := sendOutboundDMWithAttachments(t, srv, sender, conv.ID, project.ID,
		"same-project with attachment", []string{"/tmp/file.txt"})

	assert.Equal(t, http.StatusOK, rr.Code,
		"same-project attachment DM must succeed; body: %s", rr.Body.String())

	// Message persisted.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "same-project with attachment" {
			found = true
			break
		}
	}
	assert.True(t, found, "same-project attachment DM must be persisted")
}

// ---------------------------------------------------------------------------
// AC-2: Foreign text DMs reach the agent; observer messages stripped
// ---------------------------------------------------------------------------

func TestForeignAttach_TextDM_Reaches_Agent(t *testing.T) {
	srv, _, projectA, _, agentA, _, convID, dispatcher, _ := foreignAttachSetup(t)

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"text-only cross-project DM", nil)

	require.Equal(t, http.StatusOK, rr.Code,
		"text-only cross-project DM must succeed; body: %s", rr.Body.String())

	// Dispatch must have been called (target agent should receive the message).
	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call for text DM")
}

func TestForeignAttach_ObserverMessage_BodyStripped_Outbound(t *testing.T) {
	srv, _, projectA, _, agentA, agentB, convID, _, spyBus := foreignAttachSetup(t)

	// Subscribe the target project's agent so the broker actually receives the message.
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.subscribeAgent(agentB.ProjectID, agentB.Slug)
	}

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"secret cross-project body", nil)
	require.Equal(t, http.StatusOK, rr.Code,
		"text-only cross-project DM must succeed; body: %s", rr.Body.String())

	// Give async publication time to land.
	time.Sleep(200 * time.Millisecond)

	// Check spy bus events for observer messages.
	events := spyBus.getEvents()
	for _, evt := range events {
		if evt.msg != nil && evt.msg.ObserverOnly {
			// Body must be stripped for cross-project observer messages.
			assert.Empty(t, evt.msg.Msg,
				"cross-project observer message must have empty body")
			assert.Empty(t, evt.msg.Attachments,
				"cross-project observer message must have no attachments")
			// The attachments metadata key must also be absent.
			if evt.msg.Metadata != nil {
				_, hasAttachKey := evt.msg.Metadata[messages.AttachmentsMetadataKey]
				assert.False(t, hasAttachKey,
					"cross-project observer message must not carry attachment metadata")
			}
		}
	}
}

func TestForeignAttach_SameProject_ObserverMessage_BodyPreserved(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("fa-sp-obs-proj"),
		Name: "fa-sp-obs-proj",
		Slug: "fa-sp-obs-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("fa-sp-obs-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fa-sp-obs-broker",
		Slug:   "fa-sp-obs-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender := &store.Agent{
		ID:              tid("fa-sp-obs-sender"),
		Name:            "sp-obs-sender",
		Slug:            "sp-obs-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target := &store.Agent{
		ID:              tid("fa-sp-obs-target"),
		Name:            "sp-obs-target",
		Slug:            "sp-obs-target",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	spyBus := &spyBrokerBus{}
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		// Register the spy as "web" so it receives messages routed to the
		// "web" channel (which the DM backfill sets on agent DMs).
		{Name: "web", Bus: spyBus},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeAgent(project.ID, target.Slug)

	rr := sendOutboundDMWithAttachments(t, srv, sender, conv.ID, project.ID,
		"same-project body preserved", nil)
	require.Equal(t, http.StatusOK, rr.Code,
		"same-project DM must succeed; body: %s", rr.Body.String())

	// Give async publication time to land.
	time.Sleep(200 * time.Millisecond)

	// Check spy bus events — same-project observer messages keep the body.
	events2 := spyBus.getEvents()
	var foundObserver bool
	for _, evt := range events2 {
		if evt.msg != nil && evt.msg.ObserverOnly {
			foundObserver = true
			assert.Equal(t, "same-project body preserved", evt.msg.Msg,
				"same-project observer message must preserve body")
		}
	}
	assert.True(t, foundObserver, "expected at least one observer publication")
}

// ---------------------------------------------------------------------------
// AC-3: Observer publication cannot cause a second agent dispatch
// ---------------------------------------------------------------------------

func TestForeignAttach_ObserverOnly_Prevents_Dispatch(t *testing.T) {
	srv, _, projectA, _, agentA, agentB, convID, dispatcher, _ := foreignAttachSetup(t)

	// Subscribe the target agent in the broker so publication reaches it.
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.subscribeAgent(agentB.ProjectID, agentB.Slug)
	}

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"observer no double dispatch", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	// Give async publication time to land.
	time.Sleep(200 * time.Millisecond)

	// The direct dispatch via dispatchWithBrokerRetry counts as 1 call.
	// ObserverOnly publication must NOT produce a second dispatch.
	calls := dispatcher.getCalls()
	assert.LessOrEqual(t, len(calls), 1,
		"observer publication must not cause a second agent dispatch; got %d calls", len(calls))
}

// ---------------------------------------------------------------------------
// AC-4: ClassifyLegacyViewQuery is wired and tested
// ---------------------------------------------------------------------------

func TestClassifyLegacyViewQuery_CrossProject_Canonical(t *testing.T) {
	senderProjID := "project-sender"
	recipientProjID := "project-recipient"
	msg := &store.Message{
		SenderProjectID:    &senderProjID,
		RecipientProjectID: &recipientProjID,
	}
	got := ClassifyLegacyViewQuery(msg)
	assert.Equal(t, LegacyViewCanonical, got,
		"cross-project messages must use canonical view mode")
}

func TestClassifyLegacyViewQuery_SameProject_Legacy(t *testing.T) {
	projID := "same-project"
	msg := &store.Message{
		SenderProjectID:    &projID,
		RecipientProjectID: &projID,
	}
	got := ClassifyLegacyViewQuery(msg)
	assert.Equal(t, LegacyViewLegacy, got,
		"same-project messages must use legacy view mode")
}

func TestClassifyLegacyViewQuery_WithConversationID_Canonical(t *testing.T) {
	msg := &store.Message{
		ConversationID: "some-conv-id",
	}
	got := ClassifyLegacyViewQuery(msg)
	assert.Equal(t, LegacyViewCanonical, got,
		"messages with canonical conversation IDs must use canonical view mode")
}

func TestClassifyLegacyViewQuery_NilMessage_Legacy(t *testing.T) {
	got := ClassifyLegacyViewQuery(nil)
	assert.Equal(t, LegacyViewLegacy, got,
		"nil message must use legacy view mode")
}

// ---------------------------------------------------------------------------
// AC-4 (integration): Interagent view wires ClassifyLegacyViewQuery
// ---------------------------------------------------------------------------

func TestInteragentView_CrossProject_BodyStripped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Use the dev user (DevUserID) as the viewer — the dev auth path gives
	// the dev user project management access, which satisfies ActionAttach.
	projectA := &store.Project{
		ID:        tid("ia-proj-a"),
		Name:      "ia-proj-a",
		Slug:      "ia-proj-a",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("ia-proj-b"),
		Name: "ia-proj-b",
		Slug: "ia-proj-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	agentA := &store.Agent{
		ID:          tid("ia-agent-a"),
		Name:        "ia-agent-a",
		Slug:        "ia-agent-a",
		ProjectID:   projectA.ID,
		Phase:       "running",
		Visibility:  store.VisibilityPrivate,
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:          tid("ia-agent-b"),
		Name:        "ia-agent-b",
		Slug:        "ia-agent-b",
		ProjectID:   projectB.ID,
		Phase:       "running",
		Visibility:  store.VisibilityPrivate,
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create user DM key with the dev user viewing agentA's inter-agent messages.
	userDMKey, err := messages.DMConversationKey("user", DevUserID, "agent", agentA.ID)
	require.NoError(t, err)

	// Create the cross-project conversation between agents.
	agentDMKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: agentDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Register the agents as participants.
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    agentA.ID,
		Role:           "member",
	}))
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    agentB.ID,
		Role:           "member",
	}))

	// Create a second agent in projectA for same-project messaging.
	agentA2 := &store.Agent{
		ID:          tid("ia-agent-a2"),
		Name:        "ia-agent-a2",
		Slug:        "ia-agent-a2",
		ProjectID:   projectA.ID,
		Phase:       "running",
		Visibility:  store.VisibilityPrivate,
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA2))

	// Persist a cross-project inter-agent message.
	senderProjID := projectA.ID
	recipientProjID := projectB.ID
	crossProjMsg := &store.Message{
		ID:                 tid("ia-msg-cross"),
		ProjectID:          projectB.ID,
		Sender:             "agent:" + agentA.Slug,
		SenderID:           agentA.ID,
		Recipient:          "agent:" + agentB.Slug,
		RecipientID:        agentB.ID,
		Msg:                "cross-project secret body",
		Type:               "instruction",
		AgentID:            agentA.ID, // ParticipantID filter matches on agentA
		ConversationID:     conv.ID,
		SenderProjectID:    &senderProjID,
		RecipientProjectID: &recipientProjID,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateMessage(ctx, crossProjMsg))

	// Persist a same-project inter-agent message (agentA ↔ agentA2, both in
	// projectA). The viewer owns projectA, so the body must be preserved.
	sameProjConvKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentA2.ID)
	require.NoError(t, err)
	sameProjConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: sameProjConvKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: sameProjConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    agentA.ID,
		Role:           "member",
	}))
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: sameProjConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    agentA2.ID,
		Role:           "member",
	}))
	sameProjMsg := &store.Message{
		ID:              tid("ia-msg-same"),
		ProjectID:       projectA.ID,
		Sender:          "agent:" + agentA.Slug,
		SenderID:        agentA.ID,
		Recipient:       "agent:" + agentA2.Slug,
		RecipientID:     agentA2.ID,
		Msg:             "same-project visible body",
		Type:            "instruction",
		AgentID:         agentA.ID,
		ConversationID:  sameProjConv.ID,
		SenderProjectID: &senderProjID,
		CreatedAt:       time.Now(),
	}
	require.NoError(t, s.CreateMessage(ctx, sameProjMsg))

	enableCPM(t, srv, s)

	// The viewer (dev user) requests the interagent view via the full router.
	// DevUserID is NOT a participant in the cross-project agent DM, so the
	// body must be stripped by ClassifyLegacyViewQuery + AuthorizeCrossProjectContentAccess.
	endpoint := "/api/v1/chat/conversations/" + url.PathEscape(userDMKey) + "/interagent"
	rr := doRequest(t, srv, http.MethodGet, endpoint, nil)

	require.Equal(t, http.StatusOK, rr.Code,
		"interagent request must succeed; body: %s", rr.Body.String())

	var iaResp interagentResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &iaResp))

	for _, m := range iaResp.Messages {
		if m.ID == crossProjMsg.ID {
			assert.Empty(t, m.Msg,
				"cross-project message body must be stripped in interagent view for non-participant viewer")
		}
		if m.ID == sameProjMsg.ID {
			assert.Equal(t, "same-project visible body", m.Msg,
				"same-project message body must be preserved in interagent view")
		}
	}
}

// ---------------------------------------------------------------------------
// AC-5: Same-project group publication still works
// ---------------------------------------------------------------------------

func TestForeignAttach_SameProject_GroupPublish_Works(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("fa-grp-proj"),
		Name: "fa-grp-proj",
		Slug: "fa-grp-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("fa-grp-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fa-grp-broker",
		Slug:   "fa-grp-broker",
		Status: store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              tid("fa-grp-agent"),
		Name:            "fa-grp-agent",
		Slug:            "fa-grp-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	user := &store.User{
		ID:      tid("fa-grp-user"),
		Email:   "fa-grp-user@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Create group (legacy thread) conversation.
	threadID := tid("fa-grp-thread")
	projID := project.ID
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":" + threadID,
		ProjectID:   &projID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Send a group message via the outbound path.
	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		Msg:             "same-project group message",
		Type:            "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	assert.Equal(t, http.StatusOK, rr.Code,
		"same-project group message must succeed; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// AC-1 (edge): Text-only cross-project DM on outbound path succeeds
// ---------------------------------------------------------------------------

func TestForeignAttach_OutboundPath_CrossProject_TextOnly_Allowed(t *testing.T) {
	srv, s, projectA, _, agentA, _, convID, _, _ := foreignAttachSetup(t)

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"text-only cross-project DM", nil)

	require.Equal(t, http.StatusOK, rr.Code,
		"text-only cross-project DM must succeed; body: %s", rr.Body.String())

	// Message persisted.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "text-only cross-project DM" {
			found = true
			break
		}
	}
	assert.True(t, found, "text-only cross-project DM must be persisted")
}

// ---------------------------------------------------------------------------
// AC-1 (edge): Empty attachment list is not a rejection
// ---------------------------------------------------------------------------

func TestForeignAttach_EmptyAttachmentList_Allowed(t *testing.T) {
	srv, _, projectA, _, agentA, _, convID, _, _ := foreignAttachSetup(t)

	rr := sendOutboundDMWithAttachments(t, srv, agentA, convID, projectA.ID,
		"empty attachment list", []string{})

	require.Equal(t, http.StatusOK, rr.Code,
		"empty attachment list must not trigger rejection; body: %s", rr.Body.String())
}
