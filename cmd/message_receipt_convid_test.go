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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A send receipt names the conversation the message landed in as well as
// the message, so the sender can reply into that conversation.

const receiptConvProject = "project-receipt-conv"

// newReceiptConvHub is a fake hub whose send endpoints answer with a message
// ID and a conversation ID derived from the recipient.
func newReceiptConvHub(t *testing.T) *HubContext {
	t.Helper()
	projectPrefix := "/api/v1/projects/" + receiptConvProject + "/agents/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messaging/targets/resolve":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{
					"id": "target-uuid-rc", "slug": r.URL.Query().Get("agent"),
					"projectId": "proj-b-uuid", "projectSlug": r.URL.Query().Get("project"),
				},
				"messageability": map[string]interface{}{"canMessage": true},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": "msg-out", "conversation_id": "conv-out", "status": "sent",
				"recipient": "user:alice@example.com", "recipient_id": "uid-alice",
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, projectPrefix) && strings.HasSuffix(r.URL.Path, "/message"):
			slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, projectPrefix), "/message")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": "msg-" + slug, "conversation_id": "conv-" + slug,
				"status": "delivered", "agent": slug,
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/agents/"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": "msg-xproj", "conversation_id": "conv-xproj",
				"status": "delivered", "agent": "target-agent",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: receiptConvProject}
}

func TestSendReceipt_ConfirmationsNameConversation(t *testing.T) {
	cases := []struct {
		name   string
		sender string
		send   func(*HubContext) error
		want   string
	}{
		{
			name:   "direct agent send",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				return sendMessageViaHub(h, "builder", "hello", false, false, false)
			},
			want: "Message accepted for agent 'builder' (conversation conv-builder, message msg-builder).",
		},
		{
			name:   "human direct agent send",
			sender: "",
			send: func(h *HubContext) error {
				return sendMessageViaHub(h, "builder", "hello", false, false, false)
			},
			want: "Message accepted for agent 'builder' (conversation conv-builder, message msg-builder).",
		},
		{
			name:   "conversation @agent ref",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}
				return sendMessageViaConversation(h, ref, "hello", false, false, nil)
			},
			want: "Message accepted for agent 'builder' (conversation conv-builder, message msg-builder).",
		},
		{
			name:   "conv: ref",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
				return sendMessageViaConversation(h, ref, "hello", false, false, nil)
			},
			want: "Message sent to conv:11111111-1111-1111-1111-111111111111 (conversation conv-out, message msg-out).",
		},
		{
			name:   "user recipient",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				return sendOutboundMessageViaHub(h, "user:alice@example.com", "hello", false)
			},
			want: "Message sent to user:alice@example.com via Hub (conversation conv-out, message msg-out).",
		},
		{
			name:   "cross-project agent send",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				return sendCrossProjectMessage(h, "project-b", "target-agent", "hello", false, false, nil)
			},
			want: "Message accepted for agent 'target-agent' in project 'project-b' (conversation conv-xproj, message msg-xproj).",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgID3881State(t, tc.sender)
			hubCtx := newReceiptConvHub(t)
			var err error
			out := captureStdout(t, func() { err = tc.send(hubCtx) })
			require.NoError(t, err)
			assert.Equal(t, tc.want, lastLine(out))
		})
	}
}

// A group send prints the conversation and message of each recipient's copy.
func TestSendReceipt_GroupLinesNameConversation(t *testing.T) {
	msgID3881State(t, "sender-agent")
	hubCtx := newReceiptConvHub(t)
	recips := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientUser, Name: "alice"},
	}
	var err error
	out := captureStdout(t, func() { err = sendGroupMessageViaHub(hubCtx, recips, "hi", false) })
	require.NoError(t, err)
	assert.Contains(t, out, "  Accepted: agent:agent-a (conversation conv-agent-a, message msg-agent-a)\n")
	assert.Contains(t, out, "  Accepted: user:alice (conversation conv-out, message msg-out)\n")
}
