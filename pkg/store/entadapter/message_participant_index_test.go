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
// inter-agent lookups return: ParticipantID matches sender_id or
// recipient_id in any project, newest first, across keyset pages; the
// project + sender lookup matches only that sender text in that project.
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
	recvA := add(6, projectA, "user:alice", "user-1", agentID)

	ids := func(items []store.Message) []string {
		out := make([]string, 0, len(items))
		for _, m := range items {
			out = append(out, m.ID)
		}
		return out
	}

	participant := store.MessageFilter{ParticipantID: agentID}
	all, err := s.ListMessages(ctx, participant, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{recvA, sentB, recvB, sentA}, ids(all.Items))
	assert.Equal(t, 4, all.TotalCount)

	first, err := s.ListMessages(ctx, participant, store.ListOptions{Limit: 3})
	require.NoError(t, err)
	assert.Equal(t, []string{recvA, sentB, recvB}, ids(first.Items))
	require.NotEmpty(t, first.NextCursor)
	rest, err := s.ListMessages(ctx, participant, store.ListOptions{Limit: 3, Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{sentA}, ids(rest.Items))
	assert.Empty(t, rest.NextCursor)

	bySender, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectA, Sender: "agent:dm"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{legacy, sentA}, ids(bySender.Items))
	assert.Equal(t, 2, bySender.TotalCount)
}

// TestListMessages_InteragentQueriesUseIndexes pins the SQLite plans for the
// exact SQL the inter-agent lookups issue, both the COUNT and the page
// SELECT: the participant OR must be a MULTI-INDEX OR over the sender_id and
// recipient_id indexes with no table scan, and the project + sender lookup
// must search its index in index order, with no sort.
func TestListMessages_InteragentQueriesUseIndexes(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	s := NewMessageStore(ent.NewClient(ent.Driver(rec)))

	projectID := uuid.NewString()
	agentID := uuid.NewString()
	m := newTestMessage(projectID, agentID)
	require.NoError(t, s.CreateMessage(ctx, m))

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
		name    string
		filter  store.MessageFilter
		want    []string
		notWant []string
	}{
		{
			name:    "participant",
			filter:  store.MessageFilter{ParticipantID: agentID},
			want:    []string{"MULTI-INDEX OR", messageSenderIDCreatedIDIndex, messageRecipientIDCreatedIDIndex},
			notWant: []string{"SCAN messages"},
		},
		{
			name:    "project_sender",
			filter:  store.MessageFilter{ProjectID: projectID, Sender: "agent:coder"},
			want:    []string{messageProjectSenderCreatedIDIndex},
			notWant: []string{"SCAN messages", "TEMP B-TREE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			_, err := s.ListMessages(ctx, tc.filter, store.ListOptions{Limit: 200})
			require.NoError(t, err)

			rec.mu.Lock()
			queries := append([]recordedQuery(nil), rec.queries...)
			rec.mu.Unlock()
			var count, page int
			for _, q := range queries {
				switch {
				case strings.Contains(q.sql, "COUNT("):
					count++
				case strings.Contains(q.sql, "ORDER BY"):
					page++
				default:
					continue
				}
				plan := explain(t, q)
				for _, w := range tc.want {
					assert.Contains(t, plan, w, "query: %s", q.sql)
				}
				for _, nw := range tc.notWant {
					assert.NotContains(t, plan, nw, "query: %s", q.sql)
				}
			}
			assert.Equal(t, 1, count, "ListMessages must issue one COUNT")
			assert.Equal(t, 1, page, "ListMessages must issue one ordered SELECT")
		})
	}
}
