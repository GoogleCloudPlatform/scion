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
	"os"
	"path/filepath"
	"strings"
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
	noHub       bool
}

func saveMessageTestState() messageTestState {
	return messageTestState{
		projectPath: projectPath,
		noHub:       noHub,
	}
}

func (s messageTestState) restore() {
	projectPath = s.projectPath
	noHub = s.noHub
}

// messageMockServer creates a mock Hub server that handles project-scoped
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/groves/"+projectID+"/agents" || r.URL.Path == "/api/v1/projects/"+projectID+"/agents" || r.URL.Path == "/api/v1/agents"):
			// List agents endpoint
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": runningAgents,
			})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/broadcast":
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, a := range runningAgents {
				sm := sentMessage{
					AgentName:     a.Name,
					StructuredMsg: body.StructuredMessage,
					Interrupt:     body.Interrupt,
				}
				if body.StructuredMessage != nil {
					sm.Message = body.StructuredMessage.Msg
				}
				mu.Lock()
				sent = append(sent, sm)
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "accepted",
				"total":    len(runningAgents),
				"targeted": len(runningAgents),
				"skipped":  0,
			})

		case r.Method == http.MethodPost:
			// Extract agent name from path: /api/v1/projects/<projectID>/agents/<name>/message
			// or /api/v1/groves/<projectID>/agents/<name>/message (legacy)
			// or /api/v1/agents/<name>/message
			var agentName string
			projectPrefix := "/api/v1/projects/" + projectID + "/agents/"
			grovePrefix := "/api/v1/groves/" + projectID + "/agents/"
			globalPrefix := "/api/v1/agents/"
			path := r.URL.Path
			if len(path) > len(projectPrefix) && path[:len(projectPrefix)] == projectPrefix {
				rest := path[len(projectPrefix):]
				agentName = rest[:len(rest)-len("/message")]
			} else if len(path) > len(grovePrefix) && path[:len(grovePrefix)] == grovePrefix {
				rest := path[len(grovePrefix):]
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
			_ = json.NewDecoder(r.Body).Decode(&body)

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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

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
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello world", false, false, false, false, false)
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
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	// Set interrupt flag for this test
	origInterrupt := msgInterrupt
	msgInterrupt = true
	defer func() { msgInterrupt = origInterrupt }()

	err = sendMessageViaHub(hubCtx, "my-agent", "urgent", true, false, false, false, false)
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
		{Name: tid("agent-1"), Status: "running"},
		{Name: "agent-2", Status: "running"},
		{Name: "agent-3", Status: "running"},
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

	// Set broadcast flag for structured message construction
	origBroadcast := msgBroadcast
	msgBroadcast = true
	defer func() { msgBroadcast = origBroadcast }()

	err = sendMessageViaHub(hubCtx, "", "broadcast msg", false, true, false, false, false)
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
	assert.ElementsMatch(t, []string{tid("agent-1"), "agent-2", "agent-3"}, names)
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
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendMessageViaHub(hubCtx, "", "hello", false, true, false, false, false)
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

	// For --all mode, we use global agent service (no project scoping)
	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
	}

	err = sendMessageViaHub(hubCtx, "", "all msg", false, false, true, false, false)
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, false, false)
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/broadcast":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "accepted",
				"total":    2,
				"targeted": 1,
				"skipped":  1,
				"skipped_breakdown": map[string]int{
					"stopped": 1,
				},
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

	// Broadcast should not return an error on partial delivery
	err = sendMessageViaHub(hubCtx, "", "test", false, true, false, false, false)
	require.NoError(t, err)
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Notify            bool                        `json:"notify"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notifyReceived = body.Notify
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
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

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, true, false)
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Notify            bool                        `json:"notify"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notifyReceived = body.Notify
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
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

	// Explicit --no-notify: notify should be false
	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, false, false)
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/projects/"+projectID+"/agents/my-agent/outbound-message" ||
			r.URL.Path == "/api/v1/groves/"+projectID+"/agents/my-agent/outbound-message"):
			var msg hubclient.OutboundMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&msg)
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
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "grove-test",
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

