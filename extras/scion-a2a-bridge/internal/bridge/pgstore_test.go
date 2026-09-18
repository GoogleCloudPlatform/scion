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

package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// testPostgresTaskStore creates a PostgresTaskStore for testing.
// Skips the test if TEST_DATABASE_URL is not set.
// Each test using this helper is responsible for its own scoped cleanup
// of created rows. The helper only manages the connection lifecycle.
func testPostgresTaskStore(t *testing.T) *PostgresTaskStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real Postgres test")
	}
	store, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})
	return store
}

// TestPostgresTaskStoreCreateAndGet verifies basic CRUD on real Postgres.
func TestPostgresTaskStoreCreateAndGet(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-task-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}

	version, err := store.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}

	// Same owner can Get.
	stored, err := store.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Task.ID != a2a.TaskID(taskID) {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, taskID)
	}
	if stored.Version != 1 {
		t.Errorf("version = %d, want 1", stored.Version)
	}
	if stored.Task.Status.State != a2a.TaskStateSubmitted {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateSubmitted)
	}
}

// TestPostgresTaskStoreDuplicateCreate verifies ErrTaskAlreadyExists.
func TestPostgresTaskStoreDuplicateCreate(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-dup-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}

	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := store.Create(ctx, task)
	if !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Errorf("error = %v, want ErrTaskAlreadyExists", err)
	}
}

// TestPostgresTaskStoreUpdateCAS verifies optimistic concurrency.
func TestPostgresTaskStoreUpdateCAS(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-cas-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	v1, err := store.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update with correct version.
	updatedTask := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	v2, err := store.Update(ctx, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: v1,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if v2 <= v1 {
		t.Errorf("new version %d should be > %d", v2, v1)
	}

	// Update with stale version should fail.
	_, err = store.Update(ctx, &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID:        a2a.TaskID(taskID),
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
		},
		PrevVersion: v1, // stale
	})
	if !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Errorf("error = %v, want ErrConcurrentModification", err)
	}

	// Verify state didn't change.
	stored, _ := store.Get(ctx, a2a.TaskID(taskID))
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("state = %q, want %q (CAS should have rejected stale update)", stored.Task.Status.State, a2a.TaskStateWorking)
	}
}

// TestPostgresTaskStoreCrossReplicaCreateReadList simulates cross-replica
// behavior: task created on replica A (same DB) is readable from replica B.
func TestPostgresTaskStoreCrossReplicaCreateReadList(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "cross-replica-" + suffix
	ctxID := "ctx-shared-" + suffix

	// Two separate stores simulating two replicas sharing the same Postgres.
	storeA, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore (A): %v", err)
	}
	t.Cleanup(func() {
		storeA.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
		storeA.Close()
	})

	storeB, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("NewPostgresTaskStore (B): %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	ctx := ctxForRoute("proj-x", "agent-x")

	// Create on replica A.
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: ctxID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		History: []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Hello from replica A")),
		},
	}
	if _, err := storeA.Create(ctx, task); err != nil {
		t.Fatalf("Create on A: %v", err)
	}

	// Read from replica B.
	stored, err := storeB.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get on B: %v", err)
	}
	if stored.Task.ID != a2a.TaskID(taskID) {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, taskID)
	}
	if len(stored.Task.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(stored.Task.History))
	}
	if string(stored.Task.History[0].Parts[0].Content.(a2a.Text)) != "Hello from replica A" {
		t.Errorf("history text mismatch")
	}

	// List from replica B.
	listResp, err := storeB.List(ctx, &a2a.ListTasksRequest{ContextID: ctxID})
	if err != nil {
		t.Fatalf("List on B: %v", err)
	}
	if len(listResp.Tasks) != 1 {
		t.Errorf("list count = %d, want 1", len(listResp.Tasks))
	}
}

