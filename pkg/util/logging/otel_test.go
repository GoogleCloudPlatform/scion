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
		CAFile:    "/etc/ssl/certs/custom-root.pem",
		CertFile:  "/etc/ssl/certs/client.pem",
		KeyFile:   "/etc/ssl/private/client-key.pem",
		ProjectID: "test-project",
	}

	if cfg.Endpoint != "localhost:4317" {
		t.Error("Endpoint not set correctly")
	}
	if cfg.CAFile != "/etc/ssl/certs/custom-root.pem" || cfg.CertFile != "/etc/ssl/certs/client.pem" || cfg.KeyFile != "/etc/ssl/private/client-key.pem" {
		t.Error("TLS file paths not set correctly")
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
	SetupWithOTel("test-component", false, false, nil)

	// Verify logging still works
	logger := slog.Default()
	if logger == nil {
		t.Error("Default logger should be set")
	}
}

func TestCreateBaseHandler(t *testing.T) {
	// Test JSON handler (default)
	h := createBaseHandler("test", false, false)
	if h == nil {
		t.Error("createBaseHandler should return a handler")
	}

	// Test GCP handler
	h = createBaseHandler("test", false, true)
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

func TestEnvVarConstants(t *testing.T) {
	// Verify constants are set correctly
	if EnvOTelEndpoint != "SCION_OTEL_ENDPOINT" {
		t.Errorf("EnvOTelEndpoint = %s, want SCION_OTEL_ENDPOINT", EnvOTelEndpoint)
	}
	if EnvOTelInsecure != "SCION_OTEL_INSECURE" {
		t.Errorf("EnvOTelInsecure = %s, want SCION_OTEL_INSECURE", EnvOTelInsecure)
	}
	if EnvOTelCAFile != "SCION_OTEL_CA_FILE" || EnvOTelCertFile != "SCION_OTEL_CERT_FILE" || EnvOTelKeyFile != "SCION_OTEL_KEY_FILE" {
		t.Error("OTel TLS env constants not set correctly")
	}
	if EnvOTelLogEnable != "SCION_OTEL_LOG_ENABLED" {
		t.Errorf("EnvOTelLogEnable = %s, want SCION_OTEL_LOG_ENABLED", EnvOTelLogEnable)
	}
}
