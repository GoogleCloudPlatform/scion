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
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests seed a SQLite hub (the production modernc driver and the
// ent-migrated schema) with the legacy timestamp text earlier releases
// wrote, and check that utc-timestamp-normalize rewrites it (design §2.1.6,
// acceptance criterion AC2).

// normBase is the reference instant for the fixtures.
var normBase = time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

// legacyEntCase is an ent column value as some earlier release stored it.
type legacyEntCase struct {
	name string
	text string
	want time.Time
}

// legacyEntCases covers every non-canonical ent-column shape in AC2, plus a
// canonical row.
func legacyEntCases() []legacyEntCase {
	return []legacyEntCase{
		{"jst-monotonic", "2026-10-01 13:00:00.25 +0900 JST m=+0.071234", normBase.Add(250 * time.Millisecond)},
		{"rfc3339-offset", "2026-10-01T13:00:01+09:00", normBase.Add(1 * time.Second)},
		{"t-layout-z", "2026-10-01T04:00:02.5Z", normBase.Add(2500 * time.Millisecond)},
		{"kathmandu", "2026-10-01 09:45:03 +0545 +0545", normBase.Add(3 * time.Second)},
		{"kathmandu-monotonic", "2026-10-01 09:45:04.125 +0545 +0545 m=+12.5", normBase.Add(4125 * time.Millisecond)},
		{"nameless-fixed-zone", "2026-10-01 06:00:05 +0200 +0200", normBase.Add(5 * time.Second)},
		{"canonical", "2026-10-01 04:00:06 +0000 UTC", normBase.Add(6 * time.Second)},
	}
}

// legacyWebchatCases covers the non-canonical webchat-column shapes in AC2,
// plus a canonical row.
func legacyWebchatCases() []legacyEntCase {
	return []legacyEntCase{
		{"jst-string", "2026-10-01 13:00:00.25 +0900 JST m=+0.071234", normBase.Add(250 * time.Millisecond)},
		{"kathmandu-string", "2026-10-01 09:45:01 +0545 +0545", normBase.Add(1 * time.Second)},
		{"nameless-string", "2026-10-01 06:00:02 +0200 +0200", normBase.Add(2 * time.Second)},
		{"rfc3339-offset", "2026-10-01T13:00:03.5+09:00", normBase.Add(3500 * time.Millisecond)},
		{"legacy-space-offset", "2026-10-01 13:00:04+09:00", normBase.Add(4 * time.Second)},
		{"canonical", "2026-10-01T04:00:05Z", normBase.Add(5 * time.Second)},
	}
}

func normProject(t *testing.T, s store.Store, name string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid(name), Name: name, Slug: name, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

func execOne(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	res, err := db.Exec(q, args...)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "%s", q)
}

// dumpDB returns every row of every table, each value through quote(), so
// two dumps are equal only if the stored text is byte-identical.
func dumpDB(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()

	var b strings.Builder
	for _, tbl := range tables {
		crows, err := db.Query("SELECT name FROM pragma_table_info(?)", tbl)
		require.NoError(t, err)
		var cols []string
		for crows.Next() {
			var c string
			require.NoError(t, crows.Scan(&c))
			cols = append(cols, `quote("`+c+`")`)
		}
		_ = crows.Close()
		if len(cols) == 0 {
			continue
		}
		q := fmt.Sprintf(`SELECT %s FROM "%s" ORDER BY rowid`, strings.Join(cols, " || '|' || "), tbl)
		drows, err := db.Query(q)
		require.NoError(t, err, q)
		for drows.Next() {
			var line string
			require.NoError(t, drows.Scan(&line))
			b.WriteString(tbl + ": " + line + "\n")
		}
		require.NoError(t, drows.Err())
		_ = drows.Close()
	}
	return b.String()
}

func runNormalize(t *testing.T, db *sql.DB, opts entadapter.TimestampNormalizeOptions) (entadapter.TimestampNormalizeReport, string) {
	t.Helper()
	var log bytes.Buffer
	rep, err := entadapter.NormalizeUTCTimestamps(context.Background(), db, dialect.SQLite, &log, opts)
	require.NoError(t, err, log.String())
	return rep, log.String()
}

// TestUTCTimestampNormalize_RewritesBothFamilies is the AC2 fixture: every
// legacy shape in an ent column and in a webchat column ends in its family's
// canonical form, tables that were unreadable become readable, and a second
// run changes nothing.
func TestUTCTimestampNormalize_RewritesBothFamilies(t *testing.T) {
	wcs, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	proj := normProject(t, s, "norm-families")

	entCases := legacyEntCases()
	for i, c := range entCases {
		id := tid("norm-msg-" + c.name)
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
			Msg: fmt.Sprintf("m%d", i), Channel: "web", CreatedAt: normBase,
		}))
		execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", c.text, id)
	}

	// Four-digit numeric abbreviations make every ent read of the table fail.
	_, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 50})
	require.Error(t, err, "the fixture must reproduce the unreadable-table failure")

	webCases := legacyWebchatCases()
	for _, c := range webCases {
		id := tid("norm-topic-" + c.name)
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: id, ProjectID: proj.ID, Name: c.name, CreatedBy: "u1", CreatedAt: normBase,
		}))
		execOne(t, db, "UPDATE webchat_topic SET created_at = ?, last_activity_at = ?, deleted_at = ? WHERE id = ?",
			c.text, c.text, c.text, id)
	}

	rep, log := runNormalize(t, db, entadapter.TimestampNormalizeOptions{BatchSize: 2})
	assert.Zero(t, rep.Unparseable, log)
	assert.Positive(t, rep.Rewritten)

	for _, c := range entCases {
		got := rawText(t, db, "SELECT quote(created) FROM messages WHERE id = ?", tid("norm-msg-"+c.name))
		assert.Equal(t, "'"+c.want.String()+"'", got, c.name)
		assert.True(t, strings.HasSuffix(got, " +0000 UTC'"), "%s: %s", c.name, got)
	}
	for _, c := range webCases {
		for _, col := range []string{"created_at", "last_activity_at", "deleted_at"} {
			got := rawText(t, db, "SELECT quote("+col+") FROM webchat_topic WHERE id = ?", tid("norm-topic-"+c.name))
			assert.Equal(t, "'"+c.want.Format(time.RFC3339Nano)+"'", got, "%s %s", c.name, col)
		}
	}

	// The table is readable again, and the values are the right instants.
	res, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 50, SortDir: "asc"})
	require.NoError(t, err)
	require.Len(t, res.Items, len(entCases))
	for i, m := range res.Items {
		requireUTCInstant(t, m.CreatedAt, entCases[i].want)
	}

	// No ent table holds a non-canonical value any more.
	chk, err := entadapter.CheckStoredTimestamps(ctx, db, dialect.SQLite)
	require.NoError(t, err)
	assert.Equal(t, entadapter.TimestampCheck{}, chk)

	// A second run changes nothing.
	before := dumpDB(t, db)
	rep, _ = runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
	assert.Zero(t, rep.Rewritten)
	assert.Equal(t, before, dumpDB(t, db))
}

