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
	"time"
)

// Store is the persistence interface for the A2A bridge. Both SQLiteStore
// (plugin mode) and PostgresStore (standalone / HA mode) implement it.
type Store interface {
	// Tasks
	CreateTask(ctx context.Context, t *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	TouchTask(ctx context.Context, id string) error
	UpdateTaskState(ctx context.Context, id, state string) (changed bool, err error)
	ListTasksByContext(ctx context.Context, contextID string) ([]Task, error)
	ListTasksByAgent(ctx context.Context, projectID, agentSlug string) ([]Task, error)
	ListTasksByContextAndCaller(ctx context.Context, contextID, callerUserID string) ([]Task, error)

	// Correlation
	FindActiveTaskForAgent(ctx context.Context, projectID, agentSlug string) (*Task, error)
	ListStaleActiveTasks(ctx context.Context, olderThan time.Time, limit int) ([]Task, error)

	// Contexts
	CreateContext(ctx context.Context, c *Context) error
	GetContext(ctx context.Context, contextID string) (*Context, error)
	TouchContext(ctx context.Context, contextID string) error

	// Push notification configs
	SetPushConfig(ctx context.Context, cfg *PushNotificationConfig) error
	GetPushConfigsByTask(ctx context.Context, taskID string) ([]PushNotificationConfig, error)
	DeletePushConfig(ctx context.Context, id string) error
	DeletePushConfigForTask(ctx context.Context, taskID, id string) error

	// Task event log (Phase 2 will use these; defined and stubbed for now)
	AppendTaskEvent(ctx context.Context, ev *TaskEvent) (id int64, err error)
	ReadTaskEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)
	PurgeTaskEvents(ctx context.Context, olderThan time.Time) (int64, error)

	// Lifecycle
	Close() error
	Ping(ctx context.Context) error
}

// TaskEvent represents an entry in the task event log.
// Phase 2 streaming uses these for durable, ordered event delivery.
type TaskEvent struct {
	ID        int64
	TaskID    string
	Kind      string // "status" | "artifact" | "message"
	Payload   json.RawMessage
	Final     bool
	DedupKey  string
	CreatedAt time.Time
}
