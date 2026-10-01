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
	record := slog.NewRecord(snapshot.event.OccurredAt, level, EventName, 0)
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
		attrs = append(attrs, slog.Any("request", serialized.Request))
	}
	if serialized.Initiator != nil {
		attrs = append(attrs, slog.Any("initiator", serialized.Initiator))
	}
	if serialized.Principal != nil {
		attrs = append(attrs, slog.Any("principal", serialized.Principal))
	}
	if serialized.Executor != nil {
		attrs = append(attrs, slog.Any("executor", serialized.Executor))
	}
	if serialized.Credential != nil {
		attrs = append(attrs, slog.Any("credential", serialized.Credential))
	}
	if serialized.Resource != nil {
		attrs = append(attrs, slog.Any("resource", serialized.Resource))
	}
	attrs = append(attrs, slog.Any("payload", serialized.Payload))
	return attrs, nil
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
