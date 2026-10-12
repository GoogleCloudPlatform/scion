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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- pure unit tests (no database required) ---

// TestNotifyBrokerCmd_Payload verifies the SQL and JSON shape of a NOTIFY call
// issued by NotifyBrokerCmd, using the same recExec test double as the event
// publisher tests.
func TestNotifyBrokerCmd_Payload(t *testing.T) {
	bus := &PostgresCommandBus{
		ctx: context.Background(),
	}

	tx := &recExec{}
	if err := bus.NotifyBrokerCmd(context.Background(), tx, "broker-123"); err != nil {
		t.Fatalf("NotifyBrokerCmd: %v", err)
	}

	calls := tx.notifyCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 pg_notify call, got %d", len(calls))
	}

	channel := calls[0].args[0].(string)
	if channel != pgCommandChannel {
		t.Fatalf("channel = %q, want %q", channel, pgCommandChannel)
	}

	var sig cmdSignal
	payload := calls[0].args[1].(string)
	if err := json.Unmarshal([]byte(payload), &sig); err != nil {
		t.Fatalf("decode signal payload: %v", err)
	}
	if sig.BrokerID != "broker-123" {
		t.Fatalf("broker_id = %q, want %q", sig.BrokerID, "broker-123")
	}
	if sig.Kind != "dispatch" {
		t.Fatalf("kind = %q, want %q", sig.Kind, "dispatch")
	}
}

// TestHandleSignal_OwnsLocally verifies that handleSignal invokes the onSignal
// callback only when ownsLocally returns true.
func TestHandleSignal_OwnsLocally(t *testing.T) {
	var mu sync.Mutex
	var reconciled []string

	bus := &PostgresCommandBus{
		ctx: context.Background(),
		log: slog.Default(),
		ownsLocally: func(brokerID string) bool {
			return brokerID == "local-broker"
		},
		onSignal: func(_ context.Context, brokerID string) {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, brokerID)
		},
	}

	// Signal for a locally-owned broker -> should invoke callback.
	sig1, _ := json.Marshal(cmdSignal{BrokerID: "local-broker", Kind: "dispatch"})
	bus.handleSignal(string(sig1))

	// Signal for a remote broker -> should be ignored.
	sig2, _ := json.Marshal(cmdSignal{BrokerID: "remote-broker", Kind: "dispatch"})
	bus.handleSignal(string(sig2))

	mu.Lock()
	defer mu.Unlock()
	if len(reconciled) != 1 {
		t.Fatalf("expected 1 reconcile call, got %d", len(reconciled))
	}
	if reconciled[0] != "local-broker" {
		t.Fatalf("reconciled broker = %q, want %q", reconciled[0], "local-broker")
	}
}

// TestHandleSignal_EmptyBrokerID verifies signals with a missing broker_id are
// silently ignored.
func TestHandleSignal_EmptyBrokerID(t *testing.T) {
	called := false
	bus := &PostgresCommandBus{
		ctx:         context.Background(),
		log:         slog.Default(),
		ownsLocally: func(string) bool { return true },
		onSignal:    func(context.Context, string) { called = true },
	}

	sig, _ := json.Marshal(cmdSignal{Kind: "dispatch"})
	bus.handleSignal(string(sig))

	if called {
		t.Fatal("onSignal should not be called for an empty broker_id")
	}
}

// TestHandleSignal_MalformedJSON verifies malformed payloads don't panic.
func TestHandleSignal_MalformedJSON(t *testing.T) {
	called := false
	bus := &PostgresCommandBus{
		ctx:         context.Background(),
		log:         slog.Default(),
		ownsLocally: func(string) bool { return true },
		onSignal:    func(context.Context, string) { called = true },
	}

	bus.handleSignal("not valid json{{{")

	if called {
		t.Fatal("onSignal should not be called for malformed JSON")
	}
}

// TestSetOnSignal verifies the reconcile callback can be replaced after
// construction.
func TestSetOnSignal(t *testing.T) {
	var mu sync.Mutex
	var called string

	bus := &PostgresCommandBus{
		ctx:         context.Background(),
		log:         slog.Default(),
		ownsLocally: func(string) bool { return true },
		onSignal:    func(_ context.Context, id string) { mu.Lock(); called = "original-" + id; mu.Unlock() },
	}

	bus.SetOnSignal(func(_ context.Context, id string) {
		mu.Lock()
		called = "replaced-" + id
		mu.Unlock()
	})

	sig, _ := json.Marshal(cmdSignal{BrokerID: "b1", Kind: "dispatch"})
	bus.handleSignal(string(sig))

	mu.Lock()
	defer mu.Unlock()
	if called != "replaced-b1" {
		t.Fatalf("called = %q, want %q", called, "replaced-b1")
	}
}

// TestNoopCommandBus_NotifyBrokerCmd verifies the no-op bus is a safe no-op.
func TestNoopCommandBus_NotifyBrokerCmd(t *testing.T) {
	bus := NoopCommandBus{}
	tx := &recExec{}

	if err := bus.NotifyBrokerCmd(context.Background(), tx, "any-broker"); err != nil {
		t.Fatalf("NoopCommandBus.NotifyBrokerCmd: %v", err)
	}

	// No SQL should have been issued.
	if len(tx.notifyCalls()) != 0 {
		t.Fatalf("NoopCommandBus should not issue any SQL, got %d calls", len(tx.notifyCalls()))
	}

	// Close is a safe no-op.
	bus.Close()
}

// --- integration tests (require a live Postgres via SCION_TEST_POSTGRES_DSN) ---

