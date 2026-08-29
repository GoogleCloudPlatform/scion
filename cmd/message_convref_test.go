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
	AgentName string
	Recipient string
	Message   string
	Type      string
	Urgent    bool
}

// convRefMockServer extends the message mock with a /conversations/resolve endpoint
// and an outbound-message recorder.
func newConvRefMockHubServer(t *testing.T, projectID string) (*httptest.Server, *[]sentMessage, *[]resolveRequest, *[]outboundMessage) {
	t.Helper()
	var sent []sentMessage
	var resolves []resolveRequest
	var outbound []outboundMessage
	var mu sync.Mutex

	projectPrefix := "/api/v1/projects/" + projectID + "/agents/"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case path == "/api/v1/conversations/resolve" && r.Method == http.MethodPost:
			var req resolveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			resolves = append(resolves, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"conversation_id": "conv-test-12345",
				"created":         false,
			})

		case r.Method == http.MethodPost && strings.HasPrefix(path, projectPrefix) && strings.HasSuffix(path, "/outbound-message"):
			// Outbound message endpoint: /api/v1/projects/<pid>/agents/<agent>/outbound-message
			rest := path[len(projectPrefix):]
			agentName := rest[:len(rest)-len("/outbound-message")]
			var body hubclient.OutboundMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			outbound = append(outbound, outboundMessage{
				AgentName: agentName,
				Recipient: body.Recipient,
				Message:   body.Msg,
				Type:      body.Type,
				Urgent:    body.Urgent,
			})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && strings.HasPrefix(path, projectPrefix):
			// Agent message endpoint
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

	return server, &sent, &resolves, &outbound
}

type resolveRequest struct {
	Reference string `json:"reference"`
	ProjectID string `json:"project_id"`
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

// TestSendMessageViaConversation_AgentRef verifies the full flow:
// @agent → resolve → send with conversation_id.
func TestSendMessageViaConversation_AgentRef(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-convref-agent"
	server, sent, resolves, _ := newConvRefMockHubServer(t, projectID)
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

	err = sendMessageViaConversation(hubCtx, ref, "please review", false, false)
	require.NoError(t, err)

	// Verify resolve was called with the right reference.
	require.Len(t, *resolves, 1)
	assert.Equal(t, "@builder", (*resolves)[0].Reference)
	assert.Equal(t, projectID, (*resolves)[0].ProjectID)

	// Verify message was SENT to the agent with conversation_id set.
	require.Len(t, *sent, 1)
	assert.Equal(t, "builder", (*sent)[0].AgentName)
	assert.Equal(t, "please review", (*sent)[0].Message)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, "conv-test-12345", (*sent)[0].StructuredMsg.ConversationID)
}

// TestConvRef_ThreadRefGated verifies that #<thread> references are gated
// at the CLI entry point. The gate returns a non-zero exit with a clear
// error, and zero messages are sent.
func TestConvRef_ThreadRefGated(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	// Stand up a mock so we can verify zero sends AFTER the invocation.
	projectID := "proj-convref-thread-gated"
	server, sent, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	// Execute the command path — the gate fires before any hub connection.
	err := messageCmd.RunE(messageCmd, []string{"#general", "hello thread"})
	require.Error(t, err, "thread reference must be rejected by the gate")
	assert.Contains(t, err.Error(), "not yet supported")

	// Zero sends — the gate prevented any message delivery.
	assert.Len(t, *sent, 0, "no agent messages should be sent for gated ref")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent for gated ref")
}

// TestConvRef_ConvIDGated verifies that conv:<uuid> references are gated
// at the CLI entry point. The gate returns a non-zero exit with a clear
// error, and zero messages are sent.
func TestConvRef_ConvIDGated(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	// Stand up a mock so we can verify zero sends AFTER the invocation.
	projectID := "proj-convref-convid-gated"
	server, sent, _, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	// Execute the command path — the gate fires before any hub connection.
	err := messageCmd.RunE(messageCmd, []string{"conv:7f3a91c2-1234-5678-9abc-def012345678", "payload"})
	require.Error(t, err, "conv: reference must be rejected by the gate")
	assert.Contains(t, err.Error(), "not yet supported")

	// Zero sends — the gate prevented any message delivery.
	assert.Len(t, *sent, 0, "no agent messages should be sent for gated ref")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent for gated ref")
}

// TestSendMessageViaConversation_EmailRef_AgentContext verifies that @<email>
// references are delivered via the outbound message path when called from
// an agent context (SCION_AGENT_NAME is set).
func TestSendMessageViaConversation_EmailRef_AgentContext(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "proj-convref-email"
	server, sent, resolves, outbound := newConvRefMockHubServer(t, projectID)
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

	err = sendMessageViaConversation(hubCtx, ref, "hello from agent", false, false)
	require.NoError(t, err)

	// Verify resolve was called.
	require.Len(t, *resolves, 1)
	assert.Equal(t, "@user@example.com", (*resolves)[0].Reference)

	// Verify the outbound message was delivered via the recorder.
	require.Len(t, *outbound, 1, "outbound message must be delivered")
	assert.Equal(t, "user:user@example.com", (*outbound)[0].Recipient)
	assert.Equal(t, "hello from agent", (*outbound)[0].Message)
	assert.Equal(t, "test-sender-agent", (*outbound)[0].AgentName)

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
	server, sent, _, outbound := newConvRefMockHubServer(t, projectID)
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

	err = sendMessageViaConversation(hubCtx, ref, "should fail", false, false)
	require.Error(t, err, "email ref without agent context must fail")
	assert.Contains(t, err.Error(), "only supported from within an agent container")

	// Verify zero sends — no messages should be delivered.
	assert.Len(t, *sent, 0, "no agent messages should be sent")
	assert.Len(t, *outbound, 0, "no outbound messages should be sent")
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
	err = sendMessageViaHub(hubCtx, "old-agent-name", "hello world", false, false, false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "old-agent-name", (*sent)[0].AgentName)
	assert.Equal(t, "hello world", (*sent)[0].Message)
}
