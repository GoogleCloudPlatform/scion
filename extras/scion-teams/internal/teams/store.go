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

package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"
	"github.com/mitchellh/go-homedir"

	_ "modernc.org/sqlite"
)

// AdvisoryLockHandle is an alias for the shared lockloop type.
type AdvisoryLockHandle = lockloop.AdvisoryLockHandle

// NewAdvisoryLockHandle constructs an AdvisoryLockHandle (delegates to lockloop).
var NewAdvisoryLockHandle = lockloop.NewAdvisoryLockHandle

// Store defines the persistence interface for the Teams broker plugin.
type Store interface {
	// Channel links (Teams conversation <-> Scion project)
	CreateChannelLink(ctx context.Context, link *ChannelLink) error
	GetChannelLink(ctx context.Context, conversationID string) (*ChannelLink, error)
	GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error)
	GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error)
	UpdateChannelLink(ctx context.Context, link *ChannelLink) error
	DeleteChannelLink(ctx context.Context, conversationID string) error

	// Conversation references: stores serviceUrl + conversation details for proactive messaging
	UpsertConversationReference(ctx context.Context, ref *ConversationReference) error
	GetConversationReference(ctx context.Context, conversationID string) (*ConversationReference, error)
	GetConversationReferencesByTeam(ctx context.Context, teamID string) ([]*ConversationReference, error)

	// User mappings (Teams user <-> Scion identity)
	CreateUserMapping(ctx context.Context, mapping *TeamsUserMapping) error
	GetUserMapping(ctx context.Context, teamsUserID string) (*TeamsUserMapping, error)
	GetUserMappingByEmail(ctx context.Context, email string) (*TeamsUserMapping, error)
	DeleteUserMapping(ctx context.Context, teamsUserID string) error

	// Conversation context: tracks last chat context per user+project+agent
	SetConversationContext(ctx context.Context, cc *ConversationContext) error
	GetConversationContext(ctx context.Context, teamsUserID, projectID, agentSlug string) (*ConversationContext, error)
	GetLatestConversationContext(ctx context.Context, teamsUserID, projectID string) (*ConversationContext, error)

	// Agent cache
	SetProjectAgents(ctx context.Context, pa *ProjectAgents) error
	GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error)

	// Pending ask-user requests
	CreatePendingAskUser(ctx context.Context, req *PendingAskUser) error
	GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error)
	MarkAskUserResponded(ctx context.Context, requestID string) error
	DeleteExpiredAskUsers(ctx context.Context) (int, error)

	// Callback lookup
	CreateCallbackLookup(ctx context.Context, lookup *CallbackLookup) error
	GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error)
	DeleteExpiredCallbacks(ctx context.Context) (int, error)

	// Advisory locks (HA singleton coordination).
	// On Postgres, acquires a session-scoped lock on a dedicated connection.
	// The returned handle MUST be Released when the lock is no longer needed.
	// On SQLite, returns (true, noop-handle, nil) — always acquired in single-node mode.
	TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, handle *AdvisoryLockHandle, err error)

	// Lifecycle
	Close() error
}

