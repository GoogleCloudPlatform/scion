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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newScheduledMessageMockHubServer stands up a fake Hub that validates
// POST /api/v1/projects/{projectID}/scheduled-events with the same required
// message-event fields as pkg/hub/handlers_scheduled_events.go.
func newScheduledMessageMockHubServer(t *testing.T, expectedProjectID string) (*httptest.Server, *[]hubclient.CreateScheduledEventRequest) {
	t.Helper()
	var recorded []hubclient.CreateScheduledEventRequest
	var mu sync.Mutex

	expectedPath := "/api/v1/projects/" + expectedProjectID + "/scheduled-events"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})

		case r.URL.Path == expectedPath && r.Method == http.MethodPost:
			var req hubclient.CreateScheduledEventRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "validation_error", "message": "invalid JSON"},
				})
				return
			}
			mu.Lock()
			recorded = append(recorded, req)
			mu.Unlock()

			if req.Message == "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "validation_error", "message": "message is required for message events"},
				})
				return
			}
			if req.AgentID == "" && req.AgentName == "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "validation_error", "message": "agentId or agentName is required for message events"},
				})
				return
			}
			if req.FireIn == "" && req.FireAt == "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "validation_error", "message": "either fireAt or fireIn is required"},
				})
				return
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "evt-7854",
				"projectId": expectedProjectID,
				"eventType": req.EventType,
				"fireAt":    "2030-01-01T00:15:00Z",
				"status":    "pending",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &recorded
}

func setupScheduledMessageCLIEnv(t *testing.T, serverURL, projectID string) {
	t.Helper()
	orig := saveMessageTestState()
	origFormat := outputFormat
	restoreFlags := resetMessageFlags()

	t.Cleanup(func() {
		outputFormat = origFormat
		for _, name := range []string{"in", "at", "interrupt", "plain"} {
			if f := messageCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
		restoreFlags()
		orig.restore()
	})

	isolateHubEnvForTest(t, serverURL, projectID)
	t.Setenv("SCION_AGENT_NAME", "")
	t.Setenv("SCION_PROJECT", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, serverURL, projectID)
	outputFormat = ""
}

// TestMessageScheduled_RunE_AgentRecipients reproduces ptone/scion#7854:
// scion message @<agent> "..." --in/--at previously left agentName empty when
// parseRecipientArg populated convRef (RefAgent), causing the Hub to reject
// the request with "validation_error: agentId or agentName is required for
// message events". All agent recipient forms (@<agent>, agent:<name>, <name>)
// must warn on stderr, create the scheduled event with the normalized agent
// slug, and print confirmation with the event ID on stdout.
func TestMessageScheduled_RunE_AgentRecipients(t *testing.T) {
	tests := []struct {
		name      string
		recipient string
		flagName  string
		flagVal   string
		wantSlug  string
	}{
		{
			name:      "@agent recipient with --in",
			recipient: "@choices-owner",
			flagName:  "in",
			flagVal:   "45m",
			wantSlug:  "choices-owner",
		},
		{
			name:      "@Agent mixed-case recipient with --at",
			recipient: "@My-Worker",
			flagName:  "at",
			flagVal:   "2030-01-01T00:15:00Z",
			wantSlug:  "my-worker",
		},
		{
			name:      "agent: prefix recipient with --in",
			recipient: "agent:my-worker",
			flagName:  "in",
			flagVal:   "15m",
			wantSlug:  "my-worker",
		},
		{
			name:      "bare agent recipient with --in",
			recipient: "my-worker",
			flagName:  "in",
			flagVal:   "15m",
			wantSlug:  "my-worker",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const projectID = "proj-sched-7854"
			server, recorded := newScheduledMessageMockHubServer(t, projectID)
			setupScheduledMessageCLIEnv(t, server.URL, projectID)

			require.NoError(t, messageCmd.Flags().Set(tc.flagName, tc.flagVal))

			var stdout, stderr string
			var runErr error
			stdout = captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					runErr = messageCmd.RunE(messageCmd, []string{tc.recipient, "recheck status"})
				})
			})

			require.NoError(t, runErr)
			assert.Contains(t, stderr, "Warning: --"+tc.flagName+" is deprecated")
			assert.Contains(t, stderr, "scion schedule create")

			require.Len(t, *recorded, 1)
			got := (*recorded)[0]
			assert.Equal(t, "message", got.EventType)
			assert.Equal(t, tc.wantSlug, got.AgentName)
			assert.Equal(t, "recheck status", got.Message)
			if tc.flagName == "in" {
				assert.Equal(t, tc.flagVal, got.FireIn)
				assert.Empty(t, got.FireAt)
			} else {
				assert.Equal(t, tc.flagVal, got.FireAt)
				assert.Empty(t, got.FireIn)
			}

			assert.Contains(t, stdout, tc.wantSlug)
			assert.Contains(t, stdout, "2030-01-01")
			assert.Contains(t, stdout, "evt-7854", "stdout confirmation should include created event ID")
		})
	}
}

