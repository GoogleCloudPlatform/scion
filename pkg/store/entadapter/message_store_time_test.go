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
	"testing"
	"time"

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

// TestListMessages_CreatedComparisonsSurviveMonotonicSuffix is the regression
// test for ptone/scion#2553: on SQLite, rows whose stored created text carries
// a monotonic-clock suffix must compare and order by instant against
// suffix-free bound values. It covers the chat "around" shape (After set to
// an anchor's read-back CreatedAt, ascending), Before, and keyset walks in
// both directions across a three-row tie at one instant (two rows with a
// suffix, one without).
func TestListMessages_CreatedComparisonsSurviveMonotonicSuffix(t *testing.T) {
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
		{slug: "older", createdText: "2026-01-01 00:00:04.9 +0000 UTC m=+9"},
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

	anchor, err := s.GetMessage(ctx, rows[0].id)
	require.NoError(t, err)

	after := filter
	after.After = anchor.CreatedAt
	res, err := s.ListMessages(ctx, after, store.ListOptions{SortDir: "asc", Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"newer"}, slugs(res.Items), "After must exclude the anchor and its same-instant ties")
	assert.Equal(t, 1, res.TotalCount)

	before := filter
	before.Before = anchor.CreatedAt
	res, err = s.ListMessages(ctx, before, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{"older"}, slugs(res.Items), "Before must exclude the anchor and its same-instant ties")

	// Reference order: created by instant (suffix stripped), then id, both in
	// the walk direction.
	instant := func(text string) time.Time {
		if i := strings.Index(text, " m="); i >= 0 {
			text = text[:i]
		}
		tm, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", text)
		require.NoError(t, err)
		return tm
	}
	for _, dir := range []string{"asc", "desc"} {
		ref := append([]row(nil), rows...)
		sort.Slice(ref, func(i, j int) bool {
			ti, tj := instant(ref[i].createdText), instant(ref[j].createdText)
			if !ti.Equal(tj) {
				if dir == "asc" {
					return ti.Before(tj)
				}
				return ti.After(tj)
			}
			if dir == "asc" {
				return ref[i].id < ref[j].id
			}
			return ref[i].id > ref[j].id
		})
		want := make([]string, len(ref))
		for i, r := range ref {
			want[i] = r.slug
		}

		for _, pageSize := range []int{1, 2, len(rows)} {
			opts := store.ListOptions{SortDir: dir, Limit: pageSize, SkipTotalCount: true}
			var got []store.Message
			for i := 0; i <= len(rows); i++ {
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