// TestPostgresTaskStoreCrossReplicaCancelContinue simulates follow-up
// and cancel operations from a different replica.
func TestPostgresTaskStoreCrossReplicaCancelContinue(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "cross-cancel-" + suffix

	storeA, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("storeA: %v", err)
	}
	t.Cleanup(func() {
		storeA.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
		storeA.Close()
	})

	storeB, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("storeB: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	ctx := ctxForRoute("proj-y", "agent-y")

	// Create task on A.
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-cc",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	v1, err := storeA.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Continue (update) from B.
	updatedTask := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-cc",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
	v2, err := storeB.Update(ctx, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: v1,
	})
	if err != nil {
		t.Fatalf("Update from B: %v", err)
	}

	// Cancel from A (update to canceled state).
	cancelTask := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-cc",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCanceled},
	}
	_, err = storeA.Update(ctx, &taskstore.UpdateRequest{
		Task:        cancelTask,
		PrevVersion: v2,
	})
	if err != nil {
		t.Fatalf("Cancel from A: %v", err)
	}

	// Verify from B.
	stored, _ := storeB.Get(ctx, a2a.TaskID(taskID))
	if stored.Task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateCanceled)
	}
}

// TestPostgresTaskStoreRouteIsolation verifies project/agent isolation.
func TestPostgresTaskStoreRouteIsolation(t *testing.T) {
	store := testPostgresTaskStore(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-iso-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	ctxA := ctxForRoute("proj-a", "agent-1")
	ctxB := ctxForRoute("proj-b", "agent-2")

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxA, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Different route cannot Get.
	_, err := store.Get(ctxB, a2a.TaskID(taskID))
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}

	// Different route cannot Update.
	_, err = store.Update(ctxB, &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID:        a2a.TaskID(taskID),
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
		},
		PrevVersion: 1,
	})
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

// TestPostgresTaskStoreCallerIsolation verifies per-user isolation.
func TestPostgresTaskStoreCallerIsolation(t *testing.T) {
	store := testPostgresTaskStore(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	aliceTaskID := "pg-caller-iso-alice-" + suffix
	bobTaskID := "pg-caller-iso-bob-" + suffix
	ctxID := "ctx-caller-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id IN ($1, $2)`, aliceTaskID, bobTaskID)
	})

	ctxAlice := ctxForRouteAndCaller("proj-a", "agent-1", "alice-id")
	ctxBob := ctxForRouteAndCaller("proj-a", "agent-1", "bob-id")

	// Alice creates a task.
	task := &a2a.Task{
		ID:        a2a.TaskID(aliceTaskID),
		ContextID: ctxID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxAlice, task); err != nil {
		t.Fatalf("Create (Alice): %v", err)
	}

	// Alice can Get.
	stored, err := store.Get(ctxAlice, a2a.TaskID(aliceTaskID))
	if err != nil {
		t.Fatalf("Get (Alice): %v", err)
	}
	if stored.Task.ID != a2a.TaskID(aliceTaskID) {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, aliceTaskID)
	}

	// Bob cannot Get.
	_, err = store.Get(ctxBob, a2a.TaskID(aliceTaskID))
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}

	// Bob creates his own task.
	bobTask := &a2a.Task{
		ID:        a2a.TaskID(bobTaskID),
		ContextID: ctxID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxBob, bobTask); err != nil {
		t.Fatalf("Create (Bob): %v", err)
	}

	// Alice's List should only return her task.
	listResp, err := store.List(ctxAlice, &a2a.ListTasksRequest{ContextID: ctxID})
	if err != nil {
		t.Fatalf("List (Alice): %v", err)
	}
	if len(listResp.Tasks) != 1 || listResp.Tasks[0].ID != a2a.TaskID(aliceTaskID) {
		var ids []string
		for _, tk := range listResp.Tasks {
			ids = append(ids, string(tk.ID))
		}
		t.Errorf("Alice's List = %v, want [%s]", ids, aliceTaskID)
	}

	// Bob's List should only return his task.
	listResp, err = store.List(ctxBob, &a2a.ListTasksRequest{ContextID: ctxID})
	if err != nil {
		t.Fatalf("List (Bob): %v", err)
	}
	if len(listResp.Tasks) != 1 || listResp.Tasks[0].ID != a2a.TaskID(bobTaskID) {
		var ids []string
		for _, tk := range listResp.Tasks {
			ids = append(ids, string(tk.ID))
		}
		t.Errorf("Bob's List = %v, want [%s]", ids, bobTaskID)
	}
}

