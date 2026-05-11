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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messageTestState captures and restores package-level vars for test isolation.
type messageTestState struct {
	projectPath string
	noHub     bool
}

func saveMessageTestState() messageTestState {
	return messageTestState{
		projectPath: projectPath,
		noHub:     noHub,
	}
}

func (s messageTestState) restore() {
	projectPath = s.projectPath
	noHub = s.noHub
}

// messageMockServer creates a mock Hub server that handles grove-scoped
// agent message and list requests. Returns the server, a pointer to a slice of
// messages sent (as agent-name strings), and a configurable list of agents
// returned by the list endpoint.
type sentMessage struct {
	AgentName string
	Message   string
	Interrupt bool
	// Structured message fields (new)
	StructuredMsg *messages.StructuredMessage
}

func newMessageMockHubServer(t *testing.T, projectID string, runningAgents []hubclient.Agent) (*httptest.Server, *[]sentMessage) {
	t.Helper()
	var sent []sentMessage
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/groves/"+projectID+"/agents" || r.URL.Path == "/api/v1/agents"):
			// List agents endpoint
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": runningAgents,
			})

		case r.Method == http.MethodPost:
			// Extract agent name from path: /api/v1/groves/<projectID>/agents/<name>/message
			// or /api/v1/agents/<name>/message
			var agentName string
			grovePrefix := "/api/v1/groves/" + projectID + "/agents/"
			globalPrefix := "/api/v1/agents/"
			path := r.URL.Path
			if len(path) > len(grovePrefix) && path[:len(grovePrefix)] == grovePrefix {
				rest := path[len(grovePrefix):]
				// rest is "<name>/message"
				agentName = rest[:len(rest)-len("/message")]
			} else if len(path) > len(globalPrefix) && path[:len(globalPrefix)] == globalPrefix {
				rest := path[len(globalPrefix):]
				agentName = rest[:len(rest)-len("/message")]
			}

			var body struct {
				Message           string                      `json:"message"`
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			sm := sentMessage{
				AgentName:     agentName,
				Interrupt:     body.Interrupt,
				StructuredMsg: body.StructuredMessage,
			}
			// Extract message text from structured message if present
			if body.StructuredMessage != nil {
				sm.Message = body.StructuredMessage.Msg
			} else {
				sm.Message = body.Message
			}

			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &sent
}

func TestSendMessageViaHub_SingleAgent(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-single"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello world", false, false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.Equal(t, "hello world", (*sent)[0].Message)
	assert.False(t, (*sent)[0].Interrupt)
	// Verify structured message fields
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, messages.TypeInstruction, (*sent)[0].StructuredMsg.Type)
	assert.Equal(t, "agent:my-agent", (*sent)[0].StructuredMsg.Recipient)
}

func TestSendMessageViaHub_SingleAgentInterrupt(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-int"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Set interrupt flag for this test
	origInterrupt := msgInterrupt
	msgInterrupt = true
	defer func() { msgInterrupt = origInterrupt }()

	err = sendMessageViaHub(hubCtx, "my-agent", "urgent", true, false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.True(t, (*sent)[0].Interrupt)
	// Verify urgent flag is set in structured message
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.True(t, (*sent)[0].StructuredMsg.Urgent)
}

func TestSendMessageViaHub_Broadcast(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-broadcast"
	agents := []hubclient.Agent{
		{Name: "agent-1", Status: "running"},
		{Name: "agent-2", Status: "running"},
		{Name: "agent-3", Status: "running"},
	}
	server, sent := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Set broadcast flag for structured message construction
	origBroadcast := msgBroadcast
	msgBroadcast = true
	defer func() { msgBroadcast = origBroadcast }()

	err = sendMessageViaHub(hubCtx, "", "broadcast msg", false, true, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 3)
	names := make([]string, len(*sent))
	for i, s := range *sent {
		names[i] = s.AgentName
		assert.Equal(t, "broadcast msg", s.Message)
		// Verify broadcast flag in structured message
		require.NotNil(t, s.StructuredMsg)
		assert.True(t, s.StructuredMsg.Broadcasted)
	}
	assert.ElementsMatch(t, []string{"agent-1", "agent-2", "agent-3"}, names)
}

func TestSendMessageViaHub_BroadcastNoAgents(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-empty"
	server, sent := newMessageMockHubServer(t, projectID, []hubclient.Agent{})
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = sendMessageViaHub(hubCtx, "", "hello", false, true, false, false)
	require.NoError(t, err)

	// No messages should be sent
	assert.Len(t, *sent, 0)
}

func TestSendMessageViaHub_All(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-all"
	agents := []hubclient.Agent{
		{Name: "grove1-agent", Status: "running", ProjectID: "grove-a"},
		{Name: "grove2-agent", Status: "running", ProjectID: "grove-b"},
	}
	server, sent := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	// For --all mode, we use global agent service (no grove scoping)
	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
	}

	err = sendMessageViaHub(hubCtx, "", "all msg", false, false, true, false)
	require.NoError(t, err)

	require.Len(t, *sent, 2)
	for _, s := range *sent {
		assert.Equal(t, "all msg", s.Message)
	}
}

