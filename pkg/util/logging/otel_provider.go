/*
Copyright 2025 The Scion Authors.
*/

package logging

import (
	"context"
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/otlptls"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Environment variable names for OTel logging configuration. The TLS and
// header variables are the ones the agent-side exporter reads
// (pkg/sciontool/telemetry/config.go), so the server log bridge and the
// agent exporter take the same settings.
const (
	EnvOTelEndpoint      = "SCION_OTEL_ENDPOINT"
	EnvOTelInsecure      = "SCION_OTEL_INSECURE"
	EnvOTelLogEnable     = "SCION_OTEL_LOG_ENABLED"
	EnvOTelSkipTLSVerify = "SCION_OTEL_SKIP_TLS_VERIFY"
	EnvOTelCAFile        = "SCION_OTEL_CA_FILE"
	EnvOTelCertFile      = "SCION_OTEL_CERT_FILE"
	EnvOTelKeyFile       = "SCION_OTEL_KEY_FILE"
	EnvOTelHeaders       = "SCION_OTEL_HEADERS"
)

// NewLoggerProvider creates an OTel LoggerProvider for the log bridge.
// Returns nil if configuration is missing or invalid.
//
// TLS is built like the agent exporter's (otlptls.LoadClientConfig): the CA
// bundle, the client certificate and key (reloaded on each new handshake,
// so a rotated certificate is used by the next connection), and
// SkipTLSVerify. Plaintext together with any TLS option is an error.
func NewLoggerProvider(ctx context.Context, config OTelConfig) (log.LoggerProvider, func(), error) {
	if config.Endpoint == "" {
		return nil, func() {}, nil
	}

	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(config.Endpoint)}
	if config.Insecure {
		if config.SkipTLSVerify || config.CAFile != "" || config.CertFile != "" || config.KeyFile != "" {
			return nil, nil, fmt.Errorf("plaintext OTLP transport conflicts with TLS verification or client certificate options")
		}
		opts = append(opts, otlploggrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	} else if config.SkipTLSVerify || config.CAFile != "" || config.CertFile != "" || config.KeyFile != "" {
		// With no TLS option set the exporter keeps its default TLS setup
		// (system roots, and the standard OTEL_EXPORTER_OTLP_* variables).
		tlsConfig, err := otlptls.LoadClientConfig(config.CAFile, config.CertFile, config.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load OTLP TLS config: %w", err)
		}
		tlsConfig.InsecureSkipVerify = config.SkipTLSVerify //nolint:gosec // explicit operator configuration
		opts = append(opts, otlploggrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
	}
	if len(config.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(config.Headers))
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
// Fields left empty in config are read from the SCION_OTEL_* variables.
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
		config.Insecure = util.ParseBoolEnv(EnvOTelInsecure, false)
	}
	if !config.SkipTLSVerify {
		config.SkipTLSVerify = util.ParseBoolEnv(EnvOTelSkipTLSVerify, false)
	}
	if config.CAFile == "" {
		config.CAFile = os.Getenv(EnvOTelCAFile)
	}
	if config.CertFile == "" && config.KeyFile == "" {
		config.CertFile = os.Getenv(EnvOTelCertFile)
		config.KeyFile = os.Getenv(EnvOTelKeyFile)
	}
	if config.Headers == nil {
		headers, err := otlptls.ParseHeaders(os.Getenv(EnvOTelHeaders))
		if err != nil {
			// The logger is not set up yet; a bad value only drops the
			// headers, it does not disable log export.
			fmt.Fprintf(os.Stderr, "warning: ignoring %s: %v\n", EnvOTelHeaders, err)
		}
		config.Headers = headers
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
