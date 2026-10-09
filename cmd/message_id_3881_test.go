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

// ptone/scion#3881: every successful scion message send prints the ID of the
// message the hub created, as a " (message <id>)" suffix.

const msgID3881Project = "project-msgid-3881"

// newMsgID3881Hub is a fake hub whose send endpoints answer with a message
// ID derived from the recipient. With noID set, the responses carry no
// message_id, as an older hub's would.
func newMsgID3881Hub(t *testing.T, noID bool) *HubContext {
	t.Helper()
	id := func(v string) string {
		if noID {
			return ""
		}
		return v
	}
	projectPrefix := "/api/v1/projects/" + msgID3881Project + "/agents/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messaging/targets/resolve":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{
					"id": "target-uuid-3881", "slug": r.URL.Query().Get("agent"),
					"projectId": "proj-b-uuid", "projectSlug": r.URL.Query().Get("project"),
				},
				"messageability": map[string]interface{}{"canMessage": true},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": id("msg-3881-out"), "status": "sent",
				"recipient": "user:alice@example.com", "recipient_id": "uid-alice",
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, projectPrefix) && strings.HasSuffix(r.URL.Path, "/message"):
			slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, projectPrefix), "/message")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": id("msg-3881-" + slug), "status": "delivered", "agent": slug,
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/agents/"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": id("msg-3881-xproj"), "status": "delivered", "agent": "target-agent",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: msgID3881Project}
}

func msgID3881State(t *testing.T, senderAgent string) {
	t.Helper()
	orig := saveMessageTestState()
	restoreFlags := resetMessageFlags()
	origFormat, origCC := outputFormat, msgCC
	outputFormat, msgCC = "", nil
	t.Cleanup(func() {
		restoreFlags()
		orig.restore()
		outputFormat, msgCC = origFormat, origCC
	})
	t.Setenv("SCION_AGENT_NAME", senderAgent)
}

func TestMessageID3881_ConfirmationsAppendMessageID(t *testing.T) {
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
			want: "Message delivered to agent 'builder' (message msg-3881-builder).",
		},
		{
			name:   "human direct agent send",
			sender: "",
			send: func(h *HubContext) error {
				return sendMessageViaHub(h, "builder", "hello", false, false, false)
			},
			want: "Message delivered to agent 'builder' (message msg-3881-builder).",
		},
		{
			name:   "conversation @agent ref from an agent",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}
				return sendMessageViaConversation(h, ref, "hello", false, false, nil)
			},
			want: "Message delivered to agent 'builder' (message msg-3881-builder).",
		},
		{
			name:   "conversation @agent ref from a human",
			sender: "",
			send: func(h *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}
				return sendMessageViaConversation(h, ref, "hello", false, false, nil)
			},
			want: "Message delivered to agent 'builder' (message msg-3881-builder).",
		},
		{
			name:   "conv: ref",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
				return sendMessageViaConversation(h, ref, "hello", false, false, nil)
			},
			want: "Message sent to conv:11111111-1111-1111-1111-111111111111 (message msg-3881-out).",
		},
		{
			name:   "user recipient",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				return sendOutboundMessageViaHub(h, "user:alice@example.com", "hello", false)
			},
			want: "Message sent to user:alice@example.com via Hub (message msg-3881-out).",
		},
		{
			name:   "cross-project agent send",
			sender: "sender-agent",
			send: func(h *HubContext) error {
				return sendCrossProjectMessage(h, "project-b", "target-agent", "hello", false, false, nil)
			},
			want: "Message delivered to agent 'target-agent' in project 'project-b' (message msg-3881-xproj).",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgID3881State(t, tc.sender)
			hubCtx := newMsgID3881Hub(t, false)
			var err error
			out := captureStdout(t, func() { err = tc.send(hubCtx) })
			require.NoError(t, err)
			assert.Equal(t, tc.want, lastLine(out))
		})
	}
}

// A hub that reports no message ID (an older hub) leaves the confirmation
// exactly as it was before the ID was appended.
func TestMessageID3881_NoMessageIDKeepsConfirmation(t *testing.T) {
	cases := []struct {
		name string
		send func(*HubContext) error
		want string
	}{
		{
			name: "direct agent send",
			send: func(h *HubContext) error {
				return sendMessageViaHub(h, "builder", "hello", false, false, false)
			},
			want: "Message delivered to agent 'builder'.",
		},
		{
			name: "user recipient",
			send: func(h *HubContext) error {
				return sendOutboundMessageViaHub(h, "user:alice@example.com", "hello", false)
			},
			want: "Message sent to user:alice@example.com via Hub.",
		},
		{
			name: "cross-project agent send",
			send: func(h *HubContext) error {
				return sendCrossProjectMessage(h, "project-b", "target-agent", "hello", false, false, nil)
			},
			want: "Message delivered to agent 'target-agent' in project 'project-b'.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgID3881State(t, "sender-agent")
			hubCtx := newMsgID3881Hub(t, true)
			var err error
			out := captureStdout(t, func() { err = tc.send(hubCtx) })
			require.NoError(t, err)
			assert.Equal(t, tc.want, lastLine(out))
			assert.NotContains(t, out, "(message ")
		})
	}
}

func TestMessageID3881_GroupSendPrintsPerRecipientMessageID(t *testing.T) {
	groupTestState(t, "")
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a":    ok(),
		"agent-b":    {status: http.StatusAccepted, respStatus: "deferred"},
		"user:alice": ok(),
	})
	recips := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientAgent, Name: "agent-b"},
		{Kind: messages.RecipientUser, Name: "alice"},
	}

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(h.hubCtx(t), recips, "hi", false)
	})
	require.NoError(t, sendErr)
	assert.Contains(t, out, "  Delivered: agent:agent-a (message m-agent-a)\n")
	assert.Contains(t, out, "  Deferred: agent:agent-b (agent is reincarnating; saved) (message m-agent-b)\n")
	assert.Contains(t, out, "  Delivered: user:alice (message m-user:alice)\n")
	assert.Equal(t, "Group delivery complete: 2/3 delivered, 1 deferred.", lastLine(out))
}

func TestMessageID3881_GroupSendJSONCarriesMessageID(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a": ok(),
		"agent-b": hubErr(http.StatusForbidden),
	})

	got, sendErr := runGroupJSON(t, h, agentRecipients("agent-a", "agent-b"))
	require.Error(t, sendErr)
	require.Len(t, got.Results, 2)
	assert.Equal(t, "m-agent-a", got.Results[0].MessageID)
	assert.Empty(t, got.Results[1].MessageID, "a failed send has no message ID")
}

// An ambiguous (unknown) group result keeps the stored message's ID in
// message_id, so a JSON consumer can look it up without parsing the error.
func TestMessageID3881_GroupAmbiguousJSONCarriesMessageID(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a":   ok(),
		"agent-amb": ambiguous(),
	})

	got, sendErr := runGroupJSON(t, h, agentRecipients("agent-a", "agent-amb"))
	require.Error(t, sendErr)
	require.Len(t, got.Results, 2)
	assert.Equal(t, "unknown", got.Results[1].Status)
	assert.Equal(t, "m-agent-amb", got.Results[1].MessageID)
}