// TestCommandBusIntegration_SignalDelivery starts a real PostgresCommandBus and
// verifies a NOTIFY on scion_broker_cmd is received and invokes the callback.
func TestCommandBusIntegration_SignalDelivery(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	var mu sync.Mutex
	var reconciled []string

	bus, err := NewPostgresCommandBus(ctx, dsn,
		func(brokerID string) bool { return brokerID == "owned-broker" },
		func(_ context.Context, brokerID string) {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, brokerID)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewPostgresCommandBus: %v", err)
	}
	defer bus.Close()

	// Give the listener time to LISTEN.
	time.Sleep(2 * listenPollInterval)

	// Publish a signal via a direct NOTIFY (simulating the tx-scoped path).
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	sig, _ := json.Marshal(cmdSignal{BrokerID: "owned-broker", Kind: "dispatch"})
	if _, err := conn.Exec(ctx, `SELECT pg_notify($1, $2)`, pgCommandChannel, string(sig)); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}

	// Also send a signal for a non-owned broker; it should be ignored.
	sig2, _ := json.Marshal(cmdSignal{BrokerID: "remote-broker", Kind: "dispatch"})
	if _, err := conn.Exec(ctx, `SELECT pg_notify($1, $2)`, pgCommandChannel, string(sig2)); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(reconciled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reconciled) != 1 {
		t.Fatalf("expected exactly 1 reconcile, got %d: %v", len(reconciled), reconciled)
	}
	if reconciled[0] != "owned-broker" {
		t.Fatalf("reconciled %q, want %q", reconciled[0], "owned-broker")
	}
}

// TestCommandBusIntegration_NotifyBrokerCmd verifies NotifyBrokerCmd publishes a
// signal inside a caller's transaction that is received by the listener.
func TestCommandBusIntegration_NotifyBrokerCmd(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	var mu sync.Mutex
	var reconciled []string

	bus, err := NewPostgresCommandBus(ctx, dsn,
		func(string) bool { return true },
		func(_ context.Context, brokerID string) {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, brokerID)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewPostgresCommandBus: %v", err)
	}
	defer bus.Close()

	time.Sleep(2 * listenPollInterval)

	// Use the bus's own pool to create a transaction.
	tx, err := bus.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := bus.NotifyBrokerCmd(ctx, tx, "txn-broker"); err != nil {
		t.Fatalf("NotifyBrokerCmd: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(reconciled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reconciled) != 1 {
		t.Fatalf("expected 1 reconcile, got %d", len(reconciled))
	}
	if reconciled[0] != "txn-broker" {
		t.Fatalf("reconciled %q, want %q", reconciled[0], "txn-broker")
	}
}

// TestCommandBusIntegration_TransactionalRollback verifies that a NOTIFY enrolled
// in a rolled-back transaction is never delivered (mirrors the event publisher's
// transactional rollback test).
func TestCommandBusIntegration_TransactionalRollback(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	var mu sync.Mutex
	var reconciled []string

	bus, err := NewPostgresCommandBus(ctx, dsn,
		func(string) bool { return true },
		func(_ context.Context, brokerID string) {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, brokerID)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewPostgresCommandBus: %v", err)
	}
	defer bus.Close()

	time.Sleep(2 * listenPollInterval)

	// Rolled-back publish: must NOT be delivered.
	txRollback, err := bus.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := bus.NotifyBrokerCmd(ctx, txRollback, "rolled-back-broker"); err != nil {
		t.Fatalf("NotifyBrokerCmd: %v", err)
	}
	if err := txRollback.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Wait to ensure no spurious delivery.
	time.Sleep(2 * time.Second)

	mu.Lock()
	n := len(reconciled)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("rolled-back signal was delivered: %v", reconciled)
	}

	// Committed publish: must be delivered.
	txCommit, err := bus.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := bus.NotifyBrokerCmd(ctx, txCommit, "committed-broker"); err != nil {
		t.Fatalf("NotifyBrokerCmd: %v", err)
	}
	if err := txCommit.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n = len(reconciled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reconciled) != 1 || reconciled[0] != "committed-broker" {
		t.Fatalf("expected [committed-broker], got %v", reconciled)
	}
}

// TestCommandBusIntegration_Reconnect terminates the listener's backend
// connection and verifies the bus reconnects and resumes delivery.
func TestCommandBusIntegration_Reconnect(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	var mu sync.Mutex
	var reconciled []string

	bus, err := NewPostgresCommandBus(ctx, dsn,
		func(string) bool { return true },
		func(_ context.Context, brokerID string) {
			mu.Lock()
			defer mu.Unlock()
			reconciled = append(reconciled, brokerID)
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewPostgresCommandBus: %v", err)
	}
	defer bus.Close()

	time.Sleep(2 * listenPollInterval)

	// Forcibly terminate all LISTENing backends for this database.
	if _, err := bus.pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE query ILIKE 'LISTEN %' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatalf("terminate backends: %v", err)
	}

	// Wait for reconnect + resubscribe.
	time.Sleep(3 * time.Second)

	// Publish a signal after reconnect.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	sig, _ := json.Marshal(cmdSignal{BrokerID: "after-reconnect", Kind: "dispatch"})
	if _, err := conn.Exec(ctx, `SELECT pg_notify($1, $2)`, pgCommandChannel, string(sig)); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(reconciled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reconciled) == 0 {
		t.Fatal("expected delivery after reconnect, got none")
	}
	found := false
	for _, id := range reconciled {
		if id == "after-reconnect" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected after-reconnect in reconciled, got %v", reconciled)
	}
}

// TestCommandBusIntegration_CloseIsIdempotent verifies double-close is safe.
func TestCommandBusIntegration_CloseIsIdempotent(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	bus, err := NewPostgresCommandBus(ctx, dsn, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewPostgresCommandBus: %v", err)
	}
	bus.Close()
	bus.Close() // must not panic
}
