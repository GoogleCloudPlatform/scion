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

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	gcplog "cloud.google.com/go/logging"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNewMessageLogger_DefaultConfig(t *testing.T) {
	cfg := MessageLoggerConfig{
		Component: "test-server",
		Level:     slog.LevelInfo,
	}

	logger, cleanup, err := NewMessageLogger(cfg)
	if err != nil {
		t.Fatalf("NewMessageLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewMessageLogger() returned nil logger")
	}
}

func TestNewMessageLogger_WritesSubsystemAttrs(t *testing.T) {
	// Create a logger that writes to a buffer for inspection
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	// Simulate what the message logger does: log with message attributes
	logger.Info("message dispatched",
		"agent_id", "agent-123",
		AttrSender, "user:alice",
		AttrRecipient, "agent:backend-dev",
		AttrMsgType, "instruction",
		"message_content", "implement auth",
		"urgent", false,
		"broadcasted", false,
		"plain", false,
	)

	// Verify JSON output contains expected fields
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Failed to parse log entry: %v", err)
	}

	expectedFields := map[string]any{
		"msg":             "message dispatched",
		"agent_id":        "agent-123",
		"sender":          "user:alice",
		"recipient":       "agent:backend-dev",
		"msg_type":        "instruction",
		"message_content": "implement auth",
	}

	for key, want := range expectedFields {
		got, ok := entry[key]
		if !ok {
			t.Errorf("log entry missing field %q", key)
			continue
		}
		if got != want {
			t.Errorf("log entry[%q] = %v, want %v", key, got, want)
		}
	}
}

func TestNewMessageLogger_CloudRunWithoutCloudClient_KeepsStdout(t *testing.T) {
	// On Cloud Run but WITHOUT a cloud client, the stdout handler should
	// still be present because the suppression condition requires both
	// K_SERVICE set AND CloudClient != nil.
	t.Setenv("K_SERVICE", "my-service")

	cfg := MessageLoggerConfig{
		Component:   "test-server",
		CloudClient: nil, // no cloud client → stdout must be present
		UseGCP:      true,
		Level:       slog.LevelInfo,
	}

	logger, cleanup, err := NewMessageLogger(cfg)
	if err != nil {
		t.Fatalf("NewMessageLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewMessageLogger() returned nil logger")
	}

	// With CloudClient=nil and K_SERVICE set, only the stdout handler is
	// created, so the logger handler should NOT be a multiHandler.
	if _, ok := logger.Handler().(*multiHandler); ok {
		t.Error("expected single stdout handler, not multiHandler — suppression should not engage without CloudClient")
	}
}

func TestNewMessageLogger_CloudRunWithCloudClient_SuppressesStdout(t *testing.T) {
	// On Cloud Run with a cloud client, the stdout handler should be
	// suppressed to avoid duplicate entries (Cloud Run's runtime already
	// forwards stdout to Cloud Logging).
	t.Setenv("K_SERVICE", "my-service")

	client, err := gcplog.NewClient(context.Background(), "projects/fake-project",
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		option.WithEndpoint("localhost:1"),
	)
	if err != nil {
		t.Fatalf("failed to create test gcplog.Client: %v", err)
	}
	defer func() { _ = client.Close() }()

	cfg := MessageLoggerConfig{
		Component:   "test-server",
		CloudClient: client,
		UseGCP:      true,
		Level:       slog.LevelInfo,
	}

	logger, cleanup, err := NewMessageLogger(cfg)
	if err != nil {
		t.Fatalf("NewMessageLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewMessageLogger() returned nil logger")
	}

	// With both K_SERVICE set and CloudClient non-nil, only the cloud
	// handler should be active — the stdout handler must be suppressed.
	// A single handler means no multiHandler wrapping.
	h := logger.Handler()
	if _, ok := h.(*multiHandler); ok {
		t.Error("expected single cloud handler, not multiHandler — stdout handler was not suppressed on Cloud Run")
	}
	// Verify we got the cloud handler, not a stdout handler.
	if _, ok := h.(*messageCloudHandler); !ok {
		t.Errorf("expected *messageCloudHandler, got %T", h)
	}
}

func TestNewMessageLogger_NonCloudRunKeepsStdout(t *testing.T) {
	// When not on Cloud Run, stdout handler should always be present.
	t.Setenv("K_SERVICE", "")

	cfg := MessageLoggerConfig{
		Component: "test-server",
		UseGCP:    true,
		Level:     slog.LevelInfo,
	}

	logger, cleanup, err := NewMessageLogger(cfg)
	if err != nil {
		t.Fatalf("NewMessageLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewMessageLogger() returned nil logger")
	}
}

