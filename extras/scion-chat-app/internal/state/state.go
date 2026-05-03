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

package state

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// UserMapping associates a platform user with a hub user.
type UserMapping struct {
	PlatformUserID string
	Platform       string
	HubUserID      string
	HubUserEmail   string
	RegisteredAt   time.Time
	RegisteredBy   string // "auto" or "manual"
}

// SpaceLink associates a platform space/channel with a grove.
type SpaceLink struct {
	SpaceID      string
	Platform     string
	GroveID      string
	GroveSlug    string
	LinkedBy     string
	LinkedAt     time.Time
	DefaultAgent string
}

// AgentSubscription tracks a user's subscription to agent activity notifications.
type AgentSubscription struct {
	PlatformUserID string
	Platform       string
	AgentID        string
	GroveID        string
	Activities     string // Comma-separated; empty = all
	SubscribedAt   time.Time
}

// Store provides SQLite-backed local state management.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at dbPath and runs schema migrations.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS user_mappings (
			platform_user_id TEXT NOT NULL,
			platform         TEXT NOT NULL,
			hub_user_id      TEXT NOT NULL,
			hub_user_email   TEXT NOT NULL,
			registered_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			registered_by    TEXT NOT NULL DEFAULT 'auto',
			PRIMARY KEY (platform_user_id, platform)
		)`,
		`CREATE TABLE IF NOT EXISTS space_links (
			space_id    TEXT NOT NULL,
			platform    TEXT NOT NULL,
			grove_id    TEXT NOT NULL,
			grove_slug  TEXT NOT NULL,
			linked_by   TEXT NOT NULL,
			linked_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (space_id, platform)
		)`,
		`CREATE TABLE IF NOT EXISTS agent_subscriptions (
			platform_user_id TEXT NOT NULL,
			platform         TEXT NOT NULL,
			agent_id         TEXT NOT NULL,
			grove_id         TEXT NOT NULL,
			activities       TEXT NOT NULL DEFAULT '',
			subscribed_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (platform_user_id, platform, agent_id)
		)`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}

	// Additive column migrations (idempotent — ignore "duplicate column" errors).
	addColumns := []string{
		`ALTER TABLE space_links ADD COLUMN default_agent TEXT NOT NULL DEFAULT ''`,
	}
	for _, m := range addColumns {
		if _, err := s.db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("exec column migration: %w", err)
		}
	}

	return nil
}

// --- User Mappings ---

// GetUserMapping returns the user mapping for the given platform user, or nil, nil if not found.
func (s *Store) GetUserMapping(platformUserID, platform string) (*UserMapping, error) {
	m := &UserMapping{}
	err := s.db.QueryRow(
		`SELECT platform_user_id, platform, hub_user_id, hub_user_email, registered_at, registered_by
		 FROM user_mappings WHERE platform_user_id = ? AND platform = ?`,
		platformUserID, platform,
	).Scan(&m.PlatformUserID, &m.Platform, &m.HubUserID, &m.HubUserEmail, &m.RegisteredAt, &m.RegisteredBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user mapping: %w", err)
	}
	return m, nil
}

// SetUserMapping inserts or replaces a user mapping.
func (s *Store) SetUserMapping(m *UserMapping) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO user_mappings (platform_user_id, platform, hub_user_id, hub_user_email, registered_at, registered_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.PlatformUserID, m.Platform, m.HubUserID, m.HubUserEmail, m.RegisteredAt, m.RegisteredBy,
	)
	if err != nil {
		return fmt.Errorf("set user mapping: %w", err)
	}
	return nil
}

// DeleteUserMapping removes a user mapping.
func (s *Store) DeleteUserMapping(platformUserID, platform string) error {
	_, err := s.db.Exec(
		`DELETE FROM user_mappings WHERE platform_user_id = ? AND platform = ?`,
		platformUserID, platform,
	)
	if err != nil {
		return fmt.Errorf("delete user mapping: %w", err)
	}
	return nil
}

// GetUserMappingByHubID returns the user mapping for the given hub user ID, or nil, nil if not found.
func (s *Store) GetUserMappingByHubID(hubUserID string) (*UserMapping, error) {
	m := &UserMapping{}
	err := s.db.QueryRow(
		`SELECT platform_user_id, platform, hub_user_id, hub_user_email, registered_at, registered_by
		 FROM user_mappings WHERE hub_user_id = ?`,
		hubUserID,
	).Scan(&m.PlatformUserID, &m.Platform, &m.HubUserID, &m.HubUserEmail, &m.RegisteredAt, &m.RegisteredBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user mapping by hub id: %w", err)
	}
	return m, nil
}

// --- Space Links ---

// GetSpaceLink returns the space link for the given space, or nil, nil if not found.
func (s *Store) GetSpaceLink(spaceID, platform string) (*SpaceLink, error) {
	l := &SpaceLink{}
	err := s.db.QueryRow(
		`SELECT space_id, platform, grove_id, grove_slug, linked_by, linked_at, default_agent
		 FROM space_links WHERE space_id = ? AND platform = ?`,
		spaceID, platform,
	).Scan(&l.SpaceID, &l.Platform, &l.GroveID, &l.GroveSlug, &l.LinkedBy, &l.LinkedAt, &l.DefaultAgent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get space link: %w", err)
	}
	return l, nil
}

// SetSpaceLink inserts or replaces a space link.
func (s *Store) SetSpaceLink(l *SpaceLink) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO space_links (space_id, platform, grove_id, grove_slug, linked_by, linked_at, default_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.SpaceID, l.Platform, l.GroveID, l.GroveSlug, l.LinkedBy, l.LinkedAt, l.DefaultAgent,
	)
	if err != nil {
		return fmt.Errorf("set space link: %w", err)
	}
	return nil
}

// DeleteSpaceLink removes a space link.
func (s *Store) DeleteSpaceLink(spaceID, platform string) error {
	_, err := s.db.Exec(
		`DELETE FROM space_links WHERE space_id = ? AND platform = ?`,
		spaceID, platform,
	)
	if err != nil {
		return fmt.Errorf("delete space link: %w", err)
	}
	return nil
}

// ListSpaceLinks returns all space links.
func (s *Store) ListSpaceLinks() ([]SpaceLink, error) {
	rows, err := s.db.Query(
		`SELECT space_id, platform, grove_id, grove_slug, linked_by, linked_at, default_agent FROM space_links`,
	)
	if err != nil {
		return nil, fmt.Errorf("list space links: %w", err)
	}
	defer rows.Close()

	var links []SpaceLink
	for rows.Next() {
		var l SpaceLink
		if err := rows.Scan(&l.SpaceID, &l.Platform, &l.GroveID, &l.GroveSlug, &l.LinkedBy, &l.LinkedAt, &l.DefaultAgent); err != nil {
			return nil, fmt.Errorf("scan space link: %w", err)
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// SetDefaultAgent sets the default agent for a space link.
func (s *Store) SetDefaultAgent(spaceID, platform, agentSlug string) error {
	_, err := s.db.Exec(
		`UPDATE space_links SET default_agent = ? WHERE space_id = ? AND platform = ?`,
		agentSlug, spaceID, platform,
	)
	if err != nil {
		return fmt.Errorf("set default agent: %w", err)
	}
	return nil
}

// ClearDefaultAgent removes the default agent for a space link.
func (s *Store) ClearDefaultAgent(spaceID, platform string) error {
	return s.SetDefaultAgent(spaceID, platform, "")
}

// --- Agent Subscriptions ---

// GetAgentSubscription returns the subscription for the given user and agent, or nil, nil if not found.
func (s *Store) GetAgentSubscription(platformUserID, platform, agentID string) (*AgentSubscription, error) {
	sub := &AgentSubscription{}
	err := s.db.QueryRow(
		`SELECT platform_user_id, platform, agent_id, grove_id, activities, subscribed_at
		 FROM agent_subscriptions WHERE platform_user_id = ? AND platform = ? AND agent_id = ?`,
		platformUserID, platform, agentID,
	).Scan(&sub.PlatformUserID, &sub.Platform, &sub.AgentID, &sub.GroveID, &sub.Activities, &sub.SubscribedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent subscription: %w", err)
	}
	return sub, nil
}

// SetAgentSubscription inserts or replaces an agent subscription.
func (s *Store) SetAgentSubscription(sub *AgentSubscription) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO agent_subscriptions (platform_user_id, platform, agent_id, grove_id, activities, subscribed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sub.PlatformUserID, sub.Platform, sub.AgentID, sub.GroveID, sub.Activities, sub.SubscribedAt,
	)
	if err != nil {
		return fmt.Errorf("set agent subscription: %w", err)
	}
	return nil
}

// DeleteAgentSubscription removes an agent subscription.
func (s *Store) DeleteAgentSubscription(platformUserID, platform, agentID string) error {
	_, err := s.db.Exec(
		`DELETE FROM agent_subscriptions WHERE platform_user_id = ? AND platform = ? AND agent_id = ?`,
		platformUserID, platform, agentID,
	)
	if err != nil {
		return fmt.Errorf("delete agent subscription: %w", err)
	}
	return nil
}

// ListAgentSubscriptions returns all subscriptions for the given agent and grove.
func (s *Store) ListAgentSubscriptions(agentID, groveID string) ([]AgentSubscription, error) {
	rows, err := s.db.Query(
		`SELECT platform_user_id, platform, agent_id, grove_id, activities, subscribed_at
		 FROM agent_subscriptions WHERE agent_id = ? AND grove_id = ?`,
		agentID, groveID,
	)
	if err != nil {
		return nil, fmt.Errorf("list agent subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []AgentSubscription
	for rows.Next() {
		var sub AgentSubscription
		if err := rows.Scan(&sub.PlatformUserID, &sub.Platform, &sub.AgentID, &sub.GroveID, &sub.Activities, &sub.SubscribedAt); err != nil {
			return nil, fmt.Errorf("scan agent subscription: %w", err)
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// ListUserSubscriptions returns all subscriptions for the given platform user.
func (s *Store) ListUserSubscriptions(platformUserID, platform string) ([]AgentSubscription, error) {
	rows, err := s.db.Query(
		`SELECT platform_user_id, platform, agent_id, grove_id, activities, subscribed_at
		 FROM agent_subscriptions WHERE platform_user_id = ? AND platform = ?`,
		platformUserID, platform,
	)
	if err != nil {
		return nil, fmt.Errorf("list user subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []AgentSubscription
	for rows.Next() {
		var sub AgentSubscription
		if err := rows.Scan(&sub.PlatformUserID, &sub.Platform, &sub.AgentID, &sub.GroveID, &sub.Activities, &sub.SubscribedAt); err != nil {
			return nil, fmt.Errorf("scan user subscription: %w", err)
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
