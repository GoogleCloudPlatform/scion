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
	"fmt"
)

// MigrateSQLiteToPostgres reads all data from a SQLite database and writes it
// to a Postgres database. The source is opened read-only. The migration is
// idempotent: rows are inserted with ON CONFLICT DO NOTHING semantics.
// Returns a map of table name → row count migrated.
func MigrateSQLiteToPostgres(ctx context.Context, sqlitePath, postgresURL string) (map[string]int, error) {
	src, err := NewSQLiteStoreReadOnly(sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("open source sqlite: %w", err)
	}
	defer src.Close()

	dst, err := NewPostgresStore(postgresURL)
	if err != nil {
		return nil, fmt.Errorf("open target postgres: %w", err)
	}
	defer dst.Close()

	counts := make(map[string]int)

	// Migrate group links.
	links, err := src.GetAllGroupLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read group_links: %w", err)
	}
	for _, link := range links {
		if err := dst.SaveGroupLink(ctx, link); err != nil {
			return nil, fmt.Errorf("write group_link chat_id=%d: %w", link.ChatID, err)
		}
	}
	counts["telegram_group_links"] = len(links)

	// Migrate user mappings.
	mappings, err := src.GetAllUserMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read user_mappings: %w", err)
	}
	for _, m := range mappings {
		if err := dst.SaveUserMapping(ctx, m); err != nil {
			return nil, fmt.Errorf("write user_mapping id=%s: %w", m.TelegramUserID, err)
		}
	}
	counts["telegram_user_mappings"] = len(mappings)

	// Migrate notification prefs (iterate through all mapped users).
	totalPrefs := 0
	for _, m := range mappings {
		prefs, err := src.GetNotificationPrefs(ctx, m.TelegramUserID)
		if err != nil {
			return nil, fmt.Errorf("read notification_prefs for user=%s: %w", m.TelegramUserID, err)
		}
		for _, p := range prefs {
			if err := dst.SaveNotificationPref(ctx, p); err != nil {
				return nil, fmt.Errorf("write notification_pref: %w", err)
			}
			totalPrefs++
		}
	}
	counts["telegram_notification_prefs"] = totalPrefs

	// Note: conversation_contexts, project_agents, pending_ask_users,
	// callback_lookups, and topic_defaults are not migrated because:
	// - conversation_contexts and project_agents are ephemeral caches
	// - pending_ask_users and callback_lookups have expiration and are transient
	// - topic_defaults are linked to group_links which are migrated above
	//
	// To migrate topic_defaults, we'd need a ListAllTopicDefaults method
	// which doesn't exist on the Store interface. The data can be
	// reconstructed by users re-setting defaults after migration.

	return counts, nil
}
