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
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Chat search bounds (ptone/scion#7586, chat search design v3 section 6.2).
// One search must never stall the hub: each search has a hard time limit,
// a principal runs at most one search at a time, and a small hub-wide cap
// applies.
const (
	// DefaultChatSearchTimeout bounds one search when server config
	// hub.chat.search.timeout is unset.
	DefaultChatSearchTimeout = 3 * time.Second
	// DefaultChatSearchMaxConcurrentSQLite is the hub-wide cap on a SQLite
	// hub. It matches the search reader's connection cap
	// (entc.SQLiteSearchReaderMaxOpen), so a search never queues for a
	// connection.
	DefaultChatSearchMaxConcurrentSQLite = 2
	// DefaultChatSearchMaxConcurrentPostgres is the hub-wide cap on a
	// Postgres hub, which searches on its shared pool.
	DefaultChatSearchMaxConcurrentPostgres = 8
	// ChatSearchMaxQueryRunes is the longest accepted search term.
	ChatSearchMaxQueryRunes = 256
	// chatSearchSlowThreshold is the duration from which a search that
	// succeeded still writes the slow-search log line.
	chatSearchSlowThreshold = 500 * time.Millisecond
	// pgQueryCanceled is SQLSTATE 57014 (query_canceled): a statement
	// stopped by statement_timeout or a cancel request.
	pgQueryCanceled = "57014"
)

// Chat search outcomes, the metric's outcome attribute.
const (
	chatSearchOutcomeOK      = "ok"
	chatSearchOutcomeTimeout = "timeout"
	chatSearchOutcomeBusy    = "busy"
	chatSearchOutcomeError   = "error"
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

// chatSearchGateFor returns the hub's search gate, built on first use with
// the configured cap or the backend default.
func (s *Server) chatSearchGateFor(wcs WebChatStore) *searchGate {
	s.chatSearchGateOnce.Do(func() {
		capacity := s.config.ChatSearchMaxConcurrent
		if capacity <= 0 {
			capacity = DefaultChatSearchMaxConcurrentSQLite
			if chatSearchBackend(wcs) == "postgres" {
				capacity = DefaultChatSearchMaxConcurrentPostgres
			}
		}
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
// histogram of every search's duration with the attributes backend, path,
// scope and outcome.
type OTelChatSearchMetrics struct {
	duration metric.Float64Histogram
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
	return &OTelChatSearchMetrics{duration: h}, nil
}

// SetChatSearchMetrics wires the search histogram. Until it is set, searches
// are only logged.
func (s *Server) SetChatSearchMetrics(m *OTelChatSearchMetrics) {
	s.chatSearchMetrics.Store(m)
}

// recordChatSearch records a search in the histogram and, when it was slow,
// timed out or failed, writes one log line with its shape. A busy refusal
// ran no query and is counted in the histogram only.
func (s *Server) recordChatSearch(ctx context.Context, logger *slog.Logger, sh chatSearchShape) {
	if m := s.chatSearchMetrics.Load(); m != nil {
		m.duration.Record(ctx, sh.duration.Seconds(), metric.WithAttributes(
			attribute.String("backend", sh.backend),
			attribute.String("path", sh.path),
			attribute.String("scope", sh.scope),
			attribute.String("outcome", sh.outcome),
		))
	}
	if sh.outcome == chatSearchOutcomeBusy {
		return
	}
	if sh.outcome == chatSearchOutcomeOK && sh.duration < chatSearchSlowThreshold {
		return
	}
	logger.WarnContext(ctx, "chat search slow",
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
