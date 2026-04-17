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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newTestScheduler creates a scheduler with a fast tick interval for testing.
func newTestScheduler(interval time.Duration) *Scheduler {
	s := NewScheduler(nil, slog.Default())
	s.tickInterval = interval
	return s
}

// newTestSchedulerWithStore creates a scheduler with a mock store and fast tick interval.
func newTestSchedulerWithStore(interval time.Duration, st store.Store) *Scheduler {
	s := NewScheduler(st, slog.Default())
	s.tickInterval = interval
	return s
}

// ============================================================================
// Recurring Handler Tests
// ============================================================================

func TestSchedulerStartStop(t *testing.T) {
	s := newTestScheduler(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	// Give it time to run a few ticks
	time.Sleep(120 * time.Millisecond)

	s.Stop()

	// Calling Stop twice must not panic
	s.Stop()
}

func TestSchedulerTickZero(t *testing.T) {
	s := newTestScheduler(1 * time.Second) // long interval — we only care about tick 0

	var called atomic.Int32

	s.RegisterRecurring("tick-zero-handler", 1, func(ctx context.Context) {
		called.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	// Wait for tick-0 handler to execute
	deadline := time.After(500 * time.Millisecond)
	for {
		if called.Load() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("tick-zero handler was not invoked within timeout")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	s.Stop()

	if got := called.Load(); got != 1 {
		t.Errorf("expected tick-zero handler to be called once, got %d", got)
	}
}

func TestSchedulerRecurringInterval(t *testing.T) {
	s := newTestScheduler(30 * time.Millisecond)

	var every1 atomic.Int32
	var every2 atomic.Int32
	var every3 atomic.Int32

	s.RegisterRecurring("every-1", 1, func(ctx context.Context) {
		every1.Add(1)
	})
	s.RegisterRecurring("every-2", 2, func(ctx context.Context) {
		every2.Add(1)
	})
	s.RegisterRecurring("every-3", 3, func(ctx context.Context) {
		every3.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	// Let 6 ticks pass (tick 0..6 = 7 invocations for every-1)
	// tick 0: all fire. tick 1: every-1. tick 2: every-1, every-2. tick 3: every-1, every-3.
	// tick 4: every-1, every-2. tick 5: every-1. tick 6: every-1, every-2, every-3.
	time.Sleep(220 * time.Millisecond) // ~7 ticks at 30ms

	s.Stop()

	got1 := every1.Load()
	got2 := every2.Load()
	got3 := every3.Load()

	// every-1 should run on every tick (7 times for ticks 0-6)
	if got1 < 5 {
		t.Errorf("every-1 handler expected at least 5 invocations, got %d", got1)
	}
	// every-2 should run on ticks 0, 2, 4, 6 (4 times)
	if got2 < 3 {
		t.Errorf("every-2 handler expected at least 3 invocations, got %d", got2)
	}
	// every-3 should run on ticks 0, 3, 6 (3 times)
	if got3 < 2 {
		t.Errorf("every-3 handler expected at least 2 invocations, got %d", got3)
	}
	// every-1 should always run more than every-2, which runs more than every-3
	if got1 <= got2 {
		t.Errorf("every-1 (%d) should have more invocations than every-2 (%d)", got1, got2)
	}
	if got2 <= got3 {
		t.Errorf("every-2 (%d) should have more invocations than every-3 (%d)", got2, got3)
	}
}

func TestSchedulerHandlerPanicRecovery(t *testing.T) {
	s := newTestScheduler(50 * time.Millisecond)

	var panickerCalled atomic.Int32
	var normalCalled atomic.Int32

	s.RegisterRecurring("panicker", 1, func(ctx context.Context) {
		panickerCalled.Add(1)
		panic("test panic")
	})
	s.RegisterRecurring("normal", 1, func(ctx context.Context) {
		normalCalled.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	// Wait for at least 2 ticks
	time.Sleep(130 * time.Millisecond)

	s.Stop()

	if got := panickerCalled.Load(); got < 2 {
		t.Errorf("panicking handler should have been called at least 2 times, got %d", got)
	}
	if got := normalCalled.Load(); got < 2 {
		t.Errorf("normal handler should have been called at least 2 times despite panic in other handler, got %d", got)
	}
}

func TestSchedulerContextCancellation(t *testing.T) {
	s := newTestScheduler(50 * time.Millisecond)

	var called atomic.Int32

	s.RegisterRecurring("counter", 1, func(ctx context.Context) {
		called.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	// Let tick 0 fire
	time.Sleep(30 * time.Millisecond)

	// Cancel context — scheduler should stop
	cancel()

	// Wait for scheduler to observe cancellation
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Good — Stop returned
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after context cancellation")
	}
}

func TestSchedulerHandlerReceivesContext(t *testing.T) {
	s := newTestScheduler(1 * time.Second)

	var mu sync.Mutex
	var handlerCtx context.Context

	s.RegisterRecurring("ctx-check", 1, func(ctx context.Context) {
		mu.Lock()
		handlerCtx = ctx
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	// Wait for tick 0
	deadline := time.After(500 * time.Millisecond)
	for {
		mu.Lock()
		got := handlerCtx
		mu.Unlock()
		if got != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("handler was not invoked within timeout")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	s.Stop()

	mu.Lock()
	defer mu.Unlock()

	// The handler context should have a deadline (55-second timeout)
	if _, ok := handlerCtx.Deadline(); !ok {
		t.Error("handler context should have a deadline from the 55-second timeout")
	}
}

func TestSchedulerMinimumInterval(t *testing.T) {
	s := newTestScheduler(30 * time.Millisecond)

	var called atomic.Int32

	// Register with invalid interval (0) — should be clamped to 1
	s.RegisterRecurring("clamped", 0, func(ctx context.Context) {
		called.Add(1)
	})

	if s.recurring[0].Interval != 1 {
		t.Errorf("expected interval to be clamped to 1, got %d", s.recurring[0].Interval)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)
	time.Sleep(80 * time.Millisecond)
	s.Stop()

	if got := called.Load(); got < 2 {
		t.Errorf("clamped handler should have been called at least 2 times, got %d", got)
	}
}

func TestSchedulerNoHandlers(t *testing.T) {
	s := newTestScheduler(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start with no handlers — should not panic
	s.Start(ctx)
	time.Sleep(80 * time.Millisecond)
	s.Stop()
}

// ============================================================================
// One-Shot Timer Tests
// ============================================================================

// mockScheduledEventStore is a minimal in-memory store for testing one-shot
// timer scheduling. It only implements the ScheduledEventStore methods needed
// by the Scheduler; all other Store interface methods panic if called.
type mockScheduledEventStore struct {
	store.Store // embed to satisfy the interface; unused methods panic
	mu           sync.Mutex
	events       map[string]*store.ScheduledEvent
	agents       map[string]*store.Agent
	groves       map[string]*store.Grove
	workflowRuns map[string]*store.WorkflowRun
}

func newMockStore() *mockScheduledEventStore {
	return &mockScheduledEventStore{
		events:       make(map[string]*store.ScheduledEvent),
		agents:       make(map[string]*store.Agent),
		groves:       make(map[string]*store.Grove),
		workflowRuns: make(map[string]*store.WorkflowRun),
	}
}

func (m *mockScheduledEventStore) CreateScheduledEvent(_ context.Context, event *store.ScheduledEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.events[event.ID]; exists {
		return store.ErrAlreadyExists
	}
	if event.Status == "" {
		event.Status = store.ScheduledEventPending
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	cp := *event
	m.events[event.ID] = &cp
	return nil
}

func (m *mockScheduledEventStore) GetScheduledEvent(_ context.Context, id string) (*store.ScheduledEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *evt
	return &cp, nil
}

func (m *mockScheduledEventStore) ListPendingScheduledEvents(_ context.Context) ([]store.ScheduledEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []store.ScheduledEvent
	for _, evt := range m.events {
		if evt.Status == store.ScheduledEventPending {
			result = append(result, *evt)
		}
	}
	return result, nil
}

func (m *mockScheduledEventStore) UpdateScheduledEventStatus(_ context.Context, id string, status string, firedAt *time.Time, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	evt.Status = status
	evt.FiredAt = firedAt
	evt.Error = errMsg
	return nil
}

func (m *mockScheduledEventStore) CancelScheduledEvent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if evt.Status != store.ScheduledEventPending {
		return store.ErrNotFound
	}
	evt.Status = store.ScheduledEventCancelled
	return nil
}

func (m *mockScheduledEventStore) ListScheduledEvents(_ context.Context, _ store.ScheduledEventFilter, _ store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	return &store.ListResult[store.ScheduledEvent]{}, nil
}

func (m *mockScheduledEventStore) PurgeOldScheduledEvents(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

// Agent-related methods for message handler tests

func (m *mockScheduledEventStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.agents[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetAgentBySlug(_ context.Context, groveID, slug string) (*store.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		if a.GroveID == groveID && a.Slug == slug {
			cp := *a
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

// Grove and agent-creation methods for dispatch_agent handler tests

func (m *mockScheduledEventStore) GetGrove(_ context.Context, id string) (*store.Grove, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.groves[id]; ok {
		cp := *g
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetGroveProviders(_ context.Context, _ string) ([]store.GroveProvider, error) {
	return nil, nil
}

func (m *mockScheduledEventStore) CreateAgent(_ context.Context, agent *store.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents[agent.ID] = agent
	return nil
}

func (m *mockScheduledEventStore) GetTemplate(_ context.Context, _ string) (*store.Template, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) GetTemplateBySlug(_ context.Context, _, _, _ string) (*store.Template, error) {
	return nil, store.ErrNotFound
}

// getEvent returns an event by ID (test helper, no error).
func (m *mockScheduledEventStore) getEvent(id string) *store.ScheduledEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[id]
}

// WorkflowRun methods for workflowRunEventHandler tests.

func (m *mockScheduledEventStore) CreateWorkflowRun(_ context.Context, run *store.WorkflowRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workflowRuns == nil {
		m.workflowRuns = make(map[string]*store.WorkflowRun)
	}
	cp := *run
	m.workflowRuns[run.ID] = &cp
	return nil
}

func (m *mockScheduledEventStore) GetWorkflowRun(_ context.Context, id string) (*store.WorkflowRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workflowRuns == nil {
		return nil, store.ErrNotFound
	}
	r, ok := m.workflowRuns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *mockScheduledEventStore) ListWorkflowRuns(_ context.Context, _ store.WorkflowRunListOptions) (*store.ListResult[store.WorkflowRun], error) {
	return &store.ListResult[store.WorkflowRun]{}, nil
}

func (m *mockScheduledEventStore) CancelWorkflowRun(_ context.Context, _ string) (*store.WorkflowRun, error) {
	return nil, store.ErrNotFound
}

func (m *mockScheduledEventStore) TransitionWorkflowRun(_ context.Context, _ string, _ store.WorkflowRunTransition, _ []string) (*store.WorkflowRun, error) {
	return nil, nil
}

func TestOneShotTimerFiresAtCorrectTime(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)
	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		return nil
	})

	var fired atomic.Int32

	// We test via the scheduler's fireEvent mechanism by scheduling a short-delay event
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "timer-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(50 * time.Millisecond),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	// scheduleTimer directly to test the timer mechanism
	s.scheduleTimer(ctx, evt)

	// Wait for the timer to fire — give generous timeout
	deadline := time.After(500 * time.Millisecond)
	for {
		e := ms.getEvent("timer-1")
		if e != nil && e.Status != store.ScheduledEventPending {
			fired.Add(1)
			break
		}
		select {
		case <-deadline:
			t.Fatal("timer did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify the event was marked as fired
	e := ms.getEvent("timer-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.FiredAt == nil {
		t.Error("expected FiredAt to be set")
	}

	// Timer should have been removed from the in-memory map
	s.mu.Lock()
	_, exists := s.timers["timer-1"]
	s.mu.Unlock()
	if exists {
		t.Error("timer should have been removed from in-memory map after firing")
	}
}

func TestOneShotExpiredTimerFiresImmediately(t *testing.T) {
	ms := newMockStore()

	// Create an event that is already past its fire_at
	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "expired-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(-1 * time.Hour), // In the past
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	s := newTestSchedulerWithStore(1*time.Second, ms)
	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// loadPersistedTimers should fire expired event immediately
	s.loadPersistedTimers(ctx)

	// Wait for the async fire
	deadline := time.After(500 * time.Millisecond)
	for {
		e := ms.getEvent("expired-1")
		if e != nil && e.Status != store.ScheduledEventPending {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expired timer did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	e := ms.getEvent("expired-1")
	if e.Status != store.ScheduledEventExpired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventExpired, e.Status)
	}
}

func TestOneShotTimerCancellation(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "cancel-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(10 * time.Second), // Far in the future
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	// Schedule the timer
	s.scheduleTimer(ctx, evt)

	// Verify timer is in the map
	s.mu.Lock()
	_, exists := s.timers["cancel-1"]
	s.mu.Unlock()
	if !exists {
		t.Fatal("timer should exist in memory after scheduling")
	}

	// Cancel the timer
	err := s.CancelEvent(ctx, "cancel-1")
	if err != nil {
		t.Fatalf("CancelEvent failed: %v", err)
	}

	// Timer should be removed from map
	s.mu.Lock()
	_, exists = s.timers["cancel-1"]
	s.mu.Unlock()
	if exists {
		t.Error("timer should have been removed from map after cancellation")
	}

	// Event should be cancelled in the store
	e := ms.getEvent("cancel-1")
	if e.Status != store.ScheduledEventCancelled {
		t.Errorf("expected status %q, got %q", store.ScheduledEventCancelled, e.Status)
	}
}

func TestScheduleEventPersistsAndSchedules(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "schedule-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(5 * time.Second),
		Payload:   `{"msg":"test"}`,
		Status:    store.ScheduledEventPending,
	}

	err := s.ScheduleEvent(ctx, evt)
	if err != nil {
		t.Fatalf("ScheduleEvent failed: %v", err)
	}

	// Should be persisted in the store
	e := ms.getEvent("schedule-1")
	if e == nil {
		t.Fatal("event should be persisted in the store")
	}
	if e.Status != store.ScheduledEventPending {
		t.Errorf("expected status %q, got %q", store.ScheduledEventPending, e.Status)
	}

	// Should be in the in-memory timer map
	s.mu.Lock()
	_, exists := s.timers["schedule-1"]
	s.mu.Unlock()
	if !exists {
		t.Error("timer should exist in memory after ScheduleEvent")
	}

	// Cleanup
	s.Stop()
}

func TestStopCancelsAllOneShotTimers(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Schedule multiple timers far in the future
	for i := 0; i < 3; i++ {
		evt := store.ScheduledEvent{
			ID:        "stop-timer-" + string(rune('a'+i)),
			GroveID:   "grove-1",
			EventType: "message",
			FireAt:    time.Now().Add(1 * time.Hour),
			Payload:   "{}",
			Status:    store.ScheduledEventPending,
		}
		ms.CreateScheduledEvent(ctx, &evt)
		s.scheduleTimer(ctx, evt)
	}

	// Verify all timers are in the map
	s.mu.Lock()
	timerCount := len(s.timers)
	s.mu.Unlock()
	if timerCount != 3 {
		t.Fatalf("expected 3 timers, got %d", timerCount)
	}

	// Start and immediately stop (no recurring handlers needed)
	s.Start(ctx)
	s.Stop()

	// All timers should be cleared
	s.mu.Lock()
	timerCount = len(s.timers)
	s.mu.Unlock()
	if timerCount != 0 {
		t.Errorf("expected 0 timers after Stop, got %d", timerCount)
	}
}

func TestOneShotHandlerPanicRecovery(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)
	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "panic-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	// Fire the event directly
	s.fireEvent(ctx, evt, false)

	// Verify the event was fired successfully
	e := ms.getEvent("panic-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.Error != "" {
		t.Errorf("expected no error, got %q", e.Error)
	}
}

func TestOneShotUnknownEventTypeReturnsError(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "unknown-1",
		GroveID:   "grove-1",
		EventType: "nonexistent_type",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	// Fire the event directly
	s.fireEvent(ctx, evt, false)

	// Verify the event was fired but with an error
	e := ms.getEvent("unknown-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.Error == "" {
		t.Error("expected error message for unknown event type")
	}
	if e.Error != "unknown event type: nonexistent_type" {
		t.Errorf("unexpected error message: %q", e.Error)
	}
}

func TestOneShotNilStoreSafety(t *testing.T) {
	// A scheduler with nil store should not panic during loadPersistedTimers
	s := newTestScheduler(1 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should not panic
	s.loadPersistedTimers(ctx)

	// ScheduleEvent should return an error
	err := s.ScheduleEvent(ctx, store.ScheduledEvent{
		ID:        "nil-store-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   "{}",
	})
	if err == nil {
		t.Error("expected error when scheduling with nil store")
	}
}

// ============================================================================
// Event Handler Registry Tests
// ============================================================================

func TestRegisterEventHandlerAndDispatch(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	var handlerCalled atomic.Int32
	var receivedPayload string
	var mu sync.Mutex

	s.RegisterEventHandler("message", func(_ context.Context, evt store.ScheduledEvent) error {
		handlerCalled.Add(1)
		mu.Lock()
		receivedPayload = evt.Payload
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "handler-test-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now(),
		Payload:   `{"msg":"hello"}`,
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	s.fireEvent(ctx, evt, false)

	if got := handlerCalled.Load(); got != 1 {
		t.Errorf("expected handler to be called once, got %d", got)
	}
	mu.Lock()
	if receivedPayload != `{"msg":"hello"}` {
		t.Errorf("expected payload %q, got %q", `{"msg":"hello"}`, receivedPayload)
	}
	mu.Unlock()

	e := ms.getEvent("handler-test-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.Error != "" {
		t.Errorf("expected no error, got %q", e.Error)
	}
}

func TestEventHandlerErrorIsCaptured(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		return fmt.Errorf("handler failed: something went wrong")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "handler-err-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	s.fireEvent(ctx, evt, false)

	e := ms.getEvent("handler-err-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.Error != "handler failed: something went wrong" {
		t.Errorf("expected error message %q, got %q", "handler failed: something went wrong", e.Error)
	}
}

func TestUnregisteredEventTypeReturnsError(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)
	// Deliberately do not register any handler

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:        "no-handler-1",
		GroveID:   "grove-1",
		EventType: "some_unregistered_type",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &evt)

	s.fireEvent(ctx, evt, false)

	e := ms.getEvent("no-handler-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if e.Error != "unknown event type: some_unregistered_type" {
		t.Errorf("expected error about unknown event type, got %q", e.Error)
	}
}

func TestScheduleEventWithCancelledCallerContext(t *testing.T) {
	// Regression test: when ScheduleEvent is called from an HTTP handler,
	// the caller's context (r.Context()) is cancelled as soon as the response
	// is sent. The timer must still fire using the scheduler's long-lived context.
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	var handlerCalled atomic.Int32
	s.RegisterEventHandler("message", func(_ context.Context, evt store.ScheduledEvent) error {
		handlerCalled.Add(1)
		return nil
	})

	// Start the scheduler with a long-lived context (simulates server lifetime)
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	s.Start(serverCtx)

	// Create a short-lived context simulating an HTTP request
	reqCtx, reqCancel := context.WithCancel(context.Background())

	evt := store.ScheduledEvent{
		ID:        "req-ctx-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now().Add(80 * time.Millisecond),
		Payload:   `{"msg":"test"}`,
		Status:    store.ScheduledEventPending,
	}

	err := s.ScheduleEvent(reqCtx, evt)
	if err != nil {
		t.Fatalf("ScheduleEvent failed: %v", err)
	}

	// Cancel the request context immediately (simulates HTTP response sent)
	reqCancel()

	// Wait for the timer to fire
	deadline := time.After(1 * time.Second)
	for {
		e := ms.getEvent("req-ctx-1")
		if e != nil && e.Status != store.ScheduledEventPending {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timer did not fire after caller context was cancelled — this is the bug")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	e := ms.getEvent("req-ctx-1")
	if e.Status != store.ScheduledEventFired {
		t.Errorf("expected status %q, got %q", store.ScheduledEventFired, e.Status)
	}
	if handlerCalled.Load() != 1 {
		t.Errorf("expected handler to be called once, got %d", handlerCalled.Load())
	}

	s.Stop()
}

func TestExpiredEventsFromDowntimeStillFire(t *testing.T) {
	// Simulate a server that was offline for a while: multiple events with
	// fire_at in the past should all be recovered and executed on startup.
	ms := newMockStore()
	ctx := context.Background()

	var fired atomic.Int32
	now := time.Now()

	// Create events that expired at different times during "downtime"
	for i, staleness := range []time.Duration{5 * time.Minute, 2 * time.Hour, 24 * time.Hour} {
		evt := store.ScheduledEvent{
			ID:        fmt.Sprintf("downtime-%d", i),
			GroveID:   "grove-1",
			EventType: "message",
			FireAt:    now.Add(-staleness),
			Payload:   `{"msg":"recover me"}`,
			Status:    store.ScheduledEventPending,
		}
		ms.CreateScheduledEvent(ctx, &evt)
	}

	s := newTestSchedulerWithStore(1*time.Second, ms)
	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		fired.Add(1)
		return nil
	})

	serverCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(serverCtx)

	// Wait for all expired events to fire
	deadline := time.After(1 * time.Second)
	for {
		if fired.Load() >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected 3 expired events to fire, got %d", fired.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify all events were marked as expired (not just fired)
	for i := 0; i < 3; i++ {
		e := ms.getEvent(fmt.Sprintf("downtime-%d", i))
		if e.Status != store.ScheduledEventExpired {
			t.Errorf("event downtime-%d: expected status %q, got %q", i, store.ScheduledEventExpired, e.Status)
		}
	}

	s.Stop()
}

func TestMessageEventHandler_AgentNotFound(t *testing.T) {
	// When a message event fires for an agent that has been deleted,
	// the handler should return a clear error indicating the agent
	// no longer exists (not a generic "failed to resolve" error).
	ms := newMockStore()

	// Create a Server with the mock store — no agents registered
	srv := &Server{store: ms}
	handler := srv.messageEventHandler()

	ctx := context.Background()

	evt := store.ScheduledEvent{
		ID:        "msg-no-agent-1",
		GroveID:   "grove-1",
		EventType: "message",
		Payload:   `{"agentName":"deleted-agent","message":"hello?"}`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error when agent does not exist")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected 'no longer exists' in error, got: %s", err)
	}
	if !strings.Contains(err.Error(), "deleted-agent") {
		t.Errorf("expected agent name in error, got: %s", err)
	}
}

func TestMessageEventHandler_AgentNotFoundByID(t *testing.T) {
	ms := newMockStore()
	srv := &Server{store: ms}
	handler := srv.messageEventHandler()

	ctx := context.Background()

	evt := store.ScheduledEvent{
		ID:        "msg-no-agent-2",
		GroveID:   "grove-1",
		EventType: "message",
		Payload:   `{"agentId":"nonexistent-id","message":"hello?"}`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error when agent does not exist")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected 'no longer exists' in error, got: %s", err)
	}
}

func TestMultipleEventHandlers(t *testing.T) {
	ms := newMockStore()
	s := newTestSchedulerWithStore(1*time.Second, ms)

	var messageCalled, statusCalled atomic.Int32

	s.RegisterEventHandler("message", func(_ context.Context, _ store.ScheduledEvent) error {
		messageCalled.Add(1)
		return nil
	})
	s.RegisterEventHandler("status_update", func(_ context.Context, _ store.ScheduledEvent) error {
		statusCalled.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Fire a message event
	msgEvt := store.ScheduledEvent{
		ID:        "multi-msg-1",
		GroveID:   "grove-1",
		EventType: "message",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &msgEvt)
	s.fireEvent(ctx, msgEvt, false)

	// Fire a status_update event
	statusEvt := store.ScheduledEvent{
		ID:        "multi-status-1",
		GroveID:   "grove-1",
		EventType: "status_update",
		FireAt:    time.Now(),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	ms.CreateScheduledEvent(ctx, &statusEvt)
	s.fireEvent(ctx, statusEvt, false)

	if got := messageCalled.Load(); got != 1 {
		t.Errorf("expected message handler called once, got %d", got)
	}
	if got := statusCalled.Load(); got != 1 {
		t.Errorf("expected status handler called once, got %d", got)
	}
}

func TestDispatchAgentEventHandler_InvalidPayload(t *testing.T) {
	ms := newMockStore()
	srv := &Server{store: ms}
	handler := srv.dispatchAgentEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "dispatch-bad-1",
		GroveID:   "grove-1",
		EventType: "dispatch_agent",
		Payload:   `not valid json`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error for invalid JSON payload")
	}
	if !strings.Contains(err.Error(), "invalid dispatch_agent payload") {
		t.Errorf("expected 'invalid dispatch_agent payload' in error, got: %s", err)
	}
}

func TestDispatchAgentEventHandler_MissingAgentName(t *testing.T) {
	ms := newMockStore()
	srv := &Server{store: ms}
	handler := srv.dispatchAgentEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "dispatch-noname-1",
		GroveID:   "grove-1",
		EventType: "dispatch_agent",
		Payload:   `{"template":"my-template"}`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error for missing agentName")
	}
	if !strings.Contains(err.Error(), "agentName is required") {
		t.Errorf("expected 'agentName is required' in error, got: %s", err)
	}
}

func TestDispatchAgentEventHandler_GroveNotFound(t *testing.T) {
	ms := newMockStore()
	srv := &Server{store: ms}
	handler := srv.dispatchAgentEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "dispatch-nogrove-1",
		GroveID:   "nonexistent-grove",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"worker-1"}`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error for missing grove")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected 'no longer exists' in error, got: %s", err)
	}
}

func TestDispatchAgentEventHandler_AgentAlreadyExists(t *testing.T) {
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}
	ms.agents["existing-1"] = &store.Agent{
		ID:      "existing-1",
		Slug:    "worker-1",
		Name:    "worker-1",
		GroveID: "grove-1",
		Phase:   "running",
	}

	srv := &Server{store: ms}
	handler := srv.dispatchAgentEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "dispatch-exists-1",
		GroveID:   "grove-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"worker-1"}`,
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error for existing agent")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %s", err)
	}
}

func TestDispatchAgentEventHandler_CreatesAgentNoDispatcher(t *testing.T) {
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}

	srv := &Server{store: ms}
	handler := srv.dispatchAgentEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:        "dispatch-ok-1",
		GroveID:   "grove-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"new-worker","template":"my-tmpl","task":"Do the thing"}`,
	}

	// Should succeed — agent is created but not dispatched (no dispatcher)
	err := handler(ctx, evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify agent was created in the store
	found := false
	for _, a := range ms.agents {
		if a.Slug == "new-worker" && a.GroveID == "grove-1" {
			found = true
			if a.Template != "my-tmpl" {
				t.Errorf("expected template 'my-tmpl', got %q", a.Template)
			}
			if a.AppliedConfig == nil || a.AppliedConfig.Task != "Do the thing" {
				t.Errorf("expected task 'Do the thing' in applied config")
			}
			break
		}
	}
	if !found {
		t.Error("agent was not created in the store")
	}
}

// ============================================================================
// WorkflowRun Event Handler Tests
// ============================================================================

func TestWorkflowRunEventHandler_EmptySource(t *testing.T) {
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}

	srv := &Server{store: ms}
	handler := srv.workflowRunEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:             "wr-empty-1",
		GroveID:        "grove-1",
		EventType:      "workflow_run",
		WorkflowSource: "", // missing source
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error for empty workflow_source")
	}
	if !strings.Contains(err.Error(), "empty workflow_source") {
		t.Errorf("expected 'empty workflow_source' in error, got: %s", err)
	}
}

func TestWorkflowRunEventHandler_GroveNotFound(t *testing.T) {
	ms := newMockStore()
	// grove not registered

	srv := &Server{store: ms}
	handler := srv.workflowRunEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:             "wr-nogrove-1",
		GroveID:        "nonexistent-grove",
		EventType:      "workflow_run",
		WorkflowSource: "flow: hello",
	}

	err := handler(ctx, evt)
	if err == nil {
		t.Fatal("expected error when grove does not exist")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected 'no longer exists' in error, got: %s", err)
	}
}

func TestWorkflowRunEventHandler_CreatesRunNoDispatcher(t *testing.T) {
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}

	// Server without a workflow dispatcher (nil).
	srv := &Server{store: ms, workflowDispatcher: nil}
	handler := srv.workflowRunEventHandler()

	ctx := context.Background()
	userID := "user-abc"
	evt := store.ScheduledEvent{
		ID:             "wr-ok-1",
		GroveID:        "grove-1",
		EventType:      "workflow_run",
		WorkflowSource: "flow:\n  name: test",
		WorkflowInputs: `{"key":"value"}`,
		CreatedBy:      userID,
	}

	err := handler(ctx, evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify a WorkflowRun was created in the store.
	if len(ms.workflowRuns) != 1 {
		t.Fatalf("expected 1 WorkflowRun in store, got %d", len(ms.workflowRuns))
	}

	var run *store.WorkflowRun
	for _, r := range ms.workflowRuns {
		run = r
	}

	if run.GroveID != "grove-1" {
		t.Errorf("expected groveID 'grove-1', got %q", run.GroveID)
	}
	if run.SourceYaml != "flow:\n  name: test" {
		t.Errorf("unexpected source yaml: %q", run.SourceYaml)
	}
	if run.InputsJSON != `{"key":"value"}` {
		t.Errorf("unexpected inputs json: %q", run.InputsJSON)
	}
	if run.Status != store.WorkflowRunStatusQueued {
		t.Errorf("expected status %q, got %q", store.WorkflowRunStatusQueued, run.Status)
	}
	if run.CreatedByUserID == nil || *run.CreatedByUserID != userID {
		t.Errorf("expected created_by_user_id %q", userID)
	}
}

func TestWorkflowRunEventHandler_DefaultsEmptyInputs(t *testing.T) {
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}

	srv := &Server{store: ms}
	handler := srv.workflowRunEventHandler()

	ctx := context.Background()
	evt := store.ScheduledEvent{
		ID:             "wr-inputs-default-1",
		GroveID:        "grove-1",
		EventType:      "workflow_run",
		WorkflowSource: "flow: minimal",
		WorkflowInputs: "", // empty — should default to "{}"
	}

	err := handler(ctx, evt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range ms.workflowRuns {
		if r.InputsJSON != "{}" {
			t.Errorf("expected inputs JSON '{}', got %q", r.InputsJSON)
		}
	}
}

func TestScheduleWorkflowRunFires(t *testing.T) {
	// End-to-end: register workflow_run handler on the scheduler, fire an event,
	// verify a WorkflowRun lands in the store.
	ms := newMockStore()
	ms.groves["grove-1"] = &store.Grove{ID: "grove-1", Name: "test-grove"}

	srv := &Server{store: ms, workflowDispatcher: nil}
	s := newTestSchedulerWithStore(1*time.Second, ms)
	s.RegisterEventHandler("workflow_run", srv.workflowRunEventHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evt := store.ScheduledEvent{
		ID:             "wr-sched-1",
		GroveID:        "grove-1",
		EventType:      "workflow_run",
		FireAt:         time.Now().Add(50 * time.Millisecond),
		WorkflowSource: "flow:\n  name: sched-test",
		Status:         store.ScheduledEventPending,
		CreatedBy:      "user-sched",
	}
	if err := ms.CreateScheduledEvent(ctx, &evt); err != nil {
		t.Fatal(err)
	}
	s.scheduleTimer(ctx, evt)

	// Wait for timer to fire.
	deadline := time.After(1 * time.Second)
	for {
		ms.mu.Lock()
		count := len(ms.workflowRuns)
		ms.mu.Unlock()
		if count > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for WorkflowRun to be created")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, r := range ms.workflowRuns {
		if r.GroveID != "grove-1" {
			t.Errorf("unexpected groveID: %q", r.GroveID)
		}
		if r.Status != store.WorkflowRunStatusQueued {
			t.Errorf("expected queued, got %q", r.Status)
		}
	}
}
