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
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeDecisionAuditStore implements only CreateDecisionAudit; every other
// store.Store method panics through the nil embedded interface.
type fakeDecisionAuditStore struct {
	store.Store

	mu       sync.Mutex
	written  []*store.DecisionAuditRecord
	attempts int
	// failFirst makes the first failFirst attempts return failErr.
	failFirst int
	failErr   error
	// gate, if non-nil, blocks each write until it is closed or ctx is done.
	gate chan struct{}
	// delay is slept (respecting ctx) before each write.
	delay   time.Duration
	entered chan struct{} // receives one value per attempt, if non-nil
}

func (f *fakeDecisionAuditStore) CreateDecisionAudit(ctx context.Context, r *store.DecisionAuditRecord) error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failFirst {
		return f.failErr
	}
	f.written = append(f.written, r)
	return nil
}

func (f *fakeDecisionAuditStore) snapshot() (written []string, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.written {
		written = append(written, r.Reason)
	}
	return written, f.attempts
}

type fakeDecisionAuditMetrics struct {
	mu        sync.Mutex
	drops     map[string]int
	writes    int
	failures  int
	maxDepth  int64
	lastDepth int64
}

func (m *fakeDecisionAuditMetrics) RecordDecisionAuditDrop(reason DecisionAuditDropReason, decision string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drops == nil {
		m.drops = map[string]int{}
	}
	m.drops[string(reason)+"/"+decision]++
}

func (m *fakeDecisionAuditMetrics) SetDecisionAuditQueueDepth(depth int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDepth = depth
	if depth > m.maxDepth {
		m.maxDepth = depth
	}
}

func (m *fakeDecisionAuditMetrics) RecordDecisionAuditWrite(_ time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if !success {
		m.failures++
	}
}

func testDecisionAuditConfig() decisionAuditWriterConfig {
	return decisionAuditWriterConfig{
		queueSize:      16,
		workers:        1,
		maxAttempts:    4,
		attemptTimeout: 10 * time.Second,
		backoffBase:    time.Millisecond,
		backoffMax:     4 * time.Millisecond,
		drainTimeout:   5 * time.Second,
		dropLogEvery:   time.Minute,
	}
}

func newTestDecisionAuditEmitter(t *testing.T, s store.Store, cfg decisionAuditWriterConfig) *StoreDecisionAuditEmitter {
	t.Helper()
	e := newStoreDecisionAuditEmitter(s, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}

func auditRec(result, tag string) *store.DecisionAuditRecord {
	return &store.DecisionAuditRecord{Result: result, Reason: tag}
}

func TestDecisionAuditWriter_EnqueueAndWrite(t *testing.T) {
	fs := &fakeDecisionAuditStore{}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())

	e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
	e.EmitDecisionAudit(context.Background(), auditRec("allow", "a1"))
	e.Close(context.Background())

	written, _ := fs.snapshot()
	assert.ElementsMatch(t, []string{"d1", "a1"}, written)
	for _, r := range fs.written {
		assert.NotEmpty(t, r.ID, "ID is assigned before the first attempt")
	}
}

func TestDecisionAuditWriter_RetryThenSuccess(t *testing.T) {
	fs := &fakeDecisionAuditStore{failFirst: 2, failErr: context.DeadlineExceeded}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
	m := &fakeDecisionAuditMetrics{}
	e.SetMetrics(m)

	e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
	e.Close(context.Background())

	written, attempts := fs.snapshot()
	assert.Equal(t, []string{"d1"}, written)
	assert.Equal(t, 3, attempts)
	assert.Zero(t, e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
	assert.Equal(t, 3, m.writes, "write latency recorded per attempt")
	assert.Equal(t, 2, m.failures)
}

func TestDecisionAuditWriter_RetryExhaustedAndPermanentErrorsCounted(t *testing.T) {
	t.Run("transient error exhausts retries", func(t *testing.T) {
		fs := &fakeDecisionAuditStore{failFirst: 100, failErr: errors.New("database is locked")}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		e.Close(context.Background())
		_, attempts := fs.snapshot()
		assert.Equal(t, 4, attempts)
		assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
	})
	t.Run("permanent error is not retried", func(t *testing.T) {
		fs := &fakeDecisionAuditStore{failFirst: 100, failErr: store.ErrInvalidInput}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("allow", "a1"))
		e.Close(context.Background())
		_, attempts := fs.snapshot()
		assert.Equal(t, 1, attempts)
		assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropWriteFailed, "allow"))
	})
	t.Run("already exists on a retry counts as written", func(t *testing.T) {
		fs := &alreadyExistsOnRetryStore{}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		e.Close(context.Background())
		assert.Equal(t, 2, fs.attempts)
		assert.Zero(t, e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
	})
}

