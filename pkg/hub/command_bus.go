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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommandBus abstracts the inter-node command signal channel. The Postgres
// implementation LISTENs on scion_broker_cmd; the no-op implementation is
// used for SQLite (single-process, all brokers are local).
type CommandBus interface {
	// NotifyBrokerCmd issues a NOTIFY signal inside the caller's transaction,
	// so the signal commits atomically with the durable intent row.
	NotifyBrokerCmd(ctx context.Context, tx pgExecutor, brokerID string) error
	// SignalBrokerCmd is a best-effort NOTIFY using the bus's own pool (not
	// tx-scoped). Used by the message dispatch path where the durable intent
	// is the message row itself and the NOTIFY is only a wakeup hint.
	SignalBrokerCmd(ctx context.Context, brokerID string) error
	Close()
}

const (
	// pgCommandChannel is the global Postgres NOTIFY channel for broker
	// command signals. Every hub instance LISTENs on this single channel and
	// filters by local ownership.
	pgCommandChannel = "scion_broker_cmd"
)

// cmdSignal is the JSON wire format for the NOTIFY payload on scion_broker_cmd.
// It is intentionally tiny: the durable command lives in the DB; this is only
// a wakeup.
type cmdSignal struct {
	BrokerID string `json:"broker_id"`
	Kind     string `json:"kind"`
}

// PostgresCommandBus is a sibling of PostgresEventPublisher that LISTENs on
// scion_broker_cmd for dispatch wakeup signals. It maintains its OWN pgx
// connection (listener) and pool (publisher) so dispatch and event-fanout are
// independently pooled (design §5.1).
//
// On receiving a signal the bus checks local ownership via the injected
// ownsLocally func: if this node holds the broker's WebSocket, it invokes the
// onSignal callback (which will be wired to the reconcile drain in B2-5).
type PostgresCommandBus struct {
	pool *pgxpool.Pool
	dsn  string
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.RWMutex
	ownsLocally func(brokerID string) bool
	onSignal    func(ctx context.Context, brokerID string)
	onReconnect func()
	closed      bool
}

var _ CommandBus = (*PostgresCommandBus)(nil)

// NewPostgresCommandBus creates a command bus backed by Postgres LISTEN/NOTIFY.
// ownsLocally should return true when this process holds the broker's control-
// channel WebSocket (typically controlChannel.manager.IsConnected). onSignal
// is the reconcile callback invoked when a signal arrives for a locally-owned
// broker.
func NewPostgresCommandBus(
	ctx context.Context,
	dsn string,
	ownsLocally func(brokerID string) bool,
	onSignal func(ctx context.Context, brokerID string),
	log *slog.Logger,
) (*PostgresCommandBus, error) {
	if log == nil {
		log = slog.Default()
	}
	if ownsLocally == nil {
		ownsLocally = func(string) bool { return false }
	}
	if onSignal == nil {
		onSignal = func(context.Context, string) {}
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing command bus dsn: %w", err)
	}
	applyEventPoolKeepalives(poolCfg)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating command bus pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres for command bus: %w", err)
	}

	busCtx, cancel := context.WithCancel(context.Background())
	b := &PostgresCommandBus{
		pool:        pool,
		dsn:         dsn,
		log:         log,
		ctx:         busCtx,
		cancel:      cancel,
		ownsLocally: ownsLocally,
		onSignal:    onSignal,
	}

	b.wg.Add(1)
	go b.runListener()

	log.Info("Postgres command bus started", "channel", pgCommandChannel)
	return b, nil
}

// SetOnReconnect sets a callback invoked each time the listener reconnects
// after a connection loss. Used by B5-2 to increment a reconnects counter.
func (b *PostgresCommandBus) SetOnReconnect(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onReconnect = fn
}

// SetOnSignal replaces the reconcile callback. This allows wiring the
// reconcile drain (B2-5) after construction.
func (b *PostgresCommandBus) SetOnSignal(fn func(ctx context.Context, brokerID string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if fn == nil {
		fn = func(context.Context, string) {}
	}
	b.onSignal = fn
}

// NotifyBrokerCmd issues NOTIFY scion_broker_cmd inside the caller's
// transaction, so the signal commits atomically with the durable intent.
func (b *PostgresCommandBus) NotifyBrokerCmd(ctx context.Context, tx pgExecutor, brokerID string) error {
	sig := cmdSignal{BrokerID: brokerID, Kind: "dispatch"}
	payload, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("marshaling command signal: %w", err)
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, pgCommandChannel, string(payload))
	if err != nil {
		return fmt.Errorf("pg_notify on %s: %w", pgCommandChannel, err)
	}
	return nil
}

