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

package hub

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chat search isolate-and-bound tests on Postgres (ptone/scion#7586; design
// v3 sections 9.9 and 15). They self-skip without SCION_TEST_POSTGRES_DSN;
// make test-webchat-postgres runs them and fails on a skip.

// searchIDs runs a search and returns the result IDs in order. Shared with
// the SQLite tests in chat_search_bound_sqlite_test.go.
func searchIDs(t *testing.T, wcs WebChatStore, f ChatSearchFilter) []string {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 200
	}
	res, _, err := wcs.SearchChatMessages(context.Background(), f)
	require.NoError(t, err)
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.MessageID
	}
	return ids
}

// newPGSearchStore returns a Postgres web chat store on a throwaway schema
// with a minimal messages table, and its pool.
func newPGSearchStore(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	dsn := hubtestutil.RequirePostgresDSN(t)
	ctx := context.Background()
	schema := fmt.Sprintf("chat_search_test_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
    id           uuid PRIMARY KEY,
    project_id   uuid NOT NULL,
    created      timestamptz NOT NULL,
    channel      text,
    thread_id    text,
    msg          text NOT NULL DEFAULT '',
    sender       text,
    sender_id    text,
    recipient    text,
    recipient_id text
)`)
	require.NoError(t, err)
	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	return wcs, db
}

func pgInsertMessage(t *testing.T, db *sql.DB, id, projectID, threadID, msg string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO messages (id, project_id, created, channel, thread_id, msg, sender)
		VALUES ($1, $2, $3, 'web', $4, $5, 'user:a')`, id, projectID, at, threadID, msg)
	require.NoError(t, err)
}

