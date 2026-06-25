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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
)

// PostgresEventPublisher is an EventPublisher backed by PostgreSQL LISTEN/NOTIFY.
// It delivers events across replicas: a NOTIFY issued on one hub instance is
// received by the listener goroutine on every instance (including the
// publisher), which fans the event out to that instance's in-process
// subscribers using the same NATS-style subject matching as
// ChannelEventPublisher.
//
// Channel model — per grove plus a global channel (flat exact-match, since
// Postgres channels do not support wildcards):
//
//   - Grove-scoped subjects ("project.<id>.*" / "grove.<id>.*") are published
//     to a per-grove channel (scion_ev_g_<id>) AND to the global channel. The
//     per-grove channel lets a replica that only watches a specific grove (e.g.
//     a browser SSE stream) LISTEN on just that channel instead of the firehose.
//   - All other subjects ("agent.*", "user.*", "broker.*", "admin.*",
//     "notification.*") are published to the global channel only.
//   - Subscriptions with a concrete grove id resolve to that grove's channel;
//     everything else (grove-spanning wildcards used by the notification
//     dispatcher and message-broker proxy, and non-grove subjects) resolves to
//     the global channel. Each subscriber's patterns are grouped by the channel
//     they resolve to, so an event arriving on a channel is only matched against
//     the patterns that opted into that channel — no double delivery.
//
// Delivery is performed exclusively by the listener (events are not fanned out
// locally at publish time). This gives transactional publish semantics for free
// with PublishTx: a NOTIFY enrolled in a transaction that rolls back is never
// sent, so subscribers — local or remote — never observe it.
//
// Payloads larger than the Postgres 8000-byte NOTIFY limit are stored in the
// scion_event_payloads table and the NOTIFY carries a reference id; the listener
// refetches the payload on receipt (reference-and-refetch). A background
// goroutine purges old payload rows on a TTL so multiple replicas can each
// refetch the same oversized event.
type PostgresEventPublisher struct {
	eventBuilder

	pool    *pgxpool.Pool
	dsn     string
	metrics dbmetrics.Recorder
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.RWMutex
	// subs maps a Postgres channel -> subscriber -> the subset of that
	// subscriber's patterns that resolve to this channel.
	subs map[string]map[*pgSubscription][]string
	// desired counts how many subscriptions need each channel LISTENed.
	desired map[string]int
	closed  bool
}

// pgSubscription is a single Subscribe registration.
type pgSubscription struct {
	ch   chan Event
	once sync.Once
}

// pgEnvelope is the JSON wire format carried in a NOTIFY payload. The event type
// is included so out-of-process consumers can route without re-deriving it from
// the subject.
type pgEnvelope struct {
	Type    string          `json:"type"`
	Subject string          `json:"subject"`
	Data    json.RawMessage `json:"data,omitempty"`
	Ref     string          `json:"ref,omitempty"` // payload-table id when oversized
	TS      int64           `json:"ts,omitempty"`  // publish time, unix nanos (for latency)
}

const (
	pgChannelPrefix = "scion_ev_"
	pgGlobalChannel = "scion_ev_global"
	// pgNotifyMaxPayload is the threshold above which an event is offloaded to
	// the payload table. Postgres rejects NOTIFY payloads of 8000 bytes or more;
	// the margin leaves room for the envelope wrapping around the data.
	pgNotifyMaxPayload = 7000
	// maxPGIdentifier is the Postgres identifier length limit (NAMEDATALEN-1).
	maxPGIdentifier = 63
	// listenPollInterval bounds how long the listener blocks in
	// WaitForNotification before waking to apply pending LISTEN/UNLISTEN changes.
	// A WaitForNotification deadline does not invalidate the connection (pgconn
	// treats a read timeout as recoverable), so this is a cheap idle poll.
	listenPollInterval = time.Second
	// payloadTTL is how long oversized payloads are retained for refetch.
	payloadTTL = 60 * time.Second
	// publishTimeout bounds a single autocommit publish (Publish* methods). These
	// run synchronously on the caller's goroutine — typically a request handler
	// right after a CRUD write — and acquire a connection from the event pool. On
	// an undersized / connection-starved instance (see CONNECTION-BUDGET.md) that
	// acquire could otherwise block indefinitely, stalling the handler and
	// silently never emitting the NOTIFY. Bounding it converts that failure mode
	// into a logged error and a dropped event (publishing is fire-and-forget),
	// keeping CRUD responsive. The transactional path (PublishTx) is unaffected:
	// it uses the caller's context and transaction.
	publishTimeout = 5 * time.Second
)

