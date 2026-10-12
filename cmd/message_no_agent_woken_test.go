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

// A group post that reached no agent gets a one-line notice from the CLI
// (ptone/scion#4605).

// newNoAgentWokenHub answers every outbound-message send of projectID with
// the given extra response fields.
func newNoAgentWokenHub(t *testing.T, projectID string, extra map[string]interface{}) *HubContext {
	t.Helper()
	prefix := "/api/v1/projects/" + projectID + "/agents/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			body := map[string]interface{}{
				"message_id":      "msg-group",
				"status":          "sent",
				"conversation_id": "conv-group",
			}
			for k, v := range extra {
				body[k] = v
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}
}

func sendGroupPostForNotice(t *testing.T, hubCtx *HubContext) (string, string) {
	t.Helper()
	orig := saveMessageTestState()
	origFormat := outputFormat
	t.Cleanup(func() {
		orig.restore()
		outputFormat = origFormat
	})
	outputFormat = ""
	t.Setenv("SCION_AGENT_NAME", "sender")
	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "conv-group", Raw: "conv:conv-group"}
	var sendErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		sendErr = sendMessageViaConversation(hubCtx, ref, "status update", false, false, nil)
	})
	require.NoError(t, sendErr)
	return stdout, stderr
}

func TestSendMessageViaConversation_NoAgentWokenNotice(t *testing.T) {
	hubCtx := newNoAgentWokenHub(t, "proj-no-agent-woken", map[string]interface{}{
		"no_agent_woken": true,
	})
	stdout, stderr := sendGroupPostForNotice(t, hubCtx)
	assert.Contains(t, stdout, "Message sent to conv:conv-group")
	assert.Contains(t, stderr, "No agent was woken: mention an agent or set a default agent for this conversation.")
}

func TestSendMessageViaConversation_NoAgentWokenNoticeNamesDefault(t *testing.T) {
	hubCtx := newNoAgentWokenHub(t, "proj-no-agent-woken-default", map[string]interface{}{
		"no_agent_woken":       true,
		"default_agent_result": map[string]string{"slug": "lead", "status": "unauthorized"},
	})
	_, stderr := sendGroupPostForNotice(t, hubCtx)
	assert.Contains(t, stderr, "No agent was woken: default agent @lead was not reached (unauthorized).")
}

func TestSendMessageViaConversation_NoNoticeWhenAgentWoken(t *testing.T) {
	hubCtx := newNoAgentWokenHub(t, "proj-agent-woken", map[string]interface{}{
		"woken_agents":         []string{"lead"},
		"default_agent_result": map[string]string{"slug": "lead", "status": "delivered"},
	})
	_, stderr := sendGroupPostForNotice(t, hubCtx)
	assert.NotContains(t, stderr, "No agent was woken")
}
