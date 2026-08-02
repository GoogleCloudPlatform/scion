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
	"strings"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
)

// TestGCPHandler_ServiceContextOnInfo verifies that serviceContext is present
// on INFO-level entries with the correct service and version values.
func TestGCPHandler_ServiceContextOnInfo(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-hub", "")
	logger := slog.New(handler)

	logger.Info("info message")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	sc, ok := data["serviceContext"].(map[string]any)
	if !ok {
		t.Fatal("serviceContext not found or not a map")
	}
	if sc["service"] != "scion-hub" {
		t.Errorf("serviceContext.service = %v, want scion-hub", sc["service"])
	}
	ver, ok := sc["version"].(string)
	if !ok || ver == "" {
		t.Error("serviceContext.version should be a non-empty string")
	}
}

// TestGCPHandler_ErrorReportingFieldsOnError verifies that ERROR entries
// contain serviceContext, stack_trace, and @type.
func TestGCPHandler_ErrorReportingFieldsOnError(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-server", "")
	logger := slog.New(handler)

	logger.Error("something broke")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	// serviceContext must be present
	sc, ok := data["serviceContext"].(map[string]any)
	if !ok {
		t.Fatal("serviceContext not found or not a map on ERROR entry")
	}
	if sc["service"] != "scion-server" {
		t.Errorf("serviceContext.service = %v, want scion-server", sc["service"])
	}

	// stack_trace must be present and contain a Go stack trace
	st, ok := data["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty on ERROR entry")
	}
	if !strings.Contains(st, "goroutine") {
		t.Errorf("stack_trace does not look like a Go stack trace: %s", st[:min(len(st), 100)])
	}

	// @type must match the Error Reporting type
	atType, ok := data["@type"].(string)
	if !ok || atType == "" {
		t.Fatal("@type not found or empty on ERROR entry")
	}
	if atType != errorReportingType {
		t.Errorf("@type = %s, want %s", atType, errorReportingType)
	}
}

// TestGCPHandler_NoStackTraceOnInfo verifies that stack_trace and @type
// are NOT present on INFO-level entries.
func TestGCPHandler_NoStackTraceOnInfo(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-hub", "")
	logger := slog.New(handler)

	logger.Info("just info")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if _, ok := data["stack_trace"]; ok {
		t.Error("stack_trace should NOT be present on INFO entries")
	}
	if _, ok := data["@type"]; ok {
		t.Error("@type should NOT be present on INFO entries")
	}
}

// TestGCPHandler_NoStackTraceOnWarn verifies that stack_trace and @type
// are NOT present on WARN-level entries.
func TestGCPHandler_NoStackTraceOnWarn(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := NewGCPHandler(&buf, opts, "scion-hub", "")
	logger := slog.New(handler)

	logger.Warn("a warning")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if _, ok := data["stack_trace"]; ok {
		t.Error("stack_trace should NOT be present on WARN entries")
	}
	if _, ok := data["@type"]; ok {
		t.Error("@type should NOT be present on WARN entries")
	}
}

// TestCloudHandler_ServiceContextFields verifies that the CloudHandler stores
// the component and version fields correctly for serviceContext generation.
func TestCloudHandler_ServiceContextFields(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "scion-broker",
		version:   "v1.2.3",
	}

	if h.version != "v1.2.3" {
		t.Errorf("version = %s, want v1.2.3", h.version)
	}
	if h.component != "scion-broker" {
		t.Errorf("component = %s, want scion-broker", h.component)
	}
}

// TestCloudHandler_VersionPreservedThroughWithAttrs verifies that the version
// field is preserved when creating child handlers via WithAttrs.
func TestCloudHandler_VersionPreservedThroughWithAttrs(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "scion-hub",
		version:   "abc12345",
	}

	child := h.WithAttrs([]slog.Attr{slog.String("key", "val")}).(*CloudHandler)
	if child.version != "abc12345" {
		t.Errorf("version not preserved through WithAttrs: got %s, want abc12345", child.version)
	}
}

// TestCloudHandler_VersionPreservedThroughWithGroup verifies that the version
// field is preserved when creating child handlers via WithGroup.
func TestCloudHandler_VersionPreservedThroughWithGroup(t *testing.T) {
	h := &CloudHandler{
		level:     slog.LevelInfo,
		component: "scion-hub",
		version:   "def67890",
	}

	child := h.WithGroup("mygroup").(*CloudHandler)
	if child.version != "def67890" {
		t.Errorf("version not preserved through WithGroup: got %s, want def67890", child.version)
	}
}

// TestGCPHandler_VersionPreservedThroughWithAttrs verifies that the version
// field is preserved when creating child GCPHandlers via WithAttrs.
func TestGCPHandler_VersionPreservedThroughWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := NewGCPHandler(&buf, nil, "test-component", "")

	child := handler.WithAttrs([]slog.Attr{slog.String("k", "v")}).(*GCPHandler)
	if child.version == "" {
		t.Error("version should not be empty after WithAttrs")
	}
	if child.version != handler.version {
		t.Errorf("version changed through WithAttrs: parent=%s, child=%s", handler.version, child.version)
	}
}

