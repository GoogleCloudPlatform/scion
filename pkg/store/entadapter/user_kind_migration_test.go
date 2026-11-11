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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyUsersDDL is the users table shape before the kind, expires_at,
// issued_by and purpose columns. Migrate must upgrade it in place.
const legacyUsersDDL = "CREATE TABLE `users` (" +
	"`id` uuid NOT NULL, `email` text NOT NULL, `display_name` text NOT NULL, " +
	"`avatar_url` text NULL, `role` text NOT NULL DEFAULT ('member'), " +
	"`status` text NOT NULL DEFAULT ('active'), `preferences` json NULL, " +
	"`created` datetime NOT NULL, `invited_by` text NULL, `invite_note` text NULL, " +
	"`last_login` datetime NULL, `last_seen` datetime NULL, " +
	"`session_generation` integer NOT NULL DEFAULT (0), PRIMARY KEY (`id`))"

func TestMigrate_UpgradesLegacyUsersTableWithKind(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy-users.db")
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, legacyUsersDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, "CREATE UNIQUE INDEX `users_email_key` ON `users` (`email`)")
	require.NoError(t, err)
	legacyID := uuid.NewString()
	_, err = raw.ExecContext(ctx,
		"INSERT INTO users (id, email, display_name, role, status, created, session_generation) VALUES (?, 'legacy@example.com', 'Legacy', 'admin', 'active', strftime('%Y-%m-%dT%H:%M:%fZ','now'), 0)",
		legacyID)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.Migrate(ctx))

	got, err := cs.GetUser(ctx, legacyID)
	require.NoError(t, err)
	assert.Equal(t, "legacy@example.com", got.Email)
	assert.Equal(t, store.UserRoleAdmin, got.Role)
	assert.Equal(t, store.UserKindHuman, got.Kind, "existing rows default to human")
	assert.Nil(t, got.ExpiresAt)
	assert.Nil(t, got.IssuedBy)

	var tableSQL string
	require.NoError(t, cs.DB().QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'users'").Scan(&tableSQL))
	assert.Contains(t, tableSQL, "users_test_fixture_expiry_check")

	_, err = cs.DB().ExecContext(ctx,
		"INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind) VALUES (?, ?, 'x', 'member', 'active', ?, 0, 'test_fixture')",
		uuid.NewString(), "raw@"+store.TestFixtureEmailDomain, time.Now().UTC())
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "check constraint")

	exp := time.Now().Add(time.Hour)
	issuer := legacyID
	require.NoError(t, cs.CreateTestFixtureUser(ctx, &store.User{
		ID: uuid.NewString(), Email: "f@" + store.TestFixtureEmailDomain, Role: store.UserRoleMember,
		Kind: store.UserKindTestFixture, ExpiresAt: &exp, IssuedBy: &issuer,
	}))
}