// alreadyExistsOnRetryStore simulates an attempt that committed but timed
// out: the retry then reports a duplicate.
type alreadyExistsOnRetryStore struct {
	store.Store
	attempts int
}

func (s *alreadyExistsOnRetryStore) CreateDecisionAudit(context.Context, *store.DecisionAuditRecord) error {
	s.attempts++
	if s.attempts == 1 {
		return context.DeadlineExceeded
	}
	return store.ErrAlreadyExists
}

func TestDecisionAuditWriter_QueueFullShedsAllowsBeforeDenies(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	cfg := testDecisionAuditConfig()
	cfg.queueSize = 3
	e := newTestDecisionAuditEmitter(t, fs, cfg)
	m := &fakeDecisionAuditMetrics{}
	e.SetMetrics(m)
	ctx := context.Background()

	// Occupy the single worker so the queue fills.
	e.EmitDecisionAudit(ctx, auditRec("allow", "a0"))
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the store")
	}

	e.EmitDecisionAudit(ctx, auditRec("allow", "a1"))
	e.EmitDecisionAudit(ctx, auditRec("allow", "a2"))
	e.EmitDecisionAudit(ctx, auditRec("deny", "d1"))  // queue now full
	e.EmitDecisionAudit(ctx, auditRec("deny", "d2"))  // sheds a1
	e.EmitDecisionAudit(ctx, auditRec("deny", "d3"))  // sheds a2
	e.EmitDecisionAudit(ctx, auditRec("deny", "d4"))  // no allow left: dropped
	e.EmitDecisionAudit(ctx, auditRec("allow", "a3")) // full: dropped

	close(fs.gate)
	e.Close(ctx)

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"a0", "d1", "d2", "d3"}, written)
	assert.Equal(t, int64(3), e.droppedCount(DecisionAuditDropQueueFull, "allow"))
	assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropQueueFull, "deny"))
	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Equal(t, 3, m.drops["queue_full/allow"])
	assert.Equal(t, 1, m.drops["queue_full/deny"])
	assert.Equal(t, int64(3), m.maxDepth)
	assert.Equal(t, int64(0), m.lastDepth)
}

func TestDecisionAuditWriter_DrainOnShutdownFlushesPending(t *testing.T) {
	// Each write takes longer than the old per-record 1s timeout would
	// allow in aggregate, but the drain deadline covers them all.
	fs := &fakeDecisionAuditStore{delay: 20 * time.Millisecond}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
	for _, tag := range []string{"d1", "d2", "d3", "d4", "d5"} {
		e.EmitDecisionAudit(context.Background(), auditRec("deny", tag))
	}

	start := time.Now()
	e.Close(context.Background())
	assert.Less(t, time.Since(start), 5*time.Second)

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"d1", "d2", "d3", "d4", "d5"}, written)

	e.EmitDecisionAudit(context.Background(), auditRec("deny", "late"))
	assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropShutdown, "deny"))
}

func TestDecisionAuditWriter_DrainIsBoundedByDeadline(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{})} // never opens
	cfg := testDecisionAuditConfig()
	cfg.drainTimeout = 50 * time.Millisecond
	e := newTestDecisionAuditEmitter(t, fs, cfg)
	for _, tag := range []string{"d1", "d2", "a1"} {
		result := "deny"
		if tag == "a1" {
			result = "allow"
		}
		e.EmitDecisionAudit(context.Background(), auditRec(result, tag))
	}

	start := time.Now()
	e.Close(context.Background())
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, int64(2), e.droppedCount(DecisionAuditDropShutdown, "deny"))
	assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropShutdown, "allow"))
}

func TestDecisionAuditWriter_DecisionPathNeverBlocksOnSlowStore(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{})} // store hangs
	cfg := testDecisionAuditConfig()
	cfg.queueSize = 64
	cfg.drainTimeout = 50 * time.Millisecond
	e := newTestDecisionAuditEmitter(t, fs, cfg)

	authz := NewAuthzService(fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	authz.SetDecisionAuditEmitter(e)

	const n = 5000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			authz.emitDecisionAudit(context.Background(),
				AuthzRequest{Resource: Resource{Type: "project", ID: "p"}, Action: ActionRead},
				Decision{Allowed: i%2 == 0, Reason: "test"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("decision path blocked on a slow audit store")
	}
	dropped := e.droppedCount(DecisionAuditDropQueueFull, "allow") +
		e.droppedCount(DecisionAuditDropQueueFull, "deny")
	require.Greater(t, dropped, int64(0))
	// Allows were shed first, so the queue holds only denies.
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Zero(t, e.allowCount)
}