// TestGCPHandler_VersionPreservedThroughWithGroup verifies that the version
// field is preserved when creating child GCPHandlers via WithGroup.
func TestGCPHandler_VersionPreservedThroughWithGroup(t *testing.T) {
	var buf bytes.Buffer
	handler := NewGCPHandler(&buf, nil, "test-component", "")

	child := handler.WithGroup("grp").(*GCPHandler)
	if child.version == "" {
		t.Error("version should not be empty after WithGroup")
	}
	if child.version != handler.version {
		t.Errorf("version changed through WithGroup: parent=%s, child=%s", handler.version, child.version)
	}
}

// TestGCPHandler_ServiceContextOnDebug verifies that serviceContext is present
// even on DEBUG-level entries, and that stack_trace/@type are absent.
func TestGCPHandler_ServiceContextOnDebug(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := NewGCPHandler(&buf, opts, "scion-broker", "")
	logger := slog.New(handler)

	logger.Debug("debug message")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	sc, ok := data["serviceContext"].(map[string]any)
	if !ok {
		t.Fatal("serviceContext not found on DEBUG entry")
	}
	if sc["service"] != "scion-broker" {
		t.Errorf("serviceContext.service = %v, want scion-broker", sc["service"])
	}
	if _, ok := data["stack_trace"]; ok {
		t.Error("stack_trace should NOT be present on DEBUG entries")
	}
	if _, ok := data["@type"]; ok {
		t.Error("@type should NOT be present on DEBUG entries")
	}
}

// --- CloudHandler Handle()-level tests ---
// These use the logHook to capture the actual Cloud Logging entry and
// inspect the payload built by Handle().

// newTestCloudHandler returns a CloudHandler wired to capture entries via logHook.
func newTestCloudHandler(component, ver string, level slog.Level, captured *[]gcplog.Entry) *CloudHandler {
	return &CloudHandler{
		level:     level,
		component: component,
		version:   ver,
		logHook: func(e gcplog.Entry) {
			*captured = append(*captured, e)
		},
	}
}

// TestCloudHandler_HandleServiceContextOnInfo exercises Handle() on an
// INFO-level record and verifies serviceContext appears in the payload.
func TestCloudHandler_HandleServiceContextOnInfo(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-hub", "v0.8.1", slog.LevelInfo, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "healthy", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	sc, ok := payload["serviceContext"].(map[string]any)
	if !ok {
		t.Fatal("serviceContext not found in payload")
	}
	if sc["service"] != "scion-hub" {
		t.Errorf("serviceContext.service = %v, want scion-hub", sc["service"])
	}
	if sc["version"] != "v0.8.1" {
		t.Errorf("serviceContext.version = %v, want v0.8.1", sc["version"])
	}

	// stack_trace and @type must NOT be present on INFO
	if _, ok := payload["stack_trace"]; ok {
		t.Error("stack_trace should NOT appear in CloudHandler INFO payload")
	}
	if _, ok := payload["@type"]; ok {
		t.Error("@type should NOT appear in CloudHandler INFO payload")
	}
}

// TestCloudHandler_HandleErrorReportingFieldsOnError exercises Handle()
// on an ERROR-level record and verifies stack_trace and @type appear.
func TestCloudHandler_HandleErrorReportingFieldsOnError(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-server", "abc1234", slog.LevelInfo, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelError, "db connection lost", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	// serviceContext must be present
	sc, ok := payload["serviceContext"].(map[string]any)
	if !ok {
		t.Fatal("serviceContext not found in ERROR payload")
	}
	if sc["service"] != "scion-server" {
		t.Errorf("serviceContext.service = %v, want scion-server", sc["service"])
	}
	if sc["version"] != "abc1234" {
		t.Errorf("serviceContext.version = %v, want abc1234", sc["version"])
	}

	// stack_trace must be present and look like a Go stack trace
	st, ok := payload["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty in ERROR payload")
	}
	if !strings.Contains(st, "goroutine") {
		t.Errorf("stack_trace does not look like a Go stack trace: %.100s", st)
	}

	// @type must match
	atType, ok := payload["@type"].(string)
	if !ok {
		t.Fatal("@type not found in ERROR payload")
	}
	if atType != errorReportingType {
		t.Errorf("@type = %s, want %s", atType, errorReportingType)
	}
}

