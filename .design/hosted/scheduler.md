# Hub Scheduler: Timers and Recurring Events

## Status
**Design** | February 2026

## Problem

The Scion Hub currently has limited background scheduling capability — a single `startPurgeLoop` goroutine that runs every hour to remove expired soft-deleted agents. As the Hub's responsibilities grow, several features require time-based automation:

- **Agent heartbeat timeout detection**: Agents that stop reporting heartbeats should be marked as `undetermined` so operators and peer agents are aware of stale state.
- **Recurring maintenance tasks**: Broker health checks, orphaned resource cleanup, and other periodic operations.
- **One-shot scheduled actions**: Deadline-based events like "send a message to agent X in 30 minutes" or "mark agent Y as timed out at 14:00 UTC."

Today, adding each new scheduled task means adding another ad-hoc goroutine and ticker to `server.go`. This does not scale in terms of code organization, observability, or user extensibility.

### Goals

1. **Unified scheduling infrastructure** within the Hub server for both recurring and one-shot timers.
2. **1-minute granularity** for recurring timers — a root heartbeat ticker at 1-minute intervals drives all recurring work.
3. **Sub-minute precision** for one-shot scheduled events, specified as either an absolute datetime or a duration-from-now.
4. **In-memory timer management** using Go's `time.Timer` and `time.Ticker`, with persistence in the database for durability across restarts.
5. **Built-in recurring task: agent heartbeat timeout** — mark agents as `undetermined` when their last heartbeat exceeds a fixed threshold (2 minutes).
6. **Extensible design** for future user-defined schedules (cron format) and scheduled messaging.

### Non-Goals (This Iteration)

- **User-submitted cron schedules**: Full unix-cron parsing and user-facing schedule management API. Noted for future work.
- **Scheduled message commands**: New `scion message --at` or `scion message --every` flags. Noted for future work.
- **Distributed scheduling**: Multi-Hub leader election or distributed locking. The scheduler runs on a single Hub instance, consistent with the existing `ChannelEventPublisher` and `NotificationDispatcher` single-node model.
- **Sub-second precision**: The minimum granularity is 1 second for one-shot timers and 1 minute for recurring timers.

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                     Hub Server                          │
│                                                         │
│  ┌───────────────────────────────────────────────────┐  │
│  │                   Scheduler                       │  │
│  │                                                   │  │
│  │  ┌─────────────┐    ┌──────────────────────────┐  │  │
│  │  │  Root Ticker │    │  One-Shot Timer Manager  │  │  │
│  │  │  (1 minute)  │    │  (time.Timer per event)  │  │  │
│  │  └──────┬───────┘    └──────────┬───────────────┘  │  │
│  │         │                       │                  │  │
│  │         ▼                       ▼                  │  │
│  │  ┌─────────────────────────────────────────────┐   │  │
│  │  │          Registered Handlers                │   │  │
│  │  │                                             │   │  │
│  │  │  Built-in Recurring:                        │   │  │
│  │  │  ├─ Agent Heartbeat Timeout (2 min)         │   │  │
│  │  │  ├─ Soft-Delete Purge (60 min)              │   │  │
│  │  │  └─ (future: broker health, cleanup, etc.)  │   │  │
│  │  │                                             │   │  │
│  │  │  One-Shot:                                  │   │  │
│  │  │  ├─ Loaded from DB on startup               │   │  │
│  │  │  └─ Created via API at runtime              │   │  │
│  │  └─────────────────────────────────────────────┘   │  │
│  └───────────────────────────────────────────────────┘  │
│                                                         │
│  ┌──────────┐  ┌──────────┐  ┌───────────────────────┐  │
│  │  Store   │  │  Events  │  │  AgentDispatcher      │  │
│  └──────────┘  └──────────┘  └───────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

The Scheduler is a self-contained component within the Hub server. It manages two categories of timers:

1. **Recurring timers**: Driven by a single root ticker that fires every 1 minute. Recurring handlers register with a cadence (e.g., "every 5 minutes") and are invoked when the tick count is divisible by their interval.
2. **One-shot timers**: Individual `time.Timer` instances that fire once at a specific time. Persisted in the database for crash recovery. On startup, expired one-shot timers are immediately executed; future timers are scheduled in memory.

---

## Detailed Design

### 1. Scheduler Component

**New file:** `pkg/hub/scheduler.go`

