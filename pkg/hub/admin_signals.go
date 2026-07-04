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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	adminSignalChannel = "scion_integration_admin"
)

// AdminSignal is the JSON payload published via NOTIFY on the admin signal channel.
type AdminSignal struct {
	Integration string `json:"integration"`
	Kind        string `json:"kind"` // "config" or "update"
	ID          string `json:"id,omitempty"`
}

// adminSignalExecutor is satisfied by both *pgx.Conn and pgx.Tx, letting the
// publish path run either autocommit or inside a caller's transaction.
type adminSignalExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PublishAdminSignal sends a NOTIFY on the admin signal channel with the given
// signal payload. The exec parameter should be a pgx.Tx to make the NOTIFY
// transactional with the associated row write.
func PublishAdminSignal(ctx context.Context, exec adminSignalExecutor, signal AdminSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("marshal admin signal: %w", err)
	}
	_, err = exec.Exec(ctx, `SELECT pg_notify($1, $2)`, adminSignalChannel, string(payload))
	if err != nil {
		return fmt.Errorf("pg_notify on %s: %w", adminSignalChannel, err)
	}
	return nil
}

// PublishAdminSignalDB sends a NOTIFY using a *sql.DB connection (the
// standard library driver, as used by Ent). This is used when the caller has
// an Ent-managed database connection rather than a raw pgx connection.
func PublishAdminSignalDB(ctx context.Context, db *sql.DB, signal AdminSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("marshal admin signal: %w", err)
	}
	_, err = db.ExecContext(ctx, "SELECT pg_notify($1, $2)", adminSignalChannel, string(payload))
	if err != nil {
		return fmt.Errorf("pg_notify on %s: %w", adminSignalChannel, err)
	}
	return nil
}

// publishAdminSignalTx sends a NOTIFY within an Ent transaction so that the
// signal is delivered atomically with the associated row write.
func publishAdminSignalTx(ctx context.Context, tx *ent.Tx, signal AdminSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("marshal admin signal: %w", err)
	}
	var result sql.Result
	err = tx.Client().Driver().Exec(ctx,
		"SELECT pg_notify($1, $2)",
		[]any{adminSignalChannel, string(payload)},
		&result,
	)
	if err != nil {
		return fmt.Errorf("pg_notify on %s: %w", adminSignalChannel, err)
	}
	return nil
}

// AdminSignalCallback is called when an admin signal is received.
type AdminSignalCallback func(signal AdminSignal)

// AdminSignalListener listens for NOTIFY events on the admin signal channel
// and dispatches them to registered callbacks. It maintains a dedicated
// connection with reconnection logic, isolated from the main event publisher.
type AdminSignalListener struct {
	dsn    string
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.RWMutex
	callbacks []AdminSignalCallback
	onConnect func()
}

// NewAdminSignalListener creates and starts a new listener on the admin signal
// channel. Call Close to stop it.
func NewAdminSignalListener(ctx context.Context, dsn string, log *slog.Logger) *AdminSignalListener {
	if log == nil {
		log = slog.Default()
	}
	listenCtx, cancel := context.WithCancel(ctx)
	l := &AdminSignalListener{
		dsn:    dsn,
		log:    log,
		ctx:    listenCtx,
		cancel: cancel,
	}
	l.wg.Add(1)
	go l.run()
	return l
}

// OnSignal registers a callback that will be invoked for every received signal.
func (l *AdminSignalListener) OnSignal(cb AdminSignalCallback) {
	l.mu.Lock()
	l.callbacks = append(l.callbacks, cb)
	l.mu.Unlock()
}

// SetOnConnect registers a callback invoked on initial connect and every
// reconnect. Use this to re-scan tables for rows missed during LISTEN gaps,
// since Postgres does not queue NOTIFYs for disconnected listeners.
func (l *AdminSignalListener) SetOnConnect(fn func()) {
	l.mu.Lock()
	l.onConnect = fn
	l.mu.Unlock()
}

// Close stops the listener and waits for the background goroutine to exit.
func (l *AdminSignalListener) Close() {
	l.cancel()
	l.wg.Wait()
}

func (l *AdminSignalListener) run() {
	defer l.wg.Done()

	const (
		minBackoff = 250 * time.Millisecond
		maxBackoff = 10 * time.Second
		pollInterval = time.Second
	)
	backoff := minBackoff

	for {
		if l.ctx.Err() != nil {
			return
		}

		conn, err := l.connect()
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			l.log.Warn("Admin signal listener connect failed", "error", err, "backoff", backoff)
			if !l.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		l.log.Info("Admin signal listener connected")
		backoff = minBackoff

		listenCtx, listenCancel := context.WithTimeout(l.ctx, 5*time.Second)
		_, err = conn.Exec(listenCtx, fmt.Sprintf("LISTEN %q", adminSignalChannel))
		listenCancel()
		if err != nil {
			l.log.Warn("Admin signal LISTEN failed", "error", err)
			_ = conn.Close(context.Background())
			continue
		}

		l.mu.RLock()
		oc := l.onConnect
		l.mu.RUnlock()
		if oc != nil {
			oc()
		}

		loopErr := l.listenLoop(conn, pollInterval)
		_ = conn.Close(context.Background())

		if l.ctx.Err() != nil {
			return
		}

		l.log.Warn("Admin signal listener connection lost, reconnecting", "error", loopErr, "backoff", backoff)
		if !l.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (l *AdminSignalListener) connect() (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig(l.dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing admin signal dsn: %w", err)
	}
	applyConnKeepalives(cc)
	return pgx.ConnectConfig(l.ctx, cc)
}

func (l *AdminSignalListener) listenLoop(conn *pgx.Conn, pollInterval time.Duration) error {
	for {
		if l.ctx.Err() != nil {
			return l.ctx.Err()
		}

		waitCtx, cancel := context.WithTimeout(l.ctx, pollInterval)
		notif, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		}

		l.handleNotification(notif.Payload)
	}
}

func (l *AdminSignalListener) handleNotification(payload string) {
	var signal AdminSignal
	if err := json.Unmarshal([]byte(payload), &signal); err != nil {
		l.log.Error("Failed to decode admin signal", "error", err, "payload", payload)
		return
	}

	l.mu.RLock()
	cbs := make([]AdminSignalCallback, len(l.callbacks))
	copy(cbs, l.callbacks)
	l.mu.RUnlock()

	for _, cb := range cbs {
		cb(signal)
	}
}

func (l *AdminSignalListener) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