// TestUTCTimestampNormalize_ScheduledEventFixedZone covers the
// ptone/scion#2473 shape: a fire_at stored from a time with a nameless
// FixedZone makes ListScheduledEvents fail until the operation runs.
func TestUTCTimestampNormalize_ScheduledEventFixedZone(t *testing.T) {
	_, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	proj := normProject(t, s, "norm-sched")

	ev := &store.ScheduledEvent{
		ID: tid("norm-sched-ev"), ProjectID: proj.ID, EventType: "message",
		FireAt: normBase.Add(3 * time.Hour), Payload: "{}", Status: "pending",
		CreatedAt: normBase, CreatedBy: "u1",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, ev))
	execOne(t, db, "UPDATE scheduled_events SET fire_at = ? WHERE id = ?", "2026-10-01 09:00:00 +0200 +0200", ev.ID)

	_, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 10})
	require.Error(t, err, "a nameless-FixedZone fire_at must fail the listing before the run")

	runNormalize(t, db, entadapter.TimestampNormalizeOptions{})

	res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	requireUTCInstant(t, res.Items[0].FireAt, time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC))
	assert.Equal(t, "'2026-10-01 07:00:00 +0000 UTC'",
		rawText(t, db, "SELECT quote(fire_at) FROM scheduled_events WHERE id = ?", ev.ID))
}

