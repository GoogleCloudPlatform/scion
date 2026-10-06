/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// SpanMapping defines how hook events map to span names.
var SpanMapping = map[string]string{
	hooks.EventSessionStart:     "agent.session.start",
	hooks.EventSessionEnd:       "agent.session.end",
	hooks.EventToolStart:        "agent.tool.call",
	hooks.EventToolEnd:          "agent.tool.result",
	hooks.EventPromptSubmit:     "agent.user.prompt",
	hooks.EventModelStart:       "gen_ai.api.request",
	hooks.EventModelEnd:         "gen_ai.api.response",
	hooks.EventAgentStart:       "agent.turn.start",
	hooks.EventAgentEnd:         "agent.turn.end",
	hooks.EventNotification:     "agent.notification",
	hooks.EventResponseComplete: "agent.response.complete",
	hooks.EventPreStart:         "agent.lifecycle.pre_start",
	hooks.EventPostStart:        "agent.lifecycle.post_start",
	hooks.EventPreStop:          "agent.lifecycle.pre_stop",
}

// inProgressSpan tracks a span that has been started but not ended.
type inProgressSpan struct {
	span      trace.Span
	ctx       context.Context
	startTime time.Time
	toolName  string
}

// TelemetryHandler converts hook events to OTLP spans, emits correlated log records,
// and records OTel metrics for token usage, tool calls, and API performance.
type TelemetryHandler struct {
	tracer       trace.Tracer
	logger       *slog.Logger
	metricsDebug bool
	spanStore    sync.Map // map[string]*inProgressSpan - keyed by spanKey

	// Aggregator accumulates session-level metrics for Hub reporting.
	aggregator *telemetry.Aggregator

	// OnSessionEnd is called with the finalized session summary when a
	// session-end event is processed. Set by the daemon to wire up Hub
	// reporting without creating a circular dependency.
	OnSessionEnd func(summary telemetry.SessionSummary)

	// Metric instruments
	usageTokens  metric.Int64Counter // scion.usage.tokens{token_type} (design §3.5; replaces scion.hook.tokens.*)
	toolCalls    metric.Int64Counter
	toolDuration metric.Float64Histogram
	sessionCount metric.Int64Counter
	apiCalls     metric.Int64Counter
	apiDuration  metric.Float64Histogram
}

// NewTelemetryHandler creates a new telemetry handler.
// If tp is nil, a noop tracer will be used.
// If lp is non-nil, correlated log records will be emitted alongside spans.
// If mp is non-nil, OTel metric instruments will be created for recording counters and histograms.
func NewTelemetryHandler(tp trace.TracerProvider, lp otellog.LoggerProvider, _ *telemetry.Redactor, mp ...metric.MeterProvider) *TelemetryHandler {
	return newTelemetryHandler(tp, lp, hookMetricScope, mp...)
}

// NewLifecycleTelemetryHandler gives init lifecycle metrics their own source
// identity. Hook subprocesses keep the original instrumentation scope.
func NewLifecycleTelemetryHandler(tp trace.TracerProvider, lp otellog.LoggerProvider, _ *telemetry.Redactor, mp ...metric.MeterProvider) *TelemetryHandler {
	return newTelemetryHandler(tp, lp, telemetry.LifecycleMetricScope, mp...)
}

const hookMetricScope = "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"

func newTelemetryHandler(tp trace.TracerProvider, lp otellog.LoggerProvider, metricScope string, mp ...metric.MeterProvider) *TelemetryHandler {
	var tracer trace.Tracer
	if tp != nil {
		tracer = tp.Tracer(hookMetricScope)
	} else {
		tracer = noop.NewTracerProvider().Tracer("noop")
	}

	h := &TelemetryHandler{
		tracer:       tracer,
		metricsDebug: telemetry.MetricsDebugEnabled(),
		aggregator:   telemetry.NewAggregator(),
	}

	if lp != nil {
		h.logger = slog.New(otelslog.NewHandler("sciontool.hooks",
			otelslog.WithLoggerProvider(lp),
		))
	}

	// Initialize metric instruments if a MeterProvider is given
	if len(mp) > 0 && mp[0] != nil {
		h.initMetrics(mp[0], metricScope)
	}

	return h
}

