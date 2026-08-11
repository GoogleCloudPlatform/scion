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

	_ "github.com/jackc/pgx/v5/stdlib"
)

type postgresStore struct {
	db *sql.DB
}

// NewPostgresStore opens a PostgreSQL database using the given connection
// string and initialises the schema. The returned Store must be closed when
// no longer needed.
func NewPostgresStore(connString string) (Store, error) {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return nil, fmt.Errorf("open postgres database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	s := &postgresStore{db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return s, nil
}

func (s *postgresStore) createSchema() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS teams_channel_links (
	conversation_id TEXT PRIMARY KEY,
	team_id TEXT NOT NULL DEFAULT '',
	team_name TEXT NOT NULL DEFAULT '',
	channel_name TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL,
	project_slug TEXT NOT NULL DEFAULT '',
	default_agent TEXT NOT NULL DEFAULT '',
	linked_by TEXT NOT NULL DEFAULT '',
	linked_at TIMESTAMPTZ NOT NULL,
	active BOOLEAN NOT NULL DEFAULT TRUE,
	show_agent_to_agent BOOLEAN NOT NULL DEFAULT FALSE,
	show_assistant_reply BOOLEAN NOT NULL DEFAULT TRUE,
	show_state_changes BOOLEAN NOT NULL DEFAULT FALSE,
	chat_only BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_teams_channel_links_project ON teams_channel_links(project_id);
CREATE INDEX IF NOT EXISTS idx_teams_channel_links_project_slug ON teams_channel_links(project_slug);
CREATE INDEX IF NOT EXISTS idx_teams_channel_links_team ON teams_channel_links(team_id);

CREATE TABLE IF NOT EXISTS teams_conversation_references (
	conversation_id TEXT PRIMARY KEY,
	service_url TEXT NOT NULL,
	bot_id TEXT NOT NULL DEFAULT '',
	bot_name TEXT NOT NULL DEFAULT '',
	tenant_id TEXT NOT NULL DEFAULT '',
	conversation_type TEXT NOT NULL DEFAULT '',
	team_id TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '',
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_teams_conversation_references_team ON teams_conversation_references(team_id);

CREATE TABLE IF NOT EXISTS teams_user_mappings (
	teams_user_id TEXT PRIMARY KEY,
	teams_display_name TEXT NOT NULL DEFAULT '',
	scion_user_id TEXT NOT NULL DEFAULT '',
	scion_email TEXT NOT NULL DEFAULT '',
	linked_at TIMESTAMPTZ NOT NULL,
	auto_linked BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_teams_user_mappings_email ON teams_user_mappings(scion_email);

CREATE TABLE IF NOT EXISTS teams_conversation_context (
	teams_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	last_conversation_id TEXT NOT NULL,
	last_activity_id TEXT NOT NULL DEFAULT '',
	last_message_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (teams_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS teams_project_agents (
	project_id TEXT PRIMARY KEY,
	agent_slugs TEXT NOT NULL DEFAULT '[]',
	refreshed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS teams_pending_ask_users (
	request_id TEXT PRIMARY KEY,
	activity_id TEXT NOT NULL DEFAULT '',
	conversation_id TEXT NOT NULL DEFAULT '',
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices TEXT NOT NULL DEFAULT '[]',
	expires_at TIMESTAMPTZ NOT NULL,
	responded BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS teams_callback_lookups (
	short_id TEXT PRIMARY KEY,
	full_data TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);
`
	_, err := s.db.Exec(ddl)
	return err
}

func (s *postgresStore) Close() error {
	return s.db.Close()
}

// --- ChannelLink CRUD ---

func (s *postgresStore) CreateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
INSERT INTO teams_channel_links (conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT(conversation_id) DO UPDATE SET
	team_id=EXCLUDED.team_id, team_name=EXCLUDED.team_name, channel_name=EXCLUDED.channel_name,
	project_id=EXCLUDED.project_id, project_slug=EXCLUDED.project_slug,
	default_agent=EXCLUDED.default_agent, linked_by=EXCLUDED.linked_by, linked_at=EXCLUDED.linked_at,
	active=EXCLUDED.active, show_agent_to_agent=EXCLUDED.show_agent_to_agent,
	show_assistant_reply=EXCLUDED.show_assistant_reply, show_state_changes=EXCLUDED.show_state_changes,
	chat_only=EXCLUDED.chat_only`
	_, err := s.db.ExecContext(ctx, q,
		link.ConversationID, link.TeamID, link.TeamName, link.ChannelName,
		link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC(),
		link.Active, link.ShowAgentToAgent,
		link.ShowAssistantReply, link.ShowStateChanges,
		link.ChatOnly)
	return err
}

func (s *postgresStore) GetChannelLink(ctx context.Context, conversationID string) (*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM teams_channel_links WHERE conversation_id = $1`
	row := s.db.QueryRowContext(ctx, q, conversationID)
	return pgScanChannelLink(row)
}

func (s *postgresStore) GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM teams_channel_links WHERE (project_id = $1 OR project_slug = $1) AND active = true`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanChannelLinks(rows)
}

func (s *postgresStore) GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error) {
	const q = `SELECT conversation_id, team_id, team_name, channel_name, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, chat_only FROM teams_channel_links`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanChannelLinks(rows)
}

func (s *postgresStore) UpdateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
UPDATE teams_channel_links SET
	team_id=$1, team_name=$2, channel_name=$3, project_id=$4, project_slug=$5, default_agent=$6, linked_by=$7, linked_at=$8,
	active=$9, show_agent_to_agent=$10, show_assistant_reply=$11, show_state_changes=$12,
	chat_only=$13
WHERE conversation_id=$14`
	_, err := s.db.ExecContext(ctx, q,
		link.TeamID, link.TeamName, link.ChannelName, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC(),
		link.Active, link.ShowAgentToAgent,
		link.ShowAssistantReply, link.ShowStateChanges,
		link.ChatOnly,
		link.ConversationID)
	return err
}

func (s *postgresStore) DeleteChannelLink(ctx context.Context, conversationID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM teams_channel_links WHERE conversation_id = $1`, conversationID)
	return err
}

// --- ConversationReference ---

func (s *postgresStore) UpsertConversationReference(ctx context.Context, ref *ConversationReference) error {
	const q = `
INSERT INTO teams_conversation_references (conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT(conversation_id) DO UPDATE SET
	service_url=EXCLUDED.service_url, bot_id=EXCLUDED.bot_id, bot_name=EXCLUDED.bot_name,
	tenant_id=EXCLUDED.tenant_id, conversation_type=EXCLUDED.conversation_type,
	team_id=EXCLUDED.team_id, channel_id=EXCLUDED.channel_id, updated_at=EXCLUDED.updated_at`
	_, err := s.db.ExecContext(ctx, q,
		ref.ConversationID, ref.ServiceURL, ref.BotID, ref.BotName,
		ref.TenantID, ref.ConversationType, ref.TeamID, ref.ChannelID,
		ref.UpdatedAt.UTC())
	return err
}

func (s *postgresStore) GetConversationReference(ctx context.Context, conversationID string) (*ConversationReference, error) {
	const q = `SELECT conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at FROM teams_conversation_references WHERE conversation_id = $1`
	row := s.db.QueryRowContext(ctx, q, conversationID)

	var ref ConversationReference
	err := row.Scan(&ref.ConversationID, &ref.ServiceURL, &ref.BotID, &ref.BotName,
		&ref.TenantID, &ref.ConversationType, &ref.TeamID, &ref.ChannelID, &ref.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (s *postgresStore) GetConversationReferencesByTeam(ctx context.Context, teamID string) ([]*ConversationReference, error) {
	const q = `SELECT conversation_id, service_url, bot_id, bot_name, tenant_id, conversation_type, team_id, channel_id, updated_at FROM teams_conversation_references WHERE team_id = $1`
	rows, err := s.db.QueryContext(ctx, q, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []*ConversationReference
	for rows.Next() {
		var ref ConversationReference
		if err := rows.Scan(&ref.ConversationID, &ref.ServiceURL, &ref.BotID, &ref.BotName,
			&ref.TenantID, &ref.ConversationType, &ref.TeamID, &ref.ChannelID, &ref.UpdatedAt); err != nil {
			return nil, err
		}
		refs = append(refs, &ref)
	}
	return refs, rows.Err()
}

// --- User mappings ---

func (s *postgresStore) CreateUserMapping(ctx context.Context, mapping *TeamsUserMapping) error {
	const q = `
INSERT INTO teams_user_mappings (teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT(teams_user_id) DO UPDATE SET
	teams_display_name=EXCLUDED.teams_display_name, scion_user_id=EXCLUDED.scion_user_id,
	scion_email=EXCLUDED.scion_email, linked_at=EXCLUDED.linked_at, auto_linked=EXCLUDED.auto_linked`
	_, err := s.db.ExecContext(ctx, q,
		mapping.TeamsUserID, mapping.TeamsDisplayName,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC(), mapping.AutoLinked)
	return err
}

func (s *postgresStore) GetUserMapping(ctx context.Context, teamsUserID string) (*TeamsUserMapping, error) {
	const q = `SELECT teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked FROM teams_user_mappings WHERE teams_user_id = $1`
	row := s.db.QueryRowContext(ctx, q, teamsUserID)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByEmail(ctx context.Context, email string) (*TeamsUserMapping, error) {
	const q = `SELECT teams_user_id, teams_display_name, scion_user_id, scion_email, linked_at, auto_linked FROM teams_user_mappings WHERE scion_email = $1`
	row := s.db.QueryRowContext(ctx, q, email)
	return pgScanUserMapping(row)
}

func (s *postgresStore) DeleteUserMapping(ctx context.Context, teamsUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM teams_user_mappings WHERE teams_user_id = $1`, teamsUserID)
	return err
}

// --- ConversationContext ---

func (s *postgresStore) SetConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO teams_conversation_context (teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT(teams_user_id, project_id, agent_slug) DO UPDATE SET
	last_conversation_id=EXCLUDED.last_conversation_id, last_activity_id=EXCLUDED.last_activity_id, last_message_at=EXCLUDED.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.TeamsUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastConversationID, cc.LastActivityID, cc.LastMessageAt.UTC())
	return err
}

func (s *postgresStore) GetConversationContext(ctx context.Context, teamsUserID, projectID, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at FROM teams_conversation_context WHERE teams_user_id = $1 AND project_id = $2 AND agent_slug = $3`
	row := s.db.QueryRowContext(ctx, q, teamsUserID, projectID, agentSlug)

	var cc ConversationContext
	err := row.Scan(&cc.TeamsUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastConversationID, &cc.LastActivityID, &cc.LastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

func (s *postgresStore) GetLatestConversationContext(ctx context.Context, teamsUserID, projectID string) (*ConversationContext, error) {
	const q = `SELECT teams_user_id, project_id, agent_slug, last_conversation_id, last_activity_id, last_message_at
FROM teams_conversation_context
WHERE teams_user_id = $1 AND project_id = $2
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, teamsUserID, projectID)

	var cc ConversationContext
	err := row.Scan(&cc.TeamsUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastConversationID, &cc.LastActivityID, &cc.LastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

// --- ProjectAgents ---

func (s *postgresStore) SetProjectAgents(ctx context.Context, pa *ProjectAgents) error {
	slugsJSON, err := json.Marshal(pa.AgentSlugs)
	if err != nil {
		return fmt.Errorf("marshal agent_slugs: %w", err)
	}
	const q = `
INSERT INTO teams_project_agents (project_id, agent_slugs, refreshed_at)
VALUES ($1, $2, $3)
ON CONFLICT(project_id) DO UPDATE SET
	agent_slugs=EXCLUDED.agent_slugs, refreshed_at=EXCLUDED.refreshed_at`
	_, err = s.db.ExecContext(ctx, q, pa.ProjectID, string(slugsJSON), pa.RefreshedAt.UTC())
	return err
}

func (s *postgresStore) GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error) {
	const q = `SELECT project_id, agent_slugs, refreshed_at FROM teams_project_agents WHERE project_id = $1`
	row := s.db.QueryRowContext(ctx, q, projectID)

	var pa ProjectAgents
	var slugsJSON string
	err := row.Scan(&pa.ProjectID, &slugsJSON, &pa.RefreshedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(slugsJSON), &pa.AgentSlugs); err != nil {
		return nil, fmt.Errorf("unmarshal agent_slugs: %w", err)
	}
	return &pa, nil
}

// --- PendingAskUser ---

func (s *postgresStore) CreatePendingAskUser(ctx context.Context, req *PendingAskUser) error {
	choicesJSON, err := json.Marshal(req.Choices)
	if err != nil {
		return fmt.Errorf("marshal choices: %w", err)
	}
	const q = `
INSERT INTO teams_pending_ask_users (request_id, activity_id, conversation_id, agent_slug, project_id, choices, expires_at, responded)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT(request_id) DO UPDATE SET
	activity_id=EXCLUDED.activity_id, conversation_id=EXCLUDED.conversation_id, agent_slug=EXCLUDED.agent_slug,
	project_id=EXCLUDED.project_id, choices=EXCLUDED.choices, expires_at=EXCLUDED.expires_at,
	responded=EXCLUDED.responded`
	_, err = s.db.ExecContext(ctx, q,
		req.RequestID, req.ActivityID, req.ConversationID,
		req.AgentSlug, req.ProjectID, string(choicesJSON),
		req.ExpiresAt.UTC(), req.Responded)
	return err
}

func (s *postgresStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, activity_id, conversation_id, agent_slug, project_id, choices, expires_at, responded FROM teams_pending_ask_users WHERE request_id = $1`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON string
	err := row.Scan(&p.RequestID, &p.ActivityID, &p.ConversationID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &p.ExpiresAt, &p.Responded)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(choicesJSON), &p.Choices); err != nil {
		return nil, fmt.Errorf("unmarshal choices: %w", err)
	}
	return &p, nil
}

func (s *postgresStore) MarkAskUserResponded(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE teams_pending_ask_users SET responded = TRUE WHERE request_id = $1`, requestID)
	return err
}

func (s *postgresStore) DeleteExpiredAskUsers(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM teams_pending_ask_users WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- CallbackLookup ---

func (s *postgresStore) CreateCallbackLookup(ctx context.Context, lookup *CallbackLookup) error {
	const q = `
INSERT INTO teams_callback_lookups (short_id, full_data, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT(short_id) DO UPDATE SET
	full_data=EXCLUDED.full_data, expires_at=EXCLUDED.expires_at`
	_, err := s.db.ExecContext(ctx, q,
		lookup.ShortID, lookup.FullData,
		lookup.ExpiresAt.UTC())
	return err
}

func (s *postgresStore) GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error) {
	const q = `SELECT short_id, full_data, expires_at FROM teams_callback_lookups WHERE short_id = $1`
	row := s.db.QueryRowContext(ctx, q, shortID)

	var cl CallbackLookup
	err := row.Scan(&cl.ShortID, &cl.FullData, &cl.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cl, nil
}

func (s *postgresStore) DeleteExpiredCallbacks(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM teams_callback_lookups WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- scan helpers ---

func pgScanChannelLink(row *sql.Row) (*ChannelLink, error) {
	var link ChannelLink
	err := row.Scan(&link.ConversationID, &link.TeamID, &link.TeamName, &link.ChannelName,
		&link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
		&link.ShowAssistantReply, &link.ShowStateChanges, &link.ChatOnly)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func pgScanChannelLinks(rows *sql.Rows) ([]*ChannelLink, error) {
	var links []*ChannelLink
	for rows.Next() {
		var link ChannelLink
		err := rows.Scan(&link.ConversationID, &link.TeamID, &link.TeamName, &link.ChannelName,
			&link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
			&link.ShowAssistantReply, &link.ShowStateChanges, &link.ChatOnly)
		if err != nil {
			return nil, err
		}
		links = append(links, &link)
	}
	return links, rows.Err()
}

func pgScanUserMapping(row *sql.Row) (*TeamsUserMapping, error) {
	var m TeamsUserMapping
	err := row.Scan(&m.TeamsUserID, &m.TeamsDisplayName, &m.ScionUserID, &m.ScionEmail, &m.LinkedAt, &m.AutoLinked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// --- Advisory locks ---

// TryAdvisoryLock acquires a session-scoped advisory lock on a dedicated
// connection pinned from the pool. The lock stays alive as long as the
// connection lives. Mirrors the Discord pattern in store_postgres.go.
func (s *postgresStore) TryAdvisoryLock(ctx context.Context, key int64) (bool, *AdvisoryLockHandle, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("advisory lock: acquiring connection: %w", err)
	}

	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Close()
		return false, nil, fmt.Errorf("advisory lock: pg_try_advisory_lock(%d): %w", key, err)
	}

	if !acquired {
		conn.Close()
		return false, nil, nil
	}

	handle := NewAdvisoryLockHandle(
		func() error {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", key)
			closeErr := conn.Close()
			if unlockErr != nil {
				return fmt.Errorf("advisory lock: pg_advisory_unlock(%d): %w", key, unlockErr)
			}
			return closeErr
		},
		func(ctx context.Context) error {
			return conn.PingContext(ctx)
		},
	)
	return true, handle, nil
}
