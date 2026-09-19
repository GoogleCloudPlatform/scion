/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// metricFlushInterval is the minimum interval between metric exports to Cloud
// Monitoring. This prevents sampling-rate violations when multiple short-lived
// processes (hooks) send metrics in rapid succession.
const metricFlushInterval = 15 * time.Second

const (
	metricMaxAttempts = 20
	metricMaxAge      = 5 * time.Minute
)

type metricAdmission struct {
	bytes, records int
	at             time.Time
	streams        []metricStreamKey
}

// Pipeline orchestrates the telemetry collection and forwarding.
type Pipeline struct {
	config           *Config
	receiver         *Receiver
	exporter         *CloudExporter
	policy           *receiverPolicy
	mu               sync.Mutex
	running          bool
	deliveryState    atomic.Value // string; readable without taking the lifecycle mutex
	diagnosticMu     sync.Mutex
	diagnosticLast   time.Time
	diagnosticCancel context.CancelFunc
	diagnosticDone   chan struct{}
	healthCancel     context.CancelFunc
	exportErrors     otelmetric.Int64Counter
	meter            otelmetric.Meter
	retryConfig      RetryConfig
	intakeMu         sync.Mutex
	intakeClosed     bool
	intakeActive     int
	intakeDone       chan struct{}

	metricsDropWarned        sync.Once
	logsDropWarned           sync.Once
	spansDropWarned          sync.Once
	policyRejectedSpans      atomic.Int64
	policyRejectedLogs       atomic.Int64
	policyRejectedDataPoints atomic.Int64
	policyRejectedRequests   atomic.Int64

	metricStateMu                                                  sync.Mutex
	metricExportMu                                                 sync.Mutex
	metricStreams                                                  *metricStreams
	metricPending                                                  []*metricpb.ResourceMetrics
	metricDirtyAdmissions, metricPendingAdmissions                 []metricAdmission
	metricPendingAttempts                                          int
	metricNow                                                      func() time.Time
	metricRejectedPoints                                           atomic.Int64
	metricFlushCtx                                                 context.Context
	metricFlushCnl                                                 context.CancelFunc
	metricExportCtx                                                context.Context
	metricExportCnl                                                context.CancelFunc
	metricLastFlush                                                time.Time
	metricFlushWg                                                  sync.WaitGroup
	budget                                                         admissionBudget
	spanDiagnostics, metricDiagnostics, logDiagnostics             signalDiagnostics
	metricDirtyBytes, metricDirtyRecords, metricDirtyEntries       int
	metricPendingBytes, metricPendingRecords, metricPendingEntries int
	metricBatchSequence, metricPendingSequence                     uint64
}

func (p *Pipeline) now() time.Time {
	if p.metricNow != nil {
		return p.metricNow()
	}
	return time.Now()
}

// New creates a new telemetry pipeline.
// Returns nil if telemetry is not enabled.
func New() *Pipeline {
	config := LoadConfig()
	if !config.Enabled {
		return nil
	}
	return &Pipeline{
		config:      config,
		policy:      newReceiverPolicy(config),
		retryConfig: DefaultRetryConfig(),
	}
}