func TestSetRecipientFlagValidation(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		raw       bool
		broadcast bool
		all       bool
		in        string
		notify    bool
		wantErr   string
	}{
		{
			name:    "set with raw not allowed",
			args:    []string{"set[agent:a,agent:b]", "hello"},
			raw:     true,
			wantErr: "--raw cannot be used with group[] recipients",
		},
		{
			name:      "set with broadcast not allowed",
			args:      []string{"set[agent:a,agent:b]", "hello"},
			broadcast: true,
			wantErr:   "group[] recipients cannot be combined with --broadcast or --all",
		},
		{
			name:    "set with all not allowed",
			args:    []string{"set[agent:a,agent:b]", "hello"},
			all:     true,
			wantErr: "group[] recipients cannot be combined with --broadcast or --all",
		},
		{
			name:    "set with in not allowed",
			args:    []string{"set[agent:a,agent:b]", "hello"},
			in:      "30m",
			wantErr: "--in/--at cannot be used with group[] recipients",
		},
		{
			name:    "set with notify not allowed",
			args:    []string{"set[agent:a,agent:b]", "hello"},
			notify:  true,
			wantErr: "--notify cannot be used with group[] recipients",
		},
		{
			name:    "invalid set",
			args:    []string{"set[agent:a]", "hello"},
			wantErr: "invalid group recipient",
		},
		{
			name:    "empty set",
			args:    []string{"set[]", "hello"},
			wantErr: "invalid group recipient",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origRaw := msgRaw
			origBroadcast, origAll := msgBroadcast, msgAll
			origIn := msgIn
			origNotify := msgNotify
			defer func() {
				msgRaw = origRaw
				msgBroadcast = origBroadcast
				msgAll = origAll
				msgIn = origIn
				msgNotify = origNotify
			}()

			msgRaw = tc.raw
			msgBroadcast = tc.broadcast
			msgAll = tc.all
			msgIn = tc.in
			msgNotify = tc.notify

			err := messageCmd.RunE(messageCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestWakeFlagValidation(t *testing.T) {
	tests := []struct {
		name     string
		setup    func()
		teardown func()
		args     []string // cobra args; nil means use default ["agent1", "hello"]
		errMsg   string
	}{
		{
			name:     "wake with broadcast",
			setup:    func() { msgWake = true; msgBroadcast = true },
			teardown: func() { msgWake = false; msgBroadcast = false },
			args:     []string{"hello"},
			errMsg:   "--wake cannot be combined with --broadcast or --all",
		},
		{
			name:     "wake with all",
			setup:    func() { msgWake = true; msgAll = true },
			teardown: func() { msgWake = false; msgAll = false },
			args:     []string{"hello"},
			errMsg:   "--wake cannot be combined with --broadcast or --all",
		},
		{
			name:     "wake with in",
			setup:    func() { msgWake = true; msgIn = "5m" },
			teardown: func() { msgWake = false; msgIn = "" },
			errMsg:   "--wake cannot be combined with --in or --at",
		},
		{
			name:     "wake with at",
			setup:    func() { msgWake = true; msgAt = "2026-01-01T00:00:00Z" },
			teardown: func() { msgWake = false; msgAt = "" },
			errMsg:   "--wake cannot be combined with --in or --at",
		},
		{
			name:     "wake with raw",
			setup:    func() { msgWake = true; msgRaw = true },
			teardown: func() { msgWake = false; msgRaw = false },
			errMsg:   "--wake cannot be combined with --raw",
		},
		{
			name:     "wake with user recipient",
			setup:    func() { msgWake = true },
			teardown: func() { msgWake = false },
			args:     []string{"user:alice", "hello"},
			errMsg:   "--wake cannot be used with user recipients",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			defer tc.teardown()

			args := tc.args
			if args == nil {
				args = []string{"agent1", "hello"}
			}

			err := messageCmd.RunE(messageCmd, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}

func TestAttachFlagValidation(t *testing.T) {
	tests := []struct {
		name     string
		setup    func()
		teardown func()
		errMsg   string
	}{
		{
			name:     "attach with in",
			setup:    func() { msgAttach = []string{"notes.md"}; msgIn = "5m" },
			teardown: func() { msgAttach = nil; msgIn = "" },
			errMsg:   "--attach cannot be combined with --in or --at",
		},
		{
			name:     "attach with at",
			setup:    func() { msgAttach = []string{"notes.md"}; msgAt = "2026-01-01T00:00:00Z" },
			teardown: func() { msgAttach = nil; msgAt = "" },
			errMsg:   "--attach cannot be combined with --in or --at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			defer tc.teardown()

			err := messageCmd.RunE(messageCmd, []string{"agent1", "hello"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}

func TestAttachFlagValidation_NonExistentFile(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	msgAttach = []string{"/workspace/this-file-does-not-exist-xyz.go"}
	defer func() { msgAttach = nil }()

	err := messageCmd.RunE(messageCmd, []string{"agent1", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this-file-does-not-exist-xyz.go")
	assert.Contains(t, err.Error(), "attachment")
}

func TestAttachFlagValidation_OutsideAllowedRoots(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	msgAttach = []string{"/etc/passwd"}
	defer func() { msgAttach = nil }()

	err := messageCmd.RunE(messageCmd, []string{"agent1", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside allowed roots")
}

func TestAttachFlagValidation_Directory(t *testing.T) {
	if _, err := os.Stat("/workspace"); os.IsNotExist(err) {
		t.Skip("skipping: /workspace not available")
	}

	orig := saveMessageTestState()
	defer orig.restore()

	// Create a temporary directory under /workspace so it passes
	// resolveAttachmentPath's allowed-roots check.
	testDir := filepath.Join("/workspace", ".test-attach-dir-validation")
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	msgAttach = []string{testDir}
	defer func() { msgAttach = nil }()

	err = messageCmd.RunE(messageCmd, []string{"agent1", "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory, not a regular file")
}

func TestSendGroupMessageViaHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-group"
	agents := []hubclient.Agent{
		{Name: "agent-a", Status: "running"},
		{Name: "agent-b", Status: "running"},
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

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientAgent, Name: "agent-b"},
	}

	err = sendGroupMessageViaHub(hubCtx, recipients, "group hello", false)
	require.NoError(t, err)

	require.Len(t, *sent, 2)
	names := make([]string, len(*sent))
	for i, s := range *sent {
		names[i] = s.AgentName
		assert.Equal(t, "group hello", s.Message)
		require.NotNil(t, s.StructuredMsg)
		assert.NotEmpty(t, s.StructuredMsg.Metadata["group_id"])
		// Verify group-set type and recipients are set
		assert.Equal(t, messages.TypeGroupSet, s.StructuredMsg.Type, "group message type should be group-set")
		assert.NotEmpty(t, s.StructuredMsg.Recipients, "group message should have recipients populated")
		assert.True(t, messages.IsGroupRecipient(s.StructuredMsg.Recipients), "recipients should be a valid group[] string")
	}
	assert.ElementsMatch(t, []string{"agent-a", "agent-b"}, names)
}

func TestSendGroupMessageViaHub_UserRecipientType(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-group-user"
	t.Setenv("SCION_AGENT_NAME", "my-agent")

	var receivedMsg *hubclient.OutboundMessageRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			var msg hubclient.OutboundMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&msg)
			mu.Lock()
			receivedMsg = &msg
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
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

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientUser, Name: "alice"},
	}

	err = sendGroupMessageViaHub(hubCtx, recipients, "hello group", false)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, receivedMsg)
	assert.Equal(t, messages.TypeGroupSet, receivedMsg.Type, "user recipient in group should get type group-set")
	assert.NotEmpty(t, receivedMsg.Metadata["recipients"], "user recipient in group should have recipients in metadata")
	assert.NotEmpty(t, receivedMsg.Metadata["group_id"], "user recipient in group should have group_id in metadata")
}

func TestSendGroupMessageViaHub_RequiresHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	// group[] without Hub should fail at the RunE level, not get to sendGroupMessageViaHub
	origBroadcast, origAll := msgBroadcast, msgAll
	defer func() { msgBroadcast = origBroadcast; msgAll = origAll }()
	msgBroadcast = false
	msgAll = false

	err := messageCmd.RunE(messageCmd, []string{"set[agent:a,agent:b]", "hello"})
	// When Hub is not configured, this should fail with "group[] recipients require Hub mode".
	// When Hub is configured but test agents don't exist, delivery fails.
	// Either way, an error must be returned — never silent nil.
	require.Error(t, err)
}

func TestSendMessageViaHub_WakePassedThrough(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-wake"

	var wakeReceived bool
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Notify            bool                        `json:"notify"`
				Wake              bool                        `json:"wake"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			wakeReceived = body.Wake
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
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

	// Send with wake=true
	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false, false, true)
	require.NoError(t, err)

	mu.Lock()
	assert.True(t, wakeReceived, "wake should be true when passed through")
	mu.Unlock()
}

func TestBareEmailRecipientAutoPrefix(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "bare email is accepted without user: prefix",
			args: []string{"alice@example.com", "hello"},
		},
		{
			name: "bare email with subdomain is accepted",
			args: []string{"bob@corp.example.com", "check this out"},
		},
		{
			name: "user-prefixed email is still accepted",
			args: []string{"user:alice@example.com", "hello"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset flags to defaults
			origRaw := msgRaw
			origIn := msgIn
			origBroadcast, origAll := msgBroadcast, msgAll
			origNotify := msgNotify
			origWake := msgWake
			defer func() {
				msgRaw = origRaw
				msgIn = origIn
				msgBroadcast = origBroadcast
				msgAll = origAll
				msgNotify = origNotify
				msgWake = origWake
			}()
			msgRaw = false
			msgIn = ""
			msgBroadcast = false
			msgAll = false
			msgNotify = false
			msgWake = false

			err := messageCmd.RunE(messageCmd, tc.args)

			// No error at the recipient parsing stage.
			// The command may still fail (Hub not configured, etc.)
			// but NOT with an email-specific error.
			if err != nil {
				assert.NotContains(t, err.Error(), "looks like an email address")
				assert.NotContains(t, err.Error(), "missing the \"user:\" prefix")
			}
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

func TestResolveAttachmentPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "relative path resolves to /workspace",
			path: "src/main.go",
			want: "/workspace/src/main.go",
		},
		{
			name: "bare filename resolves to /workspace",
			path: "file.txt",
			want: "/workspace/file.txt",
		},
		{
			name: "absolute path under /workspace is accepted",
			path: "/workspace/pkg/api/types.go",
			want: "/workspace/pkg/api/types.go",
		},
		{
			name: "absolute path under /scion-volumes is accepted",
			path: "/scion-volumes/scratchpad/notes.md",
			want: "/scion-volumes/scratchpad/notes.md",
		},
		{
			name: "absolute path outside allowed roots is filtered",
			path: "/etc/shadow",
			want: "",
		},
		{
			name: "absolute path outside allowed roots - tmp",
			path: "/tmp/secret.txt",
			want: "",
		},
		{
			name: "path with dot-dot traversal outside workspace is filtered",
			path: "/workspace/../etc/passwd",
			want: "",
		},
		{
			name: "relative path with dot-dot staying inside workspace",
			path: "pkg/../cmd/message.go",
			want: "/workspace/cmd/message.go",
		},
		{
			name: "path with workspace prefix but different directory is filtered",
			path: "/workspace-evil/secret.txt",
			want: "",
		},
		{
			name: "path with scion-volumes prefix but different directory is filtered",
			path: "/scion-volumes-other/data.txt",
			want: "",
		},
		{
			name: "path with workspace prefix no separator is filtered",
			path: "/workspacefoo",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAttachmentPath(tc.path)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCopyFile(t *testing.T) {
	// Create a temp source file
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "source.txt")
	content := []byte("hello world\nthis is a test file\n")
	err := os.WriteFile(srcPath, content, 0644)
	require.NoError(t, err)

	// Copy it
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "dest.txt")
	err = copyFile(srcPath, dstPath)
	require.NoError(t, err)

	// Verify content matches
	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// Verify permissions are preserved
	srcInfo, err := os.Stat(srcPath)
	require.NoError(t, err)
	dstInfo, err := os.Stat(dstPath)
	require.NoError(t, err)
	assert.Equal(t, srcInfo.Mode(), dstInfo.Mode())
}

func TestCopyFile_PreservesExecutablePermission(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "script.sh")
	err := os.WriteFile(srcPath, []byte("#!/bin/sh\necho hi\n"), 0755)
	require.NoError(t, err)

	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "script.sh")
	err = copyFile(srcPath, dstPath)
	require.NoError(t, err)

	dstInfo, err := os.Stat(dstPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), dstInfo.Mode().Perm())
}

func TestUniqueDest(t *testing.T) {
	dir := t.TempDir()

	// First call: no conflict
	dest, err := uniqueDest(dir, "file.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "file.go"), dest)

	// Create the file so the next call must pick a new name
	err = os.WriteFile(dest, []byte("x"), 0644)
	require.NoError(t, err)

	// Second call: conflict, should get _1
	dest2, err := uniqueDest(dir, "file.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "file_1.go"), dest2)

	// Create that file too
	err = os.WriteFile(dest2, []byte("y"), 0644)
	require.NoError(t, err)

	// Third call: should get _2
	dest3, err := uniqueDest(dir, "file.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "file_2.go"), dest3)
}

func TestUniqueDest_NoExtension(t *testing.T) {
	dir := t.TempDir()

	// File without extension
	dest, err := uniqueDest(dir, "Makefile")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "Makefile"), dest)

	err = os.WriteFile(dest, []byte("x"), 0644)
	require.NoError(t, err)

	dest2, err := uniqueDest(dir, "Makefile")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "Makefile_1"), dest2)
}

func TestStageAttachments_HappyPath(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping stageAttachments integration test: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "test-agent")

	// Create test files under /workspace
	testDir := filepath.Join("/workspace", ".test-attachments-happy")
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	testFile1 := filepath.Join(testDir, "test1.txt")
	testFile2 := filepath.Join(testDir, "test2.txt")
	err = os.WriteFile(testFile1, []byte("content1"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(testFile2, []byte("content2"), 0644)
	require.NoError(t, err)

	staged, err := stageAttachments([]string{testFile1, testFile2})
	require.NoError(t, err)
	require.Len(t, staged, 2)

	// Verify staged files exist under the correct agent directory
	for _, sp := range staged {
		assert.Contains(t, sp, "/scion-volumes/scratchpad/.attachments/test-agent/")
		_, err := os.Stat(sp)
		require.NoError(t, err)
	}

	// Verify content was copied correctly
	c1, err := os.ReadFile(staged[0])
	require.NoError(t, err)
	assert.Equal(t, "content1", string(c1))

	c2, err := os.ReadFile(staged[1])
	require.NoError(t, err)
	assert.Equal(t, "content2", string(c2))

	// Verify original filenames are preserved
	assert.Equal(t, "test1.txt", filepath.Base(staged[0]))
	assert.Equal(t, "test2.txt", filepath.Base(staged[1]))

	// Clean up staged files
	_ = os.RemoveAll(filepath.Dir(staged[0]))
}

func TestStageAttachments_MissingScratchpad(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); err == nil {
		t.Skip("skipping missing-scratchpad test: /scion-volumes/scratchpad is mounted")
	}

	_, err := stageAttachments([]string{"/workspace/some/file.go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scratchpad volume not available")
	assert.Contains(t, err.Error(), "scion shared-dir create scratchpad")
}

func TestStageAttachments_DuplicateBasenames(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "dup-test-agent")

	// Create two files with the same basename in different directories
	testDir := filepath.Join("/workspace", ".test-dup-basenames")
	dir1 := filepath.Join(testDir, "v1")
	dir2 := filepath.Join(testDir, "v2")
	err := os.MkdirAll(dir1, 0755)
	require.NoError(t, err)
	err = os.MkdirAll(dir2, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	err = os.WriteFile(filepath.Join(dir1, "types.go"), []byte("package v1"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir2, "types.go"), []byte("package v2"), 0644)
	require.NoError(t, err)

	staged, err := stageAttachments([]string{
		filepath.Join(dir1, "types.go"),
		filepath.Join(dir2, "types.go"),
	})
	require.NoError(t, err)
	require.Len(t, staged, 2)

	// First should be types.go, second should be types_1.go
	assert.Equal(t, "types.go", filepath.Base(staged[0]))
	assert.Equal(t, "types_1.go", filepath.Base(staged[1]))

	// Verify content is correct
	c1, err := os.ReadFile(staged[0])
	require.NoError(t, err)
	assert.Equal(t, "package v1", string(c1))

	c2, err := os.ReadFile(staged[1])
	require.NoError(t, err)
	assert.Equal(t, "package v2", string(c2))

	// Cleanup
	_ = os.RemoveAll(filepath.Dir(staged[0]))
}

func TestStageAttachments_NonExistentFile(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "noexist-test")

	_, err := stageAttachments([]string{"/workspace/this-file-does-not-exist-xyz.go"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this-file-does-not-exist-xyz.go")
}

func TestStageAttachments_NonRegularFile(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "nonreg-test")

	// Create a directory (not a regular file) and try to attach it
	testDir := filepath.Join("/workspace", ".test-nonreg-attach")
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	_, err = stageAttachments([]string{testDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}

func TestStageAttachments_DefaultAgentSlug(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "")

	testDir := filepath.Join("/workspace", ".test-slug-default")
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	testFile := filepath.Join(testDir, "file.txt")
	err = os.WriteFile(testFile, []byte("test"), 0644)
	require.NoError(t, err)

	staged, err := stageAttachments([]string{testFile})
	require.NoError(t, err)
	require.Len(t, staged, 1)

	// Should use _user as the agent slug
	assert.Contains(t, staged[0], "/_user/")

	// Cleanup
	_ = os.RemoveAll(filepath.Dir(staged[0]))
}

func TestStageAttachments_FilteredPathsSkipped(t *testing.T) {
	if _, err := os.Stat("/scion-volumes/scratchpad"); os.IsNotExist(err) {
		t.Skip("skipping: /scion-volumes/scratchpad not available")
	}

	t.Setenv("SCION_AGENT_NAME", "filter-test")

	// Create a valid file
	testDir := filepath.Join("/workspace", ".test-filter-attach")
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(testDir) }()

	validFile := filepath.Join(testDir, "valid.go")
	err = os.WriteFile(validFile, []byte("package valid"), 0644)
	require.NoError(t, err)

	// Pass one valid path and one that will be filtered
	staged, err := stageAttachments([]string{validFile, "/etc/passwd"})
	require.NoError(t, err)
	// Only the valid file should be staged
	require.Len(t, staged, 1)
	assert.Equal(t, "valid.go", filepath.Base(staged[0]))

	// Cleanup
	_ = os.RemoveAll(filepath.Dir(staged[0]))
}

// --- @mention and --cc tests ---

func TestExtractMentions_Basic(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "no mentions",
			text: "hello world",
			want: nil,
		},
		{
			name: "single mention",
			text: "hey @alice check this out",
			want: []string{"alice"},
		},
		{
			name: "multiple mentions",
			text: "hey @alice and @bob check this",
			want: []string{"alice", "bob"},
		},
		{
			name: "mention at start",
			text: "@alice please review",
			want: []string{"alice"},
		},
		{
			name: "mention at end",
			text: "please review @alice",
			want: []string{"alice"},
		},
		{
			name: "duplicate mentions",
			text: "@alice hey @alice check this",
			want: []string{"alice"},
		},
		{
			name: "case insensitive dedup",
			text: "@Alice hey @alice check this",
			want: []string{"Alice"},
		},
		{
			name: "mention with trailing punctuation",
			text: "hey @alice, @bob! @charlie.",
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "mention with hyphen",
			text: "hey @my-agent check this",
			want: []string{"my-agent"},
		},
		{
			name: "mention with underscore",
			text: "hey @my_agent check this",
			want: []string{"my_agent"},
		},
		{
			name: "double at sign",
			text: "hey @@ what",
			want: nil,
		},
		{
			name: "bare at sign",
			text: "hey @ what",
			want: nil,
		},
		{
			name: "double at with name",
			text: "hey @@bob check this",
			want: []string{"@bob"},
		},
		{
			name: "email address not treated as mention",
			text: "send to user@example.com",
			want: nil,
		},
		{
			name: "mention followed by colon",
			text: "hey @alice: check this",
			want: []string{"alice"},
		},
		{
			name: "mention in parentheses",
			text: "(cc @bob)",
			want: []string{"bob"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMentions(tc.text)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseCCFlag(t *testing.T) {
	tests := []struct {
		name string
		cc   string
		want []string
	}{
		{
			name: "empty",
			cc:   "",
			want: nil,
		},
		{
			name: "single name",
			cc:   "alice",
			want: []string{"alice"},
		},
		{
			name: "multiple names",
			cc:   "alice,bob,charlie",
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "whitespace trimmed",
			cc:   " alice , bob , charlie ",
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "empty entries skipped",
			cc:   "alice,,bob",
			want: []string{"alice", "bob"},
		},
		{
			name: "duplicates removed",
			cc:   "alice,bob,alice",
			want: []string{"alice", "bob"},
		},
		{
			name: "case insensitive dedup",
			cc:   "Alice,alice",
			want: []string{"Alice"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCCFlag(tc.cc)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSendMessageViaHub_MentionFanOut(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-mention"
	agents := []hubclient.Agent{
		{Name: "primary-agent", Status: "running"},
		{Name: "mentioned-agent", Status: "running"},
		{Name: "other-agent", Status: "running"},
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

	// Reset CC flag
	origCC := msgCC
	msgCC = ""
	defer func() { msgCC = origCC }()

	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @mentioned-agent check this", false, false, false, false, false)
	require.NoError(t, err)

	// Should have 2 messages: primary + mention
	require.Len(t, *sent, 2)

	// First message is the primary instruction
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, messages.TypeInstruction, (*sent)[0].StructuredMsg.Type)

	// Second message is the mention notification
	assert.Equal(t, "mentioned-agent", (*sent)[1].AgentName)
	require.NotNil(t, (*sent)[1].StructuredMsg)
	assert.Equal(t, messages.TypeMention, (*sent)[1].StructuredMsg.Type)
	assert.Equal(t, "agent:primary-agent", (*sent)[1].StructuredMsg.Metadata["mention_source"])
	assert.Equal(t, "body", (*sent)[1].StructuredMsg.Metadata["mention_position"])
}

func TestSendMessageViaHub_MentionDedup(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-mention-dedup"
	agents := []hubclient.Agent{
		{Name: "my-agent", Status: "running"},
		{Name: "other-agent", Status: "running"},
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

	origCC := msgCC
	msgCC = ""
	defer func() { msgCC = origCC }()

	// Primary recipient is also @mentioned in body — should be deduplicated
	err = sendMessageViaHub(hubCtx, "my-agent", "hey @my-agent check @other-agent", false, false, false, false, false)
	require.NoError(t, err)

	// Should have 2 messages: primary + mention for other-agent only (my-agent deduped)
	require.Len(t, *sent, 2)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.Equal(t, "other-agent", (*sent)[1].AgentName)
	assert.Equal(t, messages.TypeMention, (*sent)[1].StructuredMsg.Type)
}

func TestSendMessageViaHub_UnknownMentionWarns(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-mention-unknown"
	agents := []hubclient.Agent{
		{Name: "my-agent", Status: "running"},
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

	origCC := msgCC
	msgCC = ""
	defer func() { msgCC = origCC }()

	// @nonexistent doesn't match any agent — should warn but not fail
	err = sendMessageViaHub(hubCtx, "my-agent", "hey @nonexistent check this", false, false, false, false, false)
	require.NoError(t, err)

	// Only the primary message should be sent
	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
}

func TestSendMessageViaHub_CCFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-cc"
	agents := []hubclient.Agent{
		{Name: "primary-agent", Status: "running"},
		{Name: "cc-agent-1", Status: "running"},
		{Name: "cc-agent-2", Status: "running"},
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

	origCC := msgCC
	msgCC = "cc-agent-1,cc-agent-2"
	defer func() { msgCC = origCC }()

	err = sendMessageViaHub(hubCtx, "primary-agent", "check this out", false, false, false, false, false)
	require.NoError(t, err)

	// Should have 3 messages: primary + 2 CC mentions
	require.Len(t, *sent, 3)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	assert.Equal(t, messages.TypeInstruction, (*sent)[0].StructuredMsg.Type)

	// CC agents get TypeMention messages (order may vary due to goroutines)
	ccNames := []string{(*sent)[1].AgentName, (*sent)[2].AgentName}
	assert.ElementsMatch(t, []string{"cc-agent-1", "cc-agent-2"}, ccNames)
	for _, s := range (*sent)[1:] {
		assert.Equal(t, messages.TypeMention, s.StructuredMsg.Type)
		assert.Equal(t, "agent:primary-agent", s.StructuredMsg.Metadata["mention_source"])
	}
}

func TestSendMessageViaHub_CCAndMentionCombined(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-cc-mention"
	agents := []hubclient.Agent{
		{Name: "primary-agent", Status: "running"},
		{Name: "mention-agent", Status: "running"},
		{Name: "cc-agent", Status: "running"},
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

	origCC := msgCC
	msgCC = "cc-agent"
	defer func() { msgCC = origCC }()

	// Both @mention in body and --cc flag
	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @mention-agent check this", false, false, false, false, false)
	require.NoError(t, err)

	// Should have 3 messages: primary + @mention + --cc
	require.Len(t, *sent, 3)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)

	mentionNames := []string{(*sent)[1].AgentName, (*sent)[2].AgentName}
	assert.ElementsMatch(t, []string{"mention-agent", "cc-agent"}, mentionNames)
}

func TestSendMessageViaHub_CCDedupWithMention(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-cc-dedup"
	agents := []hubclient.Agent{
		{Name: "primary-agent", Status: "running"},
		{Name: "shared-agent", Status: "running"},
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

	origCC := msgCC
	msgCC = "shared-agent"
	defer func() { msgCC = origCC }()

	// Same agent in both @mention and --cc — should only get one mention
	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @shared-agent check this", false, false, false, false, false)
	require.NoError(t, err)

	// Should have 2 messages: primary + 1 mention (deduped)
	require.Len(t, *sent, 2)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	assert.Equal(t, "shared-agent", (*sent)[1].AgentName)
}

func TestSendMessageViaHub_NoMentionsInBody(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-no-mention"
	agents := []hubclient.Agent{
		{Name: "my-agent", Status: "running"},
		{Name: "other-agent", Status: "running"},
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

	origCC := msgCC
	msgCC = ""
	defer func() { msgCC = origCC }()

	// No mentions in body, no --cc — only primary should be sent
	err = sendMessageViaHub(hubCtx, "my-agent", "hello world", false, false, false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
}

func TestSendGroupMessageViaHub_MentionFanOut(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "grove-msg-group-mention"
	agents := []hubclient.Agent{
		{Name: "agent-a", Status: "running"},
		{Name: "agent-b", Status: "running"},
		{Name: "agent-c", Status: "running"},
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

	// Reset CC flag
	origCC := msgCC
	msgCC = ""
	defer func() { msgCC = origCC }()

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientAgent, Name: "agent-b"},
	}

	// Send to group[a,b] with @agent-c in body
	err = sendGroupMessageViaHub(hubCtx, recipients, "hey @agent-c check this", false)
	require.NoError(t, err)

	// Should have 3 messages: agent-a, agent-b (group), agent-c (mention)
	require.Len(t, *sent, 3)

	// Categorize messages by type
	var groupMsgs []sentMessage
	var mentionMsgs []sentMessage
	for _, s := range *sent {
		require.NotNil(t, s.StructuredMsg)
		if s.StructuredMsg.Type == messages.TypeMention {
			mentionMsgs = append(mentionMsgs, s)
		} else {
			groupMsgs = append(groupMsgs, s)
		}
	}

	// Group recipients get instruction messages
	require.Len(t, groupMsgs, 2)
	groupNames := []string{groupMsgs[0].AgentName, groupMsgs[1].AgentName}
	assert.ElementsMatch(t, []string{"agent-a", "agent-b"}, groupNames)

	// agent-c gets a mention notification, not an instruction
	require.Len(t, mentionMsgs, 1)
	assert.Equal(t, "agent-c", mentionMsgs[0].AgentName)
	assert.Equal(t, messages.TypeMention, mentionMsgs[0].StructuredMsg.Type)
}

func TestCCFlagValidation(t *testing.T) {
	tests := []struct {
		name      string
		cc        string
		broadcast bool
		all       bool
		raw       bool
		userRecip bool
		in        string
		at        string
		wantErr   string
	}{
		{
			name:      "cc with broadcast",
			cc:        "agent-a",
			broadcast: true,
			wantErr:   "--cc cannot be combined with --broadcast or --all",
		},
		{
			name:    "cc with all",
			cc:      "agent-a",
			all:     true,
			wantErr: "--cc cannot be combined with --broadcast or --all",
		},
		{
			name:    "cc with raw",
			cc:      "agent-a",
			raw:     true,
			wantErr: "--cc cannot be combined with --raw",
		},
		{
			name:      "cc with user recipient",
			cc:        "agent-a",
			userRecip: true,
			wantErr:   "--cc cannot be used with user recipients",
		},
		{
			name:    "cc with --in scheduling",
			cc:      "agent-a",
			in:      "5m",
			wantErr: "--cc cannot be combined with --in or --at",
		},
		{
			name:    "cc with --at scheduling",
			cc:      "agent-a",
			at:      "2026-01-01 12:00",
			wantErr: "--cc cannot be combined with --in or --at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origCC := msgCC
			origBroadcast := msgBroadcast
			origAll := msgAll
			origRaw := msgRaw
			origIn := msgIn
			origAt := msgAt
			defer func() {
				msgCC = origCC
				msgBroadcast = origBroadcast
				msgAll = origAll
				msgRaw = origRaw
				msgIn = origIn
				msgAt = origAt
			}()

			msgCC = tc.cc
			msgBroadcast = tc.broadcast
			msgAll = tc.all
			msgRaw = tc.raw
			msgIn = tc.in
			msgAt = tc.at

			var args []string
			if tc.broadcast || tc.all {
				args = []string{"hello"}
			} else if tc.userRecip {
				args = []string{"user:alice", "hello"}
			} else {
				args = []string{"my-agent", "hello"}
			}

			err := messageCmd.RunE(messageCmd, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
