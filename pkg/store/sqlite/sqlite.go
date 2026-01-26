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
	_ "modernc.org/sqlite" // Pure Go SQLite driver
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

-- Runtime hosts table
CREATE TABLE IF NOT EXISTS runtime_hosts (
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
CREATE INDEX IF NOT EXISTS idx_runtime_hosts_slug ON runtime_hosts(slug);
CREATE INDEX IF NOT EXISTS idx_runtime_hosts_status ON runtime_hosts(status);

-- Grove contributors (many-to-many relationship)
CREATE TABLE IF NOT EXISTS grove_contributors (
	grove_id TEXT NOT NULL,
	host_id TEXT NOT NULL,
	host_name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'connected',
	status TEXT NOT NULL DEFAULT 'offline',
	profiles TEXT,
	last_seen TIMESTAMP,
	PRIMARY KEY (grove_id, host_id),
	FOREIGN KEY (grove_id) REFERENCES groves(id) ON DELETE CASCADE,
	FOREIGN KEY (host_id) REFERENCES runtime_hosts(id) ON DELETE CASCADE
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
	runtime_host_id TEXT,
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
	FOREIGN KEY (runtime_host_id) REFERENCES runtime_hosts(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_grove_slug ON agents(grove_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_agents_grove ON agents(grove_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
CREATE INDEX IF NOT EXISTS idx_agents_runtime_host ON agents(runtime_host_id);

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

// Migration V2: Add default_runtime_host_id to groves
const migrationV2 = `
-- Add default runtime host to groves
ALTER TABLE groves ADD COLUMN default_runtime_host_id TEXT REFERENCES runtime_hosts(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_groves_default_runtime_host ON groves(default_runtime_host_id);
`

// Migration V3: Add local_path to grove_contributors
const migrationV3 = `
-- Add local_path column to grove_contributors for tracking filesystem paths per host
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
			image, detached, runtime, runtime_host_id, web_pty_enabled, task_summary,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		agent.ID, agent.AgentID, agent.Name, agent.Template, agent.GroveID,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Status, agent.ConnectionState, agent.ContainerStatus, agent.SessionStatus, agent.RuntimeState,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeHostID), agent.WebPTYEnabled, agent.TaskSummary,
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
	var runtimeHostID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_host_id, web_pty_enabled, task_summary,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		FROM agents WHERE id = ?
	`, id).Scan(
		&agent.ID, &agent.AgentID, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeHostID, &agent.WebPTYEnabled, &agent.TaskSummary,
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
	if runtimeHostID.Valid {
		agent.RuntimeHostID = runtimeHostID.String
	}

	return agent, nil
}

func (s *SQLiteStore) GetAgentBySlug(ctx context.Context, groveID, slug string) (*store.Agent, error) {
	agent := &store.Agent{}
	var labels, annotations, appliedConfig string
	var lastSeen sql.NullTime
	var runtimeHostID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, template, grove_id,
			labels, annotations,
			status, connection_state, container_status, session_status, runtime_state,
			image, detached, runtime, runtime_host_id, web_pty_enabled, task_summary,
			applied_config,
			created_at, updated_at, last_seen,
			created_by, owner_id, visibility, state_version
		FROM agents WHERE grove_id = ? AND agent_id = ?
	`, groveID, slug).Scan(
		&agent.ID, &agent.AgentID, &agent.Name, &agent.Template, &agent.GroveID,
		&labels, &annotations,
		&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
		&agent.Image, &agent.Detached, &agent.Runtime, &runtimeHostID, &agent.WebPTYEnabled, &agent.TaskSummary,
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
	if runtimeHostID.Valid {
		agent.RuntimeHostID = runtimeHostID.String
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
			image = ?, detached = ?, runtime = ?, runtime_host_id = ?, web_pty_enabled = ?, task_summary = ?,
			applied_config = ?,
			updated_at = ?, last_seen = ?,
			owner_id = ?, visibility = ?, state_version = ?
		WHERE id = ? AND state_version = ?
	`,
		agent.AgentID, agent.Name, agent.Template,
		marshalJSON(agent.Labels), marshalJSON(agent.Annotations),
		agent.Status, agent.ConnectionState, agent.ContainerStatus, agent.SessionStatus, agent.RuntimeState,
		agent.Image, agent.Detached, agent.Runtime, nullableString(agent.RuntimeHostID), agent.WebPTYEnabled, agent.TaskSummary,
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
	if filter.RuntimeHostID != "" {
		conditions = append(conditions, "runtime_host_id = ?")
		args = append(args, filter.RuntimeHostID)
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
			image, detached, runtime, runtime_host_id, web_pty_enabled, task_summary,
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
		var runtimeHostID sql.NullString

		if err := rows.Scan(
			&agent.ID, &agent.AgentID, &agent.Name, &agent.Template, &agent.GroveID,
			&labels, &annotations,
			&agent.Status, &agent.ConnectionState, &agent.ContainerStatus, &agent.SessionStatus, &agent.RuntimeState,
			&agent.Image, &agent.Detached, &agent.Runtime, &runtimeHostID, &agent.WebPTYEnabled, &agent.TaskSummary,
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
		if runtimeHostID.Valid {
			agent.RuntimeHostID = runtimeHostID.String
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
			connection_state = COALESCE(NULLIF(?, ''), connection_state),
			container_status = COALESCE(NULLIF(?, ''), container_status),
			session_status = COALESCE(NULLIF(?, ''), session_status),
			runtime_state = COALESCE(NULLIF(?, ''), runtime_state),
			task_summary = COALESCE(NULLIF(?, ''), task_summary),
			updated_at = ?,
			last_seen = ?
		WHERE id = ?
	`,
		status.Status, status.ConnectionState, status.ContainerStatus,
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
		INSERT INTO groves (id, name, slug, git_remote, default_runtime_host_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		grove.ID, grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeHostID),
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
	var gitRemote, defaultRuntimeHostID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, git_remote, default_runtime_host_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility
		FROM groves WHERE id = ?
	`, id).Scan(
		&grove.ID, &grove.Name, &grove.Slug, &gitRemote, &defaultRuntimeHostID,
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
	if defaultRuntimeHostID.Valid {
		grove.DefaultRuntimeHostID = defaultRuntimeHostID.String
	}
	unmarshalJSON(labels, &grove.Labels)
	unmarshalJSON(annotations, &grove.Annotations)

	// Populate computed fields
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", id).Scan(&grove.AgentCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online'", id).Scan(&grove.ActiveHostCount)

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
			name = ?, slug = ?, git_remote = ?, default_runtime_host_id = ?,
			labels = ?, annotations = ?,
			updated_at = ?, owner_id = ?, visibility = ?
		WHERE id = ?
	`,
		grove.Name, grove.Slug, nullableString(grove.GitRemote), nullableString(grove.DefaultRuntimeHostID),
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
	if filter.HostID != "" {
		conditions = append(conditions, "id IN (SELECT grove_id FROM grove_contributors WHERE host_id = ?)")
		args = append(args, filter.HostID)
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
		SELECT id, name, slug, git_remote, default_runtime_host_id, labels, annotations, created_at, updated_at, created_by, owner_id, visibility
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
		var gitRemote, defaultRuntimeHostID sql.NullString

		if err := rows.Scan(
			&grove.ID, &grove.Name, &grove.Slug, &gitRemote, &defaultRuntimeHostID,
			&labels, &annotations,
			&grove.Created, &grove.Updated, &grove.CreatedBy, &grove.OwnerID, &grove.Visibility,
		); err != nil {
			return nil, err
		}

		if gitRemote.Valid {
			grove.GitRemote = gitRemote.String
		}
		if defaultRuntimeHostID.Valid {
			grove.DefaultRuntimeHostID = defaultRuntimeHostID.String
		}
		unmarshalJSON(labels, &grove.Labels)
		unmarshalJSON(annotations, &grove.Annotations)

		// Populate computed fields
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE grove_id = ?", grove.ID).Scan(&grove.AgentCount)
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM grove_contributors WHERE grove_id = ? AND status = 'online'", grove.ID).Scan(&grove.ActiveHostCount)

		groves = append(groves, grove)
	}

	return &store.ListResult[store.Grove]{
		Items:      groves,
		TotalCount: totalCount,
	}, nil
}

// ============================================================================
// RuntimeHost Operations
// ============================================================================

func (s *SQLiteStore) CreateRuntimeHost(ctx context.Context, host *store.RuntimeHost) error {
	now := time.Now()
	host.Created = now
	host.Updated = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_hosts (
			id, name, slug, type, mode, version,
			status, connection_state, last_heartbeat,
			capabilities, supported_harnesses, resources, runtimes,
			labels, annotations, endpoint,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		host.ID, host.Name, host.Slug, host.Type, host.Mode, host.Version,
		host.Status, host.ConnectionState, host.LastHeartbeat,
		marshalJSON(host.Capabilities), marshalJSON(host.SupportedHarnesses),
		marshalJSON(host.Resources), marshalJSON(host.Runtimes),
		marshalJSON(host.Labels), marshalJSON(host.Annotations), host.Endpoint,
		host.Created, host.Updated,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetRuntimeHost(ctx context.Context, id string) (*store.RuntimeHost, error) {
	host := &store.RuntimeHost{}
	var capabilities, harnesses, resources, runtimes, labels, annotations string
	var lastHeartbeat sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, type, mode, version,
			status, connection_state, last_heartbeat,
			capabilities, supported_harnesses, resources, runtimes,
			labels, annotations, endpoint,
			created_at, updated_at
		FROM runtime_hosts WHERE id = ?
	`, id).Scan(
		&host.ID, &host.Name, &host.Slug, &host.Type, &host.Mode, &host.Version,
		&host.Status, &host.ConnectionState, &lastHeartbeat,
		&capabilities, &harnesses, &resources, &runtimes,
		&labels, &annotations, &host.Endpoint,
		&host.Created, &host.Updated,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if lastHeartbeat.Valid {
		host.LastHeartbeat = lastHeartbeat.Time
	}
	unmarshalJSON(capabilities, &host.Capabilities)
	unmarshalJSON(harnesses, &host.SupportedHarnesses)
	unmarshalJSON(resources, &host.Resources)
	unmarshalJSON(runtimes, &host.Runtimes)
	unmarshalJSON(labels, &host.Labels)
	unmarshalJSON(annotations, &host.Annotations)

	return host, nil
}

func (s *SQLiteStore) GetRuntimeHostByName(ctx context.Context, name string) (*store.RuntimeHost, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM runtime_hosts WHERE LOWER(name) = LOWER(?)", name).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.GetRuntimeHost(ctx, id)
}

func (s *SQLiteStore) UpdateRuntimeHost(ctx context.Context, host *store.RuntimeHost) error {
	host.Updated = time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_hosts SET
			name = ?, slug = ?, type = ?, mode = ?, version = ?,
			status = ?, connection_state = ?, last_heartbeat = ?,
			capabilities = ?, supported_harnesses = ?, resources = ?, runtimes = ?,
			labels = ?, annotations = ?, endpoint = ?,
			updated_at = ?
		WHERE id = ?
	`,
		host.Name, host.Slug, host.Type, host.Mode, host.Version,
		host.Status, host.ConnectionState, host.LastHeartbeat,
		marshalJSON(host.Capabilities), marshalJSON(host.SupportedHarnesses),
		marshalJSON(host.Resources), marshalJSON(host.Runtimes),
		marshalJSON(host.Labels), marshalJSON(host.Annotations), host.Endpoint,
		host.Updated,
		host.ID,
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

func (s *SQLiteStore) DeleteRuntimeHost(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM runtime_hosts WHERE id = ?", id)
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

func (s *SQLiteStore) ListRuntimeHosts(ctx context.Context, filter store.RuntimeHostFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeHost], error) {
	var conditions []string
	var args []interface{}

	if filter.Type != "" {
		conditions = append(conditions, "type = ?")
		args = append(args, filter.Type)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Mode != "" {
		conditions = append(conditions, "mode = ?")
		args = append(args, filter.Mode)
	}
	if filter.GroveID != "" {
		conditions = append(conditions, "id IN (SELECT host_id FROM grove_contributors WHERE grove_id = ?)")
		args = append(args, filter.GroveID)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM runtime_hosts %s", whereClause)
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
			created_at, updated_at
		FROM runtime_hosts %s ORDER BY created_at DESC LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []store.RuntimeHost
	for rows.Next() {
		var host store.RuntimeHost
		var capabilities, harnesses, resources, runtimes, labels, annotations string
		var lastHeartbeat sql.NullTime

		if err := rows.Scan(
			&host.ID, &host.Name, &host.Slug, &host.Type, &host.Mode, &host.Version,
			&host.Status, &host.ConnectionState, &lastHeartbeat,
			&capabilities, &harnesses, &resources, &runtimes,
			&labels, &annotations, &host.Endpoint,
			&host.Created, &host.Updated,
		); err != nil {
			return nil, err
		}

		if lastHeartbeat.Valid {
			host.LastHeartbeat = lastHeartbeat.Time
		}
		unmarshalJSON(capabilities, &host.Capabilities)
		unmarshalJSON(harnesses, &host.SupportedHarnesses)
		unmarshalJSON(resources, &host.Resources)
		unmarshalJSON(runtimes, &host.Runtimes)
		unmarshalJSON(labels, &host.Labels)
		unmarshalJSON(annotations, &host.Annotations)

		hosts = append(hosts, host)
	}

	return &store.ListResult[store.RuntimeHost]{
		Items:      hosts,
		TotalCount: totalCount,
	}, nil
}

func (s *SQLiteStore) UpdateRuntimeHostHeartbeat(ctx context.Context, id string, status string, resources *store.HostResources) error {
	now := time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_hosts SET
			status = ?,
			last_heartbeat = ?,
			resources = COALESCE(?, resources),
			updated_at = ?
		WHERE id = ?
	`, status, now, marshalJSON(resources), now, id)
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

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO templates (id, name, slug, harness, image, config, scope, grove_id, storage_uri, owner_id, visibility, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		template.ID, template.Name, template.Slug, template.Harness, template.Image,
		marshalJSON(template.Config), template.Scope, nullableString(template.GroveID), template.StorageURI,
		template.OwnerID, template.Visibility, template.Created, template.Updated,
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
	var config string
	var groveID sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, harness, image, config, scope, grove_id, storage_uri, owner_id, visibility, created_at, updated_at
		FROM templates WHERE id = ?
	`, id).Scan(
		&template.ID, &template.Name, &template.Slug, &template.Harness, &template.Image,
		&config, &template.Scope, &groveID, &template.StorageURI,
		&template.OwnerID, &template.Visibility, &template.Created, &template.Updated,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if groveID.Valid {
		template.GroveID = groveID.String
	}
	unmarshalJSON(config, &template.Config)

	return template, nil
}

func (s *SQLiteStore) GetTemplateBySlug(ctx context.Context, slug, scope, groveID string) (*store.Template, error) {
	var id string
	var err error

	if scope == "grove" && groveID != "" {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM templates WHERE slug = ? AND scope = ? AND grove_id = ?", slug, scope, groveID).Scan(&id)
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
			name = ?, slug = ?, harness = ?, image = ?, config = ?,
			scope = ?, grove_id = ?, storage_uri = ?,
			owner_id = ?, visibility = ?, updated_at = ?
		WHERE id = ?
	`,
		template.Name, template.Slug, template.Harness, template.Image, marshalJSON(template.Config),
		template.Scope, nullableString(template.GroveID), template.StorageURI,
		template.OwnerID, template.Visibility, template.Updated,
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

	if filter.Scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.GroveID != "" {
		conditions = append(conditions, "grove_id = ?")
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
		SELECT id, name, slug, harness, image, config, scope, grove_id, storage_uri, owner_id, visibility, created_at, updated_at
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
		var config string
		var groveID sql.NullString

		if err := rows.Scan(
			&template.ID, &template.Name, &template.Slug, &template.Harness, &template.Image,
			&config, &template.Scope, &groveID, &template.StorageURI,
			&template.OwnerID, &template.Visibility, &template.Created, &template.Updated,
		); err != nil {
			return nil, err
		}

		if groveID.Valid {
			template.GroveID = groveID.String
		}
		unmarshalJSON(config, &template.Config)

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
// GroveContributor Operations
// ============================================================================

func (s *SQLiteStore) AddGroveContributor(ctx context.Context, contrib *store.GroveContributor) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO grove_contributors (grove_id, host_id, host_name, local_path, mode, status, profiles, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		contrib.GroveID, contrib.HostID, contrib.HostName, contrib.LocalPath, contrib.Mode, contrib.Status,
		marshalJSON(contrib.Profiles), contrib.LastSeen,
	)
	return err
}

func (s *SQLiteStore) RemoveGroveContributor(ctx context.Context, groveID, hostID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM grove_contributors WHERE grove_id = ? AND host_id = ?", groveID, hostID)
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

func (s *SQLiteStore) GetGroveContributor(ctx context.Context, groveID, hostID string) (*store.GroveContributor, error) {
	var contrib store.GroveContributor
	var localPath sql.NullString
	var profiles string
	var lastSeen sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT grove_id, host_id, host_name, local_path, mode, status, profiles, last_seen
		FROM grove_contributors WHERE grove_id = ? AND host_id = ?
	`, groveID, hostID).Scan(
		&contrib.GroveID, &contrib.HostID, &contrib.HostName, &localPath, &contrib.Mode, &contrib.Status,
		&profiles, &lastSeen,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if localPath.Valid {
		contrib.LocalPath = localPath.String
	}
	if lastSeen.Valid {
		contrib.LastSeen = lastSeen.Time
	}
	unmarshalJSON(profiles, &contrib.Profiles)

	return &contrib, nil
}

func (s *SQLiteStore) GetGroveContributors(ctx context.Context, groveID string) ([]store.GroveContributor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grove_id, host_id, host_name, local_path, mode, status, profiles, last_seen
		FROM grove_contributors WHERE grove_id = ?
	`, groveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contributors []store.GroveContributor
	for rows.Next() {
		var contrib store.GroveContributor
		var localPath sql.NullString
		var profiles string
		var lastSeen sql.NullTime

		if err := rows.Scan(
			&contrib.GroveID, &contrib.HostID, &contrib.HostName, &localPath, &contrib.Mode, &contrib.Status,
			&profiles, &lastSeen,
		); err != nil {
			return nil, err
		}

		if localPath.Valid {
			contrib.LocalPath = localPath.String
		}
		if lastSeen.Valid {
			contrib.LastSeen = lastSeen.Time
		}
		unmarshalJSON(profiles, &contrib.Profiles)

		contributors = append(contributors, contrib)
	}

	return contributors, nil
}

func (s *SQLiteStore) GetHostGroves(ctx context.Context, hostID string) ([]store.GroveContributor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grove_id, host_id, host_name, local_path, mode, status, profiles, last_seen
		FROM grove_contributors WHERE host_id = ?
	`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contributors []store.GroveContributor
	for rows.Next() {
		var contrib store.GroveContributor
		var localPath sql.NullString
		var profiles string
		var lastSeen sql.NullTime

		if err := rows.Scan(
			&contrib.GroveID, &contrib.HostID, &contrib.HostName, &localPath, &contrib.Mode, &contrib.Status,
			&profiles, &lastSeen,
		); err != nil {
			return nil, err
		}

		if localPath.Valid {
			contrib.LocalPath = localPath.String
		}
		if lastSeen.Valid {
			contrib.LastSeen = lastSeen.Time
		}
		unmarshalJSON(profiles, &contrib.Profiles)

		contributors = append(contributors, contrib)
	}

	return contributors, nil
}

func (s *SQLiteStore) UpdateContributorStatus(ctx context.Context, groveID, hostID, status string) error {
	now := time.Now()

	result, err := s.db.ExecContext(ctx, `
		UPDATE grove_contributors SET status = ?, last_seen = ? WHERE grove_id = ? AND host_id = ?
	`, status, now, groveID, hostID)
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

// Ensure SQLiteStore implements Store interface
var _ store.Store = (*SQLiteStore)(nil)
