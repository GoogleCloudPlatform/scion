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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstrumentedResponseWriter_CapturesStatusAndBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &InstrumentedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	w.WriteHeader(http.StatusCreated)
	n, err := w.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if w.statusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", w.statusCode)
	}
	if w.bytesWritten != int64(n) {
		t.Errorf("expected bytesWritten=%d, got %d", n, w.bytesWritten)
	}
	if w.bytesWritten != 11 {
		t.Errorf("expected 11 bytes written, got %d", w.bytesWritten)
	}
}

func TestInstrumentedResponseWriter_DefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &InstrumentedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	// Write without explicit WriteHeader → should default to 200
	w.Write([]byte("ok"))

	if w.statusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.statusCode)
	}
	if !w.wroteHeader {
		t.Error("expected wroteHeader to be true after Write")
	}
}

func TestInstrumentedResponseWriter_DoubleWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &InstrumentedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	w.WriteHeader(http.StatusNotFound)
	w.WriteHeader(http.StatusOK) // Should be ignored

	if w.statusCode != http.StatusNotFound {
		t.Errorf("expected status 404 (first call wins), got %d", w.statusCode)
	}
}

func TestInstrumentedResponseWriter_FlushBeforeWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &InstrumentedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	// Flush before any Write or WriteHeader — should commit headers through the wrapper
	w.Flush()

	if !w.wroteHeader {
		t.Error("expected wroteHeader to be true after Flush")
	}
	if w.statusCode != http.StatusOK {
		t.Errorf("expected status 200 after Flush, got %d", w.statusCode)
	}

	// Subsequent Write should not trigger a superfluous WriteHeader
	n, err := w.Write([]byte("streamed"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.bytesWritten != int64(n) {
		t.Errorf("expected bytesWritten=%d, got %d", n, w.bytesWritten)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected recorded status 200, got %d", rec.Code)
	}
}

func TestInstrumentedResponseWriter_WriteHeaderThenFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &InstrumentedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	w.WriteHeader(http.StatusAccepted)
	w.Flush()

	if w.statusCode != http.StatusAccepted {
		t.Errorf("expected status 202, got %d", w.statusCode)
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("expected recorded status 202, got %d", rec.Code)
	}
}

func TestRequestMetaContext(t *testing.T) {
	ctx := context.Background()

	// No meta in context
	if meta := RequestMetaFromContext(ctx); meta != nil {
		t.Error("expected nil meta from empty context")
	}

	// Set meta
	meta := &RequestMeta{RequestID: "req-123", TraceID: "trace-abc"}
	ctx = ContextWithRequestMeta(ctx, meta)

	got := RequestMetaFromContext(ctx)
	if got == nil {
		t.Fatal("expected non-nil meta from context")
	}
	if got.RequestID != "req-123" {
		t.Errorf("expected RequestID=req-123, got %s", got.RequestID)
	}
	if got.TraceID != "trace-abc" {
		t.Errorf("expected TraceID=trace-abc, got %s", got.TraceID)
	}
}

func TestSetRequestProjectID_AgentID(t *testing.T) {
	meta := &RequestMeta{}
	ctx := ContextWithRequestMeta(context.Background(), meta)

	SetRequestProjectID(ctx, "my-grove")
	SetRequestAgentID(ctx, "agent-42")

	if meta.ProjectID != "my-grove" {
		t.Errorf("expected ProjectID=my-grove, got %s", meta.ProjectID)
	}
	if meta.AgentID != "agent-42" {
		t.Errorf("expected AgentID=agent-42, got %s", meta.AgentID)
	}
}

func TestSetRequestBrokerID(t *testing.T) {
	meta := &RequestMeta{}
	ctx := ContextWithRequestMeta(context.Background(), meta)

	SetRequestBrokerID(ctx, "broker-west-1")

	if meta.BrokerID != "broker-west-1" {
		t.Errorf("expected BrokerID=broker-west-1, got %s", meta.BrokerID)
	}
}

func TestSetRequestProjectID_NilContext(t *testing.T) {
	// Should not panic when no meta in context
	ctx := context.Background()
	SetRequestProjectID(ctx, "test")
	SetRequestAgentID(ctx, "test")
	SetRequestBrokerID(ctx, "test")
}