The `Scheduler` struct owns the root ticker, the one-shot timer registry, and the set of registered recurring handlers.

```go
// Scheduler manages recurring and one-shot timers within the Hub server.
type Scheduler struct {
    store      store.Store
    events     EventPublisher
    dispatcher AgentDispatcher

    // Root ticker (1-minute heartbeat)
    // Monotonically increasing tick counter. Written by the ticker
    // goroutine and read by Status, so it is atomic.
    tickCount  atomic.Uint64

    // Recurring handlers
    recurring  []RecurringHandler

    // One-shot timers (in-memory)
    mu         sync.Mutex
    timers     map[string]*scheduledTimer // keyed by timer ID

    // Lifecycle
    stopCh     chan struct{}
    wg         sync.WaitGroup
}

// RecurringHandler defines a periodic task.
type RecurringHandler struct {
    Name     string        // Human-readable name for logging
    Interval int           // Run every N minutes (must be >= 1)
    Fn       func(ctx context.Context) // The work to perform
}

// scheduledTimer wraps a time.Timer with metadata.
type scheduledTimer struct {
    ID       string
    Timer    *time.Timer
    FireAt   time.Time
    Handler  func(ctx context.Context)
    Cancel   context.CancelFunc
}
```

#### Initialization

```go
func NewScheduler(store store.Store, events EventPublisher, dispatcher AgentDispatcher) *Scheduler {
    return &Scheduler{
        store:      store,
        events:     events,
        dispatcher: dispatcher,
        timers:     make(map[string]*scheduledTimer),
        stopCh:     make(chan struct{}),
    }
}
```

#### Registering Recurring Handlers

Recurring handlers are registered before `Start()` is called. This is done during Hub server setup:

```go
func (s *Scheduler) RegisterRecurring(name string, intervalMinutes int, fn func(ctx context.Context)) {
    s.recurring = append(s.recurring, RecurringHandler{
        Name:     name,
        Interval: intervalMinutes,
        Fn:       fn,
    })
}
```

#### Start and Root Ticker Loop

> **Tick-Zero Behavior (Important for Documentation):** All recurring handlers run immediately on startup (tick 0) because `tickCount` starts at 0 and `0 % N == 0` for any interval N. This is intentional and consistent with the existing `startPurgeLoop` behavior. Any user-facing documentation for recurring handlers must clearly state that handlers execute once immediately on scheduler start, then at their configured interval thereafter.

```go
func (s *Scheduler) Start(ctx context.Context) {
    // Load and schedule persisted one-shot timers
    s.loadPersistedTimers(ctx)

    // Start the root ticker
    s.wg.Add(1)
    go func() {
        defer s.wg.Done()

        ticker := time.NewTicker(1 * time.Minute)
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
                s.tickCount.Add(1)
                s.runRecurringHandlers(ctx)
            }
        }
    }()
}

func (s *Scheduler) runRecurringHandlers(ctx context.Context) {
    // Capture the tick once; handler goroutines log this value
    // rather than reading the field.
    tick := s.tickCount.Load()
    for _, h := range s.recurring {
        if tick%uint64(h.Interval) == 0 {
            // Run in a goroutine to avoid blocking the ticker
            handler := h // capture
            go func() {
                handlerCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
                defer cancel()

                start := time.Now()
                slog.Debug("Scheduler: running recurring handler", "name", handler.Name, "tick", tick)

                func() {
                    defer func() {
                        if r := recover(); r != nil {
                            slog.Error("Scheduler: recurring handler panicked",
                                "name", handler.Name, "panic", r)
                        }
                    }()
                    handler.Fn(handlerCtx)
                }()

                slog.Debug("Scheduler: recurring handler completed",
                    "name", handler.Name, "duration", time.Since(start))
            }()
        }
    }
}
```

The 55-second timeout on recurring handlers ensures they complete before the next 1-minute tick. Handlers that need longer should manage their own concurrency.

#### Shutdown

```go
func (s *Scheduler) Stop() {
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

    s.wg.Wait()
}
```

### 2. One-Shot Timers

One-shot timers fire once at a specific time. They are persisted in the database so they survive Hub restarts.

#### Data Model

