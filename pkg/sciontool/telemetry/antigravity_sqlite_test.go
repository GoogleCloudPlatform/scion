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

package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"google.golang.org/protobuf/encoding/protowire"
	_ "modernc.org/sqlite"
)

// Minimal protobuf builders matching agy 1.5.18's gen_metadata / steps layout
// (same wire tags verified in pkg/trajectory/agydecode).
type testAgyUsage struct {
	modelID    uint64
	input      uint64
	output     uint64
	cacheWrite uint64
	cacheRead  uint64
	reasoning  uint64
	id11       string
	id7        string
}

func encodeTestAgyUsage(u testAgyUsage) []byte {
	var b []byte
	if u.modelID > 0 {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, u.modelID)
	}
	if u.input > 0 {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, u.input)
	}
	if u.output > 0 {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, u.output)
	}
	if u.cacheWrite > 0 {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, u.cacheWrite)
	}
	if u.cacheRead > 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, u.cacheRead)
	}
	if u.id7 != "" {
		b = protowire.AppendTag(b, 7, protowire.BytesType)
		b = protowire.AppendString(b, u.id7)
	}
	if u.reasoning > 0 {
		b = protowire.AppendTag(b, 9, protowire.VarintType)
		b = protowire.AppendVarint(b, u.reasoning)
	}
	if u.id11 != "" {
		b = protowire.AppendTag(b, 11, protowire.BytesType)
		b = protowire.AppendString(b, u.id11)
	}
	return b
}

func encodeTestGenMeta(modelID uint64, modelName string, tsSecs uint64, u testAgyUsage) []byte {
	var d []byte
	if modelID > 0 {
		d = protowire.AppendTag(d, 3, protowire.VarintType)
		d = protowire.AppendVarint(d, modelID)
	}
	ub := encodeTestAgyUsage(u)
	d = protowire.AppendTag(d, 4, protowire.BytesType)
	d = protowire.AppendBytes(d, ub)
	if tsSecs > 0 {
		var ts []byte
		ts = protowire.AppendTag(ts, 1, protowire.VarintType)
		ts = protowire.AppendVarint(ts, tsSecs)
		var f9 []byte
		f9 = protowire.AppendTag(f9, 4, protowire.BytesType)
		f9 = protowire.AppendBytes(f9, ts)
		d = protowire.AppendTag(d, 9, protowire.BytesType)
		d = protowire.AppendBytes(d, f9)
	}
	if modelName != "" {
		d = protowire.AppendTag(d, 19, protowire.BytesType)
		d = protowire.AppendString(d, modelName)
	}
	var root []byte
	root = protowire.AppendTag(root, 1, protowire.BytesType)
	root = protowire.AppendBytes(root, d)
	return root
}

func encodeTestStepMeta(modelID uint64, modelName string, tsSecs uint64, u testAgyUsage) []byte {
	var d []byte
	if tsSecs > 0 {
		var ts []byte
		ts = protowire.AppendTag(ts, 1, protowire.VarintType)
		ts = protowire.AppendVarint(ts, tsSecs)
		d = protowire.AppendTag(d, 8, protowire.BytesType)
		d = protowire.AppendBytes(d, ts)
	}
	ub := encodeTestAgyUsage(u)
	d = protowire.AppendTag(d, 9, protowire.BytesType)
	d = protowire.AppendBytes(d, ub)
	if modelID > 0 || modelName != "" {
		var m []byte
		if modelID > 0 {
			m = protowire.AppendTag(m, 1, protowire.VarintType)
			m = protowire.AppendVarint(m, modelID)
		}
		if modelName != "" {
			m = protowire.AppendTag(m, 12, protowire.BytesType)
			m = protowire.AppendString(m, modelName)
		}
		d = protowire.AppendTag(d, 24, protowire.BytesType)
		d = protowire.AppendBytes(d, m)
	}
	return d
}

func createTestAgyDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	schema := `
		PRAGMA journal_mode=WAL;
		CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER);
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, metadata BLOB);
		CREATE TABLE trajectory_metadata_blob (id INTEGER PRIMARY KEY, data BLOB);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func drainOutboxEvents(t *testing.T, outbox *AnalyticsOutbox) []telemetrycontract.UsageEvent {
	t.Helper()
	_, _ = outbox.PinHubID("hub-test")
	if _, err := outbox.finalize(context.Background(), "hub-test"); err != nil {
		t.Fatalf("finalize outbox: %v", err)
	}
	var events []telemetrycontract.UsageEvent
	for {
		n, err := outbox.DrainBatch(context.Background(), 100, 1<<20, func(_ context.Context, lines [][]byte) (BatchSendOutcome, error) {
			for _, line := range lines {
				var ev telemetrycontract.UsageEvent
				if err := telemetrycontract.Unmarshal(line, &ev); err != nil {
					t.Fatalf("unmarshal usage event: %v", err)
				}
				events = append(events, ev)
			}
			return BatchSendOutcome{}, nil
		})
		if err != nil {
			t.Fatalf("drain outbox: %v", err)
		}
		if n == 0 {
			break
		}
	}
	return events
}

func setTestAgyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_AGENT_ID", "agent-agy-test")
	t.Setenv("SCION_PROJECT_ID", "proj-agy-test")
	t.Setenv("SCION_LAUNCH_ID", "run-agy-test")
	t.Setenv("SCION_HARNESS", "antigravity")
	t.Setenv("SCION_USAGE_SOURCE", telemetrycontract.UsageSourceSQLiteNative)
}

func TestAntigravitySQLiteTailer_GrowthDeltaRestartAndDuplicateSuppression(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-1.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         analytics,
		DetectVersion:     func(context.Context) string { return "1.5.18" },
	})

	// Round 1: initial partial steps row for call-1.
	step0 := encodeTestStepMeta(1466, "gemini-4-neon", 1795700000, testAgyUsage{
		modelID: 1466, input: 100, output: 20, id11: "call-1",
	})
	if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (0, 15, ?)`, step0); err != nil {
		t.Fatalf("insert step0: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	// Round 2: unchanged DB -> fingerprint skip -> 0 new events.
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 2 (unchanged): %v", err)
	}

	// Round 3: fuller gen_metadata for call-1 (delta) + brand-new call-2.
	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795700005, testAgyUsage{
		modelID: 1466, input: 100, output: 50, cacheRead: 300, cacheWrite: 40, reasoning: 15, id11: "call-1",
	})
	gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795700010, testAgyUsage{
		modelID: 1466, input: 200, output: 60, id11: "call-2",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?), (1, ?, ?)`, gen0, len(gen0), gen1, len(gen1)); err != nil {
		t.Fatalf("insert gen_metadata: %v", err)
	}
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 3: %v", err)
	}

	// Verify state file exists and is owner-only (0600) on Unix.
	statePath := AgySQLiteStatePath(home)
	fi, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Round 4: simulate restart with a brand-new tailer loading persisted state
	// plus a duplicate copy of call-2 in steps -> must emit 0 new events.
	step1 := encodeTestStepMeta(1466, "gemini-4-neon", 1795700010, testAgyUsage{
		modelID: 1466, input: 200, output: 60, id11: "call-2",
	})
	if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (1, 15, ?)`, step1); err != nil {
		t.Fatalf("insert duplicate step1: %v", err)
	}
	restarted := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         analytics,
		DetectVersion:     func(context.Context) string { return "1.5.18" },
	})
	if err := restarted.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll 4 (restarted): %v", err)
	}

	events := drainOutboxEvents(t, outbox)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (call-1 initial, call-1 delta, call-2)", len(events))
	}

	var totCalls, totIn, totOut, totCR, totCW, totReas int64
	for _, ev := range events {
		if ev.UsageSource != telemetrycontract.UsageSourceSQLiteNative {
			t.Errorf("usage_source = %q, want sqlite_native", ev.UsageSource)
		}
		totCalls += ev.Calls
		if ev.Input != nil {
			totIn += *ev.Input
		}
		if ev.Output != nil {
			totOut += *ev.Output
		}
		if ev.CacheRead != nil {
			totCR += *ev.CacheRead
		}
		if ev.CacheWrite != nil {
			totCW += *ev.CacheWrite
		}
		if ev.Reasoning != nil {
			totReas += *ev.Reasoning
		}
	}
	if totCalls != 0 {
		t.Errorf("tailer emitted calls = %d, want 0 (PostInvocation owns calls)", totCalls)
	}
	if totIn != 300 || totOut != 110 || totCR != 300 || totCW != 40 || totReas != 15 {
		t.Errorf("totals = (in=%d out=%d cr=%d cw=%d reas=%d), want (300, 110, 300, 40, 15)",
			totIn, totOut, totCR, totCW, totReas)
	}
}

