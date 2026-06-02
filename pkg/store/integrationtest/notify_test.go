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

//go:build integration

// Category 4 — LISTEN/NOTIFY under load. These exercise the raw Postgres
// asynchronous-notification primitive that the hub's PostgresEventPublisher is
// built on: ordered burst delivery without drops, the hard 8000-byte payload
// limit that motivates the publisher's reference-and-refetch offload, listener
// reconnect after a backend is terminated, and strict per-channel isolation.
//
// The higher-level publisher behaviors (reference-and-refetch round-trip,
// automatic resubscribe, NATS-style pattern fan-out) are covered against a live
// database in pkg/hub/events_postgres_test.go. Here we pin the underlying
// database guarantees those features depend on.
package integrationtest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// pgConnect opens a raw pgx connection to the per-package database and registers
// cleanup. LISTEN/NOTIFY channels are database-global (not schema-scoped), so the
// schema in the DSN is irrelevant here; tests use unique channel names to stay
// isolated from one another on the shared database.
func pgConnect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	require.NoError(t, err, "pgx connect")
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// uniqueChannel returns a Postgres channel name safe to use unquoted (hex + '_').
func uniqueChannel(prefix string) string { return "itest_" + prefix + "_" + shortID() }

// TestNotify_BurstDeliveredInOrderNoDrops fires a rapid burst of N notifications
// on one channel and asserts the listener receives all N, in publish order.
// Postgres guarantees ordered, lossless delivery of committed notifications to a
// session that was LISTENing before they were sent.
func TestNotify_BurstDeliveredInOrderNoDrops(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	listener := pgConnect(t, dsn)
	channel := uniqueChannel("burst")
	_, err := listener.Exec(ctx, "LISTEN "+channel)
	require.NoError(t, err)

	notifier := pgConnect(t, dsn)
	const n = 200
	for i := 0; i < n; i++ {
		_, err := notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channel, strconv.Itoa(i))
		require.NoErrorf(t, err, "publishing notification %d", i)
	}

	got := make([]string, 0, n)
	for len(got) < n {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		note, err := listener.WaitForNotification(wctx)
		cancel()
		require.NoErrorf(t, err, "received only %d of %d notifications before timing out", len(got), n)
		require.Equal(t, channel, note.Channel)
		got = append(got, note.Payload)
	}

	for i := 0; i < n; i++ {
		require.Equalf(t, strconv.Itoa(i), got[i], "notification %d out of order or corrupted", i)
	}
}

// TestNotify_OversizedPayloadRejected pins the 8000-byte NOTIFY payload limit:
// a payload at/over the limit is rejected by the server (this is exactly why the
// PostgresEventPublisher offloads oversized events to the scion_event_payloads
// table and notifies a reference id instead), while a payload comfortably under
// the limit is delivered intact.
func TestNotify_OversizedPayloadRejected(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	listener := pgConnect(t, dsn)
	channel := uniqueChannel("size")
	_, err := listener.Exec(ctx, "LISTEN "+channel)
	require.NoError(t, err)

	notifier := pgConnect(t, dsn)

	// At/over 8000 bytes Postgres rejects the NOTIFY.
	oversized := strings.Repeat("x", 8000)
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channel, oversized)
	require.Error(t, err, "Postgres must reject a NOTIFY payload of 8000 bytes")

	// Comfortably under the limit (matching the publisher's 7000-byte threshold)
	// is accepted and delivered intact.
	underLimit := strings.Repeat("y", 7000)
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channel, underLimit)
	require.NoError(t, err)

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	note, err := listener.WaitForNotification(wctx)
	cancel()
	require.NoError(t, err)
	assert.Equal(t, underLimit, note.Payload, "under-limit payload must arrive intact")
}

// TestNotify_ListenerReconnectResumes terminates a listener's backend mid-stream
// (simulating a dropped CloudSQL connection) and verifies that a freshly
// reconnected listener which re-issues LISTEN resumes receiving notifications.
// This is the database-level guarantee the publisher's automatic resubscribe
// loop relies on.
func TestNotify_ListenerReconnectResumes(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	channel := uniqueChannel("reconnect")
	notifier := pgConnect(t, dsn)

	listener1, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = listener1.Exec(ctx, "LISTEN "+channel)
	require.NoError(t, err)

	var pid uint32
	require.NoError(t, listener1.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid))

	// Sanity: delivery works before the drop.
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channel, "before")
	require.NoError(t, err)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	note, err := listener1.WaitForNotification(wctx)
	cancel()
	require.NoError(t, err)
	require.Equal(t, "before", note.Payload)

	// Drop the listener's backend from another session.
	_, err = notifier.Exec(ctx, "SELECT pg_terminate_backend($1)", pid)
	require.NoError(t, err)

	// The dead connection now errors instead of hanging.
	wctx, cancel = context.WithTimeout(ctx, 3*time.Second)
	_, err = listener1.WaitForNotification(wctx)
	cancel()
	assert.Error(t, err, "a terminated backend must surface an error to its listener")
	_ = listener1.Close(ctx)

	// Reconnect, re-LISTEN, and confirm delivery resumes.
	listener2 := pgConnect(t, dsn)
	_, err = listener2.Exec(ctx, "LISTEN "+channel)
	require.NoError(t, err)
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channel, "after")
	require.NoError(t, err)
	wctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	note, err = listener2.WaitForNotification(wctx)
	cancel()
	require.NoError(t, err, "reconnected listener must resume receiving notifications")
	assert.Equal(t, "after", note.Payload)
}

// TestNotify_CrossChannelIsolation verifies notifications are strictly scoped to
// their channel: a listener subscribed only to channel A never observes an event
// published on channel B.
func TestNotify_CrossChannelIsolation(t *testing.T) {
	requirePG(t)
	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	channelA := uniqueChannel("a")
	channelB := uniqueChannel("b")

	listener := pgConnect(t, dsn)
	_, err := listener.Exec(ctx, "LISTEN "+channelA) // only A
	require.NoError(t, err)

	notifier := pgConnect(t, dsn)
	// Publish on B first (must be ignored), then on A.
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channelB, "leak-from-B")
	require.NoError(t, err)
	_, err = notifier.Exec(ctx, "SELECT pg_notify($1, $2)", channelA, "expected-from-A")
	require.NoError(t, err)

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	note, err := listener.WaitForNotification(wctx)
	cancel()
	require.NoError(t, err)
	assert.Equal(t, channelA, note.Channel, "must only receive channel A")
	assert.Equal(t, "expected-from-A", note.Payload)

	// Nothing else should arrive — the B notification must not leak through.
	wctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
	leak, err := listener.WaitForNotification(wctx)
	cancel()
	if err == nil {
		t.Fatalf("listener received an unexpected notification on channel %q: %q", leak.Channel, leak.Payload)
	}
}
