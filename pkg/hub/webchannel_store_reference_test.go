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

package hub

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertListMessageIDsForAttachment(t *testing.T, wcs WebChatStore) {
	t.Helper()
	ctx := context.Background()
	att := tid("ref-store-attachment")
	other := tid("ref-store-other-attachment")
	msgA, msgB, msgC := tid("ref-store-msg-a"), tid("ref-store-msg-b"), tid("ref-store-msg-c")
	for _, link := range [][2]string{{msgA, att}, {msgB, att}, {msgC, att}, {msgA, other}} {
		require.NoError(t, wcs.LinkAttachmentToMessage(ctx, link[0], link[1]))
	}

	all, err := wcs.ListMessageIDsForAttachment(ctx, att, 20)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{msgA, msgB, msgC}, all)

	limited, err := wcs.ListMessageIDsForAttachment(ctx, att, 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2, "the limit is applied")

	none, err := wcs.ListMessageIDsForAttachment(ctx, tid("ref-store-unlinked"), 20)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestListMessageIDsForAttachment_SQLite(t *testing.T) {
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	assertListMessageIDsForAttachment(t, wcs)
	assert.True(t, sqliteIndexExists(db, "idx_webchat_message_attachment_attachment"))
}

func TestListMessageIDsForAttachment_Postgres(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pgDropWebchatTables(t, db)
	t.Cleanup(func() { pgDropWebchatTables(t, db) })

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	assertListMessageIDsForAttachment(t, wcs)
	assert.True(t, pgIndexExists(db, "idx_webchat_message_attachment_attachment"))
}