// pgExecutor is satisfied by both *pgxpool.Pool and pgx.Tx, letting the publish
// path run either against an autocommit pool connection or inside a caller's
// transaction.
type pgExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// compile-time check that PostgresEventPublisher satisfies EventPublisher.
var _ EventPublisher = (*PostgresEventPublisher)(nil)

// NewPostgresEventPublisher connects to Postgres at dsn, ensures the
// payload-offload table exists, and starts the listener and maintenance
// goroutines. If metrics is nil a disabled (no-op) recorder is used.
func NewPostgresEventPublisher(ctx context.Context, dsn string, metrics dbmetrics.Recorder, log *slog.Logger) (*PostgresEventPublisher, error) {
	if metrics == nil {
		metrics = dbmetrics.NewDisabled()
	}
	if log == nil {
		log = slog.Default()
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres event dsn: %w", err)
	}
	applyEventPoolKeepalives(poolCfg)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating postgres event pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres for events: %w", err)
	}

	pubCtx, cancel := context.WithCancel(context.Background())
	p := &PostgresEventPublisher{
		pool:    pool,
		dsn:     dsn,
		metrics: metrics,
		log:     log,
		ctx:     pubCtx,
		cancel:  cancel,
		subs:    make(map[string]map[*pgSubscription][]string),
		desired: make(map[string]int),
	}
	p.sink = p.publish

	if err := p.ensurePayloadTable(ctx); err != nil {
		cancel()
		pool.Close()
		return nil, err
	}

	p.wg.Add(2)
	go p.runListener()
	go p.runMaintenance()

	log.Info("Postgres event publisher started")
	return p, nil
}

