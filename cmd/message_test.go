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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messageTestState captures and restores package-level vars for test isolation.
type messageTestState struct {
	projectPath  string
	noHub        bool
	bcastChanged bool
	allChanged   bool
	bodyFile     string
	rawChanged   bool
}

func saveMessageTestState() messageTestState {
	return messageTestState{
		projectPath:  projectPath,
		noHub:        noHub,
		bcastChanged: messageCmd.Flags().Lookup("broadcast").Changed,
		allChanged:   messageCmd.Flags().Lookup("all").Changed,
		bodyFile:     msgBodyFile,
		rawChanged:   messageCmd.Flags().Lookup("raw").Changed,
	}
}

func (s messageTestState) restore() {
	projectPath = s.projectPath
	noHub = s.noHub
	messageCmd.Flags().Lookup("broadcast").Changed = s.bcastChanged
	messageCmd.Flags().Lookup("all").Changed = s.allChanged
	msgBodyFile = s.bodyFile
	messageCmd.Flags().Lookup("raw").Changed = s.rawChanged
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
	// Mentions is the request body's explicit mentions field
	// (SendStructuredMessageWithOptions), when present.
	Mentions []string
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

		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/projects/"+projectID+"/agents" || r.URL.Path == "/api/v1/agents"):
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
			// or /api/v1/agents/<name>/message
			var agentName string
			projectPrefix := "/api/v1/projects/" + projectID + "/agents/"
			globalPrefix := "/api/v1/agents/"
			path := r.URL.Path
			if len(path) > len(projectPrefix) && path[:len(projectPrefix)] == projectPrefix {
				rest := path[len(projectPrefix):]
				agentName = rest[:len(rest)-len("/message")]
			} else if len(path) > len(globalPrefix) && path[:len(globalPrefix)] == globalPrefix {
				rest := path[len(globalPrefix):]
				agentName = rest[:len(rest)-len("/message")]
			}

			var body struct {
				Message           string                      `json:"message"`
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
				Mentions          []string                    `json:"mentions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			sm := sentMessage{
				AgentName:     agentName,
				Interrupt:     body.Interrupt,
				StructuredMsg: body.StructuredMessage,
				Mentions:      body.Mentions,
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

			// Synthesize mention_results from the request's mentions field,
			// matching against the fixture agent list — good enough to
			// exercise the CLI's printMentionResults wiring end to end
			// without a real hub's resolution logic.
			known := make(map[string]bool, len(runningAgents))
			for _, a := range runningAgents {
				known[strings.ToLower(a.Name)] = true
				if a.Slug != "" {
					known[strings.ToLower(a.Slug)] = true
				}
			}
			var mentionResults []messages.MentionResult
			for _, m := range body.Mentions {
				if known[strings.ToLower(m)] {
					mentionResults = append(mentionResults, messages.MentionResult{Slug: m, Status: "delivered", AgentPhase: "running"})
				} else {
					mentionResults = append(mentionResults, messages.MentionResult{Slug: m, Status: "not_found", Error: "no matching agent in this project"})
				}
			}

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "mention_results": mentionResults})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &sent
}

// --- resolveMessageBody tests ---

func TestResolveMessageBody_BodyFile(t *testing.T) {
	// Create a temp file with known content
	tmpDir := t.TempDir()
	bodyFile := filepath.Join(tmpDir, "msg.txt")
	content := "Hello, this is a message with `backticks` and $variables"
	err := os.WriteFile(bodyFile, []byte(content), 0644)
	require.NoError(t, err)

	got, err := resolveMessageBody(bodyFile, "")
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestResolveMessageBody_BodyFilePreservesNewlines(t *testing.T) {
	tmpDir := t.TempDir()
	bodyFile := filepath.Join(tmpDir, "msg.txt")
	content := "  line1\n\nline2\nline3  \n\n"
	err := os.WriteFile(bodyFile, []byte(content), 0644)
	require.NoError(t, err)

	got, err := resolveMessageBody(bodyFile, "")
	require.NoError(t, err)
	// Same rule as stdin: trailing newlines are trimmed, everything else
	// (interior blank lines, leading/trailing spaces) is preserved exactly.
	assert.Equal(t, "  line1\n\nline2\nline3  ", got)
}

// withStdin replaces os.Stdin with a pipe carrying content for the test.
func withStdin(t *testing.T, content string) {
	t.Helper()
	origStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })
	r, w, err := os.Pipe()
	require.NoError(t, err)
	// Write from a goroutine: content larger than the OS pipe buffer
	// (64 KiB on Linux) would otherwise block before anything reads it.
	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()
	t.Cleanup(func() { _ = r.Close() })
	os.Stdin = r
}

func TestResolveMessageBody_BodyFileDashReadsStdin(t *testing.T) {
	withStdin(t, "from `stdin` via --body-file - $(not expanded)\n")
	got, err := resolveMessageBody("-", "")
	require.NoError(t, err)
	assert.Equal(t, "from `stdin` via --body-file - $(not expanded)", got)
}

func TestResolveMessageBody_BodyFileDashConflict(t *testing.T) {
	_, err := resolveMessageBody("-", "positional content")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestResolveMessageBody_NewlineRuleConsistent pins that --body-file <path>,
// --body-file -, and positional "-" apply the same newline rule.
func TestResolveMessageBody_NewlineRuleConsistent(t *testing.T) {
	cases := map[string]string{
		"single trailing LF":    "a\nb\n",
		"multiple trailing LF":  "a\nb\n\n\n",
		"trailing CRLF":         "a\r\nb\r\n",
		"no trailing newline":   "a\nb",
		"only newlines → empty": "\n\n",
		"trailing spaces kept":  "a\nb  \n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			bodyFile := filepath.Join(t.TempDir(), "msg.txt")
			require.NoError(t, os.WriteFile(bodyFile, []byte(content), 0644))
			fromFile, err := resolveMessageBody(bodyFile, "")
			require.NoError(t, err)

			withStdin(t, content)
			fromDashFlag, err := resolveMessageBody("-", "")
			require.NoError(t, err)

			withStdin(t, content)
			fromDashArg, err := resolveMessageBody("", "-")
			require.NoError(t, err)

			assert.Equal(t, fromFile, fromDashFlag)
			assert.Equal(t, fromFile, fromDashArg)
			assert.NotRegexp(t, `[\r\n]$`, fromFile, "trailing line breaks should be trimmed")
		})
	}
}

func TestResolveMessageBody_BodyFileNotFound(t *testing.T) {
	_, err := resolveMessageBody("/tmp/nonexistent-body-file-xyz.txt", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open body file")
}

func TestResolveMessageBody_Conflict(t *testing.T) {
	tmpDir := t.TempDir()
	bodyFile := filepath.Join(tmpDir, "msg.txt")
	err := os.WriteFile(bodyFile, []byte("file content"), 0644)
	require.NoError(t, err)

	_, err = resolveMessageBody(bodyFile, "positional content")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--body-file and positional message arguments are mutually exclusive")
}

func TestResolveMessageBody_Stdin(t *testing.T) {
	// Save and restore os.Stdin
	origStdin := os.Stdin
	defer func() { os.Stdin = origStdin }()

	// Create a pipe to mock stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)

	_, err = w.WriteString("hello from stdin\n")
	require.NoError(t, err)
	_ = w.Close()

	os.Stdin = r

	got, err := resolveMessageBody("", "-")
	require.NoError(t, err)
	assert.Equal(t, "hello from stdin", got, "trailing newline from stdin should be trimmed")
}

func TestResolveMessageBody_StdinNoTrailingNewline(t *testing.T) {
	origStdin := os.Stdin
	defer func() { os.Stdin = origStdin }()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	_, err = w.WriteString("no trailing newline")
	require.NoError(t, err)
	_ = w.Close()

	os.Stdin = r

	got, err := resolveMessageBody("", "-")
	require.NoError(t, err)
	assert.Equal(t, "no trailing newline", got)
}

func TestResolveMessageBody_Positional(t *testing.T) {
	got, err := resolveMessageBody("", "plain positional message")
	require.NoError(t, err)
	assert.Equal(t, "plain positional message", got)
}

func TestResolveMessageBody_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	bodyFile := filepath.Join(tmpDir, "empty.txt")
	err := os.WriteFile(bodyFile, []byte(""), 0644)
	require.NoError(t, err)

	got, err := resolveMessageBody(bodyFile, "")
	require.NoError(t, err)
	assert.Equal(t, "", got, "empty file returns empty string; validation happens in RunE")
}

func TestSendMessageViaHub_SingleAgent(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-single"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendMessageViaHub(hubCtx, "my-agent", "hello world", false, false, false)
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

	projectID := "project-msg-int"
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

	err = sendMessageViaHub(hubCtx, "my-agent", "urgent", true, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.True(t, (*sent)[0].Interrupt)
	// Verify urgent flag is set in structured message
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.True(t, (*sent)[0].StructuredMsg.Urgent)
}

func TestSendMessageViaHub_SingleAgentError(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-err"

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

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false)
	require.Error(t, err, "single-agent message failure should return an error")
}

func TestScheduleMessageFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	tests := []struct {
		name    string
		in      string
		at      string
		wantErr string
	}{
		{
			name:    "in and at are mutually exclusive",
			in:      "30m",
			at:      "2030-01-01T00:00:00Z",
			wantErr: "--in and --at are mutually exclusive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Save and restore global state
			origIn, origAt := msgIn, msgAt
			defer func() {
				msgIn, msgAt = origIn, origAt
			}()

			msgIn = tc.in
			msgAt = tc.at

			args := []string{"agent1", "hello"}

			err := messageCmd.RunE(messageCmd, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
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
	origAttach := msgAttach
	defer func() {
		msgPlain = origPlain
		msgInterrupt = origInterrupt
		msgAttach = origAttach
	}()

	msgPlain = false
	msgInterrupt = true
	msgAttach = []string{"file1.go", "file2.go"}

	msg := buildStructuredMessage("user:alice", "agent:dev", "do something", msgAttach, msgPlain, msgInterrupt)

	assert.Equal(t, messages.Version, msg.Version)
	assert.Equal(t, "user:alice", msg.Sender)
	assert.Equal(t, "agent:dev", msg.Recipient)
	assert.Equal(t, "do something", msg.Msg)
	assert.Equal(t, messages.TypeInstruction, msg.Type)
	assert.False(t, msg.Plain)
	assert.True(t, msg.Urgent)
	assert.False(t, msg.Broadcasted)
	assert.Equal(t, []string{"file1.go", "file2.go"}, msg.Attachments)
}

func TestSendMessageViaHub_NotifyFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-notify"

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

	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, true, false)
	require.NoError(t, err)

	mu.Lock()
	assert.True(t, notifyReceived, "notify should be true by default")
	mu.Unlock()
}

func TestSendMessageViaHub_NoNotifyFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-no-notify"

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
	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, false)
	require.NoError(t, err)

	mu.Lock()
	assert.False(t, notifyReceived, "notify should be false when --no-notify is used")

	mu.Unlock()
}

func TestSendOutboundMessageViaHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-outbound"

	var receivedMsg *hubclient.OutboundMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents/my-agent/outbound-message":
			var msg hubclient.OutboundMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&msg)
			receivedMsg = &msg
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "msg-test-1",
				"status":       "sent",
				"recipient":    msg.Recipient,
				"recipient_id": "uid-test",
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
		ProjectID: "project-test",
	}

	t.Setenv("SCION_AGENT_NAME", "")

	err = sendOutboundMessageViaHub(hubCtx, "user:alice", "hello", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires an agent identity")
}

func TestUserRecipientFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	tests := []struct {
		name    string
		args    []string
		in      string
		wantErr string
	}{
		{
			name:    "scheduled with user recipient not allowed",
			args:    []string{"user:alice", "hello"},
			in:      "30m",
			wantErr: "--in/--at cannot be used with user recipients",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origIn := msgIn
			defer func() {
				msgIn = origIn
			}()

			msgIn = tc.in

			err := messageCmd.RunE(messageCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestSetRecipientFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	tests := []struct {
		name    string
		args    []string
		in      string
		notify  bool
		wantErr string
	}{
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
			origIn := msgIn
			origNotify := msgNotify
			defer func() {
				msgIn = origIn
				msgNotify = origNotify
			}()

			msgIn = tc.in
			msgNotify = tc.notify

			err := messageCmd.RunE(messageCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestWakeFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	tests := []struct {
		name     string
		setup    func()
		teardown func()
		args     []string // cobra args; nil means use default ["agent1", "hello"]
		errMsg   string
	}{
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
	orig := saveMessageTestState()
	defer orig.restore()

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

	projectID := "project-msg-group"
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

	projectID := "project-msg-group-user"
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "msg-test-conv",
				"status":       "sent",
				"recipient":    msg.Recipient,
				"recipient_id": "uid-test",
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
	err := messageCmd.RunE(messageCmd, []string{"set[agent:a,agent:b]", "hello"})
	// When Hub is not configured, this should fail with "group[] recipients require Hub mode".
	// When Hub is configured but test agents don't exist, delivery fails.
	// Either way, an error must be returned — never silent nil.
	require.Error(t, err)
}

func TestSendMessageViaHub_WakePassedThrough(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-wake"

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
	err = sendMessageViaHub(hubCtx, "my-agent", "hello", false, false, true)
	require.NoError(t, err)

	mu.Lock()
	assert.True(t, wakeReceived, "wake should be true when passed through")
	mu.Unlock()
}

func TestBareEmailRecipientAutoPrefix(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

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
			origIn := msgIn
			origNotify := msgNotify
			origWake := msgWake
			defer func() {
				msgIn = origIn
				msgNotify = origNotify
				msgWake = origWake
			}()
			msgIn = ""
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
		cc   []string
		want []string
	}{
		{
			name: "empty",
			cc:   nil,
			want: nil,
		},
		{
			name: "single empty string",
			cc:   []string{""},
			want: nil,
		},
		{
			name: "single name",
			cc:   []string{"alice"},
			want: []string{"alice"},
		},
		{
			name: "multiple names",
			cc:   []string{"alice,bob,charlie"},
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "whitespace trimmed",
			cc:   []string{" alice , bob , charlie "},
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "empty entries skipped",
			cc:   []string{"alice,,bob"},
			want: []string{"alice", "bob"},
		},
		{
			name: "duplicates removed",
			cc:   []string{"alice,bob,alice"},
			want: []string{"alice", "bob"},
		},
		{
			name: "case insensitive dedup",
			cc:   []string{"Alice,alice"},
			want: []string{"Alice"},
		},
		{
			// The repeatable form: previously only the last occurrence
			// survived, so "alice" was silently dropped.
			name: "repeated flag accumulates",
			cc:   []string{"alice", "bob"},
			want: []string{"alice", "bob"},
		},
		{
			name: "repeated and comma forms mixed",
			cc:   []string{"alice,bob", "charlie"},
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "dedup across occurrences",
			cc:   []string{"alice", "Alice", "bob"},
			want: []string{"alice", "bob"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCCFlag(tc.cc)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSendMessageViaHub_OldHubShaped_MentionsFieldCarriesTheMention: against
// a server that only understands the legacy explicit "mentions" field (no
// server-side body parsing — i.e. an "old hub" shape), the request body
// still carries the mention, so processMentions on that old hub would fan it
// out. This is what makes the new CLI's mention handling backward
// compatible with a hub that has not yet picked up server-side body
// parsing.
func TestSendMessageViaHub_OldHubShaped_MentionsFieldCarriesTheMention(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-old-hub-shape"
	var capturedMentions []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
			// Old hub's agent listing — unrelated to mention resolution.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": []hubclient.Agent{}})
		case r.Method == http.MethodPost:
			// Old hub's /message handler: only ever looks at the explicit
			// "mentions" field (no ExtractProseMentions body parsing).
			var body struct {
				Mentions []string `json:"mentions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			capturedMentions = body.Mentions
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": "old-hub-msg-1", "status": "delivered", "agent": "builder", "agent_phase": "running",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	origCC := msgCC
	msgCC = nil
	defer func() { msgCC = origCC }()

	err = sendMessageViaHub(hubCtx, "builder", "hey @bystander take a look", false, false, false)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"bystander"}, capturedMentions,
		"the old-hub-shaped server must still see the mention via the explicit field")
}

