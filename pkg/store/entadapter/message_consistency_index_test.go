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

// Indexes that serve the conversation consistency check's lookups
// (messaging.CheckConversationConsistency).
const (
	messageSenderRecipientCreatedIDIndex = "message_sender_id_recipient_id_created_id"
	messageThreadCreatedIDIndex          = "message_thread_id_created_id"
)

// TestMessagesConsistencyIndexes_AutoMigrate pins that AutoMigrate creates
// the consistency check's indexes, with their columns in order, on a fresh
// database and on an existing database that predates them. It runs on
// SQLite and, under -tags integration with SCION_TEST_POSTGRES_URL set, on
// Postgres.
func TestMessagesConsistencyIndexes_AutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	want := map[string][]string{
		messageSenderRecipientCreatedIDIndex: {"sender_id", "recipient_id", "created", "id"},
		messageThreadCreatedIDIndex:          {"thread_id", "created", "id"},
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

// TestListMessages_ConsistencyCheckLookupsUseIndexes pins the SQLite plans
// of the consistency check's lookups, with the filters and options the
// check sends (divergence.go, CheckConversationConsistency):
//
//   - DM pair, each direction: SenderID + RecipientID, Limit 25,
//     SkipTotalCount. The (sender_id, recipient_id, created, id) index
//     serves the filter and the ORDER BY created, id, so the read stops
//     after limit+1 rows however many messages either side has.
//   - Thread: ThreadID, Limit 50, SkipTotalCount. The (thread_id, created,
//     id) index serves it; (thread_id, channel, created, id) matches the
//     filter but not the order, because the check sets no channel.
//
// Neither lookup may scan the table, sort in a temporary B-tree, or run a
// COUNT. The plans are logged, together with the plans the same queries get
// without the new indexes.
func TestListMessages_ConsistencyCheckLookupsUseIndexes(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	s := NewMessageStore(ent.NewClient(ent.Driver(rec)))

	projectID := uuid.NewString()
	busySender, busyRecipient, peer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// The busy sender writes to many recipients and the busy recipient hears
	// from many senders; only every tenth message is between the pair. The
	// thread spans two channels.
	for i := 0; i < 400; i++ {
		m := newTestMessage(projectID, uuid.NewString())
		m.SenderID = uuid.NewString()
		switch i % 10 {
		case 0:
			m.SenderID, m.RecipientID = busySender, busyRecipient
		case 1:
			m.SenderID, m.RecipientID = busyRecipient, busySender
		case 2, 3, 4:
			m.SenderID = busySender
		case 5, 6, 7:
			m.RecipientID = busyRecipient
		}
		if i%2 == 0 {
			m.ThreadID = "dm:busy"
			m.Channel = []string{"web", "cli"}[i%4/2]
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

	dmOpts := store.ListOptions{Limit: 25, SkipTotalCount: true}
	cases := []struct {
		name   string
		filter store.MessageFilter
		opts   store.ListOptions
		index  string
	}{
		{"pair", store.MessageFilter{SenderID: busySender, RecipientID: busyRecipient}, dmOpts, messageSenderRecipientCreatedIDIndex},
		{"pair_reverse", store.MessageFilter{SenderID: busyRecipient, RecipientID: busySender}, dmOpts, messageSenderRecipientCreatedIDIndex},
		{"thread", store.MessageFilter{ThreadID: "dm:busy"}, store.ListOptions{Limit: 50, SkipTotalCount: true}, messageThreadCreatedIDIndex},
		{"unrelated_pair", store.MessageFilter{SenderID: peer, RecipientID: busyRecipient}, dmOpts, messageSenderRecipientCreatedIDIndex},
	}
	recorded := make(map[string]recordedQuery, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			_, err := s.ListMessages(ctx, tc.filter, tc.opts)
			require.NoError(t, err)

			rec.mu.Lock()
			queries := append([]recordedQuery(nil), rec.queries...)
			rec.mu.Unlock()
			require.Len(t, queries, 1, "one SELECT and no COUNT")
			q := queries[0]
			require.Contains(t, q.sql, "ORDER BY")
			require.NotContains(t, q.sql, "COUNT(")
			recorded[tc.name] = q

			plan := explain(t, q)
			t.Logf("plan for %s:\n%s", tc.name, plan)
			assert.Contains(t, plan, tc.index, "plan for %s", q.sql)
			assert.NotContains(t, plan, "SCAN messages", "plan for %s", q.sql)
			assert.NotContains(t, plan, "TEMP B-TREE", "plan for %s", q.sql)
		})
	}

	// The plans before this change, for the record: drop the new indexes
	// and explain the same statements again.
	for _, name := range []string{messageSenderRecipientCreatedIDIndex, messageThreadCreatedIDIndex} {
		require.NoError(t, base.Driver().Exec(ctx, "DROP INDEX "+name, []any{}, nil))
	}
	for _, name := range []string{"pair", "thread"} {
		q, ok := recorded[name]
		require.True(t, ok, "no recorded query for %s", name)
		plan := explain(t, q)
		t.Logf("plan for %s without the consistency indexes:\n%s", name, plan)
		assert.False(t, strings.Contains(plan, messageSenderRecipientCreatedIDIndex) || strings.Contains(plan, messageThreadCreatedIDIndex),
			"the indexes were dropped: %s", plan)
	}
}
