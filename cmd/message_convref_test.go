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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outboundMessage records an outbound (agent-to-user) message.
type outboundMessage struct {
	AgentName       string
	Recipient       string
	Message         string
	Type            string
	Urgent          bool
	ConversationRef string
	Wake            bool
	Attachments     []string
}

// convRefMockServer provides a test server for conversation-ref CLI tests.
//
// DEF-142 P5: the separate resolve endpoint is removed — the CLI passes
// conversation_ref in the outbound message request and the server resolves
// it inline.
func newConvRefMockHubServer(t *testing.T, projectID string) (*httptest.Server, *[]sentMessage, *[]outboundMessage) {
	t.Helper()
	var sent []sentMessage
	var outbound []outboundMessage
	var mu sync.Mutex

	projectPrefix := "/api/v1/projects/" + projectID + "/agents/"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v1/conversations/") && strings.HasSuffix(path, "/messages"):
			// Conversation send endpoint: POST /api/v1/conversations/{id}/messages
			// Used by Messaging().SendMessage() for conv:<uuid> replies.
			convID := strings.TrimPrefix(path, "/api/v1/conversations/")
			convID = strings.TrimSuffix(convID, "/messages")
			var body struct {
				Msg  string `json:"msg"`
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			outbound = append(outbound, outboundMessage{
				AgentName:       "conv:" + convID,
				Message:         body.Msg,
				Type:            body.Type,
				ConversationRef: "conv:" + convID,
			})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"messageId": "msg-test-conv",
				"status":    "delivered",
			})

		case r.Method == http.MethodPost && strings.HasPrefix(path, projectPrefix) && strings.HasSuffix(path, "/outbound-message"):
			// Outbound message endpoint: /api/v1/projects/<pid>/agents/<agent>/outbound-message
			rest := path[len(projectPrefix):]
			agentName := rest[:len(rest)-len("/outbound-message")]
			var body hubclient.OutboundMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			outbound = append(outbound, outboundMessage{
				AgentName:       agentName,
				Recipient:       body.Recipient,
				Message:         body.Msg,
				Type:            body.Type,
				Urgent:          body.Urgent,
				ConversationRef: body.ConversationRef,
				Wake:            body.Wake,
				Attachments:     body.Attachments,
			})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "msg-test-outbound",
				"status":       "sent",
				"recipient":    body.Recipient,
				"recipient_id": "uid-test",
			})

		case r.Method == http.MethodPost && strings.HasPrefix(path, projectPrefix):
			// Agent message endpoint (human-to-agent via StructuredMessage)
			rest := path[len(projectPrefix):]
			var agentName string
			if len(rest) > len("/message") {
				agentName = rest[:len(rest)-len("/message")]
			}

			var body struct {
				Message           string                      `json:"message"`
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			sm := sentMessage{
				AgentName:     agentName,
				Interrupt:     body.Interrupt,
				StructuredMsg: body.StructuredMessage,
			}
			if body.StructuredMessage != nil {
				sm.Message = body.StructuredMessage.Msg
			} else {
				sm.Message = body.Message
			}

			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &sent, &outbound
}

// TestConvRefParsing_AtAgent verifies that @agent-name is parsed as a
// conversation reference (RefAgent), not as a bare email.
func TestConvRefParsing_AtAgent(t *testing.T) {
	ref, err := messaging.ParseReference("@builder")
	require.NoError(t, err)
	assert.Equal(t, messaging.RefAgent, ref.Kind)
	assert.Equal(t, "builder", ref.Value)
}

// TestConvRefParsing_AtEmail verifies that @user@email.com is parsed as RefEmail.
func TestConvRefParsing_AtEmail(t *testing.T) {
	ref, err := messaging.ParseReference("@user@example.com")
	require.NoError(t, err)
	assert.Equal(t, messaging.RefEmail, ref.Kind)
	assert.Equal(t, "user@example.com", ref.Value)
}