// TestUTCTimestampNormalize_OrderingOverMigratedAndNewRows checks that,
// after the run, migrated legacy rows and rows written afterwards on the
// same day (including sub-second neighbours) page, search, threshold and
// list in instant order with no repeats or gaps.
func TestUTCTimestampNormalize_OrderingOverMigratedAndNewRows(t *testing.T) {
	wcs, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	proj := normProject(t, s, "norm-order")

	type row struct {
		id string
		at time.Time
	}
	var all []row
	addMsg := func(name string, at time.Time, legacy string) {
		t.Helper()
		id := tid("norm-order-" + name)
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
			Msg: "needle " + name, Channel: "web", ThreadID: "topic-1", CreatedAt: at,
		}))
		if legacy != "" {
			execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", legacy, id)
		}
		all = append(all, row{id, at})
	}
	// Legacy rows, as a Tokyo, Kathmandu or RFC 3339 writer stored them.
	addMsg("l-jst", normBase.Add(250*time.Millisecond), "2026-10-01 13:00:00.25 +0900 JST m=+0.01")
	addMsg("l-ktm", normBase.Add(1*time.Second), "2026-10-01 09:45:01 +0545 +0545")
	addMsg("l-t", normBase.Add(2500*time.Millisecond), "2026-10-01T04:00:02.5Z")
	addMsg("l-fz", normBase.Add(4*time.Second), "2026-10-01 06:00:04 +0200 +0200 m=+1.5")

	// Legacy conversations: a pre-fix webchat insert (RFC 3339 text) and a
	// Tokyo-stamped ent row.
	pid := proj.ID
	convLegacyWeb, convLegacyJST := tid("norm-conv-lweb"), tid("norm-conv-ljst")
	for _, c := range []struct {
		id, text string
	}{
		{convLegacyWeb, "2026-10-01T04:30:00.5Z"},
		{convLegacyJST, "2026-10-01 13:10:00 +0900 JST m=+0.2"},
	} {
		require.NoError(t, s.CreateConversation(ctx, &store.Conversation{
			ID: c.id, ProjectID: &pid, Kind: "group", Surface: "native",
			ExternalRef: "thread:" + c.id, DriftState: "active",
			LastActivityAt: normBase, CreatedAt: normBase,
		}))
		execOne(t, db, "UPDATE conversations SET last_activity_at = ?, created_at = ? WHERE id = ?", c.text, c.text, c.id)
	}

	runNormalize(t, db, entadapter.TimestampNormalizeOptions{BatchSize: 3})

	// Rows written after the run, interleaved with the migrated ones.
	addMsg("n-300ms", normBase.Add(300*time.Millisecond), "")
	addMsg("n-1s1ms", normBase.Add(1*time.Second+time.Millisecond), "")
	addMsg("n-2400ms", normBase.Add(2400*time.Millisecond), "")
	addMsg("n-200ms", normBase.Add(200*time.Millisecond), "")
	convEnt := tid("norm-conv-ent")
	require.NoError(t, s.CreateConversation(ctx, &store.Conversation{
		ID: convEnt, ProjectID: &pid, Kind: "group", Surface: "native",
		ExternalRef: "thread:" + convEnt, DriftState: "active",
		LastActivityAt: normBase.Add(20 * time.Minute), CreatedAt: normBase.Add(20 * time.Minute),
	}))
	topicID := tid("norm-conv-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "t", CreatedBy: "u1", CreatedAt: normBase.Add(40 * time.Minute),
	}))
	convTopic, err := wcs.GetTopicConversationID(ctx, topicID)
	require.NoError(t, err)

	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	var want []string
	for _, r := range all {
		want = append(want, r.id)
	}

	// Message pagination (created DESC), Limit 2.
	var paged []string
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 20, "message paging did not terminate")
		res, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: proj.ID},
			store.ListOptions{Limit: 2, Cursor: cursor, SkipTotalCount: true})
		require.NoError(t, err)
		for _, m := range res.Items {
			paged = append(paged, m.ID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	assert.Equal(t, want, paged, "message pagination")

	// SearchChatMessages keyset paging on the ent schema terminates and
	// returns the same order.
	var searched []string
	cursor = ""
	for page := 0; ; page++ {
		require.Less(t, page, 20, "search paging did not terminate")
		results, next, err := wcs.SearchChatMessages(ctx, ChatSearchFilter{
			Query: "needle", ProjectID: proj.ID, Limit: 2, Cursor: cursor,
		})
		require.NoError(t, err)
		for _, r := range results {
			searched = append(searched, r.MessageID)
		}
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next, "search cursor did not advance")
		cursor = next
	}
	assert.Equal(t, want, searched, "SearchChatMessages paging")

	// A CreatedLT threshold selects exactly the earlier instants.
	threshold := normBase.Add(1*time.Second + 500*time.Microsecond)
	res, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: proj.ID, Before: threshold},
		store.ListOptions{Limit: 50, SkipTotalCount: true})
	require.NoError(t, err)
	var below, wantBelow []string
	for _, m := range res.Items {
		below = append(below, m.ID)
	}
	for _, r := range all {
		if r.at.Before(threshold) {
			wantBelow = append(wantBelow, r.id)
		}
	}
	assert.Equal(t, wantBelow, below, "CreatedLT threshold")

	// Conversation list order and paging over migrated, ent-created and
	// webchat-created rows.
	wantConv := []string{convTopic, convLegacyWeb, convEnt, convLegacyJST}
	var convs []string
	cursor = ""
	for page := 0; ; page++ {
		require.Less(t, page, 10, "conversation paging did not terminate")
		r, err := s.ListConversations(ctx, store.ConversationFilter{ProjectID: proj.ID},
			store.ListOptions{Limit: 1, Cursor: cursor, SkipTotalCount: true})
		require.NoError(t, err)
		for _, c := range r.Items {
			convs = append(convs, c.ID)
		}
		if r.NextCursor == "" {
			break
		}
		cursor = r.NextCursor
	}
	assert.Equal(t, wantConv, convs, "conversation paging")
}

