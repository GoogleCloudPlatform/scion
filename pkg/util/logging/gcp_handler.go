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

	gcplog "cloud.google.com/go/logging"

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

// Keys of the Error Reporting fields written on ERROR+ entries.
const (
	gcpKeyStackTrace = "stack_trace"
	gcpKeyType       = "@type"
)

// severityNames maps the Cloud Logging severities slogLevelToSeverity
// returns to their JSON names.
var severityNames = map[gcplog.Severity]string{
	gcplog.Debug:   "DEBUG",
	gcplog.Info:    "INFO",
	gcplog.Warning: "WARNING",
	gcplog.Error:   "ERROR",
}

// levelToSeverity returns the Cloud Logging severity name for level. A
// level between the standard ones maps to the nearest lower standard level,
// as in CloudHandler; levels below Debug map to DEBUG.
func levelToSeverity(level slog.Level) string {
	return severityNames[slogLevelToSeverity(level)]
}

// liftedKey reports whether an attr with this key, from With or the record,
// is written at the top level of the entry, once per entry, even when a
// group is open. These are the fields Cloud Logging and Error Reporting
// read only from the top level.
func liftedKey(key string) bool {
	return key == gcpKeyStackTrace || key == gcpKeyType || key == AttrTraceID
}

// GCPHandler is a slog.Handler that formats logs for Google Cloud Logging.
//
// Groups opened with WithGroup are tracked here instead of on the wrapped
// JSON handler, so the Cloud Logging special fields (labels, source location
// and serviceContext) stay at the top level of each entry while user
// attributes nest inside their groups. stack_trace, @type and trace_id
// attributes are written at the top level too, once each (see liftedKey).
type GCPHandler struct {
	handler   slog.Handler // ungrouped; carries attrs added before the first group
	component string
	hubName   string
	hostname  string
	projectID string
	version   string
	preAttrs  []slog.Attr // tracked for label promotion
	groups    []gcpGroup  // open groups, outermost first
	// lifted holds the With attrs written at the top level by Handle (see
	// liftedKey), resolved and pruned, oldest first.
	lifted []slog.Attr
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
			// Only the top-level record level carries a slog.Level. A caller
			// attr keyed "level" (any type, or inside a group) is left as is.
			level, ok := a.Value.Any().(slog.Level)
			if !ok || len(groups) != 0 {
				return a
			}
			return slog.String(GCPKeySeverity, levelToSeverity(level))
		case slog.MessageKey:
			// Suppress empty messages (e.g. HTTP request logs).
			if a.Value.String() == "" {
				return slog.Attr{}
			}
			return slog.Attr{Key: GCPKeyMessage, Value: a.Value}
		case slog.TimeKey:
			return slog.Attr{Key: GCPKeyTimestamp, Value: a.Value}
		case AttrTraceID:
			// Only a top-level trace ID becomes the trace field. One
			// inside a caller's group value is left as is.
			if len(groups) != 0 {
				return a
			}
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
		for _, a := range h.lifted {
			if a.Key == gcpKeyStackTrace {
				hasST = true
			}
			if a.Key == gcpKeyType {
				hasType = true
			}
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == gcpKeyStackTrace {
				hasST = true
			}
			if a.Key == gcpKeyType {
				hasType = true
			}
			return !hasST || !hasType
		})

		if !hasST {
			errAttrs = append(errAttrs, slog.String(gcpKeyStackTrace, string(debug.Stack())))
		}
		if !hasType {
			errAttrs = append(errAttrs, slog.String(gcpKeyType, errorReportingType))
		}
	}

	// stack_trace, @type and trace_id (see liftedKey) from With and from the
	// record are written top-level once each, the record's value winning
	// over With and a later With over an earlier one (see lastPerKey). The
	// record's other attrs go top-level when no group is open, else in the
	// open groups. In a group, the special fields go first: if every attr in
	// a group is dropped (zero Attr, ReplaceAttr deletion), the JSON handler
	// truncates the group without restoring its separator, so nothing may
	// follow it.
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	ungrouped := len(h.groups) == 0
	special := h.lifted[:len(h.lifted):len(h.lifted)] // append copies
	var inner []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		switch {
		case liftedKey(a.Key):
			special = append(special, a)
		case ungrouped:
			out.AddAttrs(plainLevelAttr(a))
		default:
			inner = append(inner, a)
		}
		return true
	})
	out.AddAttrs(top...)
	out.AddAttrs(lastPerKey(special)...)
	out.AddAttrs(errAttrs...)
	if !ungrouped {
		out.AddAttrs(nestInGroups(h.groups, inner)...)
	}
	return h.handler.Handle(ctx, out)
}

// lastPerKey returns one attr per key of attrs: the last one with that key,
// at the position of the first.
func lastPerKey(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
next:
	for _, a := range attrs {
		for i := range out {
			if out[i].Key == a.Key {
				out[i] = a
				continue next
			}
		}
		out = append(out, a)
	}
	return out
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
	// stack_trace, @type and trace_id are kept apart, with or without a
	// group, and written top-level by Handle (see liftedKey). They are
	// resolved and pruned now as at the top level, where they render.
	var lifted, rest []slog.Attr
	for _, a := range attrs {
		if liftedKey(a.Key) {
			lifted = append(lifted, a)
		} else {
			rest = append(rest, a)
		}
	}
	lifted = h.pruneEmpty(nil, lifted)
	if len(lifted) > 0 {
		nh.lifted = make([]slog.Attr, len(h.lifted), len(h.lifted)+len(lifted))
		copy(nh.lifted, h.lifted)
		nh.lifted = append(nh.lifted, lifted...)
	}
	if len(rest) == 0 {
		return nh
	}
	if len(h.groups) == 0 {
		plain := make([]slog.Attr, len(rest))
		for i, a := range rest {
			plain[i] = plainLevelAttr(a)
		}
		nh.handler = h.handler.WithAttrs(plain)
		return nh
	}
	// Inside a group: keep the other attrs with the innermost group, minus
	// those that would render nothing (see pruneEmpty). LogValuers are
	// resolved now; ReplaceAttr is applied again on each record.
	last := len(nh.groups) - 1
	path := make([]string, len(nh.groups))
	for i, g := range nh.groups {
		path[i] = g.name
	}
	g := nh.groups[last]
	kept := h.pruneEmpty(path, rest)
	groupAttrs := make([]slog.Attr, len(g.attrs), len(g.attrs)+len(kept))
	copy(groupAttrs, g.attrs)
	groupAttrs = append(groupAttrs, kept...)
	nh.groups[last] = gcpGroup{name: g.name, attrs: groupAttrs}
	return nh
}

// plainLevelAttr returns a, except that a slog.Level value under the key
// "level", directly or in a group value with an empty key (which the JSON
// handler inlines), is replaced by its name. The JSON output is the same,
// but the ReplaceAttr installed by NewGCPHandler, which cannot tell such an
// attr from the record's own level, no longer turns it into a second
// severity key. Only used for top-level attrs; grouped ones are left as is
// there.
func plainLevelAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if a.Key == "" && v.Kind() == slog.KindGroup {
		members := v.Group()
		plain := make([]slog.Attr, len(members))
		for i, m := range members {
			plain[i] = plainLevelAttr(m)
		}
		return slog.Attr{Value: slog.GroupValue(plain...)}
	}
	if a.Key == slog.LevelKey {
		if level, ok := v.Any().(slog.Level); ok {
			return slog.String(a.Key, level.String())
		}
	}
	return a
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
