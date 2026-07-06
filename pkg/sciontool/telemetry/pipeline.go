/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/api/googleapi"
)

// metricFlushInterval is the minimum interval between metric exports to Cloud
// Monitoring. This prevents sampling-rate violations when multiple short-lived
// processes (hooks) send metrics in rapid succession.
const metricFlushInterval = 15 * time.Second

// Pipeline orchestrates the telemetry collection and forwarding.
type Pipeline struct {
	config       *Config
	receiver     *Receiver
	exporter     *CloudExporter
	filter       *Filter
	mu           sync.Mutex
	running      bool
	healthCancel context.CancelFunc
	exportErrors otelmetric.Int64Counter
	meter        otelmetric.Meter

	metricsDropWarned sync.Once
	logsDropWarned    sync.Once
	spansDropWarned   sync.Once

	metricBuf       []*metricpb.ResourceMetrics
	metricBufMu     sync.Mutex
	metricFlushCtx  context.Context
	metricFlushCnl  context.CancelFunc
	metricLastFlush time.Time
	metricFlushWg   sync.WaitGroup
}

// New creates a new telemetry pipeline.
// Returns nil if telemetry is not enabled.
func New() *Pipeline {
	config := LoadConfig()
	if !config.Enabled {
		return nil
	}
	return &Pipeline{
		config: config,
		filter: NewFilter(config.Filter),
	}
}

// NewWithConfig creates a new telemetry pipeline with explicit configuration.
func NewWithConfig(config *Config) *Pipeline {
	if config == nil || !config.Enabled {
		return nil
	}
	return &Pipeline{
		config: config,
		filter: NewFilter(config.Filter),
	}
}

// Start starts the telemetry pipeline.
func (p *Pipeline) Start(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("pipeline already running")
	}

	// Log credential resolution for diagnostics
	if p.config.GCPCredentialsFile != "" {
		source := "env"
		if envVal := os.Getenv(EnvGCPCredentials); envVal == "" {
			source = "well-known-path"
		}
		slog.Info("telemetry pipeline credential resolution",
			"credentials_file", p.config.GCPCredentialsFile,
			"source", source,
			"project_id", p.config.ProjectID,
			"provider", p.config.CloudProvider,
			"cloud_configured", p.config.IsCloudConfigured(),
		)
	} else if p.config.IsCloudConfigured() {
		slog.Info("telemetry pipeline credential resolution",
			"credentials_file", "",
			"source", "adc",
			"project_id", p.config.ProjectID,
			"provider", p.config.CloudProvider,
			"cloud_configured", true,
		)
	}

	// Create cloud exporter if configured
	if p.config.IsCloudConfigured() {
		exporter, err := NewCloudExporter(ctx, p.config)
		if err != nil {
			log.Error("Failed to create cloud exporter: %v", err)
			// Continue without cloud export - receiver can still work for local debugging
		} else {
			p.exporter = exporter
			mode := "OTLP"
			if p.config.IsGCP() {
				mode = "GCP-native"
				if p.config.Endpoint != "" {
					log.Info("Cloud endpoint %q is ignored in GCP-native mode (SDKs use built-in endpoints)", p.config.Endpoint)
				}
			}
			log.Info("Cloud exporter initialized (%s, project: %s)", mode, p.config.ProjectID)
		}
	} else {
		slog.Warn("telemetry cloud export not configured",
			"reason", "no credentials or endpoint",
			"env_checked", EnvGCPCredentials,
			"well_known_path", WellKnownGCPCredentialsPath,
		)
	}

	// Create receiver with span and metric handlers
	p.receiver = NewReceiver(p.config, p.handleSpans, WithMetricHandler(p.handleMetrics), WithLogHandler(p.handleLogs))

	// Start receiver
	if err := p.receiver.Start(ctx); err != nil {
		if p.exporter != nil {
			_ = p.exporter.Shutdown(ctx)
		}
		return fmt.Errorf("failed to start receiver: %w", err)
	}

	p.running = true

	// Start metric flush goroutine for batching exports to Cloud Monitoring.
	if p.exporter != nil {
		p.metricFlushCtx, p.metricFlushCnl = context.WithCancel(ctx)
		p.metricFlushWg.Add(1)
		go func() {
			defer p.metricFlushWg.Done()
			p.metricFlushLoop()
		}()
	}

	// Register pipeline health gauge and export error counter.
	if p.config.IsCloudConfigured() && p.exporter != nil {
		p.initSelfMetrics(ctx)
	}

	log.Info("Telemetry pipeline started (gRPC: %d, HTTP: %d)", p.config.GRPCPort, p.config.HTTPPort)

	return nil
}

