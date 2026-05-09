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
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildLogFilter(t *testing.T) {
	tests := []struct {
		name     string
		opts     LogQueryOptions
		expected string
	}{
		{
			name:     "empty options",
			opts:     LogQueryOptions{},
			expected: "",
		},
		{
			name: "agent ID only",
			opts: LogQueryOptions{
				AgentID: "agent-123",
			},
			expected: `labels.agent_id = "agent-123"`,
		},
		{
			name: "agent ID with severity",
			opts: LogQueryOptions{
				AgentID:  "agent-123",
				Severity: "ERROR",
			},
			expected: `labels.agent_id = "agent-123" AND severity >= ERROR`,
		},
		{
			name: "all filters",
			opts: LogQueryOptions{
				AgentID:  "agent-123",
				Since:    time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC),
				Until:    time.Date(2026, 3, 7, 11, 0, 0, 0, time.UTC),
				Severity: "INFO",
			},
			expected: `labels.agent_id = "agent-123" AND timestamp >= "2026-03-07T10:00:00Z" AND timestamp < "2026-03-07T11:00:00Z" AND severity >= INFO`,
		},
		{
			name: "severity case normalization",
			opts: LogQueryOptions{
				Severity: "warning",
			},
			expected: `severity >= WARNING`,
		},
		{
			name: "broker ID filter",
			opts: LogQueryOptions{
				AgentID:  "agent-123",
				BrokerID: "broker-west-1",
			},
			expected: `labels.agent_id = "agent-123" AND labels.broker_id = "broker-west-1"`,
		},
		{
			name: "all filters with broker",
			opts: LogQueryOptions{
				AgentID:  "agent-123",
				BrokerID: "broker-east-1",
				Since:    time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC),
				Severity: "ERROR",
			},
			expected: `labels.agent_id = "agent-123" AND labels.broker_id = "broker-east-1" AND timestamp >= "2026-03-07T10:00:00Z" AND severity >= ERROR`,
		},
		{
			name: "project ID filter",
			opts: LogQueryOptions{
				AgentID: "agent-123",
				ProjectID: "project-abc",
			},
			expected: `labels.agent_id = "agent-123" AND labels.project_id = "project-abc"`,
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

func TestBuildLogFilter_LogID(t *testing.T) {
	tests := []struct {
		name      string
		opts      LogQueryOptions
		projectID string
		expected  string
	}{
		{
			name: "logID with project ID",
			opts: LogQueryOptions{
				AgentID: "agent-123",
				LogID:   "scion-messages",
			},
			projectID: "my-project",
			expected:  `logName = "projects/my-project/logs/scion-messages" AND (labels.recipient_id = "agent-123" OR labels.sender_id = "agent-123")`,
		},
		{
			name: "logID without project ID",
			opts: LogQueryOptions{
				AgentID: "agent-123",
				LogID:   "scion-messages",
			},
			projectID: "",
			expected:  `(labels.recipient_id = "agent-123" OR labels.sender_id = "agent-123")`,
		},
		{
			name: "no logID with project ID excludes request log",
			opts: LogQueryOptions{
				AgentID: "agent-123",
			},
			projectID: "my-project",
			expected:  `logName != "projects/my-project/logs/scion_request_log" AND labels.agent_id = "agent-123"`,
		},
		{
			name: "message log uses ID-based sender and recipient filter",
			opts: LogQueryOptions{
				AgentID: "agent-123",
				LogID:   "scion-messages",
			},
			projectID: "my-project",
			expected:  `logName = "projects/my-project/logs/scion-messages" AND (labels.recipient_id = "agent-123" OR labels.sender_id = "agent-123")`,
		},
		{
			name: "message log with project_id filter",
			opts: LogQueryOptions{
				AgentID: "agent-123",
				ProjectID: "project-abc",
				LogID:   "scion-messages",
			},
			projectID: "my-project",
			expected:  `logName = "projects/my-project/logs/scion-messages" AND (labels.recipient_id = "agent-123" OR labels.sender_id = "agent-123") AND labels.project_id = "project-abc"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildLogFilter(tt.opts, tt.projectID)
			if result != tt.expected {
				t.Errorf("BuildLogFilter() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestConvertLogEntry(t *testing.T) {
	t.Run("string payload", func(t *testing.T) {
		ts := time.Date(2026, 3, 7, 10, 15, 32, 0, time.UTC)
		entry := &gcplog.Entry{
			Timestamp: ts,
			Severity:  gcplog.Info,
			Payload:   "Agent started processing task",
			Labels: map[string]string{
				"agent_id": "abc123",
				"project_id": "my-project",
			},
			InsertID: "insert-1",
		}

		result := ConvertLogEntry(entry)

		if result.Timestamp != ts {
			t.Errorf("Timestamp = %v, want %v", result.Timestamp, ts)
		}
		if result.Severity != "Info" {
			t.Errorf("Severity = %q, want %q", result.Severity, "Info")
		}
		if result.Message != "Agent started processing task" {
			t.Errorf("Message = %q, want %q", result.Message, "Agent started processing task")
		}
		if result.InsertID != "insert-1" {
			t.Errorf("InsertID = %q, want %q", result.InsertID, "insert-1")
		}
		if result.Labels["agent_id"] != "abc123" {
			t.Errorf("Labels[agent_id] = %q, want %q", result.Labels["agent_id"], "abc123")
		}
	})

	t.Run("map payload with message", func(t *testing.T) {
		entry := &gcplog.Entry{
			Timestamp: time.Now(),
			Severity:  gcplog.Error,
			Payload: map[string]interface{}{
				"message":   "Failed to route message",
				"subsystem": "hub.dispatch",
				"error":     "connection refused",
			},
			InsertID: "insert-2",
		}

		result := ConvertLogEntry(entry)

		if result.Message != "Failed to route message" {
			t.Errorf("Message = %q, want %q", result.Message, "Failed to route message")
		}
		if result.JSONPayload["subsystem"] != "hub.dispatch" {
			t.Errorf("JSONPayload[subsystem] = %v, want %q", result.JSONPayload["subsystem"], "hub.dispatch")
		}
	})

	t.Run("source location", func(t *testing.T) {
		entry := &gcplog.Entry{
			Timestamp: time.Now(),
			Severity:  gcplog.Warning,
			Payload:   "test",
			InsertID:  "insert-3",
			SourceLocation: &logpb.LogEntrySourceLocation{
				File:     "pkg/hub/dispatch.go",
				Line:     342,
				Function: "github.com/GoogleCloudPlatform/scion/pkg/hub.(*Server).dispatch",
			},
		}

		result := ConvertLogEntry(entry)

		if result.SourceLocation == nil {
			t.Fatal("SourceLocation is nil")
		}
		if result.SourceLocation.File != "pkg/hub/dispatch.go" {
			t.Errorf("File = %q, want %q", result.SourceLocation.File, "pkg/hub/dispatch.go")
		}
		if result.SourceLocation.Line != "342" {
			t.Errorf("Line = %q, want %q", result.SourceLocation.Line, "342")
		}
	})

	t.Run("resource info", func(t *testing.T) {
		entry := &gcplog.Entry{
			Timestamp: time.Now(),
			Severity:  gcplog.Info,
			Payload:   "test",
			InsertID:  "insert-4",
			Resource: &mrpb.MonitoredResource{
				Type: "gce_instance",
				Labels: map[string]string{
					"instance_id": "12345",
					"zone":        "us-central1-a",
				},
			},
		}

		result := ConvertLogEntry(entry)

		if result.Resource == nil {
			t.Fatal("Resource is nil")
		}
		if result.Resource["type"] != "gce_instance" {
			t.Errorf("Resource.type = %v, want %q", result.Resource["type"], "gce_instance")
		}
		labels, ok := result.Resource["labels"].(map[string]interface{})
		if !ok {
			t.Fatal("Resource.labels is not a map")
		}
		if labels["instance_id"] != "12345" {
			t.Errorf("Resource.labels.instance_id = %v, want %q", labels["instance_id"], "12345")
		}
	})
}

func TestLogQueryOptionsTailCapping(t *testing.T) {
	// Test that tail defaults and caps are applied correctly
	// This is tested indirectly through the Query method, but we can
	// verify the BuildLogFilter doesn't add tail to the filter.
	opts := LogQueryOptions{
		AgentID: "test",
		Tail:    5000, // Over the 1000 cap
	}
	filter := BuildLogFilter(opts)
	// Tail should not appear in the filter string
	if filter != `labels.agent_id = "test"` {
		t.Errorf("BuildLogFilter() = %q, tail should not be in filter", filter)
	}
}

func TestConvertProtoLogEntry_TextPayload(t *testing.T) {
	ts := time.Date(2026, 3, 7, 10, 15, 32, 0, time.UTC)
	entry := &logpb.LogEntry{
		Timestamp: timestamppb.New(ts),
		Severity:  ltype.LogSeverity_INFO,
		Payload:   &logpb.LogEntry_TextPayload{TextPayload: "Agent started"},
		Labels: map[string]string{
			"agent_id":  "abc123",
			"broker_id": "broker-west-1",
		},
		InsertId: "insert-proto-1",
	}

	result := ConvertProtoLogEntry(entry)

	if result.Timestamp != ts {
		t.Errorf("Timestamp = %v, want %v", result.Timestamp, ts)
	}
	if result.Message != "Agent started" {
		t.Errorf("Message = %q, want %q", result.Message, "Agent started")
	}
	if result.Labels["broker_id"] != "broker-west-1" {
		t.Errorf("Labels[broker_id] = %q, want %q", result.Labels["broker_id"], "broker-west-1")
	}
	if result.InsertID != "insert-proto-1" {
		t.Errorf("InsertID = %q, want %q", result.InsertID, "insert-proto-1")
	}
}

func TestConvertProtoLogEntry_JSONPayload(t *testing.T) {
	fields := map[string]*structpb.Value{
		"message":   structpb.NewStringValue("Failed to route"),
		"subsystem": structpb.NewStringValue("hub.dispatch"),
	}
	entry := &logpb.LogEntry{
		Timestamp: timestamppb.New(time.Now()),
		Severity:  ltype.LogSeverity_ERROR,
		Payload: &logpb.LogEntry_JsonPayload{
			JsonPayload: &structpb.Struct{Fields: fields},
		},
		InsertId: "insert-proto-2",
		SourceLocation: &logpb.LogEntrySourceLocation{
			File:     "pkg/hub/dispatch.go",
			Line:     342,
			Function: "dispatch",
		},
	}

	result := ConvertProtoLogEntry(entry)

	if result.Message != "Failed to route" {
		t.Errorf("Message = %q, want %q", result.Message, "Failed to route")
	}
	if result.JSONPayload["subsystem"] != "hub.dispatch" {
		t.Errorf("JSONPayload[subsystem] = %v, want %q", result.JSONPayload["subsystem"], "hub.dispatch")
	}
	if result.SourceLocation == nil {
		t.Fatal("SourceLocation is nil")
	}
	if result.SourceLocation.File != "pkg/hub/dispatch.go" {
		t.Errorf("File = %q, want %q", result.SourceLocation.File, "pkg/hub/dispatch.go")
	}
	if result.SourceLocation.Line != "342" {
		t.Errorf("Line = %q, want %q", result.SourceLocation.Line, "342")
	}
}

func TestConvertProtoLogEntry_Resource(t *testing.T) {
	entry := &logpb.LogEntry{
		Timestamp: timestamppb.New(time.Now()),
		Severity:  ltype.LogSeverity_INFO,
		Payload:   &logpb.LogEntry_TextPayload{TextPayload: "test"},
		InsertId:  "insert-proto-3",
		Resource: &mrpb.MonitoredResource{
			Type: "gce_instance",
			Labels: map[string]string{
				"instance_id": "12345",
			},
		},
	}

	result := ConvertProtoLogEntry(entry)

	if result.Resource == nil {
		t.Fatal("Resource is nil")
	}
	if result.Resource["type"] != "gce_instance" {
		t.Errorf("Resource.type = %v, want %q", result.Resource["type"], "gce_instance")
	}
}
