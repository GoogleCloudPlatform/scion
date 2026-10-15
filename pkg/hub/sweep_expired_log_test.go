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

package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expiredLogFixedNow is the deterministic clock for the expiry-line tests.
var expiredLogFixedNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// renderExpiredPendingLine writes one expiry line with attrs through a JSON
// handler and returns the decoded record.
func renderExpiredPendingLine(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("sweep: expired stuck pending messages", attrs...)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	return rec
}

func expiredLogRecordKeys(rec map[string]any) []string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestExpiredPendingLogAttrs_NamesEachExpiredMessage(t *testing.T) {
	expired := []store.ExpiredPendingMessage{
		{ID: "id-1", Recipient: "agent:alpha", CreatedAt: expiredLogFixedNow.Add(-26*time.Hour - 30*time.Minute)},
		{ID: "id-2", Recipient: "agent:beta", CreatedAt: expiredLogFixedNow.Add(-24*time.Hour - 5*time.Second - 400*time.Millisecond)},
	}
	rec := renderExpiredPendingLine(t, expiredPendingLogAttrs(expired, expiredLogFixedNow, expiredMessageLogIDCap))

	assert.Equal(t, []string{
		"expired", "level", "message_ids", "message_ids_truncated", "msg",
		"pending_durations", "recipients", "time", "ttl",
	}, expiredLogRecordKeys(rec), "exact key set of the expiry line")
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, float64(2), rec["expired"])
	assert.Equal(t, "24h0m0s", rec["ttl"])
	assert.Equal(t, []any{"id-1", "id-2"}, rec["message_ids"])
	assert.Equal(t, []any{"agent:alpha", "agent:beta"}, rec["recipients"])
	assert.Equal(t, []any{"26h30m0s", "24h0m5s"}, rec["pending_durations"])
	assert.Equal(t, false, rec["message_ids_truncated"])
}

func TestExpiredPendingLogAttrs_CapsNamedMessages(t *testing.T) {
	expired := []store.ExpiredPendingMessage{
		{ID: "id-1", Recipient: "agent:a", CreatedAt: expiredLogFixedNow.Add(-27 * time.Hour)},
		{ID: "id-2", Recipient: "agent:b", CreatedAt: expiredLogFixedNow.Add(-26 * time.Hour)},
		{ID: "id-3", Recipient: "agent:c", CreatedAt: expiredLogFixedNow.Add(-25 * time.Hour)},
	}
	rec := renderExpiredPendingLine(t, expiredPendingLogAttrs(expired, expiredLogFixedNow, 2))

	assert.Equal(t, float64(3), rec["expired"], "count is the full number expired")
	assert.Equal(t, []any{"id-1", "id-2"}, rec["message_ids"])
	assert.Equal(t, []any{"agent:a", "agent:b"}, rec["recipients"])
	assert.Equal(t, []any{"27h0m0s", "26h0m0s"}, rec["pending_durations"])
	assert.Equal(t, true, rec["message_ids_truncated"])
}

func TestExpiredPendingLogAttrs_CapConstant(t *testing.T) {
	assert.Equal(t, 20, expiredMessageLogIDCap)
}
