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
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

var _ slog.Handler = (*captureSlogHandler)(nil)
