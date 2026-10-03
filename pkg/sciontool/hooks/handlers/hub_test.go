/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
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
type fakeHub struct {
	t *testing.T

	createdBy string
	ancestry  []string
	selfFail  bool

	mu             sync.Mutex
	outbound       []map[string]interface{} // accepted outbound-message payloads
	outboundCalls  int                      // all outbound-message requests, including rejected ones
	rejected400    int
	statusCalls    int
	selfCalls      int
	outboundScript []fakeResponse
}

type fakeResponse struct {
	status     int
	retryAfter string
	body       string
}

func newFakeHub(t *testing.T, createdBy string, ancestry ...string) *fakeHub {
	return &fakeHub{t: t, createdBy: createdBy, ancestry: ancestry}
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/test-agent-id":
		f.selfCalls++
		if f.selfFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "test-agent-id", "createdBy": f.createdBy, "ancestry": f.ancestry,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent-id/outbound-message":
		f.outboundCalls++
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(f.outboundScript) > 0 {
			next := f.outboundScript[0]
			f.outboundScript = f.outboundScript[1:]
			if next.retryAfter != "" {
				w.Header().Set("Retry-After", next.retryAfter)
			}
			w.WriteHeader(next.status)
			_, _ = w.Write([]byte(next.body))
			return
		}
		recipient, _ := payload["recipient"].(string)
		recipientID, _ := payload["recipient_id"].(string)
		convRef, _ := payload["conversation_ref"].(string)
		if recipient == "" && recipientID == "" && convRef == "" {
			f.rejected400++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"recipient is required"}}`))
			return
		}
		f.outbound = append(f.outbound, payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent-id/status":
		f.statusCalls++
		w.WriteHeader(http.StatusOK)
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

func (f *fakeHub) counts() (selfCalls, outboundCalls, accepted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfCalls, f.outboundCalls, len(f.outbound)
}

// lastOutbound returns the single accepted outbound payload, failing the
// test unless exactly one was accepted and none was rejected.
func (f *fakeHub) lastOutbound() map[string]interface{} {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejected400 != 0 {
		f.t.Fatalf("hub rejected %d outbound message(s) with 400 (no addressee)", f.rejected400)
	}
	if len(f.outbound) != 1 {
		f.t.Fatalf("Expected exactly 1 accepted outbound message, got %d", len(f.outbound))
	}
	return f.outbound[0]
}

const testCreatorUserID = "11111111-1111-1111-1111-111111111111"

func agentEndWithText(text string) *hooks.Event {
	return &hooks.Event{Name: hooks.EventAgentEnd, Data: hooks.EventData{AssistantText: text}}
}

// TestHubHandler_AssistantTextForwarding tests that agent-end events with
// AssistantText forward the text to the outbound-message endpoint addressed
// to the agent's creator, and that very large texts are truncated.
func TestHubHandler_AssistantTextForwarding(t *testing.T) {
	t.Run("forwards assistant text to the creator", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello from the agent")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}

		payload := fh.lastOutbound()
		if payload["msg"] != "Hello from the agent" {
			t.Errorf("Expected outbound msg %q, got %v", "Hello from the agent", payload["msg"])
		}
		if payload["type"] != "assistant-reply" {
			t.Errorf("Expected outbound type %q, got %v", "assistant-reply", payload["type"])
		}
		if payload["recipient_id"] != testCreatorUserID {
			t.Errorf("Expected recipient_id %q (the creator), got %v", testCreatorUserID, payload["recipient_id"])
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.statusCalls != 1 {
			t.Errorf("Expected 1 status call (working), got %d", fh.statusCalls)
		}
	})

	t.Run("skips the mirror when the creator is an agent", func(t *testing.T) {
		// Agent-created agent: CreatedBy is the parent agent, ancestry is
		// [root user, parent agent].
		fh := newFakeHub(t, "parent-agent-id", testCreatorUserID, "parent-agent-id")
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 0 {
			t.Errorf("Expected no outbound-message request, got %d", fh.outboundCalls)
		}
		if fh.statusCalls != 1 {
			t.Errorf("Expected the status update to still be sent, got %d calls", fh.statusCalls)
		}
	})

	t.Run("skips the mirror when the creator is unknown", func(t *testing.T) {
		fh := newFakeHub(t, "")
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 0 {
			t.Errorf("Expected no outbound-message request, got %d", fh.outboundCalls)
		}
	})

	t.Run("skips the mirror when the agent lookup fails", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.selfFail = true
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 0 {
			t.Errorf("Expected no outbound-message request, got %d", fh.outboundCalls)
		}
		if fh.statusCalls != 1 {
			t.Errorf("Expected the status update to still be sent, got %d calls", fh.statusCalls)
		}
	})

	t.Run("truncates assistant text to the hub message limit", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()

		bigText := strings.Repeat("A", messages.MaxMessageLength*4)
		if err := handler.Handle(agentEndWithText(bigText)); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}

		outboundMsg, _ := fh.lastOutbound()["msg"].(string)
		// Runes, not bytes: this is the unit the hub rejects on.
		if got := utf8.RuneCountInString(outboundMsg); got > messages.MaxMessageLength {
			t.Errorf("Expected outbound msg to be at most %d runes, got %d", messages.MaxMessageLength, got)
		}
		if !strings.Contains(outboundMsg, "[truncated,") {
			t.Error("Expected the truncated message to carry a marker saying how much went")
		}
	})
}

// TestHubHandler_AssistantReplyRateLimitRetry pins ptone/scion#1065: a 429
// on the assistant-reply mirror is retried once when its Retry-After fits in
// the hook's budget, and not at all otherwise.
func TestHubHandler_AssistantReplyRateLimitRetry(t *testing.T) {
	t.Run("short Retry-After: one retry, then success", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.outboundScript = []fakeResponse{{status: http.StatusTooManyRequests, retryAfter: "2"}}
		handler := fh.start()
		var waits []time.Duration
		handler.wait = func(_ context.Context, d time.Duration) error {
			waits = append(waits, d) // observe, don't sleep
			return nil
		}

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		if len(waits) != 1 || waits[0] != 2*time.Second {
			t.Errorf("Expected one wait of Retry-After (2s), got %v", waits)
		}
		payload := fh.lastOutbound()
		if payload["msg"] != "Hello" {
			t.Errorf("Expected the retried message to be delivered, got %v", payload["msg"])
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 2 {
			t.Errorf("Expected exactly 2 outbound requests (one retry), got %d", fh.outboundCalls)
		}
		if fh.statusCalls != 1 {
			t.Errorf("Expected the status update to still be sent, got %d calls", fh.statusCalls)
		}
	})

	t.Run("retry budget is the mirror's, not the whole hook's", func(t *testing.T) {
		// 3s + retryReserve fits the 5s hook budget but not mirrorBudget.
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.outboundScript = []fakeResponse{{status: http.StatusTooManyRequests, retryAfter: "3"}}
		handler := fh.start()
		handler.wait = func(context.Context, time.Duration) error {
			t.Error("Expected no wait: Retry-After exceeds the mirror budget")
			return nil
		}

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		_, outbound, _ := fh.counts()
		if outbound != 1 {
			t.Errorf("Expected exactly 1 outbound request, got %d", outbound)
		}
	})

	t.Run("long Retry-After: no retry", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.outboundScript = []fakeResponse{{status: http.StatusTooManyRequests, retryAfter: "30"}}
		handler := fh.start()

		start := time.Now()
		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Expected an immediate give-up, took %s", elapsed)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 1 {
			t.Errorf("Expected exactly 1 outbound request (no retry), got %d", fh.outboundCalls)
		}
	})

	t.Run("429 without Retry-After: no retry", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.outboundScript = []fakeResponse{{status: http.StatusTooManyRequests}}
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 1 {
			t.Errorf("Expected exactly 1 outbound request, got %d", fh.outboundCalls)
		}
	})

	t.Run("retry is attempted once only", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.outboundScript = []fakeResponse{
			{status: http.StatusTooManyRequests, retryAfter: "0"},
			{status: http.StatusTooManyRequests, retryAfter: "0"},
		}
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		fh.mu.Lock()
		defer fh.mu.Unlock()
		if fh.outboundCalls != 2 {
			t.Errorf("Expected exactly 2 outbound requests, got %d", fh.outboundCalls)
		}
		if len(fh.outbound) != 0 {
			t.Errorf("Expected nothing delivered, got %d", len(fh.outbound))
		}
	})
}

func TestTruncateAssistantText(t *testing.T) {
	const marker = "[truncated,"

	t.Run("text at or under the limit is untouched", func(t *testing.T) {
		for _, n := range []int{0, 1, messages.MaxMessageLength - 1, messages.MaxMessageLength} {
			// Multi-byte: with ASCII, bytes and runes agree and a byte-counting
			// implementation passes unnoticed.
			in := strings.Repeat("\u3042", n)
			if got := truncateAssistantText(in); got != in {
				t.Errorf("%d runes was modified", n)
			}
		}
	})

	// Non-uniform input: a homogeneous repeat cannot tell head-truncation from
	// tail-truncation.
	t.Run("keeps the START of the reply", func(t *testing.T) {
		in := "OPENING-SENTINEL" + strings.Repeat("x", messages.MaxMessageLength*2) + "CLOSING-SENTINEL"
		got := truncateAssistantText(in)

		if !strings.HasPrefix(got, "OPENING-SENTINEL") {
			t.Error("the opening of the reply was discarded")
		}
		if strings.Contains(got, "CLOSING-SENTINEL") {
			t.Error("kept the end of the reply instead of the start")
		}
		if body := got[:strings.LastIndex(got, "\n"+marker)]; !strings.HasPrefix(in, body) {
			t.Error("the kept text is not a prefix of the input")
		}
	})

	t.Run("result always fits the hub limit", func(t *testing.T) {
		for _, in := range []string{
			strings.Repeat("a", messages.MaxMessageLength+1),
			strings.Repeat("\u3042", messages.MaxMessageLength+1),
			strings.Repeat("a", messages.MaxMessageLength*10),
			"\xff\xfe" + strings.Repeat("b", messages.MaxMessageLength+1),
		} {
			got := truncateAssistantText(in)
			if n := utf8.RuneCountInString(got); n > messages.MaxMessageLength {
				t.Errorf("%d runes exceeds the hub limit of %d", n, messages.MaxMessageLength)
			}
			if !strings.Contains(got, marker) {
				t.Error("a truncated reply carries no marker")
			}
		}
	})

	t.Run("the marker reports how much went", func(t *testing.T) {
		over := 500
		got := truncateAssistantText(strings.Repeat("a", messages.MaxMessageLength+over))
		var dropped int
		if _, err := fmt.Sscanf(got[strings.LastIndex(got, marker):], "[truncated, %d characters omitted]", &dropped); err != nil {
			t.Fatalf("marker is not parseable: %v", err)
		}
		if dropped < over {
			t.Errorf("marker says %d dropped, but at least %d were", dropped, over)
		}
	})
}

// TestHubHandler_AssistantTextMetadataTagging tests that automatic
// assistant-reply messages include content classification metadata.
func TestHubHandler_AssistantTextMetadataTagging(t *testing.T) {
	t.Run("tags outbound message with metadata", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Agent response")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}

		outboundPayload := fh.lastOutbound()
		// visibility field has been removed from outbound messages
		if _, hasVis := outboundPayload["visibility"]; hasVis {
			t.Errorf("Expected visibility field to be absent, got %v", outboundPayload["visibility"])
		}
		metadata, ok := outboundPayload["metadata"].(map[string]interface{})
		if !ok {
			t.Fatal("Expected metadata to be present")
		}
		if metadata["source"] != "hook" {
			t.Errorf("Expected metadata source 'hook', got %v", metadata["source"])
		}
	})

	t.Run("sets has_thinking metadata when thinking content was filtered", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()

		err := handler.Handle(&hooks.Event{
			Name: hooks.EventAgentEnd,
			Data: hooks.EventData{
				AssistantText: "Filtered response",
				AssistantContent: &hooks.AssistantContent{
					Blocks: []hooks.ContentBlock{
						{Type: hooks.ContentBlockThinking, Text: "I need to think..."},
						{Type: hooks.ContentBlockText, Text: "Filtered response"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}

		metadata, ok := fh.lastOutbound()["metadata"].(map[string]interface{})
		if !ok {
			t.Fatal("Expected metadata to be present")
		}
		if metadata["has_thinking"] != "true" {
			t.Errorf("Expected has_thinking 'true', got %v", metadata["has_thinking"])
		}
	})
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

// TestHubHandler_AssistantReplyAddresseeCache pins that the resolved
// addressee (or the decision to skip) is cached in the agent home after the
// first successful lookup, while lookup failures are not cached.
func TestHubHandler_AssistantReplyAddresseeCache(t *testing.T) {
	cachePath := func() string { return filepath.Join(os.Getenv("HOME"), addresseeCacheFile) }

	t.Run("resolved addressee is cached and reused", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()

		for i := 0; i < 3; i++ {
			if err := handler.Handle(agentEndWithText("Hello")); err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
		}
		self, outbound, accepted := fh.counts()
		if self != 1 {
			t.Errorf("Expected 1 agent lookup across 3 Stop events, got %d", self)
		}
		if outbound != 3 || accepted != 3 {
			t.Errorf("Expected 3 delivered replies, got %d requests / %d accepted", outbound, accepted)
		}
		c, ok := readAddresseeCache(cachePath(), "test-agent-id")
		if !ok || c.RecipientID != testCreatorUserID {
			t.Errorf("Expected cache to hold %q, got %+v ok=%v", testCreatorUserID, c, ok)
		}
		info, err := os.Stat(cachePath())
		if err != nil {
			t.Fatalf("stat cache: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("Expected cache mode 0600, got %o", perm)
		}
	})

	t.Run("skip decision is cached", func(t *testing.T) {
		fh := newFakeHub(t, "parent-agent-id", testCreatorUserID, "parent-agent-id")
		handler := fh.start()

		for i := 0; i < 2; i++ {
			if err := handler.Handle(agentEndWithText("Hello")); err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
		}
		self, outbound, _ := fh.counts()
		if self != 1 {
			t.Errorf("Expected 1 agent lookup, got %d", self)
		}
		if outbound != 0 {
			t.Errorf("Expected no outbound requests, got %d", outbound)
		}
		c, ok := readAddresseeCache(cachePath(), "test-agent-id")
		if !ok || c.RecipientID != "" {
			t.Errorf("Expected a cached skip, got %+v ok=%v", c, ok)
		}
	})

	t.Run("lookup failure is not cached", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		fh.selfFail = true
		handler := fh.start()

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		if _, err := os.Stat(cachePath()); !os.IsNotExist(err) {
			t.Fatalf("Expected no cache file after a failed lookup, stat err=%v", err)
		}

		fh.mu.Lock()
		fh.selfFail = false
		fh.mu.Unlock()
		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		self, _, accepted := fh.counts()
		if self != 2 {
			t.Errorf("Expected the lookup to be retried on the next Stop (2 lookups), got %d", self)
		}
		if accepted != 1 {
			t.Errorf("Expected 1 delivered reply after recovery, got %d", accepted)
		}
	})

	t.Run("pre-existing cache is used without a lookup", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()
		const cachedID = "22222222-2222-2222-2222-222222222222"
		writeAddresseeCache(cachePath(), addresseeCache{AgentID: "test-agent-id", RecipientID: cachedID})

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		if self, _, _ := fh.counts(); self != 0 {
			t.Errorf("Expected no agent lookup, got %d", self)
		}
		if got := fh.lastOutbound()["recipient_id"]; got != cachedID {
			t.Errorf("Expected recipient_id from cache %q, got %v", cachedID, got)
		}
	})

	for name, content := range map[string]string{
		"corrupt cache is ignored and rewritten": "{not json",
		"empty cache is ignored and rewritten":   "",
		"other agent's cache is ignored":         `{"agentId":"someone-else","recipientId":"33333333-3333-3333-3333-333333333333"}`,
	} {
		t.Run(name, func(t *testing.T) {
			fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
			handler := fh.start()
			if err := os.WriteFile(cachePath(), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}

			if err := handler.Handle(agentEndWithText("Hello")); err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
			if self, _, _ := fh.counts(); self != 1 {
				t.Errorf("Expected a fresh lookup, got %d", self)
			}
			if got := fh.lastOutbound()["recipient_id"]; got != testCreatorUserID {
				t.Errorf("Expected recipient_id %q, got %v", testCreatorUserID, got)
			}
			c, ok := readAddresseeCache(cachePath(), "test-agent-id")
			if !ok || c.RecipientID != testCreatorUserID {
				t.Errorf("Expected cache rewritten with %q, got %+v ok=%v", testCreatorUserID, c, ok)
			}
		})
	}

	t.Run("symlinked cache is not followed", func(t *testing.T) {
		fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
		handler := fh.start()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, []byte(`{"agentId":"test-agent-id","recipientId":"44444444-4444-4444-4444-444444444444"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, cachePath()); err != nil {
			t.Fatal(err)
		}

		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
		if got := fh.lastOutbound()["recipient_id"]; got != testCreatorUserID {
			t.Errorf("Expected the symlinked cache to be ignored, got recipient_id %v", got)
		}
		data, _ := os.ReadFile(target)
		if !strings.Contains(string(data), "4444") {
			t.Error("Expected the symlink target to be left untouched")
		}
	})
}

