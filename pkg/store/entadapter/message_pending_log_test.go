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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingLines returns the entered-pending JSON records (and raw lines) in buf.
func pendingLines(t *testing.T, buf *bytes.Buffer) ([]map[string]any, []string) {
	t.Helper()
	var recs []map[string]any
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &rec))
		if rec["msg"] == store.MessageEnteredPendingLogMsg {
			recs = append(recs, rec)
			lines = append(lines, sc.Text())
		}
	}
	require.NoError(t, sc.Err())
	return recs, lines
}

// TestCreateMessage_LogsSchemaDefaultPending pins the store-level
// entered-pending line (ptone/scion#4827): a writer that leaves
// DispatchState empty gets the schema default "pending", and that row is
// logged with message_id, sender, recipient, type (bounded to the enum)
// and creation_path only — never
// the body. A row given an explicit state is not logged here.
func TestCreateMessage_LogsSchemaDefaultPending(t *testing.T) {
	var buf bytes.Buffer
	s := &MessageStore{
		client:        enttest.NewClient(t),
		pendingLogger: slog.New(slog.NewJSONHandler(&buf, nil)),
	}
	ctx := context.Background()
	projectID := uuid.NewString()

	const body = "store-default-body-must-not-appear-7d1e"
	defaulted := newTestMessage(projectID, "agent-1")
	defaulted.Msg = body
	defaulted.Type = "caller free text 4827"
	require.NoError(t, s.CreateMessage(ctx, defaulted))
	require.Equal(t, store.MessageDispatchPending, defaulted.DispatchState)

	for _, state := range []string{store.MessageDispatchPending, store.MessageDispatchDispatched} {
		explicit := newTestMessage(projectID, "agent-1")
		explicit.DispatchState = state
		require.NoError(t, s.CreateMessage(ctx, explicit))
	}

	recs, lines := pendingLines(t, &buf)
	require.Len(t, recs, 1, "only the schema-default row is logged by the store")
	rec := recs[0]
	assert.Equal(t, "WARN", rec["level"])
	assert.Equal(t, defaulted.ID, rec["message_id"])
	assert.Equal(t, "user:alice", rec["sender"])
	assert.Equal(t, "agent:coder", rec["recipient"])
	assert.Equal(t, store.MessagePendingPathStoreDefault, rec["creation_path"])
	assert.Equal(t, messages.LogTypeOther, rec["type"], "a type outside the enum is logged as other")
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"creation_path", "level", "message_id", "msg", "recipient", "sender", "time", "type"}, keys)
	assert.NotContains(t, lines[0], body, "the message body must never be logged")
}

// TestListStuckPendingMessageIDs pins the ID lookup behind the stuck-pending
// sweep line: same filter as CountStuckPendingMessages, oldest first, capped
// at limit, and nothing for a non-positive limit.
func TestListStuckPendingMessageIDs(t *testing.T) {
	cs := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()
	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	mk := func(recipient, state string, age time.Duration) string {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "agent:s", Recipient: recipient, Msg: "m",
			DispatchState: state, CreatedAt: time.Now().Add(-age),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		return m.ID
	}
	oldest := mk("agent:a", store.MessageDispatchPending, 30*time.Minute)
	middle := mk("agent:b", store.MessageDispatchPending, 20*time.Minute)
	newest := mk("agent:c", store.MessageDispatchPending, 10*time.Minute)
	mk("agent:d", store.MessageDispatchPending, time.Minute)  // not stuck yet
	mk("user:u", store.MessageDispatchPending, time.Hour)     // not agent-addressed
	mk("agent:e", store.MessageDispatchDispatched, time.Hour) // not pending
	cutoff := time.Now().Add(-5 * time.Minute)

	count, err := cs.CountStuckPendingMessages(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 3, count)

	ids, err := cs.ListStuckPendingMessageIDs(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, middle, newest}, ids)

	ids, err = cs.ListStuckPendingMessageIDs(ctx, cutoff, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, middle}, ids)

	ids, err = cs.ListStuckPendingMessageIDs(ctx, cutoff, 0)
	require.NoError(t, err)
	assert.Empty(t, ids)
}
