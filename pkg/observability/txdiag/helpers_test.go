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

package txdiag

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// FakeClock is a goroutine-safe manual clock for tests. Exported (in a test
// file) so the external test package can use it too.
type FakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

// NewFakeClock returns a clock at a fixed instant.
func NewFakeClock() *FakeClock {
	return &FakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now returns the current fake time, then advances it by the step (if set).
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// SetStep makes every Now call advance the clock by d (0 stops it).
func (c *FakeClock) SetStep(d time.Duration) {
	c.mu.Lock()
	c.step = d
	c.mu.Unlock()
}

// LogLine is one captured log record.
type LogLine struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

// Str returns attribute k as a string.
func (l LogLine) Str(k string) string { return l.Attrs[k].String() }

// Int returns attribute k as an int64.
func (l LogLine) Int(k string) int64 { return l.Attrs[k].Int64() }

// CaptureHandler is a slog.Handler that records every line. When Block is
// non-nil, Handle waits on it first (a stuck log pipe).
type CaptureHandler struct {
	mu    sync.Mutex
	lines []LogLine
	Block chan struct{}
	// Entered receives once per Handle call, before blocking.
	Entered chan struct{}
}

func (h *CaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *CaptureHandler) Handle(_ context.Context, r slog.Record) error {
	if h.Entered != nil {
		select {
		case h.Entered <- struct{}{}:
		default:
		}
	}
	if h.Block != nil {
		<-h.Block
	}
	l := LogLine{Level: r.Level, Msg: r.Message, Attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		l.Attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, l)
	h.mu.Unlock()
	return nil
}

func (h *CaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *CaptureHandler) WithGroup(string) slog.Handler      { return h }

// Lines returns the captured lines with message msg (all when msg is "").
func (h *CaptureHandler) Lines(msg string) []LogLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []LogLine
	for _, l := range h.lines {
		if msg == "" || l.Msg == msg {
			out = append(out, l)
		}
	}
	return out
}

func newLogger(h slog.Handler) *slog.Logger { return slog.New(h) }

const (
	slogLevelWarn = slog.LevelWarn
	slogLevelInfo = slog.LevelInfo
)

// slowHandler delays each line by d before recording it.
type slowHandler struct {
	*CaptureHandler
	d time.Duration
}

func (h *slowHandler) Handle(ctx context.Context, r slog.Record) error {
	time.Sleep(h.d)
	return h.CaptureHandler.Handle(ctx, r)
}

// Messages logged by the Monitor, for tests.
const (
	MsgHold     = msgHold
	MsgLockWait = msgLockWait
	MsgOpCap    = msgOpCap
)

// FakeRecorder counts Recorder calls.
type FakeRecorder struct {
	mu      sync.Mutex
	Holds   []RecordedHold
	Commits []RecordedWait // MS, Op, Outcome of each RecordTxCommit
	Waits   []RecordedWait
	Dropped int64
}

// RecordedHold is one RecordTxHold call.
type RecordedHold struct {
	MS                float64
	Op, Kind, Outcome string
}

// RecordedWait is one RecordLockWait call.
type RecordedWait struct {
	MS          float64
	Op, Outcome string
}

func (r *FakeRecorder) RecordTxHold(_ context.Context, ms float64, op, kind, outcome string) {
	r.mu.Lock()
	r.Holds = append(r.Holds, RecordedHold{ms, op, kind, outcome})
	r.mu.Unlock()
}

func (r *FakeRecorder) RecordTxCommit(_ context.Context, ms float64, op, outcome string) {
	r.mu.Lock()
	r.Commits = append(r.Commits, RecordedWait{ms, op, outcome})
	r.mu.Unlock()
}

// CommitsSnapshot returns a copy of the RecordTxCommit calls.
func (r *FakeRecorder) CommitsSnapshot() []RecordedWait {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RecordedWait(nil), r.Commits...)
}

func (r *FakeRecorder) RecordLockWait(_ context.Context, ms float64, op, outcome string) {
	r.mu.Lock()
	r.Waits = append(r.Waits, RecordedWait{ms, op, outcome})
	r.mu.Unlock()
}

func (r *FakeRecorder) IncDiagDropped(_ context.Context, n int64) {
	r.mu.Lock()
	r.Dropped += n
	r.mu.Unlock()
}

// Snapshot returns copies of the recorded calls.
func (r *FakeRecorder) Snapshot() ([]RecordedHold, []RecordedWait, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RecordedHold(nil), r.Holds...), append([]RecordedWait(nil), r.Waits...), r.Dropped
}

// ManualCtx is a context whose Done channel the test closes by hand, with a
// chosen Err, so a waiter's deadline or cancellation is deterministic.
type ManualCtx struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

// NewManualCtx returns an open ManualCtx.
func NewManualCtx() *ManualCtx { return &ManualCtx{done: make(chan struct{})} }

// Fire closes Done; Err then returns err.
func (c *ManualCtx) Fire(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *ManualCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *ManualCtx) Done() <-chan struct{}       { return c.done }
func (c *ManualCtx) Value(any) any               { return nil }
func (c *ManualCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
