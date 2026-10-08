/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Providers holds SDK TracerProvider, LoggerProvider, and MeterProvider for OTel export.
// All providers share the same OTLP endpoint and resource attributes.
type Providers struct {
	TracerProvider *trace.TracerProvider
	LoggerProvider *log.LoggerProvider
	MeterProvider  *metric.MeterProvider
}

// NewProviders creates SDK providers that export every producer signal to the
// container's loopback receiver. Cloud credentials and external endpoints are
// deliberately used only by the long-lived forwarding pipeline.
//
// The batch parameter controls processor mode:
//   - batch=false uses synchronous processors (for short-lived hook commands)
//   - batch=true uses batching processors (for long-lived init commands)
func NewProviders(ctx context.Context, config *Config, batch bool) (*Providers, error) {
	if config == nil || !config.Enabled {
		return nil, nil
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, err
	}

	return newLoopbackProviders(ctx, config, res, batch, loopbackExportBounds{})
}

// Bounds for the per-invocation `sciontool hook` providers.
//
// A hook runs synchronously inside the harness's tool loop, and some harnesses
// kill a hook (and fail the tool call) after a short timeout: antigravity uses
// 10s, grok-build 5s. The hook exports only to the loopback receiver in the
// same container, which answers a healthy export in a few milliseconds, so the
// OTLP defaults (10s per export, retries on Unavailable) only matter when the
// receiver is missing or wedged. In that case the export cannot succeed and
// waiting just stalls the harness. These bounds keep a hook with no receiver
// well under one second end to end while leaving a wide margin for a live
// receiver on a loaded host.
const (
	// HookExportTimeout bounds a single OTLP export (one span, one log
	// record, or the final metric collection). About 50x a typical loopback
	// round trip, including the first connection's HTTP/2 handshake.
	HookExportTimeout = 250 * time.Millisecond

	// HookShutdownTimeout bounds the provider shutdown that flushes the hook's
	// metrics. A hook performs at most a span export and a log export before
	// shutdown, so with a receiver that accepts connections but never answers
	// the worst case is about 2*HookExportTimeout + HookShutdownTimeout
	// (0.75s). With nothing listening the exports fail immediately because
	// retries are disabled.
	HookShutdownTimeout = 250 * time.Millisecond
)

// NewHookProviders creates synchronous loopback providers for a short-lived
// `sciontool hook` invocation. They behave like NewProviders(ctx, config,
// false), except that every exporter uses HookExportTimeout and does not
// retry, so a missing receiver costs milliseconds instead of the OTLP default
// 10s per export. Callers should shut the providers down with a context bounded
// by HookShutdownTimeout. A failed export is reported to the OTel global error
// handler only; it is never retried.
func NewHookProviders(ctx context.Context, config *Config) (*Providers, error) {
	if config == nil || !config.Enabled {
		return nil, nil
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, err
	}

	return newLoopbackProviders(ctx, config, res, false, loopbackExportBounds{
		timeout:      HookExportTimeout,
		disableRetry: true,
	})
}

// loopbackExportBounds overrides the OTLP exporter timeout and retry
// behaviour. The zero value keeps the OTLP defaults.
type loopbackExportBounds struct {
	timeout      time.Duration
	disableRetry bool
}

// buildResource creates the OTel resource with service name and agent identifiers.
func buildResource(ctx context.Context) (*resource.Resource, error) {
	attrs := []resource.Option{
		resource.WithAttributes(semconv.ServiceName("sciontool")),
	}
	if agentID := os.Getenv("SCION_AGENT_ID"); agentID != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.ServiceInstanceID(agentID)))
		attrs = append(attrs, resource.WithAttributes(attribute.String("scion.agent.id", agentID)))
	}
	if agentSlug := os.Getenv("SCION_AGENT_SLUG"); agentSlug != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.agent.slug", agentSlug),
		))
	}
	projectID := projectkeys.ProjectIDFromEnv(os.Getenv)
	if projectID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("scion.project.id", projectID),
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
			attribute.String("scion.broker.name", broker),
		))
	}
	if gcpProjectID := os.Getenv(EnvProjectID); gcpProjectID != "" {
		attrs = append(attrs, resource.WithAttributes(
			attribute.String("gcp.project_id", gcpProjectID),
		))
	}
	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}
	return res, nil
}

// newLoopbackProviders creates standard OTLP gRPC exporters fixed to loopback.
func newLoopbackProviders(ctx context.Context, config *Config, res *resource.Resource, batch bool, bounds loopbackExportBounds) (*Providers, error) {
	endpoint := loopbackEndpoint(config)
	pinResource := loopbackResourceDialOption(res)
	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithDialOption(pinResource),
	}
	if bounds.timeout > 0 {
		traceOpts = append(traceOpts, otlptracegrpc.WithTimeout(bounds.timeout))
	}
	if bounds.disableRetry {
		traceOpts = append(traceOpts, otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}))
	}
	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	// Create log exporter (gRPC)
	logOpts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithDialOption(pinResource),
	}
	if bounds.timeout > 0 {
		logOpts = append(logOpts, otlploggrpc.WithTimeout(bounds.timeout))
	}
	if bounds.disableRetry {
		logOpts = append(logOpts, otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}))
	}
	logExporter, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating log exporter: %w", err)
	}

	// Create metric exporter (gRPC)
	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithDialOption(pinResource),
	}
	if bounds.timeout > 0 {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithTimeout(bounds.timeout))
	}
	if bounds.disableRetry {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}))
	}
	// Hook commands are short lived independent writers. Export their counter
	// additions as deltas so the receiver can accumulate them once.
	if !batch {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithTemporalitySelector(func(kind metric.InstrumentKind) metricdata.Temporality {
			if kind == metric.InstrumentKindCounter {
				return metricdata.DeltaTemporality
			}
			return metricdata.CumulativeTemporality
		}))
	}
	rawMetricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = logExporter.Shutdown(ctx)
		return nil, fmt.Errorf("creating metric exporter: %w", err)
	}
	return buildProviders(res, traceExporter, logExporter, rawMetricExporter, batch), nil
}

func loopbackEndpoint(config *Config) string {
	return fmt.Sprintf("127.0.0.1:%d", config.GRPCPort)
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
