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
	handler   slog.Handler
	component string
	hostname  string
	projectID string
	preAttrs  []slog.Attr // tracked for label promotion
}

// NewGCPHandler creates a new GCPHandler.
func NewGCPHandler(w io.Writer, opts *slog.HandlerOptions, component string) *GCPHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}

	// Hostname for host_logs as requested in design
	hostname, _ := os.Hostname()
	projectID := resolveProjectID()

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
			// Suppress empty messages (e.g. HTTP request logs).
			if a.Value.String() == "" {
				return slog.Attr{}
			}
			return slog.Attr{Key: GCPKeyMessage, Value: a.Value}
		case slog.TimeKey:
			return slog.Attr{Key: GCPKeyTimestamp, Value: a.Value}
		case AttrTraceID:
			traceID := NormalizeTraceID(a.Value.String())
			if traceID == "" {
				return slog.Attr{}
			}
			return slog.String(GCPKeyTrace, FormatCloudTraceResource(projectID, traceID))
		}
		return a
	}

	// Create JSON handler
	jsonHandler := slog.NewJSONHandler(w, opts)

	return &GCPHandler{
		handler:   jsonHandler,
		component: component,
		hostname:  hostname,
		projectID: projectID,
	}
}

func (h *GCPHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *GCPHandler) Handle(ctx context.Context, r slog.Record) error {
	// Build labels dynamically, promoting agent_id/project_id
	labels := map[string]string{
		"component": h.component,
	}
	if h.hostname != "" {
		labels["hostname"] = h.hostname
		labels["hub"] = h.hostname
	}
	for _, a := range h.preAttrs {
		promoteAttrToLabels(labels, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		promoteAttrToLabels(labels, a)
		return true
	})
	if traceID := extractTraceIDFromAttrs(h.preAttrs, r); traceID != "" {
		labels[gcpTraceIDLabelKey] = traceID
	}
	r.AddAttrs(slog.Any(GCPKeyLabels, labels))

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
	newPreAttrs := make([]slog.Attr, len(h.preAttrs)+len(attrs))
	copy(newPreAttrs, h.preAttrs)
	copy(newPreAttrs[len(h.preAttrs):], attrs)
	return &GCPHandler{
		handler:   h.handler.WithAttrs(attrs),
		component: h.component,
		hostname:  h.hostname,
		projectID: h.projectID,
		preAttrs:  newPreAttrs,
	}
}

func (h *GCPHandler) WithGroup(name string) slog.Handler {
	return &GCPHandler{
		handler:   h.handler.WithGroup(name),
		component: h.component,
		hostname:  h.hostname,
		projectID: h.projectID,
		preAttrs:  h.preAttrs,
	}
}

func extractTraceIDFromAttrs(preAttrs []slog.Attr, r slog.Record) string {
	var traceID string
	for _, a := range preAttrs {
		if a.Key == AttrTraceID {
			traceID = NormalizeTraceID(a.Value.String())
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == AttrTraceID {
			traceID = NormalizeTraceID(a.Value.String())
		}
		return true
	})
	return traceID
}