// TestPostgresTaskStoreConcurrentCAS verifies that concurrent CAS updates
// allow exactly one winner.
func TestPostgresTaskStoreConcurrentCAS(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-cas", "agent-cas")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-conc-cas-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	v1, err := store.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 5 goroutines race to update with the same version.
	const n = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	casErrors := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := store.Update(ctx, &taskstore.UpdateRequest{
				Task: &a2a.Task{
					ID:        a2a.TaskID(taskID),
					ContextID: "ctx-1",
					Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
				},
				PrevVersion: v1,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if errors.Is(err, taskstore.ErrConcurrentModification) {
				casErrors++
			} else {
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("CAS winners = %d, want 1", winners)
	}
	if casErrors != n-1 {
		t.Errorf("CAS errors = %d, want %d", casErrors, n-1)
	}
}

// TestPostgresTaskStoreRestartRecovery simulates replica restart:
// create tasks, close the store, reopen, and verify tasks persist.
func TestPostgresTaskStoreRestartRecovery(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "restart-" + suffix

	// Phase 1: create task.
	store1, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("store1: %v", err)
	}

	ctx := ctxForRoute("proj-restart", "agent-restart")
	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-r",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
		History: []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Persisted message")),
		},
	}
	if _, err := store1.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	store1.Close()

	// Phase 2: reopen (simulating restart) and verify.
	store2, err := NewPostgresTaskStore(url)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	t.Cleanup(func() {
		// Scoped cleanup using the open store2 connection (store1 is closed).
		store2.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
		store2.Close()
	})

	stored, err := store2.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("state = %q, want %q", stored.Task.Status.State, a2a.TaskStateWorking)
	}
	if len(stored.Task.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(stored.Task.History))
	}
}

// TestPostgresTaskStoreUpdateNonexistent verifies that updating a task
// that doesn't exist returns ErrTaskNotFound.
func TestPostgresTaskStoreUpdateNonexistent(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	_, err := store.Update(ctx, &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID:        "nonexistent-pg",
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
		},
		PrevVersion: 1,
	})
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