// TestMessageScheduled_RunE_JSONOutput verifies that when outputFormat is
// "json", scheduleMessageViaHub writes the created ScheduledEvent JSON to
// stdout rather than exiting with empty stdout.
func TestMessageScheduled_RunE_JSONOutput(t *testing.T) {
	const projectID = "proj-sched-json"
	server, recorded := newScheduledMessageMockHubServer(t, projectID)
	setupScheduledMessageCLIEnv(t, server.URL, projectID)
	outputFormat = "json"

	require.NoError(t, messageCmd.Flags().Set("in", "10m"))

	var runErr error
	stdout := captureStdout(t, func() {
		_ = captureStderr(t, func() {
			runErr = messageCmd.RunE(messageCmd, []string{"@my-worker", "json callback"})
		})
	})

	require.NoError(t, runErr)
	require.Len(t, *recorded, 1)
	assert.Equal(t, "my-worker", (*recorded)[0].AgentName)

	var evt hubclient.ScheduledEvent
	require.NoError(t, json.Unmarshal([]byte(stdout), &evt), "stdout: %s", stdout)
	assert.Equal(t, "evt-7854", evt.ID)
	assert.False(t, evt.FireAt.IsZero(), "decoded FireAt should be non-zero")
}

// TestMessageScheduled_RunE_UnsupportedRecipientsFail verifies that non-agent
// or empty-slug recipients combined with --in/--at fail before contacting the
// Hub. User and group[] recipients explain that scheduled messages can only
// target agents without suggesting 'scion schedule create --agent' as an
// equivalent, while conversation/thread and empty-slug recipients point to
// 'scion schedule create'.
func TestMessageScheduled_RunE_UnsupportedRecipientsFail(t *testing.T) {
	tests := []struct {
		name               string
		recipient          string
		wantErr            string
		wantScheduleCreate bool
	}{
		{
			name:               "@email user recipient",
			recipient:          "@alice@example.com",
			wantErr:            "--in/--at cannot be used with user recipients; scheduled messages can only target agents",
			wantScheduleCreate: false,
		},
		{
			name:               "user: recipient",
			recipient:          "user:alice",
			wantErr:            "--in/--at cannot be used with user recipients; scheduled messages can only target agents",
			wantScheduleCreate: false,
		},
		{
			name:               "conv: conversation reference",
			recipient:          "conv:7f3a91c2-1234-5678-9abc-def012345678",
			wantErr:            "--in/--at cannot be used with conv:",
			wantScheduleCreate: true,
		},
		{
			name:               "#thread reference",
			recipient:          "#general",
			wantErr:            "--in/--at cannot be used with #general",
			wantScheduleCreate: true,
		},
		{
			name:               "group[] recipient",
			recipient:          "group[agent:a,agent:b]",
			wantErr:            "--in/--at cannot be used with group[] recipients; scheduled messages can only target agents",
			wantScheduleCreate: false,
		},
		{
			name:               "empty agent: prefix",
			recipient:          "agent:",
			wantErr:            "agent recipient is required for --in/--at",
			wantScheduleCreate: true,
		},
		{
			name:               "@ punctuation-only agent slug",
			recipient:          "@---",
			wantErr:            "agent recipient is required for --in/--at",
			wantScheduleCreate: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const projectID = "proj-sched-invalid-recip"
			server, recorded := newScheduledMessageMockHubServer(t, projectID)
			setupScheduledMessageCLIEnv(t, server.URL, projectID)

			require.NoError(t, messageCmd.Flags().Set("in", "15m"))

			var err error
			_ = captureStderr(t, func() {
				err = messageCmd.RunE(messageCmd, []string{tc.recipient, "hello"})
			})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			if tc.wantScheduleCreate {
				assert.Contains(t, err.Error(), "scion schedule create")
			} else {
				assert.NotContains(t, err.Error(), "scion schedule create")
			}
			assert.Empty(t, *recorded, "invalid recipient must fail before sending a Hub request")
		})
	}
}