// TestUTCTimestampNormalize_UnparseableLeftAloneAndNotLogged checks that a
// value no layout parses is reported by table, column and rowid only, and is
// not changed.
func TestUTCTimestampNormalize_UnparseableLeftAloneAndNotLogged(t *testing.T) {
	wcs, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	proj := normProject(t, s, "norm-bad")

	msgID := tid("norm-bad-msg")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: msgID, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
		Msg: "x", Channel: "web", CreatedAt: normBase,
	}))
	execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", "garbage-ent-3b9d", msgID)
	topicID := tid("norm-bad-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "t", CreatedBy: "u1", CreatedAt: normBase}))
	execOne(t, db, "UPDATE webchat_topic SET deleted_at = ? WHERE id = ?", "garbage-web-8e2a", topicID)

	rep, log := runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
	assert.Equal(t, 2, rep.Unparseable)
	assert.NotContains(t, log, "garbage-ent-3b9d")
	assert.NotContains(t, log, "garbage-web-8e2a")
	assert.Regexp(t, `table=messages column=created rowid=\d+`, log)
	assert.Regexp(t, `table=webchat_topic column=deleted_at rowid=\d+`, log)
	assert.Equal(t, "garbage-ent-3b9d", rawText(t, db, "SELECT CAST(created AS TEXT) FROM messages WHERE id = ?", msgID))
	assert.Equal(t, "garbage-web-8e2a", rawText(t, db, "SELECT CAST(deleted_at AS TEXT) FROM webchat_topic WHERE id = ?", topicID))
}

// TestUTCTimestampNormalize_ResumesAfterInterruption cancels a run part way
// through and checks that running again finishes the job with the same
// result as one uninterrupted run.
func TestUTCTimestampNormalize_ResumesAfterInterruption(t *testing.T) {
	seed := func(t *testing.T) *sql.DB {
		wcs, s, db := newEntWebChatStore(t)
		ctx := context.Background()
		proj := normProject(t, s, "norm-resume")
		for i, c := range legacyEntCases() {
			id := tid("norm-resume-" + c.name)
			require.NoError(t, s.CreateMessage(ctx, &store.Message{
				ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
				Msg: fmt.Sprint(i), Channel: "web", CreatedAt: normBase,
			}))
			execOne(t, db, "UPDATE messages SET created = ?, dispatched_at = ? WHERE id = ?", c.text, c.text, id)
		}
		for _, c := range legacyWebchatCases() {
			id := tid("norm-resume-topic-" + c.name)
			require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{ID: id, ProjectID: proj.ID, Name: c.name, CreatedBy: "u1", CreatedAt: normBase}))
			execOne(t, db, "UPDATE webchat_topic SET created_at = ? WHERE id = ?", c.text, id)
		}
		return db
	}

	// Reference: one uninterrupted run.
	refDB := seed(t)
	runNormalize(t, refDB, entadapter.TimestampNormalizeOptions{BatchSize: 2})
	refChk, err := entadapter.CheckStoredTimestamps(context.Background(), refDB, dialect.SQLite)
	require.NoError(t, err)
	require.Equal(t, entadapter.TimestampCheck{}, refChk)

	db := seed(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel as soon as the first table reports progress.
	w := &cancelOnWrite{cancel: cancel, after: "table messages"}
	_, err = entadapter.NormalizeUTCTimestamps(ctx, db, dialect.SQLite, w, entadapter.TimestampNormalizeOptions{BatchSize: 2})
	require.ErrorIs(t, err, context.Canceled)
	msgQ := "SELECT group_concat(quote(created) || quote(dispatched_at), ',') FROM (SELECT created, dispatched_at FROM messages ORDER BY id)"
	topicQ := "SELECT group_concat(quote(created_at), ',') FROM (SELECT created_at FROM webchat_topic ORDER BY id)"
	// The cancel lands after messages and before webchat_topic.
	assert.Equal(t, rawText(t, refDB, msgQ), rawText(t, db, msgQ), "messages finished before the interruption")
	require.NotEqual(t, rawText(t, refDB, topicQ), rawText(t, db, topicQ), "the interrupted run should leave work behind")

	runNormalize(t, db, entadapter.TimestampNormalizeOptions{BatchSize: 2})
	for _, q := range []string{msgQ, topicQ} {
		assert.Equal(t, rawText(t, refDB, q), rawText(t, db, q), q)
	}
}

