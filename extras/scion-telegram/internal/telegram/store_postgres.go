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

	"github.com/GoogleCloudPlatform/scion/pkg/integration/lockloop"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type postgresStore struct {
	db *sql.DB
}

// NewPostgresStore opens a Postgres database at the given URL, creates the
// schema if needed, and returns a Store backed by Postgres. The caller must
// call Close when done.
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
CREATE TABLE IF NOT EXISTS telegram_group_links (
	chat_id            BIGINT PRIMARY KEY,
	chat_title         TEXT NOT NULL DEFAULT '',
	project_id         TEXT NOT NULL,
	project_slug       TEXT NOT NULL DEFAULT '',
	default_agent      TEXT NOT NULL DEFAULT '',
	linked_by          TEXT NOT NULL DEFAULT '',
	linked_at          TIMESTAMPTZ NOT NULL,
	active             BOOLEAN NOT NULL DEFAULT TRUE,
	show_agent_to_agent    BOOLEAN NOT NULL DEFAULT FALSE,
	notify_in_group        BOOLEAN NOT NULL DEFAULT FALSE,
	show_assistant_reply   BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS idx_telegram_group_links_project ON telegram_group_links(project_id);

CREATE TABLE IF NOT EXISTS telegram_conversation_context (
	telegram_user_id TEXT NOT NULL,
	project_id       TEXT NOT NULL,
	agent_slug       TEXT NOT NULL,
	last_chat_id     BIGINT NOT NULL,
	last_message_at  TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (telegram_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS telegram_project_agents (
	project_id   TEXT PRIMARY KEY,
	agent_slugs  TEXT NOT NULL DEFAULT '[]',
	refreshed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS telegram_user_mappings (
	telegram_user_id   TEXT PRIMARY KEY,
	telegram_username  TEXT NOT NULL DEFAULT '',
	scion_user_id      TEXT NOT NULL DEFAULT '',
	scion_email        TEXT NOT NULL DEFAULT '',
	linked_at          TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_telegram_user_mappings_email ON telegram_user_mappings(scion_email);
CREATE INDEX IF NOT EXISTS idx_telegram_user_mappings_username ON telegram_user_mappings(telegram_username);

CREATE TABLE IF NOT EXISTS telegram_pending_ask_users (
	request_id TEXT PRIMARY KEY,
	message_id BIGINT NOT NULL,
	chat_id    BIGINT NOT NULL,
	agent_slug TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	choices    TEXT NOT NULL DEFAULT '[]',
	expires_at TIMESTAMPTZ NOT NULL,
	responded  BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS telegram_callback_lookups (
	short_id   TEXT PRIMARY KEY,
	full_data  TEXT NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS telegram_notification_prefs (
	telegram_user_id TEXT NOT NULL,
	project_id       TEXT NOT NULL,
	agent_slug       TEXT NOT NULL,
	enabled          BOOLEAN NOT NULL DEFAULT TRUE,
	PRIMARY KEY (telegram_user_id, project_id, agent_slug)
);

CREATE TABLE IF NOT EXISTS telegram_topic_defaults (
	chat_id    BIGINT NOT NULL,
	thread_id  BIGINT NOT NULL,
	agent_slug TEXT NOT NULL,
	PRIMARY KEY (chat_id, thread_id)
);

CREATE TABLE IF NOT EXISTS telegram_processed_updates (
	update_id  BIGINT PRIMARY KEY,
	processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
	_, err := s.db.Exec(ddl)
	return err
}

func (s *postgresStore) Close() error {
	return s.db.Close()
}

// --- GroupLink CRUD ---

func (s *postgresStore) SaveGroupLink(ctx context.Context, link *GroupLink) error {
	const q = `
INSERT INTO telegram_group_links (chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT(chat_id) DO UPDATE SET
	chat_title=EXCLUDED.chat_title, project_id=EXCLUDED.project_id, project_slug=EXCLUDED.project_slug,
	default_agent=EXCLUDED.default_agent, linked_by=EXCLUDED.linked_by, linked_at=EXCLUDED.linked_at,
	active=EXCLUDED.active, show_agent_to_agent=EXCLUDED.show_agent_to_agent,
	notify_in_group=EXCLUDED.notify_in_group, show_assistant_reply=EXCLUDED.show_assistant_reply`
	_, err := s.db.ExecContext(ctx, q,
		link.ChatID, link.ChatTitle, link.ProjectID, link.ProjectSlug,
		link.DefaultAgent, link.LinkedBy, link.LinkedAt.UTC(),
		link.Active, link.ShowAgentToAgent, link.NotifyInGroup, link.ShowAssistantReply)
	return err
}

func (s *postgresStore) GetGroupLink(ctx context.Context, chatID int64) (*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply FROM telegram_group_links WHERE chat_id = $1`
	row := s.db.QueryRowContext(ctx, q, chatID)
	return pgScanGroupLink(row)
}

func (s *postgresStore) GetGroupLinksForProject(ctx context.Context, projectID string) ([]*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply FROM telegram_group_links WHERE project_id = $1`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanGroupLinks(rows)
}

func (s *postgresStore) GetAllGroupLinks(ctx context.Context) ([]*GroupLink, error) {
	const q = `SELECT chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply FROM telegram_group_links`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgScanGroupLinks(rows)
}

func (s *postgresStore) DeleteGroupLink(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_group_links WHERE chat_id = $1`, chatID)
	return err
}

func (s *postgresStore) MigrateGroupLink(ctx context.Context, oldChatID, newChatID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
INSERT INTO telegram_group_links
  (chat_id, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply)
SELECT $1, chat_title, project_id, project_slug, default_agent, linked_by, linked_at, active, show_agent_to_agent, notify_in_group, show_assistant_reply
FROM telegram_group_links WHERE chat_id = $2
ON CONFLICT(chat_id) DO UPDATE SET
	chat_title=EXCLUDED.chat_title, project_id=EXCLUDED.project_id, project_slug=EXCLUDED.project_slug,
	default_agent=EXCLUDED.default_agent, linked_by=EXCLUDED.linked_by, linked_at=EXCLUDED.linked_at,
	active=EXCLUDED.active, show_agent_to_agent=EXCLUDED.show_agent_to_agent,
	notify_in_group=EXCLUDED.notify_in_group, show_assistant_reply=EXCLUDED.show_assistant_reply`, newChatID, oldChatID)
	if err != nil {
		return fmt.Errorf("copy group_link: %w", err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM telegram_group_links WHERE chat_id = $1`, oldChatID)
	if err != nil {
		return fmt.Errorf("delete old group_link: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO telegram_topic_defaults (chat_id, thread_id, agent_slug)
SELECT $1, thread_id, agent_slug FROM telegram_topic_defaults WHERE chat_id = $2
ON CONFLICT(chat_id, thread_id) DO UPDATE SET agent_slug=EXCLUDED.agent_slug`, newChatID, oldChatID)
	if err != nil {
		return fmt.Errorf("copy topic_defaults: %w", err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM telegram_topic_defaults WHERE chat_id = $1`, oldChatID)
	if err != nil {
		return fmt.Errorf("delete old topic_defaults: %w", err)
	}

	return tx.Commit()
}

// --- ConversationContext ---

func (s *postgresStore) SaveConversationContext(ctx context.Context, cc *ConversationContext) error {
	const q = `
INSERT INTO telegram_conversation_context (telegram_user_id, project_id, agent_slug, last_chat_id, last_message_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(telegram_user_id, project_id, agent_slug) DO UPDATE SET
	last_chat_id=EXCLUDED.last_chat_id, last_message_at=EXCLUDED.last_message_at`
	_, err := s.db.ExecContext(ctx, q,
		cc.TelegramUserID, cc.ProjectID, cc.AgentSlug,
		cc.LastChatID, cc.LastMessageAt.UTC())
	return err
}

func (s *postgresStore) GetConversationContext(ctx context.Context, telegramUserID string, projectID string, agentSlug string) (*ConversationContext, error) {
	const q = `SELECT telegram_user_id, project_id, agent_slug, last_chat_id, last_message_at FROM telegram_conversation_context WHERE telegram_user_id = $1 AND project_id = $2 AND agent_slug = $3`
	row := s.db.QueryRowContext(ctx, q, telegramUserID, projectID, agentSlug)

	var cc ConversationContext
	err := row.Scan(&cc.TelegramUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChatID, &cc.LastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

func (s *postgresStore) GetLatestConversationContext(ctx context.Context, telegramUserID string, projectID string) (*ConversationContext, error) {
	const q = `SELECT telegram_user_id, project_id, agent_slug, last_chat_id, last_message_at
FROM telegram_conversation_context
WHERE telegram_user_id = $1 AND project_id = $2
ORDER BY last_message_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, telegramUserID, projectID)

	var cc ConversationContext
	err := row.Scan(&cc.TelegramUserID, &cc.ProjectID, &cc.AgentSlug, &cc.LastChatID, &cc.LastMessageAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

// --- ProjectAgents ---

func (s *postgresStore) SaveProjectAgents(ctx context.Context, pa *ProjectAgents) error {
	slugsJSON, err := json.Marshal(pa.Agents)
	if err != nil {
		return fmt.Errorf("marshal agent_slugs: %w", err)
	}
	const q = `
INSERT INTO telegram_project_agents (project_id, agent_slugs, refreshed_at)
VALUES ($1, $2, $3)
ON CONFLICT(project_id) DO UPDATE SET
	agent_slugs=EXCLUDED.agent_slugs, refreshed_at=EXCLUDED.refreshed_at`
	_, err = s.db.ExecContext(ctx, q, pa.ProjectID, string(slugsJSON), pa.RefreshedAt.UTC())
	return err
}

func (s *postgresStore) GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error) {
	const q = `SELECT project_id, agent_slugs, refreshed_at FROM telegram_project_agents WHERE project_id = $1`
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
	if err := json.Unmarshal([]byte(slugsJSON), &pa.Agents); err != nil {
		return nil, fmt.Errorf("unmarshal agent_slugs: %w", err)
	}
	return &pa, nil
}

// --- User mappings ---

func (s *postgresStore) SaveUserMapping(ctx context.Context, mapping *TelegramUserMapping) error {
	const q = `
INSERT INTO telegram_user_mappings (telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(telegram_user_id) DO UPDATE SET
	telegram_username=EXCLUDED.telegram_username, scion_user_id=EXCLUDED.scion_user_id,
	scion_email=EXCLUDED.scion_email, linked_at=EXCLUDED.linked_at`
	_, err := s.db.ExecContext(ctx, q,
		mapping.TelegramUserID, mapping.TelegramUsername,
		mapping.ScionUserID, mapping.ScionEmail,
		mapping.LinkedAt.UTC())
	return err
}

func (s *postgresStore) GetUserMapping(ctx context.Context, telegramUserID string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM telegram_user_mappings WHERE telegram_user_id = $1`
	row := s.db.QueryRowContext(ctx, q, telegramUserID)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByEmail(ctx context.Context, email string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM telegram_user_mappings WHERE scion_email = $1`
	row := s.db.QueryRowContext(ctx, q, email)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByUsername(ctx context.Context, username string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM telegram_user_mappings WHERE LOWER(telegram_username) = LOWER($1)`
	row := s.db.QueryRowContext(ctx, q, username)
	return pgScanUserMapping(row)
}

func (s *postgresStore) GetUserMappingByScionUserID(ctx context.Context, userID string) (*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM telegram_user_mappings WHERE scion_user_id = $1`
	row := s.db.QueryRowContext(ctx, q, userID)
	return pgScanUserMapping(row)
}

func (s *postgresStore) DeleteUserMapping(ctx context.Context, telegramUserID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_user_mappings WHERE telegram_user_id = $1`, telegramUserID)
	return err
}

func (s *postgresStore) GetAllUserMappings(ctx context.Context) ([]*TelegramUserMapping, error) {
	const q = `SELECT telegram_user_id, telegram_username, scion_user_id, scion_email, linked_at FROM telegram_user_mappings`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []*TelegramUserMapping
	for rows.Next() {
		var m TelegramUserMapping
		if err := rows.Scan(&m.TelegramUserID, &m.TelegramUsername, &m.ScionUserID, &m.ScionEmail, &m.LinkedAt); err != nil {
			return nil, err
		}
		mappings = append(mappings, &m)
	}
	return mappings, rows.Err()
}

// --- PendingAskUser ---

func (s *postgresStore) SavePendingAskUser(ctx context.Context, pending *PendingAskUser) error {
	choicesJSON, err := json.Marshal(pending.Choices)
	if err != nil {
		return fmt.Errorf("marshal choices: %w", err)
	}
	const q = `
INSERT INTO telegram_pending_ask_users (request_id, message_id, chat_id, agent_slug, project_id, choices, expires_at, responded)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT(request_id) DO UPDATE SET
	message_id=EXCLUDED.message_id, chat_id=EXCLUDED.chat_id, agent_slug=EXCLUDED.agent_slug,
	project_id=EXCLUDED.project_id, choices=EXCLUDED.choices, expires_at=EXCLUDED.expires_at,
	responded=EXCLUDED.responded`
	_, err = s.db.ExecContext(ctx, q,
		pending.RequestID, pending.MessageID, pending.ChatID,
		pending.AgentSlug, pending.ProjectID, string(choicesJSON),
		pending.ExpiresAt.UTC(), pending.Responded)
	return err
}

func (s *postgresStore) GetPendingAskUser(ctx context.Context, requestID string) (*PendingAskUser, error) {
	const q = `SELECT request_id, message_id, chat_id, agent_slug, project_id, choices, expires_at, responded FROM telegram_pending_ask_users WHERE request_id = $1`
	row := s.db.QueryRowContext(ctx, q, requestID)

	var p PendingAskUser
	var choicesJSON string
	err := row.Scan(&p.RequestID, &p.MessageID, &p.ChatID, &p.AgentSlug, &p.ProjectID, &choicesJSON, &p.ExpiresAt, &p.Responded)
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

func (s *postgresStore) MarkPendingAskUserResponded(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE telegram_pending_ask_users SET responded = TRUE WHERE request_id = $1`, requestID)
	return err
}

func (s *postgresStore) CleanExpiredPendingAskUsers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_pending_ask_users WHERE expires_at < NOW()`)
	return err
}

// --- CallbackLookup ---

func (s *postgresStore) SaveCallbackLookup(ctx context.Context, lookup *CallbackLookup) error {
	const q = `
INSERT INTO telegram_callback_lookups (short_id, full_data, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT(short_id) DO UPDATE SET
	full_data=EXCLUDED.full_data, expires_at=EXCLUDED.expires_at`
	_, err := s.db.ExecContext(ctx, q,
		lookup.ShortID, lookup.FullData,
		lookup.ExpiresAt.UTC())
	return err
}

func (s *postgresStore) GetCallbackLookup(ctx context.Context, shortID string) (*CallbackLookup, error) {
	const q = `SELECT short_id, full_data, expires_at FROM telegram_callback_lookups WHERE short_id = $1`
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

func (s *postgresStore) CleanExpiredCallbackLookups(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_callback_lookups WHERE expires_at < NOW()`)
	return err
}

// --- NotificationPref ---

func (s *postgresStore) SaveNotificationPref(ctx context.Context, pref *NotificationPref) error {
	const q = `
INSERT INTO telegram_notification_prefs (telegram_user_id, project_id, agent_slug, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT(telegram_user_id, project_id, agent_slug) DO UPDATE SET
	enabled=EXCLUDED.enabled`
	_, err := s.db.ExecContext(ctx, q,
		pref.TelegramUserID, pref.ProjectID, pref.AgentSlug, pref.Enabled)
	return err
}

func (s *postgresStore) GetNotificationPrefs(ctx context.Context, telegramUserID string) ([]*NotificationPref, error) {
	const q = `SELECT telegram_user_id, project_id, agent_slug, enabled FROM telegram_notification_prefs WHERE telegram_user_id = $1`
	rows, err := s.db.QueryContext(ctx, q, telegramUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prefs []*NotificationPref
	for rows.Next() {
		var p NotificationPref
		if err := rows.Scan(&p.TelegramUserID, &p.ProjectID, &p.AgentSlug, &p.Enabled); err != nil {
			return nil, err
		}
		prefs = append(prefs, &p)
	}
	return prefs, rows.Err()
}

func (s *postgresStore) GetNotificationPref(ctx context.Context, telegramUserID, projectID, agentSlug string) (*NotificationPref, error) {
	const q = `SELECT telegram_user_id, project_id, agent_slug, enabled FROM telegram_notification_prefs WHERE telegram_user_id = $1 AND project_id = $2 AND agent_slug = $3`
	row := s.db.QueryRowContext(ctx, q, telegramUserID, projectID, agentSlug)

	var p NotificationPref
	err := row.Scan(&p.TelegramUserID, &p.ProjectID, &p.AgentSlug, &p.Enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// --- TopicDefault ---

func (s *postgresStore) GetTopicDefault(ctx context.Context, chatID int64, threadID int64) (string, error) {
	const q = `SELECT agent_slug FROM telegram_topic_defaults WHERE chat_id = $1 AND thread_id = $2`
	var agentSlug string
	err := s.db.QueryRowContext(ctx, q, chatID, threadID).Scan(&agentSlug)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return agentSlug, err
}

func (s *postgresStore) SetTopicDefault(ctx context.Context, chatID int64, threadID int64, agentSlug string) error {
	const q = `
INSERT INTO telegram_topic_defaults (chat_id, thread_id, agent_slug)
VALUES ($1, $2, $3)
ON CONFLICT(chat_id, thread_id) DO UPDATE SET agent_slug=EXCLUDED.agent_slug`
	_, err := s.db.ExecContext(ctx, q, chatID, threadID, agentSlug)
	return err
}

func (s *postgresStore) DeleteTopicDefault(ctx context.Context, chatID int64, threadID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_topic_defaults WHERE chat_id = $1 AND thread_id = $2`, chatID, threadID)
	return err
}

// --- Update dedup (F7) ---

// TryMarkUpdateProcessed attempts to insert an update_id into the dedup table.
// Returns true if the insert succeeded (first time seeing this update_id),
// false if a row already exists (duplicate delivery).
func (s *postgresStore) TryMarkUpdateProcessed(ctx context.Context, updateID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO telegram_processed_updates (update_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		updateID)
	if err != nil {
		return false, fmt.Errorf("insert update_id: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PruneProcessedUpdates removes entries older than the given duration.
func (s *postgresStore) PruneProcessedUpdates(ctx context.Context, olderThan int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM telegram_processed_updates WHERE processed_at < NOW() - ($1 || ' hours')::INTERVAL`,
		olderThan)
	return err
}

// --- Advisory lock support ---

// AdvisoryLocker is the shared lockloop interface for advisory locks.
type AdvisoryLocker = lockloop.AdvisoryLocker

// TryAdvisoryLock attempts to acquire a Postgres session-level advisory lock
// on a dedicated connection. Returns a shared lockloop.AdvisoryLockHandle.
func (s *postgresStore) TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, handle *lockloop.AdvisoryLockHandle, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("acquire connection: %w", err)
	}

	var ok bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		conn.Close()
		return false, nil, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}

	if !ok {
		conn.Close()
		return false, nil, nil
	}

	return true, lockloop.NewAdvisoryLockHandle(
		func() error {
			_, unlockErr := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
			closeErr := conn.Close()
			if unlockErr != nil {
				return fmt.Errorf("advisory unlock: %w", unlockErr)
			}
			return closeErr
		},
		func(verifyCtx context.Context) error {
			return conn.PingContext(verifyCtx)
		},
	), nil
}

// --- scan helpers ---

func pgScanGroupLink(row *sql.Row) (*GroupLink, error) {
	var link GroupLink
	err := row.Scan(&link.ChatID, &link.ChatTitle, &link.ProjectID, &link.ProjectSlug,
		&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
		&link.NotifyInGroup, &link.ShowAssistantReply)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func pgScanGroupLinks(rows *sql.Rows) ([]*GroupLink, error) {
	var links []*GroupLink
	for rows.Next() {
		var link GroupLink
		err := rows.Scan(&link.ChatID, &link.ChatTitle, &link.ProjectID, &link.ProjectSlug,
			&link.DefaultAgent, &link.LinkedBy, &link.LinkedAt, &link.Active, &link.ShowAgentToAgent,
			&link.NotifyInGroup, &link.ShowAssistantReply)
		if err != nil {
			return nil, err
		}
		links = append(links, &link)
	}
	return links, rows.Err()
}

func pgScanUserMapping(row *sql.Row) (*TelegramUserMapping, error) {
	var m TelegramUserMapping
	err := row.Scan(&m.TelegramUserID, &m.TelegramUsername, &m.ScionUserID, &m.ScionEmail, &m.LinkedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