```sql
CREATE TABLE scheduled_events (
    id TEXT PRIMARY KEY,
    grove_id TEXT NOT NULL,              -- Scope to a grove
    event_type TEXT NOT NULL,            -- e.g., "message", "status_update"
    fire_at TIMESTAMP NOT NULL,          -- When to fire (UTC)
    payload TEXT NOT NULL,               -- JSON payload (handler-specific)
    status TEXT NOT NULL DEFAULT 'pending',  -- pending, fired, cancelled, expired
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by TEXT,                     -- Principal who created the event
    fired_at TIMESTAMP,                 -- When the event actually fired
    error TEXT,                         -- Error message if firing failed

    FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE
);

CREATE INDEX idx_scheduled_events_status ON scheduled_events(status);
CREATE INDEX idx_scheduled_events_fire_at ON scheduled_events(fire_at) WHERE status = 'pending';
CREATE INDEX idx_scheduled_events_grove ON scheduled_events(grove_id);
```

#### Store Interface Extension

```go
// ScheduledEventStore manages one-shot scheduled events.
type ScheduledEventStore interface {
    // CreateScheduledEvent creates a new scheduled event.
    CreateScheduledEvent(ctx context.Context, event *ScheduledEvent) error

    // GetScheduledEvent retrieves a scheduled event by ID.
    // Returns ErrNotFound if the event doesn't exist.
    GetScheduledEvent(ctx context.Context, id string) (*ScheduledEvent, error)

    // ListPendingScheduledEvents returns all events with status "pending".
    // Used on startup to load timers into memory.
    ListPendingScheduledEvents(ctx context.Context) ([]ScheduledEvent, error)

    // UpdateScheduledEventStatus updates the status and optional error for an event.
    UpdateScheduledEventStatus(ctx context.Context, id string, status string, firedAt *time.Time, errMsg string) error

    // CancelScheduledEvent marks an event as cancelled.
    // Returns ErrNotFound if the event doesn't exist or is not pending.
    CancelScheduledEvent(ctx context.Context, id string) error

    // ListScheduledEvents returns events matching the filter criteria.
    ListScheduledEvents(ctx context.Context, filter ScheduledEventFilter, opts ListOptions) (*ListResult[ScheduledEvent], error)

    // PurgeOldScheduledEvents removes non-pending events older than cutoff.
    PurgeOldScheduledEvents(ctx context.Context, cutoff time.Time) (int, error)
}
```

#### Model

```go
// ScheduledEvent represents a one-shot timer persisted in the database.
type ScheduledEvent struct {
    ID        string    `json:"id"`
    GroveID   string    `json:"groveId"`
    EventType string    `json:"eventType"`   // "message", "status_update"
    FireAt    time.Time `json:"fireAt"`      // When to fire (UTC)
    Payload   string    `json:"payload"`     // JSON blob (handler-specific)
    Status    string    `json:"status"`      // pending, fired, cancelled, expired
    CreatedAt time.Time `json:"createdAt"`
    CreatedBy string    `json:"createdBy"`
    FiredAt   *time.Time `json:"firedAt,omitempty"`
    Error     string    `json:"error,omitempty"`
}

// ScheduledEventStatus constants
const (
    ScheduledEventPending   = "pending"
    ScheduledEventFired     = "fired"
    ScheduledEventCancelled = "cancelled"
    ScheduledEventExpired   = "expired"   // Loaded on startup past its fire time
)

// ScheduledEventFilter for listing events.
type ScheduledEventFilter struct {
    GroveID   string
    EventType string
    Status    string
}
```

#### Loading on Startup

When the scheduler starts, it loads all `pending` events from the database. Events whose `fire_at` is in the past are handled immediately (with status set to `expired` to distinguish from normal firing). Events in the future get a `time.Timer` scheduled.

```go
func (s *Scheduler) loadPersistedTimers(ctx context.Context) {
    events, err := s.store.ListPendingScheduledEvents(ctx)
    if err != nil {
        slog.Error("Scheduler: failed to load pending events", "error", err)
        return
    }

    now := time.Now()
    var expiredCount, scheduledCount int

    for _, evt := range events {
        if evt.FireAt.Before(now) || evt.FireAt.Equal(now) {
            // Expired while Hub was down — execute immediately
            expiredCount++
            go s.fireEvent(ctx, evt, true)
        } else {
            // Schedule for the future
            scheduledCount++
            s.scheduleTimer(ctx, evt)
        }
    }

    if expiredCount > 0 || scheduledCount > 0 {
        slog.Info("Scheduler: loaded persisted events",
            "expired", expiredCount, "scheduled", scheduledCount)
    }
}
```

