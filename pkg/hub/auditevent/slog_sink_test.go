// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditevent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func TestSlogSinkEmitsExactStructuredEnvelope(t *testing.T) {
	t.Parallel()

	handler := &captureSlogHandler{}
	sink, err := NewSlogSink(slog.New(handler))
	require.NoError(t, err)
	event := validCreateEvent(t)

	require.NoError(t, sink.Emit(context.Background(), event))
	records := handler.Records()
	require.Len(t, records, 1)
	record := records[0]
	assert.Equal(t, EventName, record.Message)
	assert.Equal(t, slog.LevelInfo, record.Level)
	assert.Equal(t, event.OccurredAt, record.Time)

	want, err := Render(event)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(recordAttrsJSON(t, record)))
}

func TestSlogSinkUsesOneDefensiveRenderSnapshot(t *testing.T) {
	t.Parallel()

	handler := &captureSlogHandler{}
	sink, err := NewSlogSink(slog.New(handler))
	require.NoError(t, err)
	event := validCreateEvent(t)
	payload := &changingTestPayload{}
	event.Payload = payload
	principal := event.Principal
	resource := event.Resource

	require.NoError(t, sink.Emit(context.Background(), event))
	assert.Equal(t, 1, payload.calls)

	principal.ID = "principal-private-canary"
	resource.ID = "resource-private-canary"
	record := handler.Records()[0]
	encoded := string(recordAttrsJSON(t, record))
	assert.NotContains(t, encoded, "snapshot-canary")
	assert.NotContains(t, encoded, "principal-private-canary")
	assert.NotContains(t, encoded, "resource-private-canary")
	assert.Contains(t, encoded, `"id":"constraint-1"`)
}

func TestSlogSinkUsesHandlerPortableNestedValues(t *testing.T) {
	t.Parallel()

	handler := &captureSlogHandler{}
	sink, err := NewSlogSink(slog.New(handler))
	require.NoError(t, err)
	event := fullCreateEvent(t)

	require.NoError(t, sink.Emit(context.Background(), event))
	record := handler.Records()[0]
	attrs := recordAttrs(t, record)
	for _, key := range []string{"request", "initiator", "principal", "executor", "credential", "resource", "payload"} {
		assert.Equal(t, slog.KindGroup, attrs[key].Kind(), key)
	}
	payload := groupAttrs(attrs["payload"])
	assert.Equal(t, slog.KindGroup, payload["impact_counts"].Kind())
	assert.Equal(t, slog.KindAny, payload["changed_fields"].Kind())
	assert.IsType(t, []string{}, payload["changed_fields"].Any())
	resource := groupAttrs(attrs["resource"])
	assert.NotContains(t, resource, "Scope")
	assert.NotContains(t, resource, "scope")

	got, err := json.Marshal(slogRecordMap(t, record))
	require.NoError(t, err)
	want, err := Render(event)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
}

func TestSlogSinkPreservesNestedSchemaThroughOTel(t *testing.T) {
	t.Parallel()

	exporter := &captureOTelExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	sink, err := NewSlogSink(slog.New(logging.NewOTelHandler("auditevent-test", provider)))
	require.NoError(t, err)
	event := fullCreateEvent(t)

	require.NoError(t, sink.Emit(context.Background(), event))
	records := exporter.Records()
	require.Len(t, records, 1)
	got, err := json.Marshal(otelRecordMap(records[0]))
	require.NoError(t, err)
	want, err := Render(event)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
	assert.NotContains(t, string(got), "Scope")
}

func TestEmptyRequestCanonicalizesToAbsenceAcrossRenderAndHandlers(t *testing.T) {
	t.Parallel()

	event := validCreateEvent(t)
	event.Request = &RequestRef{}
	snapshot := newRenderSnapshot(event)
	assert.Nil(t, snapshot.event.Request)
	assert.Nil(t, snapshot.serialized.Request)

	rendered, err := Render(event)
	require.NoError(t, err)
	var renderedObject map[string]any
	require.NoError(t, json.Unmarshal(rendered, &renderedObject))
	assert.NotContains(t, renderedObject, "request")

	rawHandler := &captureSlogHandler{}
	rawSink, err := NewSlogSink(slog.New(rawHandler))
	require.NoError(t, err)
	require.NoError(t, rawSink.Emit(context.Background(), event))
	rawRecord := rawHandler.Records()[0]
	assert.NotContains(t, recordAttrs(t, rawRecord), "request")
	assert.JSONEq(t, string(rendered), string(recordAttrsJSON(t, rawRecord)))

	exporter := &captureOTelExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	otelSink, err := NewSlogSink(slog.New(logging.NewOTelHandler("auditevent-test", provider)))
	require.NoError(t, err)
	require.NoError(t, otelSink.Emit(context.Background(), event))
	otelRecords := exporter.Records()
	require.Len(t, otelRecords, 1)
	otelObject := otelRecordMap(otelRecords[0])
	assert.NotContains(t, otelObject, "request")
	otelJSON, err := json.Marshal(otelObject)
	require.NoError(t, err)
	assert.JSONEq(t, string(rendered), string(otelJSON))
}

func TestSlogSinkRecordsEmitCallerPC(t *testing.T) {
	t.Parallel()

	handler := &captureSlogHandler{}
	sink, err := NewSlogSink(slog.New(handler))
	require.NoError(t, err)
	require.NoError(t, sink.Emit(context.Background(), validCreateEvent(t)))

	record := handler.Records()[0]
	require.NotZero(t, record.PC)
	frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
	assert.Contains(t, frame.Function, "TestSlogSinkRecordsEmitCallerPC")
	assert.True(t, strings.HasSuffix(frame.File, "pkg/hub/auditevent/slog_sink_test.go"), frame.File)
}

