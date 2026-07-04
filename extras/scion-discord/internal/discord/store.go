package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"

	_ "modernc.org/sqlite"
)

// AdvisoryLockHandle is an alias for the shared lockloop type.
type AdvisoryLockHandle = lockloop.AdvisoryLockHandle

// NewAdvisoryLockHandle constructs an AdvisoryLockHandle (delegates to lockloop).
var NewAdvisoryLockHandle = lockloop.NewAdvisoryLockHandle

// Store defines the persistence interface for the Discord broker plugin.
type Store interface {
	// Channel links (Discord channel <-> Scion project)
	CreateChannelLink(ctx context.Context, link *ChannelLink) error
	GetChannelLink(ctx context.Context, channelID string) (*ChannelLink, error)
	GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error)
	GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error)
	UpdateChannelLink(ctx context.Context, link *ChannelLink) error
	DeactivateLinksForGuild(ctx context.Context, guildID string) error
	DeleteChannelLink(ctx context.Context, channelID string) error

	// User mappings (Discord user <-> Scion identity)
	CreateUserMapping(ctx context.Context, mapping *DiscordUserMapping) error
	GetUserMapping(ctx context.Context, discordUserID string) (*DiscordUserMapping, error)
	GetUserMappingByEmail(ctx context.Context, email string) (*DiscordUserMapping, error)
	GetUserMappingByScionUserID(ctx context.Context, userID string) (*DiscordUserMapping, error)
	DeleteUserMapping(ctx context.Context, discordUserID string) error

	// Conversation context
	SetConversationContext(ctx context.Context, cc *ConversationContext) error
	GetConversationContext(ctx context.Context, discordUserID, projectID, agentSlug string) (*ConversationContext, error)
	GetLatestConversationContext(ctx context.Context, discordUserID, projectID string) (*ConversationContext, error)

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

	// Notification preferences
	SetNotificationPref(ctx context.Context, pref *NotificationPref) error
	GetNotificationPrefs(ctx context.Context, discordUserID, projectID string) ([]*NotificationPref, error)

	// Advisory locks (HA singleton coordination).
	// On Postgres, acquires a session-scoped lock on a dedicated connection.
	// The returned handle MUST be Released when the lock is no longer needed.
	TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, handle *AdvisoryLockHandle, err error)

	// Lifecycle
	Close() error
}

// ChannelLink represents a Discord channel linked to a Scion project.
type ChannelLink struct {
	ChannelID          string
	GuildID            string
	ProjectID          string
	ProjectSlug        string
	DefaultAgent       string
	LinkedBy           string // Discord user ID who ran /setup
	LinkedAt           time.Time
	Active             bool
	ShowAgentToAgent   bool
	ShowAssistantReply bool
	ShowStateChanges   bool
	NotifyInGroup      bool
	ChatOnly           bool
}

// DiscordUserMapping links a Discord user to a Scion user identity.
type DiscordUserMapping struct {
	DiscordUserID   string
	DiscordUsername string
	ScionUserID     string
	ScionEmail      string
	LinkedAt        time.Time
}

// ConversationContext tracks the last chat context for a user+project+agent tuple.
type ConversationContext struct {
	DiscordUserID string
	ProjectID     string
	AgentSlug     string
	LastChannelID string
	LastMessageAt time.Time
}

// ProjectAgents caches the list of agents for a project.
type ProjectAgents struct {
	ProjectID   string
	AgentSlugs  []string
	RefreshedAt time.Time
}

// PendingAskUser represents an ask-user callback awaiting a Discord user response.
type PendingAskUser struct {
	RequestID string
	MessageID string // Discord message snowflake
	ChannelID string
	AgentSlug string
	ProjectID string
	Choices   []string
	ExpiresAt time.Time
	Responded bool
}

// CallbackLookup maps a short callback ID to its full data payload.
type CallbackLookup struct {
	ShortID   string
	FullData  string
	ExpiresAt time.Time
}

// NotificationPref stores per-user, per-agent notification subscription state.
type NotificationPref struct {
	DiscordUserID string
	ProjectID     string
	AgentSlug     string
	Enabled       bool
	UpdatedAt     time.Time
}