#### Scheduling a Timer

```go
func (s *Scheduler) scheduleTimer(ctx context.Context, evt ScheduledEvent) {
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
        ID:      evt.ID,
        Timer:   timer,
        FireAt:  evt.FireAt,
        Cancel:  cancel,
    }
    s.mu.Unlock()
}
```

#### Firing an Event

```go
func (s *Scheduler) fireEvent(ctx context.Context, evt ScheduledEvent, wasExpired bool) {
    handlerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    status := ScheduledEventFired
    if wasExpired {
        status = ScheduledEventExpired
    }

    var errMsg string
    func() {
        defer func() {
            if r := recover(); r != nil {
                errMsg = fmt.Sprintf("handler panicked: %v", r)
                slog.Error("Scheduler: event handler panicked",
                    "eventID", evt.ID, "type", evt.EventType, "panic", r)
            }
        }()

        if err := s.executeEvent(handlerCtx, evt); err != nil {
            errMsg = err.Error()
            slog.Warn("Scheduler: event handler failed",
                "eventID", evt.ID, "type", evt.EventType, "error", err)
        } else {
            slog.Info("Scheduler: event fired",
                "eventID", evt.ID, "type", evt.EventType, "wasExpired", wasExpired)
        }
    }()

    now := time.Now()
    _ = s.store.UpdateScheduledEventStatus(ctx, evt.ID, status, &now, errMsg)
}

func (s *Scheduler) executeEvent(ctx context.Context, evt ScheduledEvent) error {
    switch evt.EventType {
    case "message":
        return s.handleMessageEvent(ctx, evt)
    case "status_update":
        return s.handleStatusUpdateEvent(ctx, evt)
    default:
        return fmt.Errorf("unknown event type: %s", evt.EventType)
    }
}
```

#### Creating a One-Shot Timer at Runtime

```go
// ScheduleEvent creates a new one-shot scheduled event.
// fireAt can be an absolute time, or computed from a duration (caller's responsibility).
func (s *Scheduler) ScheduleEvent(ctx context.Context, evt ScheduledEvent) error {
    // Persist to database first
    if err := s.store.CreateScheduledEvent(ctx, &evt); err != nil {
        return err
    }

    // Schedule in memory
    s.scheduleTimer(ctx, evt)

    slog.Info("Scheduler: event scheduled",
        "eventID", evt.ID, "type", evt.EventType, "fireAt", evt.FireAt)
    return nil
}

// CancelEvent cancels a pending scheduled event.
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

    return s.store.CancelScheduledEvent(ctx, id)
}
```

### 3. Built-In Recurring Handler: Agent Heartbeat Timeout

This is the first built-in recurring handler. It detects agents whose `last_seen` timestamp is older than a fixed 2-minute threshold and marks them as `undetermined`.

#### New Agent Status

```go
// In pkg/store/models.go
const (
    AgentStatusUndetermined = "undetermined"
)
```

The `undetermined` status indicates the Hub has lost confidence in the agent's actual state. The agent may still be running, it may have crashed, or its broker may have lost connectivity. It is a signal to operators and peer agents that the reported status is stale.

Any subsequent heartbeat or status update from the agent (via `UpdateAgentStatus`) clears `undetermined` and sets the agent to whatever status the update carries. No special "reset from undetermined" logic is needed — the normal status update flow handles it.

#### Store Query

A new method on `AgentStore`:

```go
// MarkStaleAgentsUndetermined marks agents with stale heartbeats as undetermined.
// Returns the updated agent records (for event publishing).
MarkStaleAgentsUndetermined(ctx context.Context, threshold time.Time) ([]Agent, error)
```

SQL implementation:

```sql
UPDATE agents
SET status = 'undetermined',
    updated_at = CURRENT_TIMESTAMP
WHERE last_seen < ?
  AND last_seen IS NOT NULL
  AND status NOT IN ('undetermined', 'stopped', 'completed', 'error', 'deleted', 'restored', 'created', 'pending')
```

This query:
- Only affects agents that have reported at least one heartbeat (`last_seen IS NOT NULL`).
- Excludes agents already in `undetermined` (idempotent).
- Excludes terminal/inactive states that should not be overwritten (`stopped`, `completed`, `error`, `deleted`, `restored`).
- Excludes agents that haven't started yet (`created`, `pending`).
- Effectively targets agents in: `running`, `busy`, `idle`, `waiting_for_input`, `provisioning`, `cloning`.

