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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func newScopedStore(t *testing.T) *ScopedTaskStore {
	t.Helper()
	auth := RouteKeyAuthenticator()
	inner := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: auth,
	})
	return NewScopedTaskStore(inner)
}

func ctxForRoute(project, agent string) context.Context {
	return WithRouteInfo(context.Background(), RouteInfo{
		ProjectSlug: project,
		AgentSlug:   agent,
	})
}

// ctxForRouteAndCaller creates a context with both route info and a caller identity,
// simulating a per-user auth request (hubJWT/hubUAT).
func ctxForRouteAndCaller(project, agent, userID string) context.Context {
	ctx := ctxForRoute(project, agent)
	return withCallerIdentity(ctx, &CallerIdentity{UserID: userID})
}

func TestScopedStoreCreateAndGet(t *testing.T) {
	store := newScopedStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	task := &a2a.Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}

	_, err := store.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Same owner can Get.
	stored, err := store.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get (same owner): %v", err)
	}
	if stored.Task.ID != "task-1" {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, "task-1")
	}
}

func TestScopedStoreGetDeniedCrossTenant(t *testing.T) {
	store := newScopedStore(t)
	ctxA := ctxForRoute("proj-a", "agent-1")
	ctxB := ctxForRoute("proj-b", "agent-2")

	task := &a2a.Task{
		ID:        "task-cross",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxA, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Different owner should get TaskNotFound.
	_, err := store.Get(ctxB, "task-cross")
	if err == nil {
		t.Fatal("expected error for cross-tenant Get")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestScopedStoreUpdateDeniedCrossTenant(t *testing.T) {
	store := newScopedStore(t)
	ctxA := ctxForRoute("proj-a", "agent-1")
	ctxB := ctxForRoute("proj-b", "agent-2")

	task := &a2a.Task{
		ID:        "task-update",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	version, err := store.Create(ctxA, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Different owner should fail to Update.
	updatedTask := &a2a.Task{
		ID:        "task-update",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
	}
	_, err = store.Update(ctxB, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: version,
	})
	if err == nil {
		t.Fatal("expected error for cross-tenant Update")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestScopedStoreCreateRequiresRouteInfo(t *testing.T) {
	store := newScopedStore(t)
	ctx := context.Background() // No route info.

	task := &a2a.Task{
		ID:        "task-noroute",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	_, err := store.Create(ctx, task)
	if err == nil {
		t.Fatal("expected error when route info is missing")
	}
}

func TestScopedStoreGetRequiresRouteInfo(t *testing.T) {
	store := newScopedStore(t)
	ctx := ctxForRoute("proj-a", "agent-1")

	task := &a2a.Task{
		ID:        "task-getnoroute",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get without route info should fail.
	_, err := store.Get(context.Background(), "task-getnoroute")
	if err == nil {
		t.Fatal("expected error when route info is missing for Get")
	}
}

// --- Empty UserID edge case: fail-closed ---

func TestScopedStoreEmptyUserIDRejected(t *testing.T) {
	// When CallerIdentity is present but UserID is empty, the request must be
	// rejected (fail-closed). Falling back to route-only keying would silently
	// restore the cross-user vulnerability that per-user scoping is meant to fix.
	store := newScopedStore(t)
	ctxEmptyUser := ctxForRouteAndCaller("proj-a", "agent-1", "")

	t.Run("Create rejected", func(t *testing.T) {
		task := &a2a.Task{
			ID:        "task-empty-uid",
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		}
		_, err := store.Create(ctxEmptyUser, task)
		if err == nil {
			t.Fatal("expected error when CallerIdentity has empty UserID")
		}
		if !errors.Is(err, a2a.ErrUnauthenticated) {
			t.Errorf("error = %v, want ErrUnauthenticated in chain", err)
		}
	})

	t.Run("Get rejected", func(t *testing.T) {
		// First create a task with a valid context so there's something to Get.
		ctxValid := ctxForRouteAndCaller("proj-a", "agent-1", "valid-user")
		task := &a2a.Task{
			ID:        "task-valid-for-get",
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		}
		if _, err := store.Create(ctxValid, task); err != nil {
			t.Fatalf("Create (valid user): %v", err)
		}

		// Getting with empty UserID must fail.
		_, err := store.Get(ctxEmptyUser, "task-valid-for-get")
		if err == nil {
			t.Fatal("expected error when CallerIdentity has empty UserID")
		}
		if !errors.Is(err, a2a.ErrUnauthenticated) {
			t.Errorf("error = %v, want ErrUnauthenticated in chain", err)
		}
	})

	t.Run("Update rejected", func(t *testing.T) {
		ctxValid := ctxForRouteAndCaller("proj-a", "agent-1", "valid-user")
		task := &a2a.Task{
			ID:        "task-valid-for-upd",
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		}
		version, err := store.Create(ctxValid, task)
		if err != nil {
			t.Fatalf("Create (valid user): %v", err)
		}

		// Updating with empty UserID must fail.
		updatedTask := &a2a.Task{
			ID:        "task-valid-for-upd",
			ContextID: "ctx-1",
			Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
		}
		_, err = store.Update(ctxEmptyUser, &taskstore.UpdateRequest{
			Task:        updatedTask,
			PrevVersion: version,
		})
		if err == nil {
			t.Fatal("expected error when CallerIdentity has empty UserID")
		}
		if !errors.Is(err, a2a.ErrUnauthenticated) {
			t.Errorf("error = %v, want ErrUnauthenticated in chain", err)
		}
	})

	t.Run("List rejected", func(t *testing.T) {
		// List goes through RouteKeyAuthenticator which also calls buildOwnerKey.
		_, err := store.List(ctxEmptyUser, &a2a.ListTasksRequest{ContextID: "ctx-1"})
		if err == nil {
			t.Fatal("expected error when CallerIdentity has empty UserID")
		}
		if !errors.Is(err, a2a.ErrUnauthenticated) {
			t.Errorf("error = %v, want ErrUnauthenticated in chain", err)
		}
	})
}

// --- Cross-user isolation tests (per-user auth: hubJWT/hubUAT) ---

func TestScopedStoreCrossUserGetDenied(t *testing.T) {
	store := newScopedStore(t)
	ctxAlice := ctxForRouteAndCaller("proj-a", "agent-1", "alice-id")
	ctxBob := ctxForRouteAndCaller("proj-a", "agent-1", "bob-id")

	task := &a2a.Task{
		ID:        "task-alice",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxAlice, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Alice can read her own task.
	stored, err := store.Get(ctxAlice, "task-alice")
	if err != nil {
		t.Fatalf("Get (Alice, own task): %v", err)
	}
	if stored.Task.ID != "task-alice" {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, "task-alice")
	}

	// Bob on the same route must NOT see Alice's task.
	_, err = store.Get(ctxBob, "task-alice")
	if err == nil {
		t.Fatal("expected error for cross-user Get on same route")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestScopedStoreCrossUserUpdateDenied(t *testing.T) {
	store := newScopedStore(t)
	ctxAlice := ctxForRouteAndCaller("proj-a", "agent-1", "alice-id")
	ctxBob := ctxForRouteAndCaller("proj-a", "agent-1", "bob-id")

	task := &a2a.Task{
		ID:        "task-alice-upd",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	version, err := store.Create(ctxAlice, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Bob must not be able to update Alice's task.
	updatedTask := &a2a.Task{
		ID:        "task-alice-upd",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateFailed},
	}
	_, err = store.Update(ctxBob, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: version,
	})
	if err == nil {
		t.Fatal("expected error for cross-user Update on same route")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestScopedStoreCrossUserListIsolated(t *testing.T) {
	store := newScopedStore(t)
	ctxAlice := ctxForRouteAndCaller("proj-a", "agent-1", "alice-id")
	ctxBob := ctxForRouteAndCaller("proj-a", "agent-1", "bob-id")

	// Alice creates a task.
	aliceTask := &a2a.Task{
		ID:        "task-alice-list",
		ContextID: "ctx-shared",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxAlice, aliceTask); err != nil {
		t.Fatalf("Create (Alice): %v", err)
	}

	// Bob creates a task on the same route and context.
	bobTask := &a2a.Task{
		ID:        "task-bob-list",
		ContextID: "ctx-shared",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxBob, bobTask); err != nil {
		t.Fatalf("Create (Bob): %v", err)
	}

	// Alice's ListTasks should return only her task.
	aliceResp, err := store.List(ctxAlice, &a2a.ListTasksRequest{ContextID: "ctx-shared"})
	if err != nil {
		t.Fatalf("List (Alice): %v", err)
	}
	if len(aliceResp.Tasks) != 1 || aliceResp.Tasks[0].ID != "task-alice-list" {
		var ids []string
		for _, tk := range aliceResp.Tasks {
			ids = append(ids, string(tk.ID))
		}
		t.Errorf("Alice's List returned tasks %v, want [task-alice-list]", ids)
	}

	// Bob's ListTasks should return only his task.
	bobResp, err := store.List(ctxBob, &a2a.ListTasksRequest{ContextID: "ctx-shared"})
	if err != nil {
		t.Fatalf("List (Bob): %v", err)
	}
	if len(bobResp.Tasks) != 1 || bobResp.Tasks[0].ID != "task-bob-list" {
		var ids []string
		for _, tk := range bobResp.Tasks {
			ids = append(ids, string(tk.ID))
		}
		t.Errorf("Bob's List returned tasks %v, want [task-bob-list]", ids)
	}
}

func TestScopedStorePerUserAuthOwnTasksWork(t *testing.T) {
	store := newScopedStore(t)
	ctxAlice := ctxForRouteAndCaller("proj-a", "agent-1", "alice-id")

	task := &a2a.Task{
		ID:        "task-alice-own",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	version, err := store.Create(ctxAlice, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Alice can Get her own task.
	stored, err := store.Get(ctxAlice, "task-alice-own")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Task.ID != "task-alice-own" {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, "task-alice-own")
	}

	// Alice can Update her own task.
	updatedTask := &a2a.Task{
		ID:        "task-alice-own",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	_, err = store.Update(ctxAlice, &taskstore.UpdateRequest{
		Task:        updatedTask,
		PrevVersion: version,
	})
	if err != nil {
		t.Fatalf("Update (own task): %v", err)
	}
}

func TestScopedStoreNoCallerIdentityFallback(t *testing.T) {
	// When no CallerIdentity is in context (legacy apiKey/bearer/none auth),
	// the route-only keying should still work as before.
	store := newScopedStore(t)
	ctxA := ctxForRoute("proj-a", "agent-1") // No CallerIdentity
	ctxB := ctxForRoute("proj-a", "agent-1") // Same route, also no CallerIdentity

	task := &a2a.Task{
		ID:        "task-legacy",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	if _, err := store.Create(ctxA, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Both contexts (same route, no caller identity) should see the task.
	stored, err := store.Get(ctxB, "task-legacy")
	if err != nil {
		t.Fatalf("Get (same route, no caller): %v", err)
	}
	if stored.Task.ID != "task-legacy" {
		t.Errorf("task ID = %q, want %q", stored.Task.ID, "task-legacy")
	}

	// But a different route still cannot see it.
	ctxOther := ctxForRoute("proj-b", "agent-2")
	_, err = store.Get(ctxOther, "task-legacy")
	if err == nil {
		t.Fatal("expected error for different-route Get")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}