// sqliteStore implements Store using SQLite via modernc.org/sqlite.
type sqliteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database at dbPath and
// initialises the schema. The returned Store must be closed when no
// longer needed.
func NewSQLiteStore(dbPath string) (Store, error) {
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
	channel_id TEXT PRIMARY KEY,
	guild_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	project_slug TEXT NOT NULL DEFAULT '',
	default_agent TEXT NOT NULL DEFAULT '',
	linked_by TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	active INTEGER NOT NULL DEFAULT 1,
	show_agent_to_agent INTEGER NOT NULL DEFAULT 0,
	show_assistant_reply INTEGER NOT NULL DEFAULT 1,
	show_state_changes INTEGER NOT NULL DEFAULT 1,
	notify_in_group INTEGER NOT NULL DEFAULT 1,
	chat_only INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_channel_links_project ON channel_links(project_id);
CREATE INDEX IF NOT EXISTS idx_channel_links_guild ON channel_links(guild_id);

CREATE TABLE IF NOT EXISTS user_mappings (
	discord_user_id TEXT PRIMARY KEY,
	discord_username TEXT NOT NULL DEFAULT '',
	scion_user_id TEXT NOT NULL DEFAULT '',
	scion_email TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_mappings_email ON user_mappings(scion_email);
CREATE INDEX IF NOT EXISTS idx_user_mappings_scion_id ON user_mappings(scion_user_id);

CREATE TABLE IF NOT EXISTS conversation_context (
	discord_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	last_channel_id TEXT NOT NULL,
	last_message_at TEXT NOT NULL,
	PRIMARY KEY (discord_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS project_agents (
	project_id TEXT PRIMARY KEY,
	agent_slugs TEXT NOT NULL DEFAULT '[]',
	refreshed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_ask_users (
	request_id TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	channel_id TEXT NOT NULL,
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

CREATE TABLE IF NOT EXISTS notification_prefs (
	discord_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (discord_user_id, project_id, agent_slug)
);
`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return err
	}
	s.migrateSchema()
	return nil
}

func (s *sqliteStore) migrateSchema() {
	migrations := []string{
		`ALTER TABLE channel_links ADD COLUMN show_state_changes INTEGER NOT NULL DEFAULT 1`,
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				slog.Warn("Failed to run migration", "migration", m, "error", err)
			}
		}
	}
}

// Close closes the underlying database connection.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// --- ChannelLink CRUD ---

func (s *sqliteStore) CreateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
INSERT INTO channel_links (channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(channel_id) DO UPDATE SET
	guild_id=excluded.guild_id, project_id=excluded.project_id, project_slug=excluded.project_slug,
	default_agent=excluded.default_agent, linked_by=excluded.linked_by, linked_at=excluded.linked_at,
	active=excluded.active, show_agent_to_agent=excluded.show_agent_to_agent,
	show_assistant_reply=excluded.show_assistant_reply, show_state_changes=excluded.show_state_changes,
	notify_in_group=excluded.notify_in_group, chat_only=excluded.chat_only`
	_, err := s.db.ExecContext(ctx, q,
		link.ChannelID, link.GuildID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowAssistantReply), boolToInt(link.ShowStateChanges),
		boolToInt(link.NotifyInGroup), boolToInt(link.ChatOnly))
	return err
}

func (s *sqliteStore) GetChannelLink(ctx context.Context, channelID string) (*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM channel_links WHERE channel_id = ?`
	row := s.db.QueryRowContext(ctx, q, channelID)
	return scanChannelLink(row)
}

func (s *sqliteStore) GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM channel_links WHERE project_id = ?`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChannelLinks(rows)
}

func (s *sqliteStore) GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, guild_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_assistant_reply, show_state_changes, notify_in_group, chat_only FROM channel_links`
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
	guild_id=?, project_id=?, project_slug=?, default_agent=?, linked_by=?, linked_at=?,
	active=?, show_agent_to_agent=?, show_assistant_reply=?, show_state_changes=?,
	notify_in_group=?, chat_only=?
WHERE channel_id=?`
	_, err := s.db.ExecContext(ctx, q,
		link.GuildID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowAssistantReply), boolToInt(link.ShowStateChanges),
		boolToInt(link.NotifyInGroup), boolToInt(link.ChatOnly),
		link.ChannelID)
	return err
}

func (s *sqliteStore) DeactivateLinksForGuild(ctx context.Context, guildID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_links SET active = 0 WHERE guild_id = ?`, guildID)
	return err
}

func (s *sqliteStore) DeleteChannelLink(ctx context.Context, channelID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_links WHERE channel_id = ?`, channelID)
	return err
}

// --- User mappings ---

func (s *sqliteStore) CreateUserMapping(ctx context.Context, mapping *DiscordUserMapping) error {
	const q = `
INSERT INTO user_mappings (discord_user_id, discord_username, scion_user_id, scion_email, linked_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(discord_user_id) DO UPDATE SET
	discord_username=excluded.discord_username, scion_user_id=excluded.scion_user_id,
	scion_email=excluded.scion_email, linked_at=excluded.linked_at`
	_, err := s.db.ExecContext(ctx, q,
		mapping.DiscordUserID, mapping.DiscordUsername,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetUserMapping(ctx context.Context, discordUserID string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE discord_user_id = ?`
	row := s.db.QueryRowContext(ctx, q, discordUserID)
	return scanUserMapping(row)
}

func (s *sqliteStore) GetUserMappingByEmail(ctx context.Context, email string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE scion_email = ?`
	row := s.db.QueryRowContext(ctx, q, email)
	return scanUserMapping(row)
}

func (s *sqliteStore) GetUserMappingByScionUserID(ctx context.Context, userID string) (*DiscordUserMapping, error) {
	const q = `SELECT discord_user_id, discord_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE scion_user_id = ?`
	row := s.db.QueryRowContext(ctx, q, userID)
	return scanUserMapping(row)
}

func (s *sqliteStore) DeleteUserMapping(ctx context.Context, discordUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mappings WHERE discord_user_id = ?`, discordUserID)
	return err
}

// --- ConversationContext ---

func (s *sqliteStore) SetConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO conversation_context (discord_user_id, project_id, agent_slug, last_channel_id, last_message_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(discord_user_id, project_id, agent_slug) DO UPDATE SET
	last_channel_id=excluded.last_channel_id, last_message_at=excluded.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.DiscordUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastChannelID, cc.LastMessageAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetConversationContext(ctx context.Context, discordUserID, projectID, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, last_channel_id, last_message_at FROM conversation_context WHERE discord_user_id = ? AND project_id = ? AND agent_slug = ?`
	row := s.db.QueryRowContext(ctx, q, discordUserID, projectID, agentSlug)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.DiscordUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &lastMessageAt)
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

func (s *sqliteStore) GetLatestConversationContext(ctx context.Context, discordUserID, projectID string) (*ConversationContext, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, last_channel_id, last_message_at
FROM conversation_context
WHERE discord_user_id = ? AND project_id = ?
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, discordUserID, projectID)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.DiscordUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &lastMessageAt)
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
INSERT INTO pending_ask_users (request_id, message_id, channel_id, agent_slug, project_id, choices, expires_at, responded)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
	message_id=excluded.message_id, channel_id=excluded.channel_id, agent_slug=excluded.agent_slug,
	project_id=excluded.project_id, choices=excluded.choices, expires_at=excluded.expires_at,
	responded=excluded.responded`
	_, err = s.db.ExecContext(ctx, q,
		req.RequestID, req.MessageID, req.ChannelID,
		req.AgentSlug, req.ProjectID, string(choicesJSON),
		req.ExpiresAt.UTC().Format(time.RFC3339), boolToInt(req.Responded))
	return err
}

func (s *sqliteStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, message_id, channel_id, agent_slug, project_id, choices, expires_at, responded FROM pending_ask_users WHERE request_id = ?`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON, expiresAt string
	var responded int
	err := row.Scan(&p.RequestID, &p.MessageID, &p.ChannelID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &expiresAt, &responded)
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

// --- NotificationPref ---

func (s *sqliteStore) SetNotificationPref(ctx context.Context, pref *NotificationPref) error {
	const q = `
INSERT INTO notification_prefs (discord_user_id, project_id, agent_slug, enabled, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(discord_user_id, project_id, agent_slug) DO UPDATE SET
	enabled=excluded.enabled, updated_at=excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q,
		pref.DiscordUserID, pref.ProjectID, pref.AgentSlug,
		boolToInt(pref.Enabled), pref.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetNotificationPrefs(ctx context.Context, discordUserID, projectID string) ([]*NotificationPref, error) {
	const q = `SELECT discord_user_id, project_id, agent_slug, enabled, updated_at FROM notification_prefs WHERE discord_user_id = ? AND project_id = ?`
	rows, err := s.db.QueryContext(ctx, q, discordUserID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prefs []*NotificationPref
	for rows.Next() {
		var p NotificationPref
		var enabled int
		var updatedAt string
		if err := rows.Scan(&p.DiscordUserID, &p.ProjectID, &p.AgentSlug, &enabled, &updatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		p.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse updated_at: %w", err)
		}
		prefs = append(prefs, &p)
	}
	return prefs, rows.Err()
}

// --- scan helpers ---

func scanChannelLink(row *sql.Row) (*ChannelLink, error) {
	var link ChannelLink
	var linkedAt string
	var active, showA2A, showAssistantReply, showStateChanges, notifyInGroup, chatOnly int
	err := row.Scan(&link.ChannelID, &link.GuildID, &link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A,
		&showAssistantReply, &showStateChanges, &notifyInGroup, &chatOnly)
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
	link.NotifyInGroup = notifyInGroup != 0
	link.ChatOnly = chatOnly != 0
	return &link, nil
}

func scanChannelLinks(rows *sql.Rows) ([]*ChannelLink, error) {
	var links []*ChannelLink
	for rows.Next() {
		var link ChannelLink
		var linkedAt string
		var active, showA2A, showAssistantReply, showStateChanges, notifyInGroup, chatOnly int
		err := rows.Scan(&link.ChannelID, &link.GuildID, &link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A,
			&showAssistantReply, &showStateChanges, &notifyInGroup, &chatOnly)
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
		link.NotifyInGroup = notifyInGroup != 0
		link.ChatOnly = chatOnly != 0
		links = append(links, &link)
	}
	return links, rows.Err()
}

func scanUserMapping(row *sql.Row) (*DiscordUserMapping, error) {
	var m DiscordUserMapping
	var linkedAt string
	err := row.Scan(&m.DiscordUserID, &m.DiscordUsername, &m.ScionUserID, &m.ScionEmail, &linkedAt)
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
