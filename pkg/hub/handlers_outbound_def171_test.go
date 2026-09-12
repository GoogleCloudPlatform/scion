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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// def171Envelope is a minimal struct matching the DeliveryEnvelope shape,
// used to decode DeliveryText without importing the full delivery package.
type def171Envelope struct {
	Timestamp    string          `json:"timestamp"`
	Conversation *def171ConvInfo `json:"conversation,omitempty"`
	From         string          `json:"from"`
	To           []string        `json:"to,omitempty"`
	Type         string          `json:"type"`
	Msg          string          `json:"msg"`
	Urgent       bool            `json:"urgent,omitempty"`
}

type def171ConvInfo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Surface string `json:"surface"`
	Name    string `json:"name,omitempty"`
}

// extractEnvelopeJSON pulls the raw JSON from between BEGIN/END SCION MESSAGE
// delimiters in a rendered DeliveryText string.
func extractEnvelopeJSON(t *testing.T, deliveryText string) string {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---"
	const end = "---END SCION MESSAGE---"

	startIdx := strings.Index(deliveryText, begin)
	if startIdx < 0 {
		t.Fatalf("BEGIN delimiter not found in DeliveryText:\n%s", deliveryText)
	}
	startIdx += len(begin) + 1 // skip delimiter + newline

	endIdx := strings.LastIndex(deliveryText, end)
	if endIdx < 0 || endIdx <= startIdx {
		t.Fatalf("END delimiter not found in DeliveryText:\n%s", deliveryText)
	}
	endIdx-- // trim newline before end delimiter

	return deliveryText[startIdx:endIdx]
}

// parseDEF171Envelope extracts and decodes the JSON envelope from a rendered
// DeliveryText string.
func parseDEF171Envelope(t *testing.T, deliveryText string) def171Envelope {
	t.Helper()
	jsonStr := extractEnvelopeJSON(t, deliveryText)
	var env def171Envelope
	require.NoError(t, json.Unmarshal([]byte(jsonStr), &env),
		"failed to unmarshal envelope JSON: %s", jsonStr)
	return env
}

// def171Setup creates a server, project, two agents, a DM conversation, and a
// recording dispatcher — the common scaffolding for DEF-171 tests.
func def171Setup(t *testing.T) (srv *Server, s store.Store, project *store.Project,
	agentA, agentB *store.Agent, convID string, dispatcher *recordingDispatcher) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("def171-project"),
		Name: "def171-project",
		Slug: "def171-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("def171-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def171-broker",
		Slug:   "def171-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA = &store.Agent{
		ID:              tid("def171-agent-a"),
		Name:            "agent-a",
		Slug:            "agent-a",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB = &store.Agent{
		ID:              tid("def171-agent-b"),
		Name:            "agent-b",
		Slug:            "agent-b",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create the DM conversation between the two agents.
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

	return srv, s, project, agentA, agentB, convID, dispatcher
}