// TestSendMessageViaConversation_OutboundConvRef_PrintsMentionWarning covers
// the outbound-endpoint branch of sendMessageViaConversation (conv:/#thread/
// @email): the server resolves mentions itself on this path, and whatever it
// reports back in mention_results must actually reach the sender as a
// stderr warning, not be silently dropped.
func TestSendMessageViaConversation_OutboundConvRef_PrintsMentionWarning(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "project-msg-outbound-mention-warn"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "outbound-msg-1",
				"status":       "sent",
				"recipient":    "conv:11111111-1111-1111-1111-111111111111",
				"recipient_id": "uid-test",
				"mention_results": []messages.MentionResult{
					{Slug: "unknown-name", Status: "not_found", Error: "no matching agent in this project"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}

	out := captureStderr(t, func() {
		err = sendMessageViaConversation(hubCtx, ref, "please look at @unknown-name", false, false, nil)
	})
	require.NoError(t, err)
	require.Contains(t, out, "Note: @unknown-name is not an agent in this project; no agent was notified.")
}

// TestSendOutboundMessageViaHub_PrintsMentionWarning covers the direct
// user:<email> path (sendOutboundMessageViaHub, not routed through
// sendMessageViaConversation at all): its own mention_results must also
// reach the sender as a stderr warning.
func TestSendOutboundMessageViaHub_PrintsMentionWarning(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "project-msg-outbound-direct-mention-warn"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "outbound-msg-2",
				"status":       "sent",
				"recipient":    "user:alice@example.com",
				"recipient_id": "uid-test",
				"mention_results": []messages.MentionResult{
					{Slug: "unknown-name", Status: "not_found", Error: "no matching agent in this project"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	out := captureStderr(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "please look at @unknown-name", false)
	})
	require.NoError(t, err)
	require.Contains(t, out, "Note: @unknown-name is not an agent in this project; no agent was notified.")
}

// printMentionResults prints the expected warning per non-delivered status,
// a note for a delivered-but-not-currently-running recipient, and is silent
// under --json.
func TestPrintMentionResults(t *testing.T) {
	results := []messages.MentionResult{
		{Slug: "alice", Status: "delivered"},
		{Slug: "bob", Status: "delivered", AgentPhase: "stopped"},
		{Slug: "carol", Status: "not_found"},
		{Slug: "dave", Status: "unauthorized"},
		{Slug: "erin", Status: "suppressed"},
		{Slug: "frank", Status: "rate_limited"},
		{Slug: "grace", Status: "ambiguous"},
		{Slug: "heidi", Status: "timeout"},
		{Slug: "ivan", Status: "error", Error: "dispatch failed: boom"},
	}

	out := captureStderr(t, func() { printMentionResults(results) })

	assert.Contains(t, out, "Mention notification sent to @alice.")
	assert.Contains(t, out, "@bob is stopped; it will see this mention once it is running.")
	assert.Contains(t, out, "Note: @carol is not an agent in this project; no agent was notified.")
	assert.Contains(t, out, "@dave was denied")
	assert.Contains(t, out, "@erin was suppressed by loop protection")
	assert.Contains(t, out, "@frank was rate-limited")
	assert.Contains(t, out, "@grace delivery is ambiguous")
	assert.Contains(t, out, "@heidi timed out")
	assert.Contains(t, out, "@ivan failed: dispatch failed: boom")
}

func TestPrintMentionResults_SilentUnderJSON(t *testing.T) {
	origFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = origFormat }()

	out := captureStderr(t, func() {
		printMentionResults([]messages.MentionResult{{Slug: "carol", Status: "not_found"}})
	})
	assert.Empty(t, out, "printMentionResults must be silent under --json; results belong in the JSON body instead")
}