// NewWithConfig creates a new telemetry pipeline with explicit configuration.
func NewWithConfig(config *Config) *Pipeline {
	if config == nil || !config.Enabled {
		return nil
	}
	return &Pipeline{
		config:      config,
		policy:      newReceiverPolicy(config),
		retryConfig: DefaultRetryConfig(),
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
		// Log credential source for diagnostics. ADC (Application Default
		// Credentials) is the standard GCP best practice, so we don't warn
		// about it. We only warn when credentials are resolved via the
		// well-known file path fallback, which is less reliable than an
		// explicit environment variable.
		if p.config.IsGCP() && p.config.GCPCredentialsFile != "" {
			if envVal := os.Getenv(EnvGCPCredentials); envVal == "" {
				slog.Warn("telemetry credentials loaded from well-known path fallback, not environment variable",
					"credentials_file", p.config.GCPCredentialsFile,
					"project_id", p.config.ProjectID,
					"hint", fmt.Sprintf("set %s to make credential source explicit", EnvGCPCredentials),
				)
			}
		}

		exporter, err := NewCloudExporter(ctx, p.config)
		if err != nil {
			p.deliveryState.Store("failed")
			p.logDeliverySnapshot(true)
			return fmt.Errorf("failed to create cloud exporter: %w", err)
		} else {
			p.exporter = exporter
			if exporter.gcpExporter != nil {
				exporter.gcpExporter.onAsyncLogError = func(error) {
					p.logDiagnostics.sdkErrors.Add(1)
					p.markDeliveryDegraded()
				}
			}
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
		if p.config.CloudEnabled {
			p.deliveryState.Store("failed")
			p.logDeliverySnapshot(true)
			if p.config.IsGCP() && p.config.ProjectID == "" {
				slog.Warn("telemetry cloud export disabled — GCP mode requires a project ID", "env_checked", EnvProjectID)
			}
			return fmt.Errorf("cloud telemetry enabled but destination is not configured")
		}
		slog.Info("telemetry cloud export disabled")
	}

	// Create receiver with span and metric handlers
	p.intakeMu.Lock()
	p.intakeClosed = false
	p.intakeDone = nil
	p.intakeMu.Unlock()
	p.receiver = NewReceiver(p.config, p.acceptSpans, WithMetricHandler(p.acceptMetrics), WithLogHandler(p.acceptLogs))

	// Start receiver
	if err := p.receiver.Start(ctx); err != nil {
		p.deliveryState.Store("failed")
		p.logDeliverySnapshot(true)
		if p.exporter != nil {
			_ = p.exporter.Shutdown(ctx)
		}
		return fmt.Errorf("failed to start receiver: %w", err)
	}

	p.running = true
	p.deliveryState.Store("running")
	p.startDiagnosticSnapshots(ctx)
	p.logDeliverySnapshot(true)

	// Start metric flush goroutine for batching exports to Cloud Monitoring.
	if p.exporter != nil {
		p.metricFlushCtx, p.metricFlushCnl = context.WithCancel(ctx)
		p.metricExportCtx, p.metricExportCnl = context.WithCancel(ctx)
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
	intakeDone := p.closeIntake()
	if p.diagnosticCancel != nil {
		p.diagnosticCancel()
		<-p.diagnosticDone
		p.diagnosticCancel = nil
		p.diagnosticDone = nil
	}

	// Stop health gauge ticker
	if p.healthCancel != nil {
		p.healthCancel()
		p.healthCancel = nil
	}

	// Stop new periodic flushes without canceling an export already in flight.
	if p.metricFlushCnl != nil {
		p.metricFlushCnl()
		p.metricFlushCnl = nil
	}
	flushDone := make(chan struct{})
	go func() {
		p.metricFlushWg.Wait()
		close(flushDone)
	}()
	select {
	case <-flushDone:
	case <-ctx.Done():
		// The caller's deadline is the bound for a healthy in-flight export.
		if p.metricExportCnl != nil {
			p.metricExportCnl()
		}
		<-flushDone
	}
	if p.metricExportCnl != nil {
		p.metricExportCnl()
		p.metricExportCnl = nil
	}
	if p.receiver != nil {
		if err := p.receiver.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("receiver stop error: %w", err))
		}
	}
	select {
	case <-intakeDone:
	default:
		select {
		case <-intakeDone:
		case <-ctx.Done():
			p.deliveryState.Store("degraded")
			p.logDeliverySnapshot(true)
			return fmt.Errorf("telemetry shutdown incomplete: %w; %d receiver handlers still active", ctx.Err(), p.activeIntake())
		}
	}
	if p.receiver != nil {
		// A forced gRPC Stop may have returned while a pre-handler RecvMsg
		// still owns a processing slot. That worker cannot touch pipeline
		// resources after the gate closes, but it must finish before a later
		// Stop call can claim a clean drain.
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for len(p.receiver.decodeSlots) != 0 {
			select {
			case <-ctx.Done():
				p.deliveryState.Store("degraded")
				p.logDeliverySnapshot(true)
				return fmt.Errorf("telemetry shutdown incomplete: %w; %d receiver processing slots still active", ctx.Err(), len(p.receiver.decodeSlots))
			case <-ticker.C:
			}
		}
	}
	p.flushMetricsOnStop(ctx)
	p.metricStateMu.Lock()
	residualPending := len(p.metricPending)
	residualDirty := 0
	if p.metricStreams != nil {
		for _, stream := range p.metricStreams.streams {
			if stream.dirty {
				residualDirty++
			}
		}
	}
	if residualPending != 0 || residualDirty != 0 || p.metricPendingEntries+p.metricDirtyEntries != 0 {
		forcedRecords := p.metricPendingRecords + p.metricDirtyRecords
		p.metricDiagnostics.terminal(forcedRecords, terminalCanceled, 0)
		log.Error("Telemetry metric shutdown residual: %d pending streams, %d newer dirty streams, %d terminal unconfirmed admitted points", residualPending, residualDirty, forcedRecords)
		errs = append(errs, fmt.Errorf("metric shutdown residual: pending=%d dirty=%d", residualPending, residualDirty))
	}
	p.budget.release(p.metricPendingBytes+p.metricDirtyBytes, p.metricPendingRecords+p.metricDirtyRecords, p.metricPendingEntries+p.metricDirtyEntries)
	p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries = 0, 0, 0
	p.metricDirtyBytes, p.metricDirtyRecords, p.metricDirtyEntries = 0, 0, 0
	p.metricPendingAdmissions, p.metricDirtyAdmissions = nil, nil
	p.metricPendingAttempts = 0
	p.metricPending = nil
	p.metricPendingSequence = 0
	p.metricStreams = nil
	p.metricStateMu.Unlock()
	if unconfirmed := p.spanDiagnostics.unconfirmed.Load() + p.metricDiagnostics.unconfirmed.Load() + p.logDiagnostics.unconfirmed.Load(); unconfirmed > 0 {
		errs = append(errs, fmt.Errorf("telemetry delivery unconfirmed for %d admitted records", unconfirmed))
	}

	// Shutdown exporter to flush any buffered spans
	if p.exporter != nil {
		if err := p.exporter.Shutdown(ctx); err != nil {
			// Keep the exporter owned by this pipeline. The intake gate and
			// receiver are already closed, but a later Stop with a live budget
			// must be able to finish closing SDK clients.
			p.deliveryState.Store("degraded")
			p.logDeliverySnapshot(true)
			return fmt.Errorf("telemetry shutdown incomplete: exporter shutdown error: %w", err)
		}
	}

	p.running = false
	if len(errs) > 0 {
		p.deliveryState.Store("degraded")
	} else {
		if p.config != nil && !p.config.CloudEnabled {
			p.deliveryState.Store("disabled")
		} else {
			p.deliveryState.Store("configured")
		}
	}
	p.logDeliverySnapshot(true)
	log.Info("Telemetry pipeline stopped")

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Receiver handlers acquire this gate before touching pipeline state or the
// exporter. Shutdown closes the gate first and keeps resources alive until all
// previously admitted handlers return. A forced Stop may return incomplete;
// another Stop call can finish cleanup after the handlers unwind.
func (p *Pipeline) beginIntake() error {
	p.intakeMu.Lock()
	defer p.intakeMu.Unlock()
	if p.intakeClosed {
		return status.Error(codes.Unavailable, "telemetry receiver is stopping")
	}
	p.intakeActive++
	return nil
}

func (p *Pipeline) endIntake() {
	p.intakeMu.Lock()
	defer p.intakeMu.Unlock()
	p.intakeActive--
	if p.intakeClosed && p.intakeActive == 0 && p.intakeDone != nil {
		close(p.intakeDone)
		p.intakeDone = nil
	}
}

func (p *Pipeline) closeIntake() <-chan struct{} {
	p.intakeMu.Lock()
	defer p.intakeMu.Unlock()
	if p.intakeDone == nil {
		p.intakeDone = make(chan struct{})
		if p.intakeActive == 0 {
			close(p.intakeDone)
		}
	}
	p.intakeClosed = true
	return p.intakeDone
}

func (p *Pipeline) activeIntake() int {
	p.intakeMu.Lock()
	defer p.intakeMu.Unlock()
	return p.intakeActive
}

func (p *Pipeline) acceptSpans(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	if err := p.beginIntake(); err != nil {
		return err
	}
	defer p.endIntake()
	return p.handleSpans(ctx, spans)
}

func (p *Pipeline) acceptMetrics(ctx context.Context, metrics []*metricpb.ResourceMetrics) error {
	if err := p.beginIntake(); err != nil {
		return err
	}
	defer p.endIntake()
	return p.handleMetrics(ctx, metrics)
}

func (p *Pipeline) acceptLogs(ctx context.Context, logs []*logspb.ResourceLogs) error {
	if err := p.beginIntake(); err != nil {
		return err
	}
	defer p.endIntake()
	return p.handleLogs(ctx, logs)
}

// DeliveryState reports the local destination lifecycle without implying that
// an intake acknowledgment proves remote delivery.
func (p *Pipeline) DeliveryState() string {
	if p == nil {
		return "disabled"
	}
	if state := p.deliveryState.Load(); state != nil {
		return state.(string)
	}
	if p.config == nil || !p.config.CloudEnabled {
		return "disabled"
	}
	return "configured"
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
	bytes, spanCount := encodedSize(resourceSpans), int(countSpans(resourceSpans))
	if bytes > maxDecodedBytes {
		return status.Error(codes.ResourceExhausted, "trace request exceeds message limit")
	}
	if spanCount == 0 {
		return nil
	}
	if err := p.budget.reserve(bytes, spanCount); err != nil {
		p.spanDiagnostics.rejected.Add(int64(spanCount))
		return err
	}
	defer func() { p.budget.release(bytes, spanCount, 1) }()
	// Clone and transform at the receiver boundary before any destination sees
	// the batch. The processed value remains immutable across export retries.
	decision := p.policy.processSpans(resourceSpans)
	if decision.Reason != "" {
		p.spanDiagnostics.rejected.Add(decision.Rejected)
		p.policyRejectedRequests.Add(1)
		p.policyRejectedSpans.Add(decision.Rejected)
		log.Error("Rejected %d spans at telemetry receiver: %s", decision.Rejected, decision.Reason)
		return status.Error(codes.InvalidArgument, decision.Reason)
	}
	filtered := decision.Data
	p.spanDiagnostics.filtered.Add(decision.Filtered)
	if len(filtered) == 0 {
		return nil
	}
	if p.config.CloudEnabled && p.exporter == nil {
		return status.Error(codes.Unavailable, "cloud exporter unavailable")
	}

	// Count total spans for logging
	processedBytes, processedCount := encodedSize(filtered), int(countSpans(filtered))
	if err := p.budget.resize(bytes, spanCount, processedBytes, processedCount); err != nil {
		p.spanDiagnostics.rejected.Add(int64(spanCount))
		return err
	}
	bytes, spanCount = processedBytes, processedCount
	p.spanDiagnostics.accepted.Add(int64(spanCount))

	// Forward to cloud exporter if available.
	// This retry is bounded to four pipeline exporter calls. Underlying SDK
	// and transport calls may have their own retry policy within the caller's
	// context; this count is not a network-attempt guarantee.
	if p.exporter != nil {
		p.spanDiagnostics.queued.Add(int64(spanCount))
		err := retryExport(ctx, p.retryConfig, "spans", func() error {
			p.spanDiagnostics.attempts.Add(1)
			return p.exporter.ExportProtoSpans(ctx, filtered)
		})
		if err != nil {
			p.spanDiagnostics.failed.Add(1)
			reason, rejected := terminalExportReason(err)
			if rejected < 0 {
				rejected = 0
			}
			p.spanDiagnostics.terminal(spanCount, reason, rejected)
			p.recordExportError(ctx, "spans", err)
			log.Error("Failed to export spans to cloud: %v", err)
			return err
		}
		p.spanDiagnostics.success(spanCount)
		log.Debug("Exported %d spans to cloud", spanCount)
	} else {
		p.spanDiagnostics.dropped.Add(int64(spanCount))
		p.spansDropWarned.Do(func() {
			log.Error("Received %d spans but cloud exporter is not configured — spans will be dropped. Set SCION_GCP_PROJECT_ID or configure telemetry.cloud", spanCount)
		})
	}

	return nil
}

// handleMetrics buffers incoming metrics for periodic export to Cloud Monitoring.
// Metrics are accumulated and flushed at metricFlushInterval to avoid
// sampling-rate violations from rapid writes (e.g. multiple hook processes).
func (p *Pipeline) handleMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	bytes, records := encodedSize(resourceMetrics), countMetricPoints(resourceMetrics)
	if bytes > maxDecodedBytes {
		return status.Error(codes.ResourceExhausted, "metric request exceeds message limit")
	}
	// Even a request with no supported points must pass kind validation. An
	// unsupported Summary or ExponentialHistogram must never look like an empty
	// successful request merely because the stream accumulator cannot count it.
	for _, rm := range resourceMetrics {
		for _, sm := range rm.GetScopeMetrics() {
			for _, metric := range sm.GetMetrics() {
				if metric == nil {
					p.metricDiagnostics.rejected.Add(int64(records))
					return status.Error(codes.InvalidArgument, "nil metric")
				}
				if _, _, _, err := metricKind(metric); err != nil {
					p.metricDiagnostics.rejected.Add(int64(records))
					return status.Error(codes.InvalidArgument, err.Error())
				}
			}
		}
	}
	if records == 0 {
		return nil
	}
	if err := p.budget.reserve(bytes, records); err != nil {
		p.metricDiagnostics.rejected.Add(int64(records))
		return err
	}
	committed := false
	defer func() {
		if !committed {
			p.budget.release(bytes, records, 1)
		}
	}()
	decision := p.policy.processMetrics(resourceMetrics)
	if decision.Reason != "" {
		p.metricDiagnostics.rejected.Add(decision.Rejected)
		p.policyRejectedRequests.Add(1)
		p.policyRejectedDataPoints.Add(decision.Rejected)
		log.Error("Rejected %d metric data points at telemetry receiver: %s", decision.Rejected, decision.Reason)
		return status.Error(codes.InvalidArgument, decision.Reason)
	}
	processed := decision.Data
	p.metricDiagnostics.filtered.Add(decision.Filtered)
	if len(processed) == 0 {
		return nil
	}
	if p.config.CloudEnabled && p.exporter == nil {
		return status.Error(codes.Unavailable, "cloud exporter unavailable")
	}

	if p.exporter != nil {
		processedBytes, processedRecords := encodedSize(processed), countMetricPoints(processed)
		if err := p.budget.resize(bytes, records, processedBytes, processedRecords); err != nil {
			p.metricDiagnostics.rejected.Add(int64(records))
			return err
		}
		bytes, records = processedBytes, processedRecords
		p.metricStateMu.Lock()
		if p.metricStreams == nil {
			p.metricStreams = newMetricStreams()
			p.metricStreams.gcp = p.config.IsGCP()
		}
		candidate := p.metricStreams.clone()
		if err := candidate.add(processed); err != nil {
			p.metricDiagnostics.rejected.Add(int64(records))
			p.metricStreams.rejected[err.Error()]++
			p.metricRejectedPoints.Add(int64(countMetricPoints(processed)))
			p.metricStateMu.Unlock()
			return status.Error(codes.InvalidArgument, err.Error())
		}
		changed := make([]metricStreamKey, 0, len(candidate.streams))
		for key, current := range candidate.streams {
			prior := p.metricStreams.streams[key]
			if prior == nil || current.dirty != prior.dirty || !proto.Equal(current.metric, prior.metric) {
				changed = append(changed, key)
			}
		}
		p.metricStreams = candidate
		if len(changed) == 0 {
			// A duplicate or older point made no stream state eligible for
			// export. It owns no retained request entry or delivery credit.
			p.metricDiagnostics.filtered.Add(int64(records))
			p.metricStateMu.Unlock()
			return nil
		}
		committed = true
		p.metricDiagnostics.accepted.Add(int64(records))
		p.metricDiagnostics.queued.Add(int64(records))
		p.metricDirtyBytes += bytes
		p.metricDirtyRecords += records
		p.metricDirtyEntries++
		p.metricDirtyAdmissions = append(p.metricDirtyAdmissions, metricAdmission{bytes: bytes, records: records, at: p.now(), streams: changed})
		p.metricStateMu.Unlock()
		log.Debug("Accepted %d policy-processed resource metric batches", len(processed))
	} else {
		p.metricDiagnostics.accepted.Add(int64(records))
		p.metricDiagnostics.dropped.Add(int64(records))
		metricCount := 0
		for _, rm := range processed {
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

// metricFlushLoop periodically flushes metric streams to Cloud Monitoring.
func (p *Pipeline) metricFlushLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.metricFlushCtx.Done():
			return
		case <-ticker.C:
			p.flushMetricBuffer(p.metricExportCtx, false)
		}
	}
}

