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

// Package lockloop provides a reusable advisory-lock lifecycle loop for
// standalone integration services. It acquires a session-scoped Postgres
// advisory lock on a dedicated connection, periodically verifies the lock
// is still held, detects lock loss, and applies a takeover delay before
// promoting a standby to primary.
//
// Extracted from scion-discord's GatewayLockLoop (Phase 5C) for reuse
// across Discord, Telegram, and future integrations (Phase 5D+).
package lockloop

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AdvisoryLockHandle represents a held advisory lock on a dedicated database
// connection. The lock stays alive as long as the underlying connection lives.
type AdvisoryLockHandle struct {
	release func() error
	verify  func(ctx context.Context) error
}

// Release unlocks the advisory lock and returns the dedicated connection to the pool.
func (h *AdvisoryLockHandle) Release() error {
	if h == nil {
		return nil
	}
	return h.release()
}

// Verify checks that the dedicated lock connection is still alive (cheap ping).
func (h *AdvisoryLockHandle) Verify(ctx context.Context) error {
	if h == nil {
		return nil
	}
	return h.verify(ctx)
}

// NewAdvisoryLockHandle constructs an AdvisoryLockHandle.
func NewAdvisoryLockHandle(release func() error, verify func(ctx context.Context) error) *AdvisoryLockHandle {
	return &AdvisoryLockHandle{release: release, verify: verify}
}

// AdvisoryLocker is the subset of a store needed by LockLoop.
type AdvisoryLocker interface {
	TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, handle *AdvisoryLockHandle, err error)
}

// LockLoop manages the advisory lock lifecycle for a standalone integration.
// It acquires a session-scoped Postgres advisory lock on a dedicated connection,
// periodically verifies the lock is still held, detects lock loss, and applies
// a takeover delay before promoting a standby to primary.
type LockLoop struct {
	locker  AdvisoryLocker
	lockKey int64
	log     *slog.Logger

	active atomic.Bool
	mu     sync.Mutex
	handle *AdvisoryLockHandle

	consecutiveAcquirable int

	LockInterval  time.Duration
	TakeoverTicks int

	// OnAcquired is called when the lock is acquired after the takeover delay.
	OnAcquired func() error

	// OnLost is called when the lock is lost during active operation.
	OnLost func()
}

// New creates a lock loop. Configure OnAcquired and OnLost before calling Run.
func New(locker AdvisoryLocker, lockKey int64, log *slog.Logger) *LockLoop {
	return &LockLoop{
		locker:        locker,
		lockKey:       lockKey,
		log:           log,
		LockInterval:  30 * time.Second,
		TakeoverTicks: 2,
	}
}

// Run starts the lock loop. It blocks until ctx is cancelled.
func (g *LockLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(g.LockInterval)
	defer ticker.Stop()

	g.Tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.Tick(ctx)
		}
	}
}

// Tick performs a single lock-loop iteration. Exported for testing.
func (g *LockLoop) Tick(ctx context.Context) {
	if g.active.Load() {
		g.verifyLock(ctx)
		return
	}
	g.tryAcquire(ctx)
}

func (g *LockLoop) verifyLock(ctx context.Context) {
	g.mu.Lock()
	h := g.handle
	g.mu.Unlock()

	if h == nil {
		return
	}

	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := h.Verify(verifyCtx); err != nil {
		if ctx.Err() != nil {
			return
		}
		g.log.Error("Advisory lock lost (connection dead)", "error", err)
		g.loselock()
	}
}

func (g *LockLoop) tryAcquire(ctx context.Context) {
	acquired, handle, err := g.locker.TryAdvisoryLock(ctx, g.lockKey)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		g.log.Error("Advisory lock error", "error", err)
		g.consecutiveAcquirable = 0
		return
	}

	if !acquired {
		g.log.Debug("Standby — another instance holds the lock")
		g.consecutiveAcquirable = 0
		return
	}

	g.consecutiveAcquirable++

	if g.consecutiveAcquirable < g.TakeoverTicks {
		handle.Release()
		g.log.Info("Lock acquirable, waiting for takeover delay",
			"ticks", g.consecutiveAcquirable, "required", g.TakeoverTicks)
		return
	}

	if g.OnAcquired != nil {
		if err := g.OnAcquired(); err != nil {
			g.log.Error("Failed to activate after lock acquisition", "error", err)
			handle.Release()
			g.consecutiveAcquirable = 0
			return
		}
	}

	g.mu.Lock()
	g.handle = handle
	g.mu.Unlock()
	g.active.Store(true)
	g.log.Info("Advisory lock acquired, activated")
}

func (g *LockLoop) loselock() {
	if g.OnLost != nil {
		g.OnLost()
	}

	g.mu.Lock()
	h := g.handle
	g.handle = nil
	g.mu.Unlock()

	if h != nil {
		if err := h.Release(); err != nil {
			g.log.Warn("Error releasing advisory lock after loss", "error", err)
		}
	}

	g.active.Store(false)
	g.consecutiveAcquirable = 0
}

// Active reports whether the lock is currently held.
func (g *LockLoop) Active() bool {
	return g.active.Load()
}

// ReleaseHandle releases the advisory lock handle without calling OnLost.
// Used for orderly shutdown after the caller has already drained connections.
func (g *LockLoop) ReleaseHandle() {
	g.mu.Lock()
	h := g.handle
	g.handle = nil
	g.mu.Unlock()

	g.active.Store(false)

	if h != nil {
		if err := h.Release(); err != nil {
			g.log.Warn("Error releasing advisory lock on shutdown", "error", err)
		}
	}
}