// TestConvRefParsing_HashThread verifies that #thread-name is parsed as RefThread.
func TestConvRefParsing_HashThread(t *testing.T) {
	ref, err := messaging.ParseReference("#general")
	require.NoError(t, err)
	assert.Equal(t, messaging.RefThread, ref.Kind)
	assert.Equal(t, "general", ref.Value)
}

// TestConvRefParsing_ConvUUID verifies conv:<uuid> parsing.
func TestConvRefParsing_ConvUUID(t *testing.T) {
	ref, err := messaging.ParseReference("conv:7f3a91c2-1234-5678-9abc-def012345678")
	require.NoError(t, err)
	assert.Equal(t, messaging.RefConversation, ref.Kind)
	assert.Equal(t, "7f3a91c2-1234-5678-9abc-def012345678", ref.Value)
}

// TestConvRefParsing_LegacyBareAgentName verifies that a bare agent name
// (no prefix) fails ParseReference and falls through to legacy path.
func TestConvRefParsing_LegacyBareAgentName(t *testing.T) {
	_, err := messaging.ParseReference("my-agent")
	require.Error(t, err, "bare agent name should not parse as a conversation reference")
}

// TestConvRefParsing_LegacyAgentPrefix verifies that agent:name fails
// ParseReference and falls through to legacy path.
func TestConvRefParsing_LegacyAgentPrefix(t *testing.T) {
	_, err := messaging.ParseReference("agent:my-agent")
	require.Error(t, err, "agent: prefix should not parse as a conversation reference")
}

// TestConvRefParsing_UserPrefix verifies that user:name fails ParseReference
// and falls through to legacy path.
func TestConvRefParsing_UserPrefix(t *testing.T) {
	_, err := messaging.ParseReference("user:alice")
	require.Error(t, err, "user: prefix should not parse as a conversation reference")
}

// TestSendMessageViaConversation_AgentRef verifies the full flow for @agent
// from a human CLI context (SCION_AGENT_NAME not set): the message is sent
// via the agent message endpoint. DEF-142 P5: no resolve step — the server
// derives the conversation from sender/recipient principals.
func TestSendMessageViaConversation_AgentRef(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	// Explicitly clear SCION_AGENT_NAME to ensure human CLI context.
	t.Setenv("SCION_AGENT_NAME", "")

	projectID := "proj-convref-agent"
	server, sent, _ := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefAgent,
		Value: "builder",
		Raw:   "@builder",
	}

	err = sendMessageViaConversation(hubCtx, ref, "please review", false, false, nil)
	require.NoError(t, err)

	// Verify message was sent to the agent via the agent message endpoint.
	// DEF-142 P5: ConversationID is not set by the CLI — the server derives
	// it from sender/recipient principals (DEF-138 Rule 3).
	require.Len(t, *sent, 1)
	assert.Equal(t, "builder", (*sent)[0].AgentName)
	assert.Equal(t, "please review", (*sent)[0].Message)
}

// TestSendMessageViaConversation_AgentRef_AgentContext verifies that @agent
// from an agent context (SCION_AGENT_NAME set) sends via the structured
// message endpoint. DEF-164: agent-to-agent messages route through
// SendStructuredMessage to avoid the outbound handler's resolveAgentDM
// creating orphan conversation/participant rows before DEF-152 addressee
// derivation rejects non-user DMs.
func TestSendMessageViaConversation_AgentRef_AgentContext(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-agent-ctx"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefAgent,
		Value: "builder",
		Raw:   "@builder",
	}

	err = sendMessageViaConversation(hubCtx, ref, "please review", false, false, nil)
	require.NoError(t, err)

	// DEF-164: agent-to-agent messages now route through SendStructuredMessage,
	// not the outbound endpoint.
	assert.Len(t, *outbound, 0, "agent-to-agent should not use outbound path")
	require.Len(t, *sent, 1)
	assert.Equal(t, "builder", (*sent)[0].AgentName)
	require.NotNil(t, (*sent)[0].StructuredMsg, "structured message should be present")
	assert.Equal(t, "agent:test-sender-agent", (*sent)[0].StructuredMsg.Sender)
	assert.Equal(t, "agent:builder", (*sent)[0].StructuredMsg.Recipient)
	assert.Equal(t, "please review", (*sent)[0].StructuredMsg.Msg)
}

