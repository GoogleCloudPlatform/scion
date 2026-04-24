/*
Copyright 2025 The Scion Authors.
*/

package logging

import (
	"context"
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/grpc/credentials"
)

// Environment variable names for OTel logging configuration.
const (
	EnvOTelEndpoint  = telemetry.EnvEndpoint
	EnvOTelInsecure  = telemetry.EnvInsecure
	EnvOTelCAFile    = telemetry.EnvCAFile
	EnvOTelCertFile  = telemetry.EnvCertFile
	EnvOTelKeyFile   = telemetry.EnvKeyFile
	EnvOTelLogEnable = "SCION_OTEL_LOG_ENABLED"
)

// NewLoggerProvider creates an OTel LoggerProvider for the log bridge.
// Returns nil if configuration is missing or invalid.
func NewLoggerProvider(ctx context.Context, config OTelConfig) (log.LoggerProvider, func(), error) {
	if config.Endpoint == "" {
		return nil, func() {}, nil
	}

	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(config.Endpoint),
	}
	if config.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	} else {
		tlsConfig, err := telemetry.LoadOTLPTLSConfig(config.CAFile, config.CertFile, config.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("loading OTLP TLS config: %w", err)
		}
		opts = append(opts, otlploggrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
	}

	// Create the exporter
	exporter, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating OTLP log exporter: %w", err)
	}

	// Create the LoggerProvider
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)

	cleanup := func() {
		if err := provider.Shutdown(ctx); err != nil {
			// Log to stderr since the logger may be shutting down
			fmt.Fprintf(os.Stderr, "error shutting down log provider: %v\n", err)
		}
	}

	return provider, cleanup, nil
}

// InitOTelLogging sets up the full OTel logging pipeline.
// Returns the LoggerProvider, a cleanup function, and any error.
// If OTel logging is not configured, returns nil provider (not an error).
func InitOTelLogging(ctx context.Context, config OTelConfig) (log.LoggerProvider, func(), error) {
	// Check if OTel logging is enabled
	if !isOTelLogEnabled() {
		return nil, func() {}, nil
	}

	// Use environment variables if config is empty
	if config.Endpoint == "" {
		config.Endpoint = os.Getenv(EnvOTelEndpoint)
	}
	if config.Endpoint == "" {
		return nil, func() {}, nil
	}

	if !config.Insecure {
		config.Insecure = os.Getenv(EnvOTelInsecure) == "true"
	}
	if config.CAFile == "" {
		config.CAFile = os.Getenv(EnvOTelCAFile)
	}
	if config.CertFile == "" {
		config.CertFile = os.Getenv(EnvOTelCertFile)
	}
	if config.KeyFile == "" {
		config.KeyFile = os.Getenv(EnvOTelKeyFile)
	}

	return NewLoggerProvider(ctx, config)
}

// isOTelLogEnabled checks if OTel log bridging is enabled.
func isOTelLogEnabled() bool {
	val := os.Getenv(EnvOTelLogEnable)
	if val == "" {
		// Default to enabled if OTEL endpoint is set
		return os.Getenv(EnvOTelEndpoint) != ""
	}
	return val == "true" || val == "1" || val == "yes"
}
