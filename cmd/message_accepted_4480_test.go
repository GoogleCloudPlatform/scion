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
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4480: a single agent send confirms the Hub accepted the
// message, never that it was delivered, for every Hub status except
// "deferred" (which keeps its reincarnation notice) and a failed send (an
// error, with no confirmation).

const accepted4480Project = "project-accepted-4480"

// newAccepted4480Hub is a fake hub whose agent-message endpoints answer every
// send with the given HTTP status, delivery status and error code.
//
// Like the real hub, it sends no message_id for "delivered_not_persisted",
// where the message was not saved.
func newAccepted4480Hub(t *testing.T, httpStatus int, status, errCode string) *HubContext {
	t.Helper()
	msgID := "m-4480"
	if status == "delivered_not_persisted" {
		msgID = ""
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messaging/targets/resolve":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{
					"id": "target-uuid-4480", "slug": r.URL.Query().Get("agent"),
					"projectId": "proj-b-uuid", "projectSlug": r.URL.Query().Get("project"),
				},
				"messageability": map[string]interface{}{"canMessage": true},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			w.WriteHeader(httpStatus)
			if errCode != "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": errCode, "message": "dispatch failed"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": msgID, "status": status, "agent": "builder",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: accepted4480Project}
}

// accepted4480Sends are the single agent send paths, each with the target
// wording its confirmation uses.
var accepted4480Sends = []struct {
	name   string
	sender string
	target string
	// deferredName is the agent as the deferred notice names it.
	deferredName string
	send         func(*HubContext) error
}{
	{
		name: "agent direct send", sender: "sender-agent", target: "'builder'", deferredName: "builder",
		send: func(h *HubContext) error { return sendMessageViaHub(h, "builder", "hello", false, false, false) },
	},
	{
		name: "human direct send", sender: "", target: "'builder'", deferredName: "builder",
		send: func(h *HubContext) error { return sendMessageViaHub(h, "builder", "hello", false, false, false) },
	},
	{
		name: "agent @agent ref", sender: "sender-agent", target: "'builder'", deferredName: "builder",
		send: func(h *HubContext) error {
			ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}
			return sendMessageViaConversation(h, ref, "hello", false, false, nil)
		},
	},
	{
		name: "human @agent ref", sender: "", target: "'builder'", deferredName: "builder",
		send: func(h *HubContext) error {
			ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}
			return sendMessageViaConversation(h, ref, "hello", false, false, nil)
		},
	},
	{
		name: "cross-project send", sender: "sender-agent", target: "'builder' in project 'project-b'", deferredName: "builder in project 'project-b'",
		send: func(h *HubContext) error {
			return sendCrossProjectMessage(h, "project-b", "builder", "hello", false, false, nil)
		},
	},
}

func TestMessageAccepted4480_StatusWording(t *testing.T) {
	statuses := []struct {
		status     string
		httpStatus int
		suffix     string
	}{
		{status: "dispatched", httpStatus: http.StatusOK, suffix: " (message m-4480)."},
		{status: "delivered", httpStatus: http.StatusOK, suffix: " (message m-4480)."},
		{status: "delivered_not_persisted", httpStatus: http.StatusOK, suffix: "."},
		{status: "ambiguous", httpStatus: http.StatusAccepted, suffix: " (delivery unconfirmed) (message m-4480)."},
		{status: "deduplicated", httpStatus: http.StatusOK, suffix: " (duplicate of an earlier send) (message m-4480)."},
	}
	for _, st := range statuses {
		for _, sc := range accepted4480Sends {
			t.Run(st.status+"/"+sc.name, func(t *testing.T) {
				msgID3881State(t, sc.sender)
				hubCtx := newAccepted4480Hub(t, st.httpStatus, st.status, "")
				var err error
				out := captureStdout(t, func() { err = sc.send(hubCtx) })
				require.NoError(t, err)
				assert.Equal(t, "Message accepted for agent "+sc.target+st.suffix, lastLine(out))
				assert.NotContains(t, out, "delivered")
				if st.status == "delivered_not_persisted" {
					assert.NotContains(t, out, "(message ")
				}
			})
		}
	}
}

// A deferred send keeps its distinct reincarnation notice on every path.
func TestMessageAccepted4480_DeferredKeepsNotice(t *testing.T) {
	for _, sc := range accepted4480Sends {
		t.Run(sc.name, func(t *testing.T) {
			msgID3881State(t, sc.sender)
			hubCtx := newAccepted4480Hub(t, http.StatusAccepted, "deferred", "")
			var err error
			out := captureStdout(t, func() { err = sc.send(hubCtx) })
			require.NoError(t, err)
			assert.Equal(t, "agent "+sc.deferredName+" is reincarnating; message saved to history and will be seen on catch-up (message m-4480).", lastLine(out))
			assert.NotContains(t, out, "accepted")
		})
	}
}

// A failed send is an error and prints no confirmation.
func TestMessageAccepted4480_FailedPrintsNoConfirmation(t *testing.T) {
	for _, sc := range accepted4480Sends {
		t.Run(sc.name, func(t *testing.T) {
			msgID3881State(t, sc.sender)
			hubCtx := newAccepted4480Hub(t, http.StatusBadGateway, "", hubCodeDeliveryFailed)
			var err error
			out := captureStdout(t, func() { err = sc.send(hubCtx) })
			require.Error(t, err)
			assert.NotContains(t, out, "Message accepted")
			assert.NotContains(t, out, "delivered")
		})
	}
}

func TestAgentAcceptedLine(t *testing.T) {
	assert.Equal(t, "Message accepted for agent 'x'.", agentAcceptedLine("'x'", nil))
	assert.Equal(t, "Message accepted for agent 'x' (message m1).",
		agentAcceptedLine("'x'", &hubclient.MessageResponse{MessageID: "m1", Status: "dispatched"}))
	assert.Equal(t, "Message accepted for agent 'x' (duplicate of an earlier send) (message m1).",
		agentAcceptedLine("'x'", &hubclient.MessageResponse{MessageID: "m1", Status: "deduplicated"}))
	assert.Equal(t, "Message accepted for agent 'x' (delivery unconfirmed) (message m1).",
		agentAcceptedLine("'x'", &hubclient.MessageResponse{MessageID: "m1", Status: "ambiguous"}))
	assert.Equal(t, "Message accepted for agent 'x'.",
		agentAcceptedLine("'x'", &hubclient.MessageResponse{Status: "delivered_not_persisted"}))
}
