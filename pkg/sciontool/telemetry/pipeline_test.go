/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestNew_Disabled(t *testing.T) {
	// Clear env and disable telemetry
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "false")

	pipeline := New()
	if pipeline != nil {
		t.Error("Expected nil pipeline when telemetry is disabled")
	}
}

func TestNew_Enabled(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")

	pipeline := New()
	if pipeline == nil {
		t.Error("Expected non-nil pipeline when telemetry is enabled")
		return
	}

	if pipeline.Config() == nil {
		t.Error("Expected pipeline to have config")
	}
}

func TestPipeline_StartStop(t *testing.T) {
	clearTelemetryEnv()
	// Use port 0 to let the OS assign ephemeral ports, avoiding conflicts
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")

	pipeline := New()
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()

	// Start pipeline
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("Failed to start pipeline: %v", err)
	}

	if !pipeline.IsRunning() {
		t.Error("Expected pipeline to be running after Start")
	}

	// Give servers time to start
	time.Sleep(50 * time.Millisecond)

	// Stop pipeline
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := pipeline.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop pipeline: %v", err)
	}

	if pipeline.IsRunning() {
		t.Error("Expected pipeline to not be running after Stop")
	}
}

func TestPipeline_DoubleStart(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")

	pipeline := New()
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = pipeline.Stop(stopCtx)
		cancel()
	}()

	// First start should succeed
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("First start failed: %v", err)
	}

	// Second start should fail
	if err := pipeline.Start(ctx); err == nil {
		t.Error("Expected error on double start")
	}
}

func TestPipeline_NilSafe(t *testing.T) {
	var pipeline *Pipeline

	// These should all be safe to call on nil
	if err := pipeline.Start(context.Background()); err != nil {
		t.Error("Start on nil should return nil")
	}
	if err := pipeline.Stop(context.Background()); err != nil {
		t.Error("Stop on nil should return nil")
	}
	if pipeline.IsRunning() {
		t.Error("IsRunning on nil should return false")
	}
	if pipeline.Config() != nil {
		t.Error("Config on nil should return nil")
	}
}

func TestNewWithConfig(t *testing.T) {
	// nil config
	if NewWithConfig(nil) != nil {
		t.Error("Expected nil pipeline for nil config")
	}

	// disabled config
	cfg := &Config{Enabled: false}
	if NewWithConfig(cfg) != nil {
		t.Error("Expected nil pipeline for disabled config")
	}

	// enabled config
	cfg = &Config{
		Enabled:  true,
		GRPCPort: 0,
		HTTPPort: 0,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Error("Expected non-nil pipeline for enabled config")
	}
}

func TestPipeline_HandleMetrics_NilExporter(t *testing.T) {
	cfg := &Config{
		Enabled:  true,
		GRPCPort: 0,
		HTTPPort: 0,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	// handleMetrics with nil exporter should not error
	err := pipeline.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{
		{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "test"}}}}},
	})
	if err != nil {
		t.Errorf("handleMetrics should not return error without exporter, got: %v", err)
	}
}

func TestPipeline_HandleMetrics_Empty(t *testing.T) {
	cfg := &Config{
		Enabled:  true,
		GRPCPort: 0,
		HTTPPort: 0,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	// Empty metrics should return nil
	err := pipeline.handleMetrics(context.Background(), nil)
	if err != nil {
		t.Errorf("handleMetrics with empty input should return nil, got: %v", err)
	}
}

func TestPipeline_MetricHandlerRegistered(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")

	pipeline := New()
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("Failed to start pipeline: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = pipeline.Stop(stopCtx)
		cancel()
	}()

	// Verify the receiver has a metric handler registered
	if pipeline.receiver == nil {
		t.Fatal("Expected receiver to be created")
	}
	if pipeline.receiver.metricHandler == nil {
		t.Error("Expected metric handler to be registered on receiver")
	}
}

func TestPipeline_HandleLogs_NilExporter(t *testing.T) {
	cfg := &Config{
		Enabled:  true,
		GRPCPort: 0,
		HTTPPort: 0,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	// handleLogs with nil exporter should not error
	err := pipeline.handleLogs(context.Background(), []*logspb.ResourceLogs{
		{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}},
	})
	if err != nil {
		t.Errorf("handleLogs should not return error without exporter, got: %v", err)
	}
}

func TestPipeline_HandleLogs_Empty(t *testing.T) {
	cfg := &Config{
		Enabled:  true,
		GRPCPort: 0,
		HTTPPort: 0,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	// Empty logs should return nil
	err := pipeline.handleLogs(context.Background(), nil)
	if err != nil {
		t.Errorf("handleLogs with empty input should return nil, got: %v", err)
	}
}

func TestPipeline_LogHandlerRegistered(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "0")
	t.Setenv(EnvHTTPPort, "0")

	pipeline := New()
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("Failed to start pipeline: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = pipeline.Stop(stopCtx)
		cancel()
	}()

	// Verify the receiver has a log handler registered
	if pipeline.receiver == nil {
		t.Fatal("Expected receiver to be created")
	}
	if pipeline.receiver.logHandler == nil {
		t.Error("Expected log handler to be registered on receiver")
	}
}