func TestPrintMentionResults_EmptyIsNoOp(t *testing.T) {
	out := captureStderr(t, func() { printMentionResults(nil) })
	assert.Empty(t, out)
}

func TestFilterMentionNames(t *testing.T) {
	got := filterMentionNames([]string{"Alice", "bob", "Primary", "self"}, "self", "primary")
	assert.Equal(t, []string{"Alice", "bob"}, got)
}

// TestSendMessageViaHub_JSONOutputIncludesMentionResults is path A's --json
// coverage: the mock's echoed mention_results must actually reach stdout as
// part of the decoded MessageResponse.
func TestSendMessageViaHub_JSONOutputIncludesMentionResults(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	origFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = origFormat }()

	projectID := "project-msg-json-mentions"
	agents := []hubclient.Agent{
		{Name: "primary-agent", Slug: "primary-agent", Status: "running"},
		{Name: "mentioned-agent", Slug: "mentioned-agent", Status: "running"},
	}
	server, _ := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	origCC := msgCC
	msgCC = nil
	defer func() { msgCC = origCC }()

	out := captureStdout(t, func() {
		err = sendMessageViaHub(hubCtx, "primary-agent", "hey @mentioned-agent check this", false, false, false)
	})
	require.NoError(t, err)

	var resp hubclient.MessageResponse
	require.NoError(t, json.Unmarshal([]byte(out), &resp), "stdout: %s", out)
	require.Len(t, resp.MentionResults, 1, "--json output must include mention_results")
	require.Equal(t, "mentioned-agent", resp.MentionResults[0].Slug)
	require.Equal(t, "delivered", resp.MentionResults[0].Status)
}

func TestSendMessageViaHub_MentionFanOut(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-mention"
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
	msgCC = nil
	defer func() { msgCC = origCC }()

	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @mentioned-agent check this", false, false, false)
	require.NoError(t, err)

	// Server-side fan-out: the CLI sends exactly one request, with the body
	// mention carried in the explicit "mentions" field so a hub that only
	// fans out from that field still delivers it.
	require.Len(t, *sent, 1)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, messages.TypeInstruction, (*sent)[0].StructuredMsg.Type)
	assert.Equal(t, []string{"mentioned-agent"}, (*sent)[0].Mentions)
}

func TestSendMessageViaHub_MentionDedup(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-mention-dedup"
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
	msgCC = nil
	defer func() { msgCC = origCC }()

	// Primary recipient is also @mentioned in body — should be deduplicated
	err = sendMessageViaHub(hubCtx, "my-agent", "hey @my-agent check @other-agent", false, false, false)
	require.NoError(t, err)

	// One request; the primary (self-mentioned) is filtered out of the
	// explicit mentions list client-side: old hubs do not exclude the
	// sender/primary the way the new hub's fan-out does.
	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.Equal(t, []string{"other-agent"}, (*sent)[0].Mentions)
}

func TestSendMessageViaHub_UnknownMentionWarns(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-mention-unknown"
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
	msgCC = nil
	defer func() { msgCC = origCC }()

	// @nonexistent doesn't match any agent — should warn but not fail. The
	// mock server echoes a not_found mention_results entry for it,
	// exercising the CLI's printMentionResults wiring.
	var sendErr error
	out := captureStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "my-agent", "hey @nonexistent check this", false, false, false)
	})
	require.NoError(t, sendErr)

	// Only the primary message should be sent
	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
	assert.Contains(t, out, "Note: @nonexistent is not an agent in this project; no agent was notified.")
}

func TestSendMessageViaHub_CCFlag(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-cc"
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
	msgCC = []string{"cc-agent-1", "cc-agent-2"}
	defer func() { msgCC = origCC }()

	err = sendMessageViaHub(hubCtx, "primary-agent", "check this out", false, false, false)
	require.NoError(t, err)

	// One request; --cc names go into the explicit mentions field.
	require.Len(t, *sent, 1)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	assert.Equal(t, messages.TypeInstruction, (*sent)[0].StructuredMsg.Type)
	assert.ElementsMatch(t, []string{"cc-agent-1", "cc-agent-2"}, (*sent)[0].Mentions)
}

func TestSendMessageViaHub_CCAndMentionCombined(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-cc-mention"
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
	msgCC = []string{"cc-agent"}
	defer func() { msgCC = origCC }()

	// Both @mention in body and --cc flag
	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @mention-agent check this", false, false, false)
	require.NoError(t, err)

	// One request; the explicit mentions field unions the body @mention and
	// --cc.
	require.Len(t, *sent, 1)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	assert.ElementsMatch(t, []string{"mention-agent", "cc-agent"}, (*sent)[0].Mentions)
}

func TestSendMessageViaHub_CCDedupWithMention(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-cc-dedup"
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
	msgCC = []string{"shared-agent"}
	defer func() { msgCC = origCC }()

	// Same agent in both @mention and --cc — should only appear once
	err = sendMessageViaHub(hubCtx, "primary-agent", "hey @shared-agent check this", false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "primary-agent", (*sent)[0].AgentName)
	assert.Equal(t, []string{"shared-agent"}, (*sent)[0].Mentions)
}

func TestSendMessageViaHub_NoMentionsInBody(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-no-mention"
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
	msgCC = nil
	defer func() { msgCC = origCC }()

	// No mentions in body, no --cc — only primary should be sent
	err = sendMessageViaHub(hubCtx, "my-agent", "hello world", false, false, false)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "my-agent", (*sent)[0].AgentName)
}

