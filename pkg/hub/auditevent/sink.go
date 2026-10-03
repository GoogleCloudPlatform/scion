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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"sync"
)

// Sink receives validated audit envelopes.
type Sink interface {
	Emit(context.Context, EnvelopeV1) error
}

// SlogSink dispatches validated audit envelopes through an injected logger's
// configured handler. Handler errors report synchronous dispatch failure only;
// they do not acknowledge delivery by an external logging backend.
type SlogSink struct {
	logger *slog.Logger
}

// NewSlogSink creates a structured-log audit sink.
func NewSlogSink(logger *slog.Logger) (*SlogSink, error) {
	if logger == nil {
		return nil, errors.New("audit slog sink requires a logger")
	}
	return &SlogSink{logger: logger}, nil
}

// Emit validates and snapshots one event before synchronous handler dispatch.
func (s *SlogSink) Emit(ctx context.Context, event EnvelopeV1) error {
	snapshot := newRenderSnapshot(event)
	attrs, err := snapshot.slogAttrs()
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if snapshot.event.Severity == SeverityWarning {
		level = slog.LevelWarn
	}
	handler := s.logger.Handler()
	if !handler.Enabled(ctx, level) {
		return nil
	}
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:]) // skip runtime.Callers and SlogSink.Emit
	record := slog.NewRecord(snapshot.event.OccurredAt, level, EventName, pcs[0])
	record.AddAttrs(attrs...)
	if err := handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("dispatch audit event: %w", err)
	}
	return nil
}

func (snapshot renderSnapshot) slogAttrs() ([]slog.Attr, error) {
	if err := snapshot.validate(); err != nil {
		return nil, err
	}

	serialized := snapshot.serialized
	attrs := []slog.Attr{
		slog.Int("schema_version", serialized.SchemaVersion),
		slog.String("event_id", serialized.EventID),
		slog.String("occurred_at", serialized.OccurredAt),
		slog.String("family", serialized.Family),
		slog.String("action", serialized.Action),
		slog.String("phase", string(serialized.Phase)),
	}
	if serialized.Outcome != "" {
		attrs = append(attrs, slog.String("outcome", string(serialized.Outcome)))
	}
	attrs = append(attrs,
		slog.String("severity", string(serialized.Severity)),
		slog.String("correlation_id", serialized.CorrelationID),
	)
	if serialized.CausationID != "" {
		attrs = append(attrs, slog.String("causation_id", serialized.CausationID))
	}
	if serialized.Request != nil {
		attrs = append(attrs, slog.Attr{Key: "request", Value: requestSlogValue(serialized.Request)})
	}
	if serialized.Initiator != nil {
		attrs = append(attrs, slog.Attr{Key: "initiator", Value: identitySlogValue(serialized.Initiator)})
	}
	if serialized.Principal != nil {
		attrs = append(attrs, slog.Attr{Key: "principal", Value: identitySlogValue(serialized.Principal)})
	}
	if serialized.Executor != nil {
		attrs = append(attrs, slog.Attr{Key: "executor", Value: identitySlogValue(serialized.Executor)})
	}
	if serialized.Credential != nil {
		attrs = append(attrs, slog.Attr{Key: "credential", Value: credentialSlogValue(serialized.Credential)})
	}
	if serialized.Resource != nil {
		attrs = append(attrs, slog.Attr{Key: "resource", Value: resourceSlogValue(serialized.Resource)})
	}
	payload, err := payloadSlogValue(serialized.Payload)
	if err != nil {
		return nil, err
	}
	attrs = append(attrs, slog.Attr{Key: "payload", Value: payload})
	return attrs, nil
}

