// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/require"
)

// TestUnreadMentionKeys_Postgres covers the Postgres-only parts of the
// unread-mention query: the text-to-uuid cast of message_id, and the
// UUID-shape guards that turn an empty or malformed watermark into "no
// watermark" and make a non-UUID mention message_id inert instead of a
// cast error. It also covers the orphan sweep, including that row.
//
// It runs in a throwaway schema with a minimal messages table, so it never
// touches an existing messages table in the target database.
func TestUnreadMentionKeys_Postgres(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// One connection, so the session search_path applies to every query.
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	schema := fmt.Sprintf("mention_dot_test_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	_, err = db.ExecContext(ctx, "SET search_path TO "+schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
    id      uuid PRIMARY KEY,
    created timestamptz NOT NULL
)`)
	require.NoError(t, err)

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())

	user := api.NewUUID()
	base := time.Now().UTC().Add(-time.Hour)
	msg := func(at time.Time) string {
		id := api.NewUUID()
		_, err := db.ExecContext(ctx, `INSERT INTO messages (id, created) VALUES ($1, $2)`, id, at)
		require.NoError(t, err)
		return id
	}

	// key -> watermark setup; every thread has one mention of user.
	cases := map[string]struct {
		watermark func(mention string) string
		want      bool
	}{
		"no-read-state": {nil, true},
		"empty":         {func(string) string { return "" }, true},
		"malformed":     {func(string) string { return "not-a-uuid" }, true},
		"missing":       {func(string) string { return api.NewUUID() }, true},
		"read":          {func(m string) string { return m }, false},
		"before":        {func(string) string { return msg(base.Add(-time.Minute)) }, true},
	}
	keys := make([]string, 0, len(cases))
	for key, tc := range cases {
		keys = append(keys, key)
		mention := msg(base)
		require.NoError(t, wcs.RecordMentions(ctx, key, mention, []string{user}))
		if tc.watermark != nil {
			require.NoError(t, wcs.SetReadState(ctx, user, key, tc.watermark(mention)))
		}
	}

	// A non-UUID message_id is ignored rather than failing the cast, and
	// the caller's other threads are still reported.
	require.NoError(t, wcs.RecordMentions(ctx, "legacy", "not-a-uuid", []string{user}))
	keys = append(keys, "legacy")

	got, err := wcs.UnreadMentionKeys(ctx, user, keys)
	require.NoError(t, err)
	for key, tc := range cases {
		if got[key] != tc.want {
			t.Errorf("%s: unread mention = %v; want %v", key, got[key], tc.want)
		}
	}
	if got["legacy"] {
		t.Error("legacy: non-UUID mention reported as unread")
	}

	// Another user's rows never surface.
	other, err := wcs.UnreadMentionKeys(ctx, api.NewUUID(), keys)
	require.NoError(t, err)
	require.Empty(t, other)

	// The orphan sweep drops a row whose message is gone and keeps the rest.
	// The non-UUID "legacy" row must not fail the sweep on the uuid cast;
	// it can never match a message, so it is swept as an orphan too.
	require.NoError(t, wcs.RecordMentions(ctx, "orphan", api.NewUUID(), []string{user}))
	n, err := wcs.PurgeOrphanMentions(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	var left int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webchat_mention`).Scan(&left))
	require.Equal(t, len(cases), left)
}