func TestExtractIDsFromPath(t *testing.T) {
	patterns := HubPathPatterns()

	tests := []struct {
		path    string
		projectID string
		agentID string
	}{
		{"/api/v1/groves/my-project/agents", "my-project", ""},
		{"/api/v1/groves/my-project", "my-project", ""},
		{"/api/v1/agents/agent-42", "", "agent-42"},
		{"/api/v1/agents/agent-42/start", "", "agent-42"},
		{"/api/v1/info", "", ""},
		{"/healthz", "", ""},
		{"/api/v1/groves/", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			projectID, agentID := extractIDsFromPath(tt.path, patterns)
			if projectID != tt.projectID {
				t.Errorf("projectID: expected %q, got %q", tt.projectID, projectID)
			}
			if agentID != tt.agentID {
				t.Errorf("agentID: expected %q, got %q", tt.agentID, agentID)
			}
		})
	}
}

func TestRequestLogMiddleware_ProducesCorrectJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	handler := RequestLogMiddleware(logger, "hub", HubPathPatterns())(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/groves/test-grove/agents", nil)
	req.Header.Set("User-Agent", "scion-cli/0.1.0")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Parse JSON output
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v\nraw: %s", err, buf.String())
	}

	// Check httpRequest group
	httpReq, ok := entry["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("missing httpRequest group in log output: %s", buf.String())
	}

	if httpReq["requestMethod"] != "GET" {
		t.Errorf("expected requestMethod=GET, got %v", httpReq["requestMethod"])
	}
	if httpReq["status"] != float64(200) {
		t.Errorf("expected status=200, got %v", httpReq["status"])
	}
	if httpReq["responseSize"] != float64(15) {
		t.Errorf("expected responseSize=15, got %v", httpReq["responseSize"])
	}
	if httpReq["userAgent"] != "scion-cli/0.1.0" {
		t.Errorf("expected userAgent=scion-cli/0.1.0, got %v", httpReq["userAgent"])
	}

	// Check labels
	if entry["component"] != "hub" {
		t.Errorf("expected component=hub, got %v", entry["component"])
	}
	if entry["grove_id"] != "test-grove" {
		t.Errorf("expected grove_id=test-grove, got %v", entry["grove_id"])
	}

	// Check request_id is present (UUID)
	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected non-empty request_id")
	}
}

func TestRequestLogMiddleware_TraceIDGeneration(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := RequestLogMiddleware(logger, "test", nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify meta is in context
			meta := RequestMetaFromContext(r.Context())
			if meta == nil {
				t.Error("expected RequestMeta in context")
				return
			}
			if meta.RequestID == "" {
				t.Error("expected non-empty RequestID")
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	// No trace headers → should generate a UUID request ID
	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	json.Unmarshal(buf.Bytes(), &entry)

	if _, ok := entry["trace_id"]; ok {
		t.Error("expected no trace_id when no trace header present")
	}
	if entry["request_id"] == nil || entry["request_id"] == "" {
		t.Error("expected non-empty request_id")
	}
}

func TestRequestLogMiddleware_TraceIDFromHeader(t *testing.T) {
	headers := []struct {
		name  string
		value string
		want  string
	}{
		{"X-Cloud-Trace-Context", "4bf92f3577b34da6a3ce929d0e0e4736/456;o=1", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"X-Trace-ID", "custom-trace-id", "custom-trace-id"},
	}

	for _, tt := range headers {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))

			handler := RequestLogMiddleware(logger, "test", nil)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			)

			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set(tt.name, tt.value)
			handler.ServeHTTP(httptest.NewRecorder(), req)

			var entry map[string]any
			json.Unmarshal(buf.Bytes(), &entry)

			if entry["trace_id"] != tt.want {
				t.Errorf("expected trace_id=%s, got %v", tt.want, entry["trace_id"])
			}
		})
	}
}

