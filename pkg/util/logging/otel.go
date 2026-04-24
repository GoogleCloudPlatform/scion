/*
Copyright 2025 The Scion Authors.
*/

package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"
)

// multiHandler sends logs to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

// newMultiHandler creates a handler that writes to all provided handlers.
func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	// Filter out nil handlers
	filtered := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	return &multiHandler{handlers: filtered}
}

// Enabled implements slog.Handler.
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle implements slog.Handler.
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				// Log errors don't propagate - just continue to next handler
				continue
			}
		}
	}
	return nil
}

// WithAttrs implements slog.Handler.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

// WithGroup implements slog.Handler.
func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

// OTelConfig holds configuration for OTel logging integration.
type OTelConfig struct {
	// Endpoint is the OTLP endpoint for logs.
	Endpoint string
	// Protocol is the transport protocol ("grpc" or "http").
	Protocol string
	// Insecure skips TLS verification.
	Insecure bool
	// CAFile is the path to a PEM-encoded CA bundle for OTLP TLS.
	CAFile string
	// CertFile is the path to a PEM-encoded client certificate for OTLP mTLS.
	CertFile string
	// KeyFile is the path to a PEM-encoded client private key for OTLP mTLS.
	KeyFile string
	// ProjectID is the GCP project ID.
	ProjectID string
}

// NewOTelHandler creates an slog handler that bridges to OTel logs.
// Returns nil if lp is nil.
func NewOTelHandler(component string, lp log.LoggerProvider) slog.Handler {
	if lp == nil {
		return nil
	}
	return otelslog.NewHandler(component, otelslog.WithLoggerProvider(lp))
}

// SetupWithOTel initializes the global logger with optional OTel bridge.
// If lp is nil, falls back to standard Setup behavior.
// Extra handlers (e.g., CloudHandler) are appended to the handler chain.
func SetupWithOTel(component string, debug bool, useGCP bool, lp log.LoggerProvider, extraHandlers ...slog.Handler) {
	// Create the base handler (same logic as Setup)
	baseHandler := createBaseHandler(component, debug, useGCP)

	// Collect all handlers
	handlers := []slog.Handler{baseHandler}

	if lp != nil {
		handlers = append(handlers, NewOTelHandler(component, lp))
	}

	handlers = append(handlers, extraHandlers...)

	var handler slog.Handler
	if len(handlers) == 1 {
		handler = handlers[0]
	} else {
		handler = newMultiHandler(handlers...)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
}
