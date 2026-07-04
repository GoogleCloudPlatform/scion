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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateSQLiteToPostgres(t *testing.T) {
	pgURL := os.Getenv("TELEGRAM_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("TELEGRAM_TEST_POSTGRES_URL not set, skipping migration test")
	}

	ctx := context.Background()

	// Set up a populated SQLite store.
	sqlitePath := filepath.Join(t.TempDir(), "migrate_test.db")
	src, err := NewSQLiteStore(sqlitePath)
	require.NoError(t, err)

	// Populate source data.
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, src.SaveGroupLink(ctx, &GroupLink{
		ChatID:             -100,
		ChatTitle:          "Group A",
		ProjectID:          "proj-1",
		ProjectSlug:        "my-project",
		DefaultAgent:       "coder",
		LinkedBy:           "user-1",
		LinkedAt:           now,
		Active:             true,
		ShowAgentToAgent:   true,
		NotifyInGroup:      false,
		ShowAssistantReply: true,
	}))
	require.NoError(t, src.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -200,
		ProjectID: "proj-2",
		LinkedAt:  now,
		Active:    true,
	}))

	require.NoError(t, src.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "user-a",
		TelegramUsername: "alice",
		ScionUserID:      "scion-a",
		ScionEmail:       "alice@example.com",
		LinkedAt:         now,
	}))
	require.NoError(t, src.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "user-b",
		TelegramUsername: "bob",
		ScionEmail:       "bob@example.com",
		LinkedAt:         now,
	}))

	require.NoError(t, src.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: "user-a",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        true,
	}))
	require.NoError(t, src.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: "user-a",
		ProjectID:      "proj-1",
		AgentSlug:      "reviewer",
		Enabled:        false,
	}))

	src.Close()

	// Clean target database.
	cleanPostgresTables(t, pgURL)

	// Run migration.
	counts, err := MigrateSQLiteToPostgres(ctx, sqlitePath, pgURL)
	require.NoError(t, err)

	// Verify row counts.
	assert.Equal(t, 2, counts["telegram_group_links"])
	assert.Equal(t, 2, counts["telegram_user_mappings"])
	assert.Equal(t, 2, counts["telegram_notification_prefs"])

	// Spot-check data in Postgres.
	dst, err := NewPostgresStore(pgURL)
	require.NoError(t, err)
	defer dst.Close()

	link, err := dst.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, "Group A", link.ChatTitle)
	assert.Equal(t, "proj-1", link.ProjectID)
	assert.Equal(t, "coder", link.DefaultAgent)
	assert.True(t, link.Active)
	assert.True(t, link.ShowAgentToAgent)

	mapping, err := dst.GetUserMappingByEmail(ctx, "alice@example.com")
	require.NoError(t, err)
	require.NotNil(t, mapping)
	assert.Equal(t, "user-a", mapping.TelegramUserID)
	assert.Equal(t, "alice", mapping.TelegramUsername)

	pref, err := dst.GetNotificationPref(ctx, "user-a", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, pref)
	assert.True(t, pref.Enabled)

	pref, err = dst.GetNotificationPref(ctx, "user-a", "proj-1", "reviewer")
	require.NoError(t, err)
	require.NotNil(t, pref)
	assert.False(t, pref.Enabled)

	// Verify idempotency: run again, should succeed.
	counts2, err := MigrateSQLiteToPostgres(ctx, sqlitePath, pgURL)
	require.NoError(t, err)
	assert.Equal(t, 2, counts2["telegram_group_links"])
}
