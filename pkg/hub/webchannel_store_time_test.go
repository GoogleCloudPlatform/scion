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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storedtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file use the production SQLite driver (modernc, "sqlite").
// It stores a bound time.Time as time.Time.String() text, which is the source
// of the bugs they cover; the mattn driver used by older webchat tests does
// not, and so hides them.

// newModerncWebChatStore opens a webchat store on a fresh in-memory modernc
// database without the ent schema.
func newModerncWebChatStore(t *testing.T) (*sqliteWebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite").(*sqliteWebChatStore)
	require.NoError(t, wcs.Init())
	return wcs, db
}

// newEntWebChatStore opens a webchat store on the ent-migrated test store's
// database, as the hub does in production.
func newEntWebChatStore(t *testing.T) (*sqliteWebChatStore, store.Store, *sql.DB) {
	t.Helper()
	s := createTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	db := dbp.DB()
	wcs := NewWebChatStore(db, "sqlite").(*sqliteWebChatStore)
	require.NoError(t, wcs.Init())
	return wcs, s, db
}

func rawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(query, args...).Scan(&s))
	return s
}

// requireCanonicalWebchatText asserts the stored text is RFC3339Nano in UTC,
// the canonical form for webchat_* columns.
func requireCanonicalWebchatText(t *testing.T, text string, want time.Time) {
	t.Helper()
	require.Equal(t, want.UTC().Format(time.RFC3339Nano), text)
}

func requireUTCInstant(t *testing.T, got, want time.Time) {
	t.Helper()
	assert.True(t, got.Equal(want), "got %v, want %v", got, want)
	assert.Equal(t, time.UTC, got.Location(), "got %v", got)
}

func tokyoTime(t *testing.T) time.Time {
	t.Helper()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	return time.Date(2026, 10, 1, 14, 20, 59, 820602740, tokyo)
}

func TestWebChatTime_TouchThreadRoundTrip(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	// time.Now() keeps its monotonic reading, which String() would print.
	now := time.Now()
	require.NoError(t, wcs.TouchThread(ctx, "u1", "p1", "agent-now", "m1", now))
	at := tokyoTime(t)
	require.NoError(t, wcs.TouchThread(ctx, "u1", "p1", "agent-tokyo", "m2", at))

	requireCanonicalWebchatText(t, rawText(t, db,
		`SELECT last_activity_at FROM webchat_thread WHERE agent_id = 'agent-tokyo'`), at)

	threads, err := wcs.GetThreads(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, threads, 2)
	got := map[string]time.Time{}
	for _, th := range threads {
		require.False(t, th.LastActivityAt.IsZero(), "thread %s has a zero LastActivityAt", th.AgentID)
		got[th.AgentID] = th.LastActivityAt
	}
	requireUTCInstant(t, got["agent-now"], now)
	requireUTCInstant(t, got["agent-tokyo"], at)
}

func TestWebChatTime_RecordChannelStoresUTC(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	at := tokyoTime(t)
	require.NoError(t, wcs.RecordChannel(ctx, "u1", "p1", "a1", "web", at))
	requireCanonicalWebchatText(t, rawText(t, db,
		`SELECT last_message_at FROM webchat_conversation_context WHERE agent_id = 'a1'`), at)

	ch, err := wcs.GetLastChannel(ctx, "u1", "p1", "a1")
	require.NoError(t, err)
	assert.Equal(t, "web", ch)
}

func TestWebChatTime_AttachmentRoundTrip(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	at := tokyoTime(t)
	require.NoError(t, wcs.CreateAttachment(ctx, AttachmentMeta{
		ID: "att-1", ProjectID: "p1", Filename: "a.txt", MimeType: "text/plain",
		Size: 1, UploadedBy: "u1", CreatedAt: at,
	}))
	requireCanonicalWebchatText(t, rawText(t, db,
		`SELECT created_at FROM webchat_attachment WHERE id = 'att-1'`), at)

	meta, err := wcs.GetAttachment(ctx, "att-1")
	require.NoError(t, err)
	requireUTCInstant(t, meta.CreatedAt, at)
}

