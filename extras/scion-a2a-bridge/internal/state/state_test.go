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

package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestSQLiteStore creates a temporary SQLiteStore for testing.
func newTestSQLiteStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newTestPostgresStore creates a PostgresStore for testing against a real Postgres.
// Skips the test if TEST_DATABASE_URL is not set.
func newTestPostgresStore(t *testing.T) Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := NewPostgres(url)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	// Clean up tables between tests to avoid cross-contamination.
	t.Cleanup(func() {
		s.db.Exec("DELETE FROM a2a_task_events")
		s.db.Exec("DELETE FROM a2a_push_notification_configs")
		s.db.Exec("DELETE FROM a2a_tasks")
		s.db.Exec("DELETE FROM a2a_contexts")
		s.Close()
	})
	return s
}

// runStoreTests runs the full test suite against any Store implementation.
func runStoreTests(t *testing.T, newStore func(t *testing.T) Store) {
	t.Run("TaskCRUD", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		task := &Task{
			ID:        "task-1",
			ContextID: "ctx-1",
			ProjectID: "grove-1",
			AgentSlug: "agent-1",
			AgentID:   "agent-id-1",
			State:     "submitted",
			CreatedAt: now,
			UpdatedAt: now,
			Metadata:  "{}",
		}

		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		got, err := s.GetTask(ctx, "task-1")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got == nil {
			t.Fatal("GetTask returned nil")
		}
		if got.State != "submitted" {
			t.Errorf("State = %q, want %q", got.State, "submitted")
		}
		if got.AgentSlug != "agent-1" {
			t.Errorf("AgentSlug = %q, want %q", got.AgentSlug, "agent-1")
		}

		changed, err := s.UpdateTaskState(ctx, "task-1", "working")
		if err != nil {
			t.Fatalf("UpdateTaskState: %v", err)
		}
		if !changed {
			t.Error("UpdateTaskState changed = false, want true")
		}

		got, err = s.GetTask(ctx, "task-1")
		if err != nil {
			t.Fatalf("GetTask after update: %v", err)
		}
		if got.State != "working" {
			t.Errorf("State = %q, want %q", got.State, "working")
		}

		// Not found.
		got, err = s.GetTask(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("GetTask nonexistent: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for nonexistent task, got %+v", got)
		}
	})

	t.Run("ListTasksByContext", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		for _, id := range []string{"t1", "t2", "t3"} {
			s.CreateTask(ctx, &Task{
				ID: id, ContextID: "ctx-a", ProjectID: "g1", AgentSlug: "a1",
				State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
			})
		}
		s.CreateTask(ctx, &Task{
			ID: "t4", ContextID: "ctx-b", ProjectID: "g1", AgentSlug: "a1",
			State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})

		tasks, err := s.ListTasksByContext(ctx, "ctx-a")
		if err != nil {
			t.Fatalf("ListTasksByContext: %v", err)
		}
		if len(tasks) != 3 {
			t.Errorf("got %d tasks, want 3", len(tasks))
		}
	})

	t.Run("ListTasksByAgent", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		s.CreateTask(ctx, &Task{
			ID: "t1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "a1",
			State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})
		s.CreateTask(ctx, &Task{
			ID: "t2", ContextID: "ctx-2", ProjectID: "g1", AgentSlug: "a2",
			State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})

		tasks, err := s.ListTasksByAgent(ctx, "g1", "a1")
		if err != nil {
			t.Fatalf("ListTasksByAgent: %v", err)
		}
		if len(tasks) != 1 {
			t.Errorf("got %d tasks, want 1", len(tasks))
		}
	})

	t.Run("ContextCRUD", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		c := &Context{
			ContextID:  "ctx-1",
			ProjectID:  "grove-1",
			AgentSlug:  "agent-1",
			AgentID:    "agent-id-1",
			CreatedAt:  now,
			LastActive: now,
		}

		if err := s.CreateContext(ctx, c); err != nil {
			t.Fatalf("CreateContext: %v", err)
		}

		got, err := s.GetContext(ctx, "ctx-1")
		if err != nil {
			t.Fatalf("GetContext: %v", err)
		}
		if got == nil {
			t.Fatal("GetContext returned nil")
		}
		if got.AgentSlug != "agent-1" {
			t.Errorf("AgentSlug = %q, want %q", got.AgentSlug, "agent-1")
		}

		if err := s.TouchContext(ctx, "ctx-1"); err != nil {
			t.Fatalf("TouchContext: %v", err)
		}

		got, err = s.GetContext(ctx, "ctx-1")
		if err != nil {
			t.Fatalf("GetContext after touch: %v", err)
		}
		if !got.LastActive.After(now.Add(-time.Second)) {
			t.Error("LastActive was not updated")
		}
	})

	t.Run("PushNotificationConfig", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		// Create parent task first (FK constraint).
		s.CreateTask(ctx, &Task{
			ID: "task-1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "a1",
			State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})

		cfg := &PushNotificationConfig{
			ID:        "push-1",
			TaskID:    "task-1",
			URL:       "https://example.com/webhook",
			Token:     "tok123",
			CreatedAt: now,
		}
		if err := s.SetPushConfig(ctx, cfg); err != nil {
			t.Fatalf("SetPushConfig: %v", err)
		}

		configs, err := s.GetPushConfigsByTask(ctx, "task-1")
		if err != nil {
			t.Fatalf("GetPushConfigsByTask: %v", err)
		}
		if len(configs) != 1 {
			t.Fatalf("got %d configs, want 1", len(configs))
		}
		if configs[0].URL != "https://example.com/webhook" {
			t.Errorf("URL = %q, want %q", configs[0].URL, "https://example.com/webhook")
		}

		if err := s.DeletePushConfig(ctx, "push-1"); err != nil {
			t.Fatalf("DeletePushConfig: %v", err)
		}

		configs, err = s.GetPushConfigsByTask(ctx, "task-1")
		if err != nil {
			t.Fatalf("GetPushConfigsByTask after delete: %v", err)
		}
		if len(configs) != 0 {
			t.Errorf("got %d configs, want 0", len(configs))
		}
	})

	t.Run("UpdateTaskStateCAS", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		s.CreateTask(ctx, &Task{
			ID: "cas-1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "a1",
			State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})

		// First terminal update should succeed.
		changed, err := s.UpdateTaskState(ctx, "cas-1", "completed")
		if err != nil {
			t.Fatalf("UpdateTaskState (first): %v", err)
		}
		if !changed {
			t.Error("first terminal update: changed = false, want true")
		}

		// Second update to a terminal state should be a no-op.
		changed, err = s.UpdateTaskState(ctx, "cas-1", "failed")
		if err != nil {
			t.Fatalf("UpdateTaskState (second): %v", err)
		}
		if changed {
			t.Error("second terminal update: changed = true, want false (task already terminal)")
		}

		// Verify state is still completed.
		task, err := s.GetTask(ctx, "cas-1")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.State != "completed" {
			t.Errorf("State = %q, want %q", task.State, "completed")
		}
	})

	t.Run("FindActiveTaskForAgent", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		// No tasks at all — should return nil.
		task, err := s.FindActiveTaskForAgent(ctx, "g1", "agent-x")
		if err != nil {
			t.Fatalf("FindActiveTaskForAgent (empty): %v", err)
		}
		if task != nil {
			t.Errorf("expected nil for no tasks, got %+v", task)
		}

		// Create one active and one terminal task.
		s.CreateTask(ctx, &Task{
			ID: "active-1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "agent-x",
			State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})
		s.CreateTask(ctx, &Task{
			ID: "done-1", ContextID: "ctx-2", ProjectID: "g1", AgentSlug: "agent-x",
			State: "completed", CreatedAt: now, UpdatedAt: now.Add(-time.Hour), Metadata: "{}",
		})

		task, err = s.FindActiveTaskForAgent(ctx, "g1", "agent-x")
		if err != nil {
			t.Fatalf("FindActiveTaskForAgent: %v", err)
		}
		if task == nil {
			t.Fatal("FindActiveTaskForAgent returned nil, expected active task")
		}
		if task.ID != "active-1" {
			t.Errorf("found task ID = %q, want %q", task.ID, "active-1")
		}
	})

	t.Run("ListStaleActiveTasks", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		now := time.Now().Truncate(time.Second)

		// Create a stale active task (updated long ago).
		staleTime := now.Add(-2 * time.Hour)
		s.CreateTask(ctx, &Task{
			ID: "stale-1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "a1",
			State: "working", CreatedAt: staleTime, UpdatedAt: staleTime, Metadata: "{}",
		})
		// Create a recent active task.
		s.CreateTask(ctx, &Task{
			ID: "recent-1", ContextID: "ctx-2", ProjectID: "g1", AgentSlug: "a1",
			State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})
		// Create a terminal task (should never be returned).
		s.CreateTask(ctx, &Task{
			ID: "done-2", ContextID: "ctx-3", ProjectID: "g1", AgentSlug: "a1",
			State: "completed", CreatedAt: staleTime, UpdatedAt: staleTime, Metadata: "{}",
		})

		cutoff := now.Add(-time.Hour)
		tasks, err := s.ListStaleActiveTasks(ctx, cutoff, 10)
		if err != nil {
			t.Fatalf("ListStaleActiveTasks: %v", err)
		}
		if len(tasks) != 1 {
			t.Fatalf("got %d stale tasks, want 1", len(tasks))
		}
		if tasks[0].ID != "stale-1" {
			t.Errorf("stale task ID = %q, want %q", tasks[0].ID, "stale-1")
		}
	})

	t.Run("AppendAndReadTaskEvents", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		ev1 := &TaskEvent{
			TaskID:  "task-ev-1",
			Kind:    "status",
			Payload: json.RawMessage(`{"state":"working"}`),
			Final:   false,
		}
		id1, err := s.AppendTaskEvent(ctx, ev1)
		if err != nil {
			t.Fatalf("AppendTaskEvent: %v", err)
		}
		if id1 == 0 {
			t.Error("AppendTaskEvent returned id=0 for non-dedup insert")
		}

		ev2 := &TaskEvent{
			TaskID:  "task-ev-1",
			Kind:    "status",
			Payload: json.RawMessage(`{"state":"completed"}`),
			Final:   true,
		}
		id2, err := s.AppendTaskEvent(ctx, ev2)
		if err != nil {
			t.Fatalf("AppendTaskEvent (2): %v", err)
		}
		if id2 <= id1 {
			t.Errorf("second event id=%d should be > first id=%d", id2, id1)
		}

		events, err := s.ReadTaskEvents(ctx, "task-ev-1", 0, 100)
		if err != nil {
			t.Fatalf("ReadTaskEvents: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2", len(events))
		}
		if events[0].Kind != "status" || events[1].Final != true {
			t.Errorf("unexpected event data: %+v", events)
		}

		// Read with afterID should skip the first event.
		events2, err := s.ReadTaskEvents(ctx, "task-ev-1", id1, 100)
		if err != nil {
			t.Fatalf("ReadTaskEvents with afterID: %v", err)
		}
		if len(events2) != 1 {
			t.Fatalf("got %d events after id1, want 1", len(events2))
		}
		if events2[0].ID != id2 {
			t.Errorf("event ID = %d, want %d", events2[0].ID, id2)
		}
	})

	t.Run("AppendTaskEventDedup", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		ev := &TaskEvent{
			TaskID:   "dedup-task-1",
			Kind:     "status",
			Payload:  json.RawMessage(`{"state":"working"}`),
			Final:    false,
			DedupKey: "unique-key-1",
		}
		id1, err := s.AppendTaskEvent(ctx, ev)
		if err != nil {
			t.Fatalf("AppendTaskEvent (dedup first): %v", err)
		}
		if id1 == 0 {
			t.Error("first dedup insert returned id=0")
		}

		// Duplicate insert should be silently ignored.
		id2, err := s.AppendTaskEvent(ctx, ev)
		if err != nil {
			t.Fatalf("AppendTaskEvent (dedup second): %v", err)
		}
		if id2 != 0 {
			t.Errorf("duplicate dedup insert returned id=%d, want 0", id2)
		}

		events, err := s.ReadTaskEvents(ctx, "dedup-task-1", 0, 100)
		if err != nil {
			t.Fatalf("ReadTaskEvents: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1 (dedup should prevent duplicate)", len(events))
		}
	})

	t.Run("PurgeTaskEvents", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		// Insert events — they'll have created_at = now.
		s.AppendTaskEvent(ctx, &TaskEvent{
			TaskID: "purge-task-1", Kind: "status",
			Payload: json.RawMessage(`{}`), Final: false,
		})

		// Purge events older than the future — should delete everything.
		n, err := s.PurgeTaskEvents(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("PurgeTaskEvents: %v", err)
		}
		if n != 1 {
			t.Errorf("PurgeTaskEvents deleted %d, want 1", n)
		}

		events, _ := s.ReadTaskEvents(ctx, "purge-task-1", 0, 100)
		if len(events) != 0 {
			t.Errorf("got %d events after purge, want 0", len(events))
		}
	})

	t.Run("Ping", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
}

func TestSQLiteStore(t *testing.T) {
	runStoreTests(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t)
	})
}

func TestPostgresStore(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	runStoreTests(t, func(t *testing.T) Store {
		return newTestPostgresStore(t)
	})
}

// TestMigrateIdempotent verifies that running NewSQLite twice on the same DB
// does not fail (schema migrations are idempotent).
func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("first NewSQLite: %v", err)
	}
	s1.Close()

	s2, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("second NewSQLite (idempotent migration): %v", err)
	}
	s2.Close()
}

// TestNewInvalidPath verifies that NewSQLite returns an error for a bad path.
func TestNewInvalidPath(t *testing.T) {
	_, err := NewSQLite(filepath.Join(os.TempDir(), "nonexistent-dir-abc123", "subdir", "test.db"))
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}
