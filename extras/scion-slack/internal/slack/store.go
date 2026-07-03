package slack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store defines the persistence interface for the Slack broker plugin.
type Store interface {
	// Channel links (Slack channel <-> Scion project)
	CreateChannelLink(ctx context.Context, link *ChannelLink) error
	GetChannelLink(ctx context.Context, channelID string) (*ChannelLink, error)
	GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error)
	GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error)
	UpdateChannelLink(ctx context.Context, link *ChannelLink) error
	DeleteChannelLink(ctx context.Context, channelID string) error

	// User mappings (Slack user <-> Scion identity)
	CreateUserMapping(ctx context.Context, mapping *SlackUserMapping) error
	GetUserMapping(ctx context.Context, slackUserID string) (*SlackUserMapping, error)
	GetUserMappingByEmail(ctx context.Context, email string) (*SlackUserMapping, error)
	DeleteUserMapping(ctx context.Context, slackUserID string) error

	// Conversation context
	SetConversationContext(ctx context.Context, cc *ConversationContext) error
	GetConversationContext(ctx context.Context, slackUserID, projectID, agentSlug string) (*ConversationContext, error)
	GetLatestConversationContext(ctx context.Context, slackUserID, projectID string) (*ConversationContext, error)

	// Agent cache
	SetProjectAgents(ctx context.Context, pa *ProjectAgents) error
	GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error)

	// Pending ask-user requests
	CreatePendingAskUser(ctx context.Context, req *PendingAskUser) error
	GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error)
	MarkAskUserResponded(ctx context.Context, requestID string) error

	// Lifecycle
	Close() error
}

// ChannelLink represents a Slack channel linked to a Scion project.
type ChannelLink struct {
	ChannelID        string
	TeamID           string
	ProjectID        string
	ProjectSlug      string
	DefaultAgent     string
	LinkedBy         string
	LinkedAt         time.Time
	Active           bool
	ShowAgentToAgent bool
	ShowStateChanges bool
}

// SlackUserMapping links a Slack user to a Scion user identity.
type SlackUserMapping struct {
	SlackUserID   string
	SlackUsername string
	ScionUserID   string
	ScionEmail    string
	LinkedAt      time.Time
}

// ConversationContext tracks the last chat context for a user+project+agent tuple.
type ConversationContext struct {
	SlackUserID   string
	ProjectID     string
	AgentSlug     string
	LastChannelID string
	LastThreadTS  string
	LastMessageAt time.Time
}

// ProjectAgents caches the list of agents for a project.
type ProjectAgents struct {
	ProjectID   string
	AgentSlugs  []string
	RefreshedAt time.Time
}

// PendingAskUser represents an ask-user callback awaiting a Slack user response.
type PendingAskUser struct {
	RequestID string
	MessageTS string
	ChannelID string
	AgentSlug string
	ProjectID string
	Choices   []string
	ExpiresAt time.Time
	Responded bool
}

