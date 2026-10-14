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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingLogBuffer is a concurrency-safe sink for a JSON slog handler.
type pendingLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *pendingLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every JSON record whose msg equals msg, plus the raw line
// of each, in write order.
func (b *pendingLogBuffer) records(t *testing.T, msg string) ([]map[string]any, []string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var recs []map[string]any
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		if rec["msg"] == msg {
			recs = append(recs, rec)
			lines = append(lines, line)
		}
	}
	require.NoError(t, sc.Err())
	return recs, lines
}

func newPendingLogCapture() (*slog.Logger, *pendingLogBuffer) {
	b := &pendingLogBuffer{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

func sortedKeys(rec map[string]any) []string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestMessagePendingLog_AgentDM pins the entered-pending line written by
// ExecuteAgentDM when it persists the row "pending" (ptone/scion#4827): one
// line per message, with message_id, sender, recipient and creation_path,
// and nothing else — in particular never the body.
func TestMessagePendingLog_AgentDM(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	logger, logs := newPendingLogCapture()
	srv.messageLog = logger

	const body = "pending-log-body-must-not-appear-3f9c"
	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, body))
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)

	recs, lines := logs.records(t, store.MessageEnteredPendingLogMsg)
	require.Len(t, recs, 1, "exactly one entered-pending line per agent DM")
	rec := recs[0]
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, result.MessageID, rec["message_id"])
	assert.Equal(t, "agent:"+sender.Slug, rec["sender"])
	assert.Equal(t, "agent:"+target.Slug, rec["recipient"])
	assert.Equal(t, pendingPathAgentDM, rec["creation_path"])
	wantKeys := []string{"creation_path", "level", "message_id", "msg", "recipient", "sender", "time"}
	assert.Equal(t, wantKeys, sortedKeys(rec), "the line carries only identifiers and routing fields")
	assert.NotContains(t, lines[0], body, "the message body must never be logged")
}

// TestMessagePendingLog_NotLoggedForOtherStates pins that the helper logs a
// row only when its stored state is pending.
func TestMessagePendingLog_NotLoggedForOtherStates(t *testing.T) {
	logger, logs := newPendingLogCapture()
	srv := &Server{messageLog: logger}
	for _, state := range []string{
		store.MessageDispatchDispatched, store.MessageDispatchDeferred,
		store.MessageDispatchFailed, store.MessageDispatchNoRecipient, "",
	} {
		srv.logMessageEnteredPending(pendingPathAgentDM, &store.Message{ID: uuid.NewString(), DispatchState: state})
	}
	srv.logMessageEnteredPending(pendingPathAgentDM, nil)
	recs, _ := logs.records(t, store.MessageEnteredPendingLogMsg)
	assert.Empty(t, recs)
}

// seedStuckPending creates n agent-addressed rows left pending well past the
// stuck threshold (but inside the expiry TTL), and returns their IDs.
func seedStuckPending(t *testing.T, cs store.Store, n int) []string {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ids := make([]string, 0, n)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "agent:s", Recipient: "agent:r", RecipientID: uuid.NewString(),
			Msg: "stuck", DispatchState: store.MessageDispatchPending,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		ids = append(ids, m.ID)
	}
	return ids
}

func anyStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	require.True(t, ok, "message_ids must be a JSON array, got %T", v)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		require.True(t, ok)
		out = append(out, s)
	}
	return out
}

// TestBrokerMessageSweep_StuckLineNamesMessageIDs pins that the stuck-pending
// sweep line names the stuck rows (ptone/scion#4827).
func TestBrokerMessageSweep_StuckLineNamesMessageIDs(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	logger, logs := newPendingLogCapture()
	srv.agentLifecycleLog = logger
	ids := seedStuckPending(t, cs, 2)

	srv.brokerMessageSweepHandler()(context.Background())

	recs, _ := logs.records(t, "sweep: stuck pending messages detected")
	require.Len(t, recs, 1)
	assert.EqualValues(t, 2, recs[0]["count"])
	assert.Equal(t, ids, anyStrings(t, recs[0]["message_ids"]), "oldest first")
	assert.Equal(t, false, recs[0]["message_ids_truncated"])
}

// TestBrokerMessageSweep_StuckLineCapsMessageIDs pins the cap on the IDs the
// sweep line names: the oldest stuckMessageLogIDCap rows, with
// message_ids_truncated set and count still reporting every stuck row.
func TestBrokerMessageSweep_StuckLineCapsMessageIDs(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	logger, logs := newPendingLogCapture()
	srv.agentLifecycleLog = logger
	require.Equal(t, 20, stuckMessageLogIDCap)
	ids := seedStuckPending(t, cs, stuckMessageLogIDCap+3)

	srv.brokerMessageSweepHandler()(context.Background())

	recs, lines := logs.records(t, "sweep: stuck pending messages detected")
	require.Len(t, recs, 1)
	assert.EqualValues(t, stuckMessageLogIDCap+3, recs[0]["count"])
	assert.Equal(t, ids[:stuckMessageLogIDCap], anyStrings(t, recs[0]["message_ids"]))
	assert.Equal(t, true, recs[0]["message_ids_truncated"])
	for _, id := range ids[stuckMessageLogIDCap:] {
		assert.False(t, strings.Contains(lines[0], id), "IDs past the cap are not on the line")
	}
}