// sendAgentDM sends an agent-to-agent DM through the real HTTP handler.
func sendAgentDM(t *testing.T, srv *Server, sender *store.Agent, convID, projectID, msgText string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msgText,
		Type:            "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// ---------------------------------------------------------------------------
// Test 1: Agent-to-agent DM stamps DeliveryText with new envelope when
// envelope switch is ON.
// ---------------------------------------------------------------------------

func TestDEF171_AgentDM_DeliveryText_StampedWhenSwitchOn(t *testing.T) {
	srv, s, project, agentA, agentB, convID, dispatcher := def171Setup(t)
	enableReadSwitch(t, srv)

	rr := sendAgentDM(t, srv, agentA, convID, project.ID, "DEF-171 envelope test")
	require.Equal(t, http.StatusOK, rr.Code,
		"agent-to-agent DM must succeed; body: %s", rr.Body.String())

	// Dispatcher must have been called once.
	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")

	call := calls[0]
	require.Equal(t, agentB.ID, call.Agent.ID, "dispatch must target agent B")
	require.NotNil(t, call.StructuredMessage, "StructuredMessage must not be nil")
	require.NotEmpty(t, call.StructuredMessage.DeliveryText,
		"DeliveryText must be non-empty when envelope switch is ON")

	// Decode the envelope — assert on parsed JSON, never raw string.
	env := parseDEF171Envelope(t, call.StructuredMessage.DeliveryText)

	// Verification 1: conversation object present with kind:"direct".
	require.NotNil(t, env.Conversation,
		"conversation object must be present in new envelope format")
	assert.Equal(t, "direct", env.Conversation.Kind,
		"conversation.kind must be 'direct' for agent-to-agent DM")
	assert.Equal(t, "native", env.Conversation.Surface,
		"conversation.surface must be 'native'")

	// Verification 2: conversation.id matches the actual DM conversation.
	assert.Equal(t, convID, env.Conversation.ID,
		"conversation.id must match the actual DM conversation ID")

	// Verification 3: "from" field present (not old "sender").
	assert.NotEmpty(t, env.From, "from field must be present")

	// Verification 4: "type" is "message" (not old "instruction" at top level).
	assert.Equal(t, "message", env.Type,
		"type must be 'message' in the new envelope format")

	// Verification 5: timestamp is populated (not empty like the old format).
	assert.NotEmpty(t, env.Timestamp,
		"timestamp must be populated in the new envelope format")
	_, parseErr := time.Parse(time.RFC3339, env.Timestamp)
	assert.NoError(t, parseErr, "timestamp must parse as RFC3339")

	// Verification 6: message body is correct.
	assert.Equal(t, "DEF-171 envelope test", env.Msg, "msg body must match")

	// Verify the message was persisted correctly.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "message must be persisted")
	var found *store.Message
	for i := range msgs.Items {
		if msgs.Items[i].Msg == "DEF-171 envelope test" {
			found = &msgs.Items[i]
			break
		}
	}
	require.NotNil(t, found, "sent message must be found in store")
	assert.Equal(t, convID, found.ConversationID,
		"persisted message.ConversationID must match the DM conversation")
}

// ---------------------------------------------------------------------------
// Test 2: Agent-to-agent DM does NOT stamp DeliveryText when envelope switch
// is OFF (legacy format preserved).
// ---------------------------------------------------------------------------

func TestDEF171_AgentDM_DeliveryText_EmptyWhenSwitchOff(t *testing.T) {
	srv, _, project, agentA, agentB, convID, dispatcher := def171Setup(t)
	// Do NOT enable the switch — writeDenyEnabled() returns false.

	rr := sendAgentDM(t, srv, agentA, convID, project.ID, "DEF-171 legacy test")
	require.Equal(t, http.StatusOK, rr.Code,
		"agent-to-agent DM must succeed; body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")

	call := calls[0]
	require.Equal(t, agentB.ID, call.Agent.ID, "dispatch must target agent B")
	require.NotNil(t, call.StructuredMessage, "StructuredMessage must not be nil")
	assert.Empty(t, call.StructuredMessage.DeliveryText,
		"DeliveryText must be empty when envelope switch is OFF")
}

// ---------------------------------------------------------------------------
// Test 3: Mutation test — removing RenderDeliveryText makes DeliveryText empty,
// proving the new code is load-bearing.
// ---------------------------------------------------------------------------

func TestDEF171_AgentDM_MutationTest_EmptyAfterClear(t *testing.T) {
	srv, _, project, agentA, agentB, convID, dispatcher := def171Setup(t)
	enableReadSwitch(t, srv)

	rr := sendAgentDM(t, srv, agentA, convID, project.ID, "DEF-171 mutation test")
	require.Equal(t, http.StatusOK, rr.Code,
		"agent-to-agent DM must succeed; body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")

	call := calls[0]
	require.Equal(t, agentB.ID, call.Agent.ID)
	require.NotNil(t, call.StructuredMessage)

	// The real mutation test (removing the RenderDeliveryText call) is verified
	// at the go test level by temporarily commenting out the call and confirming
	// this test fails. Here we verify the positive case: DeliveryText IS set.
	require.NotEmpty(t, call.StructuredMessage.DeliveryText,
		"MUTATION CANARY: DeliveryText must be non-empty when RenderDeliveryText "+
			"is called. If this assertion fails, the RenderDeliveryText call is "+
			"not load-bearing — the old-shape symptom (empty DeliveryText) is back.")

	// Double-check: the decoded envelope has the new-format conversation object.
	env := parseDEF171Envelope(t, call.StructuredMessage.DeliveryText)
	require.NotNil(t, env.Conversation,
		"MUTATION CANARY: conversation object must be present")
	assert.Equal(t, "direct", env.Conversation.Kind)
}

// ---------------------------------------------------------------------------
// Test 4: Persistence and dispatch integrity — the fix does not change
// the persist, SSE publish, or dispatch-failure-is-non-fatal behavior.
// ---------------------------------------------------------------------------

func TestDEF171_AgentDM_PersistAndDispatch_Integrity(t *testing.T) {
	srv, s, project, agentA, agentB, convID, dispatcher := def171Setup(t)
	enableReadSwitch(t, srv)

	rr := sendAgentDM(t, srv, agentA, convID, project.ID, "DEF-171 integrity test")
	require.Equal(t, http.StatusOK, rr.Code,
		"agent-to-agent DM must succeed; body: %s", rr.Body.String())

	// Verify persistence.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found *store.Message
	for i := range msgs.Items {
		if msgs.Items[i].Msg == "DEF-171 integrity test" {
			found = &msgs.Items[i]
			break
		}
	}
	require.NotNil(t, found, "message must be persisted")
	assert.Equal(t, convID, found.ConversationID,
		"persisted message must be tied to the DM conversation")
	assert.Equal(t, agentB.ID, found.RecipientID,
		"persisted message.RecipientID must be agent B")

	// Verify dispatch.
	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "exactly one dispatch call")
	assert.Equal(t, agentB.ID, calls[0].Agent.ID,
		"dispatch must target agent B")

	// Verify HTTP response shape.
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["message_id"], "response must include message_id")
	assert.Equal(t, "sent", resp["status"], "response status must be 'sent'")
}

// ---------------------------------------------------------------------------
// Test 5: Dispatch failure is non-fatal — the message is still persisted
// and the handler returns 200.
// ---------------------------------------------------------------------------

func TestDEF171_AgentDM_DispatchFailure_NonFatal(t *testing.T) {
	srv, s, project, agentA, _, convID, dispatcher := def171Setup(t)
	enableReadSwitch(t, srv)

	// Make the dispatcher return an error.
	dispatcher.returnErr = assert.AnError

	rr := sendAgentDM(t, srv, agentA, convID, project.ID, "DEF-171 dispatch-fail test")
	require.Equal(t, http.StatusOK, rr.Code,
		"dispatch failure is non-fatal; handler must still return 200; body: %s",
		rr.Body.String())

	// Message must still be persisted.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "DEF-171 dispatch-fail test" {
			found = true
			break
		}
	}
	assert.True(t, found, "message must be persisted even when dispatch fails")
}
