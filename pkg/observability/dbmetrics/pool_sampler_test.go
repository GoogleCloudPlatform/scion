/*
Copyright 2025 The Scion Authors.
*/

package dbmetrics

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// fakeStatsProvider returns a fixed DBStats snapshot.
type fakeStatsProvider struct{ s sql.DBStats }

func (f fakeStatsProvider) Stats() sql.DBStats { return f.s }

// capturingRecorder records ObservePoolStats calls and reports itself enabled.
// All other Recorder methods delegate to a disabled no-op recorder.
type capturingRecorder struct {
	Recorder
	mu   sync.Mutex
	got  []PoolStats
	seen chan struct{}
}

func newCapturingRecorder() *capturingRecorder {
	return &capturingRecorder{Recorder: NewDisabled(), seen: make(chan struct{}, 16)}
}

func (c *capturingRecorder) Enabled() bool { return true }

func (c *capturingRecorder) ObservePoolStats(_ context.Context, stats PoolStats, _ ...attribute.KeyValue) {
	c.mu.Lock()
	c.got = append(c.got, stats)
	c.mu.Unlock()
	select {
	case c.seen <- struct{}{}:
	default:
	}
}

func (c *capturingRecorder) snapshot() []PoolStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PoolStats, len(c.got))
	copy(out, c.got)
	return out
}

func TestPoolStatsFrom(t *testing.T) {
	in := sql.DBStats{
		MaxOpenConnections: 20,
		InUse:              7,
		Idle:               3,
		WaitCount:          42,
	}
	got := poolStatsFrom(in)
	want := PoolStats{Active: 7, Idle: 3, Waiting: 42, Max: 20}
	if got != want {
		t.Fatalf("poolStatsFrom(%+v) = %+v, want %+v", in, got, want)
	}
}

// A disabled recorder must not start a sampling goroutine; stop is a safe no-op.
func TestStartPoolSampler_DisabledIsNoop(t *testing.T) {
	stop := StartPoolSampler(context.Background(), NewDisabled(), fakeStatsProvider{}, time.Millisecond)
	if stop == nil {
		t.Fatal("stop must never be nil")
	}
	stop() // must not panic
}

// A nil db must not start a goroutine.
func TestStartPoolSampler_NilDBIsNoop(t *testing.T) {
	stop := StartPoolSampler(context.Background(), newCapturingRecorder(), nil, time.Millisecond)
	stop()
}

// An enabled recorder samples immediately (without waiting a full interval) and
// stop halts further sampling.
func TestStartPoolSampler_EmitsImmediatelyAndStops(t *testing.T) {
	rec := newCapturingRecorder()
	db := fakeStatsProvider{s: sql.DBStats{MaxOpenConnections: 10, InUse: 2, Idle: 1, WaitCount: 5}}

	stop := StartPoolSampler(context.Background(), rec, db, time.Hour) // long interval: rely on immediate emit
	select {
	case <-rec.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an immediate pool-stats sample")
	}
	stop()

	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one sample")
	}
	want := PoolStats{Active: 2, Idle: 1, Waiting: 5, Max: 10}
	if got[0] != want {
		t.Fatalf("first sample = %+v, want %+v", got[0], want)
	}
}

// Cancelling the parent context stops sampling.
func TestStartPoolSampler_ContextCancelStops(t *testing.T) {
	rec := newCapturingRecorder()
	db := fakeStatsProvider{s: sql.DBStats{MaxOpenConnections: 4}}

	ctx, cancel := context.WithCancel(context.Background())
	stop := StartPoolSampler(ctx, rec, db, time.Hour)
	defer stop()

	select {
	case <-rec.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an immediate sample")
	}
	cancel() // goroutine should observe ctx.Done and exit; no assertion beyond no-panic/no-leak
}
