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
	"runtime/debug"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/version"
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

// errorReportingType is the @type value that tells GCP Error Reporting
// to pick up a log entry as a reportable error event.
const errorReportingType = "type.googleapis.com/google.devtools.clouderrorreporting.v1beta1.ReportedErrorEvent"

// Map slog levels to GCP severity strings
var levelToSeverity = map[slog.Level]string{
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARNING",
	slog.LevelError: "ERROR",
}

// GCPHandler is a slog.Handler that formats logs for Google Cloud Logging.
//
// Groups opened with WithGroup are tracked here instead of on the wrapped
// JSON handler, so the Cloud Logging special fields (labels, source location
// and serviceContext) stay at the top level of each entry while user
// attributes nest inside their groups.
type GCPHandler struct {
	handler   slog.Handler // ungrouped; carries attrs added before the first group
	component string
	hubName   string
	hostname  string
	projectID string
	version   string
	preAttrs  []slog.Attr // tracked for label promotion
	groups    []gcpGroup  // open groups, outermost first
}

// gcpGroup is a group opened with WithGroup and the attrs added inside it.
type gcpGroup struct {
	name  string
	attrs []slog.Attr
}

// NewGCPHandler creates a new GCPHandler.
func NewGCPHandler(w io.Writer, opts *slog.HandlerOptions, component, hubName string) *GCPHandler {
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
		hubName:   hubName,
		hostname:  hostname,
		projectID: projectID,
		version:   version.Short(),
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
	addHubLabel(labels, h.hubName)
	if h.hostname != "" {
		labels["node"] = h.hostname
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
	top := []slog.Attr{slog.Any(GCPKeyLabels, labels)}

	// Add source location if requested or by default
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		top = append(top, slog.Any(GCPKeySourceLocation, map[string]string{
			"file":     f.File,
			"line":     strconv.Itoa(f.Line),
			"function": f.Function,
		}))
	}

	// Add serviceContext for GCP Error Reporting (all levels)
	top = append(top, slog.Group("serviceContext",
		slog.String("service", h.component),
		slog.String("version", h.version),
	))

	// ERROR+ only: stack trace and @type for GCP Error Reporting
	var errAttrs []slog.Attr
	if r.Level >= slog.LevelError {
		hasST := false
		hasType := false
		for _, a := range h.preAttrs {
			if a.Key == "stack_trace" {
				hasST = true
			}
			if a.Key == "@type" {
				hasType = true
			}
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "stack_trace" {
				hasST = true
			}
			if a.Key == "@type" {
				hasType = true
			}
			return !hasST || !hasType
		})

		if !hasST {
			errAttrs = append(errAttrs, slog.String("stack_trace", string(debug.Stack())))
		}
		if !hasType {
			errAttrs = append(errAttrs, slog.String("@type", errorReportingType))
		}
	}

	if len(h.groups) == 0 {
		r.AddAttrs(top...)
		r.AddAttrs(errAttrs...)
		return h.handler.Handle(ctx, r)
	}

	// Grouped: nest the record attrs (and stack_trace/@type, as before) in
	// the open groups ourselves, and keep the special fields top-level.
	inner := make([]slog.Attr, 0, r.NumAttrs()+len(errAttrs))
	r.Attrs(func(a slog.Attr) bool {
		inner = append(inner, a)
		return true
	})
	inner = append(inner, errAttrs...)
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(nestInGroups(h.groups, inner)...)
	out.AddAttrs(top...)
	return h.handler.Handle(ctx, out)
}

func (h *GCPHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	newPreAttrs := make([]slog.Attr, len(h.preAttrs)+len(attrs))
	copy(newPreAttrs, h.preAttrs)
	copy(newPreAttrs[len(h.preAttrs):], attrs)
	nh := h.clone()
	nh.preAttrs = newPreAttrs
	if len(h.groups) == 0 {
		nh.handler = h.handler.WithAttrs(attrs)
		return nh
	}
	// Inside a group: keep the attrs with the innermost group. Resolve
	// LogValuers now, as the JSON handler does for WithAttrs.
	last := len(nh.groups) - 1
	g := nh.groups[last]
	groupAttrs := make([]slog.Attr, len(g.attrs), len(g.attrs)+len(attrs))
	copy(groupAttrs, g.attrs)
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		groupAttrs = append(groupAttrs, a)
	}
	nh.groups[last] = gcpGroup{name: g.name, attrs: groupAttrs}
	return nh
}

func (h *GCPHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := h.clone()
	nh.groups = append(nh.groups, gcpGroup{name: name})
	return nh
}

// clone returns a copy of h whose groups slice can be changed without
// affecting h.
func (h *GCPHandler) clone() *GCPHandler {
	nh := *h
	nh.groups = make([]gcpGroup, len(h.groups), len(h.groups)+1)
	copy(nh.groups, h.groups)
	return &nh
}

// nestInGroups wraps attrs in groups, innermost last, prepending each
// group's own attrs. Like slog's JSON handler, a group with nothing to
// show is omitted.
func nestInGroups(groups []gcpGroup, attrs []slog.Attr) []slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		members := make([]slog.Attr, 0, len(groups[i].attrs)+len(attrs))
		members = append(members, groups[i].attrs...)
		members = append(members, attrs...)
		v := slog.GroupValue(members...)
		if !hasVisibleAttr(v.Group()) {
			attrs = nil
			continue
		}
		attrs = []slog.Attr{{Key: groups[i].name, Value: v}}
	}
	return attrs
}

// hasVisibleAttr reports whether any attr would be written: not the zero
// Attr and not an empty group.
func hasVisibleAttr(attrs []slog.Attr) bool {
	for _, a := range attrs {
		if a.Equal(slog.Attr{}) {
			continue
		}
		if v := a.Value.Resolve(); v.Kind() == slog.KindGroup && !hasVisibleAttr(v.Group()) {
			continue
		}
		return true
	}
	return false
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
