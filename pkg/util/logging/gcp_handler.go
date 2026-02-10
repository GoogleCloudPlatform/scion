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
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
)

// GCP-specific keys for Cloud Logging LogEntry
const (
	GCPKeySeverity       = "severity"
	GCPKeyMessage        = "message"
	GCPKeyTimestamp      = "timestamp"
	GCPKeyLabels         = "logging.googleapis.com/labels"
	GCPKeySourceLocation = "logging.googleapis.com/sourceLocation"
	GCPKeyTrace          = "logging.googleapis.com/trace"
)

// Map slog levels to GCP severity strings
var levelToSeverity = map[slog.Level]string{
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARNING",
	slog.LevelError: "ERROR",
}

// GCPHandler is a slog.Handler that formats logs for Google Cloud Logging.
type GCPHandler struct {
	handler slog.Handler
}

// NewGCPHandler creates a new GCPHandler.
func NewGCPHandler(w io.Writer, opts *slog.HandlerOptions, component string) *GCPHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}

	// Hostname for host_logs as requested in design
	hostname, _ := os.Hostname()

	originalReplace := opts.ReplaceAttr
	opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if originalReplace != nil {
			a = originalReplace(groups, a)
		}

		switch a.Key {
		case slog.LevelKey:
			level := a.Value.Any().(slog.Level)
			return slog.String(GCPKeySeverity, levelToSeverity[level])
		case slog.MessageKey:
			return slog.Attr{Key: GCPKeyMessage, Value: a.Value}
		case slog.TimeKey:
			return slog.Attr{Key: GCPKeyTimestamp, Value: a.Value}
		case AttrTraceID:
			return slog.Attr{Key: GCPKeyTrace, Value: a.Value}
		}
		return a
	}

	// Create JSON handler
	jsonHandler := slog.NewJSONHandler(w, opts)

	// Add default labels
	labels := map[string]string{
		"component": component,
	}
	if hostname != "" {
		labels["hostname"] = hostname
	}

	return &GCPHandler{
		handler: jsonHandler.WithAttrs([]slog.Attr{
			slog.Any(GCPKeyLabels, labels),
		}),
	}
}

func (h *GCPHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *GCPHandler) Handle(ctx context.Context, r slog.Record) error {
	// Add source location if requested or by default
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		r.AddAttrs(slog.Any(GCPKeySourceLocation, map[string]string{
			"file":     f.File,
			"line":     strconv.Itoa(f.Line),
			"function": f.Function,
		}))
	}

	return h.handler.Handle(ctx, r)
}

func (h *GCPHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &GCPHandler{handler: h.handler.WithAttrs(attrs)}
}

func (h *GCPHandler) WithGroup(name string) slog.Handler {
	return &GCPHandler{handler: h.handler.WithGroup(name)}
}