// SignalBrokerCmd issues a best-effort NOTIFY using the bus's own pool.
func (b *PostgresCommandBus) SignalBrokerCmd(ctx context.Context, brokerID string) error {
	return b.NotifyBrokerCmd(ctx, b.pool, brokerID)
}

// Close stops the listener and releases the pool.
func (b *PostgresCommandBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()

	b.cancel()
	b.wg.Wait()
	b.pool.Close()
}

// runListener mirrors PostgresEventPublisher.runListener: maintain a dedicated
// LISTEN connection with backoff-reconnect.
func (b *PostgresCommandBus) runListener() {
	defer b.wg.Done()

	const (
		minBackoff = 250 * time.Millisecond
		maxBackoff = 10 * time.Second
	)
	backoff := minBackoff
	firstConnect := true

	for {
		if b.ctx.Err() != nil {
			return
		}

		conn, err := b.connectListener(b.ctx)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			b.log.Warn("Command bus listener connect failed, retrying", "error", err, "backoff", backoff)
			if !b.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if !firstConnect {
			b.mu.RLock()
			fn := b.onReconnect
			b.mu.RUnlock()
			if fn != nil {
				fn()
			}
		}
		firstConnect = false
		b.log.Info("Command bus listener connected")
		backoff = minBackoff

		loopErr := b.listenLoop(conn)
		_ = conn.Close(context.Background())

		if b.ctx.Err() != nil {
			return
		}

		b.log.Warn("Command bus listener connection lost, reconnecting", "error", loopErr, "backoff", backoff)
		if !b.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// connectListener opens a dedicated LISTEN connection with TCP keepalives,
// reusing the same helper as PostgresEventPublisher.
func (b *PostgresCommandBus) connectListener(ctx context.Context) (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig(b.dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing command bus listener dsn: %w", err)
	}
	applyConnKeepalives(cc)
	return pgx.ConnectConfig(ctx, cc)
}

// listenLoop LISTENs on scion_broker_cmd and dispatches signals.
func (b *PostgresCommandBus) listenLoop(conn *pgx.Conn) error {
	if err := execListen(b.ctx, conn, "LISTEN", pgCommandChannel); err != nil {
		return fmt.Errorf("LISTEN %s: %w", pgCommandChannel, err)
	}

	for {
		if b.ctx.Err() != nil {
			return b.ctx.Err()
		}

		waitCtx, cancel := context.WithTimeout(b.ctx, listenPollInterval)
		notif, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		}

		b.handleSignal(notif.Payload)
	}
}

// handleSignal decodes a command signal and, if this node owns the broker,
// invokes the reconcile callback.
func (b *PostgresCommandBus) handleSignal(payload string) {
	var sig cmdSignal
	if err := json.Unmarshal([]byte(payload), &sig); err != nil {
		b.log.Error("Failed to decode command signal", "error", err)
		return
	}

	if sig.BrokerID == "" {
		b.log.Warn("Command signal missing broker_id, ignoring")
		return
	}

	b.mu.RLock()
	owns := b.ownsLocally(sig.BrokerID)
	onSig := b.onSignal
	b.mu.RUnlock()

	if !owns {
		return
	}

	b.log.Info("Command signal received for local broker, invoking reconcile",
		"broker_id", sig.BrokerID, "kind", sig.Kind)
	onSig(b.ctx, sig.BrokerID)
}

// sleep waits for d or until the bus context is canceled.
func (b *PostgresCommandBus) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- No-op command bus for SQLite (single-process) ---

// NoopCommandBus is a no-op CommandBus for the SQLite backend. In single-
// process mode every broker is local; no inter-node signal is needed.
type NoopCommandBus struct{}

var _ CommandBus = NoopCommandBus{}

func (NoopCommandBus) NotifyBrokerCmd(context.Context, pgExecutor, string) error { return nil }
func (NoopCommandBus) SignalBrokerCmd(context.Context, string) error             { return nil }
func (NoopCommandBus) Close()                                                    {}