func TestSendMessageViaHub_SingleAgentError(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-err"

	// Server that returns 500 for message requests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "internal",
				"message": "internal error",
			},
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, false)
	require.Error(t, err, "single-agent message failure should return an error")
}

func TestScheduleMessageFlagValidation(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		at        string
		broadcast bool
		all       bool
		wantErr   string
	}{
		{
			name:    "in and at are mutually exclusive",
			in:      "30m",
			at:      "2030-01-01T00:00:00Z",
			wantErr: "--in and --at are mutually exclusive",
		},
		{
			name:      "in with broadcast not allowed",
			in:        "30m",
			broadcast: true,
			wantErr:   "--in/--at cannot be combined with --broadcast or --all",
		},
		{
			name:    "at with all not allowed",
			at:      "2030-01-01T00:00:00Z",
			all:     true,
			wantErr: "--in/--at cannot be combined with --broadcast or --all",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Save and restore global state
			origIn, origAt := msgIn, msgAt
			origBroadcast, origAll := msgBroadcast, msgAll
			defer func() {
				msgIn, msgAt = origIn, origAt
				msgBroadcast, msgAll = origBroadcast, origAll
			}()

			msgIn = tc.in
			msgAt = tc.at
			msgBroadcast = tc.broadcast
			msgAll = tc.all

			// Build args appropriate for the flag combination
			var args []string
			if tc.broadcast || tc.all {
				args = []string{"hello"}
			} else {
				args = []string{"agent1", "hello"}
			}

			err := messageCmd.RunE(messageCmd, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestSendMessageViaHub_BroadcastPartialFailure(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-partial"
	agents := []hubclient.Agent{
		{Name: "good-agent", Status: "running"},
		{Name: "bad-agent", Status: "running"},
	}

	var sent []sentMessage
	var mu sync.Mutex
	// Server that succeeds for good-agent but fails for bad-agent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
		case r.Method == http.MethodPost:
			prefix := "/api/v1/groves/" + projectID + "/agents/"
			rest := r.URL.Path[len(prefix):]
			agentName := rest[:len(rest)-len("/message")]

			if agentName == "bad-agent" {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "internal", "message": "error"},
				})
				return
			}

			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Message           string                      `json:"message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			msg := body.Message
			if body.StructuredMessage != nil {
				msg = body.StructuredMessage.Msg
			}
			mu.Lock()
			sent = append(sent, sentMessage{AgentName: agentName, Message: msg})
			mu.Unlock()

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Broadcast should not return an error on partial failure
	err = sendMessageViaHub(hubCtx, "", "test", false, true, false, false)
	require.NoError(t, err)

	// Only the good agent should have received the message
	assert.Len(t, sent, 1)
	assert.Equal(t, "good-agent", sent[0].AgentName)
}

func TestResolveSenderIdentity_AgentContext(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "test-worker")
	hubCtx := &HubContext{}
	got := resolveSenderIdentity(hubCtx)
	assert.Equal(t, "agent:test-worker", got)
}

func TestResolveSenderIdentity_NoContext(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "")

	// With no Hub auth and no agent env, should fall back to user:unknown
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client, _ := hubclient.New(server.URL)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	got := resolveSenderIdentity(hubCtx)
	assert.Equal(t, "user:unknown", got)
}

func TestBuildStructuredMessage(t *testing.T) {
	// Save and restore global state
	origPlain, origInterrupt := msgPlain, msgInterrupt
	origBroadcast, origAll := msgBroadcast, msgAll
	origAttach := msgAttach
	defer func() {
		msgPlain = origPlain
		msgInterrupt = origInterrupt
		msgBroadcast = origBroadcast
		msgAll = origAll
		msgAttach = origAttach
	}()

	msgPlain = false
	msgInterrupt = true
	msgBroadcast = true
	msgAll = false
	msgAttach = []string{"file1.go", "file2.go"}

	msg := buildStructuredMessage("user:alice", "agent:dev", "do something")

	assert.Equal(t, messages.Version, msg.Version)
	assert.Equal(t, "user:alice", msg.Sender)
	assert.Equal(t, "agent:dev", msg.Recipient)
	assert.Equal(t, "do something", msg.Msg)
	assert.Equal(t, messages.TypeInstruction, msg.Type)
	assert.False(t, msg.Plain)
	assert.True(t, msg.Urgent)
	assert.True(t, msg.Broadcasted)
	assert.Equal(t, []string{"file1.go", "file2.go"}, msg.Attachments)
}

func TestSendMessageViaHub_NotifyFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-notify"

	var notifyReceived bool
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Notify            bool                        `json:"notify"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notifyReceived = body.Notify
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, true)
	require.NoError(t, err)

	mu.Lock()
	assert.True(t, notifyReceived, "notify should be true by default")
	mu.Unlock()
}