// TestCloudHandler_HandleNoStackTraceOnWarn exercises Handle() on a
// WARN-level record and verifies stack_trace and @type are absent.
func TestCloudHandler_HandleNoStackTraceOnWarn(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-broker", "v1.0.0", slog.LevelDebug, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelWarn, "disk space low", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	// serviceContext should still be present
	if _, ok := payload["serviceContext"]; !ok {
		t.Error("serviceContext should be present on WARN entries")
	}

	// Error-only fields must be absent
	if _, ok := payload["stack_trace"]; ok {
		t.Error("stack_trace should NOT appear in CloudHandler WARN payload")
	}
	if _, ok := payload["@type"]; ok {
		t.Error("@type should NOT appear in CloudHandler WARN payload")
	}
}

// TestGCPHandler_CallerStackTracePreserved verifies that when the caller
// provides a custom stack_trace attribute, it is preserved (not overwritten).
func TestGCPHandler_CallerStackTracePreserved(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-hub", "")
	logger := slog.New(handler)

	customStack := "custom goroutine stack from caller"
	logger.Error("error with custom stack", slog.String("stack_trace", customStack))

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	st, ok := data["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty on ERROR entry")
	}
	if st != customStack {
		t.Errorf("stack_trace was overwritten: got %q, want %q", st, customStack)
	}
}

// TestGCPHandler_CallerTypePreserved verifies that when the caller provides
// a custom @type attribute, it is preserved (not overwritten).
func TestGCPHandler_CallerTypePreserved(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-hub", "")
	logger := slog.New(handler)

	customType := "custom.error.type"
	logger.Error("error with custom type",
		slog.String("stack_trace", "custom stack"),
		slog.String("@type", customType),
	)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	atType, ok := data["@type"].(string)
	if !ok || atType == "" {
		t.Fatal("@type not found or empty on ERROR entry")
	}
	if atType != customType {
		t.Errorf("@type was overwritten: got %q, want %q", atType, customType)
	}
}

// TestCloudHandler_HandleCallerStackTracePreserved verifies that CloudHandler
// preserves a caller-provided stack_trace attribute on ERROR entries.
func TestCloudHandler_HandleCallerStackTracePreserved(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-server", "abc1234", slog.LevelInfo, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelError, "error with custom stack", 0)
	customStack := "custom goroutine stack from caller"
	r.AddAttrs(slog.String("stack_trace", customStack))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	st, ok := payload["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty in ERROR payload")
	}
	if st != customStack {
		t.Errorf("stack_trace was overwritten: got %q, want %q", st, customStack)
	}
}

// TestCloudHandler_HandleCallerTypePreserved verifies that CloudHandler
// preserves a caller-provided @type attribute on ERROR entries.
func TestCloudHandler_HandleCallerTypePreserved(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-server", "abc1234", slog.LevelInfo, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelError, "error with custom type", 0)
	customType := "custom.error.type"
	r.AddAttrs(
		slog.String("stack_trace", "custom stack"),
		slog.String("@type", customType),
	)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	atType, ok := payload["@type"].(string)
	if !ok {
		t.Fatal("@type not found in ERROR payload")
	}
	if atType != customType {
		t.Errorf("@type was overwritten: got %q, want %q", atType, customType)
	}
}

// TestGCPHandler_DebugStackOutput verifies that the auto-generated stack trace
// uses debug.Stack() format (contains "goroutine" marker).
func TestGCPHandler_DebugStackOutput(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "scion-server", "")
	logger := slog.New(handler)

	logger.Error("auto stack trace test")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	st, ok := data["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty on ERROR entry")
	}
	// debug.Stack() output includes "goroutine" and the calling function
	if !strings.Contains(st, "goroutine") {
		t.Errorf("stack_trace does not look like debug.Stack() output: %.100s", st)
	}
}

// TestCloudHandler_HandleDebugStackOutput verifies that the CloudHandler
// auto-generated stack trace uses debug.Stack() format.
func TestCloudHandler_HandleDebugStackOutput(t *testing.T) {
	var entries []gcplog.Entry
	h := newTestCloudHandler("scion-server", "abc1234", slog.LevelInfo, &entries)

	r := slog.NewRecord(time.Now(), slog.LevelError, "auto stack test", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	payload, ok := entries[0].Payload.(map[string]any)
	if !ok {
		t.Fatal("payload is not map[string]any")
	}

	st, ok := payload["stack_trace"].(string)
	if !ok || st == "" {
		t.Fatal("stack_trace not found or empty in ERROR payload")
	}
	// debug.Stack() output includes "goroutine" and the calling function
	if !strings.Contains(st, "goroutine") {
		t.Errorf("stack_trace does not look like debug.Stack() output: %.100s", st)
	}
}

// TestErrorReportingTypeConstant verifies the constant value is correct.
func TestErrorReportingTypeConstant(t *testing.T) {
	expected := "type.googleapis.com/google.devtools.clouderrorreporting.v1beta1.ReportedErrorEvent"
	if errorReportingType != expected {
		t.Errorf("errorReportingType = %s, want %s", errorReportingType, expected)
	}
}