func TestWebChatTime_EditDeleteRoundTrip(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	editedAt := tokyoTime(t)
	deletedAt := editedAt.Add(90 * time.Second)
	require.NoError(t, wcs.SetMessageEdited(ctx, "msg-1", editedAt))
	require.NoError(t, wcs.SetMessageDeleted(ctx, "msg-1", deletedAt))

	requireCanonicalWebchatText(t, rawText(t, db,
		`SELECT edited_at FROM webchat_message_ext WHERE message_id = 'msg-1'`), editedAt)
	requireCanonicalWebchatText(t, rawText(t, db,
		`SELECT deleted_at FROM webchat_message_ext WHERE message_id = 'msg-1'`), deletedAt)

	ext, err := wcs.GetMessageExt(ctx, "msg-1")
	require.NoError(t, err)
	require.NotNil(t, ext.EditedAt)
	require.NotNil(t, ext.DeletedAt)
	requireUTCInstant(t, *ext.EditedAt, editedAt)
	requireUTCInstant(t, *ext.DeletedAt, deletedAt)

	exts, err := wcs.GetMessageExts(ctx, []string{"msg-1"})
	require.NoError(t, err)
	require.Contains(t, exts, "msg-1")
	requireUTCInstant(t, *exts["msg-1"].EditedAt, editedAt)
	requireUTCInstant(t, *exts["msg-1"].DeletedAt, deletedAt)
}

// legacyWebchatTimes are text forms older binaries left in webchat_* columns,
// each naming the same instant.
var legacyWebchatTimes = []struct {
	name string
	text string
}{
	{"RFC3339 local offset", "2026-10-01T13:00:00+09:00"},
	{"Go String alphabetic with monotonic", "2026-10-01 13:00:00 +0900 JST m=+0.009943539"},
	{"Go String alphabetic", "2026-10-01 13:00:00 +0900 JST"},
	{"Go String four-digit numeric", "2026-10-01 09:45:00 +0545 +0545"},
	{"Go String four-digit numeric with monotonic", "2026-10-01 09:45:00 +0545 +0545 m=+0.071"},
	{"Go String nameless FixedZone", "2026-10-01 07:00:00 +0300 +0300"},
	{"sqlite offset layout", "2026-10-01 13:00:00+09:00"},
}

var legacyWebchatInstant = time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

func TestWebChatTime_LegacyThreadRows(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	for i, lt := range legacyWebchatTimes {
		_, err := db.Exec(`INSERT INTO webchat_thread (user_id, project_id, agent_id, last_message_id, last_activity_at, last_read_at)
VALUES ('u1', 'p1', ?, 'm', ?, ?)`, fmt.Sprintf("agent-%d", i), lt.text, lt.text)
		require.NoError(t, err)
	}

	threads, err := wcs.GetThreads(ctx, "u1", "p1", 100)
	require.NoError(t, err)
	require.Len(t, threads, len(legacyWebchatTimes))
	for _, th := range threads {
		var i int
		_, err := fmt.Sscanf(th.AgentID, "agent-%d", &i)
		require.NoError(t, err)
		name := legacyWebchatTimes[i].name
		t.Run(name, func(t *testing.T) {
			requireUTCInstant(t, th.LastActivityAt, legacyWebchatInstant)
			require.NotNil(t, th.LastReadAt)
			requireUTCInstant(t, *th.LastReadAt, legacyWebchatInstant)
		})
	}
}

func TestWebChatTime_LegacyRowsThroughParseSQLiteTime(t *testing.T) {
	wcs, db := newModerncWebChatStore(t)
	ctx := context.Background()

	for i, lt := range legacyWebchatTimes {
		t.Run(lt.name, func(t *testing.T) {
			id := fmt.Sprintf("legacy-%d", i)
			_, err := db.Exec(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by, created_at)
VALUES (?, 'p1', 'f', 'text/plain', 1, 'u1', ?)`, id, lt.text)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO webchat_message_ext (message_id, edited_at, deleted_at) VALUES (?, ?, ?)`, id, lt.text, lt.text)
			require.NoError(t, err)

			meta, err := wcs.GetAttachment(ctx, id)
			require.NoError(t, err)
			requireUTCInstant(t, meta.CreatedAt, legacyWebchatInstant)

			ext, err := wcs.GetMessageExt(ctx, id)
			require.NoError(t, err)
			requireUTCInstant(t, *ext.EditedAt, legacyWebchatInstant)
			requireUTCInstant(t, *ext.DeletedAt, legacyWebchatInstant)
		})
	}
}

