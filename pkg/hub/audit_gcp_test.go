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
	"context"
	"testing"
)

// mockAuditLogger captures audit events for testing.
type mockAuditLogger struct {
	brokerEvents []*BrokerAuthEvent
	gcpEvents    []*GCPTokenEvent
}

func (m *mockAuditLogger) LogBrokerAuthEvent(_ context.Context, event *BrokerAuthEvent) error {
	m.brokerEvents = append(m.brokerEvents, event)
	return nil
}

func (m *mockAuditLogger) LogGCPTokenEvent(_ context.Context, event *GCPTokenEvent) error {
	m.gcpEvents = append(m.gcpEvents, event)
	return nil
}

func (m *mockAuditLogger) LogInviteAuditEvent(_ context.Context, _ *InviteAuditEvent) error {
	return nil
}

func TestLogGCPTokenGeneration_Success(t *testing.T) {
	mock := &mockAuditLogger{}
	ctx := context.Background()

	LogGCPTokenGeneration(ctx, mock, GCPTokenEventAccessToken,
		"agent-123", "project-456", "sa@project.iam.gserviceaccount.com", "sa-789", true, "")

	if len(mock.gcpEvents) != 1 {
		t.Fatalf("expected 1 GCP event, got %d", len(mock.gcpEvents))
	}

	event := mock.gcpEvents[0]
	if event.EventType != GCPTokenEventAccessToken {
		t.Errorf("expected event type %q, got %q", GCPTokenEventAccessToken, event.EventType)
	}
	if event.AgentID != "agent-123" {
		t.Errorf("expected agent ID %q, got %q", "agent-123", event.AgentID)
	}
	if event.ProjectID != "project-456" {
		t.Errorf("expected project ID %q, got %q", "project-456", event.ProjectID)
	}
	if event.ServiceAccountEmail != "sa@project.iam.gserviceaccount.com" {
		t.Errorf("expected SA email %q, got %q", "sa@project.iam.gserviceaccount.com", event.ServiceAccountEmail)
	}
	if event.ServiceAccountID != "sa-789" {
		t.Errorf("expected SA ID %q, got %q", "sa-789", event.ServiceAccountID)
	}
	if !event.Success {
		t.Error("expected success=true")
	}
	if event.FailReason != "" {
		t.Errorf("expected empty fail reason, got %q", event.FailReason)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestLogGCPTokenGeneration_Failure(t *testing.T) {
	mock := &mockAuditLogger{}
	ctx := context.Background()

	LogGCPTokenGeneration(ctx, mock, GCPTokenEventIdentityToken,
		"agent-123", "project-456", "sa@project.iam.gserviceaccount.com", "sa-789", false, "impersonation denied")

	if len(mock.gcpEvents) != 1 {
		t.Fatalf("expected 1 GCP event, got %d", len(mock.gcpEvents))
	}

	event := mock.gcpEvents[0]
	if event.EventType != GCPTokenEventIdentityToken {
		t.Errorf("expected event type %q, got %q", GCPTokenEventIdentityToken, event.EventType)
	}
	if event.Success {
		t.Error("expected success=false")
	}
	if event.FailReason != "impersonation denied" {
		t.Errorf("expected fail reason %q, got %q", "impersonation denied", event.FailReason)
	}
}

func TestLogGCPTokenGeneration_NilLogger(t *testing.T) {
	// Should not panic with nil logger
	LogGCPTokenGeneration(context.Background(), nil, GCPTokenEventAccessToken,
		"agent-123", "project-456", "sa@project.iam.gserviceaccount.com", "sa-789", true, "")
}

func TestLogAuditLogger_LogGCPTokenEvent(t *testing.T) {
	logger := NewLogAuditLogger("[Test]", false)

	// Should not error for success event
	err := logger.LogGCPTokenEvent(context.Background(), &GCPTokenEvent{
		EventType:           GCPTokenEventAccessToken,
		AgentID:             "agent-1",
		ProjectID:             "project-1",
		ServiceAccountEmail: "sa@proj.iam.gserviceaccount.com",
		Success:             true,
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should not error for failure event
	err = logger.LogGCPTokenEvent(context.Background(), &GCPTokenEvent{
		EventType:           GCPTokenEventIdentityToken,
		AgentID:             "agent-1",
		ProjectID:             "project-1",
		ServiceAccountEmail: "sa@proj.iam.gserviceaccount.com",
		Success:             false,
		FailReason:          "permission denied",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
