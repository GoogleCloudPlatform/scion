/*
Copyright 2025 The Scion Authors.
*/

package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestNewMultiHandler(t *testing.T) {
	// Create with nil handlers - should filter them out
	h := newMultiHandler(nil, nil)
	if len(h.handlers) != 0 {
		t.Errorf("newMultiHandler should filter nil handlers, got %d", len(h.handlers))
	}
}

func TestMultiHandler_Enabled(t *testing.T) {
	// Create a handler that's always enabled for Info+
	jsonHandler := slog.NewJSONHandler(nil, &slog.HandlerOptions{Level: slog.LevelInfo})

	h := newMultiHandler(jsonHandler)

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("should be enabled for Info level")
	}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("should not be enabled for Debug level")
	}
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	jsonHandler := slog.NewJSONHandler(nil, nil)
	h := newMultiHandler(jsonHandler)

	attrs := []slog.Attr{slog.String("key", "value")}
	newH := h.WithAttrs(attrs)

	if newH == nil {
		t.Error("WithAttrs should return a new handler")
	}
	mh, ok := newH.(*multiHandler)
	if !ok {
		t.Error("WithAttrs should return a multiHandler")
	}
	if len(mh.handlers) != 1 {
		t.Errorf("WithAttrs should preserve handler count, got %d", len(mh.handlers))
	}
}

func TestMultiHandler_WithGroup(t *testing.T) {
	jsonHandler := slog.NewJSONHandler(nil, nil)
	h := newMultiHandler(jsonHandler)

	newH := h.WithGroup("mygroup")

	if newH == nil {
		t.Error("WithGroup should return a new handler")
	}
	mh, ok := newH.(*multiHandler)
	if !ok {
		t.Error("WithGroup should return a multiHandler")
	}
	if len(mh.handlers) != 1 {
		t.Errorf("WithGroup should preserve handler count, got %d", len(mh.handlers))
	}
}

func TestOTelConfig(t *testing.T) {
	cfg := OTelConfig{
		Endpoint:  "localhost:4317",
		Protocol:  "grpc",
		Insecure:  true,
		ProjectID: "test-project",
	}

	if cfg.Endpoint != "localhost:4317" {
		t.Error("Endpoint not set correctly")
	}
}

func TestNewOTelHandler_NilProvider(t *testing.T) {
	h := NewOTelHandler("test", nil)
	if h != nil {
		t.Error("NewOTelHandler should return nil when provider is nil")
	}
}

func TestSetupWithOTel_NilProvider(t *testing.T) {
	// Should fall back to base handler behavior
	SetupWithOTel("test-component", "", false, false, nil)

	// Verify logging still works
	logger := slog.Default()
	if logger == nil {
		t.Error("Default logger should be set")
	}
}

func TestCreateBaseHandler(t *testing.T) {
	// Test JSON handler (default)
	h := createBaseHandler("test", false, false, "")
	if h == nil {
		t.Error("createBaseHandler should return a handler")
	}

	// Test GCP handler
	h = createBaseHandler("test", false, true, "")
	if h == nil {
		t.Error("createBaseHandler should return GCP handler")
	}
}

func TestInitOTelLogging_Disabled(t *testing.T) {
	// Clear environment
	t.Setenv("SCION_OTEL_ENDPOINT", "")
	t.Setenv("SCION_OTEL_LOG_ENABLED", "false")

	lp, cleanup, err := InitOTelLogging(context.Background(), OTelConfig{})
	if err != nil {
		t.Errorf("InitOTelLogging should not error when disabled: %v", err)
	}
	if lp != nil {
		t.Error("LoggerProvider should be nil when disabled")
	}
	if cleanup == nil {
		t.Error("cleanup should never be nil")
	}
	cleanup() // Should not panic
}

func TestInitOTelLogging_NoEndpoint(t *testing.T) {
	t.Setenv("SCION_OTEL_LOG_ENABLED", "true")
	t.Setenv("SCION_OTEL_ENDPOINT", "")

	lp, cleanup, err := InitOTelLogging(context.Background(), OTelConfig{})
	if err != nil {
		t.Errorf("InitOTelLogging should not error with no endpoint: %v", err)
	}
	if lp != nil {
		t.Error("LoggerProvider should be nil with no endpoint")
	}
	cleanup() // Should not panic
}