// expireMetricAdmissions runs with metricExportMu held. A terminal pending
// snapshot leaves the cumulative baseline intact; only its admitted ownership
// and immutable retry state are removed.
func (p *Pipeline) expireMetricAdmissions(now time.Time) {
	p.metricStateMu.Lock()
	defer p.metricStateMu.Unlock()
	if len(p.metricPendingAdmissions) != 0 && !now.Before(p.metricPendingAdmissions[0].at.Add(metricMaxAge)) {
		p.disposeMetricPending(terminalAgeLimit)
	}
	if len(p.metricDirtyAdmissions) == 0 {
		return
	}
	kept := p.metricDirtyAdmissions[:0]
	expiredStreams := make(map[metricStreamKey]struct{})
	for _, admission := range p.metricDirtyAdmissions {
		if now.Before(admission.at.Add(metricMaxAge)) {
			kept = append(kept, admission)
			continue
		}
		p.metricDiagnostics.terminal(admission.records, terminalAgeLimit, 0)
		p.budget.release(admission.bytes, admission.records, 1)
		p.metricDirtyBytes -= admission.bytes
		p.metricDirtyRecords -= admission.records
		p.metricDirtyEntries--
		for _, key := range admission.streams {
			expiredStreams[key] = struct{}{}
		}
	}
	p.metricDirtyAdmissions = kept
	for _, admission := range kept {
		for _, key := range admission.streams {
			delete(expiredStreams, key)
		}
	}
	if p.metricStreams != nil {
		for key := range expiredStreams {
			if entry := p.metricStreams.streams[key]; entry != nil {
				entry.dirty = false
			}
		}
	}
}