func TestSlogSinkReturnsValidationAndHandlerErrors(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("handler failed")
	handler := &captureSlogHandler{err: handlerErr}
	sink, err := NewSlogSink(slog.New(handler))
	require.NoError(t, err)

	invalid := validCreateEvent(t)
	invalid.Resource.ProjectID = "project-private-canary\n"
	err = sink.Emit(context.Background(), invalid)
	require.Error(t, err)
	assert.NotErrorIs(t, err, handlerErr)
	assert.NotContains(t, err.Error(), "project-private-canary")
	assert.Empty(t, handler.Records())

	err = sink.Emit(context.Background(), validCreateEvent(t))
	assert.ErrorIs(t, err, handlerErr)
	assert.Len(t, handler.Records(), 1)
}

func TestNewSlogSinkRejectsNilLogger(t *testing.T) {
	t.Parallel()

	_, err := NewSlogSink(nil)
	assert.Error(t, err)
}

type captureSlogHandler struct {
	mu      sync.Mutex
	records []slog.Record
	err     error
}

func (h *captureSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureSlogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return h.err
}

func (h *captureSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureSlogHandler) WithGroup(string) slog.Handler { return h }

func (h *captureSlogHandler) Records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func recordAttrsJSON(t *testing.T, record slog.Record) []byte {
	t.Helper()
	var output bytes.Buffer
	handler := slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
		if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey || attr.Key == slog.MessageKey) {
			return slog.Attr{}
		}
		return attr
	}})
	require.NoError(t, handler.Handle(context.Background(), record))
	return []byte(strings.TrimSpace(output.String()))
}

func fullCreateEvent(t *testing.T) EnvelopeV1 {
	t.Helper()
	event := validCreateEvent(t)
	event.Request = &RequestRef{ID: "corr-1", Method: "POST", Route: "/api/v1/access-constraints", Surface: "api"}
	event.Initiator = &IdentityRef{Kind: IdentityUser, ID: "initiator-1"}
	event.Executor = &IdentityRef{Kind: IdentitySystem, ID: "executor-1"}
	credential := mustCredentialRef(t, CredentialRefInput{
		Kind: CredentialUAT, ID: "token-1", Name: "deploy",
		BoundaryKind: CredentialBoundaryProject, BoundaryProjectID: "project-1",
		Labels: map[string]string{"purpose": "automation"},
	})
	event.Credential = &credential
	event.Payload = AccessBoundaryPayload{
		Classification: BoundaryTighten,
		ImpactCounts:   &ImpactCounts{Agents: 1, Users: 2, Projects: 3},
		ChangedFields:  []string{"permissions", "subjects"},
	}
	return event
}

func recordAttrs(t *testing.T, record slog.Record) map[string]slog.Value {
	t.Helper()
	attrs := make(map[string]slog.Value)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Resolve()
		return true
	})
	return attrs
}

func groupAttrs(value slog.Value) map[string]slog.Value {
	attrs := make(map[string]slog.Value)
	for _, attr := range value.Group() {
		attrs[attr.Key] = attr.Value.Resolve()
	}
	return attrs
}

func slogRecordMap(t *testing.T, record slog.Record) map[string]any {
	t.Helper()
	result := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		result[attr.Key] = slogValue(t, attr.Value.Resolve())
		return true
	})
	return result
}

func slogValue(t *testing.T, value slog.Value) any {
	t.Helper()
	switch value.Kind() {
	case slog.KindBool:
		return value.Bool()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindString:
		return value.String()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindGroup:
		result := make(map[string]any)
		for _, attr := range value.Group() {
			result[attr.Key] = slogValue(t, attr.Value.Resolve())
		}
		return result
	case slog.KindAny:
		strings, ok := value.Any().([]string)
		require.True(t, ok, "unexpected KindAny value %T", value.Any())
		return strings
	default:
		require.FailNow(t, "unexpected slog value kind", value.Kind().String())
		return nil
	}
}

type captureOTelExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *captureOTelExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		e.records = append(e.records, records[i].Clone())
	}
	return nil
}

func (*captureOTelExporter) Shutdown(context.Context) error { return nil }

func (*captureOTelExporter) ForceFlush(context.Context) error { return nil }

func (e *captureOTelExporter) Records() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

func otelRecordMap(record sdklog.Record) map[string]any {
	result := make(map[string]any)
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		result[string(attr.Key)] = otelValue(attr.Value)
		return true
	})
	return result
}

func otelValue(value attribute.Value) any {
	switch value.Type() {
	case attribute.BOOL:
		return value.AsBool()
	case attribute.INT64:
		return value.AsInt64()
	case attribute.FLOAT64:
		return value.AsFloat64()
	case attribute.STRING:
		return value.AsString()
	case attribute.STRINGSLICE:
		return value.AsStringSlice()
	case attribute.SLICE:
		values := value.AsSlice()
		result := make([]any, len(values))
		for i := range values {
			result[i] = otelValue(values[i])
		}
		return result
	case attribute.MAP:
		result := make(map[string]any)
		for _, attr := range value.AsMap() {
			result[string(attr.Key)] = otelValue(attr.Value)
		}
		return result
	default:
		return value.AsInterface()
	}
}

var _ slog.Handler = (*captureSlogHandler)(nil)
var _ sdklog.Exporter = (*captureOTelExporter)(nil)
