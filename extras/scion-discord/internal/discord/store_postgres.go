package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type postgresStore struct {
	db *sql.DB
}

func NewPostgresStore(databaseURL string) (Store, error) {
	db, err := sql.Open("pgx", databaseURL)
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
CREATE TABLE IF NOT EXISTS discord_channel_links (
	channel_id TEXT PRIMARY KEY,
	guild_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	project_slug TEXT NOT NULL DEFAULT '',
	default_agent TEXT NOT NULL DEFAULT '',
	linked_by TEXT NOT NULL DEFAULT '',
	linked_at TIMESTAMPTZ NOT NULL,
	active BOOLEAN NOT NULL DEFAULT TRUE,
	show_agent_to_agent BOOLEAN NOT NULL DEFAULT FALSE,
	show_assistant_reply BOOLEAN NOT NULL DEFAULT TRUE,
	show_state_changes BOOLEAN NOT NULL DEFAULT TRUE,
	notify_in_group BOOLEAN NOT NULL DEFAULT TRUE,
	chat_only BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_discord_channel_links_project ON discord_channel_links(project_id);
CREATE INDEX IF NOT EXISTS idx_discord_channel_links_guild ON discord_channel_links(guild_id);

CREATE TABLE IF NOT EXISTS discord_user_mappings (
	discord_user_id TEXT PRIMARY KEY,
	discord_username TEXT NOT NULL DEFAULT '',
	scion_user_id TEXT NOT NULL DEFAULT '',
	scion_email TEXT NOT NULL DEFAULT '',
	linked_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_discord_user_mappings_email ON discord_user_mappings(scion_email);
CREATE INDEX IF NOT EXISTS idx_discord_user_mappings_scion_id ON discord_user_mappings(scion_user_id);

CREATE TABLE IF NOT EXISTS discord_conversation_context (
	discord_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	last_channel_id TEXT NOT NULL,
	last_message_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (discord_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS discord_project_agents (
	project_id TEXT PRIMARY KEY,
	agent_slugs TEXT NOT NULL DEFAULT '[]',
	refreshed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS discord_pending_ask_users (
	request_id TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	channel_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices TEXT NOT NULL DEFAULT '[]',
	expires_at TIMESTAMPTZ NOT NULL,
	responded BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS discord_callback_lookups (
	short_id TEXT PRIMARY KEY,
	full_data TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS discord_notification_prefs (
	discord_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (discord_user_id, project_id, agent_slug)
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
INSERT INTO discord_channel_links (channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT(channel_id) DO UPDATE SET
	guild_id=EXCLUDED.guild_id, project_id=EXCLUDED.project_id, project_slug=EXCLUDED.project_slug,
	default_agent=EXCLUDED.default_agent, linked_by=EXCLUDED.linked_by, linked_at=EXCLUDED.linked_at,
	active=EXCLUDED.active, show_agent_to_agent=EXCLUDED.show_agent_to_agent,
	show_assistant_reply=EXCLUDED.show_assistant_reply, show_state_changes=EXCLUDED.show_state_changes,
	notify_in_group=EXCLUDED.notify_in_group, chat_only=EXCLUDED.chat_only`
	_, err := s.db.ExecContext(ctx, q,
		link.ChannelID, link.GuildID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC(),
		link.Active, link.ShowAgentToAgent,
		link.ShowAssistantReply, link.ShowStateChanges,
		link.NotifyInGroup, link.ChatOnly)
	return err
}

func (s *postgresStore) GetChannelLink(ctx context.Context, channelID string) (*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM discord_channel_links WHERE channel_id = $1`
	row := s.db.QueryRowContext(ctx, q, channelID)
	return pgScanChannelLink(row)
}

func (s *postgresStore) GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM discord_channel_links WHERE project_id = $1`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanChannelLinks(rows)
}

func (s *postgresStore) GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM discord_channel_links`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanChannelLinks(rows)
}

func (s *postgresStore) UpdateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
UPDATE discord_channel_links SET
	guild_id=$1, project_id=$2, project_slug=$3, default_agent=$4, linked_by=$5, linked_at=$6,
	active=$7, show_agent_to_agent=$8, show_assistant_reply=$9, show_state_changes=$10,
	notify_in_group=$11, chat_only=$12
WHERE channel_id=$13`
	_, err := s.db.ExecContext(ctx, q,
		link.GuildID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC(),
		link.Active, link.ShowAgentToAgent,
		link.ShowAssistantReply, link.ShowStateChanges,
		link.NotifyInGroup, link.ChatOnly,
		link.ChannelID)
	return err
}

func (s *postgresStore) DeactivateLinksForGuild(ctx context.Context, guildID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE discord_channel_links SET active = FALSE WHERE guild_id = $1`, guildID)
	return err
}

func (s *postgresStore) DeleteChannelLink(ctx context.Context, channelID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM discord_channel_links WHERE channel_id = $1`, channelID)
	return err
}

// --- User mappings ---

func (s *postgresStore) CreateUserMapping(ctx context.Context, mapping *DiscordUserMapping) error {
	const q = `
INSERT INTO discord_user_mappings (discord_user_id, discord_username, scion_user_id, scion_email, linked_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(discord_user_id) DO UPDATE SET
	discord_username=EXCLUDED.discord_username, scion_user_id=EXCLUDED.scion_user_id,
	scion_email=EXCLUDED.scion_email, linked_at=EXCLUDED.linked_at`
	_, err := s.db.ExecContext(ctx, q,
		mapping.DiscordUserID, mapping.DiscordUsername,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC())
	return err
}

func (s *postgresStore) GetUserMapping(ctx context.Context, discordUserID string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM discord_user_mappings WHERE discord_user_id = $1`
	row := s.db.QueryRowContext(ctx, q, discordUserID)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByEmail(ctx context.Context, email string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM discord_user_mappings WHERE scion_email = $1`
	row := s.db.QueryRowContext(ctx, q, email)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByScionUserID(ctx context.Context, userID string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM discord_user_mappings WHERE scion_user_id = $1`
	row := s.db.QueryRowContext(ctx, q, userID)
	return pgScanUserMapping(row)
}

func (s *postgresStore) DeleteUserMapping(ctx context.Context, discordUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM discord_user_mappings WHERE discord_user_id = $1`, discordUserID)
	return err
}

// --- ConversationContext ---

func (s *postgresStore) SetConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO discord_conversation_context (discord_user_id, project_id, agent_slug, last_channel_id, last_message_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(discord_user_id, project_id, agent_slug) DO UPDATE SET
	last_channel_id=EXCLUDED.last_channel_id, last_message_at=EXCLUDED.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.DiscordUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastChannelID, cc.LastMessageAt.UTC())
	return err
}

func (s *postgresStore) GetConversationContext(ctx context.Context, discordUserID, projectID, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, last_channel_id, last_message_at FROM discord_conversation_context WHERE discord_user_id = $1 AND project_id = $2 AND agent_slug = $3`
	row := s.db.QueryRowContext(ctx, q, discordUserID, projectID, agentSlug)

	var cc ConversationContext
	err := row.Scan(&cc.DiscordUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &cc.LastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

func (s *postgresStore) GetLatestConversationContext(ctx context.Context, discordUserID, projectID string) (*ConversationContext, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, last_channel_id, last_message_at
FROM discord_conversation_context
WHERE discord_user_id = $1 AND project_id = $2
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, discordUserID, projectID)

	var cc ConversationContext
	err := row.Scan(&cc.DiscordUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &cc.LastMessageAt)
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
INSERT INTO discord_project_agents (project_id, agent_slugs, refreshed_at)
VALUES ($1, $2, $3)
ON CONFLICT(project_id) DO UPDATE SET
	agent_slugs=EXCLUDED.agent_slugs, refreshed_at=EXCLUDED.refreshed_at`
	_, err = s.db.ExecContext(ctx, q, pa.ProjectID, string(slugsJSON), pa.RefreshedAt.UTC())
	return err
}

func (s *postgresStore) GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error) {
	const q = `SELECT project_id, agent_slugs, refreshed_at FROM discord_project_agents WHERE project_id = $1`
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
INSERT INTO discord_pending_ask_users (request_id, message_id, channel_id, agent_slug, project_id, choices, expires_at, responded)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT(request_id) DO UPDATE SET
	message_id=EXCLUDED.message_id, channel_id=EXCLUDED.channel_id, agent_slug=EXCLUDED.agent_slug,
	project_id=EXCLUDED.project_id, choices=EXCLUDED.choices, expires_at=EXCLUDED.expires_at,
	responded=EXCLUDED.responded`
	_, err = s.db.ExecContext(ctx, q,
		req.RequestID, req.MessageID, req.ChannelID,
		req.AgentSlug, req.ProjectID, string(choicesJSON),
		req.ExpiresAt.UTC(), req.Responded)
	return err
}

func (s *postgresStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, message_id, channel_id, agent_slug, project_id, choices, expires_at, responded FROM discord_pending_ask_users WHERE request_id = $1`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON string
	err := row.Scan(&p.RequestID, &p.MessageID, &p.ChannelID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &p.ExpiresAt, &p.Responded)
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
	_, err := s.db.ExecContext(ctx, `UPDATE discord_pending_ask_users SET responded = TRUE WHERE request_id = $1`, requestID)
	return err
}

func (s *postgresStore) DeleteExpiredAskUsers(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM discord_pending_ask_users WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- CallbackLookup ---

func (s *postgresStore) CreateCallbackLookup(ctx context.Context, lookup *CallbackLookup) error {
	const q = `
INSERT INTO discord_callback_lookups (short_id, full_data, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT(short_id) DO UPDATE SET
	full_data=EXCLUDED.full_data, expires_at=EXCLUDED.expires_at`
	_, err := s.db.ExecContext(ctx, q,
		lookup.ShortID, lookup.FullData,
		lookup.ExpiresAt.UTC())
	return err
}

func (s *postgresStore) GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error) {
	const q = `SELECT short_id, full_data, expires_at FROM discord_callback_lookups WHERE short_id = $1`
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
	result, err := s.db.ExecContext(ctx, `DELETE FROM discord_callback_lookups WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// --- NotificationPref ---

func (s *postgresStore) SetNotificationPref(ctx context.Context, pref *NotificationPref) error {
	const q = `
INSERT INTO discord_notification_prefs (discord_user_id, project_id, agent_slug, enabled, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(discord_user_id, project_id, agent_slug) DO UPDATE SET
	enabled=EXCLUDED.enabled, updated_at=EXCLUDED.updated_at`
	_, err := s.db.ExecContext(ctx, q,
		pref.DiscordUserID, pref.ProjectID, pref.AgentSlug,
		pref.Enabled, pref.UpdatedAt.UTC())
	return err
}

func (s *postgresStore) GetNotificationPrefs(ctx context.Context, discordUserID, projectID string) ([]*NotificationPref, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, enabled, updated_at FROM discord_notification_prefs WHERE discord_user_id = $1 AND project_id = $2`
	rows, err := s.db.QueryContext(ctx, q, discordUserID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prefs []*NotificationPref
	for rows.Next() {
		var p NotificationPref
		if err := rows.Scan(&p.DiscordUserID, &p.ProjectID, &p.AgentSlug, &p.Enabled, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prefs = append(prefs, &p)
	}
	return prefs, rows.Err()
}

// --- scan helpers ---

func pgScanChannelLink(row *sql.Row) (*ChannelLink, error) {
	var link ChannelLink
	err := row.Scan(&link.ChannelID, &link.GuildID, &link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
		&link.ShowAssistantReply, &link.ShowStateChanges, &link.NotifyInGroup, &link.ChatOnly)
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
		err := rows.Scan(&link.ChannelID, &link.GuildID, &link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
			&link.ShowAssistantReply, &link.ShowStateChanges, &link.NotifyInGroup, &link.ChatOnly)
		if err != nil {
			return nil, err
		}
		links = append(links, &link)
	}
	return links, rows.Err()
}

func pgScanUserMapping(row *sql.Row) (*DiscordUserMapping, error) {
	var m DiscordUserMapping
	err := row.Scan(&m.DiscordUserID, &m.DiscordUsername, &m.ScionUserID, &m.ScionEmail, &m.LinkedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