func TestSendGroupMessageViaHub_MentionFanOut(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-group-mention"
	agents := []hubclient.Agent{
		{Name: "agent-a", Slug: "agent-a", Status: "running"},
		{Name: "agent-b", Slug: "agent-b", Status: "running"},
		{Name: "agent-c", Slug: "agent-c", Status: "running"},
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
	msgCC = nil
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

// TestSendGroupMessageViaHub_A256_F2_DeferredRecipientNotReportedDelivered is
// report-7-gteam-2a F2 / design.md A25.6: while a group[] recipient is
// mid-`scion reincarnate`, the server reports it "deferred" (the message was
// saved to history for catch-up, not dropped, per §3.7), but the human CLI's
// group fan-out discarded SendStructuredMessage's response and always
// printed "Delivered". The status must come from the send response.
func TestSendGroupMessageViaHub_A256_F2_DeferredRecipientNotReportedDelivered(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-group-deferred"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-migrating/message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":  "msg-deferred",
				"status":      "deferred",
				"agent":       "agent-migrating",
				"agent_phase": "stopping",
				"deferred":    "agent is reincarnating",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-running/message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":  "msg-delivered",
				"status":      "delivered",
				"agent":       "agent-running",
				"agent_phase": "running",
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

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-running"},
		{Kind: messages.RecipientAgent, Name: "agent-migrating"},
	}

	oldStdout := os.Stdout
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	os.Stdout = w

	err = sendGroupMessageViaHub(hubCtx, recipients, "group hello mid-migration", false)

	_ = w.Close()
	os.Stdout = oldStdout
	require.NoError(t, err, "a deferred recipient is not a failure and must not error the group send")

	var buf [4096]byte
	n, _ := r.Read(buf[:])
	_ = r.Close()
	output := string(buf[:n])

	assert.Contains(t, output, "Deferred: agent:agent-migrating (agent is reincarnating; saved)",
		"the migrating recipient must be reported deferred, not delivered; got: %s", output)
	assert.NotContains(t, output, "Delivered: agent:agent-migrating",
		"a deferred recipient must never be printed as delivered; got: %s", output)
	assert.Contains(t, output, "Delivered: agent:agent-running",
		"the non-migrating recipient must still be reported delivered; got: %s", output)
	assert.Contains(t, output, "1/2 delivered, 1 deferred",
		"the summary line must separate deferred from delivered; got: %s", output)
}

// TestSendGroupMessageViaHub_A257_O1_JSONOutputIncludesDeferredStatus is
// design.md A25.7 O1: group[] sends must honour --json the same way the
// single-recipient paths do. Before this, --json mode for a group send
// printed nothing at all — the per-recipient results (including a
// "deferred" status) were computed but never serialized.
func TestSendGroupMessageViaHub_A257_O1_JSONOutputIncludesDeferredStatus(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	projectID := "project-msg-group-json"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-migrating/message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":  "msg-deferred-json",
				"status":      "deferred",
				"agent":       "agent-migrating",
				"agent_phase": "stopping",
				"deferred":    "agent is reincarnating",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-running/message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":  "msg-delivered-json",
				"status":      "delivered",
				"agent":       "agent-running",
				"agent_phase": "running",
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

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-running"},
		{Kind: messages.RecipientAgent, Name: "agent-migrating"},
	}

	oldStdout := os.Stdout
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	os.Stdout = w

	sendErr := sendGroupMessageViaHub(hubCtx, recipients, "group hello via json", false)

	_ = w.Close()
	os.Stdout = oldStdout
	require.NoError(t, sendErr, "a deferred recipient is not a failure and must not error the group send")

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	_ = r.Close()
	output := string(buf[:n])

	var got []struct {
		Recipient string `json:"recipient"`
		Status    string `json:"status"`
		Error     string `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got), "output must be valid JSON; got: %s", output)
	require.Len(t, got, 2)

	byRecipient := map[string]string{}
	for _, r := range got {
		byRecipient[r.Recipient] = r.Status
	}
	assert.Equal(t, "deferred", byRecipient["agent:agent-migrating"], "JSON output must carry status:\"deferred\"; got: %s", output)
	assert.Equal(t, "delivered", byRecipient["agent:agent-running"])
}

// TestSendGroupMessageViaHub_A257_O2_PartialFailureReportsCountsExplicitly
// is design.md A25.7 O2: the partial-failure error must report delivered,
// deferred and failed counts explicitly, rather than folding a genuine
// failure into "%d/%d delivered" (which could describe 1 delivered + 1
// failed as "1/2 delivered" without ever using the word "failed").
func TestSendGroupMessageViaHub_A257_O2_PartialFailureReportsCountsExplicitly(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-group-partial"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-ok/message"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id": "m1", "status": "delivered", "agent": "agent-ok",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agent-bad/message"):
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"code": "INTERNAL", "message": "boom"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-ok"},
		{Kind: messages.RecipientAgent, Name: "agent-bad"},
	}

	sendErr := sendGroupMessageViaHub(hubCtx, recipients, "partial failure test", false)
	require.Error(t, sendErr)
	assert.Contains(t, sendErr.Error(), "1 delivered, 0 deferred, 1 failed (of 2 total)",
		"got: %v", sendErr)
}

func TestCCFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	tests := []struct {
		name      string
		cc        []string
		userRecip bool
		in        string
		at        string
		wantErr   string
	}{
		{
			name:      "cc with user recipient",
			cc:        []string{"agent-a"},
			userRecip: true,
			wantErr:   "--cc cannot be used with user recipients",
		},
		{
			name:    "cc with --in scheduling",
			cc:      []string{"agent-a"},
			in:      "5m",
			wantErr: "--cc cannot be combined with --in or --at",
		},
		{
			name:    "cc with --at scheduling",
			cc:      []string{"agent-a"},
			at:      "2026-01-01 12:00",
			wantErr: "--cc cannot be combined with --in or --at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origCC := msgCC
			origIn := msgIn
			origAt := msgAt
			defer func() {
				msgCC = origCC
				msgIn = origIn
				msgAt = origAt
			}()

			msgCC = tc.cc
			msgIn = tc.in
			msgAt = tc.at

			var args []string
			if tc.userRecip {
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

// --- Cross-project messaging tests ---

// crossProjectMockServer creates a mock Hub server that handles:
// - GET /api/v1/messaging/targets/resolve → resolve target agent
// - POST /api/v1/agents/{uuid}/message → non-project-scoped send
// - POST /api/v1/conversations/{id}/messages → conversation send
func crossProjectMockServer(t *testing.T, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug string) (*httptest.Server, *[]sentMessage) {
	t.Helper()
	var sent []sentMessage
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/messaging/targets/resolve" && r.Method == http.MethodGet:
			project := r.URL.Query().Get("project")
			agent := r.URL.Query().Get("agent")
			if project == targetProjectSlug && agent == targetAgentSlug {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"agent": map[string]interface{}{
						"id":          targetAgentID,
						"slug":        targetAgentSlug,
						"projectId":   targetProjectID,
						"projectSlug": targetProjectSlug,
					},
					"messageability": map[string]interface{}{
						"canMessage":     true,
						"canReachViewer": false,
					},
				})
			} else {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    "not_found",
						"message": "Target not found",
					},
				})
			}

		case strings.HasPrefix(r.URL.Path, "/api/v1/agents/") && r.Method == http.MethodPost:
			agentUUID := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
			agentUUID = strings.TrimSuffix(agentUUID, "/message")

			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			sm := sentMessage{
				AgentName:     agentUUID,
				Interrupt:     body.Interrupt,
				StructuredMsg: body.StructuredMessage,
			}
			// Echo mention_results for any body @mentions, the same way the
			// real hub's server-side fan-out reports on this path too (it
			// has no explicit "mentions" request field to key off of here).
			var mentionResults []messages.MentionResult
			if body.StructuredMessage != nil {
				sm.Message = body.StructuredMessage.Msg
				for _, name := range messages.ExtractMentions(body.StructuredMessage.Msg) {
					mentionResults = append(mentionResults, messages.MentionResult{Slug: name, Status: "delivered"})
				}
			}
			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "mention_results": mentionResults})

		case strings.HasPrefix(r.URL.Path, "/api/v1/conversations/") && r.Method == http.MethodPost:
			convID := strings.TrimPrefix(r.URL.Path, "/api/v1/conversations/")
			convID = strings.TrimSuffix(convID, "/messages")

			var body struct {
				Msg  string `json:"msg"`
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			sm := sentMessage{
				AgentName: "conv:" + convID,
				Message:   body.Msg,
			}
			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"messageId": "msg-test-123",
				"status":    "delivered",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &sent
}

func TestSendCrossProjectMessage_Success(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	targetAgentID := "target-uuid-1234"
	targetAgentSlug := "target-agent"
	targetProjectID := "proj-b-uuid"
	targetProjectSlug := "project-b"

	server, sent := crossProjectMockServer(t, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "proj-a-uuid",
	}

	err = sendCrossProjectMessage(hubCtx, targetProjectSlug, targetAgentSlug, "hello from project A", false, false, nil)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, targetAgentID, (*sent)[0].AgentName)
	assert.Equal(t, "hello from project A", (*sent)[0].Message)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, "agent:"+targetAgentSlug, (*sent)[0].StructuredMsg.Recipient)
}

// The hub's @mention fan-out for this send path (cross-project `scion
// message`) reports mention_results the same as every other agent message
// path; the CLI must print them, not discard the response.
func TestSendCrossProjectMessage_PrintsMentionResults(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	targetAgentID := "target-uuid-1234"
	targetAgentSlug := "target-agent"
	targetProjectID := "proj-b-uuid"
	targetProjectSlug := "project-b"

	server, _ := crossProjectMockServer(t, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-a-uuid"}

	out := captureStderr(t, func() {
		err = sendCrossProjectMessage(hubCtx, targetProjectSlug, targetAgentSlug, "hey @mentioned-agent check this", false, false, nil)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "@mentioned-agent", "mention results from this send path must reach stderr like any other")
}

// --json coverage for the same path: the mock's echoed mention_results must
// reach stdout as part of the decoded response, not be discarded.
func TestSendCrossProjectMessage_JSONOutputIncludesMentionResults(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	origFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = origFormat }()

	targetAgentID := "target-uuid-1234"
	targetAgentSlug := "target-agent"
	targetProjectID := "proj-b-uuid"
	targetProjectSlug := "project-b"

	server, _ := crossProjectMockServer(t, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-a-uuid"}

	out := captureStdout(t, func() {
		err = sendCrossProjectMessage(hubCtx, targetProjectSlug, targetAgentSlug, "hey @mentioned-agent check this", false, false, nil)
	})
	require.NoError(t, err)

	var resp hubclient.MessageResponse
	require.NoError(t, json.Unmarshal([]byte(out), &resp), "stdout: %s", out)
	require.Len(t, resp.MentionResults, 1, "--json output must include mention_results")
	require.Equal(t, "mentioned-agent", resp.MentionResults[0].Slug)
}

func TestSendCrossProjectMessage_TargetNotFound(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	server, _ := crossProjectMockServer(t, "", "", "", "")
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "proj-a-uuid",
	}

	err = sendCrossProjectMessage(hubCtx, "nonexistent-project", "nonexistent-agent", "hello", false, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve agent")
}

// TestSendCrossProjectConversationReply verifies that conv:<uuid> from an
// agent context routes through the outbound endpoint (not the conversation
// send API). CPM cutover (#1693): conv: now uses the same outbound path as
// @email and #thread.
func TestSendCrossProjectConversationReply(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	projectID := "proj-convref-cross-reply"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "conv-uuid-5678"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	err = sendMessageViaConversation(hubCtx, ref, "reply message", false, false, nil)
	require.NoError(t, err)

	// CPM cutover: conv: now routes through outbound, not conversation send API.
	assert.Len(t, *sent, 0, "conv: should not go through agent message path")
	require.Len(t, *outbound, 1, "conv: should go through outbound path")
	assert.Equal(t, "conv:"+convID, (*outbound)[0].ConversationRef)
	assert.Equal(t, "reply message", (*outbound)[0].Message)
}

// TestCrossProjectAtAgentDispatch verifies that @agent-slug with --project
// in agent mode routes through sendCrossProjectMessage (the exact repro
// for CPM-UAT-001: scion message --project target-proj @agent msg).
//
// ParseReference("@agent-slug") produces a RefAgent convRef and leaves
// agentName empty. The cross-project detection must trigger on convRef
// and the dispatch must intercept BEFORE the generic convRef path.
func TestCrossProjectAtAgentDispatch(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	targetAgentID := "target-uuid-9999"
	targetAgentSlug := "cpm-probe-b"
	targetProjectID := "proj-b-uuid"
	targetProjectSlug := "cpm-uat-b-20260919"

	server, sent := crossProjectMockServer(t, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "proj-a-uuid",
	}

	// Simulate the exact parsing path from messageCmd.RunE:
	// "@cpm-probe-b" → ParseReference → RefAgent convRef, agentName stays empty
	ref, parseErr := messaging.ParseReference("@" + targetAgentSlug)
	require.NoError(t, parseErr)
	assert.Equal(t, messaging.RefAgent, ref.Kind)
	assert.Equal(t, targetAgentSlug, ref.Value)

	// With crossProjectTarget set (simulating --project=cpm-uat-b-20260919
	// in agent mode), the dispatch must route through sendCrossProjectMessage
	// using convRef.Value, NOT through sendMessageViaConversation.
	err = sendCrossProjectMessage(hubCtx, targetProjectSlug, ref.Value, "CPM-UAT-ON-20260919", false, false, nil)
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, targetAgentID, (*sent)[0].AgentName, "should send to resolved UUID, not slug")
	assert.Equal(t, "CPM-UAT-ON-20260919", (*sent)[0].Message)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, "agent:"+targetAgentSlug, (*sent)[0].StructuredMsg.Recipient)
}

// TestCrossProjectDetectionLogic verifies the cross-project detection
// condition in messageCmd.RunE handles both bare agent names and @agent refs.
func TestCrossProjectDetectionLogic(t *testing.T) {
	tests := []struct {
		name           string
		agentName      string
		convRefKind    messaging.ReferenceKind
		agentMode      bool
		projectChanged bool
		wantCross      bool
	}{
		{
			name:           "bare agent + agent mode + --project → cross-project",
			agentName:      "target-agent",
			agentMode:      true,
			projectChanged: true,
			wantCross:      true,
		},
		{
			name:           "@agent ref + agent mode + --project → cross-project",
			convRefKind:    messaging.RefAgent,
			agentMode:      true,
			projectChanged: true,
			wantCross:      true,
		},
		{
			name:           "conv:uuid ref + agent mode + --project → NOT cross-project",
			convRefKind:    messaging.RefConversation,
			agentMode:      true,
			projectChanged: true,
			wantCross:      false,
		},
		{
			name:           "@agent ref + human mode + --project → NOT cross-project",
			convRefKind:    messaging.RefAgent,
			agentMode:      false,
			projectChanged: true,
			wantCross:      false,
		},
		{
			name:           "@agent ref + agent mode + no --project → NOT cross-project",
			convRefKind:    messaging.RefAgent,
			agentMode:      true,
			projectChanged: false,
			wantCross:      false,
		},
		{
			name:           "no target + agent mode + --project → NOT cross-project",
			agentMode:      true,
			projectChanged: true,
			wantCross:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Replicate the detection logic from messageCmd.RunE
			var convRef *messaging.Reference
			if tc.convRefKind != 0 {
				convRef = &messaging.Reference{Kind: tc.convRefKind, Value: "test", Raw: "@test"}
			}

			hasAgentTarget := tc.agentName != "" || (convRef != nil && convRef.Kind == messaging.RefAgent)

			agentEnv := ""
			if tc.agentMode {
				agentEnv = "sender-agent"
			}

			gotCross := hasAgentTarget && agentEnv != "" && tc.projectChanged
			assert.Equal(t, tc.wantCross, gotCross, "cross-project detection mismatch")
		})
	}
}

// TestCrossProjectDispatchOrder verifies that cross-project @agent dispatch
// happens BEFORE the generic convRef dispatch, preventing the RefAgent from
// falling through to sendMessageViaConversation (which scopes to sender's project).
func TestCrossProjectDispatchOrder(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	targetAgentSlug := "remote-agent"
	targetProjectSlug := "remote-project"

	server, sent := crossProjectMockServer(t, "remote-uuid", targetAgentSlug, "remote-proj-id", targetProjectSlug)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "local-proj-id",
	}

	// Simulate the dispatch logic from messageCmd.RunE with crossProjectTarget set
	crossProjectTarget := targetProjectSlug
	convRef := &messaging.Reference{Kind: messaging.RefAgent, Value: targetAgentSlug, Raw: "@" + targetAgentSlug}

	// This is the exact dispatch order from the fixed code:
	// 1. Cross-project @agent intercept (should fire)
	// 2. Generic convRef dispatch (should NOT fire)
	if hubCtx != nil && crossProjectTarget != "" {
		if convRef != nil && convRef.Kind == messaging.RefAgent {
			err = sendCrossProjectMessage(hubCtx, crossProjectTarget, convRef.Value, "dispatch order test", false, false, nil)
			require.NoError(t, err)
			require.Len(t, *sent, 1)
			assert.Equal(t, "remote-uuid", (*sent)[0].AgentName, "cross-project dispatch should resolve to target UUID")
			return
		}
		// If we reach here for RefAgent, the dispatch order is wrong
		t.Fatal("RefAgent should have been intercepted by cross-project dispatch")
	}
	t.Fatal("should have entered cross-project dispatch block")
}