// initMetrics creates OTel metric instruments on the handler.
func (h *TelemetryHandler) initMetrics(mp metric.MeterProvider, scope string) {
	meter := mp.Meter(scope)

	var err error

	h.usageTokens, err = meter.Int64Counter(telemetrycontract.MetricUsageTokens,
		metric.WithUnit("{token}"),
		metric.WithDescription("Tokens attributed to model requests, by token_type"),
	)
	if err != nil {
		log.Error("Failed to create %s counter: %v", telemetrycontract.MetricUsageTokens, err)
	}

	h.toolCalls, err = meter.Int64Counter("agent.tool.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Number of tool invocations"),
	)
	if err != nil {
		log.Error("Failed to create agent.tool.calls counter: %v", err)
	}

	h.toolDuration, err = meter.Float64Histogram("agent.tool.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of tool invocations"),
	)
	if err != nil {
		log.Error("Failed to create agent.tool.duration histogram: %v", err)
	}

	h.sessionCount, err = meter.Int64Counter("agent.session.count",
		metric.WithUnit("{session}"),
		metric.WithDescription("Number of agent sessions"),
	)
	if err != nil {
		log.Error("Failed to create agent.session.count counter: %v", err)
	}

	h.apiCalls, err = meter.Int64Counter(telemetrycontract.MetricAPICalls,
		metric.WithUnit("{call}"),
		metric.WithDescription("Number of LLM API calls"),
	)
	if err != nil {
		log.Error("Failed to create %s counter: %v", telemetrycontract.MetricAPICalls, err)
	}

	h.apiDuration, err = meter.Float64Histogram("gen_ai.api.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of LLM API calls"),
	)
	if err != nil {
		log.Error("Failed to create gen_ai.api.duration histogram: %v", err)
	}

	if h.metricsDebug {
		log.TaggedInfo("metrics", "initialized metrics instruments for hook telemetry")
	}
}

// Handle processes a hook event and emits a corresponding span.
func (h *TelemetryHandler) Handle(event *hooks.Event) error {
	if h == nil || event == nil {
		return nil
	}

	if h.metricsDebug && isMetricRelevantEvent(event.Name) {
		log.TaggedInfo("metrics",
			"normalized hook event=%s dialect=%s input_tokens=%d output_tokens=%d cached_tokens=%d success=%t has_error=%t",
			event.Name, event.Dialect, event.Data.InputTokens, event.Data.OutputTokens, event.Data.CachedTokens, event.Data.Success, event.Data.Error != "")
	}

	spanName, ok := SpanMapping[event.Name]
	if !ok {
		// Unknown event type - skip
		return nil
	}

	// Update the in-memory aggregator for Hub reporting.
	h.updateAggregator(event)

	// Handle start/end pairing for tool calls and model calls
	switch event.Name {
	case hooks.EventToolStart:
		h.startSpan(event, spanName)
	case hooks.EventToolEnd:
		h.endSpan(event, spanName, hooks.EventToolStart)
	case hooks.EventModelStart:
		h.startSpan(event, spanName)
	case hooks.EventModelEnd:
		h.endSpan(event, spanName, hooks.EventModelStart)
	case hooks.EventAgentStart:
		h.startSpan(event, spanName)
	case hooks.EventAgentEnd:
		h.endSpan(event, spanName, hooks.EventAgentStart)
	default:
		// Single-shot events - create and immediately end span
		h.singleSpan(event, spanName)
	}

	return nil
}

// spanKey generates a unique key for tracking in-progress spans.
// For tool calls, we include the tool name to handle concurrent tool calls.
func (h *TelemetryHandler) spanKey(eventType, toolName string) string {
	if toolName != "" {
		return eventType + ":" + toolName
	}
	return eventType
}

