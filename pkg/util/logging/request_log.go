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
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	gcplog "cloud.google.com/go/logging"
	"github.com/google/uuid"
)

// Environment variable for request log file path.
const EnvRequestLogPath = "SCION_SERVER_REQUEST_LOG_PATH"

// RequestLogID is the Cloud Logging log ID used for HTTP request logs.
const RequestLogID = "scion_request_log"

// HttpRequest mirrors google.logging.type.HttpRequest for structured JSON output.
type HttpRequest struct {
	RequestMethod string `json:"requestMethod"`
	RequestUrl    string `json:"requestUrl"`
	RequestSize   int64  `json:"requestSize,omitempty"`
	Status        int    `json:"status"`
	ResponseSize  int64  `json:"responseSize"`
	UserAgent     string `json:"userAgent,omitempty"`
	RemoteIp      string `json:"remoteIp"`
	ServerIp      string `json:"serverIp,omitempty"`
	Referer       string `json:"referer,omitempty"`
	Latency       string `json:"latency"`
	Protocol      string `json:"protocol"`
}

// RequestMeta holds mutable request-scoped metadata that handlers can enrich.
type RequestMeta struct {
	mu        sync.Mutex
	ProjectID   string
	AgentID   string
	BrokerID  string
	RequestID string
	TraceID   string
	Component string
}

// InstrumentedResponseWriter captures status code and bytes written.
type InstrumentedResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	wroteHeader  bool
}

func (w *InstrumentedResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *InstrumentedResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += int64(n)
	return n, err
}

// Hijack implements http.Hijacker for WebSocket support.
func (w *InstrumentedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// Flush implements http.Flusher for streaming support.
func (w *InstrumentedResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter.
func (w *InstrumentedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Context key for RequestMeta.
type requestMetaKey struct{}

// AuthTypeKey is the context key for the authentication method.
// Defined here so both the auth middleware and request logger can use it
// without circular imports.
type AuthTypeKey struct{}

// ContextWithRequestMeta stores RequestMeta in the context.
func ContextWithRequestMeta(ctx context.Context, meta *RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, meta)
}

// RequestMetaFromContext retrieves RequestMeta from the context.
func RequestMetaFromContext(ctx context.Context) *RequestMeta {
	meta, _ := ctx.Value(requestMetaKey{}).(*RequestMeta)
	return meta
}

// SetRequestProjectID sets the grove ID on the request metadata in context.
func SetRequestProjectID(ctx context.Context, groveID string) {
	if meta := RequestMetaFromContext(ctx); meta != nil {
		meta.mu.Lock()
		meta.ProjectID = groveID
		meta.mu.Unlock()
	}
}

// SetRequestAgentID sets the agent ID on the request metadata in context.
func SetRequestAgentID(ctx context.Context, agentID string) {
	if meta := RequestMetaFromContext(ctx); meta != nil {
		meta.mu.Lock()
		meta.AgentID = agentID
		meta.mu.Unlock()
	}
}

// SetRequestBrokerID sets the broker ID on the request metadata in context.
func SetRequestBrokerID(ctx context.Context, brokerID string) {
	if meta := RequestMetaFromContext(ctx); meta != nil {
		meta.mu.Lock()
		meta.BrokerID = brokerID
		meta.mu.Unlock()
	}
}

// RequestLoggerConfig configures the dedicated request logger.
type RequestLoggerConfig struct {
	FilePath    string         // From SCION_SERVER_REQUEST_LOG_PATH
	CloudClient *gcplog.Client // Shared GCP client (nil if not enabled)
	ProjectID   string         // For trace URL formatting
	Component   string         // "scion-server", "scion-hub", "scion-broker"
	UseGCP      bool           // Format output as GCP-compatible JSON
	Foreground  bool           // If true, suppress stdout output
	Level       slog.Level
}

// NewRequestLogger creates a dedicated request logger with the configured outputs.
// Returns the logger, a cleanup function, and any error.
func NewRequestLogger(cfg RequestLoggerConfig) (*slog.Logger, func(), error) {
	var handlers []slog.Handler
	var cleanups []func()

	opts := &slog.HandlerOptions{Level: cfg.Level}

	// File handler
	if cfg.FilePath != "" {
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, nil, fmt.Errorf("opening request log file %s: %w", cfg.FilePath, err)
		}
		handlers = append(handlers, slog.NewJSONHandler(f, opts))
		cleanups = append(cleanups, func() {
			f.Sync()
			f.Close()
		})
	}

	// Cloud handler
	if cfg.CloudClient != nil {
		ch := NewCloudHandlerFromClient(cfg.CloudClient, RequestLogID, cfg.Component, cfg.Level)
		handlers = append(handlers, ch)
		cleanups = append(cleanups, func() {
			ch.logger.Flush()
		})
	}

	// Stdout fallback: only if NOT foreground AND no other targets configured
	if !cfg.Foreground && len(handlers) == 0 {
		if cfg.UseGCP {
			handlers = append(handlers, NewGCPHandler(os.Stdout, opts, cfg.Component))
		} else {
			handlers = append(handlers, slog.NewJSONHandler(os.Stdout, opts))
		}
	}

	// If no handlers at all (foreground with no file/cloud), use discard
	if len(handlers) == 0 {
		return slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, nil
	}

	var handler slog.Handler
	if len(handlers) == 1 {
		handler = handlers[0]
	} else {
		handler = newMultiHandler(handlers...)
	}

	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}

	return slog.New(handler), cleanup, nil
}