// ensurePayloadTable creates the oversized-payload offload table if absent.
func (p *PostgresEventPublisher) ensurePayloadTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS scion_event_payloads (
	id         UUID PRIMARY KEY,
	subject    TEXT NOT NULL,
	data       BYTEA NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS scion_event_payloads_created_at_idx
	ON scion_event_payloads (created_at);`
	if _, err := p.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("creating scion_event_payloads table: %w", err)
	}
	return nil
}

// publish is the sink wired into eventBuilder. It marshals and NOTIFYs on the
// pool (autocommit). Errors are logged rather than returned because the
// EventPublisher Publish* methods are fire-and-forget.
func (p *PostgresEventPublisher) publish(subject string, event interface{}) {
	// Bound the publish so a saturated event pool surfaces a logged error instead
	// of blocking the calling (often request-handler) goroutine forever. See
	// publishTimeout.
	ctx, cancel := context.WithTimeout(p.ctx, publishTimeout)
	defer cancel()
	if err := p.buildAndNotify(ctx, p.pool, subject, event); err != nil {
		p.log.Error("Failed to publish event via NOTIFY", "subject", subject, "error", err)
	}
}

// PublishTx publishes an event using a caller-supplied executor, giving an
// atomic write+publish when that executor is a transaction (pgx.Tx satisfies
// pgExecutor): the NOTIFY is enrolled in the transaction and only delivered if
// it commits. If the transaction rolls back, no subscriber (local or remote)
// observes the event.
func (p *PostgresEventPublisher) PublishTx(ctx context.Context, tx pgExecutor, subject string, event interface{}) error {
	return p.buildAndNotify(ctx, tx, subject, event)
}

// buildAndNotify marshals event into an envelope, offloading the data to the
// payload table when it would exceed the NOTIFY size limit, and issues one
// NOTIFY per destination channel via exec.
func (p *PostgresEventPublisher) buildAndNotify(ctx context.Context, exec pgExecutor, subject string, event interface{}) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling event %s: %w", subject, err)
	}

	env := pgEnvelope{
		Type:    eventTypeName(event),
		Subject: subject,
		Data:    data,
		TS:      time.Now().UnixNano(),
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling envelope %s: %w", subject, err)
	}

	scope := channelScope(subject)
	p.metrics.RecordPayloadSize(ctx, int64(len(payload)), attribute.String("scope", scope))

	if len(payload) > pgNotifyMaxPayload {
		id := uuid.NewString()
		if _, err := exec.Exec(ctx,
			`INSERT INTO scion_event_payloads (id, subject, data) VALUES ($1, $2, $3)`,
			id, subject, data,
		); err != nil {
			return fmt.Errorf("storing oversized payload for %s: %w", subject, err)
		}
		env.Data = nil
		env.Ref = id
		payload, err = json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshaling oversized envelope %s: %w", subject, err)
		}
	}

	for _, channel := range channelsForSubject(subject) {
		if _, err := exec.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, string(payload)); err != nil {
			return fmt.Errorf("pg_notify on %s: %w", channel, err)
		}
	}

	p.metrics.IncPublished(ctx, 1, attribute.String("scope", scope))
	return nil
}

// Subscribe registers patterns and returns a buffered channel plus an
// unsubscribe function. Patterns use NATS-style wildcards; matching is performed
// against the subject of each received event. The listener begins LISTENing on
// any newly-needed Postgres channels within listenPollInterval.
func (p *PostgresEventPublisher) Subscribe(patterns ...string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	sub := &pgSubscription{ch: ch}

	// Group patterns by the Postgres channel they resolve to.
	byChannel := make(map[string][]string)
	for _, pattern := range patterns {
		for _, channel := range channelsForPattern(pattern) {
			byChannel[channel] = append(byChannel[channel], pattern)
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	for channel, pats := range byChannel {
		if p.subs[channel] == nil {
			p.subs[channel] = make(map[*pgSubscription][]string)
		}
		p.subs[channel][sub] = pats
		p.desired[channel]++
	}
	p.mu.Unlock()

	unsubscribe := func() {
		sub.once.Do(func() {
			p.mu.Lock()
			for channel := range byChannel {
				if m := p.subs[channel]; m != nil {
					delete(m, sub)
					if len(m) == 0 {
						delete(p.subs, channel)
					}
				}
				if p.desired[channel] > 0 {
					p.desired[channel]--
					if p.desired[channel] == 0 {
						delete(p.desired, channel)
					}
				}
			}
			p.mu.Unlock()
			close(ch)
		})
	}

	return ch, unsubscribe
}

// Close stops the background goroutines, closes the pool, and closes all
// subscriber channels.
func (p *PostgresEventPublisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.cancel()
	p.wg.Wait()
	p.pool.Close()

	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[*pgSubscription]bool)
	for _, m := range p.subs {
		for sub := range m {
			if !seen[sub] {
				sub.once.Do(func() { close(sub.ch) })
				seen[sub] = true
			}
		}
	}
	p.subs = make(map[string]map[*pgSubscription][]string)
	p.desired = make(map[string]int)
}

// runListener maintains a dedicated connection that LISTENs on the desired
// channels and dispatches received notifications. It reconnects with backoff and
// re-LISTENs (resubscribes) after any connection loss.
func (p *PostgresEventPublisher) runListener() {
	defer p.wg.Done()

	const (
		minBackoff = 250 * time.Millisecond
		maxBackoff = 10 * time.Second
	)
	backoff := minBackoff

	for {
		if p.ctx.Err() != nil {
			return
		}

		conn, err := p.connectListener(p.ctx)
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			p.log.Warn("Event listener connect failed, retrying", "error", err, "backoff", backoff)
			if !p.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		p.log.Info("Event listener connected")
		backoff = minBackoff

		// active tracks channels currently LISTENed on this connection. A fresh
		// connection starts empty, so listenLoop re-LISTENs every desired
		// channel (resubscribe).
		active := make(map[string]bool)
		loopErr := p.listenLoop(conn, active)
		_ = conn.Close(context.Background())

		if p.ctx.Err() != nil {
			return
		}

		// Unexpected connection loss: count a reconnect and retry.
		p.metrics.IncListenerReconnects(p.ctx, 1)
		p.log.Warn("Event listener connection lost, reconnecting", "error", loopErr, "backoff", backoff)
		if !p.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// connectListener opens the dedicated listener connection with TCP keepalives and
// a connect timeout applied, so the long-lived (mostly idle) LISTEN connection
// detects a silently dropped peer instead of blocking forever in
// WaitForNotification on a dead socket.
func (p *PostgresEventPublisher) connectListener(ctx context.Context) (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig(p.dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing listener dsn: %w", err)
	}
	applyConnKeepalives(cc)
	return pgx.ConnectConfig(ctx, cc)
}

// listenLoop applies pending subscription changes and waits for notifications on
// conn until the context is canceled or the connection fails. A returned error
// other than context cancellation signals the caller to reconnect.
func (p *PostgresEventPublisher) listenLoop(conn *pgx.Conn, active map[string]bool) error {
	for {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}

		desired := p.snapshotDesired()
		for channel := range desired {
			if !active[channel] {
				if err := execListen(p.ctx, conn, "LISTEN", channel); err != nil {
					return fmt.Errorf("LISTEN %s: %w", channel, err)
				}
				active[channel] = true
			}
		}
		for channel := range active {
			if !desired[channel] {
				if err := execListen(p.ctx, conn, "UNLISTEN", channel); err != nil {
					return fmt.Errorf("UNLISTEN %s: %w", channel, err)
				}
				delete(active, channel)
			}
		}

		waitCtx, cancel := context.WithTimeout(p.ctx, listenPollInterval)
		notif, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			// A poll-interval deadline is expected; loop to reapply subscriptions.
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			// Context canceled (shutdown) or a real connection error.
			return err
		}

		p.handleNotification(notif.Channel, notif.Payload)
	}
}

// handleNotification decodes a NOTIFY payload (refetching oversized payloads),
// records latency, and fans the event out to subscribers of its channel.
func (p *PostgresEventPublisher) handleNotification(channel, payload string) {
	var env pgEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		p.log.Error("Failed to decode NOTIFY payload", "channel", channel, "error", err)
		p.metrics.IncDropped(p.ctx, 1, attribute.String("reason", "decode"))
		return
	}

	data := []byte(env.Data)
	if env.Ref != "" {
		fetched, err := p.refetchPayload(env.Ref)
		if err != nil {
			p.log.Error("Failed to refetch oversized payload", "ref", env.Ref, "subject", env.Subject, "error", err)
			p.metrics.IncDropped(p.ctx, 1, attribute.String("reason", "refetch"))
			return
		}
		data = fetched
	}

	if env.TS != 0 && p.metrics.Enabled() {
		ms := float64(time.Now().UnixNano()-env.TS) / float64(time.Millisecond)
		p.metrics.RecordPublishToDeliverLatency(p.ctx, ms, attribute.String("scope", channelScope(env.Subject)))
	}

	p.fanout(channel, Event{Subject: env.Subject, Data: data})
}

// refetchPayload loads an oversized payload by reference id. Rows are not deleted
// here so every replica can refetch the same event; a TTL sweep reclaims them.
func (p *PostgresEventPublisher) refetchPayload(ref string) ([]byte, error) {
	var data []byte
	err := p.pool.QueryRow(p.ctx, `SELECT data FROM scion_event_payloads WHERE id = $1`, ref).Scan(&data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// fanout delivers evt to every subscriber of channel whose patterns (scoped to
// that channel) match the event subject. Sends are non-blocking; a full
// subscriber buffer drops the event (backpressure).
func (p *PostgresEventPublisher) fanout(channel string, evt Event) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for sub, patterns := range p.subs[channel] {
		if !anyPatternMatches(patterns, evt.Subject) {
			continue
		}
		select {
		case sub.ch <- evt:
			p.metrics.IncDelivered(p.ctx, 1, attribute.String("scope", channelScope(evt.Subject)))
		default:
			p.metrics.IncDropped(p.ctx, 1, attribute.String("reason", "full_buffer"))
		}
	}
}

// snapshotDesired returns a copy of the set of channels that should be LISTENed.
func (p *PostgresEventPublisher) snapshotDesired() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]bool, len(p.desired))
	for channel := range p.desired {
		out[channel] = true
	}
	return out
}

// runMaintenance periodically purges expired oversized payloads and reports
// connection-pool gauges.
func (p *PostgresEventPublisher) runMaintenance() {
	defer p.wg.Done()
	ticker := time.NewTicker(payloadTTL / 2)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.pool.Exec(p.ctx,
				`DELETE FROM scion_event_payloads WHERE created_at < now() - $1::interval`,
				fmt.Sprintf("%d seconds", int(payloadTTL.Seconds())),
			); err != nil && p.ctx.Err() == nil {
				p.log.Warn("Failed to purge expired event payloads", "error", err)
			}
			p.observePoolStats()
		}
	}
}

// observePoolStats records a snapshot of the pgx pool gauges.
func (p *PostgresEventPublisher) observePoolStats() {
	if !p.metrics.Enabled() {
		return
	}
	s := p.pool.Stat()
	p.metrics.ObservePoolStats(p.ctx, dbmetrics.PoolStats{
		Active:  int64(s.AcquiredConns()),
		Idle:    int64(s.IdleConns()),
		Waiting: int64(s.EmptyAcquireCount()),
		Max:     int64(s.MaxConns()),
	})
}

// sleep waits for d or until the publisher context is canceled. It reports false
// if the context was canceled.
func (p *PostgresEventPublisher) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// eventConnectTimeout bounds a single connection attempt for the event pool and
// listener, so a network black-hole surfaces as a retryable error instead of a
// hang.
const eventConnectTimeout = 10 * time.Second

// applyEventPoolKeepalives attaches TCP keepalive GUCs and a connect timeout to
// the event pool's per-connection config, and bounds idle/total connection age.
// CloudSQL (and NAT gateways) silently drop idle connections; keepalives let the
// kernel detect a dead peer and the idle/lifetime caps recycle connections before
// the remote does, so the listener and publishers don't stall on a dead socket.
func applyEventPoolKeepalives(cfg *pgxpool.Config) {
	applyConnKeepalives(cfg.ConnConfig)
	// Recycle idle event-pool connections well before CloudSQL's ~10m idle
	// timeout, and bound total connection age.
	if cfg.MaxConnIdleTime == 0 {
		cfg.MaxConnIdleTime = 5 * time.Minute
	}
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = 30 * time.Minute
	}
}

// applyConnKeepalives sets the connect timeout and server-side TCP keepalive GUCs
// on a single pgx connection config. Existing RuntimeParams are not overwritten so
// an explicit DSN setting wins. Values: probe after 60s idle, every 15s, give up
// after 4 missed probes (~2 min to detect a dead peer).
func applyConnKeepalives(cc *pgx.ConnConfig) {
	if cc == nil {
		return
	}
	if cc.ConnectTimeout == 0 {
		cc.ConnectTimeout = eventConnectTimeout
	}
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = make(map[string]string)
	}
	defaults := map[string]string{
		"tcp_keepalives_idle":     "60",
		"tcp_keepalives_interval": "15",
		"tcp_keepalives_count":    "4",
	}
	for k, v := range defaults {
		if _, ok := cc.RuntimeParams[k]; !ok {
			cc.RuntimeParams[k] = v
		}
	}
}

// --- helpers (pure functions; no receiver state) ---

// execListen runs a LISTEN or UNLISTEN for channel, quoting the identifier so
// case and special characters (e.g. UUID hyphens) match the pg_notify channel.
func execListen(ctx context.Context, conn *pgx.Conn, verb, channel string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	quoted := `"` + strings.ReplaceAll(channel, `"`, `""`) + `"`
	_, err := conn.Exec(cctx, verb+" "+quoted)
	return err
}