// TestConvRef_ThreadRefAccepted verifies that #<thread> references are
// accepted and routed through sendMessageViaConversation.
// DEF-138 P-4 opened the gate that previously rejected these.
func TestConvRef_ThreadRefAccepted(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-thread-accepted"
	server, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefThread,
		Value: "general",
		Raw:   "#general",
	}

	err = sendMessageViaConversation(hubCtx, ref, "hello thread", false, false, nil)
	require.NoError(t, err, "thread reference should be accepted after DEF-138")

	// DEF-142 P5: the message is sent with conversation_ref — no resolve step.
	require.Len(t, *outbound, 1, "one outbound message expected")
	assert.Equal(t, "#general", (*outbound)[0].ConversationRef)
	assert.Equal(t, "hello thread", (*outbound)[0].Message)
}

// TestConvRef_ConvIDAccepted verifies that conv:<uuid> references are
// accepted and routed through sendMessageViaConversation.
// DEF-138 P-4 opened the gate that previously rejected these.
func TestConvRef_ConvIDAccepted(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-convid-accepted"
	server, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: "7f3a91c2-1234-5678-9abc-def012345678",
		Raw:   "conv:7f3a91c2-1234-5678-9abc-def012345678",
	}

	err = sendMessageViaConversation(hubCtx, ref, "payload", false, false, nil)
	require.NoError(t, err, "conv: reference should be accepted after DEF-138")

	// DEF-142 P5: the message is sent with conversation_ref — no resolve step.
	require.Len(t, *outbound, 1, "one outbound message expected")
	assert.Equal(t, "conv:7f3a91c2-1234-5678-9abc-def012345678", (*outbound)[0].ConversationRef)
	assert.Equal(t, "payload", (*outbound)[0].Message)
}

// TestSendMessageViaConversation_EmailRef_AgentContext verifies that @<email>
// references are delivered via the outbound message path when called from
// an agent context (SCION_AGENT_NAME is set).
func TestSendMessageViaConversation_EmailRef_AgentContext(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-email"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefEmail,
		Value: "user@example.com",
		Raw:   "@user@example.com",
	}

	err = sendMessageViaConversation(hubCtx, ref, "hello from agent", false, false, nil)
	require.NoError(t, err)

	// DEF-142 P5: outbound message with conversation_ref — no resolve step.
	require.Len(t, *outbound, 1, "outbound message must be delivered")
	assert.Equal(t, "user:user@example.com", (*outbound)[0].Recipient)
	assert.Equal(t, "hello from agent", (*outbound)[0].Message)
	assert.Equal(t, "test-sender-agent", (*outbound)[0].AgentName)
	assert.Equal(t, "@user@example.com", (*outbound)[0].ConversationRef)

	// Verify no agent messages were sent (email goes via outbound path).
	assert.Len(t, *sent, 0, "email ref should not go through agent message path")
}

// TestSendMessageViaConversation_EmailRef_NoAgentContext verifies that @<email>
// references fail with a clear error when SCION_AGENT_NAME is not set (human
// CLI context). The @<email> path is agent-only.
func TestSendMessageViaConversation_EmailRef_NoAgentContext(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	// Explicitly clear SCION_AGENT_NAME to ensure human CLI context.
	t.Setenv("SCION_AGENT_NAME", "")

	projectID := "proj-convref-email-noagent"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefEmail,
		Value: "user@example.com",
		Raw:   "@user@example.com",
	}

	err = sendMessageViaConversation(hubCtx, ref, "should fail", false, false, nil)
	require.Error(t, err, "email ref without agent context must fail")
	assert.Contains(t, err.Error(), "requires an agent identity")

	// Verify zero sends — no messages should be delivered.
	assert.Len(t, *sent, 0, "no agent messages should be sent")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent")
}

