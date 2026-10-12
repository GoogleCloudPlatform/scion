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

package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifySource(t *testing.T) {
	tests := []struct {
		name     string
		entry    CloudLogEntry
		expected string
	}{
		{
			name: "scion-messages log name",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-messages",
			},
			expected: "messages",
		},
		{
			name: "scion-agents log name",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-agents",
			},
			expected: "agent",
		},
		{
			name: "hub subsystem",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				JSONPayload: map[string]interface{}{
					"subsystem": "hub.dispatcher",
					"message":   "Dispatching agent",
				},
			},
			expected: "hub",
		},
		{
			name: "broker subsystem",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				JSONPayload: map[string]interface{}{
					"subsystem": "broker.lifecycle",
					"message":   "Container started",
				},
			},
			expected: "broker",
		},
		{
			name: "hub component label",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				Labels: map[string]string{
					"component": "scion-hub",
				},
			},
			expected: "hub",
		},
		{
			name: "broker component label",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				Labels: map[string]string{
					"component": "scion-broker",
				},
			},
			expected: "broker",
		},
		{
			name: "server fallback",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
			},
			expected: "server",
		},
		{
			name: "subsystem takes priority over component label",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				JSONPayload: map[string]interface{}{
					"subsystem": "hub.auth",
				},
				Labels: map[string]string{
					"component": "scion-broker",
				},
			},
			expected: "hub",
		},
		{
			name: "messages log name takes priority over subsystem",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-messages",
				JSONPayload: map[string]interface{}{
					"subsystem": "hub.messaging",
				},
			},
			expected: "messages",
		},
		{
			name: "agents log name takes priority over component",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-agents",
				Labels: map[string]string{
					"component": "scion-hub",
				},
			},
			expected: "agent",
		},
		{
			name:     "empty entry defaults to server",
			entry:    CloudLogEntry{},
			expected: "server",
		},
		{
			name: "nil labels with scion-server log",
			entry: CloudLogEntry{
				LogName: "projects/my-project/logs/scion-server",
				Labels:  nil,
			},
			expected: "server",
		},
		{
			name: "nil jsonPayload with scion-server log",
			entry: CloudLogEntry{
				LogName:     "projects/my-project/logs/scion-server",
				JSONPayload: nil,
			},
			expected: "server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifySource(tt.entry)
			if result != tt.expected {
				t.Errorf("classifySource() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestToDiagnosticEntry(t *testing.T) {
	entry := CloudLogEntry{
		LogName:  "projects/my-project/logs/scion-server",
		Severity: "INFO",
		Message:  "Test message",
		InsertID: "insert-1",
		JSONPayload: map[string]interface{}{
			"subsystem": "hub.dispatcher",
			"message":   "Test message",
		},
	}

	result := toDiagnosticEntry(entry)

	if result.Source != "hub" {
		t.Errorf("Source = %q, want %q", result.Source, "hub")
	}
	if result.Message != "Test message" {
		t.Errorf("Message = %q, want %q", result.Message, "Test message")
	}
	if result.InsertID != "insert-1" {
		t.Errorf("InsertID = %q, want %q", result.InsertID, "insert-1")
	}
}

func TestBuildLogFilter_Sources(t *testing.T) {
	tests := []struct {
		name      string
		opts      LogQueryOptions
		projectID string
		expected  []string // substrings that must appear in the filter
	}{
		{
			name: "hub source filter",
			opts: LogQueryOptions{
				Sources: []string{"hub"},
			},
			projectID: "my-project",
			expected: []string{
				`scion-server`,
				`hub\\.`,
			},
		},
		{
			name: "broker source filter",
			opts: LogQueryOptions{
				Sources: []string{"broker"},
			},
			projectID: "my-project",
			expected: []string{
				`scion-server`,
				`broker\\.`,
			},
		},
		{
			name: "agent source filter",
			opts: LogQueryOptions{
				Sources: []string{"agent"},
			},
			projectID: "my-project",
			expected: []string{
				`scion-agents`,
			},
		},
		{
			name: "messages source filter",
			opts: LogQueryOptions{
				Sources: []string{"messages"},
			},
			projectID: "my-project",
			expected: []string{
				`scion-messages`,
			},
		},
		{
			name: "multiple source filters",
			opts: LogQueryOptions{
				Sources: []string{"hub", "agent"},
			},
			projectID: "my-project",
			expected: []string{
				`hub\\.`,
				`scion-agents`,
				` OR `,
			},
		},
		{
			name: "sources without project ID are ignored",
			opts: LogQueryOptions{
				Sources: []string{"hub"},
			},
			projectID: "",
			expected:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildLogFilter(tt.opts, tt.projectID)
			for _, substr := range tt.expected {
				if !strings.Contains(result, substr) {
					t.Errorf("BuildLogFilter() = %q, expected to contain %q", result, substr)
				}
			}
		})
	}
}

func TestBuildLogFilter_Search(t *testing.T) {
	tests := []struct {
		name     string
		opts     LogQueryOptions
		expected string
	}{
		{
			name: "search filter",
			opts: LogQueryOptions{
				Search: "connection refused",
			},
			expected: `jsonPayload.message:"connection refused"`,
		},
		{
			name: "search with double quotes escaped",
			opts: LogQueryOptions{
				Search: `error "timeout"`,
			},
			expected: `jsonPayload.message:"error \"timeout\""`,
		},
		{
			name: "search combined with severity",
			opts: LogQueryOptions{
				Search:   "dispatch",
				Severity: "WARNING",
			},
			expected: `severity >= WARNING AND jsonPayload.message:"dispatch"`,
		},
		{
			name: "search with backslashes escaped before quotes",
			opts: LogQueryOptions{
				Search: `C:\Users\admin`,
			},
			expected: `jsonPayload.message:"C:\\Users\\admin"`,
		},
		{
			name: "search with backslash and quote combined",
			opts: LogQueryOptions{
				Search: `error\"timeout`,
			},
			expected: `jsonPayload.message:"error\\\"timeout"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildLogFilter(tt.opts)
			if result != tt.expected {
				t.Errorf("BuildLogFilter() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Handler-level tests for handleDiagnosticsLogs
// ---------------------------------------------------------------------------

// TestHandleDiagnosticsLogs_Unauthenticated and TestHandleDiagnosticsLogs_NonAdmin
// were removed: authorization is now enforced by the routeGuard via the
// hub.diagnostics.read permission (PR-A4). The handler no longer performs
// inline admin checks. Authorization is tested in TestRouteGuardOpsPermissions.

func TestHandleDiagnosticsLogs_NoLogQueryService(t *testing.T) {
	srv := &Server{} // logQueryService is nil
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/diagnostics/logs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleDiagnosticsLogs(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing 'error' object")
	}
	if code, _ := errObj["code"].(string); code != "not_implemented" {
		t.Errorf("error code = %q, want %q", code, "not_implemented")
	}
}

func TestHandleDiagnosticsLogs_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/diagnostics/logs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleDiagnosticsLogs(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Handler-level tests for handleDiagnosticsLogsStream
// ---------------------------------------------------------------------------

// TestHandleDiagnosticsLogsStream_Unauthenticated and TestHandleDiagnosticsLogsStream_NonAdmin
// were removed: authorization is now enforced by the routeGuard via the
// hub.diagnostics.read permission (PR-A4). The handler no longer performs
// inline admin checks. Authorization is tested in TestRouteGuardOpsPermissions.

func TestHandleDiagnosticsLogsStream_NoLogQueryService(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/diagnostics/logs/stream", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleDiagnosticsLogsStream(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing 'error' object")
	}
	if code, _ := errObj["code"].(string); code != "not_implemented" {
		t.Errorf("error code = %q, want %q", code, "not_implemented")
	}
}

func TestHandleDiagnosticsLogsStream_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/diagnostics/logs/stream", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleDiagnosticsLogsStream(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}