// Stop stops the telemetry pipeline.
func (p *Pipeline) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running {
		return nil
	}

	var errs []error

	// Stop health gauge ticker
	if p.healthCancel != nil {
		p.healthCancel()
		p.healthCancel = nil
	}

	// Stop metric flush goroutine and drain remaining buffered metrics.
	if p.metricFlushCnl != nil {
		p.metricFlushCnl()
		p.metricFlushCnl = nil
	}
	p.metricFlushWg.Wait()
	p.flushMetricBuffer(ctx, true)

	// Stop receiver first
	if p.receiver != nil {
		if err := p.receiver.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("receiver stop error: %w", err))
		}
	}

	// Shutdown exporter to flush any buffered spans
	if p.exporter != nil {
		if err := p.exporter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("exporter shutdown error: %w", err))
		}
	}

	p.running = false
	log.Info("Telemetry pipeline stopped")

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// IsRunning returns true if the pipeline is running.
func (p *Pipeline) IsRunning() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Config returns the pipeline configuration.
func (p *Pipeline) Config() *Config {
	if p == nil {
		return nil
	}
	return p.config
}

// handleSpans processes incoming spans from the receiver.
func (p *Pipeline) handleSpans(ctx context.Context, resourceSpans []*tracepb.ResourceSpans) error {
	// Filter spans based on name/event type
	filtered := p.filterSpans(resourceSpans)
	if len(filtered) == 0 {
		return nil
	}

	// Count total spans for logging
	spanCount := 0
	for _, rs := range filtered {
		for _, ss := range rs.ScopeSpans {
			spanCount += len(ss.Spans)
		}
	}

	// Forward to cloud exporter if available
	if p.exporter != nil {
		if err := p.exporter.ExportProtoSpans(ctx, filtered); err != nil {
			p.recordExportError(ctx, "spans", err)
			log.Error("Failed to export spans to cloud: %v", err)
			return err
		}
		log.Debug("Exported %d spans to cloud", spanCount)
	} else {
		p.spansDropWarned.Do(func() {
			log.Error("Received %d spans but cloud exporter is not configured — spans will be dropped. Set SCION_GCP_PROJECT_ID or configure telemetry.cloud", spanCount)
		})
	}

	return nil
}

// filterSpans applies the filter to resource spans.
func (p *Pipeline) filterSpans(resourceSpans []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	if p.filter == nil {
		return resourceSpans
	}

	result := make([]*tracepb.ResourceSpans, 0, len(resourceSpans))
	for _, rs := range resourceSpans {
		filteredRS := &tracepb.ResourceSpans{
			Resource:   rs.Resource,
			ScopeSpans: make([]*tracepb.ScopeSpans, 0, len(rs.ScopeSpans)),
			SchemaUrl:  rs.SchemaUrl,
		}

		for _, ss := range rs.ScopeSpans {
			filteredSS := &tracepb.ScopeSpans{
				Scope:     ss.Scope,
				Spans:     make([]*tracepb.Span, 0, len(ss.Spans)),
				SchemaUrl: ss.SchemaUrl,
			}

			for _, span := range ss.Spans {
				if p.filter.ShouldProcessSpan(span.Name) {
					filteredSS.Spans = append(filteredSS.Spans, span)
				}
			}

			if len(filteredSS.Spans) > 0 {
				filteredRS.ScopeSpans = append(filteredRS.ScopeSpans, filteredSS)
			}
		}

		if len(filteredRS.ScopeSpans) > 0 {
			result = append(result, filteredRS)
		}
	}

	return result
}

