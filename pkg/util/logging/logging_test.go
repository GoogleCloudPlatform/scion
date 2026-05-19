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
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGCPHandler(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "test-component")
	logger := slog.New(handler)

	logger.Info("test message", "key", "value")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	assert.Equal(t, "INFO", data[GCPKeySeverity])
	assert.Equal(t, "test message", data[GCPKeyMessage])
	assert.NotNil(t, data[GCPKeyTimestamp])
	assert.Equal(t, "value", data["key"])

	labels := data[GCPKeyLabels].(map[string]interface{})
	assert.Equal(t, "test-component", labels["component"])

	hostname, _ := os.Hostname()
	if hostname != "" {
		assert.Equal(t, hostname, labels["hostname"])
		assert.Equal(t, hostname, labels["hub"])
	}
}

func TestGCPHandler_EmptyMessageSuppressed(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "test-component")
	logger := slog.New(handler)

	// Log with empty message (as HTTP request logs do)
	logger.LogAttrs(nil, slog.LevelInfo, "",
		slog.Group("httpRequest",
			slog.String("requestMethod", "GET"),
			slog.Int("status", 200),
		),
	)

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	// The "message" key should not be present
	_, hasMessage := data[GCPKeyMessage]
	assert.False(t, hasMessage, "empty message should be suppressed, got: %v", data[GCPKeyMessage])

	// httpRequest should still be present
	httpReq, ok := data["httpRequest"].(map[string]interface{})
	assert.True(t, ok, "expected httpRequest group")
	assert.Equal(t, "GET", httpReq["requestMethod"])
}

func TestGCPHandler_LabelsPromoteAgentProject(t *testing.T) {
	var buf bytes.Buffer
	handler := NewGCPHandler(&buf, nil, "test-component")
	logger := slog.New(handler)

	logger.Info("test message",
		AttrAgentID, "agent-abc",
		AttrProjectID, "grove-xyz",
	)

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	labels := data[GCPKeyLabels].(map[string]interface{})
	assert.Equal(t, "test-component", labels["component"])
	assert.Equal(t, "agent-abc", labels[AttrAgentID])
	assert.Equal(t, "grove-xyz", labels[AttrProjectID])

	// Also present in payload
	assert.Equal(t, "agent-abc", data[AttrAgentID])
	assert.Equal(t, "grove-xyz", data[AttrProjectID])
}

func TestGCPHandler_LabelsFromWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := NewGCPHandler(&buf, nil, "test-component")

	// Simulate Logger(ctx) which uses slog.With()
	childHandler := handler.WithAttrs([]slog.Attr{
		slog.String(AttrAgentID, "pre-agent"),
		slog.String(AttrProjectID, "pre-grove"),
	})
	logger := slog.New(childHandler)

	logger.Info("test message")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	labels := data[GCPKeyLabels].(map[string]interface{})
	assert.Equal(t, "pre-agent", labels[AttrAgentID])
	assert.Equal(t, "pre-grove", labels[AttrProjectID])
}

func TestGCPHandler_TraceCorrelationFields(t *testing.T) {
	t.Setenv(EnvGCPProjectID, "deploy-demo-test")

	var buf bytes.Buffer
	handler := NewGCPHandler(&buf, nil, "test-component")
	logger := slog.New(handler)

	logger.Info("trace log",
		AttrTraceID, "4bf92f3577b34da6a3ce929d0e0e4736/1;o=1",
	)

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	assert.Equal(t, "projects/deploy-demo-test/traces/4bf92f3577b34da6a3ce929d0e0e4736", data[GCPKeyTrace])

	labels := data[GCPKeyLabels].(map[string]interface{})
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", labels[gcpTraceIDLabelKey])
}

func TestSubsystemLogger(t *testing.T) {
	// Set up a JSON handler writing to a buffer so we can inspect output
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := slog.NewJSONHandler(&buf, opts).WithAttrs([]slog.Attr{
		slog.String(AttrComponent, "test-component"),
	})
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	logger := Subsystem("hub.notifications")
	logger.Info("test subsystem message")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	assert.Equal(t, "test-component", data[AttrComponent])
	assert.Equal(t, "hub.notifications", data[AttrSubsystem])
	assert.Equal(t, "test subsystem message", data["msg"])
}

func TestGCPHandlerSourceLocation(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo, AddSource: true}
	handler := NewGCPHandler(&buf, opts, "test-component")
	logger := slog.New(handler)

	logger.Info("test message")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	source := data[GCPKeySourceLocation].(map[string]interface{})
	assert.Contains(t, source["file"], "logging_test.go")
	assert.NotEmpty(t, source["line"])
	assert.Contains(t, source["function"], "TestGCPHandlerSourceLocation")
}
