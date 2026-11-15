/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// telemetrySecret is a credential-shaped token a provider error message
// can echo; logging.SanitizeText redacts it.
const telemetrySecret = "sk-abcdefghijklmnopqrstuvwxyz0123"

func TestTelemetryErrorText(t *testing.T) {
	got := telemetryErrorText("400 Bad Request\n\x1b[31mAuthorization: Bearer " + telemetrySecret + "\x1b[0m")
	assert.True(t, strings.HasPrefix(got, "400 Bad Request "), got)
	assert.NotContains(t, got, telemetrySecret)
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "\x1b")

	long := telemetryErrorText(strings.Repeat("é", 1000))
	assert.Equal(t, harnessErrorReasonMaxRunes, utf8.RuneCountInString(long))
	assert.True(t, strings.HasSuffix(long, "…"))

	assert.Empty(t, telemetryErrorText(""))
}

// TestTelemetryHandler_ExportsSanitizedError checks that the error string
// of a hook event reaches the span status, the span's error attribute and
// the log record's error attribute only in sanitized, capped form.
func TestTelemetryHandler_ExportsSanitizedError(t *testing.T) {
	scrubTelemetryEnv(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	proc := &recordingProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
	defer func() { _ = lp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(tp, lp, nil)
	raw := "API Error: 401 Incorrect key\nAuthorization: Bearer " + telemetrySecret + " " + strings.Repeat("x", 400)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentStart}))
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentError, Data: hooks.EventData{Error: raw}}))

	want := telemetryErrorText(raw)
	require.NotContains(t, want, telemetrySecret)

	spans := sr.Ended()
	require.Len(t, spans, 1, "agent-error ends the agent-start span")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Equal(t, want, spans[0].Status().Description)
	var spanErr string
	for _, kv := range spans[0].Attributes() {
		if kv.Key == "error" {
			spanErr = kv.Value.AsString()
		}
	}
	assert.Equal(t, want, spanErr)

	var logErrs []string
	for _, rec := range proc.Records() {
		rec.WalkAttributes(func(kv attribute.KeyValue) bool {
			if kv.Key == "error" {
				logErrs = append(logErrs, kv.Value.AsString())
			}
			return true
		})
	}
	assert.Equal(t, []string{want}, logErrs)
}

// TestTelemetryHandler_UncountedModelEndIsNotAModelCall checks that an
// uncounted model-end (copilot's failed-call errorOccurred) is exported as
// a span with the sanitized error, but records no gen_ai.api.calls metric
// and no usage tokens.
func TestTelemetryHandler_UncountedModelEndIsNotAModelCall(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(tp, nil, nil, mp)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{
		Error: "503 unavailable", Uncounted: true, InputTokens: 10,
	}}))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			assert.NotEqual(t, telemetrycontract.MetricAPICalls, m.Name)
		}
	}
	assert.Empty(t, usageTokenTotals(rm))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Equal(t, "503 unavailable", spans[0].Status().Description)
}