// startSpan creates a new in-progress span.
func (h *TelemetryHandler) startSpan(event *hooks.Event, spanName string) {
	ctx := context.Background()
	attrs := h.eventToAttributes(event)

	ctx, span := h.tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
	h.emitLogRecord(ctx, event, spanName)

	key := h.spanKey(event.Name, event.Data.ToolName)
	h.spanStore.Store(key, &inProgressSpan{
		span:      span,
		ctx:       ctx,
		startTime: time.Now(),
		toolName:  event.Data.ToolName,
	})
}

// endSpan ends an in-progress span.
func (h *TelemetryHandler) endSpan(event *hooks.Event, spanName, startEventType string) {
	key := h.spanKey(startEventType, event.Data.ToolName)

	val, ok := h.spanStore.LoadAndDelete(key)
	if !ok {
		// No matching start event — common in hook-per-process mode where
		// each hook invocation is a separate process. Record metrics from
		// the end event alone and emit a single span.
		h.singleSpan(event, spanName)
		h.recordUnpairedEndMetrics(event, startEventType)
		return
	}

	inProgress := val.(*inProgressSpan)

	// Add end-event attributes
	attrs := h.eventToEndAttributes(event, inProgress.startTime)
	inProgress.span.SetAttributes(attrs...)

	// Set status based on success/error
	if event.Data.Error != "" {
		inProgress.span.SetStatus(codes.Error, event.Data.Error)
	} else if event.Data.Success {
		inProgress.span.SetStatus(codes.Ok, "")
	}

	h.emitLogRecord(inProgress.ctx, event, spanName)
	inProgress.span.End()

	// Record metrics for end events
	h.recordEndMetrics(event, startEventType, inProgress)
}

// singleSpan creates and immediately ends a span.
func (h *TelemetryHandler) singleSpan(event *hooks.Event, spanName string) {
	ctx := context.Background()
	attrs := h.eventToAttributes(event)

	// For session-end events, record session metric counters
	if event.Name == hooks.EventSessionEnd {
		h.recordSessionMetrics(event)
	}

	ctx, span := h.tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
	h.emitLogRecord(ctx, event, spanName)

	// Set status based on success/error
	if event.Data.Error != "" {
		span.SetStatus(codes.Error, event.Data.Error)
	} else if event.Data.Success {
		span.SetStatus(codes.Ok, "")
	}

	span.End()
}

// emitLogRecord emits a correlated log record for the event.
// The ctx must carry the active span so the otelslog bridge can extract
// trace_id and span_id for correlation.
func (h *TelemetryHandler) emitLogRecord(ctx context.Context, event *hooks.Event, spanName string) {
	if h.logger == nil {
		return
	}

	attrs := []slog.Attr{
		slog.String("event.name", spanName),
	}

	if event.RawName != "" {
		attrs = append(attrs, slog.String("event.raw_name", event.RawName))
	}
	if event.Dialect != "" {
		attrs = append(attrs, slog.String("event.dialect", event.Dialect))
	}
	if event.Data.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", event.Data.SessionID))
	}
	if event.Data.ToolName != "" {
		attrs = append(attrs, slog.String("tool_name", event.Data.ToolName))
	}
	if event.Data.ToolInput != "" {
		attrs = append(attrs, slog.String("tool_input", event.Data.ToolInput))
	}
	if event.Data.ToolOutput != "" {
		attrs = append(attrs, slog.String("tool_output", event.Data.ToolOutput))
	}
	if event.Data.FilePath != "" {
		attrs = append(attrs, slog.String("file_path", event.Data.FilePath))
	}
	if event.Data.Prompt != "" {
		attrs = append(attrs, slog.String("prompt", event.Data.Prompt))
	}
	if event.Data.Source != "" {
		attrs = append(attrs, slog.String("source", event.Data.Source))
	}
	if event.Data.Reason != "" {
		attrs = append(attrs, slog.String("reason", event.Data.Reason))
	}
	if event.Data.Message != "" {
		attrs = append(attrs, slog.String("message", event.Data.Message))
	}
	if model := payloadModel(event); model != "" {
		attrs = append(attrs, slog.String(telemetrycontract.ModelLabel, model))
	}
	if event.Data.Success {
		attrs = append(attrs, slog.Bool("success", true))
	}
	if event.Data.Error != "" {
		attrs = append(attrs, slog.String("error", event.Data.Error))
	}
	if event.Data.InputTokens > 0 {
		attrs = append(attrs, slog.Int64("gen_ai.usage.input_tokens", event.Data.InputTokens))
	}
	if event.Data.OutputTokens > 0 {
		attrs = append(attrs, slog.Int64("gen_ai.usage.output_tokens", event.Data.OutputTokens))
	}
	if event.Data.CachedTokens > 0 {
		attrs = append(attrs, slog.Int64("gen_ai.usage.cached_tokens", event.Data.CachedTokens))
	}

	h.logger.LogAttrs(ctx, slog.LevelInfo, spanName, attrs...)
}