func TestSendMessageViaHub_NoNotifyFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-no-notify"

	var notifyReceived bool
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Notify            bool                        `json:"notify"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notifyReceived = body.Notify
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Explicit --no-notify: notify should be false
	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, false)
	require.NoError(t, err)

	mu.Lock()
	assert.False(t, notifyReceived, "notify should be false when --no-notify is used")

	mu.Unlock()
}

func TestSendOutboundMessageViaHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-outbound"

	var receivedMsg *hubclient.OutboundMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/groves/"+projectID+"/agents/my-agent/outbound-message":
			var msg hubclient.OutboundMessageRequest
			json.NewDecoder(r.Body).Decode(&msg)
			receivedMsg = &msg
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	t.Setenv("SCION_AGENT_NAME", "my-agent")

	err = sendOutboundMessageViaHub(hubCtx, "user:alice", "I need help", false)
	require.NoError(t, err)

	require.NotNil(t, receivedMsg)
	assert.Equal(t, "user:alice", receivedMsg.Recipient)
	assert.Equal(t, "I need help", receivedMsg.Msg)
	assert.Equal(t, "instruction", receivedMsg.Type)
	assert.False(t, receivedMsg.Urgent)
}

func TestSendOutboundMessageViaHub_RequiresAgentContext(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  "grove-test",
	}

	t.Setenv("SCION_AGENT_NAME", "")

	err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SCION_AGENT_NAME not set")
}

func TestUserRecipientFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		raw     bool
		in      string
		wantErr string
	}{
		{
			name:    "raw with user recipient not allowed",
			args:    []string{"user:alice", "hello"},
			raw:     true,
			wantErr: "--raw cannot be used with user recipients",
		},
		{
			name:    "scheduled with user recipient not allowed",
			args:    []string{"user:alice", "hello"},
			in:      "30m",
			wantErr: "--in/--at cannot be used with user recipients",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origRaw := msgRaw
			origIn := msgIn
			defer func() {
				msgRaw = origRaw
				msgIn = origIn
			}()

			msgRaw = tc.raw
			msgIn = tc.in

			err := messageCmd.RunE(messageCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNotifyFlagValidation(t *testing.T) {
	tests := []struct {
		name      string
		notify    bool
		broadcast bool
		all       bool
		wantErr   string
	}{
		{
			name:      "notify with broadcast not allowed",
			notify:    true,
			broadcast: true,
			wantErr:   "--notify cannot be combined with --broadcast or --all",
		},
		{
			name:    "notify with all not allowed",
			notify:  true,
			all:     true,
			wantErr: "--notify cannot be combined with --broadcast or --all",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origNotify := msgNotify
			origBroadcast, origAll := msgBroadcast, msgAll
			defer func() {
				msgNotify = origNotify
				msgBroadcast = origBroadcast
				msgAll = origAll
			}()

			msgNotify = tc.notify
			msgBroadcast = tc.broadcast
			msgAll = tc.all

			var args []string
			if tc.broadcast || tc.all {
				args = []string{"hello"}
			} else {
				args = []string{"agent1", "hello"}
			}

			err := messageCmd.RunE(messageCmd, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
