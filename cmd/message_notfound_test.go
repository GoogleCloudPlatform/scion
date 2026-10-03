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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// newAgentNotFoundHub returns a hub whose project-scoped agent message
// endpoint answers 404 agent_not_found, as the real hub does for a deleted or
// reaped agent (pkg/hub/handlers_projects_core.go).
func newAgentNotFoundHub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"agent_not_found","message":"Agent \"ghost\" not found in project"}}`))
	}))
}

// assertAgentNotFoundRendering checks the same-project 404 rendering for
// ptone/scion#631: the error names the agent as not found, carries no
// local-only hint, does not trigger the Usage block, and is returned (so
// Execute exits 1).
func assertAgentNotFoundRendering(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err, "a send to a missing agent must return an error (non-zero exit)")
	msg := err.Error()
	assert.True(t, strings.HasPrefix(msg, "agent 'ghost' not found"), "error should lead with the missing agent: %q", msg)
	assert.Contains(t, msg, `Agent "ghost" not found in project`, "hub message should be kept as the cause")
	assert.NotContains(t, msg, "scion hub disable", "a 404 must not suggest local-only mode")
	assert.True(t, apiclient.IsNotFoundError(err), "the APIError should remain reachable via errors.As")
	assert.False(t, showUsageForError(messageCmd, err, true), "a hub 404 must not print the Usage block")
}

func TestSendMessageViaHub_AgentNotFound(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv("SCION_AGENT_NAME", "")

	server := newAgentNotFoundHub(t)
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-631"}

	err = sendMessageViaHub(hubCtx, "ghost", "hello", false, false, false)
	assertAgentNotFoundRendering(t, err)
}

func TestSendMessageViaConversation_AgentNotFound(t *testing.T) {
	ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "ghost", Raw: "@ghost"}

	t.Run("human CLI @agent", func(t *testing.T) {
		orig := saveMessageTestState()
		defer orig.restore()
		t.Setenv("SCION_AGENT_ID", "")
		t.Setenv("SCION_AGENT_NAME", "")

		server := newAgentNotFoundHub(t)
		defer server.Close()
		client, err := hubclient.New(server.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-631"}

		err = sendMessageViaConversation(hubCtx, ref, "hello", false, false, nil)
		assertAgentNotFoundRendering(t, err)
	})

	t.Run("agent context @agent", func(t *testing.T) {
		orig := saveMessageTestState()
		defer orig.restore()
		t.Setenv("SCION_AGENT_ID", "sender-uuid")
		t.Setenv("SCION_AGENT_NAME", "sender-agent")

		server := newAgentNotFoundHub(t)
		defer server.Close()
		client, err := hubclient.New(server.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-631"}

		err = sendMessageViaConversation(hubCtx, ref, "hello", false, false, nil)
		assertAgentNotFoundRendering(t, err)
	})
}