// handleMetrics buffers incoming metrics for periodic export to Cloud Monitoring.
// Metrics are accumulated and flushed at metricFlushInterval to avoid
// sampling-rate violations from rapid writes (e.g. multiple hook processes).
func (p *Pipeline) handleMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if len(resourceMetrics) == 0 {
		return nil
	}

	if p.exporter != nil {
		p.metricBufMu.Lock()
		p.metricBuf = append(p.metricBuf, resourceMetrics...)
		p.metricBufMu.Unlock()
		log.Debug("Buffered %d resource metric batches for export", len(resourceMetrics))
	} else {
		metricCount := 0
		for _, rm := range resourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				metricCount += len(sm.Metrics)
			}
		}
		p.metricsDropWarned.Do(func() {
			log.Error("Received %d metrics but cloud exporter is not configured — metrics will be dropped. Set SCION_GCP_PROJECT_ID or configure telemetry.cloud", metricCount)
		})
	}

	return nil
}

// metricFlushLoop periodically flushes buffered metrics to Cloud Monitoring.
func (p *Pipeline) metricFlushLoop() {
	ticker := time.NewTicker(metricFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.metricFlushCtx.Done():
			return
		case <-ticker.C:
			p.flushMetricBuffer(p.metricFlushCtx, false)
		}
	}
}

// flushMetricBuffer deduplicates and exports buffered metrics to Cloud Monitoring.
// For cumulative metrics from short-lived hook processes, multiple data points may
// exist for the same metric+attributes combination. Only the latest data point is
// kept to avoid Cloud Monitoring sampling-rate violations.
//
// Cloud Monitoring requires a minimum 10-second interval between writes for the
// same time series. This method enforces metricFlushInterval between exports to
// prevent rapid consecutive flushes (e.g. periodic tick followed by shutdown drain)
// from triggering sampling-rate rejections. Pass force=true during shutdown to
// bypass the interval check and drain all remaining buffered metrics.
func (p *Pipeline) flushMetricBuffer(ctx context.Context, force bool) {
	p.metricBufMu.Lock()
	buf := p.metricBuf
	sinceLastFlush := time.Since(p.metricLastFlush)
	if len(buf) > 0 && !force && sinceLastFlush < metricFlushInterval {
		p.metricBufMu.Unlock()
		log.Debug("Skipping metric flush — last export was %v ago (minimum %v)", sinceLastFlush.Round(time.Millisecond), metricFlushInterval)
		return
	}
	p.metricBuf = nil
	p.metricBufMu.Unlock()

	if len(buf) == 0 || p.exporter == nil {
		return
	}

	deduped := deduplicateMetrics(buf)

	metricCount := 0
	for _, rm := range deduped {
		for _, sm := range rm.ScopeMetrics {
			metricCount += len(sm.Metrics)
		}
	}

	if err := p.exporter.ExportProtoMetrics(ctx, deduped); err != nil {
		p.recordExportError(ctx, "metrics", err)
		log.Error("Failed to export %d buffered metrics to cloud: %v", metricCount, err)
		return
	}
	p.metricBufMu.Lock()
	p.metricLastFlush = time.Now()
	p.metricBufMu.Unlock()
	log.Debug("Exported %d buffered metrics to cloud", metricCount)
}

