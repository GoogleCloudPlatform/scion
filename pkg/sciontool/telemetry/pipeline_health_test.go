/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestPipeline_HealthGauge_Registers(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "54401")
	t.Setenv(EnvHTTPPort, "54402")
	defer clearTelemetryEnv()

	cfg := &Config{
		Enabled:       true,
		CloudEnabled:  false,
		GRPCPort:      54401,
		HTTPPort:      54402,
		CloudProvider: "",
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("Failed to start pipeline: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := pipeline.Stop(stopCtx); err != nil {
			t.Errorf("pipeline.Stop: %v", err)
		}
	}()

	// Without cloud configured, health gauge should not be started
	if pipeline.healthCancel != nil {
		t.Error("Health gauge should not be started without cloud exporter")
	}
}

func TestPipeline_HealthGauge_StopsOnStop(t *testing.T) {
	clearTelemetryEnv()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvCloudEnabled, "false")
	t.Setenv(EnvGRPCPort, "54403")
	t.Setenv(EnvHTTPPort, "54404")
	defer clearTelemetryEnv()

	cfg := &Config{
		Enabled:  true,
		GRPCPort: 54403,
		HTTPPort: 54404,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	ctx := context.Background()
	if err := pipeline.Start(ctx); err != nil {
		t.Fatalf("Failed to start pipeline: %v", err)
	}

	// Stop the pipeline and verify healthCancel is cleared
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pipeline.Stop(stopCtx); err != nil {
		t.Fatalf("Failed to stop pipeline: %v", err)
	}

	if pipeline.healthCancel != nil {
		t.Error("healthCancel should be nil after Stop()")
	}
	if pipeline.IsRunning() {
		t.Error("Pipeline should not be running after Stop()")
	}
}

func TestPipeline_ExportErrors_NilCounter(t *testing.T) {
	cfg := &Config{
		Enabled:  true,
		GRPCPort: 54405,
		HTTPPort: 54406,
	}
	pipeline := NewWithConfig(cfg)
	if pipeline == nil {
		t.Fatal("Expected non-nil pipeline")
	}

	// recordExportError should be safe to call with nil counter
	pipeline.recordExportError(context.Background(), "metrics", errors.New("test error"))
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "none",
		},
		{
			name:     "deadline exceeded",
			err:      context.DeadlineExceeded,
			expected: "timeout",
		},
		{
			name:     "context canceled",
			err:      context.Canceled,
			expected: "timeout",
		},
		{
			name:     "wrapped deadline exceeded",
			err:      errors.Join(errors.New("export failed"), context.DeadlineExceeded),
			expected: "timeout",
		},
		{
			name:     "googleapi 401",
			err:      &googleapi.Error{Code: 401, Message: "unauthorized"},
			expected: "auth",
		},
		{
			name:     "googleapi 403",
			err:      &googleapi.Error{Code: 403, Message: "forbidden"},
			expected: "auth",
		},
		{
			name:     "googleapi 429",
			err:      &googleapi.Error{Code: 429, Message: "too many requests"},
			expected: "quota",
		},
		{
			name:     "permission denied string",
			err:      errors.New("rpc error: code = PermissionDenied desc = permission denied"),
			expected: "auth",
		},
		{
			name:     "unauthenticated string",
			err:      errors.New("rpc error: code = Unauthenticated"),
			expected: "auth",
		},
		{
			name:     "quota string",
			err:      errors.New("resource exhausted: quota exceeded"),
			expected: "quota",
		},
		{
			name:     "rate limit string",
			err:      errors.New("rate limit exceeded"),
			expected: "quota",
		},
		{
			name:     "timeout string",
			err:      errors.New("request timeout"),
			expected: "timeout",
		},
		{
			name:     "generic error",
			err:      errors.New("connection refused"),
			expected: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyError(tt.err)
			if result != tt.expected {
				t.Errorf("classifyError(%v) = %q, want %q", tt.err, result, tt.expected)
			}
		})
	}
}