func TestSetupWithOTel_CloudRunSuppressesStdout(t *testing.T) {
	// When K_SERVICE is set and a cloud handler is present, the stdout base
	// handler should be omitted to avoid duplicate Cloud Logging entries.
	t.Setenv("K_SERVICE", "my-service")

	// Use a real CloudHandler so hasCloudHandler detects it.
	cloudHandler := &CloudHandler{}

	SetupWithOTel("test-component", "", false, true, nil, cloudHandler)

	// The default logger should be set (not nil).
	logger := slog.Default()
	if logger == nil {
		t.Fatal("Default logger should be set")
	}

	// Verify the handler is directly the cloudHandler (no multiHandler
	// wrapping needed when there's only one handler).
	h := logger.Handler()
	if _, ok := h.(*CloudHandler); !ok {
		// If it's a multiHandler, verify it contains only the cloud handler.
		mh, isMH := h.(*multiHandler)
		if !isMH {
			t.Fatalf("expected CloudHandler or multiHandler, got %T", h)
		}
		// Should have exactly 1 handler (the cloud handler), not 2.
		if len(mh.handlers) != 1 {
			t.Errorf("expected 1 handler (cloud only), got %d — stdout handler was not suppressed", len(mh.handlers))
		}
	}
}

func TestSetupWithOTel_NonCloudRunKeepsStdout(t *testing.T) {
	// When K_SERVICE is NOT set, both stdout and cloud handlers should be present.
	t.Setenv("K_SERVICE", "")

	cloudHandler := slog.NewJSONHandler(nil, nil)

	SetupWithOTel("test-component", "", false, true, nil, cloudHandler)

	logger := slog.Default()
	if logger == nil {
		t.Fatal("Default logger should be set")
	}

	h := logger.Handler()
	mh, ok := h.(*multiHandler)
	if !ok {
		t.Fatalf("expected multiHandler with 2 handlers, got single handler %T", h)
	}
	if len(mh.handlers) != 2 {
		t.Errorf("expected 2 handlers (stdout + cloud), got %d", len(mh.handlers))
	}
}

func TestSetupWithOTel_CloudRunNoCloudHandler(t *testing.T) {
	// On Cloud Run but without a cloud handler — stdout should still be active.
	t.Setenv("K_SERVICE", "my-service")

	SetupWithOTel("test-component", "", false, true, nil)

	logger := slog.Default()
	if logger == nil {
		t.Fatal("Default logger should be set")
	}
	// With only the base handler, the logger handler should not be a multiHandler.
	if _, ok := logger.Handler().(*multiHandler); ok {
		t.Error("expected single handler (base), not multiHandler")
	}
}

func TestHasCloudHandler(t *testing.T) {
	if hasCloudHandler(nil) {
		t.Error("nil slice should return false")
	}
	if hasCloudHandler([]slog.Handler{nil, nil}) {
		t.Error("all-nil slice should return false")
	}
	if hasCloudHandler([]slog.Handler{nil, slog.NewJSONHandler(nil, nil)}) {
		t.Error("non-cloud handler (JSONHandler) should return false")
	}
	if !hasCloudHandler([]slog.Handler{nil, &CloudHandler{}}) {
		t.Error("slice with CloudHandler should return true")
	}
}

func TestEnvVarConstants(t *testing.T) {
	// Verify constants are set correctly
	if EnvOTelEndpoint != "SCION_OTEL_ENDPOINT" {
		t.Errorf("EnvOTelEndpoint = %s, want SCION_OTEL_ENDPOINT", EnvOTelEndpoint)
	}
	if EnvOTelInsecure != "SCION_OTEL_INSECURE" {
		t.Errorf("EnvOTelInsecure = %s, want SCION_OTEL_INSECURE", EnvOTelInsecure)
	}
	if EnvOTelLogEnable != "SCION_OTEL_LOG_ENABLED" {
		t.Errorf("EnvOTelLogEnable = %s, want SCION_OTEL_LOG_ENABLED", EnvOTelLogEnable)
	}
}
