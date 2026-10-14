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

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// logGCPEntry runs logFn against a fresh GCPHandler and returns the decoded
// JSON entry. A panic inside the handler is reported as a test failure.
func logGCPEntry(t *testing.T, logFn func(*slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(NewGCPHandler(&buf, opts, "test-component", ""))

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("GCPHandler panicked: %v", r)
			}
		}()
		logFn(logger)
	}()

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON %q: %v", buf.String(), err)
	}
	return data
}

// TestGCPHandler_TopLevelNonLevelValueKeptAsAttr verifies that a top-level
// caller attr keyed "level" with a non-slog.Level value does not panic, is
// logged as given, and does not change the record's severity.
func TestGCPHandler_TopLevelNonLevelValueKeptAsAttr(t *testing.T) {
	data := logGCPEntry(t, func(l *slog.Logger) {
		l.Warn("msg", "level", "some string")
	})

	if got := data[GCPKeySeverity]; got != "WARNING" {
		t.Errorf("severity = %v, want WARNING", got)
	}
	if got := data[slog.LevelKey]; got != "some string" {
		t.Errorf("level attr = %v, want %q", got, "some string")
	}
}

// TestGCPHandler_GroupedLevelAttrUnchanged verifies that an attr keyed
// "level" inside a group is left as is, whatever its type, and is never
// rewritten into a severity.
func TestGCPHandler_GroupedLevelAttrUnchanged(t *testing.T) {
	tests := []struct {
		name  string
		logFn func(*slog.Logger)
		want  any
	}{
		{
			name:  "inline group string",
			logFn: func(l *slog.Logger) { l.Info("msg", slog.Group("g", slog.String("level", "custom"))) },
			want:  "custom",
		},
		{
			name:  "inline group slog.Level",
			logFn: func(l *slog.Logger) { l.Info("msg", slog.Group("g", slog.Any("level", slog.LevelError))) },
			want:  "ERROR",
		},
		{
			name:  "WithGroup int",
			logFn: func(l *slog.Logger) { l.WithGroup("g").Info("msg", "level", 7) },
			want:  float64(7),
		},
		{
			name:  "WithGroup slog.Level",
			logFn: func(l *slog.Logger) { l.WithGroup("g").Info("msg", "level", slog.LevelWarn) },
			want:  "WARN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := logGCPEntry(t, tt.logFn)

			if got := data[GCPKeySeverity]; got != "INFO" {
				t.Errorf("severity = %v, want INFO", got)
			}
			g, ok := data["g"].(map[string]any)
			if !ok {
				t.Fatalf("group g missing or not an object: %v", data["g"])
			}
			if got := g[slog.LevelKey]; got != tt.want {
				t.Errorf("g.level = %#v, want %#v", got, tt.want)
			}
			if _, ok := g[GCPKeySeverity]; ok {
				t.Errorf("g.severity present; grouped level attr must not be rewritten: %v", g)
			}
		})
	}
}

// TestGCPHandler_SeverityMapping verifies that the record level maps to the
// Cloud Logging severity for each standard level.
func TestGCPHandler_SeverityMapping(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.level.String(), func(t *testing.T) {
			data := logGCPEntry(t, func(l *slog.Logger) {
				l.Log(t.Context(), tt.level, "msg")
			})
			if got := data[GCPKeySeverity]; got != tt.want {
				t.Errorf("severity = %v, want %s", got, tt.want)
			}
			if _, ok := data[slog.LevelKey]; ok {
				t.Errorf("raw level key present alongside severity: %v", data[slog.LevelKey])
			}
		})
	}
}