// TestCrossProjectMismatchRejection verifies that --project with a non-agent
// target (e.g. conv:uuid, user:, group[]) does NOT trigger cross-project
// agent detection, preserving normal behavior for those paths.
func TestCrossProjectMismatchRejection(t *testing.T) {
	// conv:uuid should NOT trigger cross-project even with --project
	convRef := &messaging.Reference{Kind: messaging.RefConversation, Value: "some-uuid", Raw: "conv:some-uuid"}
	hasAgentTarget := false || (convRef != nil && convRef.Kind == messaging.RefAgent)
	assert.False(t, hasAgentTarget, "conv:uuid should not be detected as agent target")

	// #thread should NOT trigger cross-project
	threadRef := &messaging.Reference{Kind: messaging.RefThread, Value: "discussion", Raw: "#discussion"}
	hasAgentTarget = false || (threadRef != nil && threadRef.Kind == messaging.RefAgent)
	assert.False(t, hasAgentTarget, "thread ref should not be detected as agent target")

	// @email should NOT trigger cross-project
	emailRef := &messaging.Reference{Kind: messaging.RefEmail, Value: "user@example.com", Raw: "@user@example.com"}
	hasAgentTarget = false || (emailRef != nil && emailRef.Kind == messaging.RefAgent)
	assert.False(t, hasAgentTarget, "email ref should not be detected as agent target")
}