// sqliteStore implements Store using SQLite via modernc.org/sqlite.
type sqliteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database and initialises the schema.
func NewSQLiteStore(dbPath string) (Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	db.SetMaxOpenConns(1)

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
	team_id TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL,
	project_slug TEXT NOT NULL DEFAULT '',
	default_agent TEXT NOT NULL DEFAULT '',
	linked_by TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	active INTEGER NOT NULL DEFAULT 1,
	show_agent_to_agent INTEGER NOT NULL DEFAULT 0,
	show_state_changes INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_channel_links_project ON channel_links(project_id);

CREATE TABLE IF NOT EXISTS user_mappings (
	slack_user_id TEXT PRIMARY KEY,
	slack_username TEXT NOT NULL DEFAULT '',
	scion_user_id TEXT NOT NULL DEFAULT '',
	scion_email TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_mappings_email ON user_mappings(scion_email);

CREATE TABLE IF NOT EXISTS conversation_context (
	slack_user_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL,
	last_channel_id TEXT NOT NULL,
	last_thread_ts TEXT NOT NULL DEFAULT '',
	last_message_at TEXT NOT NULL,
	PRIMARY KEY (slack_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS project_agents (
	project_id TEXT PRIMARY KEY,
	agent_slugs TEXT NOT NULL DEFAULT '[]',
	refreshed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_ask_users (
	request_id TEXT PRIMARY KEY,
	message_ts TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL,
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices TEXT NOT NULL DEFAULT '[]',
	expires_at TEXT NOT NULL,
	responded INTEGER NOT NULL DEFAULT 0
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

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// --- ChannelLink CRUD ---

func (s *sqliteStore) CreateChannelLink(ctx context.Context, link *ChannelLink) error {
	const q = `
INSERT INTO channel_links (channel_id, team_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_state_changes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(channel_id) DO UPDATE SET
	team_id=excluded.team_id, project_id=excluded.project_id, project_slug=excluded.project_slug,
	default_agent=excluded.default_agent, linked_by=excluded.linked_by, linked_at=excluded.linked_at,
	active=excluded.active, show_agent_to_agent=excluded.show_agent_to_agent,
	show_state_changes=excluded.show_state_changes`
	_, err := s.db.ExecContext(ctx, q,
		link.ChannelID, link.TeamID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowStateChanges))
	return err
}

func (s *sqliteStore) GetChannelLink(ctx context.Context, channelID string) (*ChannelLink, error) {
	const q = `SELECT channel_id, team_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_state_changes FROM channel_links WHERE channel_id = ?`
	row := s.db.QueryRowContext(ctx, q, channelID)
	return scanChannelLink(row)
}

func (s *sqliteStore) GetChannelLinksForProject(ctx context.Context, projectID string) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, team_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_state_changes FROM channel_links WHERE project_id = ?`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChannelLinks(rows)
}

func (s *sqliteStore) GetAllChannelLinks(ctx context.Context) ([]*ChannelLink, error) {
	const q = `SELECT channel_id, team_id, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, show_state_changes FROM channel_links`
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
	team_id=?, project_id=?, project_slug=?, default_agent=?, linked_by=?, linked_at=?,
	active=?, show_agent_to_agent=?, show_state_changes=?
WHERE channel_id=?`
	_, err := s.db.ExecContext(ctx, q,
		link.TeamID, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent),
		boolToInt(link.ShowStateChanges),
		link.ChannelID)
	return err
}

func (s *sqliteStore) DeleteChannelLink(ctx context.Context, channelID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_links WHERE channel_id = ?`, channelID)
	return err
}

// --- User mappings ---

func (s *sqliteStore) CreateUserMapping(ctx context.Context, mapping *SlackUserMapping) error {
	const q = `
INSERT INTO user_mappings (slack_user_id, slack_username, scion_user_id, scion_email, linked_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(slack_user_id) DO UPDATE SET
	slack_username=excluded.slack_username, scion_user_id=excluded.scion_user_id,
	scion_email=excluded.scion_email, linked_at=excluded.linked_at`
	_, err := s.db.ExecContext(ctx, q,
		mapping.SlackUserID, mapping.SlackUsername,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetUserMapping(ctx context.Context, slackUserID string) (*SlackUserMapping, error) {
	const q = `SELECT slack_user_id, slack_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE slack_user_id = ?`
	row := s.db.QueryRowContext(ctx, q, slackUserID)
	return scanUserMapping(row)
}

func (s *sqliteStore) GetUserMappingByEmail(ctx context.Context, email string) (*SlackUserMapping, error) {
	const q = `SELECT slack_user_id, slack_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE scion_email = ?`
	row := s.db.QueryRowContext(ctx, q, email)
	return scanUserMapping(row)
}

func (s *sqliteStore) DeleteUserMapping(ctx context.Context, slackUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mappings WHERE slack_user_id = ?`, slackUserID)
	return err
}

// --- ConversationContext ---

func (s *sqliteStore) SetConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO conversation_context (slack_user_id, project_id, agent_slug, last_channel_id, last_thread_ts, last_message_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(slack_user_id, project_id, agent_slug) DO UPDATE SET
	last_channel_id=excluded.last_channel_id, last_thread_ts=excluded.last_thread_ts, last_message_at=excluded.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.SlackUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastChannelID, cc.LastThreadTS, cc.LastMessageAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetConversationContext(ctx context.Context, slackUserID, projectID, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT slack_user_id, project_id, agent_slug, last_channel_id, last_thread_ts, last_message_at FROM conversation_context WHERE slack_user_id = ? AND project_id = ? AND agent_slug = ?`
	row := s.db.QueryRowContext(ctx, q, slackUserID, projectID, agentSlug)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.SlackUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &cc.LastThreadTS, &lastMessageAt)
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

func (s *sqliteStore) GetLatestConversationContext(ctx context.Context, slackUserID, projectID string) (*ConversationContext, error) {
	const q = `SELECT slack_user_id, project_id, agent_slug, last_channel_id, last_thread_ts, last_message_at
FROM conversation_context
WHERE slack_user_id = ? AND project_id = ?
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, slackUserID, projectID)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.SlackUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChannelID, &cc.LastThreadTS, &lastMessageAt)
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
INSERT INTO pending_ask_users (request_id, message_ts, channel_id, agent_slug, project_id, choices, expires_at, responded)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
	message_ts=excluded.message_ts, channel_id=excluded.channel_id, agent_slug=excluded.agent_slug,
	project_id=excluded.project_id, choices=excluded.choices, expires_at=excluded.expires_at,
	responded=excluded.responded`
	_, err = s.db.ExecContext(ctx, q,
		req.RequestID, req.MessageTS, req.ChannelID,
		req.AgentSlug, req.ProjectID, string(choicesJSON),
		req.ExpiresAt.UTC().Format(time.RFC3339), boolToInt(req.Responded))
	return err
}

func (s *sqliteStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, message_ts, channel_id, agent_slug, project_id, choices, expires_at, responded FROM pending_ask_users WHERE request_id = ?`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON, expiresAt string
	var responded int
	err := row.Scan(&p.RequestID, &p.MessageTS, &p.ChannelID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &expiresAt, &responded)
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

// --- scan helpers ---

func scanChannelLink(row *sql.Row) (*ChannelLink, error) {
	var link ChannelLink
	var linkedAt string
	var active, showA2A, showStateChanges int
	err := row.Scan(&link.ChannelID, &link.TeamID, &link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A, &showStateChanges)
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
	link.ShowStateChanges = showStateChanges != 0
	return &link, nil
}

func scanChannelLinks(rows *sql.Rows) ([]*ChannelLink, error) {
	var links []*ChannelLink
	for rows.Next() {
		var link ChannelLink
		var linkedAt string
		var active, showA2A, showStateChanges int
		err := rows.Scan(&link.ChannelID, &link.TeamID, &link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A, &showStateChanges)
		if err != nil {
			return nil, err
		}
		link.LinkedAt, err = time.Parse(time.RFC3339, linkedAt)
		if err != nil {
			return nil, fmt.Errorf("parse linked_at: %w", err)
		}
		link.Active = active != 0
		link.ShowAgentToAgent = showA2A != 0
		link.ShowStateChanges = showStateChanges != 0
		links = append(links, &link)
	}
	return links, rows.Err()
}

func scanUserMapping(row *sql.Row) (*SlackUserMapping, error) {
	var m SlackUserMapping
	var linkedAt string
	err := row.Scan(&m.SlackUserID, &m.SlackUsername, &m.ScionUserID, &m.ScionEmail, &linkedAt)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