#### Handler Implementation

The handler publishes status events for each affected agent so SSE subscribers and the notification system are informed, consistent with the existing event-driven architecture:

```go
// agentHeartbeatTimeoutHandler returns a recurring handler function that
// marks agents as undetermined when their last heartbeat is stale.
func (s *Server) agentHeartbeatTimeoutHandler() func(ctx context.Context) {
    return func(ctx context.Context) {
        threshold := time.Now().Add(-2 * time.Minute)

        agents, err := s.store.MarkStaleAgentsUndetermined(ctx, threshold)
        if err != nil {
            slog.Error("Scheduler: heartbeat timeout check failed", "error", err)
            return
        }

        for _, agent := range agents {
            s.events.PublishAgentStatus(ctx, &agent)
        }

        if len(agents) > 0 {
            slog.Info("Scheduler: marked stale agents as undetermined",
                "count", len(agents), "threshold", threshold)
        }
    }
}
```

The 2-minute timeout is hardcoded in the handler. The recurring handler runs every 1 minute to ensure timely detection. Per-agent or per-grove configurability can be added later if needed.

### 4. Migrating the Purge Loop

The existing `startPurgeLoop` should be migrated into the scheduler as a recurring handler. This centralizes all periodic work and removes the ad-hoc goroutine:

```go
if cfg.SoftDeleteRetention > 0 {
    intervalMinutes := 60 // Check every hour
    scheduler.RegisterRecurring("soft-delete-purge", intervalMinutes,
        srv.purgeHandler())
}
```

The handler combines the existing `purgeExpiredAgents` logic with cleanup of completed scheduled events:

```go
func (s *Server) purgeHandler() func(ctx context.Context) {
    return func(ctx context.Context) {
        // Purge soft-deleted agents
        cutoff := time.Now().Add(-s.config.SoftDeleteRetention)
        purged, err := s.store.PurgeDeletedAgents(ctx, cutoff)
        if err != nil {
            slog.Error("Scheduler: agent purge failed", "error", err)
        } else if purged > 0 {
            slog.Info("Scheduler: purged soft-deleted agents", "count", purged)
        }

        // Purge old scheduled events (non-pending, older than 7 days)
        eventCutoff := time.Now().Add(-7 * 24 * time.Hour)
        purgedEvents, err := s.store.PurgeOldScheduledEvents(ctx, eventCutoff)
        if err != nil {
            slog.Error("Scheduler: scheduled event purge failed", "error", err)
        } else if purgedEvents > 0 {
            slog.Info("Scheduler: purged old scheduled events", "count", purgedEvents)
        }
    }
}
```

### 5. Hub Server Integration

#### Server Struct

```go
type Server struct {
    // ... existing fields ...
    scheduler *Scheduler
}
```

#### Setup (in `New()` or `setupRoutes()`)

```go
srv.scheduler = NewScheduler(srv.store, srv.events, srv.GetDispatcher())

// Register built-in recurring handlers
srv.scheduler.RegisterRecurring("agent-heartbeat-timeout", 1,
    srv.agentHeartbeatTimeoutHandler())

if srv.config.SoftDeleteRetention > 0 {
    srv.scheduler.RegisterRecurring("soft-delete-purge", 60,
        srv.purgeHandler())
}
```

#### Start

```go
func (s *Server) Start(ctx context.Context) error {
    // ... existing middleware setup ...

    // Start scheduler
    s.scheduler.Start(ctx)

    // ... existing HTTP server start ...
}
```

#### Shutdown

```go
func (s *Server) Shutdown(ctx context.Context) error {
    // Stop scheduler
    if s.scheduler != nil {
        s.scheduler.Stop()
    }

    // ... existing shutdown logic ...
}
```

### 6. Edge Cases

#### Expired Timers on Startup

When the Hub restarts, one-shot timers whose `fire_at` has passed are executed immediately. Their status is recorded as `expired` (not `fired`) in the database to distinguish them from timers that fired at the intended time. The handler itself is invoked identically regardless of whether the timer was expired — event handlers should tolerate late execution. For example, a scheduled message should still be delivered even if delayed.

The `expired` vs `fired` distinction is an audit/observability concern, not a handler concern. If a future handler needs to distinguish between on-time and late execution, `wasExpired` can be threaded through to `executeEvent` at that point.

