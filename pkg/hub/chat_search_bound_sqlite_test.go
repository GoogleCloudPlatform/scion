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
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chat search isolate-and-bound tests (ptone/scion#7586; design v3 sections
// 9.9 and 15) on SQLite. The Postgres counterparts run in the T1 Postgres
// jobs (chat_search_bound_postgres_test.go).

// TestSearchChatMessages_LiteralWildcards checks design 9.9: %, _ and \ in
// the term match literally, a term of "%" or "__" returns only messages
// holding those characters, and a term ending in a single backslash matches
// literally without an error.
func TestSearchChatMessages_LiteralWildcards(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	msgs := map[string]string{
		"pct":    "50% off today",
		"dogs":   "50 dogs today",
		"under":  "snake a_b case",
		"axb":    "snake axb case",
		"dunder": "x__y marker",
		"bs":     `path ab\ end`,
		"bs2":    `ab\% literal`,
		"plain":  "nothing special",
	}
	i := 0
	for id, m := range msgs {
		insertTestMessage(t, db, id, "proj-1", "topic-1", "user:a", m, now.Add(time.Duration(i)*time.Second))
		i++
	}
	cases := []struct {
		term string
		want []string
	}{
		{"50%", []string{"pct"}},
		{"%", []string{"pct", "bs2"}},
		{"a_b", []string{"under"}},
		{"__", []string{"dunder"}},
		{`ab\`, []string{"bs", "bs2"}},
		{`b\%`, []string{"bs2"}},
		{`\`, []string{"bs", "bs2"}},
	}
	for _, tc := range cases {
		got := searchIDs(t, wcs, ChatSearchFilter{Query: tc.term, ProjectID: "proj-1"})
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		assert.Equal(t, want, got, "term %q", tc.term)
	}
}

func TestEscapeLikeTerm(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain",
		"50%":   `50\%`,
		"a_b":   `a\_b`,
		`ab\`:   `ab\\`,
		`\%`:    `\\\%`,
		`%_\`:   `\%\_\\`,
		"":      "",
		"日本_語%": `日本\_語\%`,
	} {
		assert.Equal(t, want, escapeLikeTerm(in), "escapeLikeTerm(%q)", in)
	}
}

// TestSearchChatMessages_DeletedNotFound checks design 9.9 item 1 in the
// real delete order: soft-delete, then blank msg, then the retention purge.
// The deleted message must not match its old text after each step.
func TestSearchChatMessages_DeletedNotFound(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	insertTestMessage(t, db, "del", "proj-1", "topic-1", "user:a", "the secret plan", now)
	insertTestMessage(t, db, "keep", "proj-1", "topic-1", "user:a", "a public plan", now.Add(time.Second))
	f := ChatSearchFilter{Query: "secret plan", ProjectID: "proj-1"}
	require.Equal(t, []string{"del"}, searchIDs(t, wcs, f), "control: found before delete")

	require.NoError(t, wcs.SetMessageDeleted(ctx, "del", now))
	assert.Empty(t, searchIDs(t, wcs, f), "after soft-delete")
	require.NoError(t, wcs.UpdateMessageContent(ctx, "del", ""))
	assert.Empty(t, searchIDs(t, wcs, f), "after blanking")
	_, err := db.Exec(`DELETE FROM messages WHERE id = ?`, "del")
	require.NoError(t, err)
	assert.Empty(t, searchIDs(t, wcs, f), "after purge")
	assert.Equal(t, []string{"keep"}, searchIDs(t, wcs, ChatSearchFilter{Query: "public plan", ProjectID: "proj-1"}))
}

// TestSearchChatMessages_DeletedNotFoundWhenBlankingFails checks design 9.9
// item 2: with the blanking step failed (the text still in messages.msg),
// the soft-delete condition alone excludes the message.
func TestSearchChatMessages_DeletedNotFoundWhenBlankingFails(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	insertTestMessage(t, db, "del", "proj-1", "topic-1", "user:a", "the secret plan", now)
	require.NoError(t, wcs.SetMessageDeleted(context.Background(), "del", now))
	var msg string
	require.NoError(t, db.QueryRow(`SELECT msg FROM messages WHERE id = 'del'`).Scan(&msg))
	require.Equal(t, "the secret plan", msg, "fixture: the text was not blanked")
	assert.Empty(t, searchIDs(t, wcs, ChatSearchFilter{Query: "secret", ProjectID: "proj-1"}))
}

// TestSearchChatMessages_EditedText checks design 9.9 item 3: an edited
// message is found by its new text and not by its old text.
func TestSearchChatMessages_EditedText(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	insertTestMessage(t, db, "ed", "proj-1", "topic-1", "user:a", "original wording", time.Now().UTC())
	require.NoError(t, wcs.UpdateMessageContent(context.Background(), "ed", "revised wording"))
	assert.Empty(t, searchIDs(t, wcs, ChatSearchFilter{Query: "original", ProjectID: "proj-1"}))
	assert.Equal(t, []string{"ed"}, searchIDs(t, wcs, ChatSearchFilter{Query: "revised", ProjectID: "proj-1"}))
}

// legacySearchIDs runs the search query of the previous release (an
// unescaped LIKE with no soft-delete join) for the differential test.
func legacySearchIDs(t *testing.T, db *sql.DB, term, projectID string, limit int) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM messages
 WHERE channel = 'web' AND msg LIKE '%' || ? || '%' AND project_id = ?
 ORDER BY created DESC, id DESC LIMIT ?`, term, projectID, limit)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// TestSearchChatMessages_DifferentialAgainstPreviousRelease checks
// acceptance 15.4: for terms without %, _ or \, results (set and order)
// equal the previous release's query on the same fixture, including paging.
func TestSearchChatMessages_DifferentialAgainstPreviousRelease(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	rng := rand.New(rand.NewSource(7586))
	words := []string{"alpha", "Beta", "gamma", "délta", "日本語", "emoji🙂", "plan", "PLAN", "meeting", "notes"}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 600; i++ {
		var b strings.Builder
		for j := 0; j < 1+rng.Intn(6); j++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		// Some rows share a timestamp, so the id tiebreak is exercised.
		insertTestMessage(t, db, fmt.Sprintf("m%04d", i), "proj-1", "topic-1", "user:a", b.String(),
			base.Add(time.Duration(i/3)*time.Second))
	}
	for _, term := range []string{"alpha", "beta", "PLAN", "délta", "日本", "🙂", "ta ga", "zz-absent", "me"} {
		want := legacySearchIDs(t, db, term, "proj-1", 1000)
		// Page through the new search with a small page size.
		var got []string
		cursor := ""
		for {
			res, next, err := wcs.SearchChatMessages(context.Background(),
				ChatSearchFilter{Query: term, ProjectID: "proj-1", Limit: 37, Cursor: cursor})
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

// newReaderBackedSearchStore builds a file-backed SQLite hub database, a
// web chat store whose searches run on the read-only search handle
// (entc.OpenSQLiteSearchReader), and returns the store and the main handle.
func newReaderBackedSearchStore(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	client, err := entc.OpenSQLite("file:"+path+"?cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	main := entc.SQLDB(client.Driver())
	require.NotNil(t, main)
	reader, err := entc.OpenSQLiteSearchReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	wcs := NewWebChatStore(main, "sqlite", WithSearchDB(reader))
	require.NoError(t, wcs.Init())
	_, err = main.Exec(`CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY, project_id TEXT NOT NULL, sender TEXT NOT NULL DEFAULT '',
    msg TEXT NOT NULL DEFAULT '', channel TEXT, thread_id TEXT, created DATETIME NOT NULL)`)
	require.NoError(t, err)
	return wcs, main
}

// TestSearchChatMessages_ReadOnlyHandle checks design 6.1: with WithSearchDB
// the search reads committed rows through the read-only handle, keeps the
// visibility predicates, and a timed-out search fails with an error that
// matches context.DeadlineExceeded and leaves the handle usable.
func TestSearchChatMessages_ReadOnlyHandle(t *testing.T) {
	wcs, main := newReaderBackedSearchStore(t)
	now := time.Now().UTC()
	insertTestMessage(t, main, "p1", "proj-1", "topic-1", "user:a", "needle in proj-1", now)
	insertTestMessage(t, main, "p2", "proj-2", "topic-2", "user:a", "needle in proj-2", now.Add(time.Second))
	insertTestMessage(t, main, "dm", "proj-1", "dm:agent:x:user:other", "user:a", "needle in a dm", now.Add(2*time.Second))

	sq := wcs.(*sqliteWebChatStore)
	require.NotNil(t, sq.searchDB, "the store must search on the reader")
	assert.Equal(t, []string{"p1"}, searchIDs(t, wcs, ChatSearchFilter{Query: "needle", ProjectID: "proj-1", DMParticipantUserID: "me"}))
	assert.Equal(t, []string{"p2", "p1"}, searchIDs(t, wcs, ChatSearchFilter{Query: "needle", ProjectIDs: []string{"proj-1", "proj-2"}, DMParticipantUserID: "me"}))

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, _, err := wcs.SearchChatMessages(expired, ChatSearchFilter{Query: "needle", ProjectID: "proj-1"})
	require.Error(t, err)
	assert.True(t, isChatSearchTimeout(err), "error must classify as a timeout: %v", err)
	assert.Equal(t, []string{"p1"}, searchIDs(t, wcs, ChatSearchFilter{Query: "needle", ProjectID: "proj-1", DMParticipantUserID: "me"}), "reusable after the timeout")
}

func TestSearchGate(t *testing.T) {
	g := newSearchGate(2)
	relA, ok := g.tryAcquire("alice")
	require.True(t, ok)
	_, ok = g.tryAcquire("alice")
	assert.False(t, ok, "a second search by the same principal is refused")
	relB, ok := g.tryAcquire("bob")
	require.True(t, ok, "another principal is not blocked by alice's search")
	_, ok = g.tryAcquire("carol")
	assert.False(t, ok, "the global cap is honoured")
	relA()
	relA() // idempotent
	relC, ok := g.tryAcquire("carol")
	require.True(t, ok, "a released slot is reusable")
	_, ok = g.tryAcquire("dave")
	assert.False(t, ok, "the double release did not free a second slot")
	relB()
	relC()
	assert.Len(t, g.slots, 0)
	assert.Empty(t, g.inFlight)
}

// setupSearchServer returns a server with a web chat store, alice and bob,
// and a project both can read, holding one matching web message.
func setupSearchServer(t *testing.T) (*Server, WebChatStore, *store.User, *store.User, *store.Project) {
	t.Helper()
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	wcs := NewWebChatStore(dbp.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	require.NoError(t, s.CreateMessage(context.Background(), &store.Message{
		ID:        tid("msg-needle"),
		ProjectID: project.ID,
		Sender:    "user:x",
		Recipient: "agent:y",
		Msg:       "needle text",
		Type:      "instruction",
		Channel:   "web",
		ThreadID:  "topic-x",
		CreatedAt: time.Now().UTC(),
	}))
	return srv, wcs, alice, bob, project
}

func TestChatSearch_QueryLengthLimit(t *testing.T) {
	srv, _, alice, _, project := setupSearchServer(t)
	base := "/api/v1/chat/search?projectId=" + project.ID + "&q="
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, base+url.QueryEscape(strings.Repeat("é", ChatSearchMaxQueryRunes)), nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, base+url.QueryEscape(strings.Repeat("é", ChatSearchMaxQueryRunes+1)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "at most 256 characters")
}

// TestChatSearch_PerPrincipalCap checks design 9.9: a second concurrent
// search by the same principal gets 429 with Retry-After: 1, and another
// principal is not blocked by it.
func TestChatSearch_PerPrincipalCap(t *testing.T) {
	srv, wcs, alice, bob, project := setupSearchServer(t)
	release, ok := srv.chatSearchGateFor(wcs).tryAcquire(alice.ID)
	require.True(t, ok)
	path := "/api/v1/chat/search?q=needle&projectId=" + project.ID

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "SEARCH_BUSY")

	// bob cannot read alice's project; his unscoped search still passes
	// the gate and answers 200.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/search?q=needle", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "bob is not blocked by alice: %s", rec.Body.String())

	release()
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// blockingSearchStore wraps a WebChatStore; its search waits for the
// context to end, as an interrupted statement does, and returns its error.
type blockingSearchStore struct {
	WebChatStore
}

func (b blockingSearchStore) SearchChatMessages(ctx context.Context, _ ChatSearchFilter) ([]ChatSearchResult, string, error) {
	<-ctx.Done()
	return nil, "", searchError(ctx, "search messages", ctx.Err())
}

// TestChatSearch_TimeoutIs503 checks design 9.9: a timed-out search returns
// 503 SEARCH_TIMEOUT with no partial results, within the limit plus a
// margin, and releases its gate slot.
func TestChatSearch_TimeoutIs503(t *testing.T) {
	srv, wcs, alice, _, project := setupSearchServer(t)
	srv.config.ChatSearchTimeout = 50 * time.Millisecond
	srv.SetWebChatStore(blockingSearchStore{wcs})
	start := time.Now()
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/search?q=needle&projectId="+project.ID, nil)
	elapsed := time.Since(start)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "SEARCH_TIMEOUT")
	assert.Contains(t, rec.Body.String(), "Search took too long. Try a longer phrase or search one space.")
	assert.NotContains(t, rec.Body.String(), "results")
	// A loose bound for default CI (loaded runners, -race); the tight
	// limit-plus-200-ms check is L2 in the chatsearchbench harness.
	assert.Less(t, elapsed, 50*time.Millisecond+2*time.Second)

	srv.SetWebChatStore(wcs)
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/search?q=needle&projectId="+project.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the slot was released: %s", rec.Body.String())
}

// TestChatSearch_SlowLogCarriesNoTermOrIdentifier checks acceptance 15.6:
// the slow-search line (here written for a timed-out search) carries the
// shape only, never the term, a conversation key, a user ID or a project ID.
func TestChatSearch_SlowLogCarriesNoTermOrIdentifier(t *testing.T) {
	srv, wcs, alice, _, project := setupSearchServer(t)
	srv.config.ChatSearchTimeout = 20 * time.Millisecond
	srv.SetWebChatStore(blockingSearchStore{wcs})
	buf := captureSlog(t)
	const term = "zq-distinctive-term"
	logged := 0
	for _, scope := range []string{"&projectId=" + project.ID, "&key=" + url.QueryEscape("dm:user:"+alice.ID+":user:"+tid("peer")), ""} {
		buf.Reset()
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/search?q="+term+scope, nil)
		if rec.Code != http.StatusServiceUnavailable {
			continue // a scope refused before the search (e.g. a DM key check) writes no search line
		}
		logged++
		out := buf.String()
		require.Contains(t, out, "chat search slow", "scope %q", scope)
		assert.Contains(t, out, "term_len=19")
		for _, secret := range []string{term, alice.ID, project.ID, "dm:user"} {
			assert.NotContains(t, out, secret, "scope %q leaked %q", scope, secret)
		}
	}
	// Positive control: the project scope always reaches the search, so a
	// pass cannot come from every scope being skipped.
	require.GreaterOrEqual(t, logged, 1, "no scope reached the search")
}

// TestRecordChatSearch_OKFastIsSilent checks that a fast successful search
// writes no log line, nor do a busy refusal or a canceled search (a debug
// line at most), and that a slow search does.
func TestRecordChatSearch_OKFastIsSilent(t *testing.T) {
	srv, _ := testServer(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv.recordChatSearch(context.Background(), logger, chatSearchShape{outcome: chatSearchOutcomeOK, duration: time.Millisecond})
	srv.recordChatSearchBusy(context.Background(), chatSearchShape{backend: "sqlite"})
	srv.recordChatSearch(context.Background(), logger, chatSearchShape{outcome: chatSearchOutcomeCanceled, duration: 5 * time.Second})
	assert.Empty(t, buf.String())
	srv.recordChatSearch(context.Background(), logger, chatSearchShape{outcome: chatSearchOutcomeOK, duration: time.Second})
	assert.Contains(t, buf.String(), "chat search slow")
}

// TestRecordChatSearch_SlowLineCarriesNoRequestID checks round-1 item R1-08:
// the slow-search line carries no request id even when the request context
// has one and the logger is the hub's request-id handler. A positive control
// shows that handler does add the id to a line logged with the context.
func TestRecordChatSearch_SlowLineCarriesNoRequestID(t *testing.T) {
	srv, _ := testServer(t)
	var buf bytes.Buffer
	logger := slog.New(logging.NewRequestIDHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := logging.ContextWithRequestMeta(context.Background(), &logging.RequestMeta{RequestID: "req-zq-7586"})

	logger.WarnContext(ctx, "control")
	require.Contains(t, buf.String(), "req-zq-7586", "control: the handler adds the request id to a line with the context")
	buf.Reset()

	srv.recordChatSearch(ctx, logger, chatSearchShape{outcome: chatSearchOutcomeTimeout, duration: 3 * time.Second})
	out := buf.String()
	require.Contains(t, out, "chat search slow")
	assert.NotContains(t, out, "req-zq-7586")
	assert.NotContains(t, out, logging.AttrRequestID)
}

func TestEffectiveChatSearchCap(t *testing.T) {
	for _, tc := range []struct {
		configured int
		backend    string
		pool       int
		want       int
		clamped    bool
	}{
		{2, "postgres", 5, 1, true},
		{2, "postgres", 8, 2, false},
		{2, "postgres", 20, 2, false},
		{8, "postgres", 20, 5, true},
		{0, "postgres", 5, 1, true}, // the default (2) clamped at the default pool
		{0, "postgres", 0, 1, true}, // an unknown pool reads as the default 5
		{2, "postgres", 3, 1, true}, // never below 1
		{0, "sqlite", 0, 2, false},  // the default on SQLite
		{8, "sqlite", 1, 8, false},  // no clamp on SQLite
	} {
		got, clamped := EffectiveChatSearchCap(tc.configured, tc.backend, tc.pool)
		assert.Equal(t, tc.want, got, "%+v", tc)
		assert.Equal(t, tc.clamped, clamped, "%+v", tc)
	}
}

func TestChatSearchCapStartupLine(t *testing.T) {
	line, ok := ChatSearchCapStartupLine(2, "postgres", 5)
	require.True(t, ok)
	assert.Equal(t, "Chat search: max_concurrent 2 lowered to 1 (a quarter of the Postgres pool of 5)", line)
	_, ok = ChatSearchCapStartupLine(2, "postgres", 8)
	assert.False(t, ok, "no line when the clamp does not lower the value")
	_, ok = ChatSearchCapStartupLine(8, "sqlite", 1)
	assert.False(t, ok, "no clamp on SQLite")
}

// TestChatSearchGate_PostgresPoolFive checks design v4 erratum E1 on the
// gate: with the default Postgres pool of 5 the hub-wide cap is 1, so while
// one principal holds the slot a second principal is refused (429).
func TestChatSearchGate_PostgresPoolFive(t *testing.T) {
	srv, _ := testServer(t)
	srv.config.DatabaseMaxOpenConns = 5
	g := srv.chatSearchGateFor(&pgWebChatStore{})
	assert.Equal(t, 1, cap(g.slots))
	release, ok := g.tryAcquire("alice")
	require.True(t, ok)
	_, ok = g.tryAcquire("bob")
	assert.False(t, ok, "a second principal is refused while the one Postgres slot is held")
	release()
	_, ok = g.tryAcquire("bob")
	assert.True(t, ok)
}

// stallingListStore makes ListProjects wait for its context to end while
// armed, as an enumeration that crosses the search deadline does.
type stallingListStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s stallingListStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	if s.fault.Active() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Store.ListProjects(ctx, f, o)
}

// countingSearchStore counts SearchChatMessages calls.
type countingSearchStore struct {
	WebChatStore
	calls *atomic.Int64
}

func (c countingSearchStore) SearchChatMessages(ctx context.Context, f ChatSearchFilter) ([]ChatSearchResult, string, error) {
	c.calls.Add(1)
	return c.WebChatStore.SearchChatMessages(ctx, f)
}

// TestChatSearch_EnumerationDeadlineIs503 checks design v4 erratum E1: the
// limit covers project enumeration, and a deadline hit there answers 503
// SEARCH_TIMEOUT (never 500) without calling the store.
func TestChatSearch_EnumerationDeadlineIs503(t *testing.T) {
	srv, s, alice, _, _, _, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) stallingListStore {
		return stallingListStore{Store: inner, fault: f}
	})
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbp.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	var calls atomic.Int64
	srv.SetWebChatStore(countingSearchStore{WebChatStore: wcs, calls: &calls})
	srv.config.ChatSearchTimeout = 50 * time.Millisecond
	fault.Arm()

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/search?q=needle", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "SEARCH_TIMEOUT")
	assert.NotContains(t, rec.Body.String(), "failed to list projects")
	assert.Zero(t, calls.Load(), "the store is not called after an enumeration timeout")
}

// stallingTopicStore returns one topic result fast, then makes GetTopic (the
// name enrichment) wait for its context to end.
type stallingTopicStore struct {
	WebChatStore
	projectID string
}

func (s stallingTopicStore) SearchChatMessages(context.Context, ChatSearchFilter) ([]ChatSearchResult, string, error) {
	return []ChatSearchResult{{MessageID: "m1", ConversationKey: "topic-slow", ProjectID: s.projectID, Content: "needle"}}, "", nil
}

func (s stallingTopicStore) GetTopic(ctx context.Context, _ string) (*WebChatTopic, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestChatSearch_EnrichmentDeadlineKeepsResults checks design v4 erratum
// E1: name enrichment is best-effort, so a deadline that passes there answers
// 200 with the results (the names that did not resolve stay empty).
func TestChatSearch_EnrichmentDeadlineKeepsResults(t *testing.T) {
	srv, wcs, alice, _, project := setupSearchServer(t)
	srv.config.ChatSearchTimeout = 50 * time.Millisecond
	srv.SetWebChatStore(stallingTopicStore{WebChatStore: wcs, projectID: project.ID})
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/search?q=needle&projectId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"messageId":"m1"`)
}

// TestChatSearch_ClientAbortIsCanceled checks round-1 item R1-04: a search
// whose client goes away is recorded as canceled, writes no slow line and no
// error body.
func TestChatSearch_ClientAbortIsCanceled(t *testing.T) {
	srv, wcs, alice, _, project := setupSearchServer(t)
	srv.SetWebChatStore(blockingSearchStore{wcs})
	buf := captureSlog(t)
	token, _, _, err := srv.userTokenService.GenerateTokenPair(alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb)
	require.NoError(t, err)
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/search?q=needle&projectId="+project.ID, nil).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	time.AfterFunc(50*time.Millisecond, cancel)
	srv.Handler().ServeHTTP(rec, req)
	assert.Empty(t, rec.Body.String(), "no error body for a client that went away")
	assert.NotContains(t, buf.String(), "chat search slow")
}
