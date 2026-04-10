/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Pipeline orchestrates the telemetry collection and forwarding.
type Pipeline struct {
	config   *Config
	receiver *Receiver
	exporter *CloudExporter
	filter   *Filter
	mu       sync.Mutex
	running  bool
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
		log.Info("GCP telemetry credentials: %s (source: %s, project: %s)",
			p.config.GCPCredentialsFile, source, p.config.ProjectID)
	} else if p.config.IsCloudConfigured() {
		log.Info("GCP telemetry credentials: none (using ADC fallback)")
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
		log.Debug("Cloud export not configured - telemetry will only be received locally")
	}

	// Create receiver with span and metric handlers
	p.receiver = NewReceiver(p.config, p.handleSpans, WithMetricHandler(p.handleMetrics), WithLogHandler(p.handleLogs))

	// Start receiver
	if err := p.receiver.Start(ctx); err != nil {
		if p.exporter != nil {
			p.exporter.Shutdown(ctx)
		}
		return fmt.Errorf("failed to start receiver: %w", err)
	}

	p.running = true
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
			log.Error("Failed to export spans to cloud: %v", err)
			return err
		}
		log.Debug("Exported %d spans to cloud", spanCount)
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

// handleMetrics processes incoming metrics from the receiver.
func (p *Pipeline) handleMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if len(resourceMetrics) == 0 {
		return nil
	}

	// Forward to cloud exporter if available. In GCP-native mode, OTLP metrics
	// received by this pipeline are converted and forwarded to Cloud Monitoring.
	// Sciontool's own SDK metrics may still bypass the pipeline and export
	// directly via a MeterProvider.
	if p.exporter != nil {
		if err := p.exporter.ExportProtoMetrics(ctx, resourceMetrics); err != nil {
			log.Error("Failed to export metrics to cloud: %v", err)
			return err
		}
	}

	return nil
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
			log.Error("Failed to export logs to cloud: %v", err)
			return err
		}
		log.Debug("Exported %d log records to cloud", logCount)
	}

	return nil
}