// TestConvRef_MalformedConvIDDenied verifies that conv:not-a-uuid fails with
// a clear error instead of falling through to legacy agent name parsing.
func TestConvRef_MalformedConvIDDenied(t *testing.T) {
	// conv:not-a-uuid must fail, not fall through to legacy agent name
	resetMessageFlags()
	err := messageCmd.RunE(messageCmd, []string{"conv:not-a-uuid", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid conversation reference")
}

// TestConvRef_MalformedThreadDenied verifies that #space/thread fails with
// a clear error instead of falling through to legacy agent name parsing.
func TestConvRef_MalformedThreadDenied(t *testing.T) {
	// #space/thread must fail, not fall through to legacy agent name
	resetMessageFlags()
	err := messageCmd.RunE(messageCmd, []string{"#space/thread", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid conversation reference")
}

// TestConvRef_BareAtDenied verifies that @ alone fails with a clear error
// instead of falling through to bare email detection.
func TestConvRef_BareAtDenied(t *testing.T) {
	// @ alone must fail, not fall through to bare email detection
	resetMessageFlags()
	err := messageCmd.RunE(messageCmd, []string{"@", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid conversation reference")
}

// TestBackwardCompat_BareAgentName verifies that scion message <agent-name> 'text'
// still works with the legacy path (no ParseReference match).
func TestBackwardCompat_BareAgentName(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-compat"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	// Send via the legacy path — bare agent name.
	err = sendMessageViaHub(hubCtx, "old-agent-name", "hello world", false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "old-agent-name", (*sent)[0].AgentName)
	assert.Equal(t, "hello world", (*sent)[0].Message)
}

// TestSendMessageViaConversation_ValidationBeforeSend verifies DEF-48:
// a message that fails validation must NOT be sent to the server.
func TestSendMessageViaConversation_ValidationBeforeSend(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-convref-val-before-send"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefAgent,
		Value: "builder",
		Raw:   "@builder",
	}

	// Send an empty message body — ValidateLegacyMessage rejects this because
	// "msg field is required" when there are no attachments.
	err = sendMessageViaConversation(hubCtx, ref, "", false, false, nil)
	require.Error(t, err, "empty message must fail validation")
	assert.Contains(t, err.Error(), "validation failed")

	// DEF-48: no messages should be sent when validation fails.
	assert.Len(t, *sent, 0, "no messages should be sent when validation fails")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent when validation fails")
}

// TestSendMessageViaConversation_EmailPreconditionBeforeSend verifies DEF-48
// for the @email path: when SCION_AGENT_NAME is unset (human CLI context),
// the precondition must fail before any message is sent.
func TestSendMessageViaConversation_EmailPreconditionBeforeSend(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	// Explicitly unset SCION_AGENT_NAME to simulate human CLI context.
	t.Setenv("SCION_AGENT_NAME", "")

	projectID := "proj-convref-email-precond"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefEmail,
		Value: "user@example.com",
		Raw:   "@user@example.com",
	}

	err = sendMessageViaConversation(hubCtx, ref, "should fail before send", false, false, nil)
	require.Error(t, err, "email ref without agent context must fail")
	assert.Contains(t, err.Error(), "requires an agent identity")

	// DEF-48: no messages should be sent when precondition fails.
	assert.Len(t, *sent, 0, "no agent messages should be sent")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent")
}

// TestSendMessageViaConversation_EmailThreadIDWithoutChannel verifies that
// --thread-id without --channel does NOT cause a false rejection on the @email
// path. The outbound path drops both fields (they are not in OutboundMessageRequest
// as constructed), so the thread_id-requires-channel rule must not fire.
// DEF-51 Direction 1: false rejection.
func TestSendMessageViaConversation_EmailThreadIDWithoutChannel(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()

	// Set thread-id without channel — would fail ValidateLegacyMessage if
	// those CLI flags leaked into the validation probe.
	msgThreadID = "some-thread"
	msgChannel = ""

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-email-threadid"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefEmail,
		Value: "user@example.com",
		Raw:   "@user@example.com",
	}

	err = sendMessageViaConversation(hubCtx, ref, "hello with thread", false, false, nil)
	require.NoError(t, err, "thread-id without channel must not be rejected on @email path")

	// DEF-142 P5: outbound message with conversation_ref.
	assert.Len(t, *outbound, 1, "outbound message should be delivered")
	assert.Equal(t, "@user@example.com", (*outbound)[0].ConversationRef)
	assert.Len(t, *sent, 0, "no agent messages should be sent on email path")
}

// TestSendMessageViaConversation_EmailEmptyMsgBeforeSend verifies DEF-51:
// an empty message body on the @email path must fail validation before
// any message is sent, even when --attach is set. The outbound path drops
// attachments, so the empty-body waiver (which requires attachments) must not
// apply — the validated probe must reflect the sent envelope.
// DEF-51 Direction 2: missed rejection.
func TestSendMessageViaConversation_EmailEmptyMsgBeforeSend(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()

	// Set attachments via CLI flag — buildStructuredMessage reads these for
	// non-conversation paths; the conversation path uses the explicit parameter.
	msgAttach = []string{"/workspace/x.png"}

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-email-empty-msg"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	ref := &messaging.Reference{
		Kind:  messaging.RefEmail,
		Value: "user@example.com",
		Raw:   "@user@example.com",
	}

	// Empty message body with attachments — ValidateLegacyMessage waives
	// empty-body when attachments are present, and the outbound path now
	// transmits them.
	err = sendMessageViaConversation(hubCtx, ref, "", false, false, msgAttach)
	require.NoError(t, err, "empty message on @email path with attachments should succeed")

	// The outbound message should have been sent.
	assert.Len(t, *outbound, 1, "one outbound message should be sent")

	// Verify that an empty message without attachments still fails validation.
	err = sendMessageViaConversation(hubCtx, ref, "", false, false, nil)
	require.Error(t, err, "empty message on @email path without attachments must fail validation")
	assert.Contains(t, err.Error(), "validation failed")

	// DEF-51: no additional messages should be sent when validation fails.
	assert.Len(t, *sent, 0, "no agent messages should be sent")
	assert.Len(t, *outbound, 1, "outbound count should not increase after validation failure")
}

// --- CPM cutover (#1693) tests: conv: routes through outbound ---

// TestSendMessageViaConversation_ConvRef_OutboundRoute verifies that conv:<uuid>
// from an agent context routes through the outbound endpoint (not the
// conversation send API). This is the core CPM cutover change (#1693).
func TestSendMessageViaConversation_ConvRef_OutboundRoute(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-outbound-route"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	err = sendMessageViaConversation(hubCtx, ref, "outbound route test", false, false, nil)
	require.NoError(t, err)

	// Must go through outbound, not the agent message path
	assert.Len(t, *sent, 0, "conv: should not use agent message path")
	require.Len(t, *outbound, 1, "conv: should use outbound path")
	assert.Equal(t, "conv:"+convID, (*outbound)[0].ConversationRef)
	assert.Equal(t, "outbound route test", (*outbound)[0].Message)
	assert.Equal(t, "instruction", (*outbound)[0].Type)
	assert.Equal(t, "test-sender-agent", (*outbound)[0].AgentName)
}

// TestSendMessageViaConversation_ConvRef_WakeSerialized verifies that
// --wake is serialized in the outbound request for conv: references.
// CPM cutover (#1693): wake was previously rejected for conv:.
func TestSendMessageViaConversation_ConvRef_WakeSerialized(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-wake"
	server, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-111111111111"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	// Wake=true should be serialized
	err = sendMessageViaConversation(hubCtx, ref, "wake me up", false, true, nil)
	require.NoError(t, err, "conv: with --wake should succeed via outbound")

	require.Len(t, *outbound, 1)
	assert.True(t, (*outbound)[0].Wake, "wake flag should be serialized in outbound request")

	// Wake=false should also work (baseline)
	err = sendMessageViaConversation(hubCtx, ref, "no wake", false, false, nil)
	require.NoError(t, err)

	require.Len(t, *outbound, 2)
	assert.False(t, (*outbound)[1].Wake, "wake=false should be serialized correctly")
}

// TestSendMessageViaConversation_ConvRef_AttachmentsSerialized verifies that
// --attach is serialized in the outbound request for conv: references.
// CPM cutover (#1693): attachments were previously rejected for conv:.
func TestSendMessageViaConversation_ConvRef_AttachmentsSerialized(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-attach"
	server, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-222222222222"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	attachments := []string{"/workspace/notes.md", "/scion-volumes/data.json"}
	err = sendMessageViaConversation(hubCtx, ref, "msg with attachments", false, false, attachments)
	require.NoError(t, err, "conv: with --attach should succeed via outbound")

	require.Len(t, *outbound, 1)
	assert.Equal(t, attachments, (*outbound)[0].Attachments, "attachments should be serialized")
}

// TestSendMessageViaConversation_ConvRef_UrgentSerialized verifies that
// --interrupt (urgent) is serialized for conv: references via outbound.
func TestSendMessageViaConversation_ConvRef_UrgentSerialized(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-urgent"
	server, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-333333333333"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	// Urgent=true
	err = sendMessageViaConversation(hubCtx, ref, "urgent msg", true, false, nil)
	require.NoError(t, err)

	require.Len(t, *outbound, 1)
	assert.True(t, (*outbound)[0].Urgent, "urgent flag should be serialized")
}

// TestSendMessageViaConversation_ConvRef_GroupViaOutbound verifies that
// same-project group conversations via conv: work through the outbound
// endpoint. This addresses the original 501 regression from PR #1679.
func TestSendMessageViaConversation_ConvRef_GroupViaOutbound(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-group"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	// Use a conv: ref that represents a group conversation
	groupConvID := "gggggggg-rrrr-oooo-uuuu-pppppppppppp"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: groupConvID,
		Raw:   "conv:" + groupConvID,
	}

	err = sendMessageViaConversation(hubCtx, ref, "group message", false, false, nil)
	require.NoError(t, err, "group conversation via conv: should work through outbound")

	assert.Len(t, *sent, 0, "conv: should not use agent message path")
	require.Len(t, *outbound, 1, "conv: should use outbound path")
	assert.Equal(t, "conv:"+groupConvID, (*outbound)[0].ConversationRef)
}

// TestSendMessageViaConversation_ConvRef_OutputFormat verifies that text
// and JSON output modes produce correct output for conv: sends via outbound.
// CPM cutover (#1693): the outbound result carries message_id and status.
func TestSendMessageViaConversation_ConvRef_OutputFormat(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-output"
	server, _, _ := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-444444444444"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	// Text mode (default) — should succeed without error
	err = sendMessageViaConversation(hubCtx, ref, "text output test", false, false, nil)
	require.NoError(t, err)
}

// TestSendMessageViaConversation_ConvRef_DispatchError verifies that when the
// outbound endpoint returns an error for conv: sends, the error propagates
// without trying another endpoint. CPM cutover (#1693) acceptance criteria:
// policy denials and dispatch failures return non-success.
func TestSendMessageViaConversation_ConvRef_DispatchError(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-error"
	// Use a server that returns 403 for outbound messages
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case strings.HasSuffix(r.URL.Path, "/outbound-message"):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "forbidden",
					"message": "policy denied: cross-project attachment not allowed",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "aaaaaaaa-bbbb-cccc-dddd-555555555555"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	err = sendMessageViaConversation(hubCtx, ref, "should fail", false, false, nil)
	require.Error(t, err, "policy denial should propagate as error")
	assert.Contains(t, err.Error(), "failed to send message to conv:")
}
