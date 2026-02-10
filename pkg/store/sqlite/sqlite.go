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

// Package sqlite provides a SQLite implementation of the Store interface.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

// SQLiteStore implements the Store interface using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// New creates a new SQLite store with the given database path.
// Use ":memory:" for an in-memory database.
func New(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		if strings.Contains(err.Error(), "unknown driver") {
			return nil, fmt.Errorf("sqlite driver not registered; was the binary built with -tags no_sqlite? %w", err)
		}
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Enable foreign keys and WAL mode for better performance
	if _, err := db.Exec("PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to configure database: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// Ping checks database connectivity.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Migrate applies database migrations.
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	migrations := []string{
		migrationV1,
		migrationV2,
		migrationV3,
		migrationV4,
		migrationV5,
		migrationV6,
		migrationV7,
		migrationV8,
		migrationV9,
		migrationV10,
		migrationV11,
	}

	// Create migrations table if not exists
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Get current version
	var currentVersion int
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("failed to get current schema version: %w", err)
	}

	// Apply pending migrations
	for i, migration := range migrations {
		version := i + 1
		if version <= currentVersion {
			continue
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to start transaction for migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to apply migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to record migration %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %d: %w", version, err)
		}
	}

	return nil
}

// Migration V1: Initial schema
const migrationV1 = `
-- Groves table
CREATE TABLE IF NOT EXISTS groves (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL,
	git_remote TEXT UNIQUE,
	labels TEXT,
	annotations TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	owner_id TEXT,
	visibility TEXT NOT NULL DEFAULT 'private'
);
CREATE INDEX IF NOT EXISTS idx_groves_slug ON groves(slug);
CREATE INDEX IF NOT EXISTS idx_groves_git_remote ON groves(git_remote);
CREATE INDEX IF NOT EXISTS idx_groves_owner ON groves(owner_id);

-- Runtime brokers table
CREATE TABLE IF NOT EXISTS runtime_brokers (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL,
	type TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'connected',
	version TEXT,
	status TEXT NOT NULL DEFAULT 'offline',
	connection_state TEXT DEFAULT 'disconnected',
	last_heartbeat TIMESTAMP,
	capabilities TEXT,
	supported_harnesses TEXT,
	resources TEXT,
	runtimes TEXT,
	labels TEXT,
	annotations TEXT,
	endpoint TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_runtime_brokers_slug ON runtime_brokers(slug);
CREATE INDEX IF NOT EXISTS idx_runtime_brokers_status ON runtime_brokers(status);

-- Grove contributors (many-to-many relationship)
CREATE TABLE IF NOT EXISTS grove_contributors (
	grove_id TEXT NOT NULL,
	broker_id TEXT NOT NULL,
	broker_name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'connected',
	status TEXT NOT NULL DEFAULT 'offline',
	profiles TEXT,
	last_seen TIMESTAMP,
	PRIMARY KEY (grove_id, broker_id),
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE,
	FOREIGN KEY (broker_id) REFERENCES runtime_brokers(id) ON DELETE CASCADE
);

-- Agents table
CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL,
	name TEXT NOT NULL,
	template TEXT NOT NULL,
	grove_id TEXT NOT NULL,
	labels TEXT,
	annotations TEXT,
	status TEXT NOT NULL DEFAULT 'pending',
	connection_state TEXT DEFAULT 'unknown',
	container_status TEXT,
	session_status TEXT,
	runtime_state TEXT,
	image TEXT,
	detached INTEGER NOT NULL DEFAULT 1,
	runtime TEXT,
	runtime_broker_id TEXT,
	web_pty_enabled INTEGER NOT NULL DEFAULT 0,
	task_summary TEXT,
	applied_config TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	last_seen TIMESTAMP,
	created_by TEXT,
	owner_id TEXT,
	visibility TEXT NOT NULL DEFAULT 'private',
	state_version INTEGER NOT NULL DEFAULT 1,
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE,
	FOREIGN KEY (runtime_broker_id) REFERENCES runtime_brokers(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_grove_slug ON agents(grove_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_agents_grove ON agents(grove_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
CREATE INDEX IF NOT EXISTS idx_agents_runtime_broker ON agents(runtime_broker_id);

-- Templates table
CREATE TABLE IF NOT EXISTS templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL,
	harness TEXT NOT NULL,
	image TEXT,
	config TEXT,
	scope TEXT NOT NULL DEFAULT 'global',
	grove_id TEXT,
	storage_uri TEXT,
	owner_id TEXT,
	visibility TEXT NOT NULL DEFAULT 'private',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_templates_slug_scope ON templates(slug, scope);
CREATE INDEX IF NOT EXISTS idx_templates_harness ON templates(harness);

-- Users table
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	email TEXT UNIQUE NOT NULL,
	display_name TEXT NOT NULL,
	avatar_url TEXT,
	role TEXT NOT NULL DEFAULT 'member',
	status TEXT NOT NULL DEFAULT 'active',
	preferences TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	last_login TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
`

// Migration V2: Add default_runtime_broker_id to groves
const migrationV2 = `
-- Add default runtime broker to groves
ALTER TABLE groves ADD COLUMN default_runtime_broker_id TEXT REFERENCES runtime_brokers(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_groves_default_runtime_broker ON groves(default_runtime_broker_id);
`

// Migration V3: Add local_path to grove_contributors
const migrationV3 = `
-- Add local_path column to grove_contributors for tracking filesystem paths per broker
ALTER TABLE grove_contributors ADD COLUMN local_path TEXT;
`

// Migration V4: Add environment variables and secrets tables
const migrationV4 = `
-- Environment variables table
CREATE TABLE IF NOT EXISTS env_vars (
	id TEXT PRIMARY KEY,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	scope TEXT NOT NULL,
	scope_id TEXT NOT NULL,
	description TEXT,
	sensitive INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_env_vars_key_scope ON env_vars(key, scope, scope_id);
CREATE INDEX IF NOT EXISTS idx_env_vars_scope ON env_vars(scope, scope_id);

-- Secrets table
CREATE TABLE IF NOT EXISTS secrets (
	id TEXT PRIMARY KEY,
	key TEXT NOT NULL,
	encrypted_value TEXT NOT NULL,
	scope TEXT NOT NULL,
	scope_id TEXT NOT NULL,
	description TEXT,
	version INTEGER NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	updated_by TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_secrets_key_scope ON secrets(key, scope, scope_id);
CREATE INDEX IF NOT EXISTS idx_secrets_scope ON secrets(scope, scope_id);
`

// Migration V5: Groups and Policies (Hub Permissions System)
const migrationV5 = `
-- Groups table
CREATE TABLE IF NOT EXISTS groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT UNIQUE NOT NULL,
	description TEXT,
	parent_id TEXT REFERENCES groups(id) ON DELETE SET NULL,
	labels TEXT,
	annotations TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	owner_id TEXT
);
CREATE INDEX IF NOT EXISTS idx_groups_slug ON groups(slug);
CREATE INDEX IF NOT EXISTS idx_groups_parent ON groups(parent_id);
CREATE INDEX IF NOT EXISTS idx_groups_owner ON groups(owner_id);

-- Group members table (users and nested groups)
CREATE TABLE IF NOT EXISTS group_members (
	group_id TEXT NOT NULL,
	member_type TEXT NOT NULL,  -- 'user' or 'group'
	member_id TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	added_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	added_by TEXT,
	PRIMARY KEY (group_id, member_type, member_id),
	FOREIGN KEY (group_id) REFERENCES groups(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_group_members_member ON group_members(member_type, member_id);

-- Policies table
CREATE TABLE IF NOT EXISTS policies (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT,
	scope_type TEXT NOT NULL,
	scope_id TEXT,
	resource_type TEXT NOT NULL DEFAULT '*',
	resource_id TEXT,
	actions TEXT NOT NULL,  -- JSON array
	effect TEXT NOT NULL,
	conditions TEXT,        -- JSON object
	priority INTEGER NOT NULL DEFAULT 0,
	labels TEXT,
	annotations TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_policies_scope ON policies(scope_type, scope_id);
CREATE INDEX IF NOT EXISTS idx_policies_effect ON policies(effect);
CREATE INDEX IF NOT EXISTS idx_policies_priority ON policies(priority DESC);

-- Policy bindings table
CREATE TABLE IF NOT EXISTS policy_bindings (
	policy_id TEXT NOT NULL,
	principal_type TEXT NOT NULL,  -- 'user' or 'group'
	principal_id TEXT NOT NULL,
	PRIMARY KEY (policy_id, principal_type, principal_id),
	FOREIGN KEY (policy_id) REFERENCES policies(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_policy_bindings_principal ON policy_bindings(principal_type, principal_id);
`

