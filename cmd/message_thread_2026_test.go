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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ptone/scion#2026: a user: send with a deprecated --thread-id must either
// land in an existing conversation (and say which one) or fail loudly.

const (
	thread2026Known    = "known-thread"
	thread2026Stale    = "stale-thread-uuid"
	thread2026ConvID   = "11111111-2222-3333-4444-555555555555"
	thread2026RejectMs = `thread_id "stale-thread-uuid" does not match an existing conversation; address the conversation with conv:<uuid> (see 'scion conversation list'), or omit thread_id to message the recipient directly`
)

// newThread2026Hub mimics the hub's outbound-message endpoint after #2026:
// a known thread or no thread answers 200 (with conversation_id unless
// omitConvID), a stale thread answers 422 unprocessable.
func newThread2026Hub(t *testing.T, projectID string, omitConvID bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/outbound-message") ||
			!strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+projectID+"/agents/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var msg hubclient.OutboundMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.ThreadID == thread2026Stale {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "unprocessable", "message": thread2026RejectMs},
			})
			return
		}
		resp := map[string]interface{}{
			"message_id":   "msg-2026",
			"status":       "sent",
			"recipient":    msg.Recipient,
			"recipient_id": "uid-2026",
		}
		if !omitConvID {
			resp["conversation_id"] = thread2026ConvID
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	return server
}

func thread2026HubCtx(t *testing.T, server *httptest.Server, projectID string) *HubContext {
	t.Helper()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
}

func TestSendOutboundMessageViaHub_PrintsConversationID(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()
	t.Setenv("SCION_AGENT_NAME", "my-agent")

	const projectID = "project-2026-print"
	hubCtx := thread2026HubCtx(t, newThread2026Hub(t, projectID, false), projectID)

	msgThreadID = thread2026Known
	var err error
	out := captureStdout(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Message sent to user:alice via Hub (conversation "+thread2026ConvID+").")
}

func TestSendOutboundMessageViaHub_NoConversationIDKeepsPlainLine(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()
	t.Setenv("SCION_AGENT_NAME", "my-agent")

	const projectID = "project-2026-plain"
	hubCtx := thread2026HubCtx(t, newThread2026Hub(t, projectID, true), projectID)

	var err error
	out := captureStdout(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Message sent to user:alice via Hub.")
	assert.NotContains(t, out, "conversation", "an older hub without conversation_id gets the old line")
}

func TestSendOutboundMessageViaHub_StaleThreadRejected(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()
	t.Setenv("SCION_AGENT_NAME", "my-agent")

	const projectID = "project-2026-stale"
	hubCtx := thread2026HubCtx(t, newThread2026Hub(t, projectID, false), projectID)

	msgThreadID = thread2026Stale
	var err error
	out := captureStdout(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	})
	require.Error(t, err)
	assert.NotContains(t, out, "Message sent", "a rejected send must not report success")
	assert.Contains(t, err.Error(), "failed to send message to user:alice")
	assert.Contains(t, err.Error(), "conv:<uuid>", "the hub's guidance must reach the user")
	assert.NotContains(t, err.Error(), "scion hub disable", "a 4xx gets no local-only hint")
	assert.True(t, isHubFailure(err), "the rejection must render as a hub failure (exit 1)")
	assert.False(t, showUsageForError(messageCmd, err, true), "no Usage block for a hub rejection")
}

// TestSendGroupMessageViaHub_StaleThreadUserMemberFails pins the group[]
// fan-out: a user member whose send the hub rejects is reported as failed,
// not delivered, and the command errors.
func TestSendGroupMessageViaHub_StaleThreadUserMemberFails(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restoreFlags := resetMessageFlags()
	defer restoreFlags()
	t.Setenv("SCION_AGENT_NAME", "my-agent")

	const projectID = "project-2026-group"
	hubCtx := thread2026HubCtx(t, newThread2026Hub(t, projectID, false), projectID)

	msgThreadID = thread2026Stale
	recipients := []messages.GroupRecipient{{Kind: messages.RecipientUser, Name: "alice"}}
	var err error
	out := captureStdout(t, func() {
		err = sendGroupMessageViaHub(hubCtx, recipients, "group hello", false)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group delivery failed")
	assert.Contains(t, out, "Failed: user:alice")
	assert.Contains(t, out, "conv:<uuid>", "the per-member failure carries the hub's reason")
	assert.NotContains(t, out, "Delivered: user:alice")
}