func TestRetryAfterWithinBudget(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rateLimited := func(d time.Duration) error {
		return &hub.HTTPStatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: d, HasRetryAfter: true}
	}
	withDeadline := func(remaining time.Duration) context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(remaining))
		t.Cleanup(cancel)
		return ctx
	}
	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		wantWait time.Duration
		wantOK   bool
	}{
		{"no deadline", context.Background(), rateLimited(10 * time.Second), 10 * time.Second, true},
		{"just under", withDeadline(2*time.Second + time.Millisecond), rateLimited(time.Second), time.Second, true},
		{"exactly fits", withDeadline(2 * time.Second), rateLimited(time.Second), time.Second, true},
		{"just over", withDeadline(2*time.Second - time.Millisecond), rateLimited(time.Second), 0, false},
		{"zero wait, reserve still required", withDeadline(retryReserve - time.Millisecond), rateLimited(0), 0, false},
		{"503 with Retry-After", context.Background(), &hub.HTTPStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Second, HasRetryAfter: true}, 0, false},
		{"429 without Retry-After", context.Background(), &hub.HTTPStatusError{StatusCode: http.StatusTooManyRequests}, 0, false},
		{"not an HTTPStatusError", context.Background(), errors.New("connection refused"), 0, false},
		{"wrapped 429", context.Background(), fmt.Errorf("send: %w", rateLimited(time.Second)), time.Second, true},
		{"nil error", context.Background(), nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wait, ok := retryAfterWithinBudget(tt.ctx, tt.err, now)
			if ok != tt.wantOK || wait != tt.wantWait {
				t.Errorf("retryAfterWithinBudget = (%s, %v), want (%s, %v)", wait, ok, tt.wantWait, tt.wantOK)
			}
		})
	}
}

