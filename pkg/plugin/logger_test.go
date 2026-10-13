// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugin

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// levelRecorder is a slog.Handler that records each record's level and
// message.
type levelRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

func (h *levelRecorder) only(t *testing.T) slog.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.records, 1)
	return h.records[0]
}

// go-plugin logs "plugin process exited" at ERROR whenever the process
// exited with an error. The adapter keeps genuine crashes at ERROR and
// lowers expected exits (ptone/scion#4869).
func TestHclogAdapter_PluginProcessExitLevel(t *testing.T) {
	tests := []struct {
		name     string
		stopping bool
		err      any
		want     slog.Level
	}{
		{name: "crash stays error", err: "exit status 2", want: slog.LevelError},
		{name: "sigkill stays error", err: "signal: killed", want: slog.LevelError},
		{name: "sigterm outside shutdown warns", err: "signal: terminated", want: slog.LevelWarn},
		{name: "sigint outside shutdown warns", err: "signal: interrupt", want: slog.LevelWarn},
		{name: "error value is read too", err: errors.New("signal: terminated"), want: slog.LevelWarn},
		{name: "sigterm during shutdown is info", stopping: true, err: "signal: terminated", want: slog.LevelInfo},
		{name: "kill during shutdown is info", stopping: true, err: "signal: killed", want: slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &levelRecorder{}
			stopping := tt.stopping
			logger := newHclogAdapter(slog.New(rec), func() bool { return stopping })

			// The args go-plugin passes (client.go, v1.7.0).
			logger.Error(goPluginProcessExitedMsg, "plugin", "/plugins/x", "id", "123", "error", tt.err)

			r := rec.only(t)
			assert.Equal(t, goPluginProcessExitedMsg, r.Message)
			assert.Equal(t, tt.want, r.Level)
		})
	}
}

// The expectation travels through With and Named, which go-plugin uses to
// derive its loggers.
func TestHclogAdapter_DerivedLoggersKeepExitExpectation(t *testing.T) {
	rec := &levelRecorder{}
	logger := newHclogAdapter(slog.New(rec), func() bool { return true }).Named("plugin").With("k", "v")

	logger.Error(goPluginProcessExitedMsg, "error", "exit status 1")

	assert.Equal(t, slog.LevelInfo, rec.only(t).Level)
}

// Other ERROR records are not touched.
func TestHclogAdapter_OtherErrorsStayError(t *testing.T) {
	rec := &levelRecorder{}
	logger := newHclogAdapter(slog.New(rec), func() bool { return true })

	logger.Error("client", "error", "signal: terminated")

	assert.Equal(t, slog.LevelError, rec.only(t).Level)
}

// BeginShutdown and Shutdown both mark plugin process exits as expected.
func TestManager_ShutdownMarksExitsExpected(t *testing.T) {
	mgr := NewManager(nil)
	assert.False(t, mgr.stopping.Load())
	mgr.BeginShutdown()
	assert.True(t, mgr.stopping.Load())

	mgr2 := NewManager(nil)
	mgr2.Shutdown()
	assert.True(t, mgr2.stopping.Load())
}
