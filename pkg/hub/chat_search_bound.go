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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Chat search bounds (ptone/scion#7586, chat search design v3 section 6.2).
// One search must never stall the hub: each search has a hard time limit,
// a principal runs at most one search at a time, and a small hub-wide cap
// applies.
const (
	// DefaultChatSearchTimeout bounds one search when server config
	// hub.chat.search.timeout is unset.
	DefaultChatSearchTimeout = 3 * time.Second
	// DefaultChatSearchMaxConcurrent is the configured default of the
	// hub-wide cap, on both backends (design v4 erratum E1). On SQLite it is
	// also the search reader's connection cap (entc.SQLiteSearchReaderMaxOpen),
	// so a search never queues for a connection. On Postgres the effective
	// cap is clamped to a quarter of the shared pool (EffectiveChatSearchCap).
	DefaultChatSearchMaxConcurrent = 2
	// ChatSearchMaxQueryRunes is the longest accepted search term.
	ChatSearchMaxQueryRunes = 256
	// chatSearchSlowThreshold is the duration from which a search that
	// succeeded still writes the slow-search log line.
	chatSearchSlowThreshold = 500 * time.Millisecond
	// pgQueryCanceled is SQLSTATE 57014 (query_canceled): a statement
	// stopped by statement_timeout or a cancel request.
	pgQueryCanceled = "57014"
)

// Chat search outcomes, the metric's outcome attribute. A busy refusal ran
// no search and is counted in its own counter, not in the duration histogram.
const (
	chatSearchOutcomeOK       = "ok"
	chatSearchOutcomeTimeout  = "timeout"
	chatSearchOutcomeCanceled = "canceled" // the client went away (the UI aborts superseded searches)
	chatSearchOutcomeError    = "error"
)

// searchGate bounds concurrent chat searches: at most cap searches hub-wide,
// and at most one per principal. A caller that cannot get a slot at once is
// refused (429), never queued, so a burst cannot build a backlog.
type searchGate struct {
	slots chan struct{}

	mu       sync.Mutex
	inFlight map[string]struct{}
}

func newSearchGate(capacity int) *searchGate {
	if capacity < 1 {
		capacity = 1
	}
	return &searchGate{
		slots:    make(chan struct{}, capacity),
		inFlight: make(map[string]struct{}),
	}
}

// tryAcquire takes a hub-wide slot and then the principal's single slot.
// The principal check runs inside the global slot, so one principal can
// never hold more than one of the global slots. On success it returns a
// release func that must be called exactly once.
func (g *searchGate) tryAcquire(principal string) (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
	default:
		return nil, false
	}
	g.mu.Lock()
	if _, busy := g.inFlight[principal]; busy {
		g.mu.Unlock()
		<-g.slots
		return nil, false
	}
	g.inFlight[principal] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.inFlight, principal)
			g.mu.Unlock()
			<-g.slots
		})
	}, true
}

// chatSearchBackend names the backend of a web chat store for the metric
// and log: "postgres" or "sqlite".
func chatSearchBackend(wcs WebChatStore) string {
	if _, ok := wcs.(*pgWebChatStore); ok {
		return "postgres"
	}
	return "sqlite"
}

// EffectiveChatSearchCap returns the hub-wide search cap for a backend
// ("sqlite" or "postgres"), and whether the Postgres clamp lowered the
// configured value (design v4 erratum E1). configured <= 0 means the default
// (DefaultChatSearchMaxConcurrent). On Postgres, search shares the hub's main
// pool of poolMaxOpen connections, so the cap is
// max(1, min(configured, floor(poolMaxOpen / 4))): searches hold at most a
// quarter of the pool and always at least one connection. poolMaxOpen <= 0
// (unknown) is read as the hub's default Postgres pool of 5, the value
// config.applyDatabasePoolDefaults sets. On SQLite search has its own
// read-only handle, so there is no clamp.
func EffectiveChatSearchCap(configured int, backend string, poolMaxOpen int) (effective int, clamped bool) {
	if configured <= 0 {
		configured = DefaultChatSearchMaxConcurrent
	}
	if backend != "postgres" {
		return configured, false
	}
	if poolMaxOpen <= 0 {
		poolMaxOpen = 5
	}
	limit := poolMaxOpen / 4
	if limit < 1 {
		limit = 1
	}
	if configured > limit {
		return limit, true
	}
	return configured, false
}

// ChatSearchCapStartupLine returns the one startup log line for a clamped
// Postgres search cap (numbers only), and false when the clamp did not lower
// the configured value.
func ChatSearchCapStartupLine(configured int, backend string, poolMaxOpen int) (string, bool) {
	effective, clamped := EffectiveChatSearchCap(configured, backend, poolMaxOpen)
	if !clamped {
		return "", false
	}
	if configured <= 0 {
		configured = DefaultChatSearchMaxConcurrent
	}
	return fmt.Sprintf("Chat search: max_concurrent %d lowered to %d (a quarter of the Postgres pool of %d)", configured, effective, poolMaxOpen), true
}