// TestMessageScheduled_RunE_CrossProjectRefused verifies that an agent passing
// --project <other-project> with --in/--at is refused instead of silently
// creating the scheduled event in the sender's own project.
func TestMessageScheduled_RunE_CrossProjectRefused(t *testing.T) {
	t.Run("foreign --project with @agent is refused", func(t *testing.T) {
		const senderProjectID = "proj-sender"
		server, recorded := newScheduledMessageMockHubServer(t, senderProjectID)
		setupScheduledMessageCLIEnv(t, server.URL, senderProjectID)
		t.Chdir(t.TempDir())

		t.Setenv("SCION_AGENT_NAME", "sender-agent")
		t.Setenv("SCION_PROJECT", "sender-proj")
		t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

		cmd := newProjectFlagCommand(t)
		cmd.Flags().AddFlagSet(messageCmd.LocalFlags())
		require.NoError(t, cmd.Flags().Set("project", "other-proj"))
		require.NoError(t, cmd.Flags().Set("in", "15m"))

		var err error
		_ = captureStderr(t, func() {
			err = messageCmd.RunE(cmd, []string{"@target-agent", "hello"})
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "cross-project")
		assert.Contains(t, err.Error(), "scion schedule create")
		assert.Empty(t, *recorded, "cross-project --in/--at must not create an event in sender's project")
	})

	t.Run("foreign --project with bare agent is refused instead of scheduling in sender project", func(t *testing.T) {
		const senderProjectID = "proj-sender"
		server, recorded := newScheduledMessageMockHubServer(t, senderProjectID)
		setupScheduledMessageCLIEnv(t, server.URL, senderProjectID)
		t.Chdir(t.TempDir())

		t.Setenv("SCION_AGENT_NAME", "sender-agent")
		t.Setenv("SCION_PROJECT", "sender-proj")
		t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")

		cmd := newProjectFlagCommand(t)
		cmd.Flags().AddFlagSet(messageCmd.LocalFlags())
		require.NoError(t, cmd.Flags().Set("project", "other-proj"))
		require.NoError(t, cmd.Flags().Set("in", "15m"))

		var err error
		_ = captureStdout(t, func() {
			_ = captureStderr(t, func() {
				err = messageCmd.RunE(cmd, []string{"target-agent", "hello"})
			})
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "cross-project")
		assert.Contains(t, err.Error(), "scion schedule create")
		assert.Empty(t, *recorded, "cross-project --in/--at must not silently schedule in sender's project")
	})

	t.Run("same --project as agent's own project succeeds", func(t *testing.T) {
		const senderProjectID = "proj-sender"
		server, recorded := newScheduledMessageMockHubServer(t, senderProjectID)
		setupScheduledMessageCLIEnv(t, server.URL, senderProjectID)

		ownProjectDir := projectPath
		t.Setenv("SCION_AGENT_NAME", "sender-agent")
		t.Setenv("SCION_PROJECT", ownProjectDir)

		cmd := newProjectFlagCommand(t)
		cmd.Flags().AddFlagSet(messageCmd.LocalFlags())
		require.NoError(t, cmd.Flags().Set("project", ownProjectDir))
		require.NoError(t, cmd.Flags().Set("in", "15m"))

		var err error
		_ = captureStdout(t, func() {
			_ = captureStderr(t, func() {
				err = messageCmd.RunE(cmd, []string{"@sender-agent", "self check"})
			})
		})

		require.NoError(t, err)
		require.Len(t, *recorded, 1)
		assert.Equal(t, "sender-agent", (*recorded)[0].AgentName)
	})
}

// TestScheduleMessageViaHub_RejectsEmptyEventID verifies that if an endpoint
// returns 200 OK with an empty event ID, scheduleMessageViaHub fails with a
// clear error pointing to 'scion schedule create' rather than exiting 0.
func TestScheduleMessageViaHub_RejectsEmptyEventID(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	defer resetMessageFlags()()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "proj-empty-evt",
	}
	msgIn = "15m"

	_ = captureStderr(t, func() {
		err = scheduleMessageViaHub(hubCtx, "my-agent", "hello", false, false)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scion schedule create")
}

// TestScheduleMessageViaHub_HubErrorPointsToScheduleCreate verifies that a Hub
// rejection (e.g., invalid duration) fails with non-zero exit and names
// 'scion schedule create'.
func TestScheduleMessageViaHub_HubErrorPointsToScheduleCreate(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	defer resetMessageFlags()()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "validation_error",
				"message": "invalid fireIn duration",
			},
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: "proj-bad-dur",
	}
	msgIn = "not-a-duration"

	_ = captureStderr(t, func() {
		err = scheduleMessageViaHub(hubCtx, "my-agent", "hello", false, false)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid fireIn duration")
	assert.Contains(t, err.Error(), "scion schedule create")
}