```go
func (s *Scheduler) handleMessageEvent(ctx context.Context, evt ScheduledEvent) error {
    // Messages are still relevant even if late — deliver them
    var payload MessageEventPayload
    if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
        return fmt.Errorf("invalid message payload: %w", err)
    }
    // ... dispatch message ...
}
```

#### Duplicate Timers

The `scheduled_events` table uses a UUID primary key, preventing duplicates. Callers are responsible for idempotency — if they need "at most once" semantics, they should check for existing pending events before creating new ones.

#### Timer Drift

The root ticker uses `time.NewTicker` which may drift slightly under high load. This is acceptable for minute-granularity recurring tasks. One-shot timers use `time.AfterFunc` which is based on the Go runtime's monotonic clock and has sub-millisecond accuracy.

#### Handler Panics

All handler invocations are wrapped in `recover()` to prevent a panicking handler from crashing the scheduler or the Hub server. Panics are logged at ERROR level with the handler name.

#### Concurrent Handler Execution

Recurring handlers run in their own goroutines and may overlap if a handler takes longer than its interval. The 55-second context timeout mitigates this for 1-minute handlers, but handlers with longer intervals (e.g., 60-minute purge) should be designed to be safe for concurrent execution or use their own internal locking.

#### Hub Shutdown During Handler Execution

The `ctx` passed to handlers is derived from the server's context. When `Stop()` is called, the `stopCh` is closed and handler contexts are cancelled via the parent context. The `wg.Wait()` in `Stop()` ensures the root ticker goroutine has exited, but individual handler goroutines use their own timeout contexts and will be cancelled when the server context is done.

---

## Design Considerations

### Handler Goroutine Lifecycle on Shutdown

The `Stop()` method closes `stopCh` (preventing new ticks) and cancels all one-shot timer contexts, then calls `wg.Wait()` to wait for the root ticker goroutine to exit. However, recurring handler goroutines spawned by `runRecurringHandlers` are not tracked by the WaitGroup. This means `Stop()` may return while handler goroutines are still running.

This is acceptable for the initial implementation because:
- Recurring handlers have a 55-second context timeout, so they won't run indefinitely.
- The handler contexts are derived from the server's parent context, which is cancelled during server shutdown — so handlers will observe cancellation promptly.
- One-shot timer handlers have their own `Cancel` functions called explicitly in `Stop()`.

If precise shutdown sequencing becomes important (e.g., for graceful database connection closing), a separate `sync.WaitGroup` for active handler goroutines can be added.

### Startup Burst for Expired One-Shot Timers

When the Hub restarts after downtime, `loadPersistedTimers` fires all expired timers concurrently via `go s.fireEvent(...)`. If many timers expired during downtime, this creates a burst of goroutines and potentially a burst of database/event operations. For the initial implementation this is acceptable given expected volumes, but batching or rate-limiting could be added if needed.

### RegisterRecurring Thread Safety

`RegisterRecurring` is not thread-safe — it appends to a slice without synchronization. This is by design: all handlers must be registered before `Start()` is called. This precondition should be documented on the method.

---

## Approaches Considered

### Approach A: Unified Scheduler Component (Selected)

A single `Scheduler` struct manages both recurring and one-shot timers with a clean registration API, one root ticker, and database-backed persistence for one-shot events.

**Pros:**
- Single component to initialize, start, and stop.
- Root ticker drives all recurring work — no proliferation of goroutines.
- One-shot timers use native `time.Timer` for precision, with DB persistence for durability.
- Handlers are registered declaratively, making it easy to add new ones.
- Testable: mock the store and verify handler registration/invocation.

**Cons:**
- All scheduling responsibility in one component — could become large.
- Root ticker always runs even if no recurring handlers are registered (negligible cost).

### Approach B: Per-Task Goroutines (Current Pattern)

Continue the existing `startPurgeLoop` pattern: each scheduled task gets its own goroutine with its own `time.Ticker`.

**Pros:**
- No new abstractions — direct goroutines are simple to understand.
- Each task is fully independent.

**Cons:**
- Goroutine and ticker proliferation as tasks are added.
- No unified lifecycle management — each task needs its own context handling.
- No support for one-shot timers.
- No persistence — one-shot events would need a separate mechanism.
- Harder to observe: no central place to query "what's scheduled?"

