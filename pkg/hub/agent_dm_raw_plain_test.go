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
// Regression tests: agent-sender DM with Raw/Plain must reach the dispatcher
// with the flag intact (#1808 regression).
//
// Before #1688/#1808, handleAgentMessage dispatched the client's
// structuredMsg as-is when the sender was an agent, so StructuredMessage.Raw
// and .Plain survived to the broker. After #1688 extracted ExecuteAgentDM,
// the operation rebuilt the StructuredMessage from AgentDMInput without a
// Raw/Plain field, silently downgrading `scion message --raw` /  --plain
// from agent senders into a wrapped envelope + Enter. These tests prove the
// fix: Raw/Plain flow from the client-supplied StructuredMessage through
// AgentDMInput into the StructuredMessage handed to the dispatcher.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteAgentDM_RawFlagPreserved proves that ExecuteAgentDM propagates
// AgentDMInput.Raw into the StructuredMessage handed to the dispatcher.
func TestExecuteAgentDM_RawFlagPreserved(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	input := deliveryDMInput(sender, target, "RAWPROBE")
	input.Raw = true

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "raw agent-sender DM must not be rejected")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	require.NotNil(t, calls[0].StructuredMessage, "dispatch must carry a structured message")
	assert.True(t, calls[0].StructuredMessage.Raw,
		"Raw must survive ExecuteAgentDM's StructuredMessage rebuild")
	assert.False(t, calls[0].StructuredMessage.Plain,
		"Plain must remain false when only Raw was requested")
}

// TestExecuteAgentDM_PlainFlagPreserved proves that ExecuteAgentDM
// propagates AgentDMInput.Plain into the StructuredMessage handed to the
// dispatcher.
func TestExecuteAgentDM_PlainFlagPreserved(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	input := deliveryDMInput(sender, target, "PLAINPROBE")
	input.Plain = true

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "plain agent-sender DM must not be rejected")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	require.NotNil(t, calls[0].StructuredMessage, "dispatch must carry a structured message")
	assert.True(t, calls[0].StructuredMessage.Plain,
		"Plain must survive ExecuteAgentDM's StructuredMessage rebuild")
	assert.False(t, calls[0].StructuredMessage.Raw,
		"Raw must remain false when only Plain was requested")
}

// TestAgentSenderDM_RawFlagReachesDispatcher is an end-to-end reproduction
// of the reported regression: an agent sender posts a StructuredMessage with
// Raw=true to POST /api/v1/projects/{p}/agents/{id}/message (the same wire
// path `scion message --raw` uses via SendStructuredMessage), and the
// dispatched StructuredMessage must still carry Raw=true — not a rendered
// envelope with DeliveryText set for the normal (non-raw) case.
func TestAgentSenderDM_RawFlagReachesDispatcher(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         "RAWPROBE",
		Raw:         true,
	}
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"raw agent-sender DM must be delivered; body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	require.NotNil(t, calls[0].StructuredMessage)
	assert.True(t, calls[0].StructuredMessage.Raw,
		"Raw must reach the dispatcher for agent-sender DMs (#1808 regression)")
	assert.Equal(t, "RAWPROBE", calls[0].Message)
}