func TestRequestLogMiddleware_HandlerEnrichment(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := RequestLogMiddleware(logger, "hub", nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Handler enriches metadata after middleware parsed the URL
			SetRequestProjectID(r.Context(), "enriched-grove")
			SetRequestAgentID(r.Context(), "enriched-agent")
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/other", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	json.Unmarshal(buf.Bytes(), &entry)

	if entry["grove_id"] != "enriched-grove" {
		t.Errorf("expected grove_id=enriched-grove, got %v", entry["grove_id"])
	}
	if entry["agent_id"] != "enriched-agent" {
		t.Errorf("expected agent_id=enriched-agent, got %v", entry["agent_id"])
	}
}

func TestRequestLogMiddleware_FileOutput(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "requests.log")

	logger, cleanup, err := NewRequestLogger(RequestLoggerConfig{
		FilePath:  logFile,
		Component: "test",
		Level:     slog.LevelInfo,
	})
	if err != nil {
		t.Fatalf("NewRequestLogger failed: %v", err)
	}
	defer cleanup()

	handler := RequestLogMiddleware(logger, "test", nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}),
	)

	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Force flush
	cleanup()

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	if len(content) == 0 {
		t.Fatal("expected non-empty log file")
	}

	var entry map[string]any
	if err := json.Unmarshal(content, &entry); err != nil {
		t.Fatalf("log file content is not valid JSON: %v", err)
	}

	httpReq, ok := entry["httpRequest"].(map[string]any)
	if !ok {
		t.Fatal("missing httpRequest in file output")
	}
	if httpReq["requestMethod"] != "GET" {
		t.Errorf("expected requestMethod=GET in file, got %v", httpReq["requestMethod"])
	}
}

func TestNewRequestLogger_ForegroundSuppression(t *testing.T) {
	// Foreground mode with no file/cloud targets → should produce a discard logger
	logger, cleanup, err := NewRequestLogger(RequestLoggerConfig{
		Component:  "test",
		Foreground: true,
		Level:      slog.LevelInfo,
	})
	if err != nil {
		t.Fatalf("NewRequestLogger failed: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// The logger should not panic and should handle calls gracefully
	handler := RequestLogMiddleware(logger, "test", nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	// No assertion needed — just verify it doesn't write to stdout or panic
}

func TestNewRequestLogger_ForegroundWithFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "requests.log")

	// Foreground with file target → should write to file, not stdout
	logger, cleanup, err := NewRequestLogger(RequestLoggerConfig{
		FilePath:   logFile,
		Component:  "test",
		Foreground: true,
		Level:      slog.LevelInfo,
	})
	if err != nil {
		t.Fatalf("NewRequestLogger failed: %v", err)
	}
	defer cleanup()

	handler := RequestLogMiddleware(logger, "test", nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	cleanup()

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("expected non-empty log file in foreground+file mode")
	}
}

func TestLoggerContextEnrichment(t *testing.T) {
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	meta := &RequestMeta{
		RequestID: "req-enriched",
		TraceID:   "trace-enriched",
		ProjectID:   "grove-enriched",
		AgentID:   "agent-enriched",
	}
	ctx := ContextWithRequestMeta(context.Background(), meta)

	l := Logger(ctx)
	l.Info("test message")

	output := buf.String()
	for _, expected := range []string{"req-enriched", "trace-enriched", "grove-enriched", "agent-enriched"} {
		if !strings.Contains(output, expected) {
			t.Errorf("expected output to contain %q, got: %s", expected, output)
		}
	}
}

func TestRequestLogMiddleware_StatusLevels(t *testing.T) {
	tests := []struct {
		status int
		level  string
	}{
		{200, "INFO"},
		{201, "INFO"},
		{301, "INFO"},
		{400, "WARN"},
		{404, "WARN"},
		{500, "ERROR"},
		{503, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			handler := RequestLogMiddleware(logger, "test", nil)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
				}),
			)

			req := httptest.NewRequest("GET", "/test", nil)
			handler.ServeHTTP(httptest.NewRecorder(), req)

			var entry map[string]any
			json.Unmarshal(buf.Bytes(), &entry)

			level, _ := entry["level"].(string)
			if level != tt.level {
				t.Errorf("status %d: expected level=%s, got %s", tt.status, tt.level, level)
			}
		})
	}
}