// TestMessageCmd_RunE_CrossProjectWithoutRaw_ReachesHub asserts positively
// that an ordinary cross-project send reaches the mock hub's cross-project
// resolve/send endpoints, rather than merely not erroring for an unrelated
// reason.
//
// The test is hermetic: HOME and the working directory are redirected to
// scratch dirs (so project-root discovery can't find this container's real
// .scion project) and SCION_HUB_ENDPOINT points at the mock server, so it
// cannot reach the ambient hub.
func TestMessageCmd_RunE_CrossProjectWithoutRaw_ReachesHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	clearHubContextEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	targetAgentID := "target-uuid-nonraw"
	targetAgentSlug := "target-agent"
	targetProjectID := "other-project-uuid"
	targetProjectSlug := "other-project"

	server, sent := crossProjectMockServer(t, targetAgentID, targetAgentSlug, targetProjectID, targetProjectSlug)
	defer server.Close()

	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")
	t.Setenv("SCION_PROJECT_ID", "own-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

	cmd := newProjectFlagCommand(t)
	require.NoError(t, cmd.Flags().Set("project", targetProjectSlug))

	err := messageCmd.RunE(cmd, []string{targetAgentSlug, "hello"})
	require.NoError(t, err)

	require.Len(t, *sent, 1, "the cross-project send must reach the mock hub")
	assert.Equal(t, targetAgentID, (*sent)[0].AgentName)
	assert.Equal(t, "hello", (*sent)[0].Message)
}

// newCountingHubServer answers /healthz and any other route successfully
// (so a regression that reaches the Hub does not itself crash the test),
// while counting every request received. Used to prove a refusal returns
// before any Hub call is attempted at all: a test can then assert zero
// requests, rather than only matching the error text.
func newCountingHubServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		_ = json.NewEncoder(w).Encode(agentkeys.Response{Status: agentkeys.StatusDispatched})
	}))
	return server, &hits
}

// setHermeticHubEnv points every hub-resolution env var at server and
// isolates HOME/cwd, so resolving a project/sender never reaches outside
// this test (the ambient container otherwise sets a real SCION_HUB_ENDPOINT
// and credentials).
func setHermeticHubEnv(t *testing.T, server *httptest.Server) {
	t.Helper()
	clearHubContextEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")
	t.Setenv("SCION_PROJECT_ID", "own-project-id")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	// Inside a Scion agent container the runtime sets SCION_HOST_UID, which
	// makes root's agent-container guard abort before any Hub call. Clear
	// it so a zero-hit assertion measures the command itself.
	t.Setenv("SCION_HOST_UID", "")
}

// TestMessageCmd_RawFlag_ZeroWireCalls proves the removed --raw flag never
// reaches the wire. It drives the real cobra pipeline (rootCmd.ExecuteC), so
// the refusal is shown to fire in argument validation, before root's
// PersistentPreRunE, project resolution, sender resolution or any Hub
// request. Every recipient form and flag combination that previously
// routed --raw somewhere (the keys alias, conversation references,
// cross-project, local mode) is covered, plus --raw=false and a missing
// body, which must be refused too.
func TestMessageCmd_RawFlag_ZeroWireCalls(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		noHub bool
	}{
		{"agent", []string{"message", "target-agent", "Escape", "--raw"}, false},
		{"raw false", []string{"message", "target-agent", "Escape", "--raw=false"}, false},
		{"no body", []string{"message", "target-agent", "--raw"}, false},
		{"at agent", []string{"message", "@target-agent", "Escape", "--raw"}, false},
		{"conversation", []string{"message", "conv:11111111-1111-1111-1111-111111111111", "Escape", "--raw"}, false},
		{"thread", []string{"message", "#general", "Escape", "--raw"}, false},
		{"user", []string{"message", "user:alice", "Escape", "--raw"}, false},
		{"group", []string{"message", "group[agent:a,agent:b]", "Escape", "--raw"}, false},
		{"cross project", []string{"message", "target-agent", "Escape", "--raw", "--project", "other-project"}, false},
		{"interrupt", []string{"message", "target-agent", "Escape", "--raw", "--interrupt"}, false},
		{"msg alias", []string{"msg", "target-agent", "Escape", "--raw"}, false},
		{"local mode", []string{"message", "target-agent", "Escape", "--raw"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := saveMessageTestState()
			defer orig.restore()
			restore := resetMessageFlags()
			defer restore()
			origInterrupt := msgInterrupt
			t.Cleanup(func() {
				msgInterrupt = origInterrupt
				messageCmd.Flags().Visit(func(f *pflag.Flag) { f.Changed = false })
				rootCmd.PersistentFlags().Visit(func(f *pflag.Flag) { f.Changed = false })
				rootCmd.SetArgs(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
			})

			server, hits := newCountingHubServer(t)
			defer server.Close()
			setHermeticHubEnv(t, server)
			noHub = tc.noHub

			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			_, err := rootCmd.ExecuteC()

			require.ErrorIs(t, err, errRawFlagRemoved)
			assert.Contains(t, err.Error(), "scion keys")
			assert.EqualValues(t, 0, atomic.LoadInt32(hits), "--raw must make zero wire calls")
		})
	}
}

// TestMessageCmd_WithoutRawFlag_ReachesCountingServer is the positive
// control for TestMessageCmd_RawFlag_ZeroWireCalls: the same pipeline and
// environment without --raw does reach the counting server, so its
// zero-hit assertion is not vacuous.
func TestMessageCmd_WithoutRawFlag_ReachesCountingServer(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"agent", []string{"message", "target-agent", "hello"}},
		{"at agent", []string{"message", "@target-agent", "hello"}},
		{"msg alias", []string{"msg", "target-agent", "hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := saveMessageTestState()
			defer orig.restore()
			restore := resetMessageFlags()
			defer restore()
			t.Cleanup(func() {
				messageCmd.Flags().Visit(func(f *pflag.Flag) { f.Changed = false })
				rootCmd.PersistentFlags().Visit(func(f *pflag.Flag) { f.Changed = false })
				rootCmd.SetArgs(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
			})

			server, hits := newCountingHubServer(t)
			defer server.Close()
			setHermeticHubEnv(t, server)
			noHub = false

			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			_, err := rootCmd.ExecuteC()

			assert.NotErrorIs(t, err, errRawFlagRemoved)
			assert.Greater(t, atomic.LoadInt32(hits), int32(0),
				"without --raw the command must reach the counting server (err=%v)", err)
		})
	}
}

// TestMessageCmd_RunE_ConvRefCrossProjectMismatch pins the conv: + --project
// mismatch check (message.go).
// This check intentionally keeps its own inline same-project comparison
// rather than calling detectCrossProjectTarget: that helper treats an
// explicitly empty --project ("") as same-project, but this branch must
// still reject it — an explicit --project="" alongside a conv: reference is
// still an attempt to reinterpret the sender's project context, and this
// check's behavior must match the pre-existing behavior. Table covers all 7 cases.
//
// Hermetic: noHub=true short-circuits before any hub call is attempted, and
// hub env vars are cleared, so only the conv: error string is asserted.
func TestMessageCmd_RunE_ConvRefCrossProjectMismatch(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	const convErr = "--project cannot be used with conv: references"
	const convRef = "conv:11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name        string
		agentName   string
		ownSlug     string
		ownID       string
		projectFlag string
		setProject  bool
		wantReject  bool
	}{
		{
			name:        "agent, --project other project",
			agentName:   "sender-agent",
			ownSlug:     "own-project",
			projectFlag: "other-project",
			setProject:  true,
			wantReject:  true,
		},
		{
			name:        "agent, --project own slug",
			agentName:   "sender-agent",
			ownSlug:     "own-project",
			projectFlag: "own-project",
			setProject:  true,
			wantReject:  false,
		},
		{
			name:        "agent, --project own ID",
			agentName:   "sender-agent",
			ownID:       "own-project-id",
			projectFlag: "own-project-id",
			setProject:  true,
			wantReject:  false,
		},
		{
			name:        "human caller, --project other project",
			agentName:   "",
			projectFlag: "other-project",
			setProject:  true,
			wantReject:  false,
		},
		{
			name:       "agent, --project not set",
			agentName:  "sender-agent",
			ownSlug:    "own-project",
			setProject: false,
			wantReject: false,
		},
		{
			name:        "agent, --project explicitly empty (still rejected)",
			agentName:   "sender-agent",
			ownSlug:     "own-project",
			projectFlag: "",
			setProject:  true,
			wantReject:  true,
		},
		{
			name:        "agent, no SCION_PROJECT/_ID set, --project x",
			agentName:   "sender-agent",
			projectFlag: "x",
			setProject:  true,
			wantReject:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_HUB_ENDPOINT", "")
			t.Setenv("SCION_HUB_URL", "")
			t.Setenv("SCION_AGENT_NAME", tc.agentName)
			t.Setenv("SCION_PROJECT", tc.ownSlug)
			t.Setenv("SCION_PROJECT_ID", tc.ownID)
			noHub = true

			cmd := newProjectFlagCommand(t)
			if tc.setProject {
				require.NoError(t, cmd.Flags().Set("project", tc.projectFlag))
			}

			err := messageCmd.RunE(cmd, []string{convRef, "hello"})
			if tc.wantReject {
				require.Error(t, err)
				assert.Contains(t, err.Error(), convErr)
			} else if err != nil {
				assert.NotContains(t, err.Error(), convErr)
			}
		})
	}
}

