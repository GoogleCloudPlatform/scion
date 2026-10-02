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

// ---------------------------------------------------------------------------
// Cross-project raw keystroke-injection refusal.
//
// Raw delivers keystrokes verbatim, skipping the wrapped envelope. The same
// isolation-boundary concern #1687 raised for cross-project attachments
// applies here, so Raw is refused cross-project the same way: a clear 4xx
// before persistence or dispatch, rather than downgrading the message or
// extending cross-project capabilities.
//
// This check (agent_dm_operation.go, step 4b) is intentionally isolated in
// its own admission step and its own denial code
// (MessageDenialCrossProjectRawUnsupported) so it is easy to drop pending
// confirmation of the final cross-project policy for keystroke injection
// (`scion keys` / `scion message --raw`). It does not touch Plain.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendInboundAgentMessageRaw sends a Raw=true structured message through the
// inbound handleAgentMessage path — the same wire path `scion keys` and
// `scion message --raw` use. Kept local to this file (rather than added to
// sendInboundAgentMessage in handlers_foreign_attach_test.go) so the
// cross-project Raw refusal stays isolated and easy to drop.
func sendInboundAgentMessageRaw(t *testing.T, srv *Server, senderAgent, targetAgent *store.Agent, msgText string) *httptest.ResponseRecorder {
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
		Raw:         true,
	}

	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
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

// TestExecuteAgentDM_CrossProjectRaw_RefusedByDispatchBackstop replaces the
// former TestExecuteAgentDM_CrossProjectRaw_Denied: task 2.3
// (ptone/scion#2197) removed ExecuteAgentDM's own step 4b
// (crossProjectRawUnsupported / MessageDenialCrossProjectRawUnsupported)
// because raw agent-to-agent DMs -- cross-project or not -- are now
// intercepted upstream of ExecuteAgentDM entirely, by the message-raw
// bridge in the routers (agent_keys_message_bridge.go), before
// authorizeAgentMessage ever runs. That bridge reports this exact
// cross-project case as agentkeys.OutcomeCrossProjectKeysUnsupported (see
// the bridge's own tests, AK-21d) -- not as anything ExecuteAgentDM itself
// decides.
//
// Calling ExecuteAgentDM directly with Raw==true, as this test does, can
// only happen by bypassing the bridge (a production caller never can): it
// is exactly the "classification-parity defect scenario" contract §6.1(a)/
// AK-55 describes. This test proves the remaining guarantee for that
// scenario: the dispatch-layer backstop (httpdispatcher.go's
// ErrRawDispatchRefused) refuses to deliver unconditionally, and the
// already-persisted row (persistence happens before dispatch, and this
// layer cannot undo that) is marked failed through the ordinary failure
// path -- never a broker call, regardless of project.
func TestExecuteAgentDM_CrossProjectRaw_RefusedByDispatchBackstop(t *testing.T) {
	srv, s, _, _, agentA, agentB, _, _, _ := foreignAttachSetup(t)
	ctx := context.Background()

	mockClient := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default()))

	input := deliveryDMInput(agentA, agentB, "RAWPROBE-CROSS")
	input.Raw = true
	input.ProjectID = agentB.ProjectID

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.NotNil(t, dmErr, "raw DM dispatch must be refused at the backstop")
	assert.Nil(t, result)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)

	assert.False(t, mockClient.messageCalled, "the dispatch-layer backstop must prevent any broker call")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1, "ExecuteAgentDM persists before dispatch -- the backstop cannot undo that (contract §6.1(a))")
	assert.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
}

func TestExecuteAgentDM_CrossProjectPlain_StillAllowed(t *testing.T) {
	// Plain is deliberately out of scope for the cross-project refusal
	// (isolated to Raw pending confirmation of the final policy). This test
	// locks that boundary: a cross-project Plain DM must still be accepted.
	srv, _, _, _, agentA, agentB, _, dispatcher, _ := foreignAttachSetup(t)
	ctx := context.Background()

	input := deliveryDMInput(agentA, agentB, "PLAINPROBE-CROSS")
	input.Plain = true
	input.ProjectID = agentB.ProjectID

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "cross-project plain DM must not be rejected by the raw-only cross-project check")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	assert.True(t, calls[0].StructuredMessage.Plain)
}

// TestExecuteAgentDM_SameProjectRaw_RefusedByDispatchBackstop replaces the
// former TestExecuteAgentDM_SameProjectRaw_StillWorks. Before task 2.3,
// ExecuteAgentDM delivered a same-project raw DM (only the cross-project
// case, step 4b, was refused). From 2.3 onward, raw never reaches
// ExecuteAgentDM at all in production -- the message-raw bridge
// (agent_keys_message_bridge.go) intercepts it upstream, same- or
// cross-project alike, and delegates to ExecuteAgentKeys instead. Calling
// ExecuteAgentDM directly with Raw==true, as this test does, now hits the
// unconditional dispatch-layer backstop (httpdispatcher.go's
// ErrRawDispatchRefused, contract §6.1(a)/AK-55) regardless of project,
// proving ExecuteAgentDM itself no longer special-cases Raw at all.
func TestExecuteAgentDM_SameProjectRaw_RefusedByDispatchBackstop(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	mockClient := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default()))

	input := deliveryDMInput(sender, target, "RAWPROBE-SAME-PROJECT")
	input.Raw = true

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.NotNil(t, dmErr, "raw DM dispatch must be refused at the backstop even same-project")
	assert.Nil(t, result)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)

	assert.False(t, mockClient.messageCalled, "the dispatch-layer backstop must prevent any broker call")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1, "ExecuteAgentDM persists before dispatch -- the backstop cannot undo that (contract §6.1(a))")
	assert.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
}

// TestAgentSenderDM_CrossProjectRaw_InboundPath_Denied is an end-to-end
// reproduction through the real HTTP handler (the same one `scion keys` and
// `scion message --raw` reach), mirroring
// TestForeignAttach_InboundPath_CrossProject_Denied.
func TestAgentSenderDM_CrossProjectRaw_InboundPath_Denied(t *testing.T) {
	srv, s, _, _, agentA, agentB, _, dispatcher, _ := foreignAttachSetup(t)

	rr := sendInboundAgentMessageRaw(t, srv, agentA, agentB, "inbound cross-project raw")

	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"cross-project raw via inbound path must be rejected; body: %s", rr.Body.String())

	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected cross-project raw send must produce zero message rows")
	assert.Empty(t, dispatcher.getCalls(), "rejected cross-project raw send must produce zero dispatch calls")
}