// deduplicateMetrics merges multiple ResourceMetrics into one, keeping only the
// latest data point per (metric name, attribute set) for Sum metrics. This
// prevents Cloud Monitoring sampling-rate violations when multiple hook processes
// report the same cumulative counter within a short window.
func deduplicateMetrics(rms []*metricpb.ResourceMetrics) []*metricpb.ResourceMetrics {
	if len(rms) <= 1 {
		return rms
	}

	// Flatten all metrics into a single ResourceMetrics, deduplicating data points.
	// Key: "scope/metricname" → Metric with deduplicated data points.
	type metricKey struct {
		scope  string
		metric string
	}
	latest := make(map[metricKey]*metricpb.Metric)

	var resource *metricpb.ResourceMetrics
	for _, rm := range rms {
		if rm == nil {
			continue
		}
		if resource == nil {
			resource = rm
		}
		for _, sm := range rm.ScopeMetrics {
			scopeName := ""
			if sm.Scope != nil {
				scopeName = sm.Scope.Name
			}
			for _, m := range sm.Metrics {
				key := metricKey{scope: scopeName, metric: m.Name}
				existing, ok := latest[key]
				if !ok {
					latest[key] = m
					continue
				}
				// For Sum metrics, keep only the data point with the latest timestamp
				// per attribute set. For other types, keep the latest metric entirely.
				merged := mergeMetricDataPoints(existing, m)
				latest[key] = merged
			}
		}
	}

	if resource == nil || len(latest) == 0 {
		return nil
	}

	// Rebuild scope→metrics structure.
	scopeMetrics := make(map[string][]*metricpb.Metric)
	for key, m := range latest {
		scopeMetrics[key.scope] = append(scopeMetrics[key.scope], m)
	}
	sms := make([]*metricpb.ScopeMetrics, 0, len(scopeMetrics))
	for _, metrics := range scopeMetrics {
		sms = append(sms, &metricpb.ScopeMetrics{Metrics: metrics})
	}
	return []*metricpb.ResourceMetrics{{
		Resource:     resource.Resource,
		ScopeMetrics: sms,
	}}
}

// mergeMetricDataPoints merges two proto metrics with the same name, keeping
// the latest data point per attribute set for Sum types.
func mergeMetricDataPoints(a, b *metricpb.Metric) *metricpb.Metric {
	aSum, aOK := a.Data.(*metricpb.Metric_Sum)
	bSum, bOK := b.Data.(*metricpb.Metric_Sum)
	if !aOK || !bOK {
		// For non-Sum metrics, keep the one with the latest timestamp
		return b
	}

	// Dedup by attribute set: keep the data point with the latest TimeUnixNano.
	type attrKey string
	pointMap := make(map[attrKey]*metricpb.NumberDataPoint)
	for _, dp := range aSum.Sum.DataPoints {
		key := attrKey(attrSetKey(dp.Attributes))
		pointMap[key] = dp
	}
	for _, dp := range bSum.Sum.DataPoints {
		key := attrKey(attrSetKey(dp.Attributes))
		existing, ok := pointMap[key]
		if !ok || dp.TimeUnixNano > existing.TimeUnixNano {
			pointMap[key] = dp
		}
	}

	merged := make([]*metricpb.NumberDataPoint, 0, len(pointMap))
	for _, dp := range pointMap {
		merged = append(merged, dp)
	}
	return &metricpb.Metric{
		Name:        a.Name,
		Description: a.Description,
		Unit:        a.Unit,
		Data: &metricpb.Metric_Sum{
			Sum: &metricpb.Sum{
				DataPoints:             merged,
				AggregationTemporality: aSum.Sum.AggregationTemporality,
				IsMonotonic:            aSum.Sum.IsMonotonic,
			},
		},
	}
}

// attrSetKey creates a stable string key from a list of proto attributes.
// Attributes are copied and sorted by key to ensure a stable, order-independent key.
func attrSetKey(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return ""
	}
	sorted := make([]*commonpb.KeyValue, len(attrs))
	copy(sorted, attrs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Key < sorted[j].Key
	})

	var b strings.Builder
	for i, kv := range sorted {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(kv.Key)
		b.WriteByte('=')
		if kv.Value != nil {
			switch v := kv.Value.GetValue().(type) {
			case *commonpb.AnyValue_StringValue:
				b.WriteString(v.StringValue)
			case *commonpb.AnyValue_BoolValue:
				b.WriteString(strconv.FormatBool(v.BoolValue))
			case *commonpb.AnyValue_IntValue:
				b.WriteString(strconv.FormatInt(v.IntValue, 10))
			case *commonpb.AnyValue_DoubleValue:
				b.WriteString(strconv.FormatFloat(v.DoubleValue, 'f', -1, 64))
			}
		}
	}
	return b.String()
}

