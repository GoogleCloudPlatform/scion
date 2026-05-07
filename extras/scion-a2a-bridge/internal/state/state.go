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
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Task represents an A2A task mapped to a Scion agent interaction.
type Task struct {
	ID        string
	ContextID string
	GroveID   string
	AgentSlug string
	AgentID   string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Metadata  string
}

// Context maps an A2A contextId to a Scion agent.
type Context struct {
	ContextID  string
	GroveID    string
	AgentSlug  string
	AgentID    string
	CreatedAt  time.Time
	LastActive time.Time
}

// PushNotificationConfig stores webhook configuration for push notifications.
type PushNotificationConfig struct {
	ID              string
	TaskID          string
	URL             string
	Token           string
	AuthScheme      string
	AuthCredentials string
	CreatedAt       time.Time
}

// Store provides SQLite-backed state management for the A2A bridge.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at dbPath and runs schema migrations.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks database connectivity.
func (s *Store) Ping() error {
	return s.db.Ping()
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			context_id TEXT NOT NULL,
			grove_id TEXT NOT NULL,
			agent_slug TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			metadata TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_context ON tasks(context_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_agent ON tasks(grove_id, agent_slug)`,

		`CREATE TABLE IF NOT EXISTS contexts (
			context_id TEXT PRIMARY KEY,
			grove_id TEXT NOT NULL,
			agent_slug TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_active DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		// SECURITY: token and auth_credentials are stored in cleartext. The SQLite file
		// must be on an encrypted volume with strict file permissions (0600). Deployers
		// should use short-lived tokens where possible. Envelope encryption (AES-GCM with
		// a config-supplied KEK) is planned for a future release.
		`CREATE TABLE IF NOT EXISTS push_notification_configs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			url TEXT NOT NULL,
			token TEXT NOT NULL DEFAULT '',
			auth_scheme TEXT NOT NULL DEFAULT '',
			auth_credentials TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (task_id) REFERENCES tasks(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_push_task ON push_notification_configs(task_id)`,
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
func (s *Store) CreateTask(t *Task) error {
	_, err := s.db.Exec(
		`INSERT INTO tasks (id, context_id, grove_id, agent_slug, agent_id, state, created_at, updated_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ContextID, t.GroveID, t.AgentSlug, t.AgentID, t.State, t.CreatedAt, t.UpdatedAt, t.Metadata,
	)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// GetTask returns a task by ID, or nil if not found.
func (s *Store) GetTask(id string) (*Task, error) {
	t := &Task{}
	err := s.db.QueryRow(
		`SELECT id, context_id, grove_id, agent_slug, agent_id, state, created_at, updated_at, metadata
		 FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.ContextID, &t.GroveID, &t.AgentSlug, &t.AgentID, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.Metadata)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// UpdateTaskState updates a task's state and updated_at timestamp.
// Terminal states (completed, failed, canceled, rejected) are protected:
// once a task reaches a terminal state, further updates are silently ignored.
func (s *Store) UpdateTaskState(id, state string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state NOT IN ('completed', 'failed', 'canceled', 'rejected')`,
		state, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("update task state: %w", err)
	}
	return nil
}

// ListTasksByContext returns all tasks for the given context.
func (s *Store) ListTasksByContext(ctx context.Context, contextID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, context_id, grove_id, agent_slug, agent_id, state, created_at, updated_at, metadata
		 FROM tasks WHERE context_id = ? ORDER BY created_at DESC`, contextID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by context: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ContextID, &t.GroveID, &t.AgentSlug, &t.AgentID, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.Metadata); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ListTasksByAgent returns all tasks for a given grove and agent.
func (s *Store) ListTasksByAgent(groveID, agentSlug string) ([]Task, error) {
	rows, err := s.db.Query(
		`SELECT id, context_id, grove_id, agent_slug, agent_id, state, created_at, updated_at, metadata
		 FROM tasks WHERE grove_id = ? AND agent_slug = ? ORDER BY created_at DESC`, groveID, agentSlug,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by agent: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ContextID, &t.GroveID, &t.AgentSlug, &t.AgentID, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.Metadata); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// --- Contexts ---

// CreateContext inserts a new context mapping.
func (s *Store) CreateContext(c *Context) error {
	_, err := s.db.Exec(
		`INSERT INTO contexts (context_id, grove_id, agent_slug, agent_id, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.ContextID, c.GroveID, c.AgentSlug, c.AgentID, c.CreatedAt, c.LastActive,
	)
	if err != nil {
		return fmt.Errorf("create context: %w", err)
	}
	return nil
}

// GetContext returns a context by ID, or nil if not found.
func (s *Store) GetContext(contextID string) (*Context, error) {
	c := &Context{}
	err := s.db.QueryRow(
		`SELECT context_id, grove_id, agent_slug, agent_id, created_at, last_active
		 FROM contexts WHERE context_id = ?`, contextID,
	).Scan(&c.ContextID, &c.GroveID, &c.AgentSlug, &c.AgentID, &c.CreatedAt, &c.LastActive)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get context: %w", err)
	}
	return c, nil
}

// TouchContext updates a context's last_active timestamp.
func (s *Store) TouchContext(contextID string) error {
	_, err := s.db.Exec(
		`UPDATE contexts SET last_active = ? WHERE context_id = ?`,
		time.Now(), contextID,
	)
	if err != nil {
		return fmt.Errorf("touch context: %w", err)
	}
	return nil
}

// --- Push Notification Configs ---

// SetPushConfig inserts or replaces a push notification configuration.
func (s *Store) SetPushConfig(cfg *PushNotificationConfig) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO push_notification_configs (id, task_id, url, token, auth_scheme, auth_credentials, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		cfg.ID, cfg.TaskID, cfg.URL, cfg.Token, cfg.AuthScheme, cfg.AuthCredentials, cfg.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("set push config: %w", err)
	}
	return nil
}

// GetPushConfigsByTask returns all push configs for the given task.
func (s *Store) GetPushConfigsByTask(taskID string) ([]PushNotificationConfig, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, url, token, auth_scheme, auth_credentials, created_at
		 FROM push_notification_configs WHERE task_id = ?`, taskID,
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
func (s *Store) DeletePushConfig(id string) error {
	_, err := s.db.Exec(`DELETE FROM push_notification_configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete push config: %w", err)
	}
	return nil
}

// DeletePushConfigForTask removes a push config only if it belongs to the given task.
func (s *Store) DeletePushConfigForTask(taskID, id string) error {
	result, err := s.db.Exec(`DELETE FROM push_notification_configs WHERE id = ? AND task_id = ?`, id, taskID)
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

