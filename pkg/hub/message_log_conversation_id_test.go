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

// ---------------------------------------------------------------------------
// Regression tests for ptone/scion#1635: the dedicated message audit log
// (Cloud Logging "scion-messages" stream) omitted conversation_id even when
// the conversation had already been resolved and persisted. Each test below
// exercises a distinct dedicated-log call site and asserts the resolved
// conversation_id is present in the emitted record.
// ---------------------------------------------------------------------------

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

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCapturingMessageLogger returns a slog.Logger that writes text-format
// records into the returned buffer, suitable for wiring into
// Server.SetMessageLogger or MessageBrokerProxy.messageLog in tests.
func newCapturingMessageLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &buf
}

// assertLogHasConversationID fails the test unless a log line containing
// msgSubstr also carries conversation_id=wantConvID.
func assertLogHasConversationID(t *testing.T, buf *bytes.Buffer, msgSubstr, wantConvID string) {
	t.Helper()
	require.NotEmpty(t, wantConvID, "test bug: wantConvID must not be empty")
	output := buf.String()
	var matchLine string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, msgSubstr) {
			matchLine = line
			break
		}
	}
	require.NotEmpty(t, matchLine, "expected a dedicated-log line containing %q, got:\n%s", msgSubstr, output)
	assert.Contains(t, matchLine, "conversation_id="+wantConvID,
		"dedicated-log line for %q must carry the resolved conversation_id:\n%s", msgSubstr, matchLine)
}

// ---------------------------------------------------------------------------
// handlers_broker_inbound.go: "inbound broker message delivered"
// ---------------------------------------------------------------------------

func TestDedicatedLog_InboundBrokerMessageDelivered_HasConversationID(t *testing.T) {
	f := setupDEF135(t)
	logger, buf := newCapturingMessageLogger()
	f.srv.SetMessageLogger(logger)

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		Sender:    f.senderRef,
		Recipient: "agent:" + f.agent.Slug,
		Msg:       "hello from discord DM",
		Type:      messages.TypeInstruction,
	}

	rec := f.sendBrokerInbound(t, msg, "", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	msgs, err := f.store.ListMessages(context.Background(), store.MessageFilter{
		AgentID: f.agent.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(msgs.Items), 1)
	require.NotEmpty(t, msgs.Items[0].ConversationID)

	assertLogHasConversationID(t, buf, "inbound broker message delivered", msgs.Items[0].ConversationID)
}

// ---------------------------------------------------------------------------
// handlers_broker_inbound_routed.go: "routed inbound message delivered"
// ---------------------------------------------------------------------------

func TestDedicatedLog_RoutedInboundMessageDelivered_HasConversationID(t *testing.T) {
	env := setupRoutedTestEnv(t)
	logger, buf := newCapturingMessageLogger()
	env.srv.SetMessageLogger(logger)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "conversation test",
			Type:    messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		AgentID: env.agent1.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(msgs.Items), 1)
	require.NotEmpty(t, msgs.Items[0].ConversationID)

	assertLogHasConversationID(t, buf, "routed inbound message delivered", msgs.Items[0].ConversationID)
}

// ---------------------------------------------------------------------------
// handlers_agent_messaging.go: "outbound message sent"
// ---------------------------------------------------------------------------

func TestDedicatedLog_OutboundMessageSent_HasConversationID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	threadRef := "thread:" + project.ID + ":test-thread-log-conv"
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	logger, buf := newCapturingMessageLogger()
	srv.SetMessageLogger(logger)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "reply to thread", conv.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	assertLogHasConversationID(t, buf, "outbound message sent", conv.ID)
}

// ---------------------------------------------------------------------------
// agent_dm_operation.go: "agent DM: message dispatched"
// ---------------------------------------------------------------------------

func TestDedicatedLog_AgentDMMessageDispatched_HasConversationID(t *testing.T) {
	srv, _, _, sender, _, convID, _, _ := paritySetup(t)

	logger, buf := newCapturingMessageLogger()
	srv.SetMessageLogger(logger)

	rr := sendViaOutbound(t, srv, sender, convID, "parity-outbound-log-test")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	assertLogHasConversationID(t, buf, "agent DM: message dispatched", convID)
}

// ---------------------------------------------------------------------------
// messagebroker.go: "broker message delivered" (deliverToAgent)
// ---------------------------------------------------------------------------

func TestDedicatedLog_BrokerMessageDelivered_HasConversationID(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "log-conv-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	logger, buf := newCapturingMessageLogger()
	proxy.messageLog = logger
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "log-conv-agent")

	msg := messages.NewInstruction("user:alice", "agent:log-conv-agent", "persist this")
	msg.SenderID = tid("user-alice-log-conv")
	msg.RecipientID = agent.ID
	require.NoError(t, proxy.PublishMessage(context.Background(), projectID, msg))

	time.Sleep(100 * time.Millisecond)

	result, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.NotEmpty(t, result.Items[0].ConversationID)

	assertLogHasConversationID(t, buf, "broker message delivered", result.Items[0].ConversationID)
}

// ---------------------------------------------------------------------------
// messagebroker.go: "user message delivered via broker" (deliverToUser)
// ---------------------------------------------------------------------------

func TestDedicatedLog_UserMessageDeliveredViaBroker_HasConversationID(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "log-conv-sender", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	logger, buf := newCapturingMessageLogger()
	proxy.messageLog = logger
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeProjectUserMessages(projectID)

	userID := tid("user-bob-log-conv")
	msg := messages.NewInstruction("agent:log-conv-sender", "user:bob", "question for you")
	msg.SenderID = tid("agent-log-conv-sender")
	msg.RecipientID = userID

	require.NoError(t, proxy.PublishUserMessage(context.Background(), projectID, userID, msg))

	time.Sleep(100 * time.Millisecond)

	result, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: userID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.NotEmpty(t, result.Items[0].ConversationID)

	assertLogHasConversationID(t, buf, "user message delivered via broker", result.Items[0].ConversationID)
}

// ---------------------------------------------------------------------------
// pkg/messages LogAttrs() sanity check at the hub integration layer: a
// caller-asserted conversation_id must survive into the "message dispatched"
// dedicated log line emitted by handleAgentMessage, which logs via
// structuredMsg.LogAttrs() before Phase 5 derivation runs.
// ---------------------------------------------------------------------------

func TestDedicatedLog_MessageDispatched_CallerAssertedConversationID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	threadRef := "thread:" + project.ID + ":test-thread-msgdispatch"
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	logger, buf := newCapturingMessageLogger()
	srv.SetMessageLogger(logger)

	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Type:           messages.TypeInstruction,
		Sender:         "user:" + user.Email,
		Recipient:      "agent:" + agent.Slug,
		Msg:            "hi from the web UI",
		ConversationID: conv.ID,
	}
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "cli")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agent.ID)
	// No dispatcher is wired in this fixture, so delivery fails downstream —
	// the audit log line under test is emitted before that point.
	_ = rr

	assertLogHasConversationID(t, buf, "message dispatched", conv.ID)
}
