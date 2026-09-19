/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"cloud.google.com/go/logging"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	scionlog "github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
// Metrics are converted at the forwarding boundary and sent through the SDK
// metric exporter after receiver policy processing.
type GCPExporter struct {
	traceExporter   trace.SpanExporter
	metricExporter  sdkmetric.Exporter
	logClient       *logging.Client
	logger          *logging.Logger
	logSink         func(logging.Entry) // optional local capture after conversion
	projectID       string
	metricsDebug    bool
	asyncLogErrors  atomic.Int64
	onAsyncLogError func(error)
	logSlotOnce     sync.Once
	logSlot         chan struct{} // one Logger/Client Flush outcome per pipeline batch
}

// NewGCPExporter creates a new GCP-native exporter for traces, metrics, and logs.
func NewGCPExporter(config *Config) (*GCPExporter, error) {
	ctx := context.Background()

	if config.ProjectID == "" {
		return nil, fmt.Errorf("GCP project ID is required (set SCION_GCP_PROJECT_ID or provide credentials file with project_id)")
	}

	opts := []option.ClientOption{}
	if config.GCPCredentialsFile != "" {
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, config.GCPCredentialsFile))
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
	var metricExporter = metricExp
	if config.MetricsDebug {
		metricExporter = newDebugMetricExporter(metricExporter)
	}

	// Build common labels for agent identification.
	// Most identifiers are carried as OTel resource attributes (see providers.go);
	// only hub has no resource-attribute equivalent.
	commonLabels := map[string]string{}
	if hubName := os.Getenv("SCION_HUB_NAME"); hubName != "" {
		commonLabels["hub"] = hubName
	}

	var loggerOpts []logging.LoggerOption
	if len(commonLabels) > 0 {
		loggerOpts = append(loggerOpts, logging.CommonLabels(commonLabels))
	}

	exporter := &GCPExporter{
		traceExporter:  traceExp,
		metricExporter: metricExporter,
		logClient:      logClient,
		logger:         logClient.Logger(scionlog.AgentLogID, loggerOpts...),
		projectID:      config.ProjectID,
		metricsDebug:   config.MetricsDebug,
	}
	logClient.OnError = func(err error) {
		exporter.reportAsyncLogError(err)
	}
	return exporter, nil
}

func (e *GCPExporter) reportAsyncLogError(err error) {
	e.asyncLogErrors.Add(1)
	if e.onAsyncLogError != nil {
		e.onAsyncLogError(err)
	}
	log.Error("Cloud Logging asynchronous delivery failed: %v", err)
}

func (e *GCPExporter) acquireLogSlot(ctx context.Context) error {
	e.logSlotOnce.Do(func() { e.logSlot = make(chan struct{}, 1) })
	select {
	case e.logSlot <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-e.logSlot
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExportProtoSpans converts OTLP proto spans to SDK ReadOnlySpan and exports
// via the GCP Cloud Trace exporter.
func (e *GCPExporter) ExportProtoSpans(ctx context.Context, resourceSpans []*tracepb.ResourceSpans) error {
	if e == nil || e.traceExporter == nil {
		return errors.New("GCP trace exporter unavailable")
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
// Both native and normalized SDK metrics reach this path through the local
// sciontool receiver.
func (e *GCPExporter) ExportProtoMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if e == nil || e.metricExporter == nil {
		return errors.New("GCP metric exporter unavailable")
	}

	identified, err := gcpIdentityMetrics(resourceMetrics)
	if err != nil {
		return err
	}
	sdkMetrics := protoResourceMetricsToSDK(identified)
	var errs []error
	succeeded := 0

	for i := range sdkMetrics {
		filtered := filterGCPMetricdata(&sdkMetrics[i], e.metricsDebug)
		if filtered == nil {
			continue
		}
		if err := e.metricExporter.Export(ctx, filtered); err != nil {
			errs = append(errs, err)
		} else {
			succeeded++
		}
	}
	if succeeded > 0 && len(errs) > 0 {
		return &partialSuccessError{message: fmt.Sprintf("GCP metric batch partly delivered: %d resource groups succeeded, %d failed", succeeded, len(errs))}
	}
	return errors.Join(errs...)
}

// ExportProtoLogs converts OTLP proto log records to Cloud Logging entries.
func (e *GCPExporter) ExportProtoLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	if e == nil || (e.logger == nil && e.logSink == nil) {
		return errors.New("GCP log exporter unavailable")
	}
	if err := e.acquireLogSlot(ctx); err != nil {
		return err
	}
	defer func() { <-e.logSlot }()

	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				entry := protoLogToCloudEntry(lr, rl.Resource, rl.SchemaUrl, sl.Scope, sl.SchemaUrl)
				if e.logSink != nil {
					e.logSink(entry)
				} else {
					e.logger.Log(entry)
				}
			}
		}
	}
	if e.logger != nil && e.logSink == nil {
		if err := e.logger.Flush(); err != nil {
			return &partialSuccessError{message: fmt.Sprintf("Cloud Logging flush failed with unknown per-record outcome: %v", err)}
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
		if err := e.acquireLogSlot(ctx); err != nil {
			return fmt.Errorf("log client close incomplete: %w", err)
		}
		if err := e.logClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("log client close: %w", err))
		}
		<-e.logSlot
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
