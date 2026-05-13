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

package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store defines the persistence interface for the Telegram v2 plugin.
type Store interface {
	// GroupLink CRUD
	SaveGroupLink(ctx context.Context, link *GroupLink) error
	GetGroupLink(ctx context.Context, chatID int64) (*GroupLink, error)
	GetGroupLinksForProject(ctx context.Context, projectID string) ([]*GroupLink, error)
	GetAllGroupLinks(ctx context.Context) ([]*GroupLink, error)
	DeleteGroupLink(ctx context.Context, chatID int64) error

	// ConversationContext
	SaveConversationContext(ctx context.Context, cc *ConversationContext) error
	GetConversationContext(ctx context.Context, telegramUserID string, projectID string, agentSlug string) (*ConversationContext, error)

	// ProjectAgents cache
	SaveProjectAgents(ctx context.Context, pa *ProjectAgents) error
	GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error)

	// User mappings
	SaveUserMapping(ctx context.Context, mapping *TelegramUserMapping) error
	GetUserMapping(ctx context.Context, telegramUserID string) (*TelegramUserMapping, error)
	GetUserMappingByEmail(ctx context.Context, email string) (*TelegramUserMapping, error)
	DeleteUserMapping(ctx context.Context, telegramUserID string) error
	GetAllUserMappings(ctx context.Context) ([]*TelegramUserMapping, error)

	// PendingAskUser
	SavePendingAskUser(ctx context.Context, pending *PendingAskUser) error
	GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error)
	MarkPendingAskUserResponded(ctx context.Context, requestID string) error
	CleanExpiredPendingAskUsers(ctx context.Context) error

	// CallbackLookup
	SaveCallbackLookup(ctx context.Context, lookup *CallbackLookup) error
	GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error)
	CleanExpiredCallbackLookups(ctx context.Context) error

	// Lifecycle
	Close() error
}

// GroupLink represents a Telegram group chat linked to a Scion project.
type GroupLink struct {
	ChatID           int64
	ChatTitle        string
	ProjectID        string
	ProjectSlug      string
	DefaultAgent     string
	LinkedBy         string
	LinkedAt         time.Time
	Active           bool
	ShowAgentToAgent bool
}

// ConversationContext tracks the last chat context for a user+project+agent tuple.
type ConversationContext struct {
	TelegramUserID string
	ProjectID      string
	AgentSlug      string
	LastChatID     int64
	LastMessageAt  time.Time
}

// ProjectAgents caches the list of agent slugs for a project.
type ProjectAgents struct {
	ProjectID   string
	AgentSlugs  []string
	RefreshedAt time.Time
}

// TelegramUserMapping links a Telegram user to a Scion user identity.
type TelegramUserMapping struct {
	TelegramUserID   string
	TelegramUsername string
	ScionUserID      string
	ScionEmail       string
	LinkedAt         time.Time
}

// PendingAskUser represents an ask-user callback awaiting a Telegram user response.
type PendingAskUser struct {
	RequestID string
	MessageID int64
	ChatID    int64
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

	s := &sqliteStore{db: db}
	if err := s.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return s, nil
}