func TestAntigravitySQLiteTailer_OutboxAppendFailureReEmitsIdenticalEmissionKey(t *testing.T) {
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-retry.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795701000, testAgyUsage{
		modelID: 1466, input: 500, output: 120, id11: "retry-call-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:         home,
		DisableWakeSocket: true,
		Analytics:         analytics,
		DetectVersion:     func(context.Context) string { return "1.5.18" },
	})

	// Force outbox append failure by clearing SCION_AGENT_ID temporarily.
	t.Setenv("SCION_AGENT_ID", "")
	if err := tailer.PollOnce(context.Background()); err == nil {
		t.Fatal("expected PollOnce error when outbox append fails")
	}
	if got := drainOutboxEvents(t, outbox); len(got) != 0 {
		t.Fatalf("expected 0 outbox events after failed append, got %d", len(got))
	}

	// Restore SCION_AGENT_ID: next poll must re-emit the exact record (not suppressed!).
	t.Setenv("SCION_AGENT_ID", "agent-agy-test")
	if err := tailer.PollOnce(context.Background()); err != nil {
		t.Fatalf("retry PollOnce: %v", err)
	}
	events := drainOutboxEvents(t, outbox)
	if len(events) != 1 {
		t.Fatalf("got %d events on retry, want 1", len(events))
	}
	if events[0].Input == nil || *events[0].Input != 500 || events[0].Output == nil || *events[0].Output != 120 {
		t.Fatalf("unexpected retry tokens: in=%v out=%v", events[0].Input, events[0].Output)
	}
	if events[0].SourceEventID == "" {
		t.Fatal("expected non-empty deterministic SourceEventID (EmissionKey)")
	}
}

func TestAntigravitySQLiteTailer_DriftLatchesAndDegradesToCallsOnly(t *testing.T) {
	t.Run("unverified_version", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-unv.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795702000, testAgyUsage{
			modelID: 1466, input: 999, output: 111, id11: "unv-call-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open outbox: %v", err)
		}
		analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         analytics,
			DetectVersion:     func(context.Context) string { return "agy 1.5.18.1" },
		})
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("expected SchemaDriftDetected() == true for unverified 4-part version")
		}
		events := drainOutboxEvents(t, outbox)
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1 drift sentinel", len(events))
		}
		ev := events[0]
		if ev.TokenStatus != telemetrycontract.TokenStatusUnverified {
			t.Errorf("token_status = %q, want unverified", ev.TokenStatus)
		}
		if ev.Calls != 0 || ev.Input != nil || ev.Output != nil || ev.CacheRead != nil || ev.CacheWrite != nil || ev.Reasoning != nil {
			t.Errorf("unverified drift event must carry calls=0 and nil tokens, got %+v", ev)
		}
	})

	t.Run("malformed_row_latches_across_subsequent_clean_polls", func(t *testing.T) {
		setTestAgyEnv(t)
		home := t.TempDir()
		dbPath := filepath.Join(AgyConversationsDir(home), "conv-mal.db")
		db := createTestAgyDB(t, dbPath)
		defer func() { _ = db.Close() }()

		// Poll 1: corrupt protobuf blob + clean row in same file.
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, 3)`, []byte{0xff, 0xff, 0xff}); err != nil {
			t.Fatalf("insert corrupt gen0: %v", err)
		}
		gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795702005, testAgyUsage{
			modelID: 1466, input: 400, output: 80, id11: "mal-clean-1",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
			t.Fatalf("insert gen1: %v", err)
		}

		outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("open outbox: %v", err)
		}
		analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

		tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
			AgentHome:         home,
			DisableWakeSocket: true,
			Analytics:         analytics,
			DetectVersion:     func(context.Context) string { return "1.5.18" },
		})
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 1: %v", err)
		}
		if !tailer.SchemaDriftDetected() {
			t.Fatal("expected SchemaDriftDetected() == true after malformed row")
		}

		// Poll 2: a brand-new clean row arrives (Result.Status would be verified),
		// but latched drift must continue suppressing token debits!
		gen2 := encodeTestGenMeta(1466, "gemini-4-neon", 1795702010, testAgyUsage{
			modelID: 1466, input: 700, output: 90, id11: "mal-clean-2",
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (2, ?, ?)`, gen2, len(gen2)); err != nil {
			t.Fatalf("insert gen2: %v", err)
		}
		if err := tailer.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll 2: %v", err)
		}

		events := drainOutboxEvents(t, outbox)
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1 malformed drift sentinel and 0 token debits", len(events))
		}
		if events[0].TokenStatus != telemetrycontract.TokenStatusMalformed {
			t.Errorf("token_status = %q, want malformed", events[0].TokenStatus)
		}
		if events[0].Input != nil || events[0].Output != nil {
			t.Errorf("malformed drift must not emit token debits, got in=%v out=%v", events[0].Input, events[0].Output)
		}

		// Verify HubUsageSink propagates schema_drift_detected=true and calls_only signals.
		var gotReq telemetrycontract.UsageIngestRequest
		sink := NewHubUsageSink(outbox, HubUsageSinkConfig{
			Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				gotReq = req
				return &telemetrycontract.UsageIngestResponse{}, nil
			},
			RunID:               "run-agy-test",
			SupportedSignals:    func() []string { return []string{"calls"} },
			SchemaDriftDetected: tailer.SchemaDriftDetected,
		})
		if err := sink.SendHeartbeat(context.Background(), ""); err != nil {
			t.Fatalf("SendHeartbeat: %v", err)
		}
		if !gotReq.SchemaDriftDetected {
			t.Fatal("HubUsageSink heartbeat did not carry schema_drift_detected=true")
		}
		if len(gotReq.SupportedSignals) != 1 || gotReq.SupportedSignals[0] != "calls" {
			t.Fatalf("SupportedSignals = %v, want [calls]", gotReq.SupportedSignals)
		}
	})
}