// Migration V6: Extend templates table for hosted template management
const migrationV6 = `
-- Add new columns to templates table
ALTER TABLE templates ADD COLUMN display_name TEXT;
ALTER TABLE templates ADD COLUMN description TEXT;
ALTER TABLE templates ADD COLUMN content_hash TEXT;
ALTER TABLE templates ADD COLUMN scope_id TEXT;
ALTER TABLE templates ADD COLUMN storage_bucket TEXT;
ALTER TABLE templates ADD COLUMN storage_path TEXT;
ALTER TABLE templates ADD COLUMN files TEXT;
ALTER TABLE templates ADD COLUMN base_template TEXT;
ALTER TABLE templates ADD COLUMN locked INTEGER NOT NULL DEFAULT 0;
ALTER TABLE templates ADD COLUMN status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE templates ADD COLUMN created_by TEXT;
ALTER TABLE templates ADD COLUMN updated_by TEXT;

-- Add indexes for new columns
CREATE INDEX IF NOT EXISTS idx_templates_status ON templates(status);
CREATE INDEX IF NOT EXISTS idx_templates_content_hash ON templates(content_hash);
CREATE INDEX IF NOT EXISTS idx_templates_scope_id ON templates(scope, scope_id);
`

const migrationV7 = `
-- Add API keys table
CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    scopes TEXT,
    revoked INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMP,
    last_used TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- Add indexes for API keys
CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(prefix);
`

const migrationV8 = `
-- Add message column to agents table
ALTER TABLE agents ADD COLUMN message TEXT;
`

// Migration V9: Broker secrets and join tokens for Runtime Broker authentication
const migrationV9 = `
-- Broker secrets table for HMAC-based authentication
CREATE TABLE IF NOT EXISTS broker_secrets (
    broker_id TEXT PRIMARY KEY,
    secret_key BLOB NOT NULL,
    algorithm TEXT NOT NULL DEFAULT 'hmac-sha256',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    rotated_at TIMESTAMP,
    expires_at TIMESTAMP,
    status TEXT NOT NULL DEFAULT 'active',
    FOREIGN KEY (broker_id) REFERENCES runtime_brokers(id) ON DELETE CASCADE
);

-- Broker join tokens table for registration bootstrap
CREATE TABLE IF NOT EXISTS broker_join_tokens (
    broker_id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by TEXT NOT NULL,
    FOREIGN KEY (broker_id) REFERENCES runtime_brokers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_broker_join_tokens_hash ON broker_join_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_broker_join_tokens_expires ON broker_join_tokens(expires_at);
`

// Migration V10: Add user tracking to grove_contributors and runtime_brokers
const migrationV10 = `
-- Add linked_by and linked_at columns to grove_contributors for tracking who linked a broker
ALTER TABLE grove_contributors ADD COLUMN linked_by TEXT;
ALTER TABLE grove_contributors ADD COLUMN linked_at TIMESTAMP;

-- Add created_by column to runtime_brokers for tracking who registered the broker
ALTER TABLE runtime_brokers ADD COLUMN created_by TEXT;
`

// Migration V11: Add auto_provide column to runtime_brokers
const migrationV11 = `
-- Add auto_provide column to runtime_brokers for automatic grove provider registration
ALTER TABLE runtime_brokers ADD COLUMN auto_provide INTEGER NOT NULL DEFAULT 0;
`

// Helper functions for JSON marshaling/unmarshaling
func marshalJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func unmarshalJSON[T any](data string, v *T) {
	if data == "" {
		return
	}
	json.Unmarshal([]byte(data), v)
}

// nullableString returns a sql.NullString for database insertion.
// Empty strings become NULL, which is important for UNIQUE and FK constraints.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullableTime returns a sql.NullTime for database insertion.
// Zero time values become NULL.
func nullableTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{Valid: false}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// ============================================================================
// Agent Operations
// ============================================================================

func (s *SQLiteStore) CreateAgent(ctx context.Context, agent *store.Agent) error {
	now := time.Now()
	agent.Created = now
	agent.Updated = now
	agent.StateVersion = 1

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (
			id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		agent.ID, agent.Slug, agent.Name, agent.Template, agent.GroveID,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Status, agent.ConnectionState, agent.ContainerStatus, agent.SessionStatus, agent.RuntimeState,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeBrokerID), agent.WebPTYEnabled, agent.TaskSummary, agent.Message,
		marshalJSON(agent.AppliedConfig),
		agent.Created, agent.Updated, nullableTime(agent.LastSeen),
		agent.CreatedBy, agent.OwnerID, agent.Visibility, agent.StateVersion,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	agent := &store.Agent{}
	var labels, annotations, appliedConfig string
	var lastSeen sql.NullTime
	var runtimeBrokerID, message sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		FROM agents WHERE id = ?
	`, id).Scan(
		&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
		&appliedConfig,
		&agent.Created, &agent.Updated, &lastSeen,
		&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(labels, &agent.Labels)
	unmarshalJSON(annotations, &agent.Annotations)
	unmarshalJSON(appliedConfig, &agent.AppliedConfig)
	if lastSeen.Valid {
		agent.LastSeen = lastSeen.Time
	}
	if runtimeBrokerID.Valid {
		agent.RuntimeBrokerID = runtimeBrokerID.String
	}
	if message.Valid {
		agent.Message = message.String
	}

	return agent, nil
}

func (s *SQLiteStore) GetAgentBySlug(ctx context.Context, groveID, slug string) (*store.Agent, error) {
	agent := &store.Agent{}
	var labels, annotations, appliedConfig string
	var lastSeen sql.NullTime
	var runtimeBrokerID, message sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		FROM agents WHERE grove_id = ? AND agent_id = ?
	`, groveID, slug).Scan(
		&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
		&appliedConfig,
		&agent.Created, &agent.Updated, &lastSeen,
		&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(labels, &agent.Labels)
	unmarshalJSON(annotations, &agent.Annotations)
	unmarshalJSON(appliedConfig, &agent.AppliedConfig)
	if lastSeen.Valid {
		agent.LastSeen = lastSeen.Time
	}
	if runtimeBrokerID.Valid {
		agent.RuntimeBrokerID = runtimeBrokerID.String
	}
	if message.Valid {
		agent.Message = message.String
	}

	return agent, nil
}

func (s *SQLiteStore) UpdateAgent(ctx context.Context, agent *store.Agent) error {
	agent.Updated = time.Now()
	newVersion := agent.StateVersion + 1

	result, err := s.db.ExecContext(ctx, `
		UPDATE agents SET
			agent_id = ?, name = ?, template = ?,
			labels = ?, annotations = ?,
			status = ?, connection_state = ?, container_status = ?, session_status = ?, runtime_state = ?,
			image = ?, detached = ?, runtime = ?, runtime_broker_id = ?, web_pty_enabled = ?, task_summary = ?, message = ?,
			applied_config = ?,
			updated_at = ?, last_seen = ?,
			owner_id = ?, visibility = ?, state_version = ?
		WHERE id = ? AND state_version = ?
	`,
		agent.Slug, agent.Name, agent.Template,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Status, agent.ConnectionState, agent.ContainerStatus, agent.SessionStatus, agent.RuntimeState,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeBrokerID), agent.WebPTYEnabled, agent.TaskSummary, agent.Message,
		marshalJSON(agent.AppliedConfig),
		agent.Updated, nullableTime(agent.LastSeen),
		agent.OwnerID, agent.Visibility, newVersion,
		agent.ID, agent.StateVersion,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		// Check if agent exists
		var exists bool
		s.db.QueryRowContext(ctx, "SELECT 1 FROM agents WHERE id = ?", agent.ID).Scan(&exists)
		if !exists {
			return store.ErrNotFound
		}
		return store.ErrVersionConflict
	}

	agent.StateVersion = newVersion
	return nil
}