func TestChatSearchLiteralWildcards_Postgres(t *testing.T) {
	wcs, db := newPGSearchStore(t)
	proj := api.NewUUID()
	now := time.Now().UTC()
	ids := map[string]string{}
	for i, m := range []struct{ name, text string }{
		{"pct", "50% off today"}, {"dogs", "50 dogs today"}, {"under", "snake a_b case"},
		{"axb", "snake axb case"}, {"dunder", "x__y marker"}, {"bs", `path ab\ end`},
		{"bs2", `ab\% literal`}, {"plain", "nothing special"},
	} {
		id := api.NewUUID()
		ids[id] = m.name
		pgInsertMessage(t, db, id, proj, "topic-1", m.text, now.Add(time.Duration(i)*time.Second))
	}
	names := func(f ChatSearchFilter) []string {
		got := searchIDs(t, wcs, f)
		out := make([]string, len(got))
		for i, id := range got {
			out[i] = ids[id]
		}
		sort.Strings(out)
		return out
	}
	for term, want := range map[string][]string{
		"50%": {"pct"}, "%": {"bs2", "pct"}, "a_b": {"under"}, "__": {"dunder"},
		`ab\`: {"bs", "bs2"}, `b\%`: {"bs2"}, `\`: {"bs", "bs2"},
	} {
		assert.Equal(t, want, names(ChatSearchFilter{Query: term, ProjectID: proj}), "term %q", term)
	}
}

func TestChatSearchDeletedAndEdited_Postgres(t *testing.T) {
	wcs, db := newPGSearchStore(t)
	ctx := context.Background()
	proj := api.NewUUID()
	now := time.Now().UTC()
	del, blankFail, ed := api.NewUUID(), api.NewUUID(), api.NewUUID()
	pgInsertMessage(t, db, del, proj, "topic-1", "the secret plan", now)
	pgInsertMessage(t, db, blankFail, proj, "topic-1", "another secret note", now.Add(time.Second))
	pgInsertMessage(t, db, ed, proj, "topic-1", "original wording", now.Add(2*time.Second))

	f := ChatSearchFilter{Query: "secret plan", ProjectID: proj}
	require.Equal(t, []string{del}, searchIDs(t, wcs, f), "control: found before delete")
	require.NoError(t, wcs.SetMessageDeleted(ctx, del, now))
	assert.Empty(t, searchIDs(t, wcs, f), "after soft-delete")
	require.NoError(t, wcs.UpdateMessageContent(ctx, del, ""))
	assert.Empty(t, searchIDs(t, wcs, f), "after blanking")
	_, err := db.Exec(`DELETE FROM messages WHERE id = $1`, del)
	require.NoError(t, err)
	assert.Empty(t, searchIDs(t, wcs, f), "after purge")

	// Blanking failed: the text stays, the soft-delete condition excludes it.
	require.NoError(t, wcs.SetMessageDeleted(ctx, blankFail, now))
	assert.Empty(t, searchIDs(t, wcs, ChatSearchFilter{Query: "secret note", ProjectID: proj}))

	require.NoError(t, wcs.UpdateMessageContent(ctx, ed, "revised wording"))
	assert.Empty(t, searchIDs(t, wcs, ChatSearchFilter{Query: "original", ProjectID: proj}))
	assert.Equal(t, []string{ed}, searchIDs(t, wcs, ChatSearchFilter{Query: "revised", ProjectID: proj}))
}

// TestChatSearchDifferential_Postgres checks acceptance 15.4 on Postgres:
// for terms without %, _ or \, results equal the previous release's ILIKE
// query, in order and across pages.
func TestChatSearchDifferential_Postgres(t *testing.T) {
	wcs, db := newPGSearchStore(t)
	proj := api.NewUUID()
	rng := rand.New(rand.NewSource(7586))
	words := []string{"alpha", "Beta", "gamma", "délta", "日本語", "emoji🙂", "plan", "PLAN", "meeting", "notes"}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for j := 0; j < 1+rng.Intn(6); j++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		pgInsertMessage(t, db, api.NewUUID(), proj, "topic-1", b.String(), base.Add(time.Duration(i/3)*time.Second))
	}
	for _, term := range []string{"alpha", "beta", "PLAN", "délta", "日本", "🙂", "ta ga", "zz-absent", "me"} {
		rows, err := db.Query(`SELECT id FROM messages WHERE channel = 'web' AND msg ILIKE '%' || $1 || '%'
			AND project_id = $2 ORDER BY created DESC, id DESC`, term, proj)
		require.NoError(t, err)
		var want []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			want = append(want, id)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()

		var got []string
		cursor := ""
		for {
			res, next, err := wcs.SearchChatMessages(context.Background(),
				ChatSearchFilter{Query: term, ProjectID: proj, Limit: 37, Cursor: cursor})
			require.NoError(t, err)
			for _, r := range res {
				got = append(got, r.MessageID)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		assert.Equal(t, want, got, "term %q", term)
	}
}

// TestChatSearchStatementTimeout_Postgres checks design 6.1 on Postgres: a
// search past its deadline fails as a timeout (57014 or the context
// deadline), and SET LOCAL statement_timeout does not leak to the next user
// of a pooled connection.
func TestChatSearchStatementTimeout_Postgres(t *testing.T) {
	wcs, db := newPGSearchStore(t)
	ctx := context.Background()
	db.SetMaxOpenConns(2)
	proj := api.NewUUID()
	pgInsertMessage(t, db, api.NewUUID(), proj, "topic-1", "needle", time.Now().UTC())

	var before string
	require.NoError(t, db.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&before))

	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	_, _, err := wcs.SearchChatMessages(short, ChatSearchFilter{Query: "needle", ProjectID: proj})
	require.Error(t, err)
	assert.True(t, isChatSearchTimeout(err), "error must classify as a timeout: %v", err)

	// A search that succeeds leaves no session timeout behind on any
	// pooled connection.
	assert.Len(t, searchIDs(t, wcs, ChatSearchFilter{Query: "needle", ProjectID: proj}), 1)
	for i := 0; i < 2; i++ {
		c, err := db.Conn(ctx)
		require.NoError(t, err)
		var after string
		require.NoError(t, c.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&after))
		assert.Equal(t, before, after, "connection %d", i)
		require.NoError(t, c.Close())
	}

	// statement_timeout stops a running statement on the server: 57014.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = 50")
	require.NoError(t, err)
	start := time.Now()
	_, err = tx.ExecContext(ctx, "SELECT pg_sleep(5)")
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, pgQueryCanceled, pgErr.Code)
	assert.True(t, isChatSearchTimeout(err))
	assert.Less(t, time.Since(start), time.Second)
}
