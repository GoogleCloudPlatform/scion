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
//
// This file also covers two adjacent invariants: DeliveryText (the field the
// broker actually prefers for delivery) stays unrendered for raw/plain DMs,
// and Raw does not weaken the authorization check in ExecuteAgentDM.
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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// dispatched StructuredMessage must still carry Raw=true.
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

// TestExecuteAgentDM_RawFlagDeliveryTextIsBareBody proves the other half of
// the regression fix: with the envelope switch ON (write-deny enabled), a
// raw agent-sender DM's dispatched StructuredMessage.DeliveryText equals the
// bare message body, not a rendered envelope. The broker prefers
// DeliveryText over the raw body when both are present, so this is the field
// that actually controls what a raw agent-sender DM delivers in production.
func TestExecuteAgentDM_RawFlagDeliveryTextIsBareBody(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
	enableReadSwitch(t, srv)
	ctx := context.Background()

	input := deliveryDMInput(sender, target, "RAWPROBE")
	input.Raw = true

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "raw agent-sender DM must not be rejected")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	require.NotNil(t, calls[0].StructuredMessage, "dispatch must carry a structured message")
	assert.Equal(t, "RAWPROBE", calls[0].StructuredMessage.DeliveryText,
		"DeliveryText must be the bare body for a raw agent-sender DM, not a rendered envelope")
}

// TestExecuteAgentDM_PlainFlagDeliveryTextIsBareBody is the Plain equivalent
// of TestExecuteAgentDM_RawFlagDeliveryTextIsBareBody.
func TestExecuteAgentDM_PlainFlagDeliveryTextIsBareBody(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
	enableReadSwitch(t, srv)
	ctx := context.Background()

	input := deliveryDMInput(sender, target, "PLAINPROBE")
	input.Plain = true

	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "plain agent-sender DM must not be rejected")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch must occur")
	require.NotNil(t, calls[0].StructuredMessage, "dispatch must carry a structured message")
	assert.Equal(t, "PLAINPROBE", calls[0].StructuredMessage.DeliveryText,
		"DeliveryText must be the bare body for a plain agent-sender DM, not a rendered envelope")
}

// ---------------------------------------------------------------------------
// Regression guard: Raw must not widen authorization.
//
// authorizeAgentMessage runs unconditionally in ExecuteAgentDM step 3,
// before Raw/Plain are ever consulted, so a raw agent-sender DM to a target
// that denies messages must be refused exactly like a non-raw DM: 403, with
// no message persisted and nothing dispatched. This test locks that
// invariant against a future refactor that special-cases Raw in the
// authorization path.
// ---------------------------------------------------------------------------

func TestExecuteAgentDM_RawFlagRespectsMessageModeNone(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	deniedTarget := *target
	deniedTarget.MessageMode = store.MessageModeNone

	input := deliveryDMInput(sender, &deniedTarget, "RAWPROBE-DENIED")
	input.Raw = true

	_, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.NotNil(t, dmErr, "raw DM to a message_mode=none target must be rejected")
	assert.Equal(t, ErrCodeMessageDenied, dmErr.Code)
	assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus,
		"Raw must not widen the message_mode=none authorization denial")

	assert.Empty(t, dispatcher.getCalls(), "nothing must be dispatched when authorization denies the DM")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	for _, m := range msgs.Items {
		assert.NotEqual(t, "RAWPROBE-DENIED", m.Msg,
			"authorization denial must prevent persistence, raw or not")
	}
}

// TestHandleAgentMessage_TopLevelRawAndPlainFlags verifies that REST callers
// posting top-level {"message": "...", "raw": true} or {"message": "...", "plain": true}
// (or combining top-level "raw": true with "structured_message") propagate the
// Raw/Plain flags onto the dispatched StructuredMessage instead of silently
// dropping them and wrapping keystrokes in ---BEGIN SCION MESSAGE---.
func TestHandleAgentMessage_TopLevelRawAndPlainFlags(t *testing.T) {
	t.Run("top-level message with raw=true from agent sender", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		enableReadSwitch(t, srv)

		reqBody, err := json.Marshal(map[string]any{
			"message": "/model claude-opus-4-8",
			"raw":     true,
		})
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
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Raw, "top-level raw=true must propagate to StructuredMessage.Raw")
		assert.False(t, calls[0].StructuredMessage.Plain)
		assert.Equal(t, "/model claude-opus-4-8", calls[0].StructuredMessage.DeliveryText)
	})

	t.Run("top-level message with plain=true from user sender", func(t *testing.T) {
		srv, _, _, _, target, _, dispatcher := deliverySetup(t)
		enableReadSwitch(t, srv)

		reqBody, err := json.Marshal(map[string]any{
			"message": "PLAIN-TOP-LEVEL",
			"plain":   true,
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/"+target.ID+"/message",
			bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(tid("user-1"), "alice@example.com", "Alice", "admin", "cli")))

		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, req, target.ID)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Plain, "top-level plain=true must propagate to StructuredMessage.Plain")
		assert.False(t, calls[0].StructuredMessage.Raw)
		assert.Equal(t, "PLAIN-TOP-LEVEL", calls[0].StructuredMessage.DeliveryText)
	})

	t.Run("top-level raw=true merges onto structured_message", func(t *testing.T) {
		srv, _, _, _, target, _, dispatcher := deliverySetup(t)
		enableReadSwitch(t, srv)

		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg": "Enter",
			},
			"raw": true,
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/"+target.ID+"/message",
			bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(tid("user-1"), "alice@example.com", "Alice", "admin", "cli")))

		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, req, target.ID)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Raw, "top-level raw=true must merge onto StructuredMessage.Raw")
		assert.Equal(t, "Enter", calls[0].StructuredMessage.DeliveryText)
	})
}
