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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Indexes that serve the inter-agent view: the participant OR lookup
// (recipient_id = X OR sender_id = X) and the legacy project + sender
// lookup.
const (
	messageSenderIDCreatedIDIndex      = "message_sender_id_created_id"
	messageRecipientIDCreatedIDIndex   = "message_recipient_id_created_id"
	messageProjectSenderCreatedIDIndex = "message_project_id_sender_created_id"
)

// TestMessagesParticipantIndexes_AutoMigrate pins that AutoMigrate creates
// the participant and project+sender indexes on messages, with their columns
// in order, on a fresh database and on an existing database that predates
// them. It runs on SQLite and, under -tags integration with
// SCION_TEST_POSTGRES_URL set, on Postgres.
func TestMessagesParticipantIndexes_AutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	want := map[string][]string{
		messageSenderIDCreatedIDIndex:      {"sender_id", "created", "id"},
		messageRecipientIDCreatedIDIndex:   {"recipient_id", "created", "id"},
		messageProjectSenderCreatedIDIndex: {"project_id", "sender", "created", "id"},
	}

	for name, cols := range want {
		require.Equal(t, cols, messageIndexColumns(t, client, name), "a migrated database must have %s", name)
		require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+name, []any{}, nil))
		require.Empty(t, messageIndexColumns(t, client, name))
	}
	require.NoError(t, entc.AutoMigrate(ctx, client))
	for name, cols := range want {
		assert.Equal(t, cols, messageIndexColumns(t, client, name), "AutoMigrate must add %s to an existing database", name)
	}
}

// TestListMessages_InteragentFiltersReturnSameRows pins the rows the two
// inter-agent lookups return. ParticipantID matches sender_id or
// recipient_id in any project, once per row even when both match, in
// (created, id) order in both directions and across keyset pages, with the
// COUNT of the same rows. The project + sender lookup matches only that
// sender text in that project.
func TestListMessages_InteragentFiltersReturnSameRows(t *testing.T) {
	ctx := context.Background()
	s := newTestMessageStore(t)
	projectA, projectB := uuid.NewString(), uuid.NewString()
	agentID := uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	add := func(i int, projectID, sender, senderID, recipientID string) string {
		m := newTestMessage(projectID, recipientID)
		m.Sender = sender
		m.SenderID = senderID
		m.CreatedAt = at.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateMessage(ctx, m))
		return m.ID
	}
	sentA := add(0, projectA, "agent:dm", agentID, "other-1")
	recvB := add(1, projectB, "agent:peer", "other-2", agentID)
	add(2, projectA, "user:alice", "user-1", "other-3")
	sentB := add(3, projectB, "agent:dm", agentID, "other-4")
	legacy := add(4, projectA, "agent:dm", "", "other-5")
	add(5, projectB, "agent:dm", "", "other-6")
	self := add(6, projectA, "agent:dm", agentID, agentID)
	recvA := add(7, projectA, "user:alice", "user-1", agentID)

	ids := func(items []store.Message) []string {
		out := make([]string, 0, len(items))
		for _, m := range items {
			out = append(out, m.ID)
		}
		return out
	}
	newestFirst := []string{recvA, self, sentB, recvB, sentA}

	participant := store.MessageFilter{ParticipantID: agentID}
	all, err := s.ListMessages(ctx, participant, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, newestFirst, ids(all.Items))
	assert.Equal(t, len(newestFirst), all.TotalCount)
	assert.Empty(t, all.NextCursor)

	for _, dir := range []string{"desc", "asc"} {
		want := append([]string(nil), newestFirst...)
		if dir == "asc" {
			slices.Reverse(want)
		}
		var got []string
		cursor := ""
		for range len(want) {
			res, err := s.ListMessages(ctx, participant, store.ListOptions{Limit: 2, Cursor: cursor, SortDir: dir})
			require.NoError(t, err)
			got = append(got, ids(res.Items)...)
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		assert.Equal(t, want, got, "keyset pages, %s", dir)
	}

	bySender, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectA, Sender: "agent:dm"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{self, legacy, sentA}, ids(bySender.Items))
	assert.Equal(t, 3, bySender.TotalCount)
}

