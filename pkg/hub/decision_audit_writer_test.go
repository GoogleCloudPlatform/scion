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
	"strings"
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
	mu       sync.Mutex
	drops    map[string]int
	outcomes []DecisionAuditWriteOutcome
}

func (m *fakeDecisionAuditMetrics) RecordDecisionAuditDrop(reason DecisionAuditDropReason, decision string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drops == nil {
		m.drops = map[string]int{}
	}
	m.drops[string(reason)+"/"+decision]++
}

func (m *fakeDecisionAuditMetrics) RecordDecisionAuditWrite(_ time.Duration, outcome DecisionAuditWriteOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, outcome)
}

func (m *fakeDecisionAuditMetrics) snapshot() (drops map[string]int, outcomes []DecisionAuditWriteOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	drops = map[string]int{}
	for k, v := range m.drops {
		drops[k] = v
	}
	return drops, append([]DecisionAuditWriteOutcome(nil), m.outcomes...)
}

// recordingHandler is a slog.Handler that keeps every record's message.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.msgs = append(h.msgs, msg)
	return nil
}

func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
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
		abortGrace:     time.Second,
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

// waitFor polls cond until it holds or 5s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitEntered waits until a worker has reached the fake store.
func waitEntered(t *testing.T, fs *fakeDecisionAuditStore) {
	t.Helper()
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the store")
	}
}

func TestDecisionAuditWriter_EnqueueAndWrite(t *testing.T) {
	fs := &fakeDecisionAuditStore{}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())

	e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
	e.EmitDecisionAudit(context.Background(), auditRec("allow", "a1"))
	e.Close(context.Background())

	written, _ := fs.snapshot()
	assert.ElementsMatch(t, []string{"d1", "a1"}, written)
	fs.mu.Lock()
	defer fs.mu.Unlock()
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
	_, outcomes := m.snapshot()
	assert.Equal(t, []DecisionAuditWriteOutcome{
		DecisionAuditWriteError, DecisionAuditWriteError, DecisionAuditWriteOK,
	}, outcomes, "write duration recorded per attempt")
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
	t.Run("already exists on the first attempt is a failure", func(t *testing.T) {
		fs := &fakeDecisionAuditStore{failFirst: 100, failErr: store.ErrAlreadyExists}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		e.Close(context.Background())
		_, attempts := fs.snapshot()
		assert.Equal(t, 1, attempts)
		assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
	})
	t.Run("already exists on a retry counts as written", func(t *testing.T) {
		fs := &alreadyExistsOnRetryStore{}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		m := &fakeDecisionAuditMetrics{}
		e.SetMetrics(m)
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		e.Close(context.Background())
		assert.Equal(t, 2, fs.attemptCount())
		assert.Zero(t, e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
		_, outcomes := m.snapshot()
		assert.Equal(t, []DecisionAuditWriteOutcome{
			DecisionAuditWriteError, DecisionAuditWriteDuplicate,
		}, outcomes, "the duplicate attempt is not recorded as an error")
	})
}

// alreadyExistsOnRetryStore simulates an attempt that committed but timed
// out: the retry then reports a duplicate.
type alreadyExistsOnRetryStore struct {
	store.Store
	mu       sync.Mutex
	attempts int
}

func (s *alreadyExistsOnRetryStore) CreateDecisionAudit(context.Context, *store.DecisionAuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.attempts == 1 {
		return context.DeadlineExceeded
	}
	return store.ErrAlreadyExists
}

func (s *alreadyExistsOnRetryStore) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
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
	waitEntered(t, fs)

	e.EmitDecisionAudit(ctx, auditRec("allow", "a1"))
	e.EmitDecisionAudit(ctx, auditRec("allow", "a2"))
	e.EmitDecisionAudit(ctx, auditRec("deny", "d1"))  // queue now full
	e.EmitDecisionAudit(ctx, auditRec("deny", "d2"))  // sheds a1
	e.EmitDecisionAudit(ctx, auditRec("deny", "d3"))  // sheds a2
	e.EmitDecisionAudit(ctx, auditRec("deny", "d4"))  // no allow left: dropped
	e.EmitDecisionAudit(ctx, auditRec("allow", "a3")) // full: dropped
	assert.Equal(t, 3, e.QueueDepth())

	close(fs.gate)
	e.Close(ctx)

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"a0", "d1", "d2", "d3"}, written)
	assert.Equal(t, int64(3), e.droppedCount(DecisionAuditDropQueueFull, "allow"))
	assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropQueueFull, "deny"))
	drops, _ := m.snapshot()
	assert.Equal(t, 3, drops["queue_full/allow"])
	assert.Equal(t, 1, drops["queue_full/deny"])
	assert.Zero(t, e.QueueDepth())
}

