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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRawMessageCreatedText overwrites a message's stored created TEXT
// directly via the driver, to reproduce rows written with a monotonic-clock
// suffix (see setRawCreatedUpdatedText). SQLite only.
func setRawMessageCreatedText(t *testing.T, s *MessageStore, id, createdText string) {
	t.Helper()
	err := s.client.Driver().Exec(context.Background(),
		"UPDATE messages SET created = ? WHERE id = ?", []any{createdText, id}, nil)
	require.NoError(t, err)
}

// TestListMessages_CreatedFiltersSurviveMonotonicSuffix is the regression
// test for ptone/scion#2553: on SQLite, rows whose stored created text carries
// a monotonic-clock suffix must be filtered by instant against suffix-free
// bound values. It covers the chat "around" shape (After set to an anchor's
// read-back CreatedAt, ascending) and Before, around a three-row tie at one
// instant (two rows with a suffix, one without). Keyset ordering of legacy
// suffixed ties is deliberately not pinned: ListMessages orders on the raw
// column so the indexes serve the order, and leaves legacy rows to the
// utc-timestamp-normalize maintenance operation.
func TestListMessages_CreatedFiltersSurviveMonotonicSuffix(t *testing.T) {
	enttest.SkipOnPostgres(t, "writes SQLite TEXT timestamps (with a monotonic suffix) that only the SQLite driver produces")
	ctx := context.Background()
	s := newTestMessageStore(t)
	projectID := uuid.NewString()

	type row struct{ slug, createdText, id string }
	rows := []row{
		{slug: "anchor", createdText: "2026-01-01 00:00:05.5 +0000 UTC m=+10.1"},
		{slug: "tie-plain", createdText: "2026-01-01 00:00:05.5 +0000 UTC"},
		{slug: "tie-suffix", createdText: "2026-01-01 00:00:05.5 +0000 UTC m=+10.2"},
		{slug: "newer", createdText: "2026-01-01 00:00:06 +0000 UTC"},
		{slug: "newer-suffix", createdText: "2026-01-01 00:00:05.6 +0000 UTC m=+10.3"},
		{slug: "older", createdText: "2026-01-01 00:00:04.9 +0000 UTC m=+9"},
		{slug: "older-plain", createdText: "2026-01-01 00:00:05 +0000 UTC"},
	}
	slugByID := map[string]string{}
	for i := range rows {
		m := newTestMessage(projectID, "agent-ts")
		require.NoError(t, s.CreateMessage(ctx, m))
		setRawMessageCreatedText(t, s, m.ID, rows[i].createdText)
		rows[i].id = m.ID
		slugByID[m.ID] = rows[i].slug
	}
	slugs := func(msgs []store.Message) []string {
		out := make([]string, len(msgs))
		for i, m := range msgs {
			out[i] = slugByID[m.ID]
		}
		return out
	}
	filter := store.MessageFilter{ProjectID: projectID}

	for _, anchorSlug := range []string{"anchor", "tie-plain"} {
		var anchorID string
		for _, r := range rows {
			if r.slug == anchorSlug {
				anchorID = r.id
			}
		}
		anchor, err := s.GetMessage(ctx, anchorID)
		require.NoError(t, err)

		after := filter
		after.After = anchor.CreatedAt
		res, err := s.ListMessages(ctx, after, store.ListOptions{SortDir: "asc", Limit: 50})
		require.NoError(t, err)
		assert.Equal(t, []string{"newer-suffix", "newer"}, slugs(res.Items),
			"anchor %s: After must exclude the anchor and its same-instant ties", anchorSlug)
		assert.Equal(t, 2, res.TotalCount, "anchor %s: count must agree with the page", anchorSlug)

		before := filter
		before.Before = anchor.CreatedAt
		res, err = s.ListMessages(ctx, before, store.ListOptions{SortDir: "asc", Limit: 50})
		require.NoError(t, err)
		assert.Equal(t, []string{"older", "older-plain"}, slugs(res.Items),
			"anchor %s: Before must exclude the anchor and its same-instant ties", anchorSlug)
	}
}

// planRecordingDriver records every statement sent through it (outside a
// transaction), so a test can EXPLAIN the exact SQL a store method built or
// check which statements it issued.
type planRecordingDriver struct {
	dialect.Driver
	mu      sync.Mutex
	queries []recordedQuery
}

type recordedQuery struct {
	sql  string
	args []any
}

func (d *planRecordingDriver) record(query string, args any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, _ := args.([]any)
	d.queries = append(d.queries, recordedQuery{sql: query, args: append([]any(nil), a...)})
}

func (d *planRecordingDriver) Query(ctx context.Context, query string, args, v any) error {
	d.record(query, args)
	return d.Driver.Query(ctx, query, args, v)
}

func (d *planRecordingDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.record(query, args)
	return d.Driver.Exec(ctx, query, args, v)
}