func requestSlogValue(request *RequestRef) slog.Value {
	attrs := make([]slog.Attr, 0, 4)
	if request.ID != "" {
		attrs = append(attrs, slog.String("id", request.ID))
	}
	if request.Method != "" {
		attrs = append(attrs, slog.String("method", request.Method))
	}
	if request.Route != "" {
		attrs = append(attrs, slog.String("route", request.Route))
	}
	if request.Surface != "" {
		attrs = append(attrs, slog.String("surface", request.Surface))
	}
	return slog.GroupValue(attrs...)
}

func identitySlogValue(identity *IdentityRef) slog.Value {
	return slog.GroupValue(
		slog.String("kind", string(identity.Kind)),
		slog.String("id", identity.ID),
	)
}

func credentialSlogValue(credential *serializedCredentialRef) slog.Value {
	attrs := []slog.Attr{slog.String("kind", string(credential.Kind))}
	if credential.ID != "" {
		attrs = append(attrs, slog.String("id", credential.ID))
	}
	if credential.Name != "" {
		attrs = append(attrs, slog.String("name", credential.Name))
	}
	if credential.BoundaryKind != "" {
		attrs = append(attrs, slog.String("boundary_kind", string(credential.BoundaryKind)))
	}
	if credential.BoundaryProjectID != "" {
		attrs = append(attrs, slog.String("boundary_project_id", credential.BoundaryProjectID))
	}
	if credential.Labels != nil {
		keys := sortedKeys(credential.Labels)
		labels := make([]slog.Attr, 0, len(keys))
		for _, key := range keys {
			labels = append(labels, slog.String(key, credential.Labels[key]))
		}
		attrs = append(attrs, slog.Attr{Key: "labels", Value: slog.GroupValue(labels...)})
	}
	return slog.GroupValue(attrs...)
}

func resourceSlogValue(resource *ResourceRef) slog.Value {
	attrs := []slog.Attr{
		slog.String("kind", resource.Kind),
		slog.String("id", resource.ID),
	}
	if resource.ProjectID != "" {
		attrs = append(attrs, slog.String("project_id", resource.ProjectID))
	}
	return slog.GroupValue(attrs...)
}

func payloadSlogValue(payload map[string]any) (slog.Value, error) {
	keys := sortedKeys(payload)
	attrs := make([]slog.Attr, 0, len(keys))
	for _, key := range keys {
		value, err := payloadLeafSlogValue(key, payload[key])
		if err != nil {
			return slog.Value{}, err
		}
		attrs = append(attrs, slog.Attr{Key: key, Value: value})
	}
	return slog.GroupValue(attrs...), nil
}

func payloadLeafSlogValue(name string, value any) (slog.Value, error) {
	switch value := value.(type) {
	case string:
		return slog.StringValue(value), nil
	case int64:
		return slog.Int64Value(value), nil
	case []string:
		return slog.AnyValue(value), nil
	case ImpactCounts:
		return slog.GroupValue(
			slog.Uint64("agents", uint64(value.Agents)),
			slog.Uint64("users", uint64(value.Users)),
			slog.Uint64("projects", uint64(value.Projects)),
		), nil
	default:
		return slog.Value{}, invalid("payload."+name, "must have a handler-portable slog representation")
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CaptureSink is a concurrency-safe test sink that stores the exact rendered
// records which would cross the audit boundary.
type CaptureSink struct {
	mu      sync.RWMutex
	records [][]byte
}

// NewCaptureSink creates an empty capture sink.
func NewCaptureSink() *CaptureSink {
	return &CaptureSink{}
}

// Emit validates, renders, and captures one event.
func (s *CaptureSink) Emit(_ context.Context, event EnvelopeV1) error {
	record, err := Render(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.records = append(s.records, append([]byte(nil), record...))
	s.mu.Unlock()
	return nil
}

// Records returns a defensive copy of every captured serialized event.
func (s *CaptureSink) Records() [][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([][]byte, len(s.records))
	for i, record := range s.records {
		records[i] = append([]byte(nil), record...)
	}
	return records
}

var _ Sink = (*CaptureSink)(nil)
var _ Sink = (*SlogSink)(nil)