// disposeMetricPending requires metricStateMu and metricExportMu. The later
// dirty state, if any, retains its original admission timestamps.
func (p *Pipeline) disposeMetricPending(reason terminalReason) {
	p.metricDiagnostics.terminal(p.metricPendingRecords, reason, 0)
	p.budget.release(p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries)
	p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries = 0, 0, 0
	p.metricPendingAdmissions = nil
	p.metricPendingAttempts = 0
	p.metricPending = nil
	p.metricPendingSequence = 0
	if p.metricStreams != nil {
		p.metricStreams.clearPendingMarker()
	}
}

// flushMetricsOnStop waits for the next safe Cloud write slot when the caller
// has enough time. A short deadline leaves state intact for the residual error.
func (p *Pipeline) flushMetricsOnStop(ctx context.Context) {
	for ctx.Err() == nil {
		p.metricStateMu.Lock()
		hasWork := len(p.metricPending) != 0 || p.metricStreams != nil && p.metricStreams.hasDirty()
		nextFlush := p.metricLastFlush.Add(metricFlushInterval)
		p.metricStateMu.Unlock()
		if !hasWork {
			return
		}
		if p.exporter == nil {
			return
		}
		if delay := time.Until(nextFlush); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		// A retryable failure retains the immutable pending snapshot. Recheck
		// work and wait for the next post-completion cadence slot while the
		// caller still has time; terminal and successful calls clear ownership.
		p.flushMetricBuffer(ctx, false)
	}
}