type cancelOnWrite struct {
	cancel context.CancelFunc
	after  string
	buf    bytes.Buffer
}

func (c *cancelOnWrite) Write(p []byte) (int, error) {
	c.buf.Write(p)
	if strings.Contains(c.buf.String(), c.after) {
		c.cancel()
	}
	return len(p), nil
}

// TestUTCTimestampNormalize_StartupCheck covers the hub-start probe: it is
// silent on a canonical store, reports a named-zone row and a numeric-zone
// row by table, and is silent again after the operation runs.
func TestUTCTimestampNormalize_StartupCheck(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		unreadable bool
	}{
		{"named-zone", "2026-10-01 13:00:00 +0900 JST", false},
		{"named-zone-monotonic", "2026-10-01 05:00:00 -0700 PDT m=+0.5", false},
		{"utc-monotonic", "2026-10-01 04:00:00 +0000 UTC m=+0.5", false},
		{"numeric-zone", "2026-10-01 09:45:00 +0545 +0545", true},
		{"nameless-fixed-zone", "2026-10-01 06:00:00 +0200 +0200 m=+1.5", true},
		{"rfc3339", "2026-10-01T13:00:00+09:00", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, s, db := newEntWebChatStore(t)
			ctx := context.Background()
			srv := &Server{store: s}
			proj := normProject(t, s, "norm-startup")
			id := tid("norm-startup-msg")
			require.NoError(t, s.CreateMessage(ctx, &store.Message{
				ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
				Msg: "x", Channel: "web", CreatedAt: normBase.Add(123456 * time.Microsecond),
			}))

			logs := captureSlog(t)
			srv.checkStoredTimestamps(ctx)
			assert.NotContains(t, logs.String(), entadapter.UTCTimestampNormalizeKey, "canonical store must not warn")

			execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", c.text, id)
			logs.Reset()
			srv.checkStoredTimestamps(ctx)
			out := logs.String()
			assert.Contains(t, out, "level=ERROR")
			assert.Contains(t, out, entadapter.UTCTimestampNormalizeKey)
			assert.Contains(t, out, "messages")
			assert.Equal(t, 1, strings.Count(out, "level=ERROR"), "exactly one error line")
			assert.Contains(t, out, "repaired automatically at hub start")
			if c.unreadable {
				assert.Contains(t, out, "tables_unreadable=[messages]")
			} else {
				assert.Contains(t, out, "tables_unreadable=[]")
			}
			assert.NotContains(t, out, "level=WARN")
			assert.NotContains(t, out, c.text, "a stored value reached the log")

			runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
			logs.Reset()
			srv.checkStoredTimestamps(ctx)
			assert.Empty(t, logs.String(), "the check must be silent after the run")
		})
	}
}