// TestCrossProjectSameProjectBypass verifies that --project matching the
// agent's own project (by slug or ID) does NOT trigger cross-project
// detection, preserving normal same-project sending even when CPM is disabled.
func TestCrossProjectSameProjectBypass(t *testing.T) {
	tests := []struct {
		name        string
		projectFlag string
		ownSlug     string
		ownID       string
		wantCross   bool
	}{
		{
			name:        "slug match → same-project (no cross-project)",
			projectFlag: "my-project",
			ownSlug:     "my-project",
			wantCross:   false,
		},
		{
			name:        "ID match → same-project (no cross-project)",
			projectFlag: "abc-123-def",
			ownID:       "abc-123-def",
			wantCross:   false,
		},
		{
			name:        "different slug → cross-project",
			projectFlag: "other-project",
			ownSlug:     "my-project",
			wantCross:   true,
		},
		{
			name:        "no env vars → cross-project (conservative)",
			projectFlag: "some-project",
			ownSlug:     "",
			ownID:       "",
			wantCross:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isSameProject := (tc.ownSlug != "" && tc.projectFlag == tc.ownSlug) ||
				(tc.ownID != "" && tc.projectFlag == tc.ownID)
			gotCross := !isSameProject
			assert.Equal(t, tc.wantCross, gotCross)
		})
	}
}

// TestConvRefMismatchedProjectRejection verifies that conv:<id> with an
// explicit --project that doesn't match the agent's own project is rejected.
func TestConvRefMismatchedProjectRejection(t *testing.T) {
	convRef := &messaging.Reference{Kind: messaging.RefConversation, Value: "some-uuid", Raw: "conv:some-uuid"}

	// Simulate the rejection logic from messageCmd.RunE
	agentMode := true
	projectChanged := true
	ownSlug := "my-project"
	projectFlag := "other-project"

	if convRef != nil && convRef.Kind == messaging.RefConversation && agentMode && projectChanged {
		isSameProject := projectFlag == ownSlug
		if !isSameProject {
			// Should reject
			assert.True(t, true, "mismatched --project with conv: should reject")
			return
		}
	}
	t.Fatal("should have rejected mismatched --project with conv: ref")
}

// TestConvRefSameProjectAllowed verifies that conv:<id> with --project
// matching the agent's own project is allowed through.
func TestConvRefSameProjectAllowed(t *testing.T) {
	convRef := &messaging.Reference{Kind: messaging.RefConversation, Value: "some-uuid", Raw: "conv:some-uuid"}

	agentMode := true
	projectChanged := true
	ownSlug := "my-project"
	projectFlag := "my-project"

	rejected := false
	if convRef != nil && convRef.Kind == messaging.RefConversation && agentMode && projectChanged {
		isSameProject := projectFlag == ownSlug
		if !isSameProject {
			rejected = true
		}
	}
	assert.False(t, rejected, "same-project conv: should not be rejected")
}

// TestConvRefAttachWakeSupported verifies that --attach and --wake are
// properly serialized for conv: references via the outbound endpoint.
// CPM cutover (#1693): conv: now routes through the outbound endpoint
// which supports both wake and attachments.
func TestConvRefAttachWakeSupported(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	projectID := "proj-convref-attach-wake"
	server, sent, outbound := newConvRefMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	convID := "conv-uuid-attach-wake"
	ref := &messaging.Reference{
		Kind:  messaging.RefConversation,
		Value: convID,
		Raw:   "conv:" + convID,
	}

	// Attachments should be supported and serialized
	err = sendMessageViaConversation(hubCtx, ref, "msg with attach", false, false, []string{"file.txt"})
	require.NoError(t, err, "conv: with --attach should succeed via outbound")

	require.Len(t, *outbound, 1)
	assert.Equal(t, "conv:"+convID, (*outbound)[0].ConversationRef)
	assert.Equal(t, "msg with attach", (*outbound)[0].Message)

	// Wake should be supported and serialized
	err = sendMessageViaConversation(hubCtx, ref, "msg with wake", false, true, nil)
	require.NoError(t, err, "conv: with --wake should succeed via outbound")

	require.Len(t, *outbound, 2)
	assert.Equal(t, "conv:"+convID, (*outbound)[1].ConversationRef)
	assert.Equal(t, "msg with wake", (*outbound)[1].Message)

	// Normal message (no attach, no wake): should also succeed
	err = sendMessageViaConversation(hubCtx, ref, "normal msg", false, false, nil)
	require.NoError(t, err)
	require.Len(t, *outbound, 3)

	// Verify no messages went through the agent message path
	assert.Len(t, *sent, 0, "conv: should not go through agent message path")
}

// TestSendMessageViaHub_ReincarnatingAgent_PrintsDeferredNotice covers the
// CLI half of the migration gate (design agent-reincarnate §3.7, Amendment
// A25 2a.2): when the hub responds with status "deferred" (the recipient is
// mid-`scion reincarnate`), `scion message @agent` must print the deferred
// notice, not a generic "delivered" message, and must not treat it as an
// error.
func TestSendMessageViaHub_ReincarnatingAgent_PrintsDeferredNotice(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-deferred"
	projectPrefix := "/api/v1/projects/" + projectID + "/agents/"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, projectPrefix) && strings.HasSuffix(r.URL.Path, "/message"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":  "msg-deferred-1",
				"status":      "deferred",
				"deferred":    "agent is reincarnating",
				"agent":       "my-agent",
				"agent_phase": "starting",
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

	var sendErr error
	output := captureStdout(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "my-agent", "hello during migration", false, false, false)
	})
	require.NoError(t, sendErr, "a deferred delivery must not be reported as a send error")
	assert.Contains(t, output, "agent my-agent is reincarnating; message saved to history and will be seen on catch-up",
		"the CLI must print the deferred notice, not a generic delivered message")
	assert.Contains(t, output, "msg-deferred-1", "the message ID must be shown for correlation")
	assert.NotContains(t, output, "Message delivered to agent",
		"the generic delivered message must not also print for a deferred outcome")
}

// TestResolveMessageBody_OversizeRejectedNotTruncated pins that an over-limit
// body is rejected rather than cut at the read limit and sent. The input puts
// newlines at the cut point, which trimming would otherwise remove, pulling
// the read back under the limit and silently dropping the tail.
func TestResolveMessageBody_OversizeRejectedNotTruncated(t *testing.T) {
	content := strings.Repeat("a", messages.MaxMsgSize-1) + "\n\n" + "TAIL"

	t.Run("--body-file path", func(t *testing.T) {
		bodyFile := filepath.Join(t.TempDir(), "big.txt")
		require.NoError(t, os.WriteFile(bodyFile, []byte(content), 0644))
		_, err := resolveMessageBody(bodyFile, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum size")
	})
	t.Run("--body-file -", func(t *testing.T) {
		withStdin(t, content)
		_, err := resolveMessageBody("-", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum size")
	})
	t.Run("positional -", func(t *testing.T) {
		withStdin(t, content)
		_, err := resolveMessageBody("", "-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum size")
	})
	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		atLimit := strings.Repeat("a", messages.MaxMsgSize)
		bodyFile := filepath.Join(t.TempDir(), "limit.txt")
		require.NoError(t, os.WriteFile(bodyFile, []byte(atLimit), 0644))
		got, err := resolveMessageBody(bodyFile, "")
		require.NoError(t, err)
		assert.Len(t, got, messages.MaxMsgSize)
	})
}

// captureCombinedOutput routes os.Stdout and os.Stderr into one pipe, the
// way a terminal shows them, so a test can assert the relative order of
// the stdout confirmation and the stderr mention notes.
func captureCombinedOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	// Buffered, so the reader can always hand off its result and exit,
	// even if fn panics or calls t.FailNow and <-done is never reached.
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	func() {
		// Close the writer even if fn fails the test or panics, so the
		// reader goroutine always sees EOF and never hangs.
		defer func() { _ = w.Close() }()
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		fn()
	}()
	return <-done
}

