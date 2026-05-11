/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"os"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/api/option"
)

// Providers holds SDK TracerProvider, LoggerProvider, and MeterProvider for OTel export.
// All providers share the same OTLP endpoint and resource attributes.
type Providers struct {
	TracerProvider *trace.TracerProvider
	LoggerProvider *log.LoggerProvider
	MeterProvider  *metric.MeterProvider
}

// NewProviders creates real SDK providers that export to the configured backend.
// When provider=gcp, uses GCP-native trace exporter and OTLP for logs/metrics.
// Otherwise, uses standard OTLP gRPC exporters for all signals.
//
// The batch parameter controls processor mode:
//   - batch=false uses synchronous processors (for short-lived hook commands)
//   - batch=true uses batching processors (for long-lived init commands)
func NewProviders(ctx context.Context, config *Config, batch bool) (*Providers, error) {
	if config == nil || !config.Enabled || !config.IsCloudConfigured() {
		return nil, nil
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, err
	}

	if config.IsGCP() {
		return newGCPProviders(ctx, config, res, batch)
	}

	return newOTLPProviders(ctx, config, res, batch)
}

// buildResource creates the OTel resource with service name and agent identifiers.
func buildResource(ctx context.Context) (*resource.Resource, error) {
	attrs := []resource.Option{
		resource.WithAttributes(semconv.ServiceName("sciontool")),
	}
	if agentID := os.Getenv("SCION_AGENT_ID"); agentID != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.ServiceInstanceID(agentID)))
	}
	groveID := os.Getenv("SCION_GROVE_ID")
	projectID := os.Getenv("SCION_PROJECT_ID")
	if groveID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.grove.id", groveID),
		))
	}
	if projectID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.project.id", projectID),
		))
	}
	// Ensure both are set if either is available for transition
	if groveID == "" && projectID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.grove.id", projectID),
		))
	} else if projectID == "" && groveID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.project.id", groveID),
		))
	}
	if harness := os.Getenv("SCION_HARNESS"); harness != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.harness", harness),
		))
	}
	if model := os.Getenv("SCION_MODEL"); model != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.model", model),
		))
	}
	if broker := os.Getenv("SCION_BROKER_NAME"); broker != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.broker", broker),
		))
	}
	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}
	return res, nil
}

// newGCPProviders creates providers using GCP-native exporters.
// Traces use the Cloud Trace exporter directly.
// Logs use OTLP to the local receiver (pipeline handles Cloud Logging forwarding).
// Metrics use the Cloud Monitoring exporter directly.
func newGCPProviders(ctx context.Context, config *Config, res *resource.Resource, batch bool) (*Providers, error) {
	clientOpts := []option.ClientOption{}
	if config.GCPCredentialsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(config.GCPCredentialsFile))
	}

	// GCP Cloud Trace exporter
	traceOpts := []texporter.Option{
		texporter.WithProjectID(config.ProjectID),
	}
	if len(clientOpts) > 0 {
		traceOpts = append(traceOpts, texporter.WithTraceClientOptions(clientOpts))
	}
	traceExporter, err := texporter.New(traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating GCP trace exporter: %w", err)
	}

	// Logs export to the local OTLP receiver (pipeline forwards to Cloud Logging)
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(fmt.Sprintf("localhost:%d", config.GRPCPort)),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating log exporter: %w", err)
	}

	// Metrics use the GCP Cloud Monitoring exporter (direct to Cloud Monitoring API)
	metricOpts := []mexporter.Option{
		mexporter.WithProjectID(config.ProjectID),
	}
	if len(clientOpts) > 0 {
		metricOpts = append(metricOpts, mexporter.WithMonitoringClientOptions(clientOpts...))
	}
	rawMetricExporter, err := mexporter.New(metricOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = logExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating GCP metric exporter: %w", err)
	}
	var metricExporter metric.Exporter = rawMetricExporter
	if config.MetricsDebug {
		metricExporter = newDebugMetricExporter(metricExporter)
	}

	return buildProviders(res, traceExporter, logExporter, metricExporter, batch), nil
}

// newOTLPProviders creates providers using standard OTLP gRPC exporters.
func newOTLPProviders(ctx context.Context, config *Config, res *resource.Resource, batch bool) (*Providers, error) {
	gcpDialOpts, err := loadSecureGCPDialOptions(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("loading GCP credentials: %w", err)
	}

	// Create trace exporter (gRPC)
	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(config.Endpoint),
	}
	traceOpts, err = appendOTLPTraceGRPCSecurityOption(traceOpts, config)
	if err != nil {
		return nil, fmt.Errorf("loading OTLP TLS config: %w", err)
	}
	for _, do := range gcpDialOpts {
		traceOpts = append(traceOpts, otlptracegrpc.WithDialOption(do))
	}
	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	// Create log exporter (gRPC)
	logOpts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(config.Endpoint),
	}
	logOpts, err = appendOTLPLogGRPCSecurityOption(logOpts, config)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("loading OTLP TLS config: %w", err)
	}
	for _, do := range gcpDialOpts {
		logOpts = append(logOpts, otlploggrpc.WithDialOption(do))
	}
	logExporter, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating log exporter: %w", err)
	}

	// Create metric exporter (gRPC)
	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(config.Endpoint),
	}
	metricOpts, err = appendOTLPMetricGRPCSecurityOption(metricOpts, config)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = logExporter.Shutdown(ctx)
		return nil, fmt.Errorf("loading OTLP TLS config: %w", err)
	}
	for _, do := range gcpDialOpts {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithDialOption(do))
	}
	rawMetricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = logExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating metric exporter: %w", err)
	}
	var metricExporter metric.Exporter = rawMetricExporter
	if config.MetricsDebug {
		metricExporter = newDebugMetricExporter(metricExporter)
	}

	return buildProviders(res, traceExporter, logExporter, metricExporter, batch), nil
}

// buildProviders constructs TracerProvider, LoggerProvider, and MeterProvider
// from the given exporters, using either batch or sync processing.
func buildProviders(res *resource.Resource, traceExp trace.SpanExporter, logExp log.Exporter, metricExp metric.Exporter, batch bool) *Providers {
	if !batch {
		return &Providers{
			TracerProvider: trace.NewTracerProvider(
				trace.WithResource(res),
				trace.WithSyncer(traceExp),
			),
			LoggerProvider: log.NewLoggerProvider(
				log.WithResource(res),
				log.WithProcessor(log.NewSimpleProcessor(logExp)),
			),
			MeterProvider: metric.NewMeterProvider(
				metric.WithResource(res),
				metric.WithReader(metric.NewPeriodicReader(metricExp)),
			),
		}
	}

	return &Providers{
		TracerProvider: trace.NewTracerProvider(
			trace.WithResource(res),
			trace.WithBatcher(traceExp),
		),
		LoggerProvider: log.NewLoggerProvider(
			log.WithResource(res),
			log.WithProcessor(log.NewBatchProcessor(logExp)),
		),
		MeterProvider: metric.NewMeterProvider(
			metric.WithResource(res),
			metric.WithReader(metric.NewPeriodicReader(metricExp)),
		),
	}
}

// Shutdown flushes and shuts down all providers.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}

	var firstErr error
	if p.TracerProvider != nil {
		if err := p.TracerProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if p.LoggerProvider != nil {
		if err := p.LoggerProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if p.MeterProvider != nil {
		if err := p.MeterProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
