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
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

	// Limit to a single connection so that per-connection PRAGMAs
	// (foreign_keys, journal_mode) are applied consistently. SQLite
	// serializes writes anyway, so this has no performance impact.
	db.SetMaxOpenConns(1)

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

// DB returns the underlying *sql.DB for direct access in tests.
func (s *SQLiteStore) DB() *sql.DB {
	return s.db
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
		migrationV12,
		migrationV13,
		migrationV14,
		migrationV15,
		migrationV16,
		migrationV17,
		migrationV18,
		migrationV19,
		migrationV20,
		migrationV21,
		migrationV22,
		migrationV23,
		migrationV24,
		migrationV25,
		migrationV26,
		migrationV27,
		migrationV28,
		migrationV29,
		migrationV30,
		migrationV31,
		migrationV32,
		migrationV33,
		migrationV34,
		migrationV35,
		migrationV36,
		migrationV37,
		migrationV38,
		migrationV39,
		migrationV40,
		migrationV41,
		migrationV42,
		migrationV43,
		migrationV44,
		migrationV45,
		migrationV46,
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

	// Migrations that require PRAGMA foreign_keys=OFF around the transaction.
	// SQLite ignores PRAGMA changes inside transactions, so we must disable
	// foreign keys before BeginTx and re-enable after Commit. Without this,
	// DROP TABLE on a parent table triggers ON DELETE CASCADE on child tables.
	foreignKeysOffMigrations := map[int]bool{
		40: true, // V40 drops and recreates the groves table
	}

	// Apply pending migrations
	for i, migration := range migrations {
		version := i + 1
		if version <= currentVersion {
			continue
		}

		needsFKOff := foreignKeysOffMigrations[version]
		if needsFKOff {
			if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
				return fmt.Errorf("failed to disable foreign keys for migration %d: %w", version, err)
			}
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			if needsFKOff {
				s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
			}
			return fmt.Errorf("failed to start transaction for migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			if needsFKOff {
				s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
			}
			return fmt.Errorf("failed to apply migration %d: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			tx.Rollback()
			if needsFKOff {
				s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
			}
			return fmt.Errorf("failed to record migration %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			if needsFKOff {
				s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
			}
			return fmt.Errorf("failed to commit migration %d: %w", version, err)
		}

		if needsFKOff {
			if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
				return fmt.Errorf("failed to re-enable foreign keys after migration %d: %w", version, err)
			}
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

// Migration V12: Add injection_mode and secret columns to env_vars
const migrationV12 = `
ALTER TABLE env_vars ADD COLUMN injection_mode TEXT NOT NULL DEFAULT 'as_needed';
ALTER TABLE env_vars ADD COLUMN secret INTEGER NOT NULL DEFAULT 0;
`

const migrationV13 = `
ALTER TABLE secrets ADD COLUMN secret_type TEXT NOT NULL DEFAULT 'environment';
ALTER TABLE secrets ADD COLUMN target TEXT;
`

const migrationV14 = `
ALTER TABLE secrets ADD COLUMN secret_ref TEXT;
`

const migrationV15 = `
UPDATE agents SET status = session_status WHERE session_status IS NOT NULL AND session_status != '';
ALTER TABLE agents DROP COLUMN session_status;
`

// Migration V16: Add harness_configs table
const migrationV16 = `
CREATE TABLE IF NOT EXISTS harness_configs (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL,
	display_name TEXT,
	description TEXT,
	harness TEXT NOT NULL,
	config TEXT,
	content_hash TEXT,
	scope TEXT NOT NULL DEFAULT 'global',
	scope_id TEXT,
	storage_uri TEXT,
	storage_bucket TEXT,
	storage_path TEXT,
	files TEXT,
	locked INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'active',
	owner_id TEXT,
	created_by TEXT,
	updated_by TEXT,
	visibility TEXT NOT NULL DEFAULT 'private',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_harness_configs_slug_scope ON harness_configs(slug, scope);
CREATE INDEX IF NOT EXISTS idx_harness_configs_harness ON harness_configs(harness);
CREATE INDEX IF NOT EXISTS idx_harness_configs_status ON harness_configs(status);
CREATE INDEX IF NOT EXISTS idx_harness_configs_content_hash ON harness_configs(content_hash);
CREATE INDEX IF NOT EXISTS idx_harness_configs_scope_id ON harness_configs(scope, scope_id);
`

// Migration V17: Add deleted_at column to agents for soft-delete support
const migrationV17 = `
ALTER TABLE agents ADD COLUMN deleted_at TIMESTAMP;
CREATE INDEX IF NOT EXISTS idx_agents_deleted ON agents(status, deleted_at) WHERE status = 'deleted';
`

// Migration V18: Notification subscriptions and notifications tables
const migrationV18 = `
CREATE TABLE IF NOT EXISTS notification_subscriptions (
	id TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL,
	subscriber_type TEXT NOT NULL DEFAULT 'agent',
	subscriber_id TEXT NOT NULL,
	grove_id TEXT NOT NULL,
	trigger_statuses TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT NOT NULL,
	FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_notification_subs_agent ON notification_subscriptions(agent_id);
CREATE INDEX IF NOT EXISTS idx_notification_subs_grove ON notification_subscriptions(grove_id);

CREATE TABLE IF NOT EXISTS notifications (
	id TEXT PRIMARY KEY,
	subscription_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	grove_id TEXT NOT NULL,
	subscriber_type TEXT NOT NULL,
	subscriber_id TEXT NOT NULL,
	status TEXT NOT NULL,
	message TEXT NOT NULL,
	dispatched INTEGER NOT NULL DEFAULT 0,
	acknowledged INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (subscription_id) REFERENCES notification_subscriptions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_notifications_subscriber ON notifications(subscriber_type, subscriber_id);
CREATE INDEX IF NOT EXISTS idx_notifications_grove ON notifications(grove_id);
`

const migrationV19 = `
CREATE TABLE IF NOT EXISTS scheduled_events (
	id TEXT PRIMARY KEY,
	grove_id TEXT NOT NULL,
	event_type TEXT NOT NULL,
	fire_at TIMESTAMP NOT NULL,
	payload TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	fired_at TIMESTAMP,
	error TEXT,

	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_scheduled_events_status ON scheduled_events(status);
CREATE INDEX IF NOT EXISTS idx_scheduled_events_fire_at ON scheduled_events(fire_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_scheduled_events_grove ON scheduled_events(grove_id);
`

const migrationV20 = `
ALTER TABLE agents ADD COLUMN phase TEXT NOT NULL DEFAULT 'created';
ALTER TABLE agents ADD COLUMN activity TEXT DEFAULT '';
ALTER TABLE agents ADD COLUMN tool_name TEXT DEFAULT '';

-- Backfill phase/activity from existing status values
UPDATE agents SET phase = 'created' WHERE status IN ('created', 'pending');
UPDATE agents SET phase = 'provisioning' WHERE status = 'provisioning';
UPDATE agents SET phase = 'cloning' WHERE status = 'cloning';
UPDATE agents SET phase = 'running', activity = 'idle' WHERE status = 'running';
UPDATE agents SET phase = 'stopped' WHERE status = 'stopped';
UPDATE agents SET phase = 'error' WHERE status = 'error';
UPDATE agents SET phase = 'running', activity = 'thinking' WHERE status = 'busy';
UPDATE agents SET phase = 'running', activity = 'idle' WHERE status = 'idle';
UPDATE agents SET phase = 'running', activity = 'waiting_for_input' WHERE status = 'waiting_for_input';
UPDATE agents SET phase = 'running', activity = 'completed' WHERE status = 'completed';
UPDATE agents SET phase = 'running', activity = 'limits_exceeded' WHERE status = 'limits_exceeded';
UPDATE agents SET phase = 'stopped' WHERE status IN ('deleted', 'restored');
UPDATE agents SET phase = 'running', activity = 'offline' WHERE status = 'undetermined';

CREATE INDEX IF NOT EXISTS idx_agents_phase ON agents(phase);
`

// Migration V21: Remove legacy status column from agents table.
// Phase 6 of the agent state refactor — the status column is superseded by
// the phase/activity columns added in V20.
const migrationV21 = `
-- Backfill any remaining agents where phase was not set
UPDATE agents SET phase = status WHERE (phase = '' OR phase IS NULL) AND status IN ('created','provisioning','cloning','starting','running','stopping','stopped','error');
UPDATE agents SET phase = 'created' WHERE (phase = '' OR phase IS NULL) AND status = 'pending';
UPDATE agents SET phase = 'stopped' WHERE (phase = '' OR phase IS NULL) AND status = 'deleted';

-- Backfill activity from status for running agents
UPDATE agents SET activity = status WHERE phase = 'running' AND (activity = '' OR activity IS NULL) AND status IN ('idle','waiting_for_input','completed','limits_exceeded','offline');
UPDATE agents SET activity = 'thinking' WHERE phase = 'running' AND (activity = '' OR activity IS NULL) AND status = 'busy';

-- Update soft-delete index: rely on deleted_at instead of status
DROP INDEX IF EXISTS idx_agents_deleted;
CREATE INDEX IF NOT EXISTS idx_agents_deleted ON agents(deleted_at) WHERE deleted_at IS NOT NULL;

-- Drop the status index before dropping the column
DROP INDEX IF EXISTS idx_agents_status;

-- Drop the status column (SQLite supports this from 3.35.0+)
ALTER TABLE agents DROP COLUMN status;
`

// Migration V22: Rename trigger_statuses to trigger_activities in notification_subscriptions.
const migrationV22 = `
ALTER TABLE notification_subscriptions RENAME COLUMN trigger_statuses TO trigger_activities;
`

// Migration V23: Add injection_mode column to secrets
const migrationV23 = `
ALTER TABLE secrets ADD COLUMN injection_mode TEXT NOT NULL DEFAULT 'as_needed';
`

// Migration V24: Add last_activity_event column to agents for stalled detection.
// Backfills existing agents to prevent false positives on upgrade.
const migrationV24 = `
ALTER TABLE agents ADD COLUMN last_activity_event TIMESTAMP;
UPDATE agents SET last_activity_event = COALESCE(last_seen, updated_at, created_at);
`

// Migration V25: Add stalled_from_activity column for stalled detection.
// Records the activity that was active when the agent was marked stalled,
// so heartbeats can distinguish "still stuck" from "genuinely recovered".
const migrationV25 = `
ALTER TABLE agents ADD COLUMN stalled_from_activity TEXT DEFAULT '';
`

// Migration V26: Add limits tracking columns to agents table.
// These fields are updated by sciontool status reports from inside the container.
const migrationV26 = `
ALTER TABLE agents ADD COLUMN current_turns INTEGER DEFAULT 0;
ALTER TABLE agents ADD COLUMN current_model_calls INTEGER DEFAULT 0;
ALTER TABLE agents ADD COLUMN started_at TIMESTAMP;
`

const migrationV27 = `
ALTER TABLE users ADD COLUMN last_seen TIMESTAMP;
`

// Migration V28: Add shared_dirs column to groves table.
// Stores grove-level shared directory configuration as JSON.
const migrationV28 = `
ALTER TABLE groves ADD COLUMN shared_dirs TEXT DEFAULT '';
`

// Migration V29: Add group_type and grove_id columns to groups table.
// These enable filtering groups by type and grove association.
const migrationV29 = `
ALTER TABLE groups ADD COLUMN group_type TEXT NOT NULL DEFAULT 'explicit';
ALTER TABLE groups ADD COLUMN grove_id TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_groups_grove ON groups(grove_id);
`

// Migration V30: Create gcp_service_accounts table for GCP identity management.
const migrationV30 = `
CREATE TABLE IF NOT EXISTS gcp_service_accounts (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	scope_id TEXT NOT NULL,
	email TEXT NOT NULL,
	project_id TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	default_scopes TEXT NOT NULL DEFAULT '',
	verified INTEGER NOT NULL DEFAULT 0,
	verified_at TIMESTAMP,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(email, scope, scope_id)
);
CREATE INDEX IF NOT EXISTS idx_gcp_sa_scope ON gcp_service_accounts(scope, scope_id);
`

// Migration V31: Add scope column to notification_subscriptions and make agent_id nullable.
// Enables grove-scoped subscriptions (watch all agents in a grove) in addition to
// agent-scoped subscriptions. Adds unique constraint for deduplication.
const migrationV31 = `
-- SQLite doesn't support ALTER COLUMN, so we recreate the table.
CREATE TABLE notification_subscriptions_new (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL DEFAULT 'agent',
	agent_id TEXT,
	subscriber_type TEXT NOT NULL DEFAULT 'agent',
	subscriber_id TEXT NOT NULL,
	grove_id TEXT NOT NULL,
	trigger_activities TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT NOT NULL,
	FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Copy existing data (all existing subscriptions are agent-scoped)
INSERT INTO notification_subscriptions_new
	(id, scope, agent_id, subscriber_type, subscriber_id, grove_id, trigger_activities, created_at, created_by)
SELECT id, 'agent', agent_id, subscriber_type, subscriber_id, grove_id, trigger_activities, created_at, created_by
FROM notification_subscriptions;

DROP TABLE notification_subscriptions;
ALTER TABLE notification_subscriptions_new RENAME TO notification_subscriptions;

-- Recreate indexes
CREATE INDEX IF NOT EXISTS idx_notification_subs_agent ON notification_subscriptions(agent_id);
CREATE INDEX IF NOT EXISTS idx_notification_subs_grove ON notification_subscriptions(grove_id);
CREATE INDEX IF NOT EXISTS idx_notification_subs_subscriber ON notification_subscriptions(subscriber_type, subscriber_id);

-- Unique constraint: one subscription per (scope, target, subscriber, grove)
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_subs_unique
	ON notification_subscriptions(scope, COALESCE(agent_id, ''), subscriber_type, subscriber_id, grove_id);
`

// Migration V32: Recurring schedules table and schedule_id FK on scheduled_events.
const migrationV32 = `
CREATE TABLE IF NOT EXISTS schedules (
	id TEXT PRIMARY KEY,
	grove_id TEXT NOT NULL,
	name TEXT NOT NULL,
	cron_expr TEXT NOT NULL,
	event_type TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'active',
	next_run_at TIMESTAMP,
	last_run_at TIMESTAMP,
	last_run_status TEXT,
	last_run_error TEXT,
	run_count INTEGER NOT NULL DEFAULT 0,
	error_count INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE,
	UNIQUE(grove_id, name)
);
CREATE INDEX IF NOT EXISTS idx_schedules_grove ON schedules(grove_id);
CREATE INDEX IF NOT EXISTS idx_schedules_next_run ON schedules(next_run_at) WHERE status = 'active';

ALTER TABLE scheduled_events ADD COLUMN schedule_id TEXT DEFAULT '';
`

// Migration V33: Subscription templates table.
const migrationV33 = `
CREATE TABLE IF NOT EXISTS subscription_templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT 'grove',
	trigger_activities TEXT NOT NULL,
	grove_id TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL,
	UNIQUE(grove_id, name)
);
CREATE INDEX IF NOT EXISTS idx_sub_templates_grove ON subscription_templates(grove_id);
`

// Migration V34: User access tokens table (replaces api_keys).
const migrationV34 = `
CREATE TABLE IF NOT EXISTS user_access_tokens (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	name TEXT NOT NULL,
	prefix TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	grove_id TEXT NOT NULL,
	scopes TEXT NOT NULL,
	revoked INTEGER NOT NULL DEFAULT 0,
	expires_at TIMESTAMP,
	last_used TIMESTAMP,
	created_at TIMESTAMP NOT NULL,
	FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_uat_user_id ON user_access_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_uat_key_hash ON user_access_tokens(key_hash);
`

// Migration V35: GitHub App installations and grove GitHub App fields.
const migrationV35 = `
CREATE TABLE IF NOT EXISTS github_installations (
	installation_id INTEGER PRIMARY KEY,
	account_login TEXT NOT NULL,
	account_type TEXT NOT NULL DEFAULT 'Organization',
	app_id INTEGER NOT NULL,
	repositories TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'active',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_github_installations_account ON github_installations(account_login);
CREATE INDEX IF NOT EXISTS idx_github_installations_status ON github_installations(status);

ALTER TABLE groves ADD COLUMN github_installation_id INTEGER;
ALTER TABLE groves ADD COLUMN github_permissions TEXT;
ALTER TABLE groves ADD COLUMN github_app_status TEXT;
`

// Migration V36: Git identity configuration for commit attribution.
const migrationV36 = `
ALTER TABLE groves ADD COLUMN git_identity TEXT;
`

// Migration V37: Add ancestry column for transitive access control.
const migrationV37 = `
ALTER TABLE agents ADD COLUMN ancestry TEXT;
`

// Migration V38: Backfill ancestry for existing agents from created_by.
const migrationV38 = `
UPDATE agents SET ancestry = json_array(created_by)
WHERE created_by IS NOT NULL AND created_by != '' AND ancestry IS NULL;
`

// Migration V39: Messages table for bidirectional human-agent messaging.
const migrationV39 = `
CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	grove_id TEXT NOT NULL,
	sender TEXT NOT NULL,
	sender_id TEXT NOT NULL DEFAULT '',
	recipient TEXT NOT NULL,
	recipient_id TEXT NOT NULL DEFAULT '',
	msg TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'instruction',
	urgent INTEGER NOT NULL DEFAULT 0,
	broadcasted INTEGER NOT NULL DEFAULT 0,
	read INTEGER NOT NULL DEFAULT 0,
	agent_id TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_messages_grove ON messages(grove_id);
CREATE INDEX IF NOT EXISTS idx_messages_recipient ON messages(recipient_id, read);
CREATE INDEX IF NOT EXISTS idx_messages_agent ON messages(agent_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at DESC);
`

// Migration V40: Allow multiple groves per git remote (drop UNIQUE on git_remote),
// and enforce slug uniqueness (add UNIQUE on slug). Requires table recreation
// because SQLite does not support ALTER TABLE DROP CONSTRAINT.
//
// IMPORTANT: This migration requires foreign_keys=OFF around the DROP TABLE.
// SQLite ignores PRAGMA changes inside transactions, so the migration runner
// handles this via the foreignKeysOffMigrations set. The PRAGMA statements are
// intentionally NOT included in the SQL string.
const migrationV40 = `
CREATE TABLE IF NOT EXISTS groves_new (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL UNIQUE,
	git_remote TEXT,
	labels TEXT,
	annotations TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	owner_id TEXT,
	visibility TEXT NOT NULL DEFAULT 'private',
	default_runtime_broker_id TEXT REFERENCES runtime_brokers(id) ON DELETE SET NULL,
	shared_dirs TEXT,
	github_installation_id INTEGER REFERENCES github_installations(installation_id),
	github_permissions TEXT,
	github_app_status TEXT,
	git_identity TEXT
);

INSERT OR IGNORE INTO groves_new SELECT
	id, name, slug, git_remote, labels, annotations,
	created_at, updated_at, created_by, owner_id, visibility,
	default_runtime_broker_id, shared_dirs,
	github_installation_id, github_permissions, github_app_status,
	git_identity
FROM groves;

DROP TABLE IF EXISTS groves;
ALTER TABLE groves_new RENAME TO groves;

CREATE INDEX IF NOT EXISTS idx_groves_slug ON groves(slug);
CREATE INDEX IF NOT EXISTS idx_groves_git_remote ON groves(git_remote);
CREATE INDEX IF NOT EXISTS idx_groves_owner ON groves(owner_id);
CREATE INDEX IF NOT EXISTS idx_groves_default_runtime_broker ON groves(default_runtime_broker_id);
`

// Migration V41: Maintenance operations tables for the admin maintenance panel.
// Tracks one-time migrations and repeatable operations with execution history.
const migrationV41 = `
CREATE TABLE IF NOT EXISTS maintenance_operations (
    id          TEXT PRIMARY KEY,
    key         TEXT NOT NULL UNIQUE,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  TIMESTAMP,
    completed_at TIMESTAMP,
    started_by  TEXT,
    result      TEXT,
    metadata    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_maintenance_ops_category ON maintenance_operations(category);
CREATE INDEX IF NOT EXISTS idx_maintenance_ops_status ON maintenance_operations(status);

CREATE TABLE IF NOT EXISTS maintenance_operation_runs (
    id            TEXT PRIMARY KEY,
    operation_key TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'running',
    started_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at  TIMESTAMP,
    started_by    TEXT,
    result        TEXT,
    log           TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (operation_key) REFERENCES maintenance_operations(key)
);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_key ON maintenance_operation_runs(operation_key);
CREATE INDEX IF NOT EXISTS idx_maintenance_runs_started ON maintenance_operation_runs(started_at DESC);

-- Seed: one-time migrations
INSERT INTO maintenance_operations (id, key, title, description, category, status)
VALUES (
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
    'secret-hub-id-migration',
    'Secret Hub ID Namespace Migration',
    'Migrates hub-scoped secrets from the legacy fixed "hub" scope ID to the per-instance hub ID. Required when upgrading a hub that was created before the hub ID namespacing feature. Only needed for GCP Secret Manager backend.',
    'migration',
    'pending'
);

-- Seed: repeatable operations
INSERT INTO maintenance_operations (id, key, title, description, category, status)
VALUES (
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
    'pull-images',
    'Pull Container Images',
    'Pulls the latest container images for all configured harnesses from the image registry.',
    'operation',
    'pending'
);

INSERT INTO maintenance_operations (id, key, title, description, category, status)
VALUES (
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
    'rebuild-server',
    'Rebuild Server from Git',
    'Pulls latest code from the repository, rebuilds the server binary and web assets, then restarts the hub service. Equivalent to the fast-deploy mode of gce-start-hub.sh.',
    'operation',
    'pending'
);

INSERT INTO maintenance_operations (id, key, title, description, category, status)
VALUES (
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
    'rebuild-web',
    'Rebuild Web Frontend',
    'Rebuilds only the web frontend assets from source without restarting the server binary. Changes take effect on the next page load.',
    'operation',
    'pending'
);
`

const migrationV42 = `
CREATE TABLE IF NOT EXISTS grove_sync_state (
	grove_id TEXT NOT NULL,
	broker_id TEXT NOT NULL DEFAULT '',
	last_sync_time TIMESTAMP,
	last_commit_sha TEXT,
	file_count INTEGER NOT NULL DEFAULT 0,
	total_bytes INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (grove_id, broker_id),
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_grove_sync_state_grove ON grove_sync_state(grove_id);
`

// migrationV43 fixes pre-existing signing key secrets that were stored with
// the default secret_type ('environment' or ”) instead of 'internal'. Without
// this, stale rows created before the fix would still be resolved and injected
// into agent containers.
const migrationV43 = `
UPDATE secrets SET secret_type = 'internal'
WHERE key IN ('agent_signing_key', 'user_signing_key')
  AND scope = 'hub'
  AND secret_type != 'internal';
`

// Migration V44: Add managed and managed_by columns to gcp_service_accounts table
// for hub-minted service accounts.
const migrationV44 = `
ALTER TABLE gcp_service_accounts ADD COLUMN managed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gcp_service_accounts ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';
`

// Migration V45: Add allow_progeny column to secrets table
const migrationV45 = `
ALTER TABLE secrets ADD COLUMN allow_progeny INTEGER NOT NULL DEFAULT 0;
`

const migrationV46 = `
ALTER TABLE templates ADD COLUMN default_harness_config TEXT;
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

// nullableInt64 returns a sql.NullInt64 for database insertion.
// Nil pointers become NULL.
func nullableInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// marshalJSONPtr marshals a pointer value to JSON string, returning empty string for nil pointers.
// Unlike marshalJSON, this correctly detects nil typed pointers.
func marshalJSONPtr[T any](v *T) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
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
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at,
			created_by, owner_id, visibility, state_version, ancestry
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		agent.ID, agent.Slug, agent.Name, agent.Template, agent.GroveID,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Phase, agent.Activity, agent.ToolName,
		agent.ConnectionState, agent.ContainerStatus, agent.RuntimeState,
		agent.StalledFromActivity,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeBrokerID), agent.WebPTYEnabled, agent.TaskSummary, agent.Message,
		marshalJSON(agent.AppliedConfig),
		agent.Created, agent.Updated, nullableTime(agent.LastSeen), nullableTime(agent.LastActivityEvent), nullableTime(agent.DeletedAt),
		agent.CreatedBy, agent.OwnerID, agent.Visibility, agent.StateVersion, marshalJSON(agent.Ancestry),
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
	var lastSeen, lastActivityEvent, deletedAt, startedAt sql.NullTime
	var runtimeBrokerID, message, toolName, ancestry sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			current_turns, current_model_calls,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at, started_at,
			created_by, owner_id, visibility, state_version, ancestry
		FROM agents WHERE id = ?
	`, id).Scan(
		&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Phase, &agent.Activity, &toolName,
		&agent.ConnectionState, &agent.ContainerStatus, &agent.RuntimeState,
		&agent.StalledFromActivity,
		&agent.CurrentTurns, &agent.CurrentModelCalls,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
		&appliedConfig,
		&agent.Created, &agent.Updated, &lastSeen, &lastActivityEvent, &deletedAt, &startedAt,
		&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion, &ancestry,
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
	unmarshalJSON(ancestry.String, &agent.Ancestry)
	if lastSeen.Valid {
		agent.LastSeen = lastSeen.Time
	}
	if lastActivityEvent.Valid {
		agent.LastActivityEvent = lastActivityEvent.Time
	}
	if deletedAt.Valid {
		agent.DeletedAt = deletedAt.Time
	}
	if startedAt.Valid {
		agent.StartedAt = startedAt.Time
	}
	if runtimeBrokerID.Valid {
		agent.RuntimeBrokerID = runtimeBrokerID.String
	}
	if message.Valid {
		agent.Message = message.String
	}
	if toolName.Valid {
		agent.ToolName = toolName.String
	}

	return agent, nil
}

func (s *SQLiteStore) GetAgentBySlug(ctx context.Context, groveID, slug string) (*store.Agent, error) {
	agent := &store.Agent{}
	var labels, annotations, appliedConfig string
	var lastSeen, lastActivityEvent, deletedAt, startedAt sql.NullTime
	var runtimeBrokerID, message, toolName, ancestry sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			current_turns, current_model_calls,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at, started_at,
			created_by, owner_id, visibility, state_version, ancestry
		FROM agents WHERE grove_id = ? AND agent_id = ?
	`, groveID, slug).Scan(
		&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Phase, &agent.Activity, &toolName,
		&agent.ConnectionState, &agent.ContainerStatus, &agent.RuntimeState,
		&agent.StalledFromActivity,
		&agent.CurrentTurns, &agent.CurrentModelCalls,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
		&appliedConfig,
		&agent.Created, &agent.Updated, &lastSeen, &lastActivityEvent, &deletedAt, &startedAt,
		&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion, &ancestry,
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
	unmarshalJSON(ancestry.String, &agent.Ancestry)
	if lastSeen.Valid {
		agent.LastSeen = lastSeen.Time
	}
	if lastActivityEvent.Valid {
		agent.LastActivityEvent = lastActivityEvent.Time
	}
	if deletedAt.Valid {
		agent.DeletedAt = deletedAt.Time
	}
	if startedAt.Valid {
		agent.StartedAt = startedAt.Time
	}
	if runtimeBrokerID.Valid {
		agent.RuntimeBrokerID = runtimeBrokerID.String
	}
	if message.Valid {
		agent.Message = message.String
	}
	if toolName.Valid {
		agent.ToolName = toolName.String
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
			phase = ?, activity = ?, tool_name = ?,
			connection_state = ?, container_status = ?, runtime_state = ?,
			stalled_from_activity = ?,
			image = ?, detached = ?, runtime = ?, runtime_broker_id = ?, web_pty_enabled = ?, task_summary = ?, message = ?,
			applied_config = ?,
			updated_at = ?, last_seen = ?, last_activity_event = ?, deleted_at = ?,
			owner_id = ?, visibility = ?, state_version = ?
		WHERE id = ? AND state_version = ?
	`,
		agent.Slug, agent.Name, agent.Template,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Phase, agent.Activity, agent.ToolName,
		agent.ConnectionState, agent.ContainerStatus, agent.RuntimeState,
		agent.StalledFromActivity,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeBrokerID), agent.WebPTYEnabled, agent.TaskSummary, agent.Message,
		marshalJSON(agent.AppliedConfig),
		agent.Updated, nullableTime(agent.LastSeen), nullableTime(agent.LastActivityEvent), nullableTime(agent.DeletedAt),
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

	if len(filter.MemberOrOwnerGroveIDs) > 0 {
		// Combine grove_id membership with owner_id match using OR
		placeholders := make([]string, len(filter.MemberOrOwnerGroveIDs))
		for i, id := range filter.MemberOrOwnerGroveIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		orParts := []string{"grove_id IN (" + strings.Join(placeholders, ",") + ")"}
		if filter.OwnerID != "" {
			orParts = append(orParts, "owner_id = ?")
			args = append(args, filter.OwnerID)
		}
		conditions = append(conditions, "("+strings.Join(orParts, " OR ")+")")
	} else if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.GroveID != "" {
		conditions = append(conditions, "grove_id = ?")
		args = append(args, filter.GroveID)
	}
	if filter.RuntimeBrokerID != "" {
		conditions = append(conditions, "runtime_broker_id = ?")
		args = append(args, filter.RuntimeBrokerID)
	}
	if filter.Phase != "" {
		conditions = append(conditions, "phase = ?")
		args = append(args, filter.Phase)
	}
	if filter.AncestorID != "" {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM json_each(ancestry) WHERE json_each.value = ?)")
		args = append(args, filter.AncestorID)
	}

	// Exclude soft-deleted agents unless explicitly requested
	if !filter.IncludeDeleted {
		conditions = append(conditions, "deleted_at IS NULL")
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
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			current_turns, current_model_calls,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at, started_at,
			created_by, owner_id, visibility, state_version, ancestry
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
		var lastSeen, lastActivityEvent, deletedAt, startedAt sql.NullTime
		var runtimeBrokerID, message, toolName, ancestry sql.NullString

		if err := rows.Scan(
			&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
			&labels, &annotations,
			&agent.Phase, &agent.Activity, &toolName,
			&agent.ConnectionState, &agent.ContainerStatus, &agent.RuntimeState,
			&agent.StalledFromActivity,
			&agent.CurrentTurns, &agent.CurrentModelCalls,
			&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
			&appliedConfig,
			&agent.Created, &agent.Updated, &lastSeen, &lastActivityEvent, &deletedAt, &startedAt,
			&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion, &ancestry,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(labels, &agent.Labels)
		unmarshalJSON(annotations, &agent.Annotations)
		unmarshalJSON(appliedConfig, &agent.AppliedConfig)
		unmarshalJSON(ancestry.String, &agent.Ancestry)
		if lastSeen.Valid {
			agent.LastSeen = lastSeen.Time
		}
		if lastActivityEvent.Valid {
			agent.LastActivityEvent = lastActivityEvent.Time
		}
		if deletedAt.Valid {
			agent.DeletedAt = deletedAt.Time
		}
		if startedAt.Valid {
			agent.StartedAt = startedAt.Time
		}
		if runtimeBrokerID.Valid {
			agent.RuntimeBrokerID = runtimeBrokerID.String
		}
		if message.Valid {
			agent.Message = message.String
		}
		if toolName.Valid {
			agent.ToolName = toolName.String
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

func (s *SQLiteStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	now := time.Now()

	// When activity is being updated to something other than "executing",
	// clear tool_name (it's only meaningful during execution).
	// We signal this by setting the activity-provided flag.
	activityProvided := su.Activity != ""

	// Prepare nullable values for limits tracking fields
	var currentTurnsProvided bool
	var currentTurnsVal int
	if su.CurrentTurns != nil {
		currentTurnsProvided = true
		currentTurnsVal = *su.CurrentTurns
	}
	var currentModelCallsProvided bool
	var currentModelCallsVal int
	if su.CurrentModelCalls != nil {
		currentModelCallsProvided = true
		currentModelCallsVal = *su.CurrentModelCalls
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE agents SET
			phase = COALESCE(NULLIF(?, ''), phase),
			activity = CASE WHEN ? != '' THEN ? ELSE activity END,
			tool_name = CASE WHEN ? THEN ? ELSE tool_name END,
			message = COALESCE(NULLIF(?, ''), message),
			connection_state = COALESCE(NULLIF(?, ''), connection_state),
			container_status = COALESCE(NULLIF(?, ''), container_status),
			runtime_state = COALESCE(NULLIF(?, ''), runtime_state),
			task_summary = COALESCE(NULLIF(?, ''), task_summary),
			stalled_from_activity = CASE WHEN ? != '' THEN '' ELSE stalled_from_activity END,
			last_activity_event = CASE WHEN ? != '' THEN ? ELSE last_activity_event END,
			current_turns = CASE WHEN ? THEN ? ELSE current_turns END,
			current_model_calls = CASE WHEN ? THEN ? ELSE current_model_calls END,
			started_at = COALESCE(NULLIF(?, ''), started_at),
			updated_at = ?,
			last_seen = ?
		WHERE id = ?
	`,
		su.Phase,
		su.Activity, su.Activity,
		activityProvided, su.ToolName,
		su.Message, su.ConnectionState, su.ContainerStatus,
		su.RuntimeState, su.TaskSummary,
		su.Activity,
		su.Activity, now,
		currentTurnsProvided, currentTurnsVal,
		currentModelCallsProvided, currentModelCallsVal,
		su.StartedAt,
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

func (s *SQLiteStore) PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM agents WHERE deleted_at IS NOT NULL AND deleted_at < ?",
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(rowsAffected), nil
}

func (s *SQLiteStore) MarkStaleAgentsOffline(ctx context.Context, threshold time.Time) ([]store.Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now()

	// Update stale agents to offline activity.
	// Only affects agents that:
	// - Have reported at least one heartbeat (last_seen IS NOT NULL)
	// - Are in the running phase
	// - Are not already in a terminal/sticky activity (completed, limits_exceeded, offline)
	_, err = tx.ExecContext(ctx, `
		UPDATE agents SET
			activity = 'offline',
			updated_at = ?
		WHERE last_seen < ?
		  AND last_seen IS NOT NULL
		  AND phase = 'running'
		  AND activity NOT IN ('completed', 'limits_exceeded', 'blocked', 'offline')
	`, now, threshold)
	if err != nil {
		return nil, err
	}

	// Fetch the agents that were just updated.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			current_turns, current_model_calls,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at, started_at,
			created_by, owner_id, visibility, state_version, ancestry
		FROM agents
		WHERE activity = 'offline' AND updated_at = ?
		  AND last_seen < ?
		  AND last_seen IS NOT NULL
		  AND phase = 'running'
	`, now, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []store.Agent
	for rows.Next() {
		var agent store.Agent
		var labels, annotations, appliedConfig string
		var lastSeen, lastActivityEvent, deletedAt, startedAt sql.NullTime
		var runtimeBrokerID, message, toolName, ancestry sql.NullString

		if err := rows.Scan(
			&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
			&labels, &annotations,
			&agent.Phase, &agent.Activity, &toolName,
			&agent.ConnectionState, &agent.ContainerStatus, &agent.RuntimeState,
			&agent.StalledFromActivity,
			&agent.CurrentTurns, &agent.CurrentModelCalls,
			&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
			&appliedConfig,
			&agent.Created, &agent.Updated, &lastSeen, &lastActivityEvent, &deletedAt, &startedAt,
			&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion, &ancestry,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(labels, &agent.Labels)
		unmarshalJSON(annotations, &agent.Annotations)
		unmarshalJSON(appliedConfig, &agent.AppliedConfig)
		unmarshalJSON(ancestry.String, &agent.Ancestry)
		if lastSeen.Valid {
			agent.LastSeen = lastSeen.Time
		}
		if lastActivityEvent.Valid {
			agent.LastActivityEvent = lastActivityEvent.Time
		}
		if deletedAt.Valid {
			agent.DeletedAt = deletedAt.Time
		}
		if startedAt.Valid {
			agent.StartedAt = startedAt.Time
		}
		if runtimeBrokerID.Valid {
			agent.RuntimeBrokerID = runtimeBrokerID.String
		}
		if message.Valid {
			agent.Message = message.String
		}
		if toolName.Valid {
			agent.ToolName = toolName.String
		}

		agents = append(agents, agent)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return agents, nil
}

func (s *SQLiteStore) MarkStalledAgents(ctx context.Context, activityThreshold, heartbeatRecency time.Time) ([]store.Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now()

	// Update agents to stalled activity.
	// Only affects agents that:
	// - Have a stale last_activity_event (older than activityThreshold)
	// - Have a recent heartbeat (last_seen >= heartbeatRecency) — process is alive
	// - Are in the running phase
	// - Are not already in a terminal/sticky/waiting activity or already stalled/offline
	_, err = tx.ExecContext(ctx, `
		UPDATE agents SET
			stalled_from_activity = activity,
			activity = 'stalled',
			updated_at = ?
		WHERE last_activity_event < ?
		  AND last_activity_event IS NOT NULL
		  AND last_seen >= ?
		  AND last_seen IS NOT NULL
		  AND phase = 'running'
		  AND activity NOT IN ('completed', 'limits_exceeded', 'blocked', 'stalled', 'offline', 'idle', 'waiting_for_input')
	`, now, activityThreshold, heartbeatRecency)
	if err != nil {
		return nil, err
	}

	// Fetch the agents that were just updated.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			phase, activity, tool_name,
			connection_state, container_status, runtime_state,
			stalled_from_activity,
			current_turns, current_model_calls,
			image, detached, runtime, runtime_broker_id, web_pty_enabled, task_summary, message,
			applied_config,
			created_at, updated_at, last_seen, last_activity_event, deleted_at, started_at,
			created_by, owner_id, visibility, state_version, ancestry
		FROM agents
		WHERE activity = 'stalled' AND updated_at = ?
		  AND last_activity_event < ?
		  AND last_activity_event IS NOT NULL
		  AND last_seen >= ?
		  AND last_seen IS NOT NULL
		  AND phase = 'running'
	`, now, activityThreshold, heartbeatRecency)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []store.Agent
	for rows.Next() {
		var agent store.Agent
		var labels, annotations, appliedConfig string
		var lastSeen, lastActivityEvent, deletedAt, startedAt sql.NullTime
		var runtimeBrokerID, message, toolName, ancestry sql.NullString

		if err := rows.Scan(
			&agent.ID, &agent.Slug, &agent.Name, &agent.Template, &agent.GroveID,
			&labels, &annotations,
			&agent.Phase, &agent.Activity, &toolName,
			&agent.ConnectionState, &agent.ContainerStatus, &agent.RuntimeState,
			&agent.StalledFromActivity,
			&agent.CurrentTurns, &agent.CurrentModelCalls,
			&agent.Image, &agent.Detached, &agent.Runtime, &runtimeBrokerID, &agent.WebPTYEnabled, &agent.TaskSummary, &message,
			&appliedConfig,
			&agent.Created, &agent.Updated, &lastSeen, &lastActivityEvent, &deletedAt, &startedAt,
			&agent.CreatedBy, &agent.OwnerID, &agent.Visibility, &agent.StateVersion, &ancestry,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(labels, &agent.Labels)
		unmarshalJSON(annotations, &agent.Annotations)
		unmarshalJSON(appliedConfig, &agent.AppliedConfig)
		unmarshalJSON(ancestry.String, &agent.Ancestry)
		if lastSeen.Valid {
			agent.LastSeen = lastSeen.Time
		}
		if lastActivityEvent.Valid {
			agent.LastActivityEvent = lastActivityEvent.Time
		}
		if deletedAt.Valid {
			agent.DeletedAt = deletedAt.Time
		}
		if startedAt.Valid {
			agent.StartedAt = startedAt.Time
		}
		if runtimeBrokerID.Valid {
			agent.RuntimeBrokerID = runtimeBrokerID.String
		}
		if message.Valid {
			agent.Message = message.String
		}
		if toolName.Valid {
			agent.ToolName = toolName.String
		}

		agents = append(agents, agent)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return agents, nil
}

// ============================================================================
// Grove Operations
// ============================================================================

func (s *SQLiteStore) CreateGrove(ctx context.Context, grove *store.Grove) error {
	now := time.Now()
	grove.Created = now
	grove.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO groves (id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, shared_dirs, created_at, updated_at, created_by, owner_id, visibility, github_installation_id, github_permissions, github_app_status, git_identity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		grove.ID, grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeBrokerID),
		marshalJSON(grove.Labels), marshalJSON(grove.Annotations), marshalJSON(grove.SharedDirs),
		grove.Created, grove.Updated, grove.CreatedBy, grove.OwnerID, grove.Visibility,
		nullableInt64(grove.GitHubInstallationID), marshalJSONPtr(grove.GitHubPermissions), marshalJSONPtr(grove.GitHubAppStatus),
		marshalJSONPtr(grove.GitIdentity),
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
	var labels, annotations, sharedDirs string
	var gitRemote, defaultRuntimeBrokerID sql.NullString
	var githubInstallationID sql.NullInt64
	var githubPermissions, githubAppStatus, gitIdentity string

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, shared_dirs, created_at, updated_at, created_by, owner_id, visibility, github_installation_id, COALESCE(github_permissions, ''), COALESCE(github_app_status, ''), COALESCE(git_identity, '')
		FROM groves WHERE id = ?
	`, id).Scan(
		&grove.ID, &grove.Name, &grove.Slug, &gitRemote, &defaultRuntimeBrokerID,
		&labels, &annotations, &sharedDirs,
		&grove.Created, &grove.Updated, &grove.CreatedBy, &grove.OwnerID, &grove.Visibility,
		&githubInstallationID, &githubPermissions, &githubAppStatus, &gitIdentity,
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
	if githubInstallationID.Valid {
		id := githubInstallationID.Int64
		grove.GitHubInstallationID = &id
	}
	unmarshalJSON(labels, &grove.Labels)
	unmarshalJSON(annotations, &grove.Annotations)
	unmarshalJSON(sharedDirs, &grove.SharedDirs)
	if githubPermissions != "" {
		grove.GitHubPermissions = &store.GitHubTokenPermissions{}
		unmarshalJSON(githubPermissions, grove.GitHubPermissions)
	}
	if githubAppStatus != "" {
		grove.GitHubAppStatus = &store.GitHubAppGroveStatus{}
		unmarshalJSON(githubAppStatus, grove.GitHubAppStatus)
	}
	if gitIdentity != "" {
		grove.GitIdentity = &store.GitIdentityConfig{}
		unmarshalJSON(gitIdentity, grove.GitIdentity)
	}

	// Populate computed fields
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", id).Scan(&grove.AgentCount)
	s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online')
		     + (SELECT COUNT(*) FROM runtime_brokers WHERE auto_provide = 1 AND status = 'online'
		            AND id NOT IN (SELECT broker_id FROM grove_contributors WHERE grove_id = ?))
	`, id, id).Scan(&grove.ActiveBrokerCount)
	s.populateGroveType(ctx, grove)

	return grove, nil
}

// populateGroveType sets the computed GroveType field based on how the grove was established.
// Type is "linked" (pre-existing local grove linked to Hub) or "hub-native" (created via Hub).
// Whether a grove is git-backed is orthogonal — indicated by the GitRemote field.
func (s *SQLiteStore) populateGroveType(ctx context.Context, grove *store.Grove) {
	// Check if any provider has a local_path not under ~/.scion/groves/ (i.e. broker-linked)
	var linkedCount int
	s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND local_path != '' AND local_path NOT LIKE '%/.scion/groves/%'",
		grove.ID).Scan(&linkedCount)
	if linkedCount > 0 {
		grove.GroveType = store.GroveTypeLinked
		return
	}
	grove.GroveType = store.GroveTypeHubNative
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

func (s *SQLiteStore) GetGrovesByGitRemote(ctx context.Context, gitRemote string) ([]*store.Grove, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM groves WHERE git_remote = ? ORDER BY created_at ASC", gitRemote)
	if err != nil {
		return nil, err
	}

	// Collect all IDs first, then close the cursor before calling GetGrove
	// (SQLite single-connection can't serve a new query while rows are open).
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	groves := make([]*store.Grove, 0, len(ids))
	for _, id := range ids {
		grove, err := s.GetGrove(ctx, id)
		if err != nil {
			return nil, err
		}
		groves = append(groves, grove)
	}
	return groves, nil
}

func (s *SQLiteStore) NextAvailableSlug(ctx context.Context, baseSlug string) (string, error) {
	// Check if the base slug is available
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groves WHERE slug = ?", baseSlug).Scan(&count); err != nil {
		return "", err
	}
	if count == 0 {
		return baseSlug, nil
	}

	// Find the next available serial suffix
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d", baseSlug, i)
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groves WHERE slug = ?", candidate).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
}

func (s *SQLiteStore) UpdateGrove(ctx context.Context, grove *store.Grove) error {
	grove.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE groves SET
			name = ?, slug = ?, git_remote = ?, default_runtime_broker_id = ?,
			labels = ?, annotations = ?, shared_dirs = ?,
			updated_at = ?, owner_id = ?, visibility = ?,
			github_installation_id = ?, github_permissions = ?, github_app_status = ?,
			git_identity = ?
		WHERE id = ?
	`,
		grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeBrokerID),
		marshalJSON(grove.Labels), marshalJSON(grove.Annotations), marshalJSON(grove.SharedDirs),
		grove.Updated, grove.OwnerID, grove.Visibility,
		nullableInt64(grove.GitHubInstallationID), marshalJSONPtr(grove.GitHubPermissions), marshalJSONPtr(grove.GitHubAppStatus),
		marshalJSONPtr(grove.GitIdentity),
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

	if len(filter.MemberOrOwnerIDs) > 0 {
		// Combine owner_id match with grove ID membership using OR
		placeholders := make([]string, len(filter.MemberOrOwnerIDs))
		for i, id := range filter.MemberOrOwnerIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		orParts := []string{"id IN (" + strings.Join(placeholders, ",") + ")"}
		if filter.OwnerID != "" {
			orParts = append(orParts, "owner_id = ?")
			args = append(args, filter.OwnerID)
		}
		conditions = append(conditions, "("+strings.Join(orParts, " OR ")+")")
	} else if len(filter.MemberGroveIDs) > 0 {
		// Strict grove ID membership (no owner OR)
		placeholders := make([]string, len(filter.MemberGroveIDs))
		for i, id := range filter.MemberGroveIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions = append(conditions, "id IN ("+strings.Join(placeholders, ",")+")")
	} else if filter.OwnerID != "" {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.ExcludeOwnerID != "" {
		conditions = append(conditions, "owner_id != ?")
		args = append(args, filter.ExcludeOwnerID)
	}
	if filter.Visibility != "" {
		conditions = append(conditions, "visibility = ?")
		args = append(args, filter.Visibility)
	}
	if filter.GitRemote != "" {
		conditions = append(conditions, "git_remote = ?")
		args = append(args, filter.GitRemote)
	} else if filter.GitRemotePrefix != "" {
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
	if filter.Slug != "" {
		conditions = append(conditions, "LOWER(slug) = LOWER(?)")
		args = append(args, filter.Slug)
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
		SELECT id, name, slug, git_remote, default_runtime_broker_id, labels, annotations, shared_dirs, created_at, updated_at, created_by, owner_id, visibility,
		       github_installation_id, COALESCE(github_permissions, ''), COALESCE(github_app_status, ''), COALESCE(git_identity, '')
		FROM groves %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groves []store.Grove
	type groveRow struct {
		grove                store.Grove
		labels               string
		annotations          string
		sharedDirs           string
		gitRemote            sql.NullString
		brokerID             sql.NullString
		githubInstallationID sql.NullInt64
		githubPermissions    string
		githubAppStatus      string
		gitIdentity          string
	}
	var rowData []groveRow

	for rows.Next() {
		var r groveRow
		if err := rows.Scan(
			&r.grove.ID, &r.grove.Name, &r.grove.Slug, &r.gitRemote, &r.brokerID,
			&r.labels, &r.annotations, &r.sharedDirs,
			&r.grove.Created, &r.grove.Updated, &r.grove.CreatedBy, &r.grove.OwnerID, &r.grove.Visibility,
			&r.githubInstallationID, &r.githubPermissions, &r.githubAppStatus, &r.gitIdentity,
		); err != nil {
			return nil, err
		}
		rowData = append(rowData, r)
	}
	rows.Close() // Close early to release connection for nested queries

	for _, r := range rowData {
		grove := r.grove
		if r.gitRemote.Valid {
			grove.GitRemote = r.gitRemote.String
		}
		if r.brokerID.Valid {
			grove.DefaultRuntimeBrokerID = r.brokerID.String
		}
		if r.githubInstallationID.Valid {
			id := r.githubInstallationID.Int64
			grove.GitHubInstallationID = &id
		}
		unmarshalJSON(r.labels, &grove.Labels)
		unmarshalJSON(r.annotations, &grove.Annotations)
		unmarshalJSON(r.sharedDirs, &grove.SharedDirs)
		if r.githubPermissions != "" {
			grove.GitHubPermissions = &store.GitHubTokenPermissions{}
			unmarshalJSON(r.githubPermissions, grove.GitHubPermissions)
		}
		if r.githubAppStatus != "" {
			grove.GitHubAppStatus = &store.GitHubAppGroveStatus{}
			unmarshalJSON(r.githubAppStatus, grove.GitHubAppStatus)
		}
		if r.gitIdentity != "" {
			grove.GitIdentity = &store.GitIdentityConfig{}
			unmarshalJSON(r.gitIdentity, grove.GitIdentity)
		}

		// Populate computed fields - these now have a connection available
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", grove.ID).Scan(&grove.AgentCount)
		s.db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online')
			     + (SELECT COUNT(*) FROM runtime_brokers WHERE auto_provide = 1 AND status = 'online'
			            AND id NOT IN (SELECT broker_id FROM grove_contributors WHERE grove_id = ?))
		`, grove.ID, grove.ID).Scan(&grove.ActiveBrokerCount)
		s.populateGroveType(ctx, &grove)

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
		conditions = append(conditions, "(id IN (SELECT broker_id FROM grove_contributors WHERE grove_id = ?) OR auto_provide = 1)")
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
			id, name, slug, display_name, description, harness, default_harness_config, image, config,
			content_hash, scope, scope_id, grove_id,
			storage_uri, storage_bucket, storage_path, files,
			base_template, locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		template.ID, template.Name, template.Slug, nullableString(template.DisplayName), nullableString(template.Description),
		template.Harness, nullableString(template.DefaultHarnessConfig), template.Image, marshalJSON(template.Config),
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
	var defaultHarnessConfig sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, display_name, description, harness, default_harness_config, image, config,
			content_hash, scope, scope_id, grove_id,
			storage_uri, storage_bucket, storage_path, files,
			base_template, locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		FROM templates WHERE id = ?
	`, id).Scan(
		&template.ID, &template.Name, &template.Slug, &displayName, &description,
		&template.Harness, &defaultHarnessConfig, &template.Image, &config,
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
	if defaultHarnessConfig.Valid {
		template.DefaultHarnessConfig = defaultHarnessConfig.String
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
			harness = ?, default_harness_config = ?, image = ?, config = ?,
			content_hash = ?, scope = ?, scope_id = ?, grove_id = ?,
			storage_uri = ?, storage_bucket = ?, storage_path = ?, files = ?,
			base_template = ?, locked = ?, status = ?,
			owner_id = ?, updated_by = ?, visibility = ?,
			updated_at = ?
		WHERE id = ?
	`,
		template.Name, template.Slug, nullableString(template.DisplayName), nullableString(template.Description),
		template.Harness, nullableString(template.DefaultHarnessConfig), template.Image, marshalJSON(template.Config),
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

func (s *SQLiteStore) DeleteTemplatesByScope(ctx context.Context, scope, scopeID string) (int, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM templates WHERE scope = ? AND scope_id = ?", scope, scopeID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
	} else if filter.GroveID != "" && filter.Scope == "" {
		// When groveId is set without scope, return global + grove-scoped templates for this grove
		conditions = append(conditions, "(scope = 'global' OR (scope = 'grove' AND (scope_id = ? OR grove_id = ?)))")
		args = append(args, filter.GroveID, filter.GroveID)
	} else if filter.GroveID != "" {
		// Backwards compatibility: groveId with explicit scope
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
		SELECT id, name, slug, display_name, description, harness, default_harness_config, image, config,
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
		var defaultHarnessConfig sql.NullString

		if err := rows.Scan(
			&template.ID, &template.Name, &template.Slug, &displayName, &description,
			&template.Harness, &defaultHarnessConfig, &template.Image, &config,
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
		if defaultHarnessConfig.Valid {
			template.DefaultHarnessConfig = defaultHarnessConfig.String
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
// HarnessConfig Operations
// ============================================================================

func (s *SQLiteStore) CreateHarnessConfig(ctx context.Context, hc *store.HarnessConfig) error {
	now := time.Now()
	hc.Created = now
	hc.Updated = now

	if hc.Status == "" {
		hc.Status = store.HarnessConfigStatusActive
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO harness_configs (
			id, name, slug, display_name, description, harness, config,
			content_hash, scope, scope_id,
			storage_uri, storage_bucket, storage_path, files,
			locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		hc.ID, hc.Name, hc.Slug, nullableString(hc.DisplayName), nullableString(hc.Description),
		hc.Harness, marshalJSON(hc.Config),
		nullableString(hc.ContentHash), hc.Scope, nullableString(hc.ScopeID),
		nullableString(hc.StorageURI), nullableString(hc.StorageBucket), nullableString(hc.StoragePath), marshalJSON(hc.Files),
		hc.Locked, hc.Status,
		nullableString(hc.OwnerID), nullableString(hc.CreatedBy), nullableString(hc.UpdatedBy), hc.Visibility,
		hc.Created, hc.Updated,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetHarnessConfig(ctx context.Context, id string) (*store.HarnessConfig, error) {
	hc := &store.HarnessConfig{}
	var configJSON, filesJSON string
	var displayName, description, contentHash, scopeID sql.NullString
	var storageURI, storageBucket, storagePath sql.NullString
	var createdBy, updatedBy, ownerID, visibility sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, display_name, description, harness, config,
			content_hash, scope, scope_id,
			storage_uri, storage_bucket, storage_path, files,
			locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		FROM harness_configs WHERE id = ?
	`, id).Scan(
		&hc.ID, &hc.Name, &hc.Slug, &displayName, &description,
		&hc.Harness, &configJSON,
		&contentHash, &hc.Scope, &scopeID,
		&storageURI, &storageBucket, &storagePath, &filesJSON,
		&hc.Locked, &hc.Status,
		&ownerID, &createdBy, &updatedBy, &visibility,
		&hc.Created, &hc.Updated,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if displayName.Valid {
		hc.DisplayName = displayName.String
	}
	if description.Valid {
		hc.Description = description.String
	}
	if contentHash.Valid {
		hc.ContentHash = contentHash.String
	}
	if scopeID.Valid {
		hc.ScopeID = scopeID.String
	}
	if storageURI.Valid {
		hc.StorageURI = storageURI.String
	}
	if storageBucket.Valid {
		hc.StorageBucket = storageBucket.String
	}
	if storagePath.Valid {
		hc.StoragePath = storagePath.String
	}
	if ownerID.Valid {
		hc.OwnerID = ownerID.String
	}
	if createdBy.Valid {
		hc.CreatedBy = createdBy.String
	}
	if updatedBy.Valid {
		hc.UpdatedBy = updatedBy.String
	}
	if visibility.Valid {
		hc.Visibility = visibility.String
	}
	unmarshalJSON(configJSON, &hc.Config)
	unmarshalJSON(filesJSON, &hc.Files)

	return hc, nil
}

func (s *SQLiteStore) GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*store.HarnessConfig, error) {
	var id string
	var err error

	if scopeID != "" {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM harness_configs WHERE slug = ? AND scope = ? AND scope_id = ?", slug, scope, scopeID).Scan(&id)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM harness_configs WHERE slug = ? AND scope = ?", slug, scope).Scan(&id)
	}

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetHarnessConfig(ctx, id)
}

func (s *SQLiteStore) UpdateHarnessConfig(ctx context.Context, hc *store.HarnessConfig) error {
	hc.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE harness_configs SET
			name = ?, slug = ?, display_name = ?, description = ?,
			harness = ?, config = ?,
			content_hash = ?, scope = ?, scope_id = ?,
			storage_uri = ?, storage_bucket = ?, storage_path = ?, files = ?,
			locked = ?, status = ?,
			owner_id = ?, updated_by = ?, visibility = ?,
			updated_at = ?
		WHERE id = ?
	`,
		hc.Name, hc.Slug, nullableString(hc.DisplayName), nullableString(hc.Description),
		hc.Harness, marshalJSON(hc.Config),
		nullableString(hc.ContentHash), hc.Scope, nullableString(hc.ScopeID),
		nullableString(hc.StorageURI), nullableString(hc.StorageBucket), nullableString(hc.StoragePath), marshalJSON(hc.Files),
		hc.Locked, hc.Status,
		nullableString(hc.OwnerID), nullableString(hc.UpdatedBy), hc.Visibility,
		hc.Updated,
		hc.ID,
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

func (s *SQLiteStore) DeleteHarnessConfig(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM harness_configs WHERE id = ?", id)
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

func (s *SQLiteStore) DeleteHarnessConfigsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM harness_configs WHERE scope = ? AND scope_id = ?", scope, scopeID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *SQLiteStore) ListHarnessConfigs(ctx context.Context, filter store.HarnessConfigFilter, opts store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
	var conditions []string
	var args []interface{}

	if filter.Name != "" {
		conditions = append(conditions, "(name = ? OR slug = ?)")
		args = append(args, filter.Name, filter.Name)
	}
	if filter.Scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.ScopeID != "" {
		conditions = append(conditions, "scope_id = ?")
		args = append(args, filter.ScopeID)
	} else if filter.GroveID != "" && filter.Scope == "" {
		// When groveId is set without scope, return global + grove-scoped configs for this grove
		conditions = append(conditions, "(scope = 'global' OR (scope = 'grove' AND scope_id = ?))")
		args = append(args, filter.GroveID)
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
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM harness_configs %s", whereClause)
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		SELECT id, name, slug, display_name, description, harness, config,
			content_hash, scope, scope_id,
			storage_uri, storage_bucket, storage_path, files,
			locked, status,
			owner_id, created_by, updated_by, visibility,
			created_at, updated_at
		FROM harness_configs %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var harnessConfigs []store.HarnessConfig
	for rows.Next() {
		var hc store.HarnessConfig
		var configJSON, filesJSON string
		var displayName, description, contentHash, scopeID sql.NullString
		var storageURI, storageBucket, storagePath sql.NullString
		var createdBy, updatedBy, ownerID, visibility sql.NullString

		if err := rows.Scan(
			&hc.ID, &hc.Name, &hc.Slug, &displayName, &description,
			&hc.Harness, &configJSON,
			&contentHash, &hc.Scope, &scopeID,
			&storageURI, &storageBucket, &storagePath, &filesJSON,
			&hc.Locked, &hc.Status,
			&ownerID, &createdBy, &updatedBy, &visibility,
			&hc.Created, &hc.Updated,
		); err != nil {
			return nil, err
		}

		if displayName.Valid {
			hc.DisplayName = displayName.String
		}
		if description.Valid {
			hc.Description = description.String
		}
		if contentHash.Valid {
			hc.ContentHash = contentHash.String
		}
		if scopeID.Valid {
			hc.ScopeID = scopeID.String
		}
		if storageURI.Valid {
			hc.StorageURI = storageURI.String
		}
		if storageBucket.Valid {
			hc.StorageBucket = storageBucket.String
		}
		if storagePath.Valid {
			hc.StoragePath = storagePath.String
		}
		if ownerID.Valid {
			hc.OwnerID = ownerID.String
		}
		if createdBy.Valid {
			hc.CreatedBy = createdBy.String
		}
		if updatedBy.Valid {
			hc.UpdatedBy = updatedBy.String
		}
		if visibility.Valid {
			hc.Visibility = visibility.String
		}
		unmarshalJSON(configJSON, &hc.Config)
		unmarshalJSON(filesJSON, &hc.Files)

		harnessConfigs = append(harnessConfigs, hc)
	}

	return &store.ListResult[store.HarnessConfig]{
		Items:      harnessConfigs,
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
	var lastLogin, lastSeen sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, email, display_name, avatar_url, role, status, preferences, created_at, last_login, last_seen
		FROM users WHERE id = ?
	`, id).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
		&preferences, &user.Created, &lastLogin, &lastSeen,
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
	if lastSeen.Valid {
		user.LastSeen = lastSeen.Time
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
			role = ?, status = ?, preferences = ?, last_login = ?, last_seen = ?
		WHERE id = ?
	`,
		user.Email, user.DisplayName, user.AvatarURL,
		user.Role, user.Status, marshalJSON(user.Preferences), user.LastLogin, user.LastSeen,
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

func (s *SQLiteStore) UpdateUserLastSeen(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_seen = ? WHERE id = ?`, t, id)
	return err
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
	if filter.Search != "" {
		pattern := "%" + filter.Search + "%"
		conditions = append(conditions, "(email LIKE ? OR display_name LIKE ?)")
		args = append(args, pattern, pattern)
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

	if limit > 200 {
		limit = 200
	}

	offset := 0
	if opts.Cursor != "" {
		if parsed, err := strconv.Atoi(opts.Cursor); err == nil && parsed > 0 {
			offset = parsed
		}
	}

	// Map sort field to column name (whitelist to prevent SQL injection)
	orderColumn := "created_at"
	orderDir := "DESC"
	switch opts.SortBy {
	case "name":
		orderColumn = "display_name"
		orderDir = "ASC" // default ascending for name
	case "lastSeen":
		orderColumn = "last_seen"
	case "created":
		orderColumn = "created_at"
	}
	if opts.SortDir == "asc" {
		orderDir = "ASC"
	} else if opts.SortDir == "desc" {
		orderDir = "DESC"
	}

	query := fmt.Sprintf(`
		SELECT id, email, display_name, avatar_url, role, status, preferences, created_at, last_login, last_seen
		FROM users %s ORDER BY %s %s LIMIT ? OFFSET ?
	`, whereClause, orderColumn, orderDir)
	args = append(args, limit+1, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []store.User
	for rows.Next() {
		var user store.User
		var preferences string
		var lastLogin, lastSeen sql.NullTime

		if err := rows.Scan(
			&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
			&preferences, &user.Created, &lastLogin, &lastSeen,
		); err != nil {
			return nil, err
		}

		if lastLogin.Valid {
			user.LastLogin = lastLogin.Time
		}
		if lastSeen.Valid {
			user.LastSeen = lastSeen.Time
		}
		unmarshalJSON(preferences, &user.Preferences)

		users = append(users, user)
	}

	result := &store.ListResult[store.User]{
		Items:      users,
		TotalCount: totalCount,
	}

	// Handle pagination: if we got more than limit, there's a next page
	if len(users) > limit {
		result.Items = users[:limit]
		result.NextCursor = strconv.Itoa(offset + limit)
	}

	return result, nil
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
		INSERT INTO env_vars (id, key, value, scope, scope_id, description, sensitive, injection_mode, secret, created_at, updated_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		envVar.ID, envVar.Key, envVar.Value, envVar.Scope, envVar.ScopeID,
		envVar.Description, envVar.Sensitive, envVar.InjectionMode, envVar.Secret,
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
		SELECT id, key, value, scope, scope_id, description, sensitive, injection_mode, secret, created_at, updated_at, created_by
		FROM env_vars WHERE key = ? AND scope = ? AND scope_id = ?
	`, key, scope, scopeID).Scan(
		&envVar.ID, &envVar.Key, &envVar.Value, &envVar.Scope, &envVar.ScopeID,
		&envVar.Description, &envVar.Sensitive, &envVar.InjectionMode, &envVar.Secret,
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
			value = ?, description = ?, sensitive = ?, injection_mode = ?, secret = ?, updated_at = ?
		WHERE key = ? AND scope = ? AND scope_id = ?
	`,
		envVar.Value, envVar.Description, envVar.Sensitive, envVar.InjectionMode, envVar.Secret, envVar.Updated,
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

func (s *SQLiteStore) DeleteEnvVarsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM env_vars WHERE scope = ? AND scope_id = ?", scope, scopeID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
		SELECT id, key, value, scope, scope_id, description, sensitive, injection_mode, secret, created_at, updated_at, created_by
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
			&envVar.Description, &envVar.Sensitive, &envVar.InjectionMode, &envVar.Secret,
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

	if secret.SecretType == "" {
		secret.SecretType = store.SecretTypeEnvironment
	}
	if secret.Target == "" {
		secret.Target = secret.Key
	}
	if secret.InjectionMode == "" {
		secret.InjectionMode = store.InjectionModeAsNeeded
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets (id, key, encrypted_value, secret_ref, secret_type, target, scope, scope_id, description, injection_mode, allow_progeny, version, created_at, updated_at, created_by, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		secret.ID, secret.Key, secret.EncryptedValue, nullableString(secret.SecretRef),
		secret.SecretType, nullableString(secret.Target),
		secret.Scope, secret.ScopeID,
		secret.Description, secret.InjectionMode, boolToInt(secret.AllowProgeny), secret.Version,
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
	var target sql.NullString
	var secretRef sql.NullString

	var allowProgeny int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, key, encrypted_value, secret_ref, secret_type, COALESCE(target, key), scope, scope_id, description, injection_mode, allow_progeny, version, created_at, updated_at, created_by, updated_by
		FROM secrets WHERE key = ? AND scope = ? AND scope_id = ?
	`, key, scope, scopeID).Scan(
		&secret.ID, &secret.Key, &secret.EncryptedValue, &secretRef,
		&secret.SecretType, &target,
		&secret.Scope, &secret.ScopeID,
		&secret.Description, &secret.InjectionMode, &allowProgeny, &secret.Version,
		&secret.Created, &secret.Updated, &secret.CreatedBy, &secret.UpdatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if target.Valid {
		secret.Target = target.String
	}
	if secretRef.Valid {
		secret.SecretRef = secretRef.String
	}
	secret.AllowProgeny = allowProgeny != 0

	return secret, nil
}

func (s *SQLiteStore) UpdateSecret(ctx context.Context, secret *store.Secret) error {
	secret.Updated = time.Now()
	secret.Version++ // Increment version on each update

	if secret.SecretType == "" {
		secret.SecretType = store.SecretTypeEnvironment
	}
	if secret.Target == "" {
		secret.Target = secret.Key
	}
	if secret.InjectionMode == "" {
		secret.InjectionMode = store.InjectionModeAsNeeded
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE secrets SET
			encrypted_value = ?, secret_ref = ?, secret_type = ?, target = ?, description = ?, injection_mode = ?, allow_progeny = ?, version = ?, updated_at = ?, updated_by = ?
		WHERE key = ? AND scope = ? AND scope_id = ?
	`,
		secret.EncryptedValue, nullableString(secret.SecretRef),
		secret.SecretType, nullableString(secret.Target),
		secret.Description, secret.InjectionMode, boolToInt(secret.AllowProgeny), secret.Version, secret.Updated, secret.UpdatedBy,
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

func (s *SQLiteStore) DeleteSecretsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE scope = ? AND scope_id = ?", scope, scopeID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
	if filter.Type != "" {
		conditions = append(conditions, "secret_type = ?")
		args = append(args, filter.Type)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Note: We do NOT select encrypted_value for listing
	query := fmt.Sprintf(`
		SELECT id, key, secret_ref, secret_type, COALESCE(target, key), scope, scope_id, description, injection_mode, allow_progeny, version, created_at, updated_at, created_by, updated_by
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
		var target sql.NullString
		var secretRef sql.NullString
		var allowProgeny int
		if err := rows.Scan(
			&secret.ID, &secret.Key, &secretRef, &secret.SecretType, &target,
			&secret.Scope, &secret.ScopeID,
			&secret.Description, &secret.InjectionMode, &allowProgeny, &secret.Version,
			&secret.Created, &secret.Updated, &secret.CreatedBy, &secret.UpdatedBy,
		); err != nil {
			return nil, err
		}
		if target.Valid {
			secret.Target = target.String
		}
		if secretRef.Valid {
			secret.SecretRef = secretRef.String
		}
		secret.AllowProgeny = allowProgeny != 0
		secrets = append(secrets, secret)
	}

	return secrets, nil
}

func (s *SQLiteStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	if len(ancestorIDs) == 0 {
		return nil, nil
	}

	// Build placeholder list for IN clause
	placeholders := make([]string, len(ancestorIDs))
	args := make([]interface{}, len(ancestorIDs))
	for i, id := range ancestorIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT id, key, secret_ref, secret_type, COALESCE(target, key), scope, scope_id, description, injection_mode, allow_progeny, version, created_at, updated_at, created_by, updated_by
		FROM secrets
		WHERE scope = 'user' AND allow_progeny = 1 AND created_by IN (%s)
		ORDER BY key
	`, strings.Join(placeholders, ", "))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []store.Secret
	for rows.Next() {
		var secret store.Secret
		var target sql.NullString
		var secretRef sql.NullString
		var allowProgeny int
		if err := rows.Scan(
			&secret.ID, &secret.Key, &secretRef, &secret.SecretType, &target,
			&secret.Scope, &secret.ScopeID,
			&secret.Description, &secret.InjectionMode, &allowProgeny, &secret.Version,
			&secret.Created, &secret.Updated, &secret.CreatedBy, &secret.UpdatedBy,
		); err != nil {
			return nil, err
		}
		if target.Valid {
			secret.Target = target.String
		}
		if secretRef.Valid {
			secret.SecretRef = secretRef.String
		}
		secret.AllowProgeny = allowProgeny != 0
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
	if group.GroupType == "" {
		group.GroupType = store.GroupTypeExplicit
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO groups (id, name, slug, description, group_type, grove_id, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		group.ID, group.Name, group.Slug, group.Description,
		group.GroupType, nullableString(group.GroveID),
		nullableString(group.ParentID),
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
	var parentID, groveID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, description, group_type, grove_id, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id
		FROM groups WHERE id = ?
	`, id).Scan(
		&group.ID, &group.Name, &group.Slug, &group.Description,
		&group.GroupType, &groveID,
		&parentID,
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
	if groveID.Valid {
		group.GroveID = groveID.String
	}
	unmarshalJSON(labels, &group.Labels)
	unmarshalJSON(annotations, &group.Annotations)
	if group.GroupType == "" {
		group.GroupType = store.GroupTypeExplicit
	}

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
			name = ?, slug = ?, description = ?, group_type = ?, grove_id = ?,
			parent_id = ?, labels = ?, annotations = ?,
			updated_at = ?, owner_id = ?
		WHERE id = ?
	`,
		group.Name, group.Slug, group.Description,
		group.GroupType, nullableString(group.GroveID),
		nullableString(group.ParentID),
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
	if filter.GroupType != "" {
		conditions = append(conditions, "group_type = ?")
		args = append(args, filter.GroupType)
	}
	if filter.GroveID != "" {
		conditions = append(conditions, "grove_id = ?")
		args = append(args, filter.GroveID)
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
		SELECT id, name, slug, description, group_type, grove_id, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id
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
		var parentID, groveID sql.NullString

		if err := rows.Scan(
			&group.ID, &group.Name, &group.Slug, &group.Description,
			&group.GroupType, &groveID,
			&parentID,
			&labels, &annotations,
			&group.Created, &group.Updated, &group.CreatedBy, &group.OwnerID,
		); err != nil {
			return nil, err
		}

		if parentID.Valid {
			group.ParentID = parentID.String
		}
		if groveID.Valid {
			group.GroveID = groveID.String
		}
		unmarshalJSON(labels, &group.Labels)
		unmarshalJSON(annotations, &group.Annotations)
		if group.GroupType == "" {
			group.GroupType = store.GroupTypeExplicit
		}

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

func (s *SQLiteStore) UpdateGroupMemberRole(ctx context.Context, groupID, memberType, memberID, newRole string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE group_members SET role = ? WHERE group_id = ? AND member_type = ? AND member_id = ?`,
		newRole, groupID, memberType, memberID,
	)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return store.ErrNotFound
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

// GetGroupByGroveID retrieves the grove_agents group associated with a grove.
func (s *SQLiteStore) GetGroupByGroveID(ctx context.Context, groveID string) (*store.Group, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM groups WHERE grove_id = ? AND group_type = ? LIMIT 1",
		groveID, store.GroupTypeGroveAgents).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetGroup(ctx, id)
}

// GetEffectiveGroupsForAgent returns all groups an agent belongs to.
func (s *SQLiteStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	return nil, nil
}

// CheckDelegatedAccess is a stub for the SQLite store. Delegation resolution
// is implemented in the Ent adapter.
func (s *SQLiteStore) CheckDelegatedAccess(ctx context.Context, agentID string, conditions *store.PolicyConditions) (bool, error) {
	return false, nil
}

// GetGroupsByIDs is a stub for the SQLite store. Group retrieval by IDs
// is implemented in the Ent adapter.
func (s *SQLiteStore) GetGroupsByIDs(ctx context.Context, ids []string) ([]store.Group, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, slug, description, group_type, grove_id, parent_id, labels, annotations, created_at, updated_at, created_by, owner_id
		FROM groups WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []store.Group
	for rows.Next() {
		var g store.Group
		var labels, annotations string
		var parentID, groveID sql.NullString
		if err := rows.Scan(
			&g.ID, &g.Name, &g.Slug, &g.Description,
			&g.GroupType, &groveID,
			&parentID,
			&labels, &annotations,
			&g.Created, &g.Updated, &g.CreatedBy, &g.OwnerID,
		); err != nil {
			return nil, err
		}
		if parentID.Valid {
			g.ParentID = parentID.String
		}
		if groveID.Valid {
			g.GroveID = groveID.String
		}
		unmarshalJSON(labels, &g.Labels)
		unmarshalJSON(annotations, &g.Annotations)
		if g.GroupType == "" {
			g.GroupType = store.GroupTypeExplicit
		}
		groups = append(groups, g)
	}

	return groups, rows.Err()
}

func (s *SQLiteStore) CountGroupMembersByRole(ctx context.Context, groupID, role string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM group_members WHERE group_id = ? AND role = ?`,
		groupID, role,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
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

	if filter.Name != "" {
		conditions = append(conditions, "name = ?")
		args = append(args, filter.Name)
	}
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

func (s *SQLiteStore) GetPoliciesForPrincipals(ctx context.Context, principals []store.PrincipalRef) ([]store.Policy, error) {
	if len(principals) == 0 {
		return nil, nil
	}

	// Build dynamic OR clauses for each principal
	var clauses []string
	var args []interface{}
	for _, p := range principals {
		clauses = append(clauses, "(pb.principal_type = ? AND pb.principal_id = ?)")
		args = append(args, p.Type, p.ID)
	}

	query := `
		SELECT DISTINCT p.id, p.name, p.description, p.scope_type, p.scope_id, p.resource_type, p.resource_id, p.actions, p.effect, p.conditions, p.priority, p.labels, p.annotations, p.created_at, p.updated_at, p.created_by
		FROM policies p
		INNER JOIN policy_bindings pb ON p.id = pb.policy_id
		WHERE ` + strings.Join(clauses, " OR ") + `
		ORDER BY
			CASE p.scope_type WHEN 'hub' THEN 0 WHEN 'grove' THEN 1 WHEN 'resource' THEN 2 END,
			p.priority ASC
	`

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

	return policies, nil
}

// ============================================================================
// User Access Token Operations
// ============================================================================

func (s *SQLiteStore) CreateUserAccessToken(ctx context.Context, token *store.UserAccessToken) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_access_tokens (
			id, user_id, name, prefix, key_hash, grove_id, scopes,
			revoked, expires_at, last_used, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		token.ID, token.UserID, token.Name, token.Prefix, token.KeyHash,
		token.GroveID, marshalJSON(token.Scopes),
		token.Revoked, nullableTimePtr(token.ExpiresAt), nullableTimePtr(token.LastUsed), token.Created,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return store.ErrInvalidInput
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetUserAccessToken(ctx context.Context, id string) (*store.UserAccessToken, error) {
	token := &store.UserAccessToken{}
	var scopes string
	var expiresAt, lastUsed sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, prefix, key_hash, grove_id, scopes,
			revoked, expires_at, last_used, created_at
		FROM user_access_tokens WHERE id = ?
	`, id).Scan(
		&token.ID, &token.UserID, &token.Name, &token.Prefix, &token.KeyHash,
		&token.GroveID, &scopes,
		&token.Revoked, &expiresAt, &lastUsed, &token.Created,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(scopes, &token.Scopes)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Time
	}
	if lastUsed.Valid {
		token.LastUsed = &lastUsed.Time
	}
	return token, nil
}

func (s *SQLiteStore) GetUserAccessTokenByHash(ctx context.Context, hash string) (*store.UserAccessToken, error) {
	token := &store.UserAccessToken{}
	var scopes string
	var expiresAt, lastUsed sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, prefix, key_hash, grove_id, scopes,
			revoked, expires_at, last_used, created_at
		FROM user_access_tokens WHERE key_hash = ?
	`, hash).Scan(
		&token.ID, &token.UserID, &token.Name, &token.Prefix, &token.KeyHash,
		&token.GroveID, &scopes,
		&token.Revoked, &expiresAt, &lastUsed, &token.Created,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	unmarshalJSON(scopes, &token.Scopes)
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Time
	}
	if lastUsed.Valid {
		token.LastUsed = &lastUsed.Time
	}
	return token, nil
}

func (s *SQLiteStore) UpdateUserAccessTokenLastUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE user_access_tokens SET last_used = ? WHERE id = ?",
		time.Now(), id,
	)
	return err
}

func (s *SQLiteStore) RevokeUserAccessToken(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE user_access_tokens SET revoked = 1 WHERE id = ?", id,
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

func (s *SQLiteStore) DeleteUserAccessToken(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM user_access_tokens WHERE id = ?", id)
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

func (s *SQLiteStore) ListUserAccessTokens(ctx context.Context, userID string) ([]store.UserAccessToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, prefix, grove_id, scopes,
			revoked, expires_at, last_used, created_at
		FROM user_access_tokens WHERE user_id = ?
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []store.UserAccessToken
	for rows.Next() {
		var token store.UserAccessToken
		var scopes string
		var expiresAt, lastUsed sql.NullTime

		if err := rows.Scan(
			&token.ID, &token.UserID, &token.Name, &token.Prefix,
			&token.GroveID, &scopes,
			&token.Revoked, &expiresAt, &lastUsed, &token.Created,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(scopes, &token.Scopes)
		if expiresAt.Valid {
			token.ExpiresAt = &expiresAt.Time
		}
		if lastUsed.Valid {
			token.LastUsed = &lastUsed.Time
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

func (s *SQLiteStore) CountUserAccessTokens(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_access_tokens WHERE user_id = ? AND revoked = 0",
		userID,
	).Scan(&count)
	return count, err
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
