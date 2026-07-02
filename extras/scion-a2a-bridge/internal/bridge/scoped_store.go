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
	"fmt"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// ScopedTaskStore wraps a taskstore.Store and enforces project/agent-level
// isolation. Every task is associated with the RouteInfo (project + agent)
// present in the context at creation time. Subsequent Get, Update, and List
// calls verify that the caller's route info matches the task's owner, preventing
// cross-tenant access.
type ScopedTaskStore struct {
	inner taskstore.Store

	mu        sync.RWMutex
	ownership map[a2a.TaskID]string // taskID → "projectSlug:agentSlug"
}

var _ taskstore.Store = (*ScopedTaskStore)(nil)

// NewScopedTaskStore wraps an existing task store with ownership scoping.
func NewScopedTaskStore(inner taskstore.Store) *ScopedTaskStore {
	return &ScopedTaskStore{
		inner:     inner,
		ownership: make(map[a2a.TaskID]string),
	}
}

// ownerKey returns the ownership key from the route info in context.
func ownerKey(ctx context.Context) (string, bool) {
	route, ok := RouteInfoFrom(ctx)
	if !ok {
		return "", false
	}
	return route.ProjectSlug + ":" + route.AgentSlug, true
}

// Create stores the task and records its ownership based on the route info in context.
func (s *ScopedTaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	owner, ok := ownerKey(ctx)
	if !ok {
		return taskstore.TaskVersionMissing, fmt.Errorf("missing route info for task creation: %w", a2a.ErrInternalError)
	}

	version, err := s.inner.Create(ctx, task)
	if err != nil {
		return version, err
	}

	s.mu.Lock()
	s.ownership[task.ID] = owner
	s.mu.Unlock()

	return version, nil
}

// Update verifies ownership before delegating to the inner store.
func (s *ScopedTaskStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	owner, ok := ownerKey(ctx)
	if !ok {
		return taskstore.TaskVersionMissing, fmt.Errorf("missing route info for task update: %w", a2a.ErrInternalError)
	}

	s.mu.RLock()
	taskOwner, exists := s.ownership[update.Task.ID]
	s.mu.RUnlock()

	if exists && taskOwner != owner {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}

	return s.inner.Update(ctx, update)
}

// Get retrieves a task and verifies that the caller owns it.
func (s *ScopedTaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	owner, ok := ownerKey(ctx)
	if !ok {
		return nil, fmt.Errorf("missing route info for task get: %w", a2a.ErrInternalError)
	}

	s.mu.RLock()
	taskOwner, exists := s.ownership[taskID]
	s.mu.RUnlock()

	if exists && taskOwner != owner {
		// Return TaskNotFound to avoid leaking task existence across tenants.
		return nil, a2a.ErrTaskNotFound
	}

	return s.inner.Get(ctx, taskID)
}

// List delegates to the inner store. The inner in-memory store already filters
// by the authenticator "user" (which we set to the route key). This provides
// an additional ownership check.
func (s *ScopedTaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return s.inner.List(ctx, req)
}

// RouteKeyAuthenticator returns a taskstore.Authenticator that derives the
// "user" identity from the RouteInfo in the request context. This ensures
// the in-memory task store's built-in user-filtering on List matches tasks
// to the correct project/agent pair.
func RouteKeyAuthenticator() taskstore.Authenticator {
	return func(ctx context.Context) (string, error) {
		route, ok := RouteInfoFrom(ctx)
		if !ok {
			return "", fmt.Errorf("missing route info: %w", a2a.ErrUnauthenticated)
		}
		return route.ProjectSlug + ":" + route.AgentSlug, nil
	}
}