// sqliteStore implements Store using SQLite via modernc.org/sqlite.
type sqliteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database at dbPath and
// initialises the schema. The returned Store must be closed when no
// longer needed.
func NewSQLiteStore(dbPath string) (Store, error) {
	// Expand ~ in the path; Go's database/sql and SQLite do not handle
	// tilde expansion, which would create a literal "~" directory.
	if expanded, err := homedir.Expand(dbPath); err == nil {
		dbPath = expanded
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	// Enable WAL mode for concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	// Set busy timeout to avoid SQLITE_BUSY errors under contention.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	s := &sqliteStore{db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return s, nil
}

func (s *sqliteStore) createSchema() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS channel_links (
	conversation_id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	team_name TEXT NOT NULL DEFAULT '',
	channel_name TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL,
	project_slug TEXT NOT NULL DEFAULT '',
	default_agent TEXT NOT NULL DEFAULT '',
	linked_by TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	active INTEGER NOT NULL DEFAULT 1,
	show_agent_to_agent INTEGER NOT NULL DEFAULT 0,
	show_assistant_reply INTEGER NOT NULL DEFAULT 1,
	show_state_changes INTEGER NOT NULL DEFAULT 0,
	chat_only INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_channel_links_project ON channel_links(project_id);
CREATE INDEX IF NOT EXISTS idx_channel_links_project_slug ON channel_links(project_slug);
CREATE INDEX IF NOT EXISTS idx_channel_links_team ON channel_links(team_id);

CREATE TABLE IF NOT EXISTS conversation_references (
	conversation_id TEXT PRIMARY KEY,
	service_url TEXT NOT NULL,
	bot_id TEXT NOT NULL DEFAULT '',
	bot_name TEXT NOT NULL DEFAULT '',
	tenant_id TEXT NOT NULL DEFAULT '',
	conversation_type TEXT NOT NULL DEFAULT '',
	team_id TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_conversation_references_team ON conversation_references(team_id);

CREATE TABLE IF NOT EXISTS user_mappings (
	teams_user_id TEXT PRIMARY KEY,
	teams_display_name TEXT NOT NULL DEFAULT '',
	scion_user_id TEXT NOT NULL DEFAULT '',
	scion_email TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	auto_linked INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_user_mappings_email ON user_mappings(scion_email);

CREATE TABLE IF NOT EXISTS conversation_context (
	teams_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	last_conversation_id TEXT NOT NULL,
	last_activity_id TEXT NOT NULL DEFAULT '',
	last_message_at TEXT NOT NULL,
	PRIMARY KEY (teams_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS project_agents (
	project_id TEXT PRIMARY KEY,
	agent_slugs TEXT NOT NULL DEFAULT '[]',
	refreshed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_ask_users (
	request_id TEXT PRIMARY KEY,
	activity_id TEXT NOT NULL DEFAULT '',
	conversation_id TEXT NOT NULL DEFAULT '',
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices TEXT NOT NULL DEFAULT '[]',
	expires_at TEXT NOT NULL,
	responded INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS callback_lookups (
	short_id TEXT PRIMARY KEY,
	full_data TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
`
	_, err := s.db.Exec(ddl)
	return err
}

// Close closes the underlying database connection.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// --- ChannelLink CRUD ---

func (s *sqliteStore) CreateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
INSERT INTO channel_links (conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET
	team_id=excluded.team_id, team_name=excluded.team_name, channel_name=excluded.channel_name,
	project_id=excluded.project_id, project_slug=excluded.project_slug,
	default_agent=excluded.default_agent, linked_by=excluded.linked_by, linked_at=excluded.linked_at,
	active=excluded.active, show_agent_to_agent=excluded.show_agent_to_agent,
	show_assistant_reply=excluded.show_assistant_reply, show_state_changes=excluded.show_state_changes,
	chat_only=excluded.chat_only`
	_, err := s.db.ExecContext(ctx, q,
		link.ConversationID, link.TeamID, link.TeamName, link.ChannelName,
		link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowAssistantReply), boolToInt(link.ShowStateChanges),
		boolToInt(link.ChatOnly))
	return err
}

func (s *sqliteStore) GetChannelLink(ctx context.Context, conversationID string) (*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM channel_links WHERE conversation_id = ?`
	row := s.db.QueryRowContext(ctx, q, conversationID)
	return scanChannelLink(row)
}

func (s *sqliteStore) GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM channel_links WHERE (project_id = ? OR project_slug = ?) AND active = 1`
	rows, err := s.db.QueryContext(ctx, q, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChannelLinks(rows)
}

func (s *sqliteStore) GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM channel_links`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChannelLinks(rows)
}

func (s *sqliteStore) UpdateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
UPDATE channel_links SET
	team_id=?, team_name=?, channel_name=?, project_id=?, project_slug=?, default_agent=?, linked_by=?, linked_at=?,
	active=?, show_agent_to_agent=?, show_assistant_reply=?, show_state_changes=?,
	chat_only=?
WHERE conversation_id=?`
	_, err := s.db.ExecContext(ctx, q,
		link.TeamID, link.TeamName, link.ChannelName, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowAssistantReply), boolToInt(link.ShowStateChanges),
		boolToInt(link.ChatOnly),
		link.ConversationID)
	return err
}

func (s *sqliteStore) DeleteChannelLink(ctx context.Context, conversationID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_links WHERE conversation_id = ?`, conversationID)
	return err
}

// --- ConversationReference ---

func (s *sqliteStore) UpsertConversationReference(ctx context.Context, ref *ConversationReference) error {
	const q = `
INSERT INTO conversation_references (conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET
	service_url=excluded.service_url, bot_id=excluded.bot_id, bot_name=excluded.bot_name,
	tenant_id=excluded.tenant_id, conversation_type=excluded.conversation_type,
	team_id=excluded.team_id, channel_id=excluded.channel_id, updated_at=excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q,
		ref.ConversationID, ref.ServiceURL, ref.BotID, ref.BotName,
		ref.TenantID, ref.ConversationType, ref.TeamID, ref.ChannelID,
		ref.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetConversationReference(ctx context.Context, conversationID string) (*ConversationReference, error) {
	const q = `SELECT conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at FROM conversation_references WHERE conversation_id = ?`
	row := s.db.QueryRowContext(ctx, q, conversationID)

	var ref ConversationReference
	var updatedAt string
	err := row.Scan(&ref.ConversationID, &ref.ServiceURL, &ref.BotID, &ref.BotName,
		&ref.TenantID, &ref.ConversationType, &ref.TeamID, &ref.ChannelID, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ref.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &ref, nil
}

func (s *sqliteStore) GetConversationReferencesByTeam(ctx context.Context, teamID string) ([]*ConversationReference, error) {
	const q = `SELECT conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at FROM conversation_references WHERE team_id = ?`
	rows, err := s.db.QueryContext(ctx, q, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []*ConversationReference
	for rows.Next() {
		var ref ConversationReference
		var updatedAt string
		if err := rows.Scan(&ref.ConversationID, &ref.ServiceURL, &ref.BotID, &ref.BotName,
			&ref.TenantID, &ref.ConversationType, &ref.TeamID, &ref.ChannelID, &updatedAt); err != nil {
			return nil, err
		}
		ref.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse updated_at: %w", err)
		}
		refs = append(refs, &ref)
	}
	return refs, rows.Err()
}

// --- User mappings ---

func (s *sqliteStore) CreateUserMapping(ctx context.Context, mapping *TeamsUserMapping) error {
	const q = `
INSERT INTO user_mappings (teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(teams_user_id) DO UPDATE SET
	teams_display_name=excluded.teams_display_name, scion_user_id=excluded.scion_user_id,
	scion_email=excluded.scion_email, linked_at=excluded.linked_at, auto_linked=excluded.auto_linked`
	_, err := s.db.ExecContext(ctx, q,
		mapping.TeamsUserID, mapping.TeamsDisplayName,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC().Format(time.RFC3339), boolToInt(mapping.AutoLinked))
	return err
}

func (s *sqliteStore) GetUserMapping(ctx context.Context, teamsUserID string) (*TeamsUserMapping, error) {
	const q = `SELECT teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked FROM user_mappings WHERE teams_user_id = ?`
	row := s.db.QueryRowContext(ctx, q, teamsUserID)
	return scanUserMapping(row)
}

func (s *sqliteStore) GetUserMappingByEmail(ctx context.Context, email string) (*TeamsUserMapping, error) {
	const q = `SELECT teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked FROM user_mappings WHERE scion_email = ?`
	row := s.db.QueryRowContext(ctx, q, email)
	return scanUserMapping(row)
}

func (s *sqliteStore) DeleteUserMapping(ctx context.Context, teamsUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mappings WHERE teams_user_id = ?`, teamsUserID)
	return err
}

// --- ConversationContext ---

func (s *sqliteStore) SetConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO conversation_context (teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(teams_user_id, project_id, agent_slug) DO UPDATE SET
	last_conversation_id=excluded.last_conversation_id, last_activity_id=excluded.last_activity_id, last_message_at=excluded.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.TeamsUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastConversationID, cc.LastActivityID, cc.LastMessageAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetConversationContext(ctx context.Context, teamsUserID, projectID, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at FROM conversation_context WHERE teams_user_id = ? AND project_id = ? AND agent_slug = ?`
	row := s.db.QueryRowContext(ctx, q, teamsUserID, projectID, agentSlug)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.TeamsUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastConversationID, &cc.LastActivityID, &lastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cc.LastMessageAt, err = time.Parse(time.RFC3339, lastMessageAt)
	if err != nil {
		return nil, fmt.Errorf("parse last_message_at: %w", err)
	}
	return &cc, nil
}

func (s *sqliteStore) GetLatestConversationContext(ctx context.Context, teamsUserID, projectID string) (*ConversationContext, error) {
	const q = `SELECT teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at
FROM conversation_context
WHERE teams_user_id = ? AND project_id = ?
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, teamsUserID, projectID)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.TeamsUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastConversationID, &cc.LastActivityID, &lastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cc.LastMessageAt, err = time.Parse(time.RFC3339, lastMessageAt)
	if err != nil {
		return nil, fmt.Errorf("parse last_message_at: %w", err)
	}
	return &cc, nil
}

// --- ProjectAgents ---

func (s *sqliteStore) SetProjectAgents(ctx context.Context, pa *ProjectAgents) error {
	slugsJSON, err := json.Marshal(pa.AgentSlugs)
	if err != nil {
		return fmt.Errorf("marshal agent_slugs: %w", err)
	}
	const q = `
INSERT INTO project_agents (project_id, agent_slugs, refreshed_at)
VALUES (?, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
	agent_slugs=excluded.agent_slugs, refreshed_at=excluded.refreshed_at`
	_, err = s.db.ExecContext(ctx, q, pa.ProjectID, string(slugsJSON), pa.RefreshedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error) {
	const q = `SELECT project_id, agent_slugs, refreshed_at FROM project_agents WHERE project_id = ?`
	row := s.db.QueryRowContext(ctx, q, projectID)

	var pa ProjectAgents
	var slugsJSON, refreshedAt string
	err := row.Scan(&pa.ProjectID, &slugsJSON, &refreshedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(slugsJSON), &pa.AgentSlugs); err != nil {
		return nil, fmt.Errorf("unmarshal agent_slugs: %w", err)
	}
	pa.RefreshedAt, err = time.Parse(time.RFC3339, refreshedAt)
	if err != nil {
		return nil, fmt.Errorf("parse refreshed_at: %w", err)
	}
	return &pa, nil
}

// --- PendingAskUser ---

func (s *sqliteStore) CreatePendingAskUser(ctx context.Context, req *PendingAskUser) error {
	choicesJSON, err := json.Marshal(req.Choices)
	if err != nil {
		return fmt.Errorf("marshal choices: %w", err)
	}
	const q = `
INSERT INTO pending_ask_users (request_id, activity_id, conversation_id, agent_slug, project_id, choices, expires_at, responded)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
	activity_id=excluded.activity_id, conversation_id=excluded.conversation_id, agent_slug=excluded.agent_slug,
	project_id=excluded.project_id, choices=excluded.choices, expires_at=excluded.expires_at,
	responded=excluded.responded`
	_, err = s.db.ExecContext(ctx, q,
		req.RequestID, req.ActivityID, req.ConversationID,
		req.AgentSlug, req.ProjectID, string(choicesJSON),
		req.ExpiresAt.UTC().Format(time.RFC3339), boolToInt(req.Responded))
	return err
}

func (s *sqliteStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, activity_id, conversation_id, agent_slug, project_id, choices, expires_at, responded FROM pending_ask_users WHERE request_id = ?`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON, expiresAt string
	var responded int
	err := row.Scan(&p.RequestID, &p.ActivityID, &p.ConversationID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &expiresAt, &responded)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(choicesJSON), &p.Choices); err != nil {
		return nil, fmt.Errorf("unmarshal choices: %w", err)
	}
	p.ExpiresAt, err = time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	p.Responded = responded != 0
	return &p, nil
}

func (s *sqliteStore) MarkAskUserResponded(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pending_ask_users SET responded = 1 WHERE request_id = ?`, requestID)
	return err
}

func (s *sqliteStore) DeleteExpiredAskUsers(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM pending_ask_users WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- CallbackLookup ---

func (s *sqliteStore) CreateCallbackLookup(ctx context.Context, lookup *CallbackLookup) error {
	const q = `
INSERT INTO callback_lookups (short_id, full_data, expires_at)
VALUES (?, ?, ?)
ON CONFLICT(short_id) DO UPDATE SET
	full_data=excluded.full_data, expires_at=excluded.expires_at`
	_, err := s.db.ExecContext(ctx, q,
		lookup.ShortID, lookup.FullData,
		lookup.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error) {
	const q = `SELECT short_id, full_data, expires_at FROM callback_lookups WHERE short_id = ?`
	row := s.db.QueryRowContext(ctx, q, shortID)

	var cl CallbackLookup
	var expiresAt string
	err := row.Scan(&cl.ShortID, &cl.FullData, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cl.ExpiresAt, err = time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	return &cl, nil
}

func (s *sqliteStore) DeleteExpiredCallbacks(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM callback_lookups WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- scan helpers ---

func scanChannelLink(row *sql.Row) (*ChannelLink, error) {
	var link ChannelLink
	var linkedAt string
	var active, showA2A, showAssistantReply, showStateChanges, chatOnly int
	err := row.Scan(&link.ConversationID, &link.TeamID, &link.TeamName, &link.ChannelName,
		&link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A,
		&showAssistantReply, &showStateChanges, &chatOnly)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	link.LinkedAt, err = time.Parse(time.RFC3339, linkedAt)
	if err != nil {
		return nil, fmt.Errorf("parse linked_at: %w", err)
	}
	link.Active = active != 0
	link.ShowAgentToAgent = showA2A != 0
	link.ShowAssistantReply = showAssistantReply != 0
	link.ShowStateChanges = showStateChanges != 0
	link.ChatOnly = chatOnly != 0
	return &link, nil
}

func scanChannelLinks(rows *sql.Rows) ([]*ChannelLink, error) {
	var links []*ChannelLink
	for rows.Next() {
		var link ChannelLink
		var linkedAt string
		var active, showA2A, showAssistantReply, showStateChanges, chatOnly int
		err := rows.Scan(&link.ConversationID, &link.TeamID, &link.TeamName, &link.ChannelName,
			&link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A,
			&showAssistantReply, &showStateChanges, &chatOnly)
		if err != nil {
			return nil, err
		}
		link.LinkedAt, err = time.Parse(time.RFC3339, linkedAt)
		if err != nil {
			return nil, fmt.Errorf("parse linked_at: %w", err)
		}
		link.Active = active != 0
		link.ShowAgentToAgent = showA2A != 0
		link.ShowAssistantReply = showAssistantReply != 0
		link.ShowStateChanges = showStateChanges != 0
		link.ChatOnly = chatOnly != 0
		links = append(links, &link)
	}
	return links, rows.Err()
}

func scanUserMapping(row *sql.Row) (*TeamsUserMapping, error) {
	var m TeamsUserMapping
	var linkedAt string
	var autoLinked int
	err := row.Scan(&m.TeamsUserID, &m.TeamsDisplayName, &m.ScionUserID, &m.ScionEmail, &linkedAt, &autoLinked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.LinkedAt, err = time.Parse(time.RFC3339, linkedAt)
	if err != nil {
		return nil, fmt.Errorf("parse linked_at: %w", err)
	}
	m.AutoLinked = autoLinked != 0
	return &m, nil
}

// --- Advisory locks (SQLite stub — single-node, no contention) ---

func (s *sqliteStore) TryAdvisoryLock(_ context.Context, _ int64) (bool, *AdvisoryLockHandle, error) {
	return true, NewAdvisoryLockHandle(
		func() error { return nil },
		func(_ context.Context) error { return nil },
	), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