// eventToAttributes converts event data to span attributes.
func (h *TelemetryHandler) eventToAttributes(event *hooks.Event) []attribute.KeyValue {
	eventName := SpanMapping[event.Name]
	if eventName == "" {
		eventName = event.Name
	}
	attrs := []attribute.KeyValue{
		attribute.String("event.name", eventName),
	}

	if event.RawName != "" {
		attrs = append(attrs, attribute.String("event.raw_name", event.RawName))
	}
	if event.Dialect != "" {
		attrs = append(attrs, attribute.String("event.dialect", event.Dialect))
	}

	// Add data fields with redaction
	if event.Data.SessionID != "" {
		attrs = append(attrs, attribute.String("session_id", event.Data.SessionID))
	}

	if event.Data.ToolName != "" {
		attrs = append(attrs, attribute.String("tool_name", event.Data.ToolName))
	}

	if event.Data.ToolInput != "" {
		attrs = append(attrs, attribute.String("tool_input", event.Data.ToolInput))
	}

	if event.Data.ToolOutput != "" {
		attrs = append(attrs, attribute.String("tool_output", event.Data.ToolOutput))
	}

	if event.Data.FilePath != "" {
		attrs = append(attrs, attribute.String("file_path", event.Data.FilePath))
	}

	if event.Data.Prompt != "" {
		attrs = append(attrs, attribute.String("prompt", event.Data.Prompt))
	}

	if event.Data.Source != "" {
		attrs = append(attrs, attribute.String("source", event.Data.Source))
	}

	if event.Data.Reason != "" {
		attrs = append(attrs, attribute.String("reason", event.Data.Reason))
	}

	if event.Data.Message != "" {
		attrs = append(attrs, attribute.String("message", event.Data.Message))
	}

	if model := payloadModel(event); model != "" {
		attrs = append(attrs, attribute.String(telemetrycontract.ModelLabel, model))
	}

	return attrs
}

// eventToEndAttributes creates attributes specific to end events.
func (h *TelemetryHandler) eventToEndAttributes(event *hooks.Event, startTime time.Time) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.Int64("duration_ms", time.Since(startTime).Milliseconds()),
	}

	if event.Data.Success {
		attrs = append(attrs, attribute.Bool("success", true))
	}

	if event.Data.Error != "" {
		attrs = append(attrs, attribute.String("error", event.Data.Error))
	}

	// Add tool output for end events
	if event.Data.ToolOutput != "" {
		attrs = append(attrs, attribute.String("tool_output", event.Data.ToolOutput))
	}

	// Add token usage attributes
	if event.Data.InputTokens > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", event.Data.InputTokens))
	}
	if event.Data.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.output_tokens", event.Data.OutputTokens))
	}
	if event.Data.CachedTokens > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.cached_tokens", event.Data.CachedTokens))
	}

	return attrs
}