// TestSearchChatMessages_PagesToExhaustionOnEntSchema pages through search
// results on the ent-migrated messages table, whose created column holds
// time.Time.String() text. An RFC3339 cursor compared as text never advanced
// there, so every page repeated the first one.
func TestSearchChatMessages_PagesToExhaustionOnEntSchema(t *testing.T) {
	wcs, s, _ := newEntWebChatStore(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("search-paging"), Name: "search-paging", Slug: "search-paging",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))

	base := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	// Newest first, as search returns them. The two "tie" messages share a
	// created instant, so paging also crosses the (created = ? AND id < ?) arm.
	type msg struct {
		name    string
		created time.Time
	}
	msgs := []msg{
		{"m5", base.Add(5 * time.Minute)},
		{"tie-b", base.Add(4 * time.Minute)},
		{"tie-a", base.Add(4 * time.Minute)},
		{"m2", base.Add(2*time.Minute + 500*time.Millisecond)},
		{"m1", base.Add(2 * time.Minute)},
		{"m0", base},
	}
	for _, m := range msgs {
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: tid("search-paging-" + m.name), ProjectID: proj.ID,
			Sender: "user:alice", Recipient: "agent:bot", Msg: "needle " + m.name,
			Channel: "web", ThreadID: "topic-1", CreatedAt: m.created,
		}))
	}
	// Expected order: created DESC, id DESC within a tie.
	want := []string{tid("search-paging-m5")}
	tieA, tieB := tid("search-paging-tie-a"), tid("search-paging-tie-b")
	if tieA > tieB {
		want = append(want, tieA, tieB)
	} else {
		want = append(want, tieB, tieA)
	}
	want = append(want, tid("search-paging-m2"), tid("search-paging-m1"), tid("search-paging-m0"))

	var got []string
	cursor := ""
	const maxPages = 10 // a regression must fail here, not hang
	pages := 0
	for {
		pages++
		require.LessOrEqual(t, pages, maxPages, "paging did not terminate; cursor %q, ids so far %v", cursor, got)
		results, next, err := wcs.SearchChatMessages(ctx, ChatSearchFilter{
			Query: "needle", ProjectID: proj.ID, Limit: 2, Cursor: cursor,
		})
		require.NoError(t, err)
		for _, r := range results {
			got = append(got, r.MessageID)
			assert.Equal(t, time.UTC, r.Timestamp.Location())
		}
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next, "cursor did not advance")
		cursor = next
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 3, pages)
}

func TestSearchChatMessages_InvalidCursorTimestamp(t *testing.T) {
	wcs, _, _ := newEntWebChatStore(t)
	_, _, err := wcs.SearchChatMessages(context.Background(), ChatSearchFilter{
		Query: "needle", Limit: 2, Cursor: "not-a-time|some-id",
	})
	require.ErrorIs(t, err, ErrInvalidSearchCursor)
}

