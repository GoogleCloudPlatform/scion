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
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore provides Postgres-backed state management for the A2A bridge
// in standalone / HA mode.
type PostgresStore struct {
	db *sql.DB
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)

// NewPostgres connects to the Postgres database at databaseURL and runs
// schema migrations. The schema uses the a2a_ prefix on all tables to
// coexist with other hub tables in the same database.
func NewPostgres(databaseURL string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	s := &PostgresStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection pool.
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// Ping checks database connectivity.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *PostgresStore) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS a2a_tasks (
			id TEXT PRIMARY KEY,
			context_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			agent_slug TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			caller_user_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			metadata TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_tasks_context ON a2a_tasks(context_id)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_tasks_agent ON a2a_tasks(project_id, agent_slug)`,

		`CREATE TABLE IF NOT EXISTS a2a_contexts (
			context_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			agent_slug TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_active TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS a2a_push_notification_configs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES a2a_tasks(id),
			url TEXT NOT NULL,
			token TEXT NOT NULL DEFAULT '',
			auth_scheme TEXT NOT NULL DEFAULT '',
			auth_credentials TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_push_task ON a2a_push_notification_configs(task_id)`,

		`CREATE TABLE IF NOT EXISTS a2a_task_events (
			id BIGSERIAL PRIMARY KEY,
			task_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload JSONB NOT NULL,
			final BOOLEAN NOT NULL DEFAULT FALSE,
			dedup_key TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_task_events_task ON a2a_task_events (task_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_task_events_created ON a2a_task_events (created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_a2a_task_events_dedup ON a2a_task_events (task_id, dedup_key) WHERE dedup_key IS NOT NULL`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	return nil
}

// --- Tasks ---

// CreateTask inserts a new task record.
func (s *PostgresStore) CreateTask(ctx context.Context, t *Task) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO a2a_tasks (id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t.ID, t.ContextID, t.ProjectID, t.AgentSlug, t.AgentID, t.State, t.CallerUserID, t.CreatedAt, t.UpdatedAt, t.Metadata,
	)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// GetTask returns a task by ID, or nil if not found.
func (s *PostgresStore) GetTask(ctx context.Context, id string) (*Task, error) {
	t := &Task{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks WHERE id = $1`, id,
	).Scan(&t.ID, &t.ContextID, &t.ProjectID, &t.AgentSlug, &t.AgentID, &t.State, &t.CallerUserID, &t.CreatedAt, &t.UpdatedAt, &t.Metadata)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// TouchTask updates only the updated_at timestamp without changing state.
func (s *PostgresStore) TouchTask(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE a2a_tasks SET updated_at = NOW() WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("touch task: %w", err)
	}
	return nil
}

// UpdateTaskState updates a task's state and updated_at timestamp.
// Terminal states (completed, failed, canceled) are protected.
// Returns changed=true if the row was actually updated (CAS semantics).
func (s *PostgresStore) UpdateTaskState(ctx context.Context, id, state string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE a2a_tasks SET state = $1, updated_at = NOW() WHERE id = $2 AND state NOT IN ('completed', 'failed', 'canceled', 'rejected')`,
		state, id,
	)
	if err != nil {
		return false, fmt.Errorf("update task state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update task state rows affected: %w", err)
	}
	return rows > 0, nil
}

// ListTasksByContext returns all tasks for the given context.
func (s *PostgresStore) ListTasksByContext(ctx context.Context, contextID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks WHERE context_id = $1 ORDER BY created_at DESC`, contextID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by context: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListTasksByAgent returns all tasks for a given project and agent.
func (s *PostgresStore) ListTasksByAgent(ctx context.Context, projectID, agentSlug string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks WHERE project_id = $1 AND agent_slug = $2 ORDER BY created_at DESC`, projectID, agentSlug,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by agent: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListTasksByContextAndCaller returns tasks for the given context filtered by caller user ID.
func (s *PostgresStore) ListTasksByContextAndCaller(ctx context.Context, contextID, callerUserID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks WHERE context_id = $1 AND caller_user_id = $2 ORDER BY created_at DESC`, contextID, callerUserID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by context and caller: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// FindActiveTaskForAgent returns the most recently updated non-terminal task
// for the given project and agent, or nil if none exists.
func (s *PostgresStore) FindActiveTaskForAgent(ctx context.Context, projectID, agentSlug string) (*Task, error) {
	t := &Task{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks
		 WHERE project_id = $1 AND agent_slug = $2 AND state NOT IN ('completed','failed','canceled','rejected')
		 ORDER BY updated_at DESC LIMIT 1`, projectID, agentSlug,
	).Scan(&t.ID, &t.ContextID, &t.ProjectID, &t.AgentSlug, &t.AgentID, &t.State, &t.CallerUserID, &t.CreatedAt, &t.UpdatedAt, &t.Metadata)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active task for agent: %w", err)
	}
	return t, nil
}

// ListStaleActiveTasks returns non-terminal tasks whose updated_at is older
// than olderThan, ordered by updated_at ascending, up to limit rows.
func (s *PostgresStore) ListStaleActiveTasks(ctx context.Context, olderThan time.Time, limit int) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, context_id, project_id, agent_slug, agent_id, state, caller_user_id, created_at, updated_at, metadata
		 FROM a2a_tasks
		 WHERE state NOT IN ('completed','failed','canceled','rejected') AND updated_at < $1
		 ORDER BY updated_at ASC LIMIT $2`, olderThan, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale active tasks: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// --- Contexts ---

// CreateContext inserts a new context mapping.
func (s *PostgresStore) CreateContext(ctx context.Context, c *Context) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO a2a_contexts (context_id, project_id, agent_slug, agent_id, created_at, last_active)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ContextID, c.ProjectID, c.AgentSlug, c.AgentID, c.CreatedAt, c.LastActive,
	)
	if err != nil {
		return fmt.Errorf("create context: %w", err)
	}
	return nil
}

// GetContext returns a context by ID, or nil if not found.
func (s *PostgresStore) GetContext(ctx context.Context, contextID string) (*Context, error) {
	c := &Context{}
	err := s.db.QueryRowContext(ctx,
		`SELECT context_id, project_id, agent_slug, agent_id, created_at, last_active
		 FROM a2a_contexts WHERE context_id = $1`, contextID,
	).Scan(&c.ContextID, &c.ProjectID, &c.AgentSlug, &c.AgentID, &c.CreatedAt, &c.LastActive)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get context: %w", err)
	}
	return c, nil
}

// TouchContext updates a context's last_active timestamp.
func (s *PostgresStore) TouchContext(ctx context.Context, contextID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE a2a_contexts SET last_active = NOW() WHERE context_id = $1`, contextID,
	)
	if err != nil {
		return fmt.Errorf("touch context: %w", err)
	}
	return nil
}

// --- Push Notification Configs ---

// SetPushConfig inserts or updates a push notification configuration.
func (s *PostgresStore) SetPushConfig(ctx context.Context, cfg *PushNotificationConfig) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO a2a_push_notification_configs (id, task_id, url, token, auth_scheme, auth_credentials, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (id) DO UPDATE SET
		     task_id = EXCLUDED.task_id,
		     url = EXCLUDED.url,
		     token = EXCLUDED.token,
		     auth_scheme = EXCLUDED.auth_scheme,
		     auth_credentials = EXCLUDED.auth_credentials,
		     created_at = EXCLUDED.created_at`,
		cfg.ID, cfg.TaskID, cfg.URL, cfg.Token, cfg.AuthScheme, cfg.AuthCredentials, cfg.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("set push config: %w", err)
	}
	return nil
}

// GetPushConfigsByTask returns all push configs for the given task.
func (s *PostgresStore) GetPushConfigsByTask(ctx context.Context, taskID string) ([]PushNotificationConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, url, token, auth_scheme, auth_credentials, created_at
		 FROM a2a_push_notification_configs WHERE task_id = $1`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("get push configs: %w", err)
	}
	defer rows.Close()

	var configs []PushNotificationConfig
	for rows.Next() {
		var c PushNotificationConfig
		if err := rows.Scan(&c.ID, &c.TaskID, &c.URL, &c.Token, &c.AuthScheme, &c.AuthCredentials, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan push config: %w", err)
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

// DeletePushConfig removes a push notification configuration.
func (s *PostgresStore) DeletePushConfig(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM a2a_push_notification_configs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete push config: %w", err)
	}
	return nil
}

// DeletePushConfigForTask removes a push config only if it belongs to the given task.
func (s *PostgresStore) DeletePushConfigForTask(ctx context.Context, taskID, id string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM a2a_push_notification_configs WHERE id = $1 AND task_id = $2`, id, taskID,
	)
	if err != nil {
		return fmt.Errorf("delete push config: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete push config: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("push config not found for task")
	}
	return nil
}

// --- Task Event Log ---

// AppendTaskEvent inserts a new event into the task event log.
// If ev.DedupKey is non-empty and a matching row already exists,
// the insert is silently skipped and id=0 is returned.
func (s *PostgresStore) AppendTaskEvent(ctx context.Context, ev *TaskEvent) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO a2a_task_events (task_id, kind, payload, final, dedup_key, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (task_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		 RETURNING id`,
		ev.TaskID, ev.Kind, json.RawMessage(ev.Payload), ev.Final, nullableString(ev.DedupKey),
	).Scan(&id)
	if err == sql.ErrNoRows {
		// Dedup conflict — row was not inserted.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("append task event: %w", err)
	}

	// Best-effort notification — error intentionally discarded since
	// NOTIFY is an accelerator; the poll loop catches everything. See design §5.2 (D7).
	_, _ = s.db.ExecContext(ctx, "SELECT pg_notify('a2a_task_event', $1)", ev.TaskID)

	return id, nil
}

// ReadTaskEvents returns events for a task with id > afterID, up to limit rows.
func (s *PostgresStore) ReadTaskEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, kind, payload, final, dedup_key, created_at
		 FROM a2a_task_events
		 WHERE task_id = $1 AND id > $2
		 ORDER BY id ASC LIMIT $3`, taskID, afterID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read task events: %w", err)
	}
	defer rows.Close()

	var events []TaskEvent
	for rows.Next() {
		var ev TaskEvent
		var dedupKey sql.NullString
		var payload []byte
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.Kind, &payload, &ev.Final, &dedupKey, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan task event: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		if dedupKey.Valid {
			ev.DedupKey = dedupKey.String
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// PurgeTaskEvents deletes events older than olderThan and returns the count deleted.
func (s *PostgresStore) PurgeTaskEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM a2a_task_events WHERE created_at < $1`, olderThan,
	)
	if err != nil {
		return 0, fmt.Errorf("purge task events: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge task events rows affected: %w", err)
	}
	return n, nil
}

// scanTasks is a helper to collect Task rows from a query result.
func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ContextID, &t.ProjectID, &t.AgentSlug, &t.AgentID, &t.State, &t.CallerUserID, &t.CreatedAt, &t.UpdatedAt, &t.Metadata); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
