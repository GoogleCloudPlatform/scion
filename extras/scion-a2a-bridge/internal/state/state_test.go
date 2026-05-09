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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTaskCRUD(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	task := &Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		ProjectID:   "grove-1",
		AgentSlug: "agent-1",
		AgentID:   "agent-id-1",
		State:     "submitted",
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}

	if err := s.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.GetTask("task-1")
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

	if err := s.UpdateTaskState("task-1", "working"); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}

	got, err = s.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if got.State != "working" {
		t.Errorf("State = %q, want %q", got.State, "working")
	}

	// Not found.
	got, err = s.GetTask("nonexistent")
	if err != nil {
		t.Fatalf("GetTask nonexistent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent task, got %+v", got)
	}
}

func TestListTasksByContext(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	for _, id := range []string{"t1", "t2", "t3"} {
		s.CreateTask(&Task{
			ID: id, ContextID: "ctx-a", ProjectID: "g1", AgentSlug: "a1",
			State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
		})
	}
	s.CreateTask(&Task{
		ID: "t4", ContextID: "ctx-b", ProjectID: "g1", AgentSlug: "a1",
		State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	tasks, err := s.ListTasksByContext(context.Background(), "ctx-a")
	if err != nil {
		t.Fatalf("ListTasksByContext: %v", err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestListTasksByAgent(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	s.CreateTask(&Task{
		ID: "t1", ContextID: "ctx-1", ProjectID: "g1", AgentSlug: "a1",
		State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	s.CreateTask(&Task{
		ID: "t2", ContextID: "ctx-2", ProjectID: "g1", AgentSlug: "a2",
		State: "submitted", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	tasks, err := s.ListTasksByAgent("g1", "a1")
	if err != nil {
		t.Fatalf("ListTasksByAgent: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want 1", len(tasks))
	}
}

func TestContextCRUD(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	ctx := &Context{
		ContextID:  "ctx-1",
		ProjectID:    "grove-1",
		AgentSlug:  "agent-1",
		AgentID:    "agent-id-1",
		CreatedAt:  now,
		LastActive: now,
	}

	if err := s.CreateContext(ctx); err != nil {
		t.Fatalf("CreateContext: %v", err)
	}

	got, err := s.GetContext("ctx-1")
	if err != nil {
		t.Fatalf("GetContext: %v", err)
	}
	if got == nil {
		t.Fatal("GetContext returned nil")
	}
	if got.AgentSlug != "agent-1" {
		t.Errorf("AgentSlug = %q, want %q", got.AgentSlug, "agent-1")
	}

	if err := s.TouchContext("ctx-1"); err != nil {
		t.Fatalf("TouchContext: %v", err)
	}

	got, err = s.GetContext("ctx-1")
	if err != nil {
		t.Fatalf("GetContext after touch: %v", err)
	}
	if !got.LastActive.After(now.Add(-time.Second)) {
		t.Error("LastActive was not updated")
	}
}

func TestPushNotificationConfig(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	// Create parent task first (FK constraint).
	s.CreateTask(&Task{
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
	if err := s.SetPushConfig(cfg); err != nil {
		t.Fatalf("SetPushConfig: %v", err)
	}

	configs, err := s.GetPushConfigsByTask("task-1")
	if err != nil {
		t.Fatalf("GetPushConfigsByTask: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(configs))
	}
	if configs[0].URL != "https://example.com/webhook" {
		t.Errorf("URL = %q, want %q", configs[0].URL, "https://example.com/webhook")
	}

	if err := s.DeletePushConfig("push-1"); err != nil {
		t.Fatalf("DeletePushConfig: %v", err)
	}

	configs, err = s.GetPushConfigsByTask("task-1")
	if err != nil {
		t.Fatalf("GetPushConfigsByTask after delete: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("got %d configs, want 0", len(configs))
	}
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s1, err := New(dbPath)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	s1.Close()

	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("second New (idempotent migration): %v", err)
	}
	s2.Close()
}

func TestNewInvalidPath(t *testing.T) {
	_, err := New(filepath.Join(os.TempDir(), "nonexistent-dir-abc123", "subdir", "test.db"))
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}