func TestPromoteMessageAttrToLabels(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		promoted bool
	}{
		{"sender promoted", AttrSender, "user:alice", true},
		{"sender_id promoted", AttrSenderID, "user-uuid-123", true},
		{"recipient promoted", AttrRecipient, "agent:dev", true},
		{"recipient_id promoted", AttrRecipientID, "agent-uuid-456", true},
		{"msg_type promoted", AttrMsgType, "instruction", true},
		{"project_id promoted", AttrMsgProjectID, "project-abc", true},
		{"agent_id not promoted by message func", AttrAgentID, "abc123", false},
		{"arbitrary not promoted", "foo", "bar", false},
		{"empty value not promoted", AttrSender, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := make(map[string]string)
			attr := slog.String(tt.key, tt.value)
			promoteMessageAttrToLabels(labels, attr)

			_, found := labels[tt.key]
			if found != tt.promoted {
				t.Errorf("promoteMessageAttrToLabels(%q, %q) promoted = %v, want %v",
					tt.key, tt.value, found, tt.promoted)
			}
		})
	}
}

func TestMessageCloudHandler_WithAttrs_PreservesFields(t *testing.T) {
	hookCalled := false
	h := &messageCloudHandler{
		CloudHandler: CloudHandler{
			level:     slog.LevelInfo,
			component: "test-component",
			hubName:   "test-hub",
			hubID:     "hub-abc-123",
			hostname:  "node-1",
			projectID: "my-gcp-project",
			version:   "v1.2.3",
			logHook:   func(e gcplog.Entry) { hookCalled = true },
		},
	}

	derived := h.WithAttrs([]slog.Attr{slog.String("extra", "val")})
	mch, ok := derived.(*messageCloudHandler)
	if !ok {
		t.Fatal("WithAttrs should return a *messageCloudHandler")
	}

	if mch.hubID != "hub-abc-123" {
		t.Errorf("hubID = %q, want %q", mch.hubID, "hub-abc-123")
	}
	if mch.projectID != "my-gcp-project" {
		t.Errorf("projectID = %q, want %q", mch.projectID, "my-gcp-project")
	}
	if mch.version != "v1.2.3" {
		t.Errorf("version = %q, want %q", mch.version, "v1.2.3")
	}
	if mch.logHook == nil {
		t.Fatal("logHook should be preserved, got nil")
	}
	if mch.component != "test-component" {
		t.Errorf("component = %q, want %q", mch.component, "test-component")
	}
	if mch.hubName != "test-hub" {
		t.Errorf("hubName = %q, want %q", mch.hubName, "test-hub")
	}
	if mch.hostname != "node-1" {
		t.Errorf("hostname = %q, want %q", mch.hostname, "node-1")
	}
	if len(mch.attrs) != 1 {
		t.Errorf("expected 1 attr, got %d", len(mch.attrs))
	}

	// Verify the hook is callable (same function reference)
	mch.logHook(gcplog.Entry{})
	if !hookCalled {
		t.Error("logHook should be the same function as original")
	}

	// Original should be unchanged
	if len(h.attrs) != 0 {
		t.Error("original handler should not be modified")
	}
}

func TestMessageCloudHandler_WithGroup_PreservesFields(t *testing.T) {
	hookCalled := false
	h := &messageCloudHandler{
		CloudHandler: CloudHandler{
			level:     slog.LevelInfo,
			component: "test-component",
			hubName:   "test-hub",
			hubID:     "hub-abc-123",
			hostname:  "node-1",
			projectID: "my-gcp-project",
			version:   "v1.2.3",
			logHook:   func(e gcplog.Entry) { hookCalled = true },
		},
	}

	derived := h.WithGroup("mygroup")
	mch, ok := derived.(*messageCloudHandler)
	if !ok {
		t.Fatal("WithGroup should return a *messageCloudHandler")
	}

	if mch.hubID != "hub-abc-123" {
		t.Errorf("hubID = %q, want %q", mch.hubID, "hub-abc-123")
	}
	if mch.projectID != "my-gcp-project" {
		t.Errorf("projectID = %q, want %q", mch.projectID, "my-gcp-project")
	}
	if mch.version != "v1.2.3" {
		t.Errorf("version = %q, want %q", mch.version, "v1.2.3")
	}
	if mch.logHook == nil {
		t.Fatal("logHook should be preserved, got nil")
	}
	if mch.component != "test-component" {
		t.Errorf("component = %q, want %q", mch.component, "test-component")
	}
	if mch.hubName != "test-hub" {
		t.Errorf("hubName = %q, want %q", mch.hubName, "test-hub")
	}
	if mch.hostname != "node-1" {
		t.Errorf("hostname = %q, want %q", mch.hostname, "node-1")
	}
	if len(mch.groups) != 1 || mch.groups[0] != "mygroup" {
		t.Errorf("groups = %v, want [mygroup]", mch.groups)
	}

	// Verify the hook is callable (same function reference)
	mch.logHook(gcplog.Entry{})
	if !hookCalled {
		t.Error("logHook should be the same function as original")
	}

	// Original should be unchanged
	if len(h.groups) != 0 {
		t.Error("original handler should not be modified")
	}
}