// TestListMessages_TailQueryUsesIndexOrder pins that the conversation+channel
// tail and history reads keep using the (conversation_id, channel, created,
// id) index for their ORDER BY, with and without a created range and on a
// keyset page: the SQLite plan for the exact SQL ListMessages issues must
// not contain a TEMP B-TREE sort, and a created range must be an index
// seek (see the index comment in the message schema, and ptone/scion#2553,
// which must not trade that index away).
func TestListMessages_TailQueryUsesIndexOrder(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	s := NewMessageStore(ent.NewClient(ent.Driver(rec)))

	convID := uuid.NewString()
	projectID := uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		m := newTestMessage(projectID, "agent-plan")
		m.ConversationID = convID
		m.Channel = "web"
		m.CreatedAt = at.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateMessage(ctx, m))
	}
	tail := store.MessageFilter{ConversationID: convID, Channel: "web"}
	page, err := s.ListMessages(ctx, tail, store.ListOptions{Limit: 2, SkipTotalCount: true})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)

	withBefore, withAfter := tail, tail
	withBefore.Before = at.Add(3 * time.Second)
	withAfter.After = at.Add(1 * time.Second)
	// wantSeek, when set, is the created range the index search must use.
	// For after-asc it pins that messageCreatedAfter keeps its raw
	// created > ? term: the normalized term alone filters correctly but
	// loses the seek.
	cases := []struct {
		name     string
		filter   store.MessageFilter
		opts     store.ListOptions
		wantSeek string
	}{
		{"tail", tail, store.ListOptions{Limit: 2, SkipTotalCount: true}, ""},
		{"before", withBefore, store.ListOptions{Limit: 2, SkipTotalCount: true}, "created<?"},
		{"after-asc", withAfter, store.ListOptions{Limit: 2, SortDir: "asc", SkipTotalCount: true}, "created>?"},
		{"cursor-page", tail, store.ListOptions{Limit: 2, Cursor: page.NextCursor, SkipTotalCount: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			_, err := s.ListMessages(ctx, tc.filter, tc.opts)
			require.NoError(t, err)

			rec.mu.Lock()
			var q *recordedQuery
			for i := range rec.queries {
				if strings.Contains(rec.queries[i].sql, "ORDER BY") {
					q = &rec.queries[i]
				}
			}
			rec.mu.Unlock()
			require.NotNil(t, q, "ListMessages must issue an ordered SELECT")

			rows := &entsql.Rows{}
			require.NoError(t, base.Driver().Query(ctx, "EXPLAIN QUERY PLAN "+q.sql, q.args, rows))
			var plan string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan += detail + "\n"
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Contains(t, plan, "message_conversation_id_channel_created_id", "query: %s", q.sql)
			assert.NotContains(t, plan, "TEMP B-TREE FOR ORDER BY", "query: %s", q.sql)
			if tc.wantSeek != "" {
				assert.Contains(t, plan, tc.wantSeek, "the index search must seek on the created range; plan: %s", plan)
			}
		})
	}
}

// TestListMessages_CreatedFiltersAndKeysetTies runs the same filter and
// keyset checks with typed created values only, so it also runs on Postgres
// under -tags integration and pins that the normalized comparison path
// (messageCreatedCmp, messageCreatedIDOrder) keeps plain timestamptz
// semantics there.
func TestListMessages_CreatedFiltersAndKeysetTies(t *testing.T) {
	ctx := context.Background()
	s := newTestMessageStore(t)
	projectID := uuid.NewString()

	tie := time.Date(2026, 1, 1, 0, 0, 5, 500000000, time.UTC)
	created := map[string]time.Time{
		"anchor": tie,
		"tie-1":  tie,
		"tie-2":  tie,
		"newer":  tie.Add(time.Microsecond),
		"older":  tie.Add(-time.Microsecond),
	}
	slugByID := map[string]string{}
	idBySlug := map[string]string{}
	for slug, at := range created {
		m := newTestMessage(projectID, "agent-ts")
		m.CreatedAt = at
		require.NoError(t, s.CreateMessage(ctx, m))
		slugByID[m.ID] = slug
		idBySlug[slug] = m.ID
	}
	slugs := func(msgs []store.Message) []string {
		out := make([]string, len(msgs))
		for i, m := range msgs {
			out[i] = slugByID[m.ID]
		}
		return out
	}
	filter := store.MessageFilter{ProjectID: projectID}

	anchor, err := s.GetMessage(ctx, idBySlug["anchor"])
	require.NoError(t, err)
	after := filter
	after.After = anchor.CreatedAt
	res, err := s.ListMessages(ctx, after, store.ListOptions{SortDir: "asc", Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"newer"}, slugs(res.Items))
	before := filter
	before.Before = anchor.CreatedAt
	res, err = s.ListMessages(ctx, before, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"older"}, slugs(res.Items))

	for _, dir := range []string{"asc", "desc"} {
		want := make([]string, 0, len(created))
		for slug := range created {
			want = append(want, slug)
		}
		sort.Slice(want, func(i, j int) bool {
			ti, tj := created[want[i]], created[want[j]]
			if !ti.Equal(tj) {
				if dir == "asc" {
					return ti.Before(tj)
				}
				return ti.After(tj)
			}
			if dir == "asc" {
				return idBySlug[want[i]] < idBySlug[want[j]]
			}
			return idBySlug[want[i]] > idBySlug[want[j]]
		})
		for _, pageSize := range []int{1, 2, len(created)} {
			opts := store.ListOptions{SortDir: dir, Limit: pageSize, SkipTotalCount: true}
			var got []store.Message
			for i := 0; i <= len(created); i++ {
				page, err := s.ListMessages(ctx, filter, opts)
				require.NoError(t, err)
				got = append(got, page.Items...)
				if page.NextCursor == "" {
					break
				}
				opts.Cursor = page.NextCursor
			}
			assert.Equal(t, want, slugs(got), "dir=%s pageSize=%d", dir, pageSize)
		}
	}
}