// A creator the hub no longer resolves (400 addr_unknown) disables the
// mirror for the agent: the skip is cached, so later Stops send nothing.
func TestHubHandler_AssistantReplyAddrUnknownCachesSkip(t *testing.T) {
	fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
	fh.outboundScript = []fakeResponse{{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":"addr_unknown","message":"recipient_id is not a valid addressee"}}`,
	}}
	handler := fh.start()

	for i := 0; i < 2; i++ {
		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
	}
	self, outbound, _ := fh.counts()
	if outbound != 1 {
		t.Errorf("Expected only the first Stop to send (1 outbound request), got %d", outbound)
	}
	if self != 1 {
		t.Errorf("Expected 1 agent lookup, got %d", self)
	}
	c, ok := readAddresseeCache(filepath.Join(os.Getenv("HOME"), addresseeCacheFile), "test-agent-id")
	if !ok || c.RecipientID != "" {
		t.Errorf("Expected a cached skip, got %+v ok=%v", c, ok)
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.statusCalls != 2 {
		t.Errorf("Expected both status updates to be sent, got %d", fh.statusCalls)
	}
}

// Any other 400 is not treated as a permanent addressee failure.
func TestHubHandler_AssistantReplyOther400DoesNotCacheSkip(t *testing.T) {
	fh := newFakeHub(t, testCreatorUserID, testCreatorUserID)
	fh.outboundScript = []fakeResponse{{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":"validation_error","message":"msg too long"}}`,
	}}
	handler := fh.start()

	for i := 0; i < 2; i++ {
		if err := handler.Handle(agentEndWithText("Hello")); err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
	}
	_, outbound, accepted := fh.counts()
	if outbound != 2 || accepted != 1 {
		t.Errorf("Expected the second Stop to send and be accepted, got %d requests / %d accepted", outbound, accepted)
	}
}