### Approach C: External Scheduler (e.g., cron daemon, Cloud Scheduler)

Offload scheduling to an external system that calls Hub API endpoints at the appropriate times.

**Pros:**
- Proven scheduling infrastructure.
- Naturally supports cron syntax.
- Hub remains stateless with respect to scheduling.

**Cons:**
- Adds operational dependency — the Hub can no longer function standalone.
- Latency and reliability depend on the external system.
- One-shot timers require an external API (Cloud Tasks, etc.) which adds cost and complexity.
- Breaks the self-contained Hub deployment model.

### Decision

**Approach A** is selected. It provides the right balance of simplicity and capability for the Hub's current single-node architecture. The unified component replaces the ad-hoc purge loop and provides a clean extension point for new recurring and one-shot tasks.

---

## Decisions

The following questions were raised during design review and have been resolved:

### 1. `undetermined` notifications — No (default set)

`UNDETERMINED` is not added to the default notification trigger set. It is an infrastructure signal, not a task-level status change. However, this should be surfaced through user-configurable notification preferences in a future iteration, since an `undetermined` agent may need human attention.

### 2. Heartbeat timeout configurability — Hardcoded

The 2-minute timeout is hardcoded directly in the handler function. No `ServerConfig` field is added. This avoids configuration surface area for a value that doesn't need to vary yet. Per-agent or hub-wide configurability can be introduced later if needed.

### 3. One-shot timer retry — No

No retry on failure. Failed events are logged and marked with an error message. Retry with backoff can be added to the `scheduled_events` table in a future enhancement if needed.

### 4. Event payload validation — Basic

Validate `event_type` against known types and perform basic payload structure validation at creation time. Unknown types are rejected. Full schema validation per event type is deferred.

### 5. One-shot timer horizon — Load all, improve later

All pending timers are loaded into memory on startup regardless of how far in the future they fire. This is simple and correct. **Future improvement:** adopt a horizon-based approach where only timers within the next N hours are loaded into memory, with a recurring handler that promotes timers from DB to memory as they approach their fire time.

### 6. Scheduled event cleanup — Part of purge

Non-pending scheduled events older than 7 days are cleaned up by the existing purge recurring handler. This is implemented in the `purgeHandler` (see Section 4).

### 7. Broker disconnect does not imply `undetermined` — No

A disconnected Runtime Broker does **not** warrant immediately marking its agents as `undetermined`. The `sciontool` (running inside each agent container) sends heartbeat updates to the Hub independently of the broker's control channel. A broker disconnect means the Hub can no longer issue commands to agents on that broker, but the agents themselves continue reporting their status directly. The heartbeat timeout mechanism (2-minute `last_seen` threshold) remains the correct signal — if the agent itself stops reporting, *then* it becomes `undetermined`.

### 8. Tick-zero behavior — Keep, document prominently

All recurring handlers run immediately on startup (tick 0) because `0 % N == 0` for any interval N. This is intentional and matches the existing `startPurgeLoop` behavior. This must be prominently documented in any user-facing documentation for the scheduler. If a future handler should *not* run on startup, the registration API can add an optional `SkipTickZero` field to `RecurringHandler` at that point.

---

### 9. `MarkStaleAgentsUndetermined` returning updated rows

The SQL shown is a plain `UPDATE` statement, but the Go method signature returns `[]Agent`. The implementation will need SQLite's `RETURNING *` clause (available since SQLite 3.35.0, 2021) or a two-step SELECT-then-UPDATE approach. The `RETURNING` clause is preferred for atomicity. Worth confirming the project's minimum SQLite version supports it.

---

## Future Work

### User-Submitted Cron Schedules

A future iteration can add support for user-defined recurring schedules in unix-cron format. This would involve:

- A `schedules` table with cron expression, handler type, and payload.
- A cron parser (e.g., `robfig/cron`) integrated into the scheduler.
- API endpoints for CRUD operations on schedules.
- The root ticker would evaluate cron expressions on each tick to determine which schedules should fire.

### Scheduled Message Commands

New flags on `scion message`:

```bash
# Send a message in 30 minutes
scion message --in 30m agent-foo "Time to wrap up"

# Send a message at a specific time
scion message --at "2026-02-26T14:00:00Z" agent-foo "Standup time"

# Send a recurring message every 2 hours
scion message --every 2h agent-foo "Status check: how's it going?"
```