// flushMetricBuffer exports one immutable cumulative snapshot at a time.
//
// Cloud Monitoring requires a minimum 10-second interval between writes for the
// same time series. This method enforces metricFlushInterval between exports to
// prevent rapid consecutive flushes (e.g. periodic tick followed by shutdown drain)
// from triggering sampling-rate rejections. Tests may force a flush; shutdown
// observes the cadence and reports residual state if it cannot drain safely.
func (p *Pipeline) flushMetricBuffer(ctx context.Context, force bool) bool {
	p.metricExportMu.Lock()
	defer p.metricExportMu.Unlock()
	p.expireMetricAdmissions(p.now())
	p.metricStateMu.Lock()
	sinceLastFlush := p.now().Sub(p.metricLastFlush)
	if !force && sinceLastFlush < metricFlushInterval {
		p.metricStateMu.Unlock()
		log.Debug("Skipping metric flush — last export was %v ago (minimum %v)", sinceLastFlush.Round(time.Millisecond), metricFlushInterval)
		return false
	}
	if p.metricStreams == nil {
		p.metricStreams = newMetricStreams()
	}
	if len(p.metricPending) == 0 {
		p.metricPending = p.metricStreams.snapshot()
		if len(p.metricPending) != 0 {
			p.metricBatchSequence++
			p.metricPendingSequence = p.metricBatchSequence
		}
		p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries = p.metricDirtyBytes, p.metricDirtyRecords, p.metricDirtyEntries
		p.metricPendingAdmissions, p.metricDirtyAdmissions = p.metricDirtyAdmissions, nil
		p.metricPendingAttempts = 0
		p.metricDirtyBytes, p.metricDirtyRecords, p.metricDirtyEntries = 0, 0, 0
	}
	batch := p.metricPending
	sequence := p.metricPendingSequence
	p.metricStateMu.Unlock()

	if len(batch) == 0 {
		p.metricStateMu.Lock()
		p.budget.release(p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries)
		p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries = 0, 0, 0
		p.metricPendingAdmissions = nil
		p.metricPendingAttempts = 0
		p.metricPendingSequence = 0
		p.metricStateMu.Unlock()
		return false
	}
	if p.exporter == nil {
		return false
	}

	metricCount := 0
	for _, rm := range batch {
		for _, sm := range rm.ScopeMetrics {
			metricCount += len(sm.Metrics)
		}
	}

	// Each cadence slot makes exactly one pipeline exporter invocation. The
	// immutable snapshot remains pending for a later slot on transient failure.
	p.metricStateMu.Lock()
	if p.metricPendingAttempts >= metricMaxAttempts {
		p.disposeMetricPending(terminalAttemptLimit)
		p.metricStateMu.Unlock()
		return false
	}
	remaining := metricMaxAge
	if len(p.metricPendingAdmissions) != 0 {
		remaining = p.metricPendingAdmissions[0].at.Add(metricMaxAge).Sub(p.now())
	}
	p.metricPendingAttempts++
	p.metricStateMu.Unlock()
	if remaining <= 0 {
		p.metricStateMu.Lock()
		p.metricPendingAttempts--
		p.disposeMetricPending(terminalAgeLimit)
		p.metricStateMu.Unlock()
		return false
	}
	attemptCtx, cancel := context.WithTimeout(ctx, remaining)
	p.metricDiagnostics.attempts.Add(1)
	err := p.exporter.ExportProtoMetrics(attemptCtx, batch)
	cancel()
	p.metricStateMu.Lock()
	p.metricLastFlush = p.now()
	p.metricStateMu.Unlock()
	if err != nil {
		p.metricDiagnostics.failed.Add(1)
		p.markDeliveryDegraded()
		// Do not create another error-counter point when that diagnostic stream
		// itself failed. The local log remains the failure signal.
		if !metricBatchContains(batch, pipelineMetricScope, "scion.telemetry.export.errors") {
			p.recordExportError(ctx, "metrics", err)
		}
		log.Error("Failed to export %d metric streams to cloud: %v", metricCount, err)
		if !isRetryable(err) {
			p.metricStateMu.Lock()
			reason, rejected := terminalExportReason(err)
			if rejected > int64(p.metricPendingRecords) {
				rejected = int64(p.metricPendingRecords)
			}
			p.disposeMetricPending(reason)
			p.metricDiagnostics.backendRejected.Add(rejected)
			p.metricStateMu.Unlock()
		} else {
			p.metricStateMu.Lock()
			if p.metricPendingAttempts >= metricMaxAttempts {
				p.disposeMetricPending(terminalAttemptLimit)
			} else if len(p.metricPendingAdmissions) != 0 && !p.now().Before(p.metricPendingAdmissions[0].at.Add(metricMaxAge)) {
				p.disposeMetricPending(terminalAgeLimit)
			}
			p.metricStateMu.Unlock()
		}
		return false
	}
	p.metricStateMu.Lock()
	p.metricDiagnostics.success(p.metricPendingRecords)
	p.metricPending = nil
	p.metricPendingSequence = 0
	p.metricPendingAdmissions = nil
	p.metricPendingAttempts = 0
	p.budget.release(p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries)
	p.metricPendingBytes, p.metricPendingRecords, p.metricPendingEntries = 0, 0, 0
	p.metricStreams.clearPendingMarker()
	p.metricStateMu.Unlock()
	log.Info("Telemetry metric batch confirmed sequence=%d digest=%s resource_metrics=%d points=%d", sequence, metricBatchDigest(batch), len(batch), countMetricPoints(batch))
	log.Debug("Exported %d metric streams to cloud", metricCount)
	return true
}