// handleLogs processes incoming logs from the receiver.
func (p *Pipeline) handleLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	if len(resourceLogs) == 0 {
		return nil
	}

	// Count total log records for logging
	logCount := 0
	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			logCount += len(sl.LogRecords)
		}
	}

	// Forward to cloud exporter if available
	if p.exporter != nil {
		if err := p.exporter.ExportProtoLogs(ctx, resourceLogs); err != nil {
			p.recordExportError(ctx, "logs", err)
			log.Error("Failed to export logs to cloud: %v", err)
			return err
		}
		log.Debug("Exported %d log records to cloud", logCount)
	} else {
		p.logsDropWarned.Do(func() {
			log.Error("Received %d log records but cloud exporter is not configured — logs will be dropped. Set SCION_GCP_PROJECT_ID or configure telemetry.cloud", logCount)
		})
	}

	return nil
}

// initSelfMetrics creates a minimal MeterProvider for self-monitoring metrics
// (pipeline health gauge and export error counter) and starts the health ticker.
func (p *Pipeline) initSelfMetrics(ctx context.Context) {
	providers, err := NewProviders(ctx, p.config, true)
	if err != nil || providers == nil || providers.MeterProvider == nil {
		log.Debug("Could not create MeterProvider for pipeline self-metrics: %v", err)
		p.meter = noop.Meter{}
	} else {
		// Shut down TracerProvider and LoggerProvider immediately — we only
		// need the MeterProvider for self-monitoring metrics.
		if providers.TracerProvider != nil {
			_ = providers.TracerProvider.Shutdown(ctx)
		}
		if providers.LoggerProvider != nil {
			_ = providers.LoggerProvider.Shutdown(ctx)
		}
		p.meter = providers.MeterProvider.Meter("github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry")
	}

	p.exportErrors, err = p.meter.Int64Counter("scion.telemetry.export.errors",
		otelmetric.WithDescription("Count of telemetry export failures by signal type"),
		otelmetric.WithUnit("{error}"),
	)
	if err != nil {
		log.Debug("Failed to create export error counter: %v", err)
	}

	p.startHealthGauge(ctx, providers)
}

// startHealthGauge registers the scion.telemetry.pipeline.status gauge and
// starts a background ticker that reports value 1 every 60 seconds.
func (p *Pipeline) startHealthGauge(ctx context.Context, providers *Providers) {
	gauge, err := p.meter.Int64Gauge("scion.telemetry.pipeline.status",
		otelmetric.WithDescription("Pipeline health status (1=running)"),
		otelmetric.WithUnit("{status}"),
	)
	if err != nil {
		log.Debug("Failed to create pipeline health gauge: %v", err)
		if providers != nil && providers.MeterProvider != nil {
			_ = providers.MeterProvider.Shutdown(ctx)
		}
		return
	}

	attrs := otelmetric.WithAttributes(
		attribute.String("scion.telemetry.provider", p.config.CloudProvider),
		attribute.String("scion.telemetry.project_id", p.config.ProjectID),
	)

	healthCtx, cancel := context.WithCancel(ctx)
	p.healthCancel = cancel

	gauge.Record(healthCtx, 1, attrs)

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-healthCtx.Done():
				if providers != nil && providers.MeterProvider != nil {
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = providers.MeterProvider.Shutdown(shutdownCtx)
					shutdownCancel()
				}
				return
			case <-ticker.C:
				gauge.Record(healthCtx, 1, attrs)
			}
		}
	}()
}

// recordExportError increments the export error counter if registered.
func (p *Pipeline) recordExportError(ctx context.Context, signal string, err error) {
	if p.exportErrors == nil {
		return
	}
	p.exportErrors.Add(ctx, 1,
		otelmetric.WithAttributes(
			attribute.String("signal", signal),
			attribute.String("error_type", classifyError(err)),
		),
	)
}

// classifyError buckets an export error into a category for metric attributes.
func classifyError(err error) string {
	if err == nil {
		return "none"
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "timeout"
	}

	var gapiErr *googleapi.Error
	if errors.As(err, &gapiErr) {
		switch gapiErr.Code {
		case 401, 403:
			return "auth"
		case 429:
			return "quota"
		}
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "unauthenticated") || strings.Contains(msg, "permission denied"):
		return "auth"
	case strings.Contains(msg, "quota") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "resource exhausted"):
		return "quota"
	case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timeout"):
		return "timeout"
	}

	return "other"
}