// metricAttrs returns common OTel metric attributes derived from environment.
func (h *TelemetryHandler) metricAttrs() []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if v := os.Getenv("SCION_AGENT_ID"); v != "" {
		attrs = append(attrs, attribute.String("agent_id", v))
	}
	if v := os.Getenv("SCION_HARNESS"); v != "" {
		attrs = append(attrs, attribute.String(telemetrycontract.HarnessLabel, v))
	}
	projectID := projectkeys.ProjectIDFromEnv(os.Getenv)
	if projectID != "" {
		attrs = append(attrs, attribute.String("project_id", projectID))
	}
	return attrs
}

// usageHookRecordingEnabled reports whether this process should record
// hook-sourced usage (gen_ai.api.calls and scion.usage.tokens). Per design
// D4/D10 and §3.5 "Single source":
//   - SCION_USAGE_SOURCE=hooks: this harness has opted in behind a
//     fixture-backed PR, so hook usage is recorded;
//   - SCION_USAGE_SOURCE=native: the native UsageDeriver owns usage instead,
//     so hook usage is skipped;
//   - unset (the vetting-gate default, D10): usage is skipped until a
//     harness's provision.py explicitly declares a source.
//
// Tool, session, turn and every other hook metric, span and log is
// unaffected by this gate (D4, narrow).
func usageHookRecordingEnabled() bool {
	v := os.Getenv("SCION_USAGE_SOURCE")
	if v == "hooks" {
		return true
	}
	// "native" is the expected value that intentionally suppresses hook usage
	// (the deriver owns it); anything else non-empty is likely a provision.py
	// typo (e.g. "HOOKS" or a trailing space) rather than a deliberate choice.
	// The safe D10 default (no usage) still applies either way; this only
	// makes an unrecognized value visible for debugging.
	if v != "" && v != "native" {
		log.Debug("SCION_USAGE_SOURCE=%q is not a recognized usage source (want \"hooks\" or \"native\"); hook usage stays suppressed", v)
	}
	return false
}

// hookModelLabel resolves the model label for a hook-sourced usage point
// (design §3.2): the event payload's own model when the dialect mapped one,
// then SCION_MODEL, then "unknown", truncated to 128 bytes. The native
// UsageDeriver resolves through the same telemetrycontract helper.
func hookModelLabel(event *hooks.Event) string {
	return telemetrycontract.ResolveModelLabel(event.Data.Model, os.Getenv("SCION_MODEL"))
}

// payloadModel returns the event payload's own model (bounded like the
// metric label), or "" when the payload carried none. Hook spans and logs
// carry a model attribute only in that case: unlike the usage metrics, they
// describe the event itself, so they never fall back to SCION_MODEL or
// "unknown".
func payloadModel(event *hooks.Event) string {
	if strings.TrimSpace(event.Data.Model) == "" {
		return ""
	}
	return telemetrycontract.ResolveModelLabel(event.Data.Model, "")
}

// recordEndMetrics records metrics when a paired end event completes.
func (h *TelemetryHandler) recordEndMetrics(event *hooks.Event, startEventType string, inProgress *inProgressSpan) {
	ctx := context.Background()
	durationMs := float64(time.Since(inProgress.startTime).Milliseconds())
	baseAttrs := h.metricAttrs()

	switch startEventType {
	case hooks.EventToolStart:
		if h.toolCalls != nil {
			status := "success"
			if event.Data.Error != "" {
				status = "error"
			}
			attrs := append(baseAttrs,
				attribute.String("tool_name", event.Data.ToolName),
				attribute.String("status", status),
			)
			h.toolCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
		}
		if h.toolDuration != nil {
			attrs := append(baseAttrs,
				attribute.String("tool_name", event.Data.ToolName),
			)
			h.toolDuration.Record(ctx, durationMs, metric.WithAttributes(attrs...))
		}

	case hooks.EventModelStart:
		usageEnabled := usageHookRecordingEnabled()
		if h.apiCalls != nil && usageEnabled {
			status := telemetrycontract.StatusSuccess
			if event.Data.Error != "" {
				status = telemetrycontract.StatusError
			}
			attrs := append(baseAttrs, attribute.String(telemetrycontract.ModelLabel, hookModelLabel(event)))
			attrs = append(attrs, attribute.String(telemetrycontract.StatusLabel, status))
			h.apiCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
		}
		if h.apiDuration != nil {
			attrs := append(baseAttrs, attribute.String(telemetrycontract.ModelLabel, hookModelLabel(event)))
			h.apiDuration.Record(ctx, durationMs, metric.WithAttributes(attrs...))
		}

		// Record token usage from model-end events, subject to the same
		// usage-source gate as gen_ai.api.calls above (design §3.5).
		if usageEnabled {
			h.recordTokenMetrics(ctx, event)
		}
	}
}