// TestChatSearch_InvalidCursorIsBadRequest checks that a malformed cursor is
// reported as a client error, not a server error.
func TestChatSearch_InvalidCursorIsBadRequest(t *testing.T) {
	srv, s := testServer(t)
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	wcs := NewWebChatStore(dbp.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	ctx := context.Background()
	proj := &store.Project{ID: tid("search-bad-cursor"), Name: "search-bad-cursor", Slug: "search-bad-cursor",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))
	srv.createProjectMembersGroup(ctx, proj)
	base := "/api/v1/chat/search?q=needle&projectId=" + proj.ID + "&cursor="

	rec := doRequest(t, srv, http.MethodGet, base+url.QueryEscape("not-a-time|some-id"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidCursor)

	// A well-formed cursor still searches normally.
	rec = doRequest(t, srv, http.MethodGet, base+url.QueryEscape("2026-10-01T04:00:00Z|some-id"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// requireCanonicalEntText asserts the stored text is what the SQLite driver
// writes for a UTC time.Time bound into an ent column.
func requireCanonicalEntText(t *testing.T, text string, want time.Time) {
	t.Helper()
	require.Equal(t, want.UTC().String(), text)
}

// TestWebChatTime_ConversationInsertsBindTime checks that every raw webchat
// INSERT into the ent conversations table stores ent's canonical text, not
// RFC3339 (which sorts after every same-day ent row).
func TestWebChatTime_ConversationInsertsBindTime(t *testing.T) {
	wcs, s, db := newEntWebChatStore(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("conv-insert"), Name: "conv-insert", Slug: "conv-insert",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))

	convTimes := func(id string) (string, string) {
		t.Helper()
		return rawText(t, db, `SELECT CAST(last_activity_at AS TEXT) FROM conversations WHERE id = ?`, id),
			rawText(t, db, `SELECT CAST(created_at AS TEXT) FROM conversations WHERE id = ?`, id)
	}
	topicConv := func(topicID string) string {
		t.Helper()
		return rawText(t, db, `SELECT conversation_id FROM webchat_topic WHERE id = ?`, topicID)
	}

	t.Run("CreateTopic", func(t *testing.T) {
		at := tokyoTime(t)
		topicID := tid("conv-insert-topic")
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: "topic", CreatedBy: "u1", CreatedAt: at,
		}))
		last, created := convTimes(topicConv(topicID))
		requireCanonicalEntText(t, last, at)
		requireCanonicalEntText(t, created, at)
		requireCanonicalWebchatText(t, rawText(t, db, `SELECT created_at FROM webchat_topic WHERE id = ?`, topicID), at)
	})

	t.Run("PromoteDM", func(t *testing.T) {
		at := tokyoTime(t).Add(time.Minute)
		topicID := tid("conv-insert-promoted")
		_, err := wcs.PromoteDM(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: "promoted", CreatedBy: "u1",
			CreatedAt: at, LastActivityAt: at,
		}, PromoteKeys{DMKey: "dm:agent:a:user:u1"})
		require.NoError(t, err)
		last, created := convTimes(topicConv(topicID))
		requireCanonicalEntText(t, last, at)
		requireCanonicalEntText(t, created, at)
	})

	t.Run("EnsureGeneralTopic", func(t *testing.T) {
		before := time.Now()
		topicID, inserted, err := wcs.EnsureGeneralTopic(ctx, proj.ID, "u1")
		require.NoError(t, err)
		require.True(t, inserted)
		last, created := convTimes(topicConv(topicID))
		assert.True(t, strings.HasSuffix(last, " +0000 UTC"), "last_activity_at %q", last)
		assert.Equal(t, last, created)
		stored, err := storedtime.ParseGoString(last)
		require.NoError(t, err)
		assert.False(t, stored.Before(before.Truncate(time.Second)), "stored %v before %v", stored, before)
		// The webchat_topic row carries the same instant in its own canonical form.
		requireCanonicalWebchatText(t, rawText(t, db, `SELECT created_at FROM webchat_topic WHERE id = ?`, topicID), stored)
	})

	t.Run("backfillTopicConversations", func(t *testing.T) {
		topicID := tid("conv-insert-backfill")
		_, err := db.Exec(`INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at)
VALUES (?, ?, 'backfill', 0, 'u1', ?)`, topicID, proj.ID, time.Now().UTC().Format(time.RFC3339Nano))
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'`)
		require.NoError(t, err)

		require.NoError(t, wcs.backfillTopicConversations())
		last, created := convTimes(topicConv(topicID))
		assert.True(t, strings.HasSuffix(last, " +0000 UTC"), "last_activity_at %q", last)
		assert.True(t, strings.HasSuffix(created, " +0000 UTC"), "created_at %q", created)
	})
}

// TestWebChatTime_ConversationListOrdersMixedRows lists conversations created
// by ent and by the webchat store together: the ent store orders and pages on
// last_activity_at text, so both writers must store the same form.
func TestWebChatTime_ConversationListOrdersMixedRows(t *testing.T) {
	wcs, s, _ := newEntWebChatStore(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("conv-order"), Name: "conv-order", Slug: "conv-order",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))

	base := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	entConvID := tid("conv-order-ent")
	pid := proj.ID
	require.NoError(t, s.CreateConversation(ctx, &store.Conversation{
		ID: entConvID, ProjectID: &pid, Kind: "group", Surface: "native",
		ExternalRef: "thread:" + proj.ID + ":ent", DriftState: "active",
		LastActivityAt: base.Add(time.Hour), CreatedAt: base.Add(time.Hour),
	}))

	createTopic := func(name string, at time.Time) string {
		t.Helper()
		topicID := tid("conv-order-" + name)
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: name, CreatedBy: "u1", CreatedAt: at,
		}))
		convID, err := wcs.GetTopicConversationID(ctx, topicID)
		require.NoError(t, err)
		return convID
	}
	older := createTopic("older", base)
	newer := createTopic("newer", base.Add(2*time.Hour))

	res, err := s.ListConversations(ctx, store.ConversationFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	var ids []string
	for _, c := range res.Items {
		ids = append(ids, c.ID)
	}
	assert.Equal(t, []string{newer, entConvID, older}, ids)

	// Paging with Limit 1 walks the same order through the keyset cursor.
	var paged []string
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 10, "conversation paging did not terminate")
		r, err := s.ListConversations(ctx, store.ConversationFilter{ProjectID: proj.ID},
			store.ListOptions{Limit: 1, Cursor: cursor, SkipTotalCount: true})
		require.NoError(t, err)
		for _, c := range r.Items {
			paged = append(paged, c.ID)
		}
		if r.NextCursor == "" {
			break
		}
		cursor = r.NextCursor
	}
	assert.Equal(t, []string{newer, entConvID, older}, paged)
}
