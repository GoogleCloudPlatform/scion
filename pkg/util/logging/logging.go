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
	"log/slog"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// Standard attribute keys
const (
	AttrComponent = "component"
	AttrSubsystem = "subsystem"
	AttrTraceID   = "trace_id"
	AttrProjectID = "project_id"
	AttrAgentID   = "agent_id"
	AttrBrokerID  = "broker_id"
	AttrRequestID = "request_id"
	AttrUserID    = "user_id"
	AttrAuthType  = "auth_type"
)

// Setup initializes the global logger.
// component is the name of the service (e.g., "hub", "runtimebroker").
// debug enables DEBUG level logging (flag precedence; see ApplyDebugFlag).
// useGCP formats logs for Google Cloud Logging.
//
// The level is otherwise taken from the shared level state in package
// loglevel (SCION_LOG_LEVEL, the deprecated SCION_DEBUG alias, or a
// settings value applied with SetLogLevelSetting), including per-component
// levels keyed on Subsystem names.
func Setup(component string, debug bool, useGCP bool) {
	ApplyDebugFlag(debug)
	handler := newLevelFilter(createBaseHandler(component, useGCP, ""))
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

// ApplyDebugFlag maps a --debug style flag onto the shared level state: when
// debug is true the default level becomes debug at flag precedence. It is a
// no-op when debug is false.
func ApplyDebugFlag(debug bool) {
	if debug {
		loglevel.EnableDebug(loglevel.SourceFlag)
	}
}

// ParseLevelSpec parses a SCION_LOG_LEVEL style level spec, such as
// "info,hub.auth=debug". See package loglevel for the syntax.
func ParseLevelSpec(s string) (loglevel.Spec, error) {
	return loglevel.ParseLevelSpec(s)
}

// SetLogLevelSetting applies a settings-file level spec (for example
// server.log_level) at setting precedence: it takes effect only when neither
// a flag nor the environment chose a level, and it may be called again on
// settings reload. An empty spec reverts the setting to the built-in default
// (info), again only when neither a flag nor the environment chose a level.
//
// The level filter that Setup and SetupWithOTel install picks up the change
// on the next record, as do handlers built with ResolveLogLeveler (the main
// CloudHandler, the request logger and the message logger). Handlers built
// with a fixed slog.Level keep the floor they were constructed with.
func SetLogLevelSetting(spec string) (applied bool, err error) {
	return loglevel.SetSetting(spec)
}

// EffectiveLevel returns the effective level for a component (a Subsystem
// name such as "hub.auth"); an empty component returns the default level.
func EffectiveLevel(component string) slog.Level {
	return loglevel.Effective(component)
}

// createBaseHandler creates the base slog handler for local logging. It
// passes every record; callers wrap it with newLevelFilter so the shared
// level state decides what is emitted.
func createBaseHandler(component string, useGCP bool, hubName string) slog.Handler {
	opts := &slog.HandlerOptions{
		Level: passAllLevel,
	}

	if useGCP {
		return NewGCPHandler(os.Stdout, opts, component, hubName)
	}

	// Default to JSON handler for structured logging
	return slog.NewJSONHandler(os.Stdout, opts).WithAttrs([]slog.Attr{
		slog.String(AttrComponent, component),
	})
}

// WithMetadata returns a context with the provided metadata attached as slog attributes.
func WithMetadata(ctx context.Context, attrs ...slog.Attr) context.Context {
	// This is a placeholder for context-based logging if needed.
	// For now, we can just use slog.With() on the logger.
	return ctx
}

// Logger returns a logger enriched with request-scoped metadata from the context.
// When called within an HTTP handler wrapped by RequestLogMiddleware, the returned
// logger automatically includes request_id, trace_id, project_id, and agent_id.
func Logger(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if meta := RequestMetaFromContext(ctx); meta != nil {
		meta.mu.Lock()
		var attrs []any
		if meta.RequestID != "" {
			attrs = append(attrs, slog.String(AttrRequestID, meta.RequestID))
		}
		if meta.TraceID != "" {
			attrs = append(attrs, slog.String(AttrTraceID, meta.TraceID))
		}
		if meta.ProjectID != "" {
			attrs = append(attrs, slog.String(AttrProjectID, meta.ProjectID))
		}
		if meta.AgentID != "" {
			attrs = append(attrs, slog.String(AttrAgentID, meta.AgentID))
		}
		if meta.BrokerID != "" {
			attrs = append(attrs, slog.String(AttrBrokerID, meta.BrokerID))
		}
		meta.mu.Unlock()
		if len(attrs) > 0 {
			l = l.With(attrs...)
		}
	}
	return l
}

// RequestIDFromContext returns the request ID recorded on the request's
// RequestMeta (installed by RequestLogMiddleware), or "" when none is set —
// for example, code running outside an HTTP request, or a test that never
// installed request metadata. E.2a (ptone/scion#2127) uses this as the
// correlation ID shared by request logs, decision audit, and mutation audit
// for a single request.
func RequestIDFromContext(ctx context.Context) string {
	meta := RequestMetaFromContext(ctx)
	if meta == nil {
		return ""
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return meta.RequestID
}

// Subsystem returns a child logger with the given subsystem attribute.
// The returned logger inherits the root component attribute and adds
// a "subsystem" field for finer-grained log filtering.
func Subsystem(name string) *slog.Logger {
	return slog.Default().With(slog.String(AttrSubsystem, name))
}