// recordUnpairedEndMetrics records counter metrics from an end event that had no
// matching start event in the spanStore. This is the normal case in hook-per-process
// mode where each harness event invokes a separate sciontool process. Duration
// metrics are skipped since the start time is unknown.
func (h *TelemetryHandler) recordUnpairedEndMetrics(event *hooks.Event, startEventType string) {
	ctx := context.Background()
	baseAttrs := h.metricAttrs()

	switch startEventType {
	case hooks.EventToolStart:
		if h.toolCalls != nil {
			status := "success"
			if event.Data.Error != "" {
				status = "error"
			}
			attrs := append(baseAttrs,
				attribute.String("tool_name", event.Data.ToolName),
				attribute.String("status", status),
			)
			h.toolCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
		}

	case hooks.EventModelStart:
		usageEnabled := usageHookRecordingEnabled()
		if h.apiCalls != nil && usageEnabled {
			status := telemetrycontract.StatusSuccess
			if event.Data.Error != "" {
				status = telemetrycontract.StatusError
			}
			attrs := append(baseAttrs, attribute.String(telemetrycontract.ModelLabel, hookModelLabel(event)))
			attrs = append(attrs, attribute.String(telemetrycontract.StatusLabel, status))
			h.apiCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
		}

		// Record token usage from model-end events, subject to the same
		// usage-source gate as gen_ai.api.calls above (design §3.5).
		if usageEnabled {
			h.recordTokenMetrics(ctx, event)
		}
	}
}

// toOTelAttrs converts contract label pairs into OTel attributes,
// preallocated to the input length.
func toOTelAttrs(kvs []telemetrycontract.LabelKV) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, len(kvs))
	for i, kv := range kvs {
		attrs[i] = attribute.String(kv.Key, kv.Value)
	}
	return attrs
}

// recordTokenMetrics records token usage counters from an event's token
// fields. Unlike gen_ai.api.calls (which keeps agent_id/project_id for
// descriptor compatibility, design §3.2), scion.usage.tokens's producer
// label set is exactly harness, model and token_type -- nothing else. The
// GCP allowlist for this one metric is cloudUsageTokenFields, narrower than
// the general hook-counter allowlist cloudPointFields
// (gcp_metric_identity.go): a point carrying any other key, such as
// agent_id or project_id, gets the whole OTLP request rejected. This builds
// its attribute set from telemetrycontract.UsageTokenPointAttrs instead of
// taking baseAttrs from the caller -- the same helper the admission
// regression test in pkg/sciontool/telemetry uses, so the two can't drift
// apart.
func (h *TelemetryHandler) recordTokenMetrics(ctx context.Context, event *hooks.Event) {
	if h.usageTokens == nil {
		return
	}

	attrs := toOTelAttrs(telemetrycontract.UsageTokenPointAttrs(os.Getenv("SCION_HARNESS"), hookModelLabel(event)))

	recorded := false
	record := func(tokenType string, n int64) {
		if n <= 0 {
			return
		}
		// Built fresh on every call: appending onto a shared prefix's spare
		// capacity would be safe only as long as metric.WithAttributes
		// copies synchronously before the next call -- an easy invariant to
		// break later, so this avoids depending on it.
		pointAttrs := make([]attribute.KeyValue, len(attrs), len(attrs)+1)
		copy(pointAttrs, attrs)
		pointAttrs = append(pointAttrs, attribute.String(telemetrycontract.TokenTypeLabel, tokenType))
		h.usageTokens.Add(ctx, n, metric.WithAttributes(pointAttrs...))
		recorded = true
	}

	// scion.hook.tokens.{input,output,cached} is replaced by a single
	// scion.usage.tokens counter with a token_type attribute (design §3.5).
	// "cached" maps to the canonical "cache_read" (design §3.2/§3.5).
	record(telemetrycontract.TokenTypeInput, event.Data.InputTokens)
	record(telemetrycontract.TokenTypeOutput, event.Data.OutputTokens)
	record(telemetrycontract.TokenTypeCacheRead, event.Data.CachedTokens)
	record(telemetrycontract.TokenTypeCacheWrite, event.Data.CacheWriteTokens)
	// Reasoning is informational only: design §3.2 defines canonical
	// "output" as generated tokens *including* reasoning, and a source that
	// reports ReasoningTokens separately already includes it in
	// event.Data.OutputTokens too. So this is recorded as its own
	// token_type for visibility, but never added into TokenTypeOutput
	// above, which would double count it against the total.
	record(telemetrycontract.TokenTypeReasoning, event.Data.ReasoningTokens)

	if h.metricsDebug {
		if recorded {
			log.TaggedInfo("metrics",
				"recorded token metrics for event=%s input=%d output=%d cached=%d cache_write=%d reasoning=%d",
				event.Name, event.Data.InputTokens, event.Data.OutputTokens, event.Data.CachedTokens, event.Data.CacheWriteTokens, event.Data.ReasoningTokens)
		} else if event.Name == hooks.EventModelEnd || event.Name == hooks.EventSessionEnd {
			log.TaggedInfo("metrics", "no token metrics recorded for event=%s (no token fields present)", event.Name)
		}
	}
}