func TestDecisionAuditWriter_DeniesWrittenBeforeAllows(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
	ctx := context.Background()

	e.EmitDecisionAudit(ctx, auditRec("allow", "a0"))
	waitEntered(t, fs)
	e.EmitDecisionAudit(ctx, auditRec("allow", "a1"))
	e.EmitDecisionAudit(ctx, auditRec("deny", "d1"))
	e.EmitDecisionAudit(ctx, auditRec("allow", "a2"))
	e.EmitDecisionAudit(ctx, &store.DecisionAuditRecord{Result: "", Reason: "u1"})

	close(fs.gate)
	e.Close(ctx)

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"a0", "d1", "u1", "a1", "a2"}, written,
		"queued deny and unknown records are written before queued allows, each in FIFO order")
}

// TestDecisionAuditWriter_QueueDepthIsCurrentWhenIdle checks that the depth
// the queue_depth gauge observes falls back to zero once the workers have
// drained the queue, without waiting for Close.
func TestDecisionAuditWriter_QueueDepthIsCurrentWhenIdle(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
	ctx := context.Background()

	assert.Zero(t, e.QueueDepth())
	e.EmitDecisionAudit(ctx, auditRec("deny", "d0"))
	waitEntered(t, fs)
	e.EmitDecisionAudit(ctx, auditRec("deny", "d1"))
	e.EmitDecisionAudit(ctx, auditRec("allow", "a1"))
	assert.Equal(t, 2, e.QueueDepth(), "the in-flight record is not counted")

	close(fs.gate)
	waitFor(t, "all records written", func() bool {
		written, _ := fs.snapshot()
		return len(written) == 3
	})
	assert.Zero(t, e.QueueDepth(), "an idle writer reports an empty queue before Close")
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

func TestDecisionAuditWriter_DefaultCloseBoundIsFiveSeconds(t *testing.T) {
	cfg := defaultDecisionAuditWriterConfig()
	assert.Equal(t, 5*time.Second, cfg.drainTimeout+cfg.abortGrace)
}

// stuckDecisionAuditStore ignores ctx: each write blocks until release is
// closed, then fails.
type stuckDecisionAuditStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func (s *stuckDecisionAuditStore) CreateDecisionAudit(context.Context, *store.DecisionAuditRecord) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return errors.New("connection reset")
}

func TestDecisionAuditWriter_CloseReturnsWhenStoreIgnoresContext(t *testing.T) {
	fs := &stuckDecisionAuditStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	cfg := testDecisionAuditConfig()
	cfg.drainTimeout = 20 * time.Millisecond
	cfg.abortGrace = 20 * time.Millisecond
	h := &recordingHandler{}
	e := newStoreDecisionAuditEmitter(fs, slog.New(h), cfg)

	e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the store")
	}

	start := time.Now()
	e.Close(context.Background())
	assert.Less(t, time.Since(start), 2*time.Second, "Close is bounded by drainTimeout + abortGrace")
	assert.Zero(t, e.droppedCount(DecisionAuditDropShutdown, "deny"), "the stuck record is not counted yet")

	// When the stuck call returns, its record is counted and logged.
	close(fs.release)
	waitFor(t, "late drop counted", func() bool {
		return e.droppedCount(DecisionAuditDropShutdown, "deny") == 1
	})
	waitFor(t, "late drop logged", func() bool {
		for _, m := range h.messages() {
			if strings.Contains(m, "shutdown/deny=1") {
				return true
			}
		}
		return false
	})
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
	assert.Zero(t, e.allows.len())
	assert.Equal(t, cfg.queueSize, e.denies.len())
}

// TestDecisionAuditWriter_DropsAreNotLoggedOnDecisionPath checks that a
// drop on the decision path only counts, and that the writer-owned loop
// logs it on the next tick without needing a later drop. The test drives
// the ticks itself, so no assertion races a real ticker.
func TestDecisionAuditWriter_DropsAreNotLoggedOnDecisionPath(t *testing.T) {
	fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	tick := make(chan time.Time) // unbuffered: a send returns once the loop has the tick
	cfg := testDecisionAuditConfig()
	cfg.queueSize = 1
	cfg.dropLogTick = tick
	h := &recordingHandler{}
	e := newStoreDecisionAuditEmitter(fs, slog.New(h), cfg)
	t.Cleanup(func() {
		close(fs.gate)
		e.Close(context.Background())
	})
	ctx := context.Background()

	e.EmitDecisionAudit(ctx, auditRec("deny", "d0"))
	waitEntered(t, fs)
	e.EmitDecisionAudit(ctx, auditRec("deny", "d1")) // fills the queue
	e.EmitDecisionAudit(ctx, auditRec("deny", "d2")) // first drop ever
	assert.Empty(t, h.messages(), "the first drop is not logged inline")

	tick <- time.Now()
	waitFor(t, "first summary", func() bool { return len(h.messages()) == 1 })
	assert.Contains(t, h.messages()[0], "queue_full/deny=1")

	// A short burst right after a summary is reported on the next tick,
	// with no further drops.
	e.EmitDecisionAudit(ctx, auditRec("deny", "d3"))
	e.EmitDecisionAudit(ctx, auditRec("allow", "a1"))
	assert.Len(t, h.messages(), 1)
	tick <- time.Now()
	waitFor(t, "second summary", func() bool { return len(h.messages()) == 2 })
	assert.Contains(t, h.messages()[1], "queue_full/allow=1 queue_full/deny=1")
	assert.Contains(t, h.messages()[1], "dropped=2")

	// A tick with no new drops logs nothing.
	tick <- time.Now()
	tick <- time.Now() // the loop has finished the previous tick
	assert.Len(t, h.messages(), 2)
}