// TestPostgresTaskStoreGetNonexistent verifies ErrTaskNotFound for missing tasks.
func TestPostgresTaskStoreGetNonexistent(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	_, err := store.Get(ctx, "definitely-not-here")
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

// TestPostgresTaskStoreEmptyCallerRejected verifies fail-closed behavior
// when CallerIdentity is present but UserID is empty.
func TestPostgresTaskStoreEmptyCallerRejected(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctxEmpty := ctxForRouteAndCaller("proj-a", "agent-1", "")

	task := &a2a.Task{
		ID:        "pg-empty-uid",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	_, err := store.Create(ctxEmpty, task)
	if err == nil {
		t.Fatal("expected error for empty UserID")
	}
	if !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("error = %v, want ErrUnauthenticated in chain", err)
	}
}

// TestPostgresTaskStoreNoRouteRejected verifies that operations without
// route info are rejected.
func TestPostgresTaskStoreNoRouteRejected(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := context.Background()

	task := &a2a.Task{
		ID:        "pg-noroute",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	_, err := store.Create(ctx, task)
	if err == nil {
		t.Fatal("expected error for missing route info")
	}
}

// TestPostgresTaskStoreListPagination verifies cursor-based pagination.
func TestPostgresTaskStoreListPagination(t *testing.T) {
	store := testPostgresTaskStore(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ctxID := "ctx-page-" + suffix
	ctx := ctxForRoute("proj-page", "agent-page")

	var taskIDs []string
	// Create 5 tasks.
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("pg-page-%d-%s", i, suffix)
		taskIDs = append(taskIDs, tid)
		task := &a2a.Task{
			ID:        a2a.TaskID(tid),
			ContextID: ctxID,
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		}
		if _, err := store.Create(ctx, task); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	t.Cleanup(func() {
		for _, tid := range taskIDs {
			store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, tid)
		}
	})

	// List first page (2 items).
	resp1, err := store.List(ctx, &a2a.ListTasksRequest{
		ContextID: ctxID,
		PageSize:  2,
	})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(resp1.Tasks) != 2 {
		t.Fatalf("page 1 count = %d, want 2", len(resp1.Tasks))
	}
	if resp1.NextPageToken == "" {
		t.Fatal("expected NextPageToken for page 1")
	}
	if resp1.TotalSize != 5 {
		t.Errorf("total size = %d, want 5", resp1.TotalSize)
	}

	// List second page.
	resp2, err := store.List(ctx, &a2a.ListTasksRequest{
		ContextID: ctxID,
		PageSize:  2,
		PageToken: resp1.NextPageToken,
	})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(resp2.Tasks) != 2 {
		t.Fatalf("page 2 count = %d, want 2", len(resp2.Tasks))
	}

	// List third page (should have 1 item).
	resp3, err := store.List(ctx, &a2a.ListTasksRequest{
		ContextID: ctxID,
		PageSize:  2,
		PageToken: resp2.NextPageToken,
	})
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(resp3.Tasks) != 1 {
		t.Fatalf("page 3 count = %d, want 1", len(resp3.Tasks))
	}
	if resp3.NextPageToken != "" {
		t.Errorf("expected empty NextPageToken for last page, got %q", resp3.NextPageToken)
	}
}

// TestPostgresTaskStoreHistoryPreservation verifies that task history
// and artifacts survive Create and Get roundtrips.
func TestPostgresTaskStoreHistoryPreservation(t *testing.T) {
	store := testPostgresTaskStore(t)
	ctx := ctxForRoute("proj-hist", "agent-hist")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taskID := "pg-hist-" + suffix

	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, taskID)
	})

	task := &a2a.Task{
		ID:        a2a.TaskID(taskID),
		ContextID: "ctx-hist",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		History: []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Question 1")),
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Answer 1")),
			a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Question 2")),
		},
		Artifacts: []*a2a.Artifact{
			{
				ID:    "art-1",
				Parts: a2a.ContentParts{a2a.NewTextPart("Artifact content")},
			},
		},
	}

	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stored, err := store.Get(ctx, a2a.TaskID(taskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(stored.Task.History) != 3 {
		t.Errorf("history len = %d, want 3", len(stored.Task.History))
	}
	if len(stored.Task.Artifacts) != 1 {
		t.Errorf("artifacts len = %d, want 1", len(stored.Task.Artifacts))
	}
}

// TestPostgresTaskStoreCanarySurvival verifies that scoped cleanup in other
// tests does not destroy unrelated rows (canary proof of test isolation).
func TestPostgresTaskStoreCanarySurvival(t *testing.T) {
	store := testPostgresTaskStore(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	canaryID := "canary-task-" + suffix
	testID := "non-canary-" + suffix

	ctx := ctxForRoute("proj-canary", "agent-canary")

	// Insert canary row.
	canary := &a2a.Task{
		ID:        a2a.TaskID(canaryID),
		ContextID: "ctx-canary",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctx, canary); err != nil {
		t.Fatalf("Create canary: %v", err)
	}
	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, canaryID)
	})

	// Insert a separate test row and clean it up with scoped DELETE.
	testTask := &a2a.Task{
		ID:        a2a.TaskID(testID),
		ContextID: "ctx-canary",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctx, testTask); err != nil {
		t.Fatalf("Create test task: %v", err)
	}

	// Scoped cleanup — only deletes testID, not canaryID.
	store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE id = $1`, testID)

	// Canary must survive the scoped cleanup.
	stored, err := store.Get(ctx, a2a.TaskID(canaryID))
	if err != nil {
		t.Fatalf("Canary was destroyed by scoped cleanup! Get error: %v", err)
	}
	if stored.Task.ID != a2a.TaskID(canaryID) {
		t.Errorf("canary ID = %q, want %q", stored.Task.ID, canaryID)
	}
	t.Logf("Canary survived: %s (test row %s was cleaned up)", canaryID, testID)
}