// recordSessionMetrics records session completion. Session-end token totals
// overlap model-end increments, so they are omitted from normalized counters.
func (h *TelemetryHandler) recordSessionMetrics(event *hooks.Event) {
	ctx := context.Background()
	baseAttrs := h.metricAttrs()

	if h.sessionCount != nil {
		status := "completed"
		if event.Data.Error != "" {
			status = "error"
		}
		sessionAttrs := append(baseAttrs, attribute.String("status", status))
		h.sessionCount.Add(ctx, 1, metric.WithAttributes(sessionAttrs...))
	}

}

// Flush ends any in-progress spans. Called during shutdown.
func (h *TelemetryHandler) Flush() {
	h.spanStore.Range(func(key, value any) bool {
		if inProgress, ok := value.(*inProgressSpan); ok {
			inProgress.span.SetStatus(codes.Error, "span not properly ended - flushed during shutdown")
			inProgress.span.End()
		}
		h.spanStore.Delete(key)
		return true
	})
}

// updateAggregator feeds event data into the in-memory aggregator that
// accumulates session-level metrics for Hub reporting.
func (h *TelemetryHandler) updateAggregator(event *hooks.Event) {
	if h.aggregator == nil {
		return
	}

	switch event.Name {
	case hooks.EventSessionStart:
		h.aggregator.StartSession(event.Data.SessionID)

	case hooks.EventToolEnd:
		h.aggregator.RecordToolEnd(event.Data.ToolName, event.Data.Error)

	case hooks.EventModelEnd:
		h.aggregator.RecordModelEnd(
			event.Data.InputTokens,
			event.Data.OutputTokens,
			event.Data.CachedTokens,
			event.Data.ReasoningTokens,
		)

	case hooks.EventAgentEnd:
		h.aggregator.RecordTurn()

	case hooks.EventSessionEnd:
		summary := h.aggregator.Finalize(
			event.Data.InputTokens,
			event.Data.OutputTokens,
			event.Data.CachedTokens,
			event.Data.ReasoningTokens,
			event.Data.Error,
		)
		if h.OnSessionEnd != nil {
			h.OnSessionEnd(summary)
		}
	}
}

func isMetricRelevantEvent(name string) bool {
	switch name {
	case hooks.EventModelEnd, hooks.EventSessionEnd, hooks.EventToolEnd:
		return true
	default:
		return false
	}
}
