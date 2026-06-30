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
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// EventHandler processes a one-shot scheduled event of a specific type.
type EventHandler func(ctx context.Context, evt store.ScheduledEvent) error

// Scheduler manages recurring and one-shot timers within the Hub server.
// A single root ticker fires every 1 minute and drives all registered
// recurring handlers based on their configured interval.
//
// One-shot timers are persisted in the database and scheduled in memory
// via time.AfterFunc. On startup, expired timers fire immediately; future
// timers are scheduled for their fire_at time.
//
// All recurring handlers must be registered via RegisterRecurring before
// Start is called. RegisterRecurring is not safe for concurrent use.
type Scheduler struct {
	// Store for persisting one-shot events
	store store.Store

	// Root ticker interval
	tickInterval time.Duration

	// MaxJitter is the upper bound on random delay added before each recurring
	// handler invocation. Spreading handlers over this window prevents the
	// "thundering herd" where all background tasks hit the DB simultaneously.
	// Defaults to 30 s in production; tests can set it to 0 for determinism.
	MaxJitter time.Duration

	// Recurring handlers
	recurring []RecurringHandler

	// Event type handlers for one-shot events
	eventHandlers map[string]EventHandler

	// Tick counter (monotonically increasing)
	tickCount uint64

	// One-shot timers (in-memory)
	mu     sync.Mutex
	timers map[string]*scheduledTimer

	// Logger
	log *slog.Logger

	// Lifecycle
	ctx      context.Context // long-lived context from Start(); used for timer callbacks
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// RecurringHandler defines a periodic task driven by the root ticker.
type RecurringHandler struct {
	Name     string                    // Human-readable name for logging
	Interval int                       // Run every N ticks (must be >= 1)
	Fn       func(ctx context.Context) // The work to perform
}

// scheduledTimer wraps a time.Timer with metadata for one-shot events.
type scheduledTimer struct {
	ID     string
	Timer  *time.Timer
	FireAt time.Time
	Cancel context.CancelFunc
}

// NewScheduler creates a new Scheduler with a 1-minute root ticker interval
// and a 30-second max jitter to desynchronize recurring handlers.
func NewScheduler(st store.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store:         st,
		tickInterval:  1 * time.Minute,
		MaxJitter:     maxJitter,
		timers:        make(map[string]*scheduledTimer),
		eventHandlers: make(map[string]EventHandler),
		log:           log,
		stopCh:        make(chan struct{}),
	}
}

// RegisterEventHandler registers a handler for a specific event type.
// Must be called before Start(). Not safe for concurrent use.
func (s *Scheduler) RegisterEventHandler(eventType string, handler EventHandler) {
	s.eventHandlers[eventType] = handler
}

// GetEventHandler returns the handler for the given event type, if registered.
func (s *Scheduler) GetEventHandler(eventType string) (EventHandler, bool) {
	handler, ok := s.eventHandlers[eventType]
	return handler, ok
}

// RegisterRecurring registers a recurring handler that runs every intervalMinutes
// minutes. All handlers must be registered before Start is called.
//
// Tick-Zero Behavior: All recurring handlers run immediately on startup (tick 0)
// because 0 % N == 0 for any interval N. This is intentional.
func (s *Scheduler) RegisterRecurring(name string, intervalMinutes int, fn func(ctx context.Context)) {
	if intervalMinutes < 1 {
		intervalMinutes = 1
	}
	s.recurring = append(s.recurring, RecurringHandler{
		Name:     name,
		Interval: intervalMinutes,
		Fn:       fn,
	})
}

// RegisterRecurringSingleton registers a recurring handler that runs on AT MOST
// ONE replica per tick, guarded by a cluster-wide advisory lock keyed by key.
//
// This is the singleton/leader primitive (P3-5) for cluster-wide-once work such
// as the stale-agent sweep, stalled detection, purge, schedule evaluation, and
// the GitHub App health check. In a single-replica or SQLite deployment the
// lock is a no-op that always succeeds, so the handler runs exactly as before.
//
// If the store does not implement store.AdvisoryLocker, the handler runs
// unguarded (correct for a single replica).
func (s *Scheduler) RegisterRecurringSingleton(name string, intervalMinutes int, key store.AdvisoryLockKey, fn func(ctx context.Context)) {
	s.RegisterRecurring(name, intervalMinutes, s.singletonGuard(name, key, fn))
}

