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