// TestUTCTimestampNormalize_ExecutorWiring checks the seed, the executor
// registration, a dry run and a real run through the migrations endpoint.
func TestUTCTimestampNormalize_ExecutorWiring(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	key := entadapter.UTCTimestampNormalizeKey
	require.Equal(t, "utc-timestamp-normalize", key)

	op, err := s.GetMaintenanceOperation(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceCategoryMigration, op.Category)
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)

	ex, err := srv.resolveMaintenanceExecutor(key)
	require.NoError(t, err)
	nex, ok := ex.(*UTCTimestampNormalizeExecutor)
	require.True(t, ok, "got %T", ex)
	require.NotNil(t, nex.DB)
	db := nex.DB

	proj := normProject(t, s, "norm-exec")
	id := tid("norm-exec-msg")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
		Msg: "x", Channel: "web", CreatedAt: normBase,
	}))
	const legacy = "2026-10-01 09:45:00 +0545 +0545"
	execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", legacy, id)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	run := func(body string) *store.MaintenanceOperation {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/"+key+"/run", strings.NewReader(body))
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleAdminMaintenanceMigrations(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var got *store.MaintenanceOperation
		require.Eventually(t, func() bool {
			got, err = s.GetMaintenanceOperation(ctx, key)
			return err == nil && got.Status != store.MaintenanceStatusRunning
		}, 10*time.Second, 20*time.Millisecond)
		return got
	}

	got := run(`{"params":{"dryRun":true}}`)
	assert.Equal(t, store.MaintenanceStatusPending, got.Status, got.Result)
	assert.Contains(t, got.Result, "dry run")
	assert.Equal(t, legacy, rawText(t, db, "SELECT CAST(created AS TEXT) FROM messages WHERE id = ?", id))

	got = run(`{}`)
	assert.Equal(t, store.MaintenanceStatusCompleted, got.Status, got.Result)
	assert.NotContains(t, got.Result, legacy)
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", rawText(t, db, "SELECT CAST(created AS TEXT) FROM messages WHERE id = ?", id))

	// A completed run can run again: a row written afterwards (an older
	// binary, a restored backup) is rewritten and the startup check goes
	// silent.
	id2 := tid("norm-exec-msg-2")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: id2, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
		Msg: "y", Channel: "web", CreatedAt: normBase,
	}))
	execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", "2026-10-01 13:00:01 +0900 JST m=+0.5", id2)
	logs := captureSlog(t)
	srv.checkStoredTimestamps(ctx)
	assert.Contains(t, logs.String(), "level=ERROR")

	got = run(`{}`)
	assert.Equal(t, store.MaintenanceStatusCompleted, got.Status, got.Result)
	assert.Contains(t, got.Result, "rewrote 1 values")
	assert.Equal(t, "2026-10-01 04:00:01 +0000 UTC", rawText(t, db, "SELECT CAST(created AS TEXT) FROM messages WHERE id = ?", id2))
	logs.Reset()
	srv.checkStoredTimestamps(ctx)
	assert.Empty(t, logs.String())
}

// TestUTCTimestampNormalize_StartupCheckUnparseableOnly checks that when the
// only non-canonical values left are unparseable, the check points at the
// run log instead of asking for another run, and that a parseable leftover
// still asks for a run.
func TestUTCTimestampNormalize_StartupCheckUnparseableOnly(t *testing.T) {
	_, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	srv := &Server{store: s}
	proj := normProject(t, s, "norm-startup-bad")
	mk := func(name, text string) string {
		t.Helper()
		id := tid("norm-startup-bad-" + name)
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
			Msg: name, Channel: "web", CreatedAt: normBase,
		}))
		execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", text, id)
		return id
	}
	mk("bad", "garbage-startup-51d0")

	rep, _ := runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
	require.Equal(t, 1, rep.Unparseable)

	logs := captureSlog(t)
	srv.checkStoredTimestamps(ctx)
	out := logs.String()
	assert.NotContains(t, out, "level=ERROR", "nothing for the operation to do")
	assert.Equal(t, 1, strings.Count(out, "level=WARN"))
	assert.Contains(t, out, "run log")
	assert.Contains(t, out, "tables=[messages]")
	assert.Contains(t, out, "tables_unreadable=[]")
	assert.NotContains(t, out, "garbage-startup-51d0")

	// An unparseable value in the four-digit shape (the date does not
	// exist) leaves the table unreadable: an error that names the table
	// in tables_unreadable, still pointing at the run log.
	mk("feb30", "2026-02-30 14:45:00 +0545 +0545")
	rep, _ = runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
	require.Equal(t, 2, rep.Unparseable)
	require.Zero(t, rep.Rewritten)
	logs.Reset()
	srv.checkStoredTimestamps(ctx)
	out = logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=ERROR"), out)
	assert.NotContains(t, out, "level=WARN")
	assert.Contains(t, out, "run log")
	assert.Contains(t, out, "tables_unreadable=[messages]")
	assert.NotContains(t, out, "2026-02-30")

	// A parseable leftover in the same table means the operation has work.
	mk("jst", "2026-10-01 13:00:00 +0900 JST")
	logs.Reset()
	srv.checkStoredTimestamps(ctx)
	out = logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=ERROR"))
	assert.NotContains(t, out, "level=WARN")
	assert.Contains(t, out, "tables_unreadable=[messages]")
}