func (s *SQLiteStore) DeleteAgent(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM agents WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	var conditions []string
	var args []interface{}

	if filter.GroveID != "" {
		conditions = append(conditions, "grove_id = ?")
		args = append(args, filter.GroveID)
	}
	if filter.RuntimeBrokerID != "" {
		conditions = append(conditions, "runtime_broker_id = ?")
		args = append(args, filter.RuntimeBrokerID)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Get total count
	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM agents %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	// Apply pagination
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	query := fmt.Sprintf(`
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		FROM agents %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit+1) // Fetch one extra to determine if there's a next page

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []store.Agent
	for rows.Next() {
		var agent store.Agent
		var labels, annotations, appliedConfig string
		var lastSeen sql.NullTime
		var runtimeBrokerID, message sql.NullString

		if err := rows.Scan(
			&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
			&labels, &annotations,
			&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
			&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
			&appliedConfig,
			&agent.Created, &agent.Updated, &lastSeen,
			&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(labels, &agent.Labels)
		unmarshalJSON(annotations, &agent.Annotations)
		unmarshalJSON(appliedConfig, &agent.AppliedConfig)
		if lastSeen.Valid {
			agent.LastSeen = lastSeen.Time
		}
		if runtimeBrokerID.Valid {
			agent.RuntimeBrokerID = runtimeBrokerID.String
		}
		if message.Valid {
			agent.Message = message.String
		}

		agents = append(agents, agent)
	}

	result := &store.ListResult[store.Agent]{
		Items:      agents,
		TotalCount: totalCount,
	}

	// Handle pagination
	if len(agents) > limit {
		result.Items = agents[:limit]
		result.NextCursor = agents[limit-1].ID
	}

	return result, nil
}

func (s *SQLiteStore) UpdateAgentStatus(ctx context.Context, id string, status store.AgentStatusUpdate) error {
	now := time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE agents SET
			status = COALESCE(NULLIF(?, ''), status),
			message = COALESCE(NULLIF(?, ''), message),
			connection_state = COALESCE(NULLIF(?, ''), connection_state),
			container_status = COALESCE(NULLIF(?, ''), container_status),
			session_status = COALESCE(NULLIF(?, ''), session_status),
			runtime_state = COALESCE(NULLIF(?, ''), runtime_state),
			task_summary = COALESCE(NULLIF(?, ''), task_summary),
			updated_at = ?,
			last_seen = ?
		WHERE id = ?
	`,
		status.Status, status.Message, status.ConnectionState, status.ContainerStatus,
		status.SessionStatus, status.RuntimeState, status.TaskSummary,
		now, now, id,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ============================================================================
// Grove Operations
// ============================================================================

func (s *SQLiteStore) CreateGrove(ctx context.Context, grove *store.Grove) error {
	now := time.Now()
	grove.Created = now
	grove.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO groves (id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		grove.ID, grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeBrokerID),
		marshalJSON(grove.Labels), marshalJSON(grove.Annotations),
		grove.Created, grove.Updated, grove.CreatedBy, grove.OwnerID, grove.Visibility,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetGrove(ctx context.Context, id string) (*store.Grove, error) {
	grove := &store.Grove{}
	var labels, annotations string
	var gitRemote, defaultRuntimeBrokerID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility
		FROM groves WHERE id = ?
	`, id).Scan(
		&grove.ID, &grove.Name, &grove.Slug, &gitRemote, &defaultRuntimeBrokerID,
		&labels, &annotations,
		&grove.Created, &grove.Updated, &grove.CreatedBy, &grove.OwnerID, &grove.Visibility,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if gitRemote.Valid {
		grove.GitRemote = gitRemote.String
	}
	if defaultRuntimeBrokerID.Valid {
		grove.DefaultRuntimeBrokerID = defaultRuntimeBrokerID.String
	}
	unmarshalJSON(labels, &grove.Labels)
	unmarshalJSON(annotations, &grove.Annotations)

	// Populate computed fields
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", id).Scan(&grove.AgentCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online'", id).Scan(&grove.ActiveBrokerCount)

	return grove, nil
}

func (s *SQLiteStore) GetGroveBySlug(ctx context.Context, slug string) (*store.Grove, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM groves WHERE slug = ?", slug).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetGrove(ctx, id)
}

func (s *SQLiteStore) GetGroveBySlugCaseInsensitive(ctx context.Context, slug string) (*store.Grove, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM groves WHERE LOWER(slug) = LOWER(?)", slug).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetGrove(ctx, id)
}

func (s *SQLiteStore) GetGroveByGitRemote(ctx context.Context, gitRemote string) (*store.Grove, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM groves WHERE git_remote = ?", gitRemote).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetGrove(ctx, id)
}

func (s *SQLiteStore) UpdateGrove(ctx context.Context, grove *store.Grove) error {
	grove.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE groves SET
			name = ?, slug = ?, git_remote = ?, default_runtime_broker_id = ?,
			labels = ?, annotations = ?,
			updated_at = ?, owner_id = ?, visibility = ?
		WHERE id = ?
	`,
		grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeBrokerID),
		marshalJSON(grove.Labels), marshalJSON(grove.Annotations),
		grove.Updated, grove.OwnerID, grove.Visibility,
		grove.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteGrove(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM groves WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListGroves(ctx context.Context, filter store.GroveFilter, opts store.ListOptions) (*store.ListResult[store.Grove], error) {
	var conditions []string
	var args []interface{}

	if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.Visibility != "" {
		conditions = append(conditions, "visibility = ?")
		args = append(args, filter.Visibility)
	}
	if filter.GitRemotePrefix != "" {
		conditions = append(conditions, "git_remote LIKE ?")
		args = append(args, filter.GitRemotePrefix+"%")
	}
	if filter.BrokerID != "" {
		conditions = append(conditions, "id IN (SELECT grove_id FROM grove_contributors WHERE broker_id = ?)")
		args = append(args, filter.BrokerID)
	}
	if filter.Name != "" {
		conditions = append(conditions, "LOWER(name) = LOWER(?)")
		args = append(args, filter.Name)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM groves %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility
		FROM groves %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groves []store.Grove
	for rows.Next() {
		var grove store.Grove
		var labels, annotations string
		var gitRemote, defaultRuntimeBrokerID sql.NullString

		if err := rows.Scan(
			&grove.ID, &grove.Name, &grove.Slug, &gitRemote, &defaultRuntimeBrokerID,
			&labels, &annotations,
			&grove.Created, &grove.Updated, &grove.CreatedBy, &grove.OwnerID, &grove.Visibility,
		); err != nil {
			return nil, err
		}

		if gitRemote.Valid {
			grove.GitRemote = gitRemote.String
		}
		if defaultRuntimeBrokerID.Valid {
			grove.DefaultRuntimeBrokerID = defaultRuntimeBrokerID.String
		}
		unmarshalJSON(labels, &grove.Labels)
		unmarshalJSON(annotations, &grove.Annotations)

		// Populate computed fields
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", grove.ID).Scan(&grove.AgentCount)
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online'", grove.ID).Scan(&grove.ActiveBrokerCount)

		groves = append(groves, grove)
	}

	return &store.ListResult[store.Grove]{
		Items:      groves,
		TotalCount: totalCount,
	}, nil
}

// ============================================================================
// RuntimeBroker Operations
// ============================================================================

func (s *SQLiteStore) CreateRuntimeBroker(ctx context.Context, broker *store.RuntimeBroker) error {
	now := time.Now()
	broker.Created = now
	broker.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_brokers (
			id, name, slug, type, mode, version,
			status, connection_state, last_heartbeat,
			capabilities, supported_harnesses, resources, runtimes,
			labels, annotations, endpoint,
			created_at, updated_at, created_by, auto_provide
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		broker.ID, broker.Name, broker.Slug, "", "", broker.Version,
		broker.Status, broker.ConnectionState, broker.LastHeartbeat,
		marshalJSON(broker.Capabilities), "[]",
		"{}", marshalJSON(broker.Profiles),
		marshalJSON(broker.Labels), marshalJSON(broker.Annotations), broker.Endpoint,
		broker.Created, broker.Updated, nullableString(broker.CreatedBy), broker.AutoProvide,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	broker := &store.RuntimeBroker{}
	var capabilities, profiles, labels, annotations string
	var brokerType, brokerMode, harnesses, resources string // unused columns kept for schema compatibility
	var lastHeartbeat sql.NullTime
	var createdBy sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, type, mode, version,
			status, connection_state, last_heartbeat,
			capabilities, supported_harnesses, resources, runtimes,
			labels, annotations, endpoint,
			created_at, updated_at, created_by, auto_provide
		FROM runtime_brokers WHERE id = ?
	`, id).Scan(
		&broker.ID, &broker.Name, &broker.Slug, &brokerType, &brokerMode, &broker.Version,
		&broker.Status, &broker.ConnectionState, &lastHeartbeat,
		&capabilities, &harnesses, &resources, &profiles,
		&labels, &annotations, &broker.Endpoint,
		&broker.Created, &broker.Updated, &createdBy, &broker.AutoProvide,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if lastHeartbeat.Valid {
		broker.LastHeartbeat = lastHeartbeat.Time
	}
	if createdBy.Valid {
		broker.CreatedBy = createdBy.String
	}
	unmarshalJSON(capabilities, &broker.Capabilities)
	unmarshalJSON(profiles, &broker.Profiles)
	unmarshalJSON(labels, &broker.Labels)
	unmarshalJSON(annotations, &broker.Annotations)

	return broker, nil
}

func (s *SQLiteStore) GetRuntimeBrokerByName(ctx context.Context, name string) (*store.RuntimeBroker, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM runtime_brokers WHERE LOWER(name) = LOWER(?)", name).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetRuntimeBroker(ctx, id)
}

func (s *SQLiteStore) UpdateRuntimeBroker(ctx context.Context, broker *store.RuntimeBroker) error {
	broker.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_brokers SET
			name = ?, slug = ?, type = ?, version = ?,
			status = ?, connection_state = ?, last_heartbeat = ?,
			capabilities = ?, supported_harnesses = ?, resources = ?, runtimes = ?,
			labels = ?, annotations = ?, endpoint = ?,
			updated_at = ?, auto_provide = ?
		WHERE id = ?
	`,
		broker.Name, broker.Slug, "", broker.Version,
		broker.Status, broker.ConnectionState, broker.LastHeartbeat,
		marshalJSON(broker.Capabilities), "[]",
		"{}", marshalJSON(broker.Profiles),
		marshalJSON(broker.Labels), marshalJSON(broker.Annotations), broker.Endpoint,
		broker.Updated, broker.AutoProvide,
		broker.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteRuntimeBroker(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM runtime_brokers WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	var conditions []string
	var args []interface{}

	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.GroveID != "" {
		conditions = append(conditions, "id IN (SELECT broker_id FROM grove_contributors WHERE grove_id = ?)")
		args = append(args, filter.GroveID)
	}
	if filter.Name != "" {
		conditions = append(conditions, "LOWER(name) = LOWER(?)")
		args = append(args, filter.Name)
	}
	if filter.AutoProvide != nil {
		conditions = append(conditions, "auto_provide = ?")
		args = append(args, *filter.AutoProvide)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM runtime_brokers %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, slug, type, mode, version,
			status, connection_state, last_heartbeat,
			capabilities, supported_harnesses, resources, runtimes,
			labels, annotations, endpoint,
			created_at, updated_at, created_by, auto_provide
		FROM runtime_brokers %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []store.RuntimeBroker
	for rows.Next() {
		var broker store.RuntimeBroker
		var capabilities, profiles, labels, annotations string
		var brokerType, brokerMode, harnesses, resources string // unused columns kept for schema compatibility
		var lastHeartbeat sql.NullTime
		var createdBy sql.NullString

		if err := rows.Scan(
			&broker.ID, &broker.Name, &broker.Slug, &brokerType, &brokerMode, &broker.Version,
			&broker.Status, &broker.ConnectionState, &lastHeartbeat,
			&capabilities, &harnesses, &resources, &profiles,
			&labels, &annotations, &broker.Endpoint,
			&broker.Created, &broker.Updated, &createdBy, &broker.AutoProvide,
		); err != nil {
			return nil, err
		}

		if lastHeartbeat.Valid {
			broker.LastHeartbeat = lastHeartbeat.Time
		}
		if createdBy.Valid {
			broker.CreatedBy = createdBy.String
		}
		unmarshalJSON(capabilities, &broker.Capabilities)
		unmarshalJSON(profiles, &broker.Profiles)
		unmarshalJSON(labels, &broker.Labels)
		unmarshalJSON(annotations, &broker.Annotations)

		hosts = append(hosts, broker)
	}

	return &store.ListResult[store.RuntimeBroker]{
		Items:      hosts,
		TotalCount: totalCount,
	}, nil
}

func (s *SQLiteStore) UpdateRuntimeBrokerHeartbeat(ctx context.Context, id string, status string) error {
	now := time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_brokers SET
			status = ?,
			last_heartbeat = ?,
			updated_at = ?
		WHERE id = ?
	`, status, now, now, id)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ============================================================================
// Template Operations
// ============================================================================

func (s *SQLiteStore) CreateTemplate(ctx context.Context, template *store.Template) error {
	now := time.Now()
	template.Created = now
	template.Updated = now

	// Set default status if not provided
	if template.Status == "" {
		template.Status = store.TemplateStatusActive
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO templates (
			id, name, slug, display_name, description, harness, image, config,
			content_hash, scope, scope_id, grove_id,
			storage_uri, storage_bucket, storage_path, files,
			base_template, locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		template.ID, template.Name, template.Slug, nullableString(template.DisplayName), nullableString(template.Description),
		template.Harness, template.Image, marshalJSON(template.Config),
		nullableString(template.ContentHash), template.Scope, nullableString(template.ScopeID), nullableString(template.GroveID),
		nullableString(template.StorageURI), nullableString(template.StorageBucket), nullableString(template.StoragePath), marshalJSON(template.Files),
		nullableString(template.BaseTemplate), template.Locked, template.Status,
		nullableString(template.OwnerID), nullableString(template.CreatedBy), nullableString(template.UpdatedBy), template.Visibility,
		template.Created, template.Updated,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetTemplate(ctx context.Context, id string) (*store.Template, error) {
	template := &store.Template{}
	var config, files string
	var displayName, description, contentHash, scopeID, groveID sql.NullString
	var storageURI, storageBucket, storagePath, baseTemplate sql.NullString
	var createdBy, updatedBy, ownerID, visibility sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, display_name, description, harness, image, config,
			content_hash, scope, scope_id, grove_id,
			storage_uri, storage_bucket, storage_path, files,
			base_template, locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		FROM templates WHERE id = ?
	`, id).Scan(
		&template.ID, &template.Name, &template.Slug, &displayName, &description,
		&template.Harness, &template.Image, &config,
		&contentHash, &template.Scope, &scopeID, &groveID,
		&storageURI, &storageBucket, &storagePath, &files,
		&baseTemplate, &template.Locked, &template.Status,
		&ownerID, &createdBy, &updatedBy, &visibility,
		&template.Created, &template.Updated,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if displayName.Valid {
		template.DisplayName = displayName.String
	}
	if description.Valid {
		template.Description = description.String
	}
	if contentHash.Valid {
		template.ContentHash = contentHash.String
	}
	if scopeID.Valid {
		template.ScopeID = scopeID.String
	}
	if groveID.Valid {
		template.GroveID = groveID.String
	}
	if storageURI.Valid {
		template.StorageURI = storageURI.String
	}
	if storageBucket.Valid {
		template.StorageBucket = storageBucket.String
	}
	if storagePath.Valid {
		template.StoragePath = storagePath.String
	}
	if baseTemplate.Valid {
		template.BaseTemplate = baseTemplate.String
	}
	if ownerID.Valid {
		template.OwnerID = ownerID.String
	}
	if createdBy.Valid {
		template.CreatedBy = createdBy.String
	}
	if updatedBy.Valid {
		template.UpdatedBy = updatedBy.String
	}
	if visibility.Valid {
		template.Visibility = visibility.String
	}
	unmarshalJSON(config, &template.Config)
	unmarshalJSON(files, &template.Files)

	return template, nil
}

func (s *SQLiteStore) GetTemplateBySlug(ctx context.Context, slug, scope, scopeID string) (*store.Template, error) {
	var id string
	var err error

	if scope == "grove" && scopeID != "" {
		// Try scope_id first, then fall back to grove_id for backwards compatibility
		err = s.db.QueryRowContext(ctx, "SELECT id FROM templates WHERE slug = ? AND scope = ? AND (scope_id = ? OR grove_id = ?)", slug, scope, scopeID, scopeID).Scan(&id)
	} else if scope == "user" && scopeID != "" {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM templates WHERE slug = ? AND scope = ? AND scope_id = ?", slug, scope, scopeID).Scan(&id)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM templates WHERE slug = ? AND scope = ?", slug, scope).Scan(&id)
	}

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetTemplate(ctx, id)
}

func (s *SQLiteStore) UpdateTemplate(ctx context.Context, template *store.Template) error {
	template.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE templates SET
			name = ?, slug = ?, display_name = ?, description = ?,
			harness = ?, image = ?, config = ?,
			content_hash = ?, scope = ?, scope_id = ?, grove_id = ?,
			storage_uri = ?, storage_bucket = ?, storage_path = ?, files = ?,
			base_template = ?, locked = ?, status = ?,
			owner_id = ?, updated_by = ?, visibility = ?,
			updated_at = ?
		WHERE id = ?
	`,
		template.Name, template.Slug, nullableString(template.DisplayName), nullableString(template.Description),
		template.Harness, template.Image, marshalJSON(template.Config),
		nullableString(template.ContentHash), template.Scope, nullableString(template.ScopeID), nullableString(template.GroveID),
		nullableString(template.StorageURI), nullableString(template.StorageBucket), nullableString(template.StoragePath), marshalJSON(template.Files),
		nullableString(template.BaseTemplate), template.Locked, template.Status,
		nullableString(template.OwnerID), nullableString(template.UpdatedBy), template.Visibility,
		template.Updated,
		template.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteTemplate(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM templates WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListTemplates(ctx context.Context, filter store.TemplateFilter, opts store.ListOptions) (*store.ListResult[store.Template], error) {
	var conditions []string
	var args []interface{}

	if filter.Name != "" {
		// Exact match on name or slug
		conditions = append(conditions, "(name = ? OR slug = ?)")
		args = append(args, filter.Name, filter.Name)
	}
	if filter.Scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.ScopeID != "" {
		conditions = append(conditions, "(scope_id = ? OR grove_id = ?)")
		args = append(args, filter.ScopeID, filter.ScopeID)
	} else if filter.GroveID != "" {
		// Backwards compatibility
		conditions = append(conditions, "(scope_id = ? OR grove_id = ?)")
		args = append(args, filter.GroveID, filter.GroveID)
	}
	if filter.Harness != "" {
		conditions = append(conditions, "harness = ?")
		args = append(args, filter.Harness)
	}
	if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Search != "" {
		conditions = append(conditions, "(name LIKE ? OR description LIKE ?)")
		searchPattern := "%" + filter.Search + "%"
		args = append(args, searchPattern, searchPattern)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM templates %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, slug, display_name, description, harness, image, config,
			content_hash, scope, scope_id, grove_id,
			storage_uri, storage_bucket, storage_path, files,
			base_template, locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		FROM templates %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []store.Template
	for rows.Next() {
		var template store.Template
		var config, files string
		var displayName, description, contentHash, scopeID, groveID sql.NullString
		var storageURI, storageBucket, storagePath, baseTemplate sql.NullString
		var createdBy, updatedBy, ownerID, visibility sql.NullString

		if err := rows.Scan(
			&template.ID, &template.Name, &template.Slug, &displayName, &description,
			&template.Harness, &template.Image, &config,
			&contentHash, &template.Scope, &scopeID, &groveID,
			&storageURI, &storageBucket, &storagePath, &files,
			&baseTemplate, &template.Locked, &template.Status,
			&ownerID, &createdBy, &updatedBy, &visibility,
			&template.Created, &template.Updated,
		); err != nil {
			return nil, err
		}

		if displayName.Valid {
			template.DisplayName = displayName.String
		}
		if description.Valid {
			template.Description = description.String
		}
		if contentHash.Valid {
			template.ContentHash = contentHash.String
		}
		if scopeID.Valid {
			template.ScopeID = scopeID.String
		}
		if groveID.Valid {
			template.GroveID = groveID.String
		}
		if storageURI.Valid {
			template.StorageURI = storageURI.String
		}
		if storageBucket.Valid {
			template.StorageBucket = storageBucket.String
		}
		if storagePath.Valid {
			template.StoragePath = storagePath.String
		}
		if baseTemplate.Valid {
			template.BaseTemplate = baseTemplate.String
		}
		if ownerID.Valid {
			template.OwnerID = ownerID.String
		}
		if createdBy.Valid {
			template.CreatedBy = createdBy.String
		}
		if updatedBy.Valid {
			template.UpdatedBy = updatedBy.String
		}
		if visibility.Valid {
			template.Visibility = visibility.String
		}
		unmarshalJSON(config, &template.Config)
		unmarshalJSON(files, &template.Files)

		templates = append(templates, template)
	}

	return &store.ListResult[store.Template]{
		Items:      templates,
		TotalCount: totalCount,
	}, nil
}

// ============================================================================
// User Operations
// ============================================================================

func (s *SQLiteStore) CreateUser(ctx context.Context, user *store.User) error {
	now := time.Now()
	user.Created = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, email, display_name, avatar_url, role, status, preferences, created_at, last_login)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		user.ID, user.Email, user.DisplayName, user.AvatarURL, user.Role, user.Status,
		marshalJSON(user.Preferences), user.Created, user.LastLogin,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	user := &store.User{}
	var preferences string
	var lastLogin sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, email, display_name, avatar_url, role, status, preferences, created_at, last_login
		FROM users WHERE id = ?
	`, id).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
		&preferences, &user.Created, &lastLogin,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if lastLogin.Valid {
		user.LastLogin = lastLogin.Time
	}
	unmarshalJSON(preferences, &user.Preferences)

	return user, nil
}

func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ?", email).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetUser(ctx, id)
}

func (s *SQLiteStore) UpdateUser(ctx context.Context, user *store.User) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE users SET
			email = ?, display_name = ?, avatar_url = ?,
			role = ?, status = ?, preferences = ?, last_login = ?
		WHERE id = ?
	`,
		user.Email, user.DisplayName, user.AvatarURL,
		user.Role, user.Status, marshalJSON(user.Preferences), user.LastLogin,
		user.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteUser(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListUsers(ctx context.Context, filter store.UserFilter, opts store.ListOptions) (*store.ListResult[store.User], error) {
	var conditions []string
	var args []interface{}

	if filter.Role != "" {
		conditions = append(conditions, "role = ?")
		args = append(args, filter.Role)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM users %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, email, display_name, avatar_url, role, status, preferences, created_at, last_login
		FROM users %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []store.User
	for rows.Next() {
		var user store.User
		var preferences string
		var lastLogin sql.NullTime

		if err := rows.Scan(
			&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
			&preferences, &user.Created, &lastLogin,
		); err != nil {
			return nil, err
		}

		if lastLogin.Valid {
			user.LastLogin = lastLogin.Time
		}
		unmarshalJSON(preferences, &user.Preferences)

		users = append(users, user)
	}

	return &store.ListResult[store.User]{
		Items:      users,
		TotalCount: totalCount,
	}, nil
}

// ============================================================================
// GroveProvider Operations
// ============================================================================

func (s *SQLiteStore) AddGroveProvider(ctx context.Context, provider *store.GroveProvider) error {
	// Set LinkedAt to now if not already set
	if provider.LinkedAt.IsZero() && provider.LinkedBy != "" {
		provider.LinkedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO grove_contributors (grove_id, broker_id, broker_name, local_path, mode, status, profiles, last_seen, linked_by, linked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		provider.GroveID, provider.BrokerID, provider.BrokerName, provider.LocalPath, "", provider.Status,
		"[]", provider.LastSeen, // profiles column kept for schema compat but no longer used
		nullableString(provider.LinkedBy), nullableTime(provider.LinkedAt),
	)
	return err
}

func (s *SQLiteStore) RemoveGroveProvider(ctx context.Context, groveID, brokerID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM grove_contributors WHERE grove_id = ? AND broker_id = ?", groveID, brokerID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) GetGroveProvider(ctx context.Context, groveID, brokerID string) (*store.GroveProvider, error) {
	var provider store.GroveProvider
	var localPath, linkedBy sql.NullString
	var providerMode, profiles string // unused columns kept for schema compat
	var lastSeen, linkedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT grove_id, broker_id, broker_name, local_path, mode, status, profiles, last_seen, linked_by, linked_at
		FROM grove_contributors WHERE grove_id = ? AND broker_id = ?
	`, groveID, brokerID).Scan(
		&provider.GroveID, &provider.BrokerID, &provider.BrokerName, &localPath, &providerMode, &provider.Status,
		&profiles, &lastSeen, &linkedBy, &linkedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if localPath.Valid {
		provider.LocalPath = localPath.String
	}
	if lastSeen.Valid {
		provider.LastSeen = lastSeen.Time
	}
	if linkedBy.Valid {
		provider.LinkedBy = linkedBy.String
	}
	if linkedAt.Valid {
		provider.LinkedAt = linkedAt.Time
	}
	// profiles column no longer used - lookup from RuntimeBroker.Profiles instead

	return &provider, nil
}

func (s *SQLiteStore) GetGroveProviders(ctx context.Context, groveID string) ([]store.GroveProvider, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grove_id, broker_id, broker_name, local_path, mode, status, profiles, last_seen, linked_by, linked_at
		FROM grove_contributors WHERE grove_id = ?
	`, groveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []store.GroveProvider
	for rows.Next() {
		var provider store.GroveProvider
		var localPath, linkedBy sql.NullString
		var providerMode, profiles string // unused columns kept for schema compat
		var lastSeen, linkedAt sql.NullTime

		if err := rows.Scan(
			&provider.GroveID, &provider.BrokerID, &provider.BrokerName, &localPath, &providerMode, &provider.Status,
			&profiles, &lastSeen, &linkedBy, &linkedAt,
		); err != nil {
			return nil, err
		}

		if localPath.Valid {
			provider.LocalPath = localPath.String
		}
		if lastSeen.Valid {
			provider.LastSeen = lastSeen.Time
		}
		if linkedBy.Valid {
			provider.LinkedBy = linkedBy.String
		}
		if linkedAt.Valid {
			provider.LinkedAt = linkedAt.Time
		}
		// profiles column no longer used - lookup from RuntimeBroker.Profiles instead

		providers = append(providers, provider)
	}

	return providers, nil
}

func (s *SQLiteStore) GetBrokerGroves(ctx context.Context, brokerID string) ([]store.GroveProvider, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grove_id, broker_id, broker_name, local_path, mode, status, profiles, last_seen, linked_by, linked_at
		FROM grove_contributors WHERE broker_id = ?
	`, brokerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []store.GroveProvider
	for rows.Next() {
		var provider store.GroveProvider
		var localPath, linkedBy sql.NullString
		var providerMode, profiles string // unused columns kept for schema compat
		var lastSeen, linkedAt sql.NullTime

		if err := rows.Scan(
			&provider.GroveID, &provider.BrokerID, &provider.BrokerName, &localPath, &providerMode, &provider.Status,
			&profiles, &lastSeen, &linkedBy, &linkedAt,
		); err != nil {
			return nil, err
		}

		if localPath.Valid {
			provider.LocalPath = localPath.String
		}
		if lastSeen.Valid {
			provider.LastSeen = lastSeen.Time
		}
		if linkedBy.Valid {
			provider.LinkedBy = linkedBy.String
		}
		if linkedAt.Valid {
			provider.LinkedAt = linkedAt.Time
		}
		// profiles column no longer used - lookup from RuntimeBroker.Profiles instead

		providers = append(providers, provider)
	}

	return providers, nil
}

func (s *SQLiteStore) UpdateProviderStatus(ctx context.Context, groveID, brokerID, status string) error {
	now := time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE grove_contributors SET status = ?, last_seen = ? WHERE grove_id = ? AND broker_id = ?
	`, status, now, groveID, brokerID)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ============================================================================
// EnvVar Operations
// ============================================================================

func (s *SQLiteStore) CreateEnvVar(ctx context.Context, envVar *store.EnvVar) error {
	now := time.Now()
	envVar.Created = now
	envVar.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO env_vars (id, key, value, scope, scope_id, description, sensitive, created_at, updated_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		envVar.ID, envVar.Key, envVar.Value, envVar.Scope, envVar.ScopeID,
		envVar.Description, envVar.Sensitive,
		envVar.Created, envVar.Updated, envVar.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetEnvVar(ctx context.Context, key, scope, scopeID string) (*store.EnvVar, error) {
	envVar := &store.EnvVar{}

	err := s.db.QueryRowContext(ctx, `
		SELECT id, key, value, scope, scope_id, description, sensitive, created_at, updated_at, created_by
		FROM env_vars WHERE key = ? AND scope = ? AND scope_id = ?
	`, key, scope, scopeID).Scan(
		&envVar.ID, &envVar.Key, &envVar.Value, &envVar.Scope, &envVar.ScopeID,
		&envVar.Description, &envVar.Sensitive,
		&envVar.Created, &envVar.Updated, &envVar.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	return envVar, nil
}

func (s *SQLiteStore) UpdateEnvVar(ctx context.Context, envVar *store.EnvVar) error {
	envVar.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE env_vars SET
			value = ?, description = ?, sensitive = ?, updated_at = ?
		WHERE key = ? AND scope = ? AND scope_id = ?
	`,
		envVar.Value, envVar.Description, envVar.Sensitive, envVar.Updated,
		envVar.Key, envVar.Scope, envVar.ScopeID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpsertEnvVar(ctx context.Context, envVar *store.EnvVar) (bool, error) {
	now := time.Now()
	envVar.Updated = now

	// Check if it already exists
	existing, err := s.GetEnvVar(ctx, envVar.Key, envVar.Scope, envVar.ScopeID)
	if err != nil && err != store.ErrNotFound {
		return false, err
	}

	if existing != nil {
		// Update existing
		envVar.ID = existing.ID
		envVar.Created = existing.Created
		envVar.CreatedBy = existing.CreatedBy
		if err := s.UpdateEnvVar(ctx, envVar); err != nil {
			return false, err
		}
		return false, nil
	}

	// Create new
	envVar.Created = now
	if err := s.CreateEnvVar(ctx, envVar); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) DeleteEnvVar(ctx context.Context, key, scope, scopeID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM env_vars WHERE key = ? AND scope = ? AND scope_id = ?", key, scope, scopeID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	var conditions []string
	var args []interface{}

	if filter.Scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.ScopeID != "" {
		conditions = append(conditions, "scope_id = ?")
		args = append(args, filter.ScopeID)
	}
	if filter.Key != "" {
		conditions = append(conditions, "key = ?")
		args = append(args, filter.Key)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT id, key, value, scope, scope_id, description, sensitive, created_at, updated_at, created_by
		FROM env_vars %s ORDER BY key
	`, whereClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var envVars []store.EnvVar
	for rows.Next() {
		var envVar store.EnvVar
		if err := rows.Scan(
			&envVar.ID, &envVar.Key, &envVar.Value, &envVar.Scope, &envVar.ScopeID,
			&envVar.Description, &envVar.Sensitive,
			&envVar.Created, &envVar.Updated, &envVar.CreatedBy,
		); err != nil {
			return nil, err
		}
		envVars = append(envVars, envVar)
	}

	return envVars, nil
}

// ============================================================================
// Secret Operations
// ============================================================================

func (s *SQLiteStore) CreateSecret(ctx context.Context, secret *store.Secret) error {
	now := time.Now()
	secret.Created = now
	secret.Updated = now
	secret.Version = 1

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets (id, key, encrypted_value, scope, scope_id, description, version, created_at, updated_at, created_by, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		secret.ID, secret.Key, secret.EncryptedValue, secret.Scope, secret.ScopeID,
		secret.Description, secret.Version,
		secret.Created, secret.Updated, secret.CreatedBy, secret.UpdatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	secret := &store.Secret{}

	err := s.db.QueryRowContext(ctx, `
		SELECT id, key, encrypted_value, scope, scope_id, description, version, created_at, updated_at, created_by, updated_by
		FROM secrets WHERE key = ? AND scope = ? AND scope_id = ?
	`, key, scope, scopeID).Scan(
		&secret.ID, &secret.Key, &secret.EncryptedValue, &secret.Scope, &secret.ScopeID,
		&secret.Description, &secret.Version,
		&secret.Created, &secret.Updated, &secret.CreatedBy, &secret.UpdatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	return secret, nil
}

func (s *SQLiteStore) UpdateSecret(ctx context.Context, secret *store.Secret) error {
	secret.Updated = time.Now()
	secret.Version++ // Increment version on each update

	result, err := s.db.ExecContext(ctx, `
		UPDATE secrets SET
			encrypted_value = ?, description = ?, version = ?, updated_at = ?, updated_by = ?
		WHERE key = ? AND scope = ? AND scope_id = ?
	`,
		secret.EncryptedValue, secret.Description, secret.Version, secret.Updated, secret.UpdatedBy,
		secret.Key, secret.Scope, secret.ScopeID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpsertSecret(ctx context.Context, secret *store.Secret) (bool, error) {
	now := time.Now()
	secret.Updated = now

	// Check if it already exists
	existing, err := s.GetSecret(ctx, secret.Key, secret.Scope, secret.ScopeID)
	if err != nil && err != store.ErrNotFound {
		return false, err
	}

	if existing != nil {
		// Update existing
		secret.ID = existing.ID
		secret.Created = existing.Created
		secret.CreatedBy = existing.CreatedBy
		secret.Version = existing.Version // Will be incremented in UpdateSecret
		if err := s.UpdateSecret(ctx, secret); err != nil {
			return false, err
		}
		return false, nil
	}

	// Create new
	secret.Created = now
	if err := s.CreateSecret(ctx, secret); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) DeleteSecret(ctx context.Context, key, scope, scopeID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE key = ? AND scope = ? AND scope_id = ?", key, scope, scopeID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	var conditions []string
	var args []interface{}

	if filter.Scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.ScopeID != "" {
		conditions = append(conditions, "scope_id = ?")
		args = append(args, filter.ScopeID)
	}
	if filter.Key != "" {
		conditions = append(conditions, "key = ?")
		args = append(args, filter.Key)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Note: We do NOT select encrypted_value for listing
	query := fmt.Sprintf(`
		SELECT id, key, scope, scope_id, description, version, created_at, updated_at, created_by, updated_by
		FROM secrets %s ORDER BY key
	`, whereClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []store.Secret
	for rows.Next() {
		var secret store.Secret
		if err := rows.Scan(
			&secret.ID, &secret.Key, &secret.Scope, &secret.ScopeID,
			&secret.Description, &secret.Version,
			&secret.Created, &secret.Updated, &secret.CreatedBy, &secret.UpdatedBy,
		); err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}

	return secrets, nil
}

func (s *SQLiteStore) GetSecretValue(ctx context.Context, key, scope, scopeID string) (string, error) {
	var encryptedValue string

	err := s.db.QueryRowContext(ctx, `
		SELECT encrypted_value FROM secrets WHERE key = ? AND scope = ? AND scope_id = ?
	`, key, scope, scopeID).Scan(&encryptedValue)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}

	return encryptedValue, nil
}

// ============================================================================
// Group Operations
// ============================================================================

func (s *SQLiteStore) CreateGroup(ctx context.Context, group *store.Group) error {
	now := time.Now()
	group.Created = now
	group.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO groups (id, name, slug, description, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		group.ID, group.Name, group.Slug, group.Description, nullableString(group.ParentID),
		marshalJSON(group.Labels), marshalJSON(group.Annotations),
		group.Created, group.Updated, group.CreatedBy, group.OwnerID,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetGroup(ctx context.Context, id string) (*store.Group, error) {
	group := &store.Group{}
	var labels, annotations string
	var parentID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, description, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id
		FROM groups WHERE id = ?
	`, id).Scan(
		&group.ID, &group.Name, &group.Slug, &group.Description, &parentID,
		&labels, &annotations,
		&group.Created, &group.Updated, &group.CreatedBy, &group.OwnerID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if parentID.Valid {
		group.ParentID = parentID.String
	}
	unmarshalJSON(labels, &group.Labels)
	unmarshalJSON(annotations, &group.Annotations)

	return group, nil
}

func (s *SQLiteStore) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM groups WHERE slug = ?", slug).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetGroup(ctx, id)
}

func (s *SQLiteStore) UpdateGroup(ctx context.Context, group *store.Group) error {
	group.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE groups SET
			name = ?, slug = ?, description = ?, parent_id = ?,
			labels = ?, annotations = ?,
			updated_at = ?, owner_id = ?
		WHERE id = ?
	`,
		group.Name, group.Slug, group.Description, nullableString(group.ParentID),
		marshalJSON(group.Labels), marshalJSON(group.Annotations),
		group.Updated, group.OwnerID,
		group.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteGroup(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM groups WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListGroups(ctx context.Context, filter store.GroupFilter, opts store.ListOptions) (*store.ListResult[store.Group], error) {
	var conditions []string
	var args []interface{}

	if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.ParentID != "" {
		conditions = append(conditions, "parent_id = ?")
		args = append(args, filter.ParentID)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM groups %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, slug, description, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id
		FROM groups %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []store.Group
	for rows.Next() {
		var group store.Group
		var labels, annotations string
		var parentID sql.NullString

		if err := rows.Scan(
			&group.ID, &group.Name, &group.Slug, &group.Description, &parentID,
			&labels, &annotations,
			&group.Created, &group.Updated, &group.CreatedBy, &group.OwnerID,
		); err != nil {
			return nil, err
		}

		if parentID.Valid {
			group.ParentID = parentID.String
		}
		unmarshalJSON(labels, &group.Labels)
		unmarshalJSON(annotations, &group.Annotations)

		groups = append(groups, group)
	}

	return &store.ListResult[store.Group]{
		Items:      groups,
		TotalCount: totalCount,
	}, nil
}

func (s *SQLiteStore) AddGroupMember(ctx context.Context, member *store.GroupMember) error {
	if member.AddedAt.IsZero() {
		member.AddedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO group_members (group_id, member_type, member_id, role, added_at, added_by)
		VALUES (?, ?, ?, ?, ?, ?)
	`,
		member.GroupID, member.MemberType, member.MemberID, member.Role, member.AddedAt, member.AddedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM group_members WHERE group_id = ? AND member_type = ? AND member_id = ?",
		groupID, memberType, memberID,
	)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_id, member_type, member_id, role, added_at, added_by
		FROM group_members WHERE group_id = ?
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []store.GroupMember
	for rows.Next() {
		var member store.GroupMember
		if err := rows.Scan(
			&member.GroupID, &member.MemberType, &member.MemberID, &member.Role, &member.AddedAt, &member.AddedBy,
		); err != nil {
			return nil, err
		}
		members = append(members, member)
	}

	return members, nil
}

func (s *SQLiteStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_id, member_type, member_id, role, added_at, added_by
		FROM group_members WHERE member_type = 'user' AND member_id = ?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memberships []store.GroupMember
	for rows.Next() {
		var member store.GroupMember
		if err := rows.Scan(
			&member.GroupID, &member.MemberType, &member.MemberID, &member.Role, &member.AddedAt, &member.AddedBy,
		); err != nil {
			return nil, err
		}
		memberships = append(memberships, member)
	}

	return memberships, nil
}

func (s *SQLiteStore) GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*store.GroupMember, error) {
	member := &store.GroupMember{}

	err := s.db.QueryRowContext(ctx, `
		SELECT group_id, member_type, member_id, role, added_at, added_by
		FROM group_members WHERE group_id = ? AND member_type = ? AND member_id = ?
	`, groupID, memberType, memberID).Scan(
		&member.GroupID, &member.MemberType, &member.MemberID, &member.Role, &member.AddedAt, &member.AddedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	return member, nil
}

// WouldCreateCycle checks if adding memberGroupID as a member of groupID would create a cycle.
// A cycle exists if groupID is reachable from memberGroupID by following the containment relationship.
// Example: if A contains B, and we try to add A as member of B, we'd have A->B->A (cycle).
func (s *SQLiteStore) WouldCreateCycle(ctx context.Context, groupID, memberGroupID string) (bool, error) {
	// If they're the same, it's a direct cycle
	if groupID == memberGroupID {
		return true, nil
	}

	// Check if groupID is reachable from memberGroupID by traversing DOWN the containment graph
	// (i.e., checking what groups memberGroupID contains, and what those contain, etc.)
	visited := make(map[string]bool)
	return s.hasPathDown(ctx, memberGroupID, groupID, visited)
}

// hasPathDown checks if 'target' is reachable from 'current' by following containment.
// It looks at what groups 'current' contains as members.
func (s *SQLiteStore) hasPathDown(ctx context.Context, current, target string, visited map[string]bool) (bool, error) {
	if current == target {
		return true, nil
	}
	if visited[current] {
		return false, nil
	}
	visited[current] = true

	// Get all groups that 'current' contains (groups where current is the group_id)
	rows, err := s.db.QueryContext(ctx,
		"SELECT member_id FROM group_members WHERE member_type = 'group' AND group_id = ?", current)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var childGroupID string
		if err := rows.Scan(&childGroupID); err != nil {
			return false, err
		}
		found, err := s.hasPathDown(ctx, childGroupID, target, visited)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}

	return false, nil
}

// GetEffectiveGroups returns all groups a user belongs to, including transitive memberships.
func (s *SQLiteStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	// Start with direct group memberships
	directMemberships, err := s.GetUserGroups(ctx, userID)
	if err != nil {
		return nil, err
	}

	effectiveGroups := make(map[string]bool)
	for _, m := range directMemberships {
		effectiveGroups[m.GroupID] = true
		// Add transitive group memberships
		if err := s.addTransitiveGroups(ctx, m.GroupID, effectiveGroups); err != nil {
			return nil, err
		}
	}

	result := make([]string, 0, len(effectiveGroups))
	for groupID := range effectiveGroups {
		result = append(result, groupID)
	}

	return result, nil
}

// addTransitiveGroups recursively adds all groups that contain the given group.
func (s *SQLiteStore) addTransitiveGroups(ctx context.Context, groupID string, visited map[string]bool) error {
	// Find all groups where this group is a member
	rows, err := s.db.QueryContext(ctx,
		"SELECT group_id FROM group_members WHERE member_type = 'group' AND member_id = ?", groupID)
	if err != nil {
		return err
	}

	// Collect all parent group IDs first, then close rows before recursing
	// This avoids issues with SQLite connections during recursive queries
	var parentGroupIDs []string
	for rows.Next() {
		var parentGroupID string
		if err := rows.Scan(&parentGroupID); err != nil {
			rows.Close()
			return err
		}
		parentGroupIDs = append(parentGroupIDs, parentGroupID)
	}
	rows.Close()

	// Now recurse after rows are closed
	for _, parentGroupID := range parentGroupIDs {
		if !visited[parentGroupID] {
			visited[parentGroupID] = true
			if err := s.addTransitiveGroups(ctx, parentGroupID, visited); err != nil {
				return err
			}
		}
	}

	return nil
}

// ============================================================================
// Policy Operations
// ============================================================================

func (s *SQLiteStore) CreatePolicy(ctx context.Context, policy *store.Policy) error {
	now := time.Now()
	policy.Created = now
	policy.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policies (id, name, description, scope_type, scope_id, resource_type, resource_id, actions, effect, conditions, priority, labels, annotations, created_at, updated_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		policy.ID, policy.Name, policy.Description, policy.ScopeType, policy.ScopeID,
		policy.ResourceType, policy.ResourceID,
		marshalJSON(policy.Actions), policy.Effect, marshalJSON(policy.Conditions),
		policy.Priority, marshalJSON(policy.Labels), marshalJSON(policy.Annotations),
		policy.Created, policy.Updated, policy.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetPolicy(ctx context.Context, id string) (*store.Policy, error) {
	policy := &store.Policy{}
	var actions, conditions, labels, annotations string

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, scope_type, scope_id, resource_type, resource_id, actions, effect, conditions, priority, labels, annotations, created_at, updated_at, created_by
		FROM policies WHERE id = ?
	`, id).Scan(
		&policy.ID, &policy.Name, &policy.Description, &policy.ScopeType, &policy.ScopeID,
		&policy.ResourceType, &policy.ResourceID,
		&actions, &policy.Effect, &conditions,
		&policy.Priority, &labels, &annotations,
		&policy.Created, &policy.Updated, &policy.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(actions, &policy.Actions)
	unmarshalJSON(conditions, &policy.Conditions)
	unmarshalJSON(labels, &policy.Labels)
	unmarshalJSON(annotations, &policy.Annotations)

	return policy, nil
}

func (s *SQLiteStore) UpdatePolicy(ctx context.Context, policy *store.Policy) error {
	policy.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE policies SET
			name = ?, description = ?, scope_type = ?, scope_id = ?,
			resource_type = ?, resource_id = ?,
			actions = ?, effect = ?, conditions = ?,
			priority = ?, labels = ?, annotations = ?,
			updated_at = ?
		WHERE id = ?
	`,
		policy.Name, policy.Description, policy.ScopeType, policy.ScopeID,
		policy.ResourceType, policy.ResourceID,
		marshalJSON(policy.Actions), policy.Effect, marshalJSON(policy.Conditions),
		policy.Priority, marshalJSON(policy.Labels), marshalJSON(policy.Annotations),
		policy.Updated,
		policy.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeletePolicy(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM policies WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListPolicies(ctx context.Context, filter store.PolicyFilter, opts store.ListOptions) (*store.ListResult[store.Policy], error) {
	var conditions []string
	var args []interface{}

	if filter.ScopeType != "" {
		conditions = append(conditions, "scope_type = ?")
		args = append(args, filter.ScopeType)
	}
	if filter.ScopeID != "" {
		conditions = append(conditions, "scope_id = ?")
		args = append(args, filter.ScopeID)
	}
	if filter.ResourceType != "" {
		conditions = append(conditions, "resource_type = ?")
		args = append(args, filter.ResourceType)
	}
	if filter.Effect != "" {
		conditions = append(conditions, "effect = ?")
		args = append(args, filter.Effect)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM policies %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, description, scope_type, scope_id, resource_type, resource_id, actions, effect, conditions, priority, labels, annotations, created_at, updated_at, created_by
		FROM policies %s ORDER BY priority DESC, created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []store.Policy
	for rows.Next() {
		var policy store.Policy
		var actions, conditions, labels, annotations string

		if err := rows.Scan(
			&policy.ID, &policy.Name, &policy.Description, &policy.ScopeType, &policy.ScopeID,
			&policy.ResourceType, &policy.ResourceID,
			&actions, &policy.Effect, &conditions,
			&policy.Priority, &labels, &annotations,
			&policy.Created, &policy.Updated, &policy.CreatedBy,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(actions, &policy.Actions)
		unmarshalJSON(conditions, &policy.Conditions)
		unmarshalJSON(labels, &policy.Labels)
		unmarshalJSON(annotations, &policy.Annotations)

		policies = append(policies, policy)
	}

	return &store.ListResult[store.Policy]{
		Items:      policies,
		TotalCount: totalCount,
	}, nil
}

func (s *SQLiteStore) AddPolicyBinding(ctx context.Context, binding *store.PolicyBinding) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policy_bindings (policy_id, principal_type, principal_id)
		VALUES (?, ?, ?)
	`,
		binding.PolicyID, binding.PrincipalType, binding.PrincipalID,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) RemovePolicyBinding(ctx context.Context, policyID, principalType, principalID string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM policy_bindings WHERE policy_id = ? AND principal_type = ? AND principal_id = ?",
		policyID, principalType, principalID,
	)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) GetPolicyBindings(ctx context.Context, policyID string) ([]store.PolicyBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT policy_id, principal_type, principal_id
		FROM policy_bindings WHERE policy_id = ?
	`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []store.PolicyBinding
	for rows.Next() {
		var binding store.PolicyBinding
		if err := rows.Scan(&binding.PolicyID, &binding.PrincipalType, &binding.PrincipalID); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}

	return bindings, nil
}

func (s *SQLiteStore) GetPoliciesForPrincipal(ctx context.Context, principalType, principalID string) ([]store.Policy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.description, p.scope_type, p.scope_id, p.resource_type, p.resource_id, p.actions, p.effect, p.conditions, p.priority, p.labels, p.annotations, p.created_at, p.updated_at, p.created_by
		FROM policies p
		INNER JOIN policy_bindings pb ON p.id = pb.policy_id
		WHERE pb.principal_type = ? AND pb.principal_id = ?
		ORDER BY p.priority DESC, p.created_at DESC
	`, principalType, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []store.Policy
	for rows.Next() {
		var policy store.Policy
		var actions, conditions, labels, annotations string

		if err := rows.Scan(
			&policy.ID, &policy.Name, &policy.Description, &policy.ScopeType, &policy.ScopeID,
			&policy.ResourceType, &policy.ResourceID,
			&actions, &policy.Effect, &conditions,
			&policy.Priority, &labels, &annotations,
			&policy.Created, &policy.Updated, &policy.CreatedBy,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(actions, &policy.Actions)
		unmarshalJSON(conditions, &policy.Conditions)
		unmarshalJSON(labels, &policy.Labels)
		unmarshalJSON(annotations, &policy.Annotations)

		policies = append(policies, policy)
	}

	return policies, nil
}

// ============================================================================
// API Key Operations
// ============================================================================

func (s *SQLiteStore) CreateAPIKey(ctx context.Context, key *store.APIKey) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, user_id, name, prefix, key_hash, scopes, revoked, expires_at, last_used, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		key.ID, key.UserID, key.Name, key.Prefix, key.KeyHash,
		marshalJSON(key.Scopes), key.Revoked,
		nullableTimePtr(key.ExpiresAt), nullableTimePtr(key.LastUsed), key.Created,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetAPIKey(ctx context.Context, id string) (*store.APIKey, error) {
	key := &store.APIKey{}
	var scopes string
	var expiresAt, lastUsed sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, prefix, key_hash, scopes, revoked, expires_at, last_used, created_at
		FROM api_keys WHERE id = ?
	`, id).Scan(
		&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.KeyHash,
		&scopes, &key.Revoked, &expiresAt, &lastUsed, &key.Created,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(scopes, &key.Scopes)
	if expiresAt.Valid {
		key.ExpiresAt = &expiresAt.Time
	}
	if lastUsed.Valid {
		key.LastUsed = &lastUsed.Time
	}

	return key, nil
}

func (s *SQLiteStore) GetAPIKeyByHash(ctx context.Context, hash string) (*store.APIKey, error) {
	key := &store.APIKey{}
	var scopes string
	var expiresAt, lastUsed sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, prefix, key_hash, scopes, revoked, expires_at, last_used, created_at
		FROM api_keys WHERE key_hash = ?
	`, hash).Scan(
		&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.KeyHash,
		&scopes, &key.Revoked, &expiresAt, &lastUsed, &key.Created,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(scopes, &key.Scopes)
	if expiresAt.Valid {
		key.ExpiresAt = &expiresAt.Time
	}
	if lastUsed.Valid {
		key.LastUsed = &lastUsed.Time
	}

	return key, nil
}

func (s *SQLiteStore) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*store.APIKey, error) {
	key := &store.APIKey{}
	var scopes string
	var expiresAt, lastUsed sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, prefix, key_hash, scopes, revoked, expires_at, last_used, created_at
		FROM api_keys WHERE prefix = ?
	`, prefix).Scan(
		&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.KeyHash,
		&scopes, &key.Revoked, &expiresAt, &lastUsed, &key.Created,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(scopes, &key.Scopes)
	if expiresAt.Valid {
		key.ExpiresAt = &expiresAt.Time
	}
	if lastUsed.Valid {
		key.LastUsed = &lastUsed.Time
	}

	return key, nil
}

func (s *SQLiteStore) UpdateAPIKey(ctx context.Context, key *store.APIKey) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET
			name = ?, scopes = ?, revoked = ?, expires_at = ?, last_used = ?
		WHERE id = ?
	`,
		key.Name, marshalJSON(key.Scopes), key.Revoked,
		nullableTimePtr(key.ExpiresAt), nullableTimePtr(key.LastUsed),
		key.ID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpdateAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used = ? WHERE id = ?",
		time.Now(), id,
	)
	return err
}

func (s *SQLiteStore) DeleteAPIKey(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListAPIKeys(ctx context.Context, userID string) ([]store.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, prefix, scopes, revoked, expires_at, last_used, created_at
		FROM api_keys WHERE user_id = ? AND revoked = 0
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []store.APIKey
	for rows.Next() {
		var key store.APIKey
		var scopes string
		var expiresAt, lastUsed sql.NullTime

		if err := rows.Scan(
			&key.ID, &key.UserID, &key.Name, &key.Prefix, &scopes,
			&key.Revoked, &expiresAt, &lastUsed, &key.Created,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(scopes, &key.Scopes)
		if expiresAt.Valid {
			key.ExpiresAt = &expiresAt.Time
		}
		if lastUsed.Valid {
			key.LastUsed = &lastUsed.Time
		}

		keys = append(keys, key)
	}

	return keys, nil
}

func (s *SQLiteStore) RevokeUserAPIKeys(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET revoked = 1 WHERE user_id = ?",
		userID,
	)
	return err
}

// nullableTimePtr returns a sql.NullTime for a time pointer.
func nullableTimePtr(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{Valid: false}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// Ensure SQLiteStore implements Store interface
var _ store.Store = (*SQLiteStore)(nil)
