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
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func mkProject(id string) *store.Project {
	return &store.Project{ID: id, Name: id, Slug: id, Created: time.Now()}
}

func mkMessage(agentID, msg string) *store.Message {
	return &store.Message{
		ID:        "msg-" + agentID,
		AgentID:   agentID,
		Sender:    "agent:" + agentID,
		Recipient: "agent:" + agentID,
		Msg:       msg,
		Type:      "instruction",
		CreatedAt: time.Now(),
	}
}

// --- test doubles ---

// recExec records Exec calls so publish-path tests can assert the SQL and
// arguments without a real database.
type recExec struct {
	mu    sync.Mutex
	calls []recCall
}

type recCall struct {
	sql  string
	args []any
}

func (e *recExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, recCall{sql: sql, args: args})
	return pgconn.CommandTag{}, nil
}

func (e *recExec) notifyCalls() []recCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recCall
	for _, c := range e.calls {
		if strings.Contains(c.sql, "pg_notify") {
			out = append(out, c)
		}
	}
	return out
}

func (e *recExec) inserts() []recCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recCall
	for _, c := range e.calls {
		if strings.Contains(c.sql, "INSERT INTO scion_event_payloads") {
			out = append(out, c)
		}
	}
	return out
}

// countingRecorder is a dbmetrics.Recorder that tallies calls for assertions.
type countingRecorder struct {
	mu             sync.Mutex
	published      int64
	delivered      int64
	dropped        int64
	reconnects     int64
	payloadSizes   []int64
	latencies      []float64
	poolObserved   int
	enabledReturns bool
}

func (r *countingRecorder) RecordPublishToDeliverLatency(_ context.Context, ms float64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latencies = append(r.latencies, ms)
}
func (r *countingRecorder) IncPublished(_ context.Context, n int64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published += n
}
func (r *countingRecorder) IncDelivered(_ context.Context, n int64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivered += n
}
func (r *countingRecorder) IncDropped(_ context.Context, n int64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped += n
}
func (r *countingRecorder) ObserveSubscriberLag(_ context.Context, _ int64, _ ...attribute.KeyValue) {
}
func (r *countingRecorder) IncListenerReconnects(_ context.Context, n int64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconnects += n
}
func (r *countingRecorder) RecordPayloadSize(_ context.Context, bytes int64, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloadSizes = append(r.payloadSizes, bytes)
}
func (r *countingRecorder) ObservePoolStats(_ context.Context, _ dbmetrics.PoolStats, _ ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poolObserved++
}
func (r *countingRecorder) Enabled() bool { return r.enabledReturns }

func (r *countingRecorder) snapshot() (pub, del, drop, recon int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.published, r.delivered, r.dropped, r.reconnects
}

// newTestPostgresPublisher builds a publisher with no live connection or
// goroutines, suitable for exercising the pure routing/registry/publish logic.
func newTestPostgresPublisher(rec dbmetrics.Recorder) *PostgresEventPublisher {
	if rec == nil {
		rec = dbmetrics.NewDisabled()
	}
	p := &PostgresEventPublisher{
		metrics: rec,
		log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		ctx:     context.Background(),
		subs:    make(map[string]map[*pgSubscription][]string),
		desired: make(map[string]int),
	}
	p.sink = p.publish
	return p
}

// --- pure helper tests (no database required) ---

func TestChannelsForSubject(t *testing.T) {
	tests := []struct {
		subject string
		want    []string
	}{
		{"project.G1.agent.status", []string{groveChannel("G1"), pgGlobalChannel}},
		{"grove.G2.notification", []string{groveChannel("G2"), pgGlobalChannel}},
		{"agent.A1.status", []string{pgGlobalChannel}},
		{"user.U1.message", []string{pgGlobalChannel}},
		{"broker.B1.status", []string{pgGlobalChannel}},
		{"admin.allowlist.changed", []string{pgGlobalChannel}},
		{"notification.created", []string{pgGlobalChannel}},
	}
	for _, tt := range tests {
		got := channelsForSubject(tt.subject)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("channelsForSubject(%q) = %v, want %v", tt.subject, got, tt.want)
		}
	}
}

func TestChannelsForPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    []string
	}{
		{"project.G1.>", []string{groveChannel("G1")}},
		{"grove.G2.agent.status", []string{groveChannel("G2")}},
		{"project.>.agent.status", []string{pgGlobalChannel}}, // spanning wildcard
		{"project.*.agent.status", []string{pgGlobalChannel}}, // single-token wildcard grove
		{"agent.A1.message", []string{pgGlobalChannel}},
		{"notification.created", []string{pgGlobalChannel}},
	}
	for _, tt := range tests {
		got := channelsForPattern(tt.pattern)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("channelsForPattern(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestGroveChannel_BoundedLength(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := groveChannel(long)
	if len(got) > maxPGIdentifier {
		t.Errorf("groveChannel(long) length = %d, want <= %d", len(got), maxPGIdentifier)
	}
	// Deterministic.
	if got != groveChannel(long) {
		t.Errorf("groveChannel not deterministic")
	}
	// A normal UUID-length id is passed through unhashed.
	uuidLike := "11111111-2222-3333-4444-555555555555"
	if groveChannel(uuidLike) != pgChannelPrefix+"g_"+uuidLike {
		t.Errorf("groveChannel(uuid) = %q, want passthrough", groveChannel(uuidLike))
	}
}

func TestEventTypeName(t *testing.T) {
	if got := eventTypeName(AgentStatusEvent{}); got != "AgentStatusEvent" {
		t.Errorf("eventTypeName = %q, want AgentStatusEvent", got)
	}
}

// --- registry / fan-out tests (no database required) ---

// TestPostgresFanout_ScopedSubscriberNoDoubleDelivery verifies a grove-scoped
// subscriber receives grove events on the grove channel exactly once and is not
// also matched on the global channel (which carries a mirror of grove events).
func TestPostgresFanout_ScopedSubscriberNoDoubleDelivery(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	ch, unsub := p.Subscribe("project.G1.>")
	defer unsub()

	evt := Event{Subject: "project.G1.agent.status", Data: []byte(`{}`)}

	// Delivered on the grove channel.
	p.fanout(groveChannel("G1"), evt)
	select {
	case got := <-ch:
		if got.Subject != evt.Subject {
			t.Fatalf("got subject %q", got.Subject)
		}
	case <-time.After(time.Second):
		t.Fatal("expected delivery on grove channel")
	}

	// NOT delivered again on the global channel: the subscriber's patterns do
	// not resolve to the global channel.
	p.fanout(pgGlobalChannel, evt)
	select {
	case got := <-ch:
		t.Fatalf("unexpected duplicate delivery on global channel: %q", got.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPostgresFanout_SpanningSubscriber verifies a grove-spanning subscriber
// (e.g. the notification dispatcher) receives grove events via the global
// channel and not via the per-grove channel.
func TestPostgresFanout_SpanningSubscriber(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	ch, unsub := p.Subscribe("project.>.agent.status")
	defer unsub()

	evt := Event{Subject: "project.G9.agent.status", Data: []byte(`{}`)}

	p.fanout(pgGlobalChannel, evt)
	select {
	case got := <-ch:
		if got.Subject != evt.Subject {
			t.Fatalf("got subject %q", got.Subject)
		}
	case <-time.After(time.Second):
		t.Fatal("expected delivery on global channel for spanning subscriber")
	}

	// The per-grove channel must not deliver to a spanning subscriber.
	p.fanout(groveChannel("G9"), evt)
	select {
	case got := <-ch:
		t.Fatalf("unexpected delivery on grove channel: %q", got.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPostgresFanout_MixedPatternsNoDuplicate verifies that a single Subscribe
// call mixing a grove-scoped and a non-grove pattern never double-delivers an
// event that happens to be mirrored onto both channels.
func TestPostgresFanout_MixedPatternsNoDuplicate(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	ch, unsub := p.Subscribe("project.G1.agent.status", "agent.A1.message")
	defer unsub()

	evt := Event{Subject: "project.G1.agent.status", Data: []byte(`{}`)}

	// On the global channel, only the agent.A1.message pattern is active, which
	// does not match the project subject -> no delivery here.
	p.fanout(pgGlobalChannel, evt)
	// On the grove channel, the project pattern matches -> exactly one delivery.
	p.fanout(groveChannel("G1"), evt)

	received := 0
	for {
		select {
		case <-ch:
			received++
		case <-time.After(150 * time.Millisecond):
			if received != 1 {
				t.Fatalf("expected exactly 1 delivery, got %d", received)
			}
			return
		}
	}
}

func TestPostgresSubscribe_Unsubscribe(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	ch, unsub := p.Subscribe("project.G1.>")

	gc := groveChannel("G1")
	if p.desired[gc] != 1 {
		t.Fatalf("desired[%s] = %d, want 1", gc, p.desired[gc])
	}
	unsub()
	if _, ok := p.desired[gc]; ok {
		t.Fatalf("desired[%s] should be cleared after unsubscribe", gc)
	}
	// Channel must be closed.
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel should be closed after unsubscribe")
	}
	// Double unsubscribe is safe.
	unsub()
}

// --- publish-path tests using a fake executor (no database required) ---

func TestBuildAndNotify_SmallPayload(t *testing.T) {
	rec := &countingRecorder{}
	p := newTestPostgresPublisher(rec)
	exec := &recExec{}

	err := p.buildAndNotify(context.Background(), exec, "project.G1.agent.status", AgentStatusEvent{AgentID: "a1"})
	if err != nil {
		t.Fatalf("buildAndNotify: %v", err)
	}

	// Grove subject -> NOTIFY on grove channel and global channel.
	notifies := exec.notifyCalls()
	if len(notifies) != 2 {
		t.Fatalf("expected 2 pg_notify calls, got %d", len(notifies))
	}
	gotChannels := map[string]bool{}
	for _, c := range notifies {
		gotChannels[c.args[0].(string)] = true
		// Payload should carry the inline data and the event type.
		var env pgEnvelope
		if err := json.Unmarshal([]byte(c.args[1].(string)), &env); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if env.Ref != "" {
			t.Fatalf("small payload should not be offloaded; got ref %q", env.Ref)
		}
		if env.Type != "AgentStatusEvent" {
			t.Fatalf("envelope type = %q", env.Type)
		}
		if len(env.Data) == 0 {
			t.Fatal("envelope data should be inline")
		}
	}
	if !gotChannels[groveChannel("G1")] || !gotChannels[pgGlobalChannel] {
		t.Fatalf("notify channels = %v", gotChannels)
	}
	if len(exec.inserts()) != 0 {
		t.Fatalf("small payload must not INSERT into payload table")
	}
	if pub, _, _, _ := rec.snapshot(); pub != 1 {
		t.Fatalf("published metric = %d, want 1", pub)
	}
}

func TestBuildAndNotify_OversizedPayloadOffloaded(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	exec := &recExec{}

	// A message larger than the NOTIFY threshold forces reference-and-refetch.
	big := strings.Repeat("z", pgNotifyMaxPayload+500)
	err := p.buildAndNotify(context.Background(), exec, "agent.A1.message", UserMessageEvent{ID: "m1", Msg: big})
	if err != nil {
		t.Fatalf("buildAndNotify: %v", err)
	}

	if got := len(exec.inserts()); got != 1 {
		t.Fatalf("oversized payload should INSERT once, got %d", got)
	}
	notifies := exec.notifyCalls()
	if len(notifies) != 1 { // non-grove subject -> global only
		t.Fatalf("expected 1 pg_notify, got %d", len(notifies))
	}
	var env pgEnvelope
	if err := json.Unmarshal([]byte(notifies[0].args[1].(string)), &env); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if env.Ref == "" {
		t.Fatal("oversized envelope should carry a ref")
	}
	if len(env.Data) != 0 {
		t.Fatal("oversized envelope must not inline data")
	}
	if len(notifies[0].args[1].(string)) > pgNotifyMaxPayload {
		t.Fatalf("reference envelope still exceeds NOTIFY limit: %d bytes", len(notifies[0].args[1].(string)))
	}
}

// TestPublishTx_UsesProvidedExecutor verifies the transactional publish path
// enrolls the NOTIFY on the caller's transaction (the fake executor) rather than
// the pool, which is what gives rollback==no-deliver semantics at the DB layer.
func TestPublishTx_UsesProvidedExecutor(t *testing.T) {
	p := newTestPostgresPublisher(nil)
	tx := &recExec{}

	if err := p.PublishTx(context.Background(), tx, "grove.G1.created", ProjectCreatedEvent{ProjectID: "G1"}); err != nil {
		t.Fatalf("PublishTx: %v", err)
	}
	if len(tx.notifyCalls()) == 0 {
		t.Fatal("PublishTx should issue pg_notify on the provided transaction")
	}
}

func TestHandleNotification_InlineDeliversAndRecordsMetrics(t *testing.T) {
	rec := &countingRecorder{enabledReturns: true}
	p := newTestPostgresPublisher(rec)
	ch, unsub := p.Subscribe("agent.A1.status")
	defer unsub()

	data, _ := json.Marshal(AgentStatusEvent{AgentID: "A1", Phase: "running"})
	env := pgEnvelope{Type: "AgentStatusEvent", Subject: "agent.A1.status", Data: data, TS: time.Now().Add(-5 * time.Millisecond).UnixNano()}
	payload, _ := json.Marshal(env)

	p.handleNotification(pgGlobalChannel, string(payload))

	select {
	case got := <-ch:
		if got.Subject != "agent.A1.status" {
			t.Fatalf("subject = %q", got.Subject)
		}
	case <-time.After(time.Second):
		t.Fatal("expected delivery")
	}

	if _, del, _, _ := rec.snapshot(); del != 1 {
		t.Fatalf("delivered metric = %d, want 1", del)
	}
	rec.mu.Lock()
	gotLatency := len(rec.latencies)
	rec.mu.Unlock()
	if gotLatency != 1 {
		t.Fatalf("expected 1 latency sample, got %d", gotLatency)
	}
}

func TestHandleNotification_FullBufferDropsAndCounts(t *testing.T) {
	rec := &countingRecorder{}
	p := newTestPostgresPublisher(rec)
	// Register a subscriber with a tiny buffer by reaching into the registry.
	sub := &pgSubscription{ch: make(chan Event, 1)}
	p.subs[pgGlobalChannel] = map[*pgSubscription][]string{sub: {"agent.>"}}

	evt := Event{Subject: "agent.A1.status", Data: []byte(`{}`)}
	p.fanout(pgGlobalChannel, evt) // fills the buffer (delivered)
	p.fanout(pgGlobalChannel, evt) // dropped (buffer full)

	_, del, drop, _ := rec.snapshot()
	if del != 1 || drop != 1 {
		t.Fatalf("delivered=%d dropped=%d, want 1 and 1", del, drop)
	}
}

// --- integration tests (require a live Postgres via SCION_TEST_POSTGRES_DSN) ---

func requirePostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run Postgres LISTEN/NOTIFY integration tests")
	}
	return dsn
}

// TestPostgresIntegration_CrossReplicaDelivery starts two independent publishers
// against the same database (simulating two hub replicas) and asserts an event
// published on one is delivered to a subscriber on the other.
func TestPostgresIntegration_CrossReplicaDelivery(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	a, err := NewPostgresEventPublisher(ctx, dsn, dbmetrics.NewDisabled(), nil)
	if err != nil {
		t.Fatalf("publisher A: %v", err)
	}
	defer a.Close()
	b, err := NewPostgresEventPublisher(ctx, dsn, dbmetrics.NewDisabled(), nil)
	if err != nil {
		t.Fatalf("publisher B: %v", err)
	}
	defer b.Close()

	pid := "proj-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	ch, unsub := b.Subscribe("project." + pid + ".>")
	defer unsub()

	// Give B's listener time to LISTEN on the grove channel.
	time.Sleep(2 * listenPollInterval)

	a.PublishProjectCreated(ctx, mkProject(pid))

	select {
	case got := <-ch:
		if !strings.HasPrefix(got.Subject, "project."+pid) {
			t.Fatalf("unexpected subject %q", got.Subject)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cross-replica event not delivered")
	}
}

// TestPostgresIntegration_OversizedRoundTrip verifies an event larger than the
// NOTIFY limit is delivered intact via reference-and-refetch.
func TestPostgresIntegration_OversizedRoundTrip(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	pub, err := NewPostgresEventPublisher(ctx, dsn, dbmetrics.NewDisabled(), nil)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()

	aid := "agent-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	ch, unsub := pub.Subscribe("agent." + aid + ".message")
	defer unsub()
	time.Sleep(2 * listenPollInterval)

	big := strings.Repeat("Q", pgNotifyMaxPayload+2048)
	pub.PublishUserMessage(ctx, mkMessage(aid, big))

	select {
	case got := <-ch:
		var evt UserMessageEvent
		if err := json.Unmarshal(got.Data, &evt); err != nil {
			t.Fatalf("decode delivered event: %v", err)
		}
		if evt.Msg != big {
			t.Fatalf("oversized payload not delivered intact: got %d bytes, want %d", len(evt.Msg), len(big))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("oversized event not delivered")
	}
}

// TestPostgresIntegration_TransactionalRollback verifies a NOTIFY enrolled in a
// rolled-back transaction is never delivered, while a committed one is.
func TestPostgresIntegration_TransactionalRollback(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	pub, err := NewPostgresEventPublisher(ctx, dsn, dbmetrics.NewDisabled(), nil)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()

	pid := "txn-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	ch, unsub := pub.Subscribe("project." + pid + ".>")
	defer unsub()
	time.Sleep(2 * listenPollInterval)

	// Rolled-back publish: must NOT be delivered.
	txRollback, err := pub.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := pub.PublishTx(ctx, txRollback, "project."+pid+".updated", ProjectUpdatedEvent{ProjectID: pid, Name: "rolled-back"}); err != nil {
		t.Fatalf("PublishTx: %v", err)
	}
	if err := txRollback.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	select {
	case got := <-ch:
		t.Fatalf("rolled-back event was delivered: %q", got.Subject)
	case <-time.After(2 * time.Second):
		// expected: nothing delivered
	}

	// Committed publish: must be delivered.
	txCommit, err := pub.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := pub.PublishTx(ctx, txCommit, "project."+pid+".updated", ProjectUpdatedEvent{ProjectID: pid, Name: "committed"}); err != nil {
		t.Fatalf("PublishTx: %v", err)
	}
	if err := txCommit.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case got := <-ch:
		if got.Subject != "project."+pid+".updated" {
			t.Fatalf("subject = %q", got.Subject)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("committed event not delivered")
	}
}

// TestPostgresIntegration_HandlerCreateProjectEmitsNotify exercises the full
// production publish path end-to-end: an HTTP project-create request handled by
// the Hub server calls s.events.PublishProjectCreated on a real
// PostgresEventPublisher, which must emit a pg_notify observable by an
// independent raw LISTEN connection. This is the exact capability the
// multi-replica live test probed with psql (create project => NOTIFY on
// scion_ev_global); it guards against regressions in the cmd-level wiring that
// connects the handler's s.events to the Postgres backend.
func TestPostgresIntegration_HandlerCreateProjectEmitsNotify(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	srv, _ := testServer(t)
	pub, err := NewPostgresEventPublisher(ctx, dsn, dbmetrics.NewDisabled(), nil)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()
	srv.SetEventPublisher(pub)

	// Independent raw LISTEN on the global channel — bypasses the publisher's own
	// listener/subscription machinery, mirroring the psql probe from the live test.
	lconn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("listen conn: %v", err)
	}
	defer func() { _ = lconn.Close(context.Background()) }()
	if _, err := lconn.Exec(ctx, `LISTEN scion_ev_global`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{
		"name": "pg-notify-wiring-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Expect a project.<id>.created NOTIFY on the global channel.
	deadline := time.Now().Add(5 * time.Second)
	for {
		wctx, cancel := context.WithTimeout(ctx, time.Until(deadline))
		n, werr := lconn.WaitForNotification(wctx)
		cancel()
		if werr != nil {
			t.Fatalf("no NOTIFY observed for handler-driven project create (publish path not wired): %v", werr)
		}
		if strings.Contains(n.Payload, ".created") {
			return // success: handler -> s.events -> pg_notify works
		}
	}
}

// TestPostgresIntegration_ReconnectResubscribe terminates the listener's backend
// connection and verifies the publisher reconnects, re-LISTENs, and resumes
// delivery, incrementing the reconnect metric.
func TestPostgresIntegration_ReconnectResubscribe(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := context.Background()

	rec := &countingRecorder{enabledReturns: true}
	pub, err := NewPostgresEventPublisher(ctx, dsn, rec, nil)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()

	pid := "rc-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	ch, unsub := pub.Subscribe("project." + pid + ".>")
	defer unsub()
	time.Sleep(2 * listenPollInterval)

	// Forcibly terminate all LISTENing backends for this database, dropping the
	// listener connection and forcing a reconnect.
	if _, err := pub.pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE query ILIKE 'LISTEN %' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatalf("terminate backends: %v", err)
	}

	// Wait for reconnect + resubscribe.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, recon := rec.snapshot(); recon > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, _, _, recon := rec.snapshot(); recon == 0 {
		t.Fatal("expected a listener reconnect to be recorded")
	}

	// Allow the resubscribe poll to re-LISTEN, then verify delivery resumes.
	time.Sleep(2 * listenPollInterval)
	pub.PublishProjectCreated(ctx, mkProject(pid))

	select {
	case got := <-ch:
		if !strings.HasPrefix(got.Subject, "project."+pid) {
			t.Fatalf("unexpected subject %q", got.Subject)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not resume after reconnect")
	}
}