// channelsForSubject returns the Postgres channels a subject is published to.
func channelsForSubject(subject string) []string {
	if gc := groveChannelForSubject(subject); gc != "" {
		return []string{gc, pgGlobalChannel}
	}
	return []string{pgGlobalChannel}
}

// channelsForPattern returns the Postgres channels a subscription pattern needs.
// A concrete grove/project pattern resolves to that grove's channel; everything
// else (wildcard grove, or non-grove subjects) resolves to the global channel.
func channelsForPattern(pattern string) []string {
	parts := strings.SplitN(pattern, ".", 3)
	if len(parts) >= 2 && (parts[0] == "project" || parts[0] == "grove") && isConcreteToken(parts[1]) {
		return []string{groveChannel(parts[1])}
	}
	return []string{pgGlobalChannel}
}

// groveChannelForSubject returns the per-grove channel for a grove-scoped
// subject, or "" if the subject is not grove-scoped.
func groveChannelForSubject(subject string) string {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) >= 2 && (parts[0] == "project" || parts[0] == "grove") {
		return groveChannel(parts[1])
	}
	return ""
}

// groveChannel builds the Postgres channel name for a grove id, hashing the id
// if the resulting identifier would exceed the Postgres length limit.
func groveChannel(id string) string {
	name := pgChannelPrefix + "g_" + id
	if len(name) <= maxPGIdentifier {
		return name
	}
	sum := sha256.Sum256([]byte(id))
	return pgChannelPrefix + "g_" + hex.EncodeToString(sum[:])[:32]
}

// channelScope returns a low-cardinality label ("grove" or "global") for the
// channel a subject maps to, suitable for use as a metric attribute.
func channelScope(subject string) string {
	if groveChannelForSubject(subject) != "" {
		return "grove"
	}
	return "global"
}

func isConcreteToken(t string) bool { return t != "" && t != "*" && t != ">" }

// anyPatternMatches reports whether any pattern matches the subject.
func anyPatternMatches(patterns []string, subject string) bool {
	for _, pattern := range patterns {
		if subjectMatchesPattern(pattern, subject) {
			return true
		}
	}
	return false
}

// eventTypeName returns the bare Go type name of an event value (e.g.
// "AgentStatusEvent"), used as the envelope type tag.
func eventTypeName(event interface{}) string {
	t := fmt.Sprintf("%T", event)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// nextBackoff doubles d up to max.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}