// abortingDecisionAuditStore aborts the writer during each write, then
// returns err: the drain deadline passes right after the write failed.
type abortingDecisionAuditStore struct {
	store.Store
	e   *StoreDecisionAuditEmitter
	err error
}

func (s *abortingDecisionAuditStore) CreateDecisionAudit(context.Context, *store.DecisionAuditRecord) error {
	s.e.abort()
	return s.err
}

// TestDecisionAuditWriter_FailedWriteKeepsReasonWhenAbortFollows checks
// that a write that failed for good (a permanent error, or the last
// attempt) stays write_failed when the abort lands right after it.
func TestDecisionAuditWriter_FailedWriteKeepsReasonWhenAbortFollows(t *testing.T) {
	for name, err := range map[string]error{
		"permanent error":  store.ErrInvalidInput,
		"attempts used up": errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			fs := &abortingDecisionAuditStore{err: err}
			cfg := testDecisionAuditConfig()
			cfg.maxAttempts = 1
			e := newTestDecisionAuditEmitter(t, fs, cfg)
			fs.e = e

			e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
			waitFor(t, "drop counted", func() bool {
				return e.droppedCount(DecisionAuditDropWriteFailed, "deny")+
					e.droppedCount(DecisionAuditDropShutdown, "deny") == 1
			})
			assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropWriteFailed, "deny"))
			assert.Zero(t, e.droppedCount(DecisionAuditDropShutdown, "deny"))
		})
	}
}

func TestDecisionAuditWriter_CloseLifecycle(t *testing.T) {
	t.Run("close twice", func(t *testing.T) {
		fs := &fakeDecisionAuditStore{}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		e.Close(context.Background())
		e.Close(context.Background())
		written, _ := fs.snapshot()
		assert.Equal(t, []string{"d1"}, written)
	})
	t.Run("already cancelled ctx aborts at once", func(t *testing.T) {
		fs := &fakeDecisionAuditStore{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		e := newTestDecisionAuditEmitter(t, fs, testDecisionAuditConfig())
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d1"))
		waitEntered(t, fs)
		e.EmitDecisionAudit(context.Background(), auditRec("deny", "d2"))
		e.EmitDecisionAudit(context.Background(), auditRec("allow", "a1"))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		e.Close(ctx)
		assert.Less(t, time.Since(start), time.Second, "drain timeout is 5s; a cancelled ctx must not wait for it")
		assert.Equal(t, int64(2), e.droppedCount(DecisionAuditDropShutdown, "deny"))
		assert.Equal(t, int64(1), e.droppedCount(DecisionAuditDropShutdown, "allow"))
		written, _ := fs.snapshot()
		assert.Empty(t, written)
	})
	t.Run("concurrent emit and close account for every record once", func(t *testing.T) {
		for iter := 0; iter < 10; iter++ {
			fs := &fakeDecisionAuditStore{delay: 50 * time.Microsecond}
			cfg := testDecisionAuditConfig()
			cfg.queueSize = 32
			cfg.workers = 4
			cfg.drainTimeout = time.Duration(iter) * time.Millisecond
			cfg.abortGrace = time.Second
			e := newTestDecisionAuditEmitter(t, fs, cfg)

			const emitters, perEmitter = 8, 200
			var wg sync.WaitGroup
			for g := 0; g < emitters; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < perEmitter; i++ {
						result := "allow"
						if (g+i)%3 == 0 {
							result = "deny"
						}
						e.EmitDecisionAudit(context.Background(), auditRec(result, "r"))
					}
				}(g)
			}
			var closers sync.WaitGroup
			for c := 0; c < 2; c++ {
				closers.Add(1)
				go func() {
					defer closers.Done()
					time.Sleep(time.Millisecond)
					e.Close(context.Background())
				}()
			}
			wg.Wait()
			closers.Wait()

			written, _ := fs.snapshot()
			var dropped int64
			for _, reason := range []DecisionAuditDropReason{
				DecisionAuditDropQueueFull, DecisionAuditDropWriteFailed, DecisionAuditDropShutdown,
			} {
				dropped += e.droppedCount(reason, "allow") + e.droppedCount(reason, "deny")
			}
			assert.Equal(t, int64(emitters*perEmitter), int64(len(written))+dropped,
				"iteration %d: written + dropped == emitted", iter)
			assert.Zero(t, e.QueueDepth())
		}
	})
}
