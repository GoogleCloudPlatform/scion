/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"

	"cloud.google.com/go/logging"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/api/option"
)

// GCPExporter exports telemetry data to GCP using native APIs.
// It uses Cloud Trace for spans and Cloud Logging for logs.
// Metrics are forwarded via the SDK metric exporter (see providers.go).
type GCPExporter struct {
	traceExporter  trace.SpanExporter
	metricExporter sdkmetric.Exporter
	logClient      *logging.Client
	logger         *logging.Logger
	projectID      string
	metricsDebug   bool
}

// NewGCPExporter creates a new GCP-native exporter for traces, metrics, and logs.
func NewGCPExporter(config *Config) (*GCPExporter, error) {
	ctx := context.Background()

	if config.ProjectID == "" {
		return nil, fmt.Errorf("GCP project ID is required (set SCION_GCP_PROJECT_ID or provide credentials file with project_id)")
	}

	opts := []option.ClientOption{}
	if config.GCPCredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(config.GCPCredentialsFile))
	}

	// Create GCP Cloud Trace exporter
	traceOpts := []texporter.Option{
		texporter.WithProjectID(config.ProjectID),
	}
	if len(opts) > 0 {
		traceOpts = append(traceOpts, texporter.WithTraceClientOptions(opts))
	}
	traceExp, err := texporter.New(traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating GCP trace exporter: %w", err)
	}

	// Create Cloud Logging client for log forwarding
	logClient, err := logging.NewClient(ctx, config.ProjectID, opts...)
	if err != nil {
		_ = traceExp.Shutdown(ctx)
		return nil, fmt.Errorf("creating Cloud Logging client: %w", err)
	}

	metricOpts := []mexporter.Option{
		mexporter.WithProjectID(config.ProjectID),
	}
	if len(opts) > 0 {
		metricOpts = append(metricOpts, mexporter.WithMonitoringClientOptions(opts...))
	}
	metricExp, err := mexporter.New(metricOpts...)
	if err != nil {
		_ = traceExp.Shutdown(ctx)
		_ = logClient.Close()
		return nil, fmt.Errorf("creating Cloud Monitoring metric exporter: %w", err)
	}
	var metricExporter sdkmetric.Exporter = metricExp
	if config.MetricsDebug {
		metricExporter = newDebugMetricExporter(metricExporter)
	}

	// Build common labels for agent identification
	commonLabels := map[string]string{}
	if agentID := os.Getenv("SCION_AGENT_ID"); agentID != "" {
		commonLabels["agent_id"] = agentID
	}
	groveID := os.Getenv("SCION_GROVE_ID")
	projectID := os.Getenv("SCION_PROJECT_ID")
	if groveID != "" {
		commonLabels["grove_id"] = groveID
	}
	if projectID != "" {
		commonLabels["project_id"] = projectID
	}
	// Ensure both are set if either is available for transition
	if groveID == "" && projectID != "" {
		commonLabels["grove_id"] = projectID
	} else if projectID == "" && groveID != "" {
		commonLabels["project_id"] = groveID
	}

	var loggerOpts []logging.LoggerOption
	if len(commonLabels) > 0 {
		loggerOpts = append(loggerOpts, logging.CommonLabels(commonLabels))
	}

	return &GCPExporter{
		traceExporter:  traceExp,
		metricExporter: metricExporter,
		logClient:      logClient,
		logger:         logClient.Logger("scion-agents", loggerOpts...),
		projectID:      config.ProjectID,
		metricsDebug:   config.MetricsDebug,
	}, nil
}

// ExportProtoSpans converts OTLP proto spans to SDK ReadOnlySpan and exports
// via the GCP Cloud Trace exporter.
func (e *GCPExporter) ExportProtoSpans(ctx context.Context, resourceSpans []*tracepb.ResourceSpans) error {
	if e == nil || e.traceExporter == nil {
		return nil
	}

	sdkSpans := protoResourceSpansToSDK(resourceSpans)
	if len(sdkSpans) == 0 {
		return nil
	}

	return e.traceExporter.ExportSpans(ctx, sdkSpans)
}

// ExportProtoMetrics converts OTLP proto metrics to SDK metricdata and exports
// them via the Cloud Monitoring exporter.
//
// This is primarily used for harnesses that emit native OTLP metrics to the
// local sciontool receiver. Sciontool's own normalized SDK metrics may still be
// exported directly by a MeterProvider configured in providers.go.
func (e *GCPExporter) ExportProtoMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if e == nil || e.metricExporter == nil {
		return nil
	}

	sdkMetrics := protoResourceMetricsToSDK(resourceMetrics)
	var errs []error

	for i := range sdkMetrics {
		filtered := filterGCPMetricdata(&sdkMetrics[i], e.metricsDebug)
		if filtered == nil {
			continue
		}
		if err := e.metricExporter.Export(ctx, filtered); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// ExportProtoLogs converts OTLP proto log records to Cloud Logging entries.
func (e *GCPExporter) ExportProtoLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	if e == nil || e.logger == nil {
		return nil
	}

	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				entry := protoLogToCloudEntry(lr, rl.Resource)
				e.logger.Log(entry)
			}
		}
	}

	return nil
}

// Shutdown flushes and closes all GCP clients.
func (e *GCPExporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}

	var errs []error

	if e.traceExporter != nil {
		if err := e.traceExporter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("trace exporter shutdown: %w", err))
		}
	}

	if e.metricExporter != nil {
		if err := e.metricExporter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("metric exporter shutdown: %w", err))
		}
	}

	if e.logClient != nil {
		if err := e.logClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("log client close: %w", err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func filterGCPMetricdata(rm *metricdata.ResourceMetrics, metricsDebug bool) *metricdata.ResourceMetrics {
	if rm == nil {
		return nil
	}

	filtered := &metricdata.ResourceMetrics{
		Resource: rm.Resource,
	}

	for _, sm := range rm.ScopeMetrics {
		scopeMetrics := metricdata.ScopeMetrics{Scope: sm.Scope}
		for _, metric := range sm.Metrics {
			if isGCPMetricAggregationSupported(metric.Data) {
				scopeMetrics.Metrics = append(scopeMetrics.Metrics, metric)
				continue
			}
			if metricsDebug {
				log.TaggedInfo("metrics", "dropping unsupported GCP metric %s of type %T", metric.Name, metric.Data)
			}
		}
		if len(scopeMetrics.Metrics) > 0 {
			filtered.ScopeMetrics = append(filtered.ScopeMetrics, scopeMetrics)
		}
	}

	if len(filtered.ScopeMetrics) == 0 {
		return nil
	}
	return filtered
}

func isGCPMetricAggregationSupported(agg metricdata.Aggregation) bool {
	switch agg.(type) {
	case metricdata.Gauge[int64], metricdata.Gauge[float64]:
		return true
	case metricdata.Sum[int64], metricdata.Sum[float64]:
		return true
	case metricdata.Histogram[int64], metricdata.Histogram[float64]:
		return true
	case metricdata.ExponentialHistogram[int64], metricdata.ExponentialHistogram[float64]:
		return true
	default:
		return false
	}
}
