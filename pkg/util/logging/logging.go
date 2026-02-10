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
)

// Standard attribute keys
const (
	AttrComponent = "component"
	AttrTraceID   = "trace_id"
	AttrGroveID   = "grove_id"
	AttrAgentID   = "agent_id"
	AttrRequestID = "request_id"
	AttrUserID    = "user_id"
)

// Setup initializes the global logger.
// component is the name of the service (e.g., "hub", "runtimebroker").
// debug enables DEBUG level logging.
// useGCP formats logs for Google Cloud Logging.
func Setup(component string, debug bool, useGCP bool) {
	handler := createBaseHandler(component, debug, useGCP)
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

// createBaseHandler creates the base slog handler for local logging.
func createBaseHandler(component string, debug bool, useGCP bool) slog.Handler {
	level := slog.LevelInfo
	if debug || os.Getenv("SCION_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	if useGCP {
		return NewGCPHandler(os.Stdout, opts, component)
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

// Logger returns a logger with contextual metadata from the context.
// Currently it just returns the default logger.
func Logger(ctx context.Context) *slog.Logger {
	return slog.Default()
}

// Handler with component name
type componentHandler struct {
	slog.Handler
	component string
}

func (h *componentHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.String(AttrComponent, h.component))
	return h.Handler.Handle(ctx, r)
}