func metricBatchContains(rms []*metricpb.ResourceMetrics, scopeName, name string) bool {
	for _, rm := range rms {
		for _, sm := range rm.GetScopeMetrics() {
			if sm.GetScope().GetName() != scopeName {
				continue
			}
			for _, metric := range sm.GetMetrics() {
				if metric.GetName() == name {
					return true
				}
			}
		}
	}
	return false
}

func metricBatchDigest(batch []*metricpb.ResourceMetrics) string {
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: batch})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func countMetricPoints(rms []*metricpb.ResourceMetrics) int {
	count := 0
	for _, rm := range rms {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				count += len(m.GetSum().GetDataPoints()) + len(m.GetGauge().GetDataPoints()) + len(m.GetHistogram().GetDataPoints()) + len(m.GetSummary().GetDataPoints()) + len(m.GetExponentialHistogram().GetDataPoints())
			}
		}
	}
	return count
}

func encodedSize[T proto.Message](messages []T) int {
	size := 0
	for _, message := range messages {
		size += proto.Size(message)
	}
	return size
}

// handleLogs processes incoming logs from the receiver.
func (p *Pipeline) handleLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	bytes, logCount := encodedSize(resourceLogs), int(countLogs(resourceLogs))
	if bytes > maxDecodedBytes {
		return status.Error(codes.ResourceExhausted, "log request exceeds message limit")
	}
	if logCount == 0 {
		return nil
	}
	if err := p.budget.reserve(bytes, logCount); err != nil {
		p.logDiagnostics.rejected.Add(int64(logCount))
		return err
	}
	defer func() { p.budget.release(bytes, logCount, 1) }()
	decision := p.policy.processLogs(resourceLogs)
	if decision.Reason != "" {
		p.logDiagnostics.rejected.Add(decision.Rejected)
		p.policyRejectedRequests.Add(1)
		p.policyRejectedLogs.Add(decision.Rejected)
		log.Error("Rejected %d log records at telemetry receiver: %s", decision.Rejected, decision.Reason)
		return status.Error(codes.InvalidArgument, decision.Reason)
	}
	processed := decision.Data
	p.logDiagnostics.filtered.Add(decision.Filtered)
	if len(processed) == 0 {
		return nil
	}
	if p.config.CloudEnabled && p.exporter == nil {
		return status.Error(codes.Unavailable, "cloud exporter unavailable")
	}

	// Count total log records for logging
	processedBytes, processedCount := encodedSize(processed), int(countLogs(processed))
	if err := p.budget.resize(bytes, logCount, processedBytes, processedCount); err != nil {
		p.logDiagnostics.rejected.Add(int64(logCount))
		return err
	}
	bytes, logCount = processedBytes, processedCount
	p.logDiagnostics.accepted.Add(int64(logCount))

	// Forward to cloud exporter if available.
	// Pipeline-level retry on top of gRPC/SDK transport retry — see
	// handleSpans for the rationale on intentional double-retry layering.
	if p.exporter != nil {
		p.logDiagnostics.queued.Add(int64(logCount))
		err := retryExport(ctx, p.retryConfig, "logs", func() error {
			p.logDiagnostics.attempts.Add(1)
			return p.exporter.ExportProtoLogs(ctx, processed)
		})
		if err != nil {
			p.logDiagnostics.failed.Add(1)
			reason, rejected := terminalExportReason(err)
			p.logDiagnostics.terminal(logCount, reason, rejected)
			p.recordExportError(ctx, "logs", err)
			log.Error("Failed to export logs to cloud: %v", err)
			return err
		}
		p.logDiagnostics.success(logCount)
		log.Debug("Exported %d log records to cloud", logCount)
	} else {
		p.logDiagnostics.dropped.Add(int64(logCount))
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
	p.markDeliveryDegraded()
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

	// Structured gRPC status check — GCP-native SDKs (Cloud Trace,
	// Monitoring, Logging) return gRPC status errors, not googleapi.Error.
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unauthenticated, codes.PermissionDenied:
			return "auth"
		case codes.ResourceExhausted:
			return "quota"
		case codes.DeadlineExceeded:
			return "timeout"
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
