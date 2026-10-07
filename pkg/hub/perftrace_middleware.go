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

package hub

import (
	"bufio"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// perfTraceMiddleware installs a fresh PerfTrace into each request context,
// times the serialize phase (first response write to handler return), adds
// the X-Scion-Perf-* response headers when the request opted in with
// X-Scion-Perf-Trace: 1, and writes one "perf_trace" log line per request.
//
// applyMiddleware installs it only when server.hub.perf_trace is on, as the
// innermost middleware, so the trace reaches the route handler, the
// authorization service and its store and audit-emitter decorators. With
// the setting off it is not in the chain at all.
//
// The response body and status are passed through unchanged; only the
// opt-in headers are added, and only for an unscoped local platform admin
// (see perfHeadersAllowed). The serialize phase and the log line are
// deferred, so a handler panic is still traced.
func (s *Server) perfTraceMiddleware(next http.Handler) http.Handler {
	db := s.perfTraceDB()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		trace := perfTraceFrom(ctx)
		if trace == nil {
			trace = newPerfTrace(db)
			ctx = contextWithPerfTrace(ctx, trace)
			r = r.WithContext(ctx)
		}
		pw := &perfResponseWriter{
			ResponseWriter: w,
			trace:          trace,
			emitHeaders:    r.Header.Get(HeaderPerfTraceRequest) == "1" && perfHeadersAllowed(r),
		}
		defer func() {
			if pw.wroteHeader {
				trace.addPhase(perfPhaseSerialize, time.Since(pw.firstWrite))
			}
			s.logPerfTrace(r, trace)
		}()
		next.ServeHTTP(pw, r)
	})
}

// perfHeadersAllowed reports whether a request may receive the
// X-Scion-Perf-* response headers. The counts reveal more than timing: deny
// counts give the number of candidate rows hidden from the caller, store
// calls hint at owner and group structure, and DB pool figures describe
// process-wide load. So only an unscoped local platform admin gets them,
// never on an unauthenticated endpoint, and never an agent, broker, scoped
// token or federated identity. The check reads identity fields only: no
// decision, store read or audit record.
func perfHeadersAllowed(r *http.Request) bool {
	if isUnauthenticatedEndpoint(r.URL.Path) {
		return false
	}
	return IsUnscopedLocalPlatformAdmin(GetUserIdentityFromContext(r.Context()))
}

// perfTraceDB returns the store's connection pool when the store exposes
// one, for the DB pool wait counters. It returns nil otherwise.
func (s *Server) perfTraceDB() *sql.DB {
	return webPerfTraceDB(s.store)
}

func webPerfTraceDB(st any) *sql.DB {
	if dbp, ok := st.(interface{ DB() *sql.DB }); ok {
		return dbp.DB()
	}
	return nil
}

var (
	perfTraceLoggerOnce sync.Once
	perfTraceLoggerVal  *slog.Logger
)

// perfTraceLogger returns the hub.perf-trace subsystem logger, created on
// first use (only ever reached with the setting on). A variable so tests can
// capture the lines.
var perfTraceLogger = func() *slog.Logger {
	perfTraceLoggerOnce.Do(func() { perfTraceLoggerVal = logging.Subsystem("hub.perf-trace") })
	return perfTraceLoggerVal
}

func (s *Server) logPerfTrace(r *http.Request, trace *PerfTrace) {
	logger := s.perfTraceLog
	if logger == nil {
		return
	}
	logPerfTraceLine(logger, r, trace.Snapshot)
}

// logPerfTraceLine writes one perf_trace line from snap(). Only the method,
// the bounded endpoint class, the snapshot's fixed-key attributes, the
// request ID (correlation only) and the caller's fixed extra attributes are
// logged: never the path, query, caller identity or any request content.
func logPerfTraceLine(logger *slog.Logger, r *http.Request, snap func() PerfTraceSnapshot, extra ...slog.Attr) {
	if logger == nil {
		return
	}
	attrs := snap().LogAttrs()
	attrs = append(attrs, extra...)
	attrs = append(attrs, slog.String("method", r.Method))
	if meta := logging.RequestMetaFromContext(r.Context()); meta != nil && meta.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", meta.RequestID))
	}
	logger.LogAttrs(r.Context(), slog.LevelInfo, "perf_trace", attrs...)
}

// perfResponseWriter records when the response starts and, for an allowed
// opt-in request, sets the perf headers just before the status line is
// written.
// It forwards every write, flush and hijack unchanged.
type perfResponseWriter struct {
	http.ResponseWriter
	trace       *PerfTrace
	emitHeaders bool
	wroteHeader bool
	firstWrite  time.Time
}

func (w *perfResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.firstWrite = time.Now()
		if w.emitHeaders {
			h := w.Header()
			for k, v := range w.trace.Snapshot().HeaderValues() {
				h.Set(k, v)
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *perfResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher, as the request-log writer it wraps does.
func (w *perfResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker, as the request-log writer it wraps does.
func (w *perfResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("perf trace writer: %w", http.ErrNotSupported)
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController.
func (w *perfResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