These would create `scheduled_events` (one-shot) or recurring handler registrations (recurring) via the Hub API. The recurring message feature would depend on the cron schedule infrastructure above.

### Observability

- **Metrics**: Timer count (active one-shot, registered recurring), handler execution duration, handler error count.
- **API**: `GET /api/v1/scheduler/status` — list registered recurring handlers and their last run time; list active one-shot timers.
- **Logging**: Structured logging with handler names and tick counts for tracing.

### Horizon-Based Timer Loading

The initial implementation loads all pending one-shot timers into memory on startup. If the number of long-horizon timers grows large (hundreds or thousands scheduled days/weeks out), a hybrid approach would reduce memory usage: only load timers within the next N hours into memory, and add a recurring handler that periodically promotes upcoming timers from the database.

### Distributed Scheduling

When the Hub moves to a multi-node deployment, the scheduler will need leader election to ensure only one Hub instance runs the recurring handlers and fires one-shot timers. This aligns with the broader `PostgresEventPublisher` migration path. Options include:
- PostgreSQL advisory locks.
- Kubernetes lease-based leader election.
- etcd-backed leader election.

---

## Implementation Plan

### Phase 1: Scheduler Core ✅
1. ~~Add `Scheduler` struct and `RecurringHandler` type to `pkg/hub/scheduler.go`.~~
2. ~~Implement root ticker loop with handler registration and invocation.~~
3. ~~Wire scheduler into `Server` startup and shutdown.~~
4. ~~Migrate existing `startPurgeLoop` to a registered recurring handler.~~
5. ~~Unit tests for scheduler lifecycle and handler invocation.~~

### Phase 2: Agent Heartbeat Timeout ✅
6. ~~Add `AgentStatusUndetermined` constant to `pkg/store/models.go`.~~
7. ~~Add `MarkStaleAgentsUndetermined` to `AgentStore` interface and SQLite implementation.~~
8. ~~Implement heartbeat timeout recurring handler (hardcoded 2-minute threshold).~~
9. ~~Wire handler registration into server setup (always registered, 1-minute interval).~~
10. ~~Tests for the heartbeat timeout query and handler.~~

### Phase 3: One-Shot Timer Infrastructure ✅
11. ~~Add `scheduled_events` table (new SQLite migration).~~
12. ~~Add `ScheduledEventStore` interface and SQLite implementation.~~
13. ~~Add `ScheduledEvent` model to `pkg/store/models.go`.~~
14. ~~Implement one-shot timer loading, scheduling, firing, and cancellation.~~
15. ~~Add scheduled event cleanup to the purge recurring handler.~~
16. ~~Tests for one-shot timer lifecycle, expired timer handling, and cancellation.~~

### Phase 4: Scheduler API and CLI ✅
17. ~~Hub API endpoints for creating, listing, getting, and cancelling scheduled events.~~
18. ~~CLI `--in`/`--at` flags on `scion message` for scheduling future message delivery.~~
19. ~~Event handler registry, message event handler, Hub client service, and integration tests.~~

---

## Files Affected

| File | Change |
|---|---|
| `pkg/hub/scheduler.go` | **New** — `Scheduler`, `RecurringHandler`, one-shot timer management |
| `pkg/hub/scheduler_test.go` | **New** — Unit tests |
| `pkg/hub/server.go` | Add `scheduler` field, wire into `Start()`/`Shutdown()`, remove `startPurgeLoop` |
| `pkg/store/models.go` | Add `AgentStatusUndetermined`, `ScheduledEvent` model, status constants |
| `pkg/store/store.go` | Add `MarkStaleAgentsUndetermined` to `AgentStore`, add `ScheduledEventStore` interface |
| `pkg/store/sqlite/sqlite.go` | New migration (scheduled_events table), implement new store methods |
| `pkg/hub/handlers.go` | Add heartbeat timeout handler and purge handler functions |

---

## Related Documents

- [Hub Soft Delete](hub-soft-delete.md) — Existing purge loop that will migrate into the scheduler.
- [Notifications](notifications.md) — Event-driven notification system that will receive `undetermined` status events.
- [Hosted Architecture](hosted-architecture.md) — System overview and component relationships.
- [Web Realtime Events](web-realtime.md) — `ChannelEventPublisher` for agent status events.
- [Status](status.md) — Implementation status including identified gaps for heartbeat detection.
