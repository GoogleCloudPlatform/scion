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

package handlers

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"google.golang.org/protobuf/encoding/protowire"
	_ "modernc.org/sqlite"
)

// TestAntigravityLiveTrack1_OnePostInvocationPlusOneGenMetadata proves the
// single-source calls/tokens invariant:
//   - 1 Antigravity PostInvocation hook event -> calls=1, no tokens
//   - 1 decoded gen_metadata row from the live SQLite tailer -> calls=0, expected tokens
//   - Combined Track 1 Hub ingest totals: calls=1 and expected tokens exactly once.
func TestAntigravityLiveTrack1_OnePostInvocationPlusOneGenMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCION_AGENT_ID", "agent-agy-e2e")
	t.Setenv("SCION_PROJECT_ID", "proj-agy-e2e")
	t.Setenv("SCION_LAUNCH_ID", "run-agy-e2e")
	t.Setenv("SCION_HARNESS", "antigravity")
	t.Setenv("SCION_USAGE_SOURCE", telemetrycontract.UsageSourceSQLiteNative)
	t.Setenv("SCION_USAGE_HUB_INGEST", "1")

	hubDir := telemetry.HubAnalyticsOutboxDirFor(home)
	hubOutbox, err := telemetry.OpenAnalyticsOutbox(hubDir)
	if err != nil {
		t.Fatalf("open hub outbox: %v", err)
	}
	// Mark Hub outbox active on disk so the hook-path lazy producer writes to it
	// (matching sciontool init's Hub sink activation).
	if err := os.WriteFile(filepath.Join(hubDir, "active"), []byte("1\n"), 0o600); err != nil {
		t.Fatalf("write active marker: %v", err)
	}

	// 1. Fire one Antigravity PostInvocation (model-end) through TelemetryHandler.
	handler := NewTelemetryHandler(nil, nil, nil)
	handler.Analytics = telemetry.NewLazyHookUsageAnalyticsForSource(
		home, false, nil, telemetrycontract.UsageSourceSQLiteNative,
	)
	postInvEvent := &hooks.Event{
		Name:    hooks.EventModelEnd,
		RawName: "PostInvocation",
		Dialect: "antigravity",
		Data: hooks.EventData{
			SessionID: "conv-e2e",
			Model:     "gemini-4-neon",
			Raw: map[string]interface{}{
				"conversationId":  "conv-e2e",
				"initialNumSteps": float64(1),
				"invocationNum":   float64(1),
				"modelName":       "gemini-4-neon",
			},
		},
	}
	if err := handler.Handle(postInvEvent); err != nil {
		t.Fatalf("TelemetryHandler.Handle(PostInvocation): %v", err)
	}

	// 2. Write one gen_metadata row into the conversation DB and run the live tailer.
	dbPath := filepath.Join(telemetry.AgyConversationsDir(home), "conv-e2e.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir conversations: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER);
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, metadata BLOB);
	`); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	var usagePB []byte
	usagePB = protowire.AppendTag(usagePB, 1, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 1466)
	usagePB = protowire.AppendTag(usagePB, 2, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 1200) // input (exclusive)
	usagePB = protowire.AppendTag(usagePB, 3, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 350) // output (inclusive of reasoning)
	usagePB = protowire.AppendTag(usagePB, 4, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 50) // cache_write
	usagePB = protowire.AppendTag(usagePB, 5, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 400) // cache_read
	usagePB = protowire.AppendTag(usagePB, 9, protowire.VarintType)
	usagePB = protowire.AppendVarint(usagePB, 100) // reasoning
	usagePB = protowire.AppendTag(usagePB, 11, protowire.BytesType)
	usagePB = protowire.AppendString(usagePB, "e2e-req-1")

	var innerPB []byte
	innerPB = protowire.AppendTag(innerPB, 3, protowire.VarintType)
	innerPB = protowire.AppendVarint(innerPB, 1466)
	innerPB = protowire.AppendTag(innerPB, 4, protowire.BytesType)
	innerPB = protowire.AppendBytes(innerPB, usagePB)
	innerPB = protowire.AppendTag(innerPB, 19, protowire.BytesType)
	innerPB = protowire.AppendString(innerPB, "gemini-4-neon")

	var genPB []byte
	genPB = protowire.AppendTag(genPB, 1, protowire.BytesType)
	genPB = protowire.AppendBytes(genPB, innerPB)

	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, genPB, len(genPB)); err != nil {
		t.Fatalf("insert gen_metadata: %v", err)
	}

	tailer := telemetry.NewAntigravitySQLiteTailer(telemetry.AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         telemetry.NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative),
		DetectVersion:     func(context.Context) string { return "1.5.18" },
	})
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("tailer.PollOnce: %v", err)
	}
	// A second poll must be a no-op (no duplicate tokens).
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("tailer.PollOnce #2: %v", err)
	}

	// 3. Drain Hub outbox through AnalyticsDrainer + HubUsageSink and verify combined totals.
	var received []telemetrycontract.UsageEvent
	sink := telemetry.NewHubUsageSink(hubOutbox, telemetry.HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			received = append(received, req.Events...)
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID: "run-agy-e2e",
		HubID: "hub-e2e",
	})
	drainer := telemetry.NewHubAnalyticsDrainer(hubOutbox, sink, func(context.Context) (string, error) {
		return "hub-e2e", nil
	})
	drainer.RunOnce(context.Background())

	if len(received) != 2 {
		t.Fatalf("received %d events, want 2 (1 PostInvocation call + 1 SQLite token event)", len(received))
	}

	var calls, input, output, cacheRead, cacheWrite, reasoning int64
	for _, ev := range received {
		if err := ev.Validate(); err != nil {
			t.Errorf("event %s invalid: %v", ev.EventID, err)
		}
		if ev.UsageSource != telemetrycontract.UsageSourceSQLiteNative {
			t.Errorf("usage_source = %q, want sqlite_native", ev.UsageSource)
		}
		calls += ev.Calls
		if ev.Input != nil {
			input += *ev.Input
		}
		if ev.Output != nil {
			output += *ev.Output
		}
		if ev.CacheRead != nil {
			cacheRead += *ev.CacheRead
		}
		if ev.CacheWrite != nil {
			cacheWrite += *ev.CacheWrite
		}
		if ev.Reasoning != nil {
			reasoning += *ev.Reasoning
		}
	}

	if calls != 1 {
		t.Errorf("total calls = %d, want 1", calls)
	}
	if input != 1200 || output != 350 || cacheRead != 400 || cacheWrite != 50 || reasoning != 100 {
		t.Errorf("total tokens = (in=%d out=%d cr=%d cw=%d reas=%d), want (1200, 350, 400, 50, 100)",
			input, output, cacheRead, cacheWrite, reasoning)
	}
}