// TestListMessages_ParticipantBranchTiesOrderByID pins the merge of the two
// ParticipantID branches when rows from different branches share a created
// time: they order by id, in both directions and across keyset pages of one
// row. The received row (read in the first branch) has the lower id, so a
// merge that kept branch order instead of comparing ids would fail.
func TestListMessages_ParticipantBranchTiesOrderByID(t *testing.T) {
	ctx := context.Background()
	s := newTestMessageStore(t)
	projectID := uuid.NewString()
	agentID := uuid.NewString()
	tie := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)

	add := func(id, senderID, recipientID string, at time.Time) string {
		m := newTestMessage(projectID, recipientID)
		m.ID = id
		m.Sender = "agent:x"
		m.SenderID = senderID
		m.CreatedAt = at
		require.NoError(t, s.CreateMessage(ctx, m))
		return id
	}
	older := add("00000000-0000-4000-8000-000000000003", agentID, "other-0", tie.Add(-time.Second))
	received := add("00000000-0000-4000-8000-000000000001", "other-1", agentID, tie)
	sent := add("00000000-0000-4000-8000-000000000002", agentID, "other-2", tie)
	newer := add("00000000-0000-4000-8000-000000000000", "other-3", agentID, tie.Add(time.Second))

	participant := store.MessageFilter{ParticipantID: agentID}
	all, err := s.ListMessages(ctx, participant, store.ListOptions{})
	require.NoError(t, err)
	got := make([]string, 0, len(all.Items))
	for _, m := range all.Items {
		got = append(got, m.ID)
	}
	newestFirst := []string{newer, sent, received, older}
	assert.Equal(t, newestFirst, got)

	for _, dir := range []string{"desc", "asc"} {
		want := append([]string(nil), newestFirst...)
		if dir == "asc" {
			slices.Reverse(want)
		}
		var paged []string
		cursor := ""
		for range len(want) + 1 {
			res, err := s.ListMessages(ctx, participant, store.ListOptions{Limit: 1, Cursor: cursor, SortDir: dir})
			require.NoError(t, err)
			for _, m := range res.Items {
				paged = append(paged, m.ID)
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		assert.Equal(t, want, paged, "keyset pages of one row, %s", dir)
	}
}

// TestListMessages_InteragentQueriesUseIndexes pins the SQLite plans for the
// exact SQL the inter-agent lookups issue, on a table where one busy
// participant is in a quarter of the rows. The table has no statistics, as
// on a Hub (nothing runs ANALYZE); without statistics SQLite's plan does not
// depend on the table's size, so these plans are the ones a large Hub gets.
// Plans after ANALYZE depend on size and distribution, which a small fixture
// cannot model, so they are not asserted here.
//   - Participant COUNT: a MULTI-INDEX OR over the sender_id and
//     recipient_id indexes, with no table scan.
//   - Participant page: one SELECT per branch, each searching its own index
//     in index order with no sort, so each reads at most limit+1 rows
//     however many rows the participant has.
//   - Project + sender COUNT and page: the project/sender index, with no
//     scan or sort.
func TestListMessages_InteragentQueriesUseIndexes(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	s := NewMessageStore(ent.NewClient(ent.Driver(rec)))

	projectID := uuid.NewString()
	busy := uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Each row's sender and recipient text names its own principal, as on a
	// Hub. The busy participant sends every eighth message and receives the
	// next one.
	for i := 0; i < 400; i++ {
		m := newTestMessage(projectID, uuid.NewString())
		m.Sender, m.SenderID = fmt.Sprintf("user:u%d", i), uuid.NewString()
		m.Recipient = fmt.Sprintf("agent:o%d", i)
		switch i % 8 {
		case 0:
			m.Sender, m.SenderID = "agent:busy", busy
		case 1:
			m.Recipient, m.RecipientID = "agent:busy", busy
		}
		m.CreatedAt = at.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateMessage(ctx, m))
	}

	explain := func(t *testing.T, q recordedQuery) string {
		t.Helper()
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
		return plan
	}

	cases := []struct {
		name      string
		filter    store.MessageFilter
		countWant []string
		pageWant  [][]string // one entry per ordered SELECT, in issue order
	}{
		{
			name:      "participant",
			filter:    store.MessageFilter{ParticipantID: busy},
			countWant: []string{"MULTI-INDEX OR", messageSenderIDCreatedIDIndex, messageRecipientIDCreatedIDIndex},
			pageWant:  [][]string{{messageRecipientIDCreatedIDIndex}, {messageSenderIDCreatedIDIndex}},
		},
		{
			name:      "project_sender",
			filter:    store.MessageFilter{ProjectID: projectID, Sender: "agent:busy"},
			countWant: []string{messageProjectSenderCreatedIDIndex},
			pageWant:  [][]string{{messageProjectSenderCreatedIDIndex}},
		},
	}
	for _, tc := range cases {
		first, err := s.ListMessages(ctx, tc.filter, store.ListOptions{Limit: 20})
		require.NoError(t, err)
		require.NotEmpty(t, first.NextCursor, "the seed must fill more than one page")
		for _, pc := range []struct {
			name string
			opts store.ListOptions
		}{
			{"first_page", store.ListOptions{Limit: 20}},
			{"cursor_page", store.ListOptions{Limit: 20, Cursor: first.NextCursor}},
			{"cursor_page_asc", store.ListOptions{Limit: 20, Cursor: first.NextCursor, SortDir: "asc"}},
		} {
			t.Run(tc.name+"_"+pc.name, func(t *testing.T) {
				rec.mu.Lock()
				rec.queries = nil
				rec.mu.Unlock()
				_, err := s.ListMessages(ctx, tc.filter, pc.opts)
				require.NoError(t, err)

				rec.mu.Lock()
				queries := append([]recordedQuery(nil), rec.queries...)
				rec.mu.Unlock()
				var counts, pages []recordedQuery
				for _, q := range queries {
					switch {
					case strings.Contains(q.sql, "COUNT("):
						counts = append(counts, q)
					case strings.Contains(q.sql, "ORDER BY"):
						pages = append(pages, q)
					}
				}
				require.Len(t, counts, 1, "ListMessages must issue one COUNT")
				require.Len(t, pages, len(tc.pageWant), "ordered SELECTs")

				plan := explain(t, counts[0])
				for _, w := range tc.countWant {
					assert.Contains(t, plan, w, "COUNT plan for %s", counts[0].sql)
				}
				assert.NotContains(t, plan, "SCAN messages", "COUNT plan for %s", counts[0].sql)
				for i, q := range pages {
					plan := explain(t, q)
					for _, w := range tc.pageWant[i] {
						assert.Contains(t, plan, w, "page plan for %s", q.sql)
					}
					assert.NotContains(t, plan, "MULTI-INDEX OR", "page plan for %s", q.sql)
					assert.NotContains(t, plan, "SCAN messages", "page plan for %s", q.sql)
					assert.NotContains(t, plan, "TEMP B-TREE", "page plan for %s", q.sql)
				}
			})
		}
	}
}
