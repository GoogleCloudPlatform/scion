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
)

// ptone/scion#2026: outbound user: sends print the conversation ID when the
// hub reports it, and keep the plain line when the hub omits it.

const (
	thread2026ConvID = "11111111-2222-3333-4444-555555555555"
)

// newThread2026Hub mimics the hub's outbound-message endpoint, answering 200
// with conversation_id unless omitConvID is set.
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

	var err error
	out := captureStdout(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Message sent to user:alice via Hub (conversation "+thread2026ConvID+", message msg-2026).")
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
	assert.Contains(t, out, "Message sent to user:alice via Hub (message msg-2026).")
	assert.NotContains(t, out, "conversation", "an older hub without conversation_id gets the old line")
}