// singletonGuard wraps fn so it only runs while this replica holds the named
// advisory lock. The lock is released as soon as fn returns, so the next tick on
// any replica is free to win it.
//
// A store that does not implement AdvisoryLocker falls open to running fn
// unguarded — correct for a single-replica / SQLite deployment where there is no
// other replica to collide with.
//
// A lock-acquisition error (e.g. a connection timeout to Postgres) does NOT fall
// open: in a multi-replica deployment running unguarded would let two replicas
// execute the same singleton work concurrently. Since we cannot prove we are
// alone when the lock query itself failed, we SKIP this tick and let the next one
// retry. Missing one tick of idempotent maintenance work is safer than running it
// in duplicate.
func (s *Scheduler) singletonGuard(name string, key store.AdvisoryLockKey, fn func(ctx context.Context)) func(ctx context.Context) {
	return func(ctx context.Context) {
		locker, ok := s.store.(store.AdvisoryLocker)
		if !ok {
			fn(ctx)
			return
		}
		acquired, release, err := locker.TryAdvisoryLock(ctx, key)
		if err != nil {
			s.log.Warn("Scheduler: advisory lock acquisition failed; skipping tick to avoid running unguarded across replicas",
				"name", name, "error", err)
			return
		}
		if !acquired {
			s.log.Debug("Scheduler: singleton handler held by another replica, skipping",
				"name", name)
			return
		}
		defer func() {
			if rerr := release(); rerr != nil {
				s.log.Warn("Scheduler: advisory unlock failed", "name", name, "error", rerr)
			}
		}()
		fn(ctx)
	}
}

// Start begins the root ticker loop and runs eligible handlers immediately
// on startup (tick 0). The provided context is used as the parent for handler
// invocations. Before starting the ticker, persisted one-shot timers are
// loaded from the database.
func (s *Scheduler) Start(ctx context.Context) {
	// Store the long-lived server context for use by timer callbacks.
	// This prevents timers scheduled via HTTP requests from inheriting
	// the short-lived request context.
	s.ctx = ctx

	// Load and schedule persisted one-shot timers
	s.loadPersistedTimers(ctx)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()

		// Run eligible handlers immediately on startup (tick 0).
		// All handlers fire at tick 0 because 0 % N == 0 for any interval.
		s.runRecurringHandlers(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.tickCount++
				s.runRecurringHandlers(ctx)
			}
		}
	}()
}

// Stop signals the scheduler to stop, cancels all pending one-shot timers,
// and waits for the root ticker goroutine to exit. In-flight handler
// goroutines are not tracked; they will be cancelled via the parent context
// when the server shuts down. It is safe to call multiple times.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)

		// Cancel all one-shot timers
		s.mu.Lock()
		for _, st := range s.timers {
			st.Timer.Stop()
			if st.Cancel != nil {
				st.Cancel()
			}
		}
		s.timers = make(map[string]*scheduledTimer)
		s.mu.Unlock()
	})

	s.wg.Wait()
}

// maxJitter is the upper bound on random delay added before each recurring
// handler invocation. Spreading handlers over a 30-second window prevents the
// "thundering herd" where all background tasks hit the DB simultaneously.
const maxJitter = 30 * time.Second

// runRecurringHandlers invokes all handlers whose interval divides the current
// tick count. Each handler runs in its own goroutine with a timeout context.
// A random jitter (0–30 s) is added before each invocation so background tasks
// desynchronize and avoid saturating the DB connection pool.
func (s *Scheduler) runRecurringHandlers(ctx context.Context) {
	for _, h := range s.recurring {
		if s.tickCount%uint64(h.Interval) == 0 {
			handler := h // capture loop variable
			go func() {
				// Stagger start: sleep a random 0–MaxJitter so tasks that fire on
				// the same tick don't all compete for DB connections at once.
				jitter := time.Duration(0)
				if s.MaxJitter > 0 {
					jitter = time.Duration(rand.Int63n(int64(s.MaxJitter)))
				}
				select {
				case <-time.After(jitter):
				case <-ctx.Done():
					return
				}

				handlerCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
				defer cancel()

				start := time.Now()
				s.log.Debug("Scheduler: running recurring handler", "name", handler.Name, "tick", s.tickCount)

				func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Error("Scheduler: recurring handler panicked",
								"name", handler.Name, "panic", r)
						}
					}()
					handler.Fn(handlerCtx)
				}()

				s.log.Debug("Scheduler: recurring handler completed",
					"name", handler.Name, "duration", time.Since(start))
			}()
		}
	}
}

