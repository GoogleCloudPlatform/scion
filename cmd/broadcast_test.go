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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroadcastCmd_ProjectScoped(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-bcast-project"
	agents := []hubclient.Agent{
		{Name: "agent-1", Status: "running"},
		{Name: "agent-2", Status: "running"},
	}
	server, sent := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	// Save and restore broadcast-specific globals
	origBcastAll := bcastAll
	origBcastInterrupt := bcastInterrupt
	defer func() {
		bcastAll = origBcastAll
		bcastInterrupt = origBcastInterrupt
	}()
	bcastAll = false
	bcastInterrupt = false

	err = broadcastViaHub(hubCtx, "deploy starting")
	require.NoError(t, err)

	// Project-scoped broadcast uses the broadcast endpoint
	require.Len(t, *sent, 2)
	for _, s := range *sent {
		assert.Equal(t, "deploy starting", s.Message)
		require.NotNil(t, s.StructuredMsg)
		assert.True(t, s.StructuredMsg.Broadcasted)
	}
}

func TestBroadcastCmd_GlobalAll(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-bcast-all"
	agents := []hubclient.Agent{
		{Name: "cross-1", Status: "running", ProjectID: "proj-a"},
		{Name: "cross-2", Status: "running", ProjectID: "proj-b"},
	}
	server, sent := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
	}

	origBcastAll := bcastAll
	origBcastInterrupt := bcastInterrupt
	defer func() {
		bcastAll = origBcastAll
		bcastInterrupt = origBcastInterrupt
	}()
	bcastAll = true
	bcastInterrupt = false

	err = broadcastViaHub(hubCtx, "global broadcast")
	require.NoError(t, err)

	require.Len(t, *sent, 2)
	for _, s := range *sent {
		assert.Equal(t, "global broadcast", s.Message)
	}
}

func TestBroadcastCmd_NoAgents(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-bcast-empty"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/"+projectID+"/broadcast":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "accepted",
				"total":    0,
				"targeted": 0,
				"skipped":  0,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	origBcastAll := bcastAll
	defer func() { bcastAll = origBcastAll }()
	bcastAll = false

	err = broadcastViaHub(hubCtx, "hello")
	require.NoError(t, err)
}

func TestBroadcastCmd_WithInterrupt(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "proj-bcast-interrupt"
	agents := []hubclient.Agent{
		{Name: "agent-1", Status: "running"},
	}

	var receivedInterrupt bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects/"+projectID+"/broadcast":
			var body struct {
				Interrupt         bool                        `json:"interrupt"`
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedInterrupt = body.Interrupt
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "accepted",
				"total":    len(agents),
				"targeted": len(agents),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	origBcastAll := bcastAll
	origBcastInterrupt := bcastInterrupt
	defer func() {
		bcastAll = origBcastAll
		bcastInterrupt = origBcastInterrupt
	}()
	bcastAll = false
	bcastInterrupt = true

	err = broadcastViaHub(hubCtx, "stop now")
	require.NoError(t, err)
	assert.True(t, receivedInterrupt)
}

func TestBroadcastCmd_BuildMessage(t *testing.T) {
	origBcastInterrupt := bcastInterrupt
	origBcastVisibility := bcastVisibility
	defer func() {
		bcastInterrupt = origBcastInterrupt
		bcastVisibility = origBcastVisibility
	}()

	bcastInterrupt = true
	bcastVisibility = "verbose"

	msg := buildBroadcastMessage("user:alice", "stop everything")
	assert.Equal(t, "user:alice", msg.Sender)
	assert.Equal(t, "", msg.Recipient)
	assert.Equal(t, "stop everything", msg.Msg)
	assert.True(t, msg.Broadcasted)
	assert.True(t, msg.Urgent)
	assert.Equal(t, "verbose", msg.Visibility)
}