func TestAntigravitySQLiteTailer_WakeDebounceAndShutdownDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix domain wake socket test is unix-only")
	}
	setTestAgyEnv(t)
	home := t.TempDir()
	dbPath := filepath.Join(AgyConversationsDir(home), "conv-wake.db")
	db := createTestAgyDB(t, dbPath)
	defer func() { _ = db.Close() }()

	outbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	analytics := NewUsageAnalytics(outbox, telemetrycontract.UsageSourceSQLiteNative)

	var mu sync.Mutex
	var gotSessionTokens int64
	tailer := NewAntigravitySQLiteTailer(AntigravitySQLiteConfig{
		AgentHome:     home,
		Analytics:     analytics,
		Debounce:      30 * time.Millisecond,
		PollInterval:  time.Hour, // prove wake + shutdown drain work without fallback ticker
		DetectVersion: func(context.Context) string { return "1.5.18" },
		SessionUsage: func(u SessionUsage) {
			mu.Lock()
			gotSessionTokens += u.TokensInput
			mu.Unlock()
		},
	})
	if err := tailer.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Insert row 0 and wake via Unix domain socket.
	gen0 := encodeTestGenMeta(1466, "gemini-4-neon", 1795703000, testAgyUsage{
		modelID: 1466, input: 111, output: 22, id11: "wake-call-1",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}
	NotifyAgySQLiteWake(home)
	NotifyAgySQLiteWake(home) // coalesced by debounce

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := gotSessionTokens
		mu.Unlock()
		if n == 111 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	mu.Lock()
	if gotSessionTokens != 111 {
		mu.Unlock()
		t.Fatalf("after wake got session input tokens = %d, want 111", gotSessionTokens)
	}
	mu.Unlock()

	// Insert row 1 without waking, then Stop -> graceful-shutdown drain must capture it!
	gen1 := encodeTestGenMeta(1466, "gemini-4-neon", 1795703005, testAgyUsage{
		modelID: 1466, input: 222, output: 33, id11: "drain-call-2",
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
		t.Fatalf("insert gen1: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := drainOutboxEvents(t, outbox)
	if len(events) != 2 {
		b, _ := json.Marshal(events)
		t.Fatalf("got %d events (%s), want 2 (1 wake + 1 shutdown drain)", len(events), b)
	}
}