// =============================================================================
// One-Shot Timer Methods
// =============================================================================

// loadPersistedTimers loads all pending events from the database on startup.
// Events whose fire_at is in the past are executed immediately with status
// "expired". Future events are scheduled in memory.
func (s *Scheduler) loadPersistedTimers(ctx context.Context) {
	if s.store == nil {
		return
	}

	events, err := s.store.ListPendingScheduledEvents(ctx)
	if err != nil {
		s.log.Error("Scheduler: failed to load pending events", "error", err)
		return
	}

	now := time.Now()
	var expiredCount, scheduledCount int

	for _, evt := range events {
		if evt.FireAt.Before(now) || evt.FireAt.Equal(now) {
			// Expired while Hub was down — execute immediately
			expiredCount++
			staleness := now.Sub(evt.FireAt)
			s.log.Warn("Scheduler: recovering expired event from downtime",
				"eventID", evt.ID,
				"type", evt.EventType,
				"scheduledFor", evt.FireAt.Format(time.RFC3339),
				"staleness", staleness.Truncate(time.Second).String())
			go s.fireEvent(ctx, evt, true)
		} else {
			// Schedule for the future
			scheduledCount++
			s.scheduleTimer(ctx, evt)
		}
	}

	if expiredCount > 0 || scheduledCount > 0 {
		s.log.Info("Scheduler: loaded persisted events",
			"expired", expiredCount, "scheduled", scheduledCount)
	}
}

// scheduleTimer creates a time.AfterFunc timer for the given event and tracks
// it in the in-memory timer map.
func (s *Scheduler) scheduleTimer(ctx context.Context, evt store.ScheduledEvent) {
	delay := time.Until(evt.FireAt)
	if delay < 0 {
		delay = 0
	}

	timerCtx, cancel := context.WithCancel(ctx)

	timer := time.AfterFunc(delay, func() {
		defer cancel()
		s.fireEvent(timerCtx, evt, false)
		s.mu.Lock()
		delete(s.timers, evt.ID)
		s.mu.Unlock()
	})

	s.mu.Lock()
	s.timers[evt.ID] = &scheduledTimer{
		ID:     evt.ID,
		Timer:  timer,
		FireAt: evt.FireAt,
		Cancel: cancel,
	}
	s.mu.Unlock()
}

// fireEvent executes the event handler with panic recovery and updates the
// database status. wasExpired indicates the timer was past its fire_at when
// loaded on startup.
func (s *Scheduler) fireEvent(ctx context.Context, evt store.ScheduledEvent, wasExpired bool) {
	handlerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	status := store.ScheduledEventFired
	if wasExpired {
		status = store.ScheduledEventExpired
	}

	// Multi-replica dedup (P3-5): several replicas may each recover the same
	// pending event from the database on startup and arm a timer for it. Claim
	// the event atomically (pending -> status) before running its side effect so
	// exactly one replica delivers it. If the store supports claiming and we
	// lose the race (already claimed/cancelled), skip silently. Backends without
	// the capability fall through to the legacy run-then-mark behavior, which is
	// correct for a single replica.
	if claimer, ok := s.store.(store.ScheduledEventClaimer); ok {
		claimed, err := claimer.ClaimScheduledEvent(ctx, evt.ID, status)
		if err != nil {
			s.log.Warn("Scheduler: failed to claim scheduled event; skipping to avoid duplicate",
				"eventID", evt.ID, "type", evt.EventType, "error", err)
			return
		}
		if !claimed {
			s.log.Debug("Scheduler: scheduled event already claimed by another replica, skipping",
				"eventID", evt.ID, "type", evt.EventType)
			return
		}
	}

	var errMsg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				errMsg = fmt.Sprintf("handler panicked: %v", r)
				s.log.Error("Scheduler: event handler panicked",
					"eventID", evt.ID, "type", evt.EventType, "panic", r)
			}
		}()

		if err := s.executeEvent(handlerCtx, evt); err != nil {
			errMsg = err.Error()
			s.log.Warn("Scheduler: event handler failed",
				"eventID", evt.ID, "type", evt.EventType, "error", err)
		} else {
			s.log.Info("Scheduler: event fired",
				"eventID", evt.ID, "type", evt.EventType, "wasExpired", wasExpired)
		}
	}()

	now := time.Now()
	if s.store != nil {
		_ = s.store.UpdateScheduledEventStatus(ctx, evt.ID, status, &now, errMsg)
	}
}

