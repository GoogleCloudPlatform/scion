/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
)

// scrubHubEnv clears all Hub-related environment variables for the
// duration of the test, preventing accidental communication with a
// real Hub when tests run inside an agent container. See issue #123.
func scrubHubEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SCION_HUB_ENDPOINT",
		"SCION_HUB_URL",
		"SCION_AUTH_TOKEN",
		"SCION_AGENT_ID",
		"SCION_AGENT_MODE",
	} {
		t.Setenv(key, "")
	}
}

// TestHubHandler_EventMapping tests that events are correctly mapped to Hub status updates.
func TestHubHandler_EventMapping(t *testing.T) {
	tests := []struct {
		name           string
		eventName      string
		eventData      hooks.EventData
		expectCall     bool
		expectedStatus string
	}{
		{
			name:           "session start sends working (running phase)",
			eventName:      hooks.EventSessionStart,
			expectCall:     true,
			expectedStatus: "working",
		},
		{
			name:           "prompt submit sends thinking",
			eventName:      hooks.EventPromptSubmit,
			expectCall:     true,
			expectedStatus: "thinking",
		},
		{
			name:           "agent start sends thinking",
			eventName:      hooks.EventAgentStart,
			expectCall:     true,
			expectedStatus: "thinking",
		},
		{
			name:           "tool start sends executing",
			eventName:      hooks.EventToolStart,
			eventData:      hooks.EventData{ToolName: "Bash"},
			expectCall:     true,
			expectedStatus: "executing",
		},
		{
			name:           "tool end sends working",
			eventName:      hooks.EventToolEnd,
			expectCall:     true,
			expectedStatus: "working",
		},
		{
			name:           "agent end sends working",
			eventName:      hooks.EventAgentEnd,
			expectCall:     true,
			expectedStatus: "working",
		},
		{
			name:           "notification sends waiting_for_input",
			eventName:      hooks.EventNotification,
			eventData:      hooks.EventData{Message: "What should I do?"},
			expectCall:     true,
			expectedStatus: "waiting_for_input",
		},
		{
			name:           "session end sends stopped",
			eventName:      hooks.EventSessionEnd,
			expectCall:     true,
			expectedStatus: "stopped",
		},
		{
			name:       "pre start does not send",
			eventName:  hooks.EventPreStart,
			expectCall: false,
		},
		{
			name:       "post start does not send",
			eventName:  hooks.EventPostStart,
			expectCall: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)

			var receivedStatus string
			var mu sync.Mutex
			callCount := 0

			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				callCount++

				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("Failed to decode request body: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}

				// Status field carries backward-compat value
				if status, ok := payload["status"].(string); ok {
					receivedStatus = status
				}

				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()

			// Clear real Hub env, then point at the test server (issue #123).
			scrubHubEnv(t)
			t.Setenv("SCION_HUB_ENDPOINT", server.URL)
			t.Setenv("SCION_AUTH_TOKEN", "test-token")
			t.Setenv("SCION_AGENT_ID", "test-agent-id")

			// Create handler
			handler := NewHubHandler()
			if handler == nil {
				t.Fatal("Expected handler to be created, got nil")
			}

			// Process event
			event := &hooks.Event{
				Name: tt.eventName,
				Data: tt.eventData,
			}

			err := handler.Handle(event)
			if err != nil {
				t.Errorf("Handle returned error: %v", err)
			}

			mu.Lock()
			gotCalls := callCount
			gotStatus := receivedStatus
			mu.Unlock()

			if tt.expectCall {
				if gotCalls != 1 {
					t.Errorf("Expected 1 call, got %d", gotCalls)
				}
				if gotStatus != tt.expectedStatus {
					t.Errorf("Expected status %q, got %q", tt.expectedStatus, gotStatus)
				}
			} else {
				if gotCalls != 0 {
					t.Errorf("Expected no calls, got %d", gotCalls)
				}
			}
		})
	}
}

// TestHubHandler_NotConfigured tests that nil handler doesn't panic.
func TestHubHandler_NotConfigured(t *testing.T) {
	// Clear environment to ensure client is not configured (issue #123).
	scrubHubEnv(t)

	handler := NewHubHandler()
	if handler != nil {
		t.Error("Expected handler to be nil when not configured")
	}

	// Nil handler should not panic when Handle is called
	var nilHandler *HubHandler
	err := nilHandler.Handle(&hooks.Event{Name: hooks.EventSessionStart})
	if err != nil {
		t.Errorf("Nil handler returned error: %v", err)
	}
}