// TestUTCTimestampNormalize_UnreadableProbeMatchesEntReads checks that the
// four-digit probe flags exactly the values that make ent reads fail.
func TestUTCTimestampNormalize_UnreadableProbeMatchesEntReads(t *testing.T) {
	ktm := time.FixedZone("+0545", 5*3600+45*60)
	for _, text := range []string{
		"2026-10-01 09:45:00 +0545 +0545",
		"2026-10-01 09:45:00.125 +0545 +0545 m=+12.5",
		"2026-10-01 06:00:00 +0200 +0200",
		time.Date(2026, 10, 1, 9, 45, 0, 7, ktm).String(),
		"2026-10-01 13:00:00 +0900 JST",
		"2026-10-01 13:00:00.25 +0900 JST m=+0.07",
		"2026-10-01 01:00:00 -0300 -03",
		"2026-10-01T13:00:00+09:00",
		"2026-10-01 04:00:00 +0000 UTC",
	} {
		t.Run(text, func(t *testing.T) {
			_, s, db := newEntWebChatStore(t)
			ctx := context.Background()
			proj := normProject(t, s, "norm-unreadable")
			id := tid("norm-unreadable-msg")
			require.NoError(t, s.CreateMessage(ctx, &store.Message{
				ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
				Msg: "x", Channel: "web", CreatedAt: normBase,
			}))
			execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", text, id)

			_, readErr := s.ListMessages(ctx, store.MessageFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 10})
			chk, err := entadapter.CheckStoredTimestamps(ctx, db, dialect.SQLite)
			require.NoError(t, err)
			flagged := len(chk.Unreadable) == 1 && chk.Unreadable[0] == "messages"
			assert.Equal(t, readErr != nil, flagged, "ent read error: %v; probe: %v", readErr, chk.Unreadable)
		})
	}
}

// A panic in the background check is logged and does not crash the hub.
func TestUTCTimestampNormalize_StartupCheckRecoversFromPanic(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	done := make(chan struct{})
	orig := storedTimestampCheck
	storedTimestampCheck = func(*Server, context.Context) {
		defer close(done)
		panic("boom")
	}
	t.Cleanup(func() { storedTimestampCheck = orig })

	(&Server{}).startStoredTimestampCheck(context.Background())
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the check never ran")
	}
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "timestamp check: recovered from panic")
	}, 10*time.Second, 10*time.Millisecond)
	assert.Contains(t, logs.String(), "panic=boom")
}

// TestUTCTimestampNormalize_StartupCheckRunsInBackground checks that the
// start hook returns at once and the check still logs.
func TestUTCTimestampNormalize_StartupCheckRunsInBackground(t *testing.T) {
	_, s, db := newEntWebChatStore(t)
	ctx := context.Background()
	srv := &Server{store: s}
	proj := normProject(t, s, "norm-startup-bg")
	id := tid("norm-startup-bg-msg")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
		Msg: "x", Channel: "web", CreatedAt: normBase,
	}))
	execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", "2026-10-01 09:45:00 +0545 +0545", id)

	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Hold the check until startStoredTimestampCheck has returned: a
	// synchronous call would block here and fail the test.
	release, started := make(chan struct{}), make(chan struct{})
	orig := storedTimestampCheck
	storedTimestampCheck = func(s *Server, ctx context.Context) {
		close(started)
		<-release
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline, "the background check must have a bounded context")
		orig(s, ctx)
	}
	t.Cleanup(func() { storedTimestampCheck = orig })

	returned := make(chan struct{})
	go func() {
		srv.startStoredTimestampCheck(ctx)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("startStoredTimestampCheck did not return while the check was blocked")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the check never started")
	}
	assert.NotContains(t, logs.String(), "tables_unreadable", "the check ran before it was released")
	close(release)
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "tables_unreadable=[messages]")
	}, 10*time.Second, 10*time.Millisecond)
}