// executeEvent dispatches the event to the appropriate handler based on its
// EventType. Unknown event types return an error.
func (s *Scheduler) executeEvent(ctx context.Context, evt store.ScheduledEvent) error {
	handler, ok := s.eventHandlers[evt.EventType]
	if !ok {
		return fmt.Errorf("unknown event type: %s", evt.EventType)
	}
	return handler(ctx, evt)
}

// ScheduleEvent creates a new one-shot scheduled event. The event is persisted
// to the database first, then scheduled in memory.
func (s *Scheduler) ScheduleEvent(ctx context.Context, evt store.ScheduledEvent) error {
	if s.store == nil {
		return fmt.Errorf("scheduler has no store configured")
	}

	// Persist to database first (use caller's context for the DB write)
	if err := s.store.CreateScheduledEvent(ctx, &evt); err != nil {
		return err
	}

	// Schedule in memory using the long-lived server context so the timer
	// callback is not cancelled when the originating HTTP request completes.
	timerCtx := s.ctx
	if timerCtx == nil {
		timerCtx = context.Background()
	}
	s.scheduleTimer(timerCtx, evt)

	s.log.Info("Scheduler: event scheduled",
		"eventID", evt.ID, "type", evt.EventType, "fireAt", evt.FireAt)
	return nil
}

// SchedulerStatus holds a point-in-time snapshot of the scheduler's state.
type SchedulerStatus struct {
	TickCount     uint64                 `json:"tickCount"`
	TickInterval  string                 `json:"tickInterval"`
	Recurring     []RecurringHandlerInfo `json:"recurringHandlers"`
	EventHandlers []string               `json:"eventHandlers"`
	ActiveTimers  int                    `json:"activeTimers"`
}

// RecurringHandlerInfo is the public view of a registered recurring handler.
type RecurringHandlerInfo struct {
	Name     string `json:"name"`
	Interval int    `json:"intervalMinutes"`
}

// Status returns a snapshot of the scheduler's current state.
func (s *Scheduler) Status() SchedulerStatus {
	recurring := make([]RecurringHandlerInfo, len(s.recurring))
	for i, h := range s.recurring {
		recurring[i] = RecurringHandlerInfo{
			Name:     h.Name,
			Interval: h.Interval,
		}
	}

	eventHandlers := make([]string, 0, len(s.eventHandlers))
	for t := range s.eventHandlers {
		eventHandlers = append(eventHandlers, t)
	}

	s.mu.Lock()
	activeTimers := len(s.timers)
	s.mu.Unlock()

	return SchedulerStatus{
		TickCount:     s.tickCount,
		TickInterval:  s.tickInterval.String(),
		Recurring:     recurring,
		EventHandlers: eventHandlers,
		ActiveTimers:  activeTimers,
	}
}

// CancelEvent cancels a pending scheduled event. The in-memory timer is
// stopped and the database record is marked as cancelled.
func (s *Scheduler) CancelEvent(ctx context.Context, id string) error {
	s.mu.Lock()
	if st, ok := s.timers[id]; ok {
		st.Timer.Stop()
		if st.Cancel != nil {
			st.Cancel()
		}
		delete(s.timers, id)
	}
	s.mu.Unlock()

	if s.store == nil {
		return fmt.Errorf("scheduler has no store configured")
	}

	return s.store.CancelScheduledEvent(ctx, id)
}