// chatSearchGateFor returns the hub's search gate, built on first use with
// the effective cap for the store's backend.
func (s *Server) chatSearchGateFor(wcs WebChatStore) *searchGate {
	s.chatSearchGateOnce.Do(func() {
		capacity, _ := EffectiveChatSearchCap(s.config.ChatSearchMaxConcurrent, chatSearchBackend(wcs), s.config.DatabaseMaxOpenConns)
		s.chatSearchGate = newSearchGate(capacity)
	})
	return s.chatSearchGate
}

// chatSearchTimeout returns the configured search time limit.
func (s *Server) chatSearchTimeout() time.Duration {
	if s.config.ChatSearchTimeout > 0 {
		return s.config.ChatSearchTimeout
	}
	return DefaultChatSearchTimeout
}

// isChatSearchTimeout reports whether a search failed because its time
// limit passed: the context deadline (SQLite interrupt, or a client-side
// wait) or Postgres statement_timeout (SQLSTATE 57014).
func isChatSearchTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled
}

// chatSearchShape describes one search for the slow-search log line and the
// metric. It never holds the term, a conversation key, a user ID or a
// project ID (design v3 section 6.5): only sizes, counts and labels.
type chatSearchShape struct {
	backend      string
	path         string // "like" in Phase 1
	scope        string // "conversation", "project" or "all"
	projectCount int
	termLen      int // in runes
	results      int
	hasCursor    bool
	outcome      string
	duration     time.Duration
}

// OTelChatSearchMetrics records hub_chat_search_duration_seconds, a
// histogram of every search that ran (attributes backend, path, scope and
// outcome: ok, timeout, canceled or error), and hub_chat_search_busy_total, a
// counter of searches refused by the gate (attribute backend).
type OTelChatSearchMetrics struct {
	duration metric.Float64Histogram
	busy     metric.Int64Counter
}

// NewOTelChatSearchMetrics creates the histogram. The OTel name
// hub.chat.search.duration with unit "s" is exported to Prometheus as
// hub_chat_search_duration_seconds.
func NewOTelChatSearchMetrics(mp metric.MeterProvider) (*OTelChatSearchMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel chat search metrics: nil MeterProvider")
	}
	h, err := mp.Meter(instrumentationScope).Float64Histogram("hub.chat.search.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of web chat searches."),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10))
	if err != nil {
		return nil, err
	}
	b, err := mp.Meter(instrumentationScope).Int64Counter("hub.chat.search.busy",
		metric.WithUnit("{search}"),
		metric.WithDescription("Web chat searches refused because the search gate was full."))
	if err != nil {
		return nil, err
	}
	return &OTelChatSearchMetrics{duration: h, busy: b}, nil
}

// SetChatSearchMetrics wires the search histogram. Until it is set, searches
// are only logged.
func (s *Server) SetChatSearchMetrics(m *OTelChatSearchMetrics) {
	s.chatSearchMetrics.Store(m)
}

// recordChatSearchBusy counts a search the gate refused. It ran no query,
// so it stays out of the duration histogram and writes no log line.
func (s *Server) recordChatSearchBusy(ctx context.Context, sh chatSearchShape) {
	if m := s.chatSearchMetrics.Load(); m != nil {
		m.busy.Add(spanFreeContext(ctx), 1, metric.WithAttributes(attribute.String("backend", sh.backend)))
	}
}

// spanFreeContext returns ctx without its trace span. The search metric
// records use it so the SDK's trace-based exemplars cannot attach a sampled
// request's trace and span ids to the metric points: design 6.5 allows no
// identifier on the search signal (round-2 item R2-S1).
func spanFreeContext(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(ctx, trace.SpanContext{})
}

// recordChatSearch records a search in the histogram and, when it was slow,
// timed out or failed, writes one log line with its shape. A canceled search
// (the client went away) writes a debug line at most, so the aborts the UI
// sends on every superseded keystroke do not drown the slow and timeout
// signal.
//
// The log line is written WITHOUT the request context: the default handler
// adds the request id to any record whose context carries one, which would let
// an operator join the search shape to the request's access-log line. Design
// 6.5 allows no identifier on this line (round-1 item R1-08).
func (s *Server) recordChatSearch(ctx context.Context, logger *slog.Logger, sh chatSearchShape) {
	if m := s.chatSearchMetrics.Load(); m != nil {
		m.duration.Record(spanFreeContext(ctx), sh.duration.Seconds(), metric.WithAttributes(
			attribute.String("backend", sh.backend),
			attribute.String("path", sh.path),
			attribute.String("scope", sh.scope),
			attribute.String("outcome", sh.outcome),
		))
	}
	if sh.outcome == chatSearchOutcomeCanceled {
		logger.Debug("chat search canceled", "duration_ms", sh.duration.Milliseconds(), "backend", sh.backend, "scope", sh.scope)
		return
	}
	if sh.outcome == chatSearchOutcomeOK && sh.duration < chatSearchSlowThreshold {
		return
	}
	logger.Warn("chat search slow",
		"duration_ms", sh.duration.Milliseconds(),
		"timed_out", sh.outcome == chatSearchOutcomeTimeout,
		"outcome", sh.outcome,
		"backend", sh.backend,
		"path", sh.path,
		"scope", sh.scope,
		"project_count", sh.projectCount,
		"term_len", sh.termLen,
		"results", sh.results,
		"has_cursor", sh.hasCursor,
	)
}