func (s *sqliteStore) createTables() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS group_links (
	chat_id            INTEGER PRIMARY KEY,
	chat_title         TEXT NOT NULL DEFAULT '',
	project_id         TEXT NOT NULL,
	project_slug       TEXT NOT NULL DEFAULT '',
	default_agent      TEXT NOT NULL DEFAULT '',
	linked_by          TEXT NOT NULL DEFAULT '',
	linked_at          TEXT NOT NULL,
	active             INTEGER NOT NULL DEFAULT 1,
	show_agent_to_agent INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_group_links_project ON group_links(project_id);

CREATE TABLE IF NOT EXISTS conversation_contexts (
	telegram_user_id TEXT NOT NULL,
	project_id       TEXT NOT NULL,
	agent_slug       TEXT NOT NULL,
	last_chat_id     INTEGER NOT NULL,
	last_message_at  TEXT NOT NULL,
	PRIMARY KEY (telegram_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS project_agents (
	project_id   TEXT PRIMARY KEY,
	agent_slugs  TEXT NOT NULL,
	refreshed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_mappings (
	telegram_user_id   TEXT PRIMARY KEY,
	telegram_username  TEXT NOT NULL DEFAULT '',
	scion_user_id      TEXT NOT NULL DEFAULT '',
	scion_email        TEXT NOT NULL DEFAULT '',
	linked_at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_mappings_email ON user_mappings(scion_email);

CREATE TABLE IF NOT EXISTS pending_ask_users (
	request_id TEXT PRIMARY KEY,
	message_id INTEGER NOT NULL,
	chat_id    INTEGER NOT NULL,
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices    TEXT NOT NULL DEFAULT '[]',
	expires_at TEXT NOT NULL,
	responded  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS callback_lookups (
	short_id   TEXT PRIMARY KEY,
	full_data  TEXT NOT NULL,
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

// --- GroupLink CRUD ---

func (s *sqliteStore) SaveGroupLink(ctx context.Context, link *GroupLink) error {
	const q = `
INSERT INTO group_links (chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_id) DO UPDATE SET
	chat_title=excluded.chat_title, project_id=excluded.project_id, project_slug=excluded.project_slug,
	default_agent=excluded.default_agent, linked_by=excluded.linked_by, linked_at=excluded.linked_at,
	active=excluded.active, show_agent_to_agent=excluded.show_agent_to_agent`
	_, err := s.db.ExecContext(ctx, q,
		link.ChatID, link.ChatTitle, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC().Format(time.RFC3339),
		boolToInt(link.Active), boolToInt(link.ShowAgentToAgent))
	return err
}

func (s *sqliteStore) GetGroupLink(ctx context.Context, chatID int64) (*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent FROM group_links WHERE chat_id = ?`
	row := s.db.QueryRowContext(ctx, q, chatID)
	return scanGroupLink(row)
}

func (s *sqliteStore) GetGroupLinksForProject(ctx context.Context, projectID string) ([]*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent FROM group_links WHERE project_id = ?`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGroupLinks(rows)
}

func (s *sqliteStore) GetAllGroupLinks(ctx context.Context) ([]*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent FROM group_links`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGroupLinks(rows)
}

func (s *sqliteStore) DeleteGroupLink(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM group_links WHERE chat_id = ?`, chatID)
	return err
}

// --- ConversationContext ---

func (s *sqliteStore) SaveConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO conversation_contexts (telegram_user_id, project_id, agent_slug, last_chat_id, last_message_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(telegram_user_id, project_id, agent_slug) DO UPDATE SET
	last_chat_id=excluded.last_chat_id, last_message_at=excluded.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.TelegramUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastChatID, cc.LastMessageAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetConversationContext(ctx context.Context, telegramUserID string, projectID string, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT telegram_user_id, project_id, agent_slug, last_chat_id, last_message_at FROM conversation_contexts WHERE telegram_user_id = ? AND project_id = ? AND agent_slug = ?`
	row := s.db.QueryRowContext(ctx, q, telegramUserID, projectID, agentSlug)

	var cc ConversationContext
	var lastMessageAt string
	err := row.Scan(&cc.TelegramUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChatID, &lastMessageAt)
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

func (s *sqliteStore) SaveProjectAgents(ctx context.Context, pa *ProjectAgents) error {
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

// --- User mappings ---

func (s *sqliteStore) SaveUserMapping(ctx context.Context, mapping *TelegramUserMapping) error {
	const q = `
INSERT INTO user_mappings (telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(telegram_user_id) DO UPDATE SET
	telegram_username=excluded.telegram_username, scion_user_id=excluded.scion_user_id,
	scion_email=excluded.scion_email, linked_at=excluded.linked_at`
	_, err := s.db.ExecContext(ctx, q,
		mapping.TelegramUserID, mapping.TelegramUsername,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *sqliteStore) GetUserMapping(ctx context.Context, telegramUserID string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE telegram_user_id = ?`
	row := s.db.QueryRowContext(ctx, q, telegramUserID)
	return scanUserMapping(row)
}

func (s *sqliteStore) GetUserMappingByEmail(ctx context.Context, email string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM user_mappings WHERE scion_email = ?`
	row := s.db.QueryRowContext(ctx, q, email)
	return scanUserMapping(row)
}

func (s *sqliteStore) DeleteUserMapping(ctx context.Context, telegramUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mappings WHERE telegram_user_id = ?`, telegramUserID)
	return err
}

func (s *sqliteStore) GetAllUserMappings(ctx context.Context) ([]*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM user_mappings`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []*TelegramUserMapping
	for rows.Next() {
		m, err := scanUserMappingRow(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

// --- PendingAskUser ---

func (s *sqliteStore) SavePendingAskUser(ctx context.Context, pending *PendingAskUser) error {
	choicesJSON, err := json.Marshal(pending.Choices)
	if err != nil {
		return fmt.Errorf("marshal choices: %w", err)
	}
	const q = `
INSERT INTO pending_ask_users (request_id, message_id, chat_id, agent_slug, project_id, choices, expires_at, responded)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
	message_id=excluded.message_id, chat_id=excluded.chat_id, agent_slug=excluded.agent_slug,
	project_id=excluded.project_id, choices=excluded.choices, expires_at=excluded.expires_at,
	responded=excluded.responded`
	_, err = s.db.ExecContext(ctx, q,
		pending.RequestID, pending.MessageID, pending.ChatID,
		pending.AgentSlug, pending.ProjectID, string(choicesJSON),
		pending.ExpiresAt.UTC().Format(time.RFC3339), boolToInt(pending.Responded))
	return err
}

func (s *sqliteStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, message_id, chat_id, agent_slug, project_id, choices, expires_at, responded FROM pending_ask_users WHERE request_id = ?`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON, expiresAt string
	var responded int
	err := row.Scan(&p.RequestID, &p.MessageID, &p.ChatID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &expiresAt, &responded)
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

func (s *sqliteStore) MarkPendingAskUserResponded(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pending_ask_users SET responded = 1 WHERE request_id = ?`, requestID)
	return err
}

func (s *sqliteStore) CleanExpiredPendingAskUsers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_ask_users WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	return err
}

// --- CallbackLookup ---

func (s *sqliteStore) SaveCallbackLookup(ctx context.Context, lookup *CallbackLookup) error {
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

func (s *sqliteStore) CleanExpiredCallbackLookups(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM callback_lookups WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	return err
}

// --- scan helpers ---

func scanGroupLink(row *sql.Row) (*GroupLink, error) {
	var link GroupLink
	var linkedAt string
	var active, showA2A int
	err := row.Scan(&link.ChatID, &link.ChatTitle, &link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A)
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
	return &link, nil
}

func scanGroupLinks(rows *sql.Rows) ([]*GroupLink, error) {
	var links []*GroupLink
	for rows.Next() {
		var link GroupLink
		var linkedAt string
		var active, showA2A int
		err := rows.Scan(&link.ChatID, &link.ChatTitle, &link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &linkedAt, &active, &showA2A)
		if err != nil {
			return nil, err
		}
		link.LinkedAt, err = time.Parse(time.RFC3339, linkedAt)
		if err != nil {
			return nil, fmt.Errorf("parse linked_at: %w", err)
		}
		link.Active = active != 0
		link.ShowAgentToAgent = showA2A != 0
		links = append(links, &link)
	}
	return links, rows.Err()
}

func scanUserMapping(row *sql.Row) (*TelegramUserMapping, error) {
	var m TelegramUserMapping
	var linkedAt string
	err := row.Scan(&m.TelegramUserID, &m.TelegramUsername, &m.ScionUserID, &m.ScionEmail, &linkedAt)
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

func scanUserMappingRow(rows *sql.Rows) (*TelegramUserMapping, error) {
	var m TelegramUserMapping
	var linkedAt string
	err := rows.Scan(&m.TelegramUserID, &m.TelegramUsername, &m.ScionUserID, &m.ScionEmail, &linkedAt)
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