// PathPattern defines a URL pattern for extracting grove/agent IDs.
type PathPattern struct {
	Prefix   string // e.g. "/api/v1/groves/"
	GroveIdx int    // segment index after prefix for grove ID (-1 if N/A)
	AgentIdx int    // segment index after prefix for agent ID (-1 if N/A)
}

// HubPathPatterns returns the URL patterns for the Hub API.
func HubPathPatterns() []PathPattern {
	return []PathPattern{
		{Prefix: "/api/v1/projects/", GroveIdx: 0, AgentIdx: -1},
		{Prefix: "/api/v1/groves/", GroveIdx: 0, AgentIdx: -1},
		{Prefix: "/api/v1/agents/", GroveIdx: -1, AgentIdx: 0},
	}
}

// BrokerPathPatterns returns the URL patterns for the Broker API.
func BrokerPathPatterns() []PathPattern {
	return []PathPattern{
		{Prefix: "/api/v1/projects/", GroveIdx: 0, AgentIdx: -1},
		{Prefix: "/api/v1/groves/", GroveIdx: 0, AgentIdx: -1},
		{Prefix: "/api/v1/agents/", GroveIdx: -1, AgentIdx: 0},
	}
}

// extractIDsFromPath extracts grove and agent IDs from the URL path
// using the provided patterns.
func extractIDsFromPath(path string, patterns []PathPattern) (groveID, agentID string) {
	for _, p := range patterns {
		if !strings.HasPrefix(path, p.Prefix) {
			continue
		}
		remainder := path[len(p.Prefix):]
		segments := strings.Split(strings.TrimSuffix(remainder, "/"), "/")

		if p.GroveIdx >= 0 && p.GroveIdx < len(segments) && segments[p.GroveIdx] != "" {
			groveID = segments[p.GroveIdx]
		}
		if p.AgentIdx >= 0 && p.AgentIdx < len(segments) && segments[p.AgentIdx] != "" {
			agentID = segments[p.AgentIdx]
		}
		return
	}
	return
}

// RequestLogMiddleware creates HTTP middleware that logs each request
// to the dedicated request logger using the HttpRequest format.
func RequestLogMiddleware(logger *slog.Logger, component string, patterns []PathPattern) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Extract and normalize trace ID from headers.
			traceID := ExtractTraceIDFromHeaders(r)

			// Generate request ID if no trace header present
			requestID := uuid.New().String()

			// Best-effort extract grove/agent IDs from path
			groveID, agentID := extractIDsFromPath(r.URL.Path, patterns)

			// Create request metadata and store in context
			meta := &RequestMeta{
				ProjectID:   groveID,
				AgentID:   agentID,
				RequestID: requestID,
				TraceID:   traceID,
				Component: component,
			}
			ctx := ContextWithRequestMeta(r.Context(), meta)
			r = r.WithContext(ctx)

			// Read auth type from context (set by auth middleware before this point)
			authType, _ := r.Context().Value(AuthTypeKey{}).(string)

			// Wrap response writer
			wrapped := &InstrumentedResponseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			next.ServeHTTP(wrapped, r)

			// Read final metadata (handlers may have enriched it)
			meta.mu.Lock()
			finalProjectID := meta.ProjectID
			finalAgentID := meta.AgentID
			finalBrokerID := meta.BrokerID
			meta.mu.Unlock()

			finalAuthType := authType

			duration := time.Since(start)

			// Build HttpRequest struct
			httpReq := HttpRequest{
				RequestMethod: r.Method,
				RequestUrl:    r.URL.String(),
				RequestSize:   r.ContentLength,
				Status:        wrapped.statusCode,
				ResponseSize:  wrapped.bytesWritten,
				UserAgent:     r.UserAgent(),
				RemoteIp:      r.RemoteAddr,
				Referer:       r.Referer(),
				Latency:       fmt.Sprintf("%.3fs", duration.Seconds()),
				Protocol:      r.Proto,
			}

			// Warn on slow requests (>2s) via the default logger
			if duration > 2*time.Second {
				slog.Warn("Slow request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Duration("elapsed", duration),
					slog.Int("status", wrapped.statusCode),
				)
			}

			// Determine log level
			level := slog.LevelInfo
			if wrapped.statusCode >= 500 {
				level = slog.LevelError
			} else if wrapped.statusCode >= 400 {
				level = slog.LevelWarn
			}

			// Build attrs
			attrs := []slog.Attr{
				slog.Group("httpRequest",
					slog.String("requestMethod", httpReq.RequestMethod),
					slog.String("requestUrl", httpReq.RequestUrl),
					slog.Int64("requestSize", httpReq.RequestSize),
					slog.Int("status", httpReq.Status),
					slog.Int64("responseSize", httpReq.ResponseSize),
					slog.String("userAgent", httpReq.UserAgent),
					slog.String("remoteIp", httpReq.RemoteIp),
					slog.String("referer", httpReq.Referer),
					slog.String("latency", httpReq.Latency),
					slog.String("protocol", httpReq.Protocol),
				),
				slog.String(AttrComponent, component),
				slog.String(AttrProjectID, finalProjectID),
				slog.String(AttrAgentID, finalAgentID),
				slog.String(AttrBrokerID, finalBrokerID),
				slog.String(AttrAuthType, finalAuthType),
				slog.String(AttrRequestID, requestID),
			}

			if traceID != "" {
				attrs = append(attrs, slog.String(AttrTraceID, traceID))
			}

			logger.LogAttrs(ctx, level, "", attrs...)
		})
	}
}
