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
	// replaceAttr is the composed ReplaceAttr of the JSON handler, used to
	// drop With attrs that would render nothing inside a group.
	replaceAttr func(groups []string, a slog.Attr) slog.Attr
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
		handler:     jsonHandler,
		component:   component,
		hubName:     hubName,
		hostname:    hostname,
		projectID:   projectID,
		version:     version.Short(),
		replaceAttr: opts.ReplaceAttr,
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
	// The special fields go first: if every attr in a group is dropped
	// (zero Attr, ReplaceAttr deletion), the JSON handler truncates the
	// group without restoring its separator, so nothing may follow it.
	inner := make([]slog.Attr, 0, r.NumAttrs()+len(errAttrs))
	r.Attrs(func(a slog.Attr) bool {
		inner = append(inner, a)
		return true
	})
	inner = append(inner, errAttrs...)
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(top...)
	out.AddAttrs(nestInGroups(h.groups, inner)...)
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
	// Inside a group: keep the attrs with the innermost group, minus those
	// that would render nothing (see pruneEmpty). LogValuers are resolved
	// now; ReplaceAttr is applied again on each record.
	last := len(nh.groups) - 1
	path := make([]string, len(nh.groups))
	for i, g := range nh.groups {
		path[i] = g.name
	}
	g := nh.groups[last]
	kept := h.pruneEmpty(path, attrs)
	groupAttrs := make([]slog.Attr, len(g.attrs), len(g.attrs)+len(kept))
	copy(groupAttrs, g.attrs)
	groupAttrs = append(groupAttrs, kept...)
	nh.groups[last] = gcpGroup{name: g.name, attrs: groupAttrs}
	return nh
}

// pruneEmpty returns attrs, with LogValuers resolved, minus those slog's
// JSON handler would render as nothing under the group path groups: zero
// Attrs, attrs that ReplaceAttr deletes, and groups whose members are all
// dropped. slog drops these once when it preformats With attrs. Here they
// are rendered per record, where an empty group followed by a sibling
// would lose its separator and break the JSON, so they are dropped now.
func (h *GCPHandler) pruneEmpty(groups []string, attrs []slog.Attr) []slog.Attr {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			path := groups
			if a.Key != "" {
				path = append(groups[:len(groups):len(groups)], a.Key)
			}
			members := h.pruneEmpty(path, a.Value.Group())
			if len(members) == 0 {
				continue
			}
			kept = append(kept, slog.Attr{Key: a.Key, Value: slog.GroupValue(members...)})
			continue
		}
		r := a
		if h.replaceAttr != nil {
			r = h.replaceAttr(groups, r)
			r.Value = r.Value.Resolve()
		}
		// slog elides an Attr whose key and value are both zero.
		if r.Key == "" && r.Value.Kind() == slog.KindAny && r.Value.Any() == nil {
			continue
		}
		kept = append(kept, a)
	}
	return kept
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
// group's own attrs. Groups with nothing to show are dropped by
// slog.GroupValue, Record.AddAttrs and the JSON handler, as in slog.
func nestInGroups(groups []gcpGroup, attrs []slog.Attr) []slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		members := make([]slog.Attr, 0, len(groups[i].attrs)+len(attrs))
		members = append(members, groups[i].attrs...)
		members = append(members, attrs...)
		attrs = []slog.Attr{{Key: groups[i].name, Value: slog.GroupValue(members...)}}
	}
	return attrs
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