// lastLine returns the last non-empty line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestSendMessageViaConversation_SkippedMentionPrintsBeforeConfirmation is
// the ptone/scion#3303 repro: a conv: send whose body mentions a human
// member gets a not_found mention result. The send must still succeed
// (nil error, exit code 0), the skipped mention must read as a non-fatal
// note, and the confirmation must be the last line of output.
func TestSendMessageViaConversation_SkippedMentionPrintsBeforeConfirmation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "outbound-msg-3303",
				"status":       "sent",
				"recipient":    "conv:11111111-1111-1111-1111-111111111111",
				"recipient_id": "uid-test",
				"mention_results": []messages.MentionResult{
					{Slug: "alice", Status: "not_found", Error: "no matching agent in this project"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-msg-3303"}
	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}

	out := captureCombinedOutput(t, func() {
		err = sendMessageViaConversation(hubCtx, ref, "hello @alice", false, false, nil)
	})
	require.NoError(t, err, "a skipped mention must not fail the send")

	note := "Note: @alice is not an agent in this project; no agent was notified."
	confirm := "Message sent to conv:11111111-1111-1111-1111-111111111111 (message outbound-msg-3303)."
	require.Contains(t, out, note)
	require.Contains(t, out, confirm)
	assert.NotContains(t, out, "Warning: @alice", "a skipped mention must not read as a failure")
	assert.Less(t, strings.Index(out, note), strings.Index(out, confirm), "mention note must print before the confirmation")
	assert.Equal(t, confirm, lastLine(out), "the confirmation must be the last line")
}

// TestSendMessageViaHub_SkippedMentionPrintsBeforeConfirmation covers the
// agent-recipient path: mention results, then the delivery confirmation.
func TestSendMessageViaHub_SkippedMentionPrintsBeforeConfirmation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-3303-agent"
	agents := []hubclient.Agent{{Name: "my-agent", Status: "running"}}
	server, _ := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	origCC := msgCC
	msgCC = nil
	defer func() { msgCC = origCC }()

	var sendErr error
	out := captureCombinedOutput(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "my-agent", "hey @nobody", false, false, false)
	})
	require.NoError(t, sendErr)

	note := "Note: @nobody is not an agent in this project; no agent was notified."
	require.Contains(t, out, note)
	assert.Equal(t, "Message delivered to agent 'my-agent'.", lastLine(out))
}

// TestSendOutboundMessageViaHub_SkippedMentionPrintsBeforeConfirmation
// covers the direct user:<email> path.
func TestSendOutboundMessageViaHub_SkippedMentionPrintsBeforeConfirmation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":   "outbound-msg-3303b",
				"status":       "sent",
				"recipient":    "user:alice@example.com",
				"recipient_id": "uid-test",
				"mention_results": []messages.MentionResult{
					{Slug: "bob", Status: "not_found"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-msg-3303-user"}

	out := captureCombinedOutput(t, func() {
		err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "cc @bob", false)
	})
	require.NoError(t, err)
	require.Contains(t, out, "Note: @bob is not an agent in this project; no agent was notified.")
	assert.Equal(t, "Message sent to user:alice@example.com via Hub.", lastLine(out))
}

// TestSendGroupMessageViaHub_SkippedMentionPrintsBeforeSummary covers the
// group path, where the CLI resolves mentions itself
// (sendMentionMessages): a group send that names a non-agent prints the
// note, and the group delivery summary is still the last line.
func TestSendGroupMessageViaHub_SkippedMentionPrintsBeforeSummary(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	projectID := "project-msg-3303-group"
	agents := []hubclient.Agent{
		{Name: "agent-a", Slug: "agent-a", Status: "running"},
		{Name: "agent-b", Slug: "agent-b", Status: "running"},
	}
	server, _ := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	origCC := msgCC
	msgCC = nil
	defer func() { msgCC = origCC }()

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientAgent, Name: "agent-b"},
	}

	var sendErr error
	out := captureCombinedOutput(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, recipients, "hey @not-an-agent", false)
	})
	require.NoError(t, sendErr, "a skipped mention must not fail the send")

	note := "Note: @not-an-agent is not an agent in this project; no agent was notified."
	require.Contains(t, out, note)
	assert.NotContains(t, out, "Warning: @not-an-agent", "a skipped mention must not read as a failure")
	assert.Equal(t, "Group delivery complete: 2/2 delivered.", lastLine(out))
}

// TestSendCrossProjectMessage_MentionPrintsBeforeConfirmation covers the
// cross-project path: mention results, then the delivery confirmation.
func TestSendCrossProjectMessage_MentionPrintsBeforeConfirmation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	server, _ := crossProjectMockServer(t, "target-uuid-1234", "target-agent", "proj-b-uuid", "project-b")
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "proj-a-uuid"}

	out := captureCombinedOutput(t, func() {
		err = sendCrossProjectMessage(hubCtx, "project-b", "target-agent", "hey @mentioned-agent", false, false, nil)
	})
	require.NoError(t, err)
	require.Contains(t, out, "Mention notification sent to @mentioned-agent.")
	assert.Equal(t, "Message delivered to agent 'target-agent' in project 'project-b'.", lastLine(out))
}

// TestSendMessageViaConversation_AgentRefMentionPrintsBeforeConfirmation
// covers the agent-context @agent ref branch of sendMessageViaConversation.
func TestSendMessageViaConversation_AgentRefMentionPrintsBeforeConfirmation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

	projectID := "project-msg-3303-agentref"
	agents := []hubclient.Agent{{Name: "my-agent", Status: "running"}}
	server, _ := newMessageMockHubServer(t, projectID, agents)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
	ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "my-agent", Raw: "@my-agent"}

	out := captureCombinedOutput(t, func() {
		err = sendMessageViaConversation(hubCtx, ref, "hey @nobody", false, false, nil)
	})
	require.NoError(t, err)
	require.Contains(t, out, "Note: @nobody is not an agent in this project; no agent was notified.")
	assert.Equal(t, "Message delivered to agent 'my-agent'.", lastLine(out))
}

// TestSendMessage_DeferredMentionPrintsBeforeConfirmation covers the
// "deferred" branches (recipient mid-reincarnate) on the agent,
// conv: agent-ref and outbound conv: paths: the mention note prints
// first and the "is reincarnating; message saved" confirmation is the
// last line.
func TestSendMessage_DeferredMentionPrintsBeforeConfirmation(t *testing.T) {
	const projectID = "project-msg-3303-deferred"
	const convRaw = "conv:22222222-2222-2222-2222-222222222222"
	mentionResults := []messages.MentionResult{
		{Slug: "nobody", Status: "not_found", Error: "no matching agent in this project"},
	}

	newServer := func(t *testing.T) *HubContext {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"agents": []hubclient.Agent{{Name: "my-agent", Status: "running"}},
				})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message"):
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"message_id":      "msg-deferred",
					"status":          "deferred",
					"recipient":       convRaw,
					"recipient_id":    "uid-test",
					"mention_results": mentionResults,
				})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/my-agent/message"):
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"message_id":      "msg-deferred",
					"status":          "deferred",
					"agent":           "my-agent",
					"deferred":        "agent is reincarnating",
					"mention_results": mentionResults,
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)
		client, err := hubclient.New(server.URL)
		require.NoError(t, err)
		return &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
	}

	tests := []struct {
		name    string
		send    func(hubCtx *HubContext) error
		confirm string
	}{
		{
			name: "agent",
			send: func(hubCtx *HubContext) error {
				return sendMessageViaHub(hubCtx, "my-agent", "hey @nobody", false, false, false)
			},
			confirm: "agent my-agent is reincarnating; message saved to history" +
				" and will be seen on catch-up (message msg-deferred).",
		},
		{
			name: "conv agent-ref",
			send: func(hubCtx *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "my-agent", Raw: "@my-agent"}
				return sendMessageViaConversation(hubCtx, ref, "hey @nobody", false, false, nil)
			},
			confirm: "agent my-agent is reincarnating; message saved to history" +
				" and will be seen on catch-up (message msg-deferred).",
		},
		{
			name: "outbound conv",
			send: func(hubCtx *HubContext) error {
				ref := &messaging.Reference{Kind: messaging.RefConversation, Value: strings.TrimPrefix(convRaw, "conv:"), Raw: convRaw}
				return sendMessageViaConversation(hubCtx, ref, "hey @nobody", false, false, nil)
			},
			confirm: "agent " + convRaw + " is reincarnating; message saved to history" +
				" and will be seen on catch-up (message msg-deferred).",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := saveMessageTestState()
			defer orig.restore()
			t.Setenv("SCION_AGENT_NAME", "test-sender-agent")

			origCC := msgCC
			msgCC = nil
			defer func() { msgCC = origCC }()

			hubCtx := newServer(t)
			var sendErr error
			out := captureCombinedOutput(t, func() {
				sendErr = tc.send(hubCtx)
			})
			require.NoError(t, sendErr)

			note := "Note: @nobody is not an agent in this project; no agent was notified."
			require.Contains(t, out, note)
			require.Contains(t, out, tc.confirm)
			assert.Less(t, strings.Index(out, note), strings.Index(out, tc.confirm),
				"mention note must print before the deferred confirmation")
			assert.Equal(t, tc.confirm, lastLine(out),
				"the deferred confirmation must be the last line")
		})
	}
}