// TestHubHandler_ReportMethods tests the explicit report methods.
func TestHubHandler_ReportMethods(t *testing.T) {
	var receivedPayload map[string]interface{}
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		_ = json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// Clear real Hub env, then point at the test server (issue #123).
	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	handler := NewHubHandler()
	if handler == nil {
		t.Fatal("Expected handler to be created")
	}

	t.Run("ReportWaitingForInput", func(t *testing.T) {
		mu.Lock()
		receivedPayload = nil
		mu.Unlock()

		err := handler.ReportWaitingForInput("What should I do?")
		if err != nil {
			t.Errorf("ReportWaitingForInput returned error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedPayload["status"] != "waiting_for_input" {
			t.Errorf("Expected status 'waiting_for_input', got %v", receivedPayload["status"])
		}
		if receivedPayload["activity"] != "waiting_for_input" {
			t.Errorf("Expected activity 'waiting_for_input', got %v", receivedPayload["activity"])
		}
		if receivedPayload["message"] != "What should I do?" {
			t.Errorf("Expected message 'What should I do?', got %v", receivedPayload["message"])
		}
	})

	t.Run("ReportTaskCompleted", func(t *testing.T) {
		mu.Lock()
		receivedPayload = nil
		mu.Unlock()

		err := handler.ReportTaskCompleted("Fixed the bug")
		if err != nil {
			t.Errorf("ReportTaskCompleted returned error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedPayload["status"] != "completed" {
			t.Errorf("Expected status 'completed', got %v", receivedPayload["status"])
		}
		if receivedPayload["activity"] != "completed" {
			t.Errorf("Expected activity 'completed', got %v", receivedPayload["activity"])
		}
		if receivedPayload["taskSummary"] != "Fixed the bug" {
			t.Errorf("Expected taskSummary 'Fixed the bug', got %v", receivedPayload["taskSummary"])
		}
	})
}

// TestHubHandler_StickyStatus tests that the Hub handler respects sticky activities.
// When the local activity (written by StatusHandler) is waiting_for_input or completed,
// non-new-work events should not overwrite it on the Hub.
func TestHubHandler_StickyStatus(t *testing.T) {
	tests := []struct {
		name           string
		localActivity  string // activity in agent-info.json
		eventName      string
		eventData      hooks.EventData
		expectCall     bool
		expectedStatus string
	}{
		{
			name:          "tool-end skipped when local activity is waiting_for_input",
			localActivity: "waiting_for_input",
			eventName:     hooks.EventToolEnd,
			expectCall:    false,
		},
		{
			name:          "tool-end skipped when local activity is completed",
			localActivity: "completed",
			eventName:     hooks.EventToolEnd,
			expectCall:    false,
		},
		{
			name:           "tool-end sends working when local activity is working",
			localActivity:  "working",
			eventName:      hooks.EventToolEnd,
			expectCall:     true,
			expectedStatus: "working",
		},
		{
			name:          "agent-end skipped when local activity is waiting_for_input",
			localActivity: "waiting_for_input",
			eventName:     hooks.EventAgentEnd,
			expectCall:    false,
		},
		{
			name:          "model-end skipped when local activity is completed",
			localActivity: "completed",
			eventName:     hooks.EventModelEnd,
			expectCall:    false,
		},
		{
			name:          "model-start skipped when local activity is waiting_for_input",
			localActivity: "waiting_for_input",
			eventName:     hooks.EventModelStart,
			expectCall:    false,
		},
		{
			name:          "model-start skipped when local activity is completed",
			localActivity: "completed",
			eventName:     hooks.EventModelStart,
			expectCall:    false,
		},
		{
			name:           "model-start sends thinking when local activity is working",
			localActivity:  "working",
			eventName:      hooks.EventModelStart,
			expectCall:     true,
			expectedStatus: "thinking",
		},
		{
			name:          "tool-start skipped when local activity is completed",
			localActivity: "completed",
			eventName:     hooks.EventToolStart,
			eventData:     hooks.EventData{ToolName: "Bash"},
			expectCall:    false,
		},
		{
			name:           "tool-start sends executing when local activity is working",
			localActivity:  "working",
			eventName:      hooks.EventToolStart,
			eventData:      hooks.EventData{ToolName: "Bash"},
			expectCall:     true,
			expectedStatus: "executing",
		},
		{
			name:           "prompt-submit always sends thinking (clears sticky waiting_for_input)",
			localActivity:  "waiting_for_input",
			eventName:      hooks.EventPromptSubmit,
			expectCall:     true,
			expectedStatus: "thinking",
		},
		{
			name:           "agent-start always sends thinking (clears sticky completed)",
			localActivity:  "completed",
			eventName:      hooks.EventAgentStart,
			expectCall:     true,
			expectedStatus: "thinking",
		},
		{
			name:           "session-start always sends working (clears sticky)",
			localActivity:  "waiting_for_input",
			eventName:      hooks.EventSessionStart,
			expectCall:     true,
			expectedStatus: "working",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up a temp dir with agent-info.json containing the local activity
			tmpDir := t.TempDir()
			info := map[string]interface{}{"activity": tt.localActivity}
			data, _ := json.Marshal(info)
			_ = os.WriteFile(tmpDir+"/agent-info.json", data, 0644)

			// Point HOME to the temp dir so readLocalActivity finds our file
			t.Setenv("HOME", tmpDir)

			var mu sync.Mutex
			callCount := 0
			var receivedStatus string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				callCount++

				var payload map[string]interface{}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if s, ok := payload["status"].(string); ok {
					receivedStatus = s
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()

			// Clear real Hub env, then point at the test server (issue #123).
			scrubHubEnv(t)
			t.Setenv("SCION_HUB_ENDPOINT", server.URL)
			t.Setenv("SCION_AUTH_TOKEN", "test-token")
			t.Setenv("SCION_AGENT_ID", "test-agent-id")

			handler := NewHubHandler()
			if handler == nil {
				t.Fatal("Expected handler to be created")
			}

			err := handler.Handle(&hooks.Event{
				Name: tt.eventName,
				Data: tt.eventData,
			})
			if err != nil {
				t.Errorf("Handle returned error: %v", err)
			}

			mu.Lock()
			gotCalls := callCount
			gotStatus := receivedStatus
			mu.Unlock()

			if tt.expectCall {
				if gotCalls != 1 {
					t.Errorf("Expected 1 call, got %d", gotCalls)
				}
				if gotStatus != tt.expectedStatus {
					t.Errorf("Expected status %q, got %q", tt.expectedStatus, gotStatus)
				}
			} else {
				if gotCalls != 0 {
					t.Errorf("Expected no calls, got %d", gotCalls)
				}
			}
		})
	}
}

// TestHubHandler_SessionStart_WirePayloadShape pins the exact wire body a
// real Claude SessionStart hook produces, end to end through the dialect and
// HubHandler (not a hand-built JSON literal). pkg/hub's since_create_ms
// start-time attribution (handlers_agent_lifecycle.go's updateAgentStatus)
// keys on exactly this phase/activity/message combination to recognize the
// harness's SessionStart report; if this shape ever changes, that hub-side
// match needs to change with it — this test is the tripwire for that.
func TestHubHandler_SessionStart_WirePayloadShape(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	var mu sync.Mutex
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		body = b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent")

	handler := NewHubHandler()
	if handler == nil {
		t.Fatal("expected HubHandler to be created")
	}

	// Parse the raw Claude Code hook payload through the real dialect, the
	// same way cmd/sciontool/commands/hook.go does for a live SessionStart
	// invocation, rather than constructing a hooks.Event by hand.
	dialect := dialects.NewClaudeDialect()
	raw := map[string]interface{}{
		"hook_event_name": "SessionStart",
		"session_id":      "sess-123",
		"source":          "startup",
	}
	event, err := dialect.Parse(raw)
	if err != nil {
		t.Fatalf("dialect.Parse: %v", err)
	}
	if event.Name != hooks.EventSessionStart {
		t.Fatalf("dialect.Parse normalized SessionStart to %q, want %q", event.Name, hooks.EventSessionStart)
	}

	if err := handler.Handle(event); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	mu.Lock()
	got := body
	mu.Unlock()

	var payload struct {
		Phase    string `json:"phase"`
		Activity string `json:"activity"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal wire body %s: %v", got, err)
	}

	if payload.Phase != "running" || payload.Activity != "working" || payload.Message != "Session started" {
		t.Errorf("SessionStart wire payload = %+v, want phase=running activity=working message=%q (pkg/hub's since_create_ms match depends on this exact shape)",
			payload, "Session started")
	}
}

// TestHubHandler_ModeBehavior verifies behavior differences between local and hub modes.
func TestHubHandler_ModeBehavior(t *testing.T) {
	t.Run("local mode: HubHandler is nil", func(t *testing.T) {
		// Clear hub env vars to simulate local mode (issue #123).
		scrubHubEnv(t)

		handler := NewHubHandler()
		if handler != nil {
			t.Error("HubHandler should be nil in local mode (no hub configured)")
		}
	})

	t.Run("local mode: StatusHandler always writes agent-info.json", func(t *testing.T) {
		// Even without a hub, the StatusHandler must write to agent-info.json
		// for local observability (defense-in-depth).
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		// Clear hub env to ensure local mode (issue #123).
		scrubHubEnv(t)

		statusHandler := NewStatusHandler()
		event := &hooks.Event{
			Name: hooks.EventSessionStart,
		}
		err := statusHandler.Handle(event)
		if err != nil {
			t.Fatalf("StatusHandler.Handle returned error: %v", err)
		}

		// Verify agent-info.json was written
		infoPath := tmpHome + "/agent-info.json"
		data, err := os.ReadFile(infoPath)
		if err != nil {
			t.Fatalf("agent-info.json should exist in local mode: %v", err)
		}

		var info map[string]interface{}
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatalf("agent-info.json should be valid JSON: %v", err)
		}
	})

	t.Run("hub mode: HubHandler is active and sends updates", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		callCount := 0
		var mu sync.Mutex

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			callCount++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		// Clear real Hub env, then point at the test server (issue #123).
		scrubHubEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", server.URL)
		t.Setenv("SCION_AUTH_TOKEN", "test-token")
		t.Setenv("SCION_AGENT_ID", "test-agent")

		handler := NewHubHandler()
		if handler == nil {
			t.Fatal("HubHandler should be non-nil when hub is configured")
		}

		event := &hooks.Event{
			Name: hooks.EventSessionStart,
		}
		err := handler.Handle(event)
		if err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}

		mu.Lock()
		got := callCount
		mu.Unlock()
		if got != 1 {
			t.Errorf("Expected 1 hub API call, got %d", got)
		}
	})

	t.Run("hub mode: StatusHandler still writes agent-info.json", func(t *testing.T) {
		// In hub mode, StatusHandler should still write locally for defense-in-depth.
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		// Clear real Hub env, then point at the test server (issue #123).
		scrubHubEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", server.URL)
		t.Setenv("SCION_AUTH_TOKEN", "test-token")
		t.Setenv("SCION_AGENT_ID", "test-agent")

		statusHandler := NewStatusHandler()
		event := &hooks.Event{
			Name: hooks.EventSessionStart,
		}
		err := statusHandler.Handle(event)
		if err != nil {
			t.Fatalf("StatusHandler.Handle returned error: %v", err)
		}

		// Verify agent-info.json was still written (defense-in-depth)
		infoPath := tmpHome + "/agent-info.json"
		data, err := os.ReadFile(infoPath)
		if err != nil {
			t.Fatalf("agent-info.json should exist even in hub mode: %v", err)
		}

		var info map[string]interface{}
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatalf("agent-info.json should be valid JSON: %v", err)
		}
	})
}

// fakeHub is a test Hub that mirrors the real hub's outbound-message
// contract: a request naming no addressee (recipient, recipient_id or
// conversation_ref) is rejected with 400, as resolveOutboundRouting does.
// GET /api/v1/agents/{id} returns the configured creator attribution.
// outboundStatus, when set, scripts the status (and Retry-After) of
// successive outbound-message requests before falling back to 200.
// fakeHub records the requests a HubHandler makes.
type fakeHub struct {
	t *testing.T

	mu            sync.Mutex
	outboundCalls int
	selfCalls     int
	statusCalls   int
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/test-agent-id":
		f.selfCalls++
		_, _ = w.Write([]byte(`{"id":"test-agent-id"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent-id/outbound-message":
		f.outboundCalls++
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent-id/status":
		f.statusCalls++
		_, _ = w.Write([]byte(`{}`))
	default:
		f.t.Errorf("fakeHub: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// start points the hub client env at f and returns a HubHandler.
func (f *fakeHub) start() *HubHandler {
	f.t.Helper()
	f.t.Setenv("HOME", f.t.TempDir())
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)

	// Clear real Hub env, then point at the test server (issue #123).
	scrubHubEnv(f.t)
	f.t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	f.t.Setenv("SCION_AUTH_TOKEN", "test-token")
	f.t.Setenv("SCION_AGENT_ID", "test-agent-id")

	handler := NewHubHandler()
	if handler == nil {
		f.t.Fatal("Expected handler to be created")
	}
	return handler
}

// TestHubHandler_AgentEndSendsNoMessage verifies that an end-of-turn event
// carrying the assistant's final text only updates status: the hook no
// longer mirrors end-of-turn text to the hub as a message.
func TestHubHandler_AgentEndSendsNoMessage(t *testing.T) {
	fh := &fakeHub{t: t}
	handler := fh.start()

	payload := map[string]interface{}{
		"hook_event_name":        "Stop",
		"session_id":             "s1",
		"last_assistant_message": "Here is the final answer.",
	}
	event, err := dialects.NewClaudeDialect().Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name != hooks.EventAgentEnd {
		t.Fatalf("expected %s, got %s", hooks.EventAgentEnd, event.Name)
	}
	if err := handler.Handle(event); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.outboundCalls != 0 || fh.selfCalls != 0 {
		t.Errorf("expected no outbound message or self lookup, got outbound=%d self=%d",
			fh.outboundCalls, fh.selfCalls)
	}
	if fh.statusCalls != 1 {
		t.Errorf("expected 1 status update, got %d", fh.statusCalls)
	}
}

// TestTruncateMessage tests the truncation helper function.
func TestTruncateMessage(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is a longer message", 10, "this is..."},
		{"", 10, ""},
	}

	for _, tt := range tests {
		result := truncateMessage(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncateMessage(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}
